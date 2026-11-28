package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// newIngest returns an ingest for -listen tests, with a queue the test can read.
func newIngest(maxConns int, idle time.Duration) (*ingest, chan frame) {
	frames := make(chan frame, 64)
	return &ingest{log: slog.New(slog.DiscardHandler), frames: frames, maxConns: maxConns, idleTimeout: idle}, frames
}

// startAcceptLoop runs an acceptLoop on ln, or on a fresh loopback listener when ln is nil, and returns its address
// and a function that stops it and waits for it, failing the test if it doesn't return.
func startAcceptLoop(t *testing.T, in *ingest, ln net.Listener) (string, func()) {
	t.Helper()
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		in.acceptLoop(ctx, ln)
		close(stopped)
	}()
	return ln.Addr().String(), func() {
		t.Helper()
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("acceptLoop didn't return after its context ended")
		}
	}
}

// tlsListener returns a mutual-TLS listener on a fresh loopback port, and a client config it trusts.
func tlsListener(t *testing.T) (net.Listener, *tls.Config) {
	t.Helper()
	f := devCerts(t)
	server, err := transport.ServerTLS(f.ServerCert, f.ServerKey, f.CA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := transport.ClientTLS(f.ClientCert, f.ClientKey, f.CA, "")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	return ln, client
}

// closedByServer reports whether the server closes conn within five seconds, without sending anything.
func closedByServer(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	return errors.Is(err, io.EOF)
}

// dial connects to addr, failing the test if it can't, and closes the connection when the test ends.
func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestConnectionLimit checks that a connection beyond -max-conns is closed at once.
func TestConnectionLimit(t *testing.T) {
	in, frames := newIngest(1, time.Minute)
	addr, stop := startAcceptLoop(t, in, nil)
	defer stop()
	first := dial(t, addr)
	// Make sure the first connection holds the only slot before the second arrives.
	if _, err := io.WriteString(first, scenario(t, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("the first connection's report never arrived")
	}
	if !closedByServer(dial(t, addr)) {
		t.Error("the connection over the limit was not closed")
	}
}

// TestIdleTimeout checks that a connection that sends nothing is closed after -idle-timeout, as a stream error but not
// a suspect one.
func TestIdleTimeout(t *testing.T) {
	in, _ := newIngest(4, 100*time.Millisecond)
	addr, stop := startAcceptLoop(t, in, nil)
	defer stop()
	if !closedByServer(dial(t, addr)) {
		t.Error("the idle connection was not closed")
	}
	waitFor(t, "the timeout to count as a stream error", func() bool {
		failed, suspect := in.streamCounts()
		return failed == 1 && suspect == 0
	})
}

// TestStopClosesConnections checks that a stop closes open connections, so the accept loop returns.
func TestStopClosesConnections(t *testing.T) {
	in, _ := newIngest(4, time.Hour)
	addr, stop := startAcceptLoop(t, in, nil)
	conn := dial(t, addr)
	waitFor(t, "the connection to open", func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return in.conns == 1
	})
	stop()
	if !closedByServer(conn) {
		t.Error("the stop didn't close the connection")
	}
}

// TestHandshakeTakesNoSlot checks that a client stalled in its TLS handshake doesn't hold a -max-conns slot: with one
// slot and a stalled client, a verified client still gets in.
func TestHandshakeTakesNoSlot(t *testing.T) {
	ln, client := tlsListener(t)
	in, frames := newIngest(1, time.Minute)
	addr, stop := startAcceptLoop(t, in, ln)
	defer stop()
	_ = dial(t, addr) // connects, and never sends a ClientHello
	waitFor(t, "the stalled handshake", func() bool { return in.pendingHandshakes() == 1 })

	conn, err := tls.Dial("tcp", addr, client)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, scenario(t, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		if f.origin != "cert:takgw-dev-client" {
			t.Errorf("origin = %q, want the client certificate's identity", f.origin)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the verified client's report never arrived")
	}
}

// TestHandshakeLimitPerIP checks that one address can't have more than maxHandshakesPerIP handshakes in progress: the
// next connection is closed at once.
func TestHandshakeLimitPerIP(t *testing.T) {
	ln, _ := tlsListener(t)
	in, _ := newIngest(4, time.Minute)
	addr, stop := startAcceptLoop(t, in, ln)
	defer stop()
	for range maxHandshakesPerIP {
		_ = dial(t, addr)
	}
	waitFor(t, "the stalled handshakes", func() bool { return in.pendingHandshakes() == maxHandshakesPerIP })
	start := time.Now()
	if !closedByServer(dial(t, addr)) {
		t.Fatal("the connection over the per-address limit was not closed")
	}
	if waited := time.Since(start); waited >= handshakeTimeout {
		t.Errorf("the refusal took %v: it waited for the handshake timeout instead of refusing at once", waited)
	}
}

// pendingHandshakes returns how many TLS handshakes are in progress.
func (in *ingest) pendingHandshakes() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.handshakes
}

// flakyListener fails its first fails calls to Accept, as a listener does when the process is out of file
// descriptors, then accepts normally.
type flakyListener struct {
	net.Listener
	fails int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails > 0 {
		l.fails--
		return nil, fmt.Errorf("accept: %w", os.ErrDeadlineExceeded)
	}
	return l.Listener.Accept()
}

// TestAcceptRetry checks that the accept loop carries on after a failure that isn't the listener closing.
func TestAcceptRetry(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	in, frames := newIngest(4, time.Minute)
	addr, stop := startAcceptLoop(t, in, &flakyListener{Listener: inner, fails: 3})
	defer stop()
	if _, err := io.WriteString(dial(t, addr), scenario(t, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("no report arrived after the failed accepts")
	}
}

// TestStreamFailed checks which stream errors count as suspect: input the framer couldn't follow, not a timeout.
func TestStreamFailed(t *testing.T) {
	in, _ := newIngest(1, time.Minute)
	in.streamFailed(fmt.Errorf("frame: %w", transport.ErrFrameTooLarge))
	in.streamFailed(fmt.Errorf("frame: %w", transport.ErrMalformedStream))
	in.streamFailed(fmt.Errorf("frame: %w", transport.ErrTruncated))
	in.streamFailed(os.ErrDeadlineExceeded)
	in.handshakeFailed()
	if failed, suspect := in.streamCounts(); failed != 5 || suspect != 3 {
		t.Errorf("streamCounts = %d, %d; want 5 stream errors, 3 of them suspect", failed, suspect)
	}
}

// TestConnectInput checks the health asset's view of a -connect input: starting until the first attempt is over, then
// up or down.
func TestConnectInput(t *testing.T) {
	in := &ingest{connectAddr: "tak.example.org:8089"}
	if got := in.input(); !got.Starting {
		t.Errorf("before the first attempt: %+v, want starting", got)
	}
	in.setState(connected)
	if got := in.input(); got.Starting || !got.Up {
		t.Errorf("connected: %+v, want up", got)
	}
	in.setState(reconnecting)
	if got := in.input(); got.Starting || got.Up {
		t.Errorf("reconnecting: %+v, want down", got)
	}
}

// TestThrottle checks the warning limit: warnBurst per window, then silence, then a count of what was held back.
func TestThrottle(t *testing.T) {
	var th throttle
	now := time.Now()
	for i := range warnBurst {
		if ok, _ := th.allow(now); !ok {
			t.Fatalf("warning %d was held back, want the first %d allowed", i+1, warnBurst)
		}
	}
	for range 3 {
		if ok, _ := th.allow(now.Add(time.Second)); ok {
			t.Fatal("a warning past the burst was allowed")
		}
	}
	if ok, held := th.allow(now.Add(warnWindow)); !ok || held != 3 {
		t.Errorf("after the window: allow = %v, held = %d; want true, 3", ok, held)
	}
}
