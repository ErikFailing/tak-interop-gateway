package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/devcert"
	"github.com/erikfailing/tak-interop-gateway/internal/hostile"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// TestList checks that -list names every case.
func TestList(t *testing.T) {
	var stdout bytes.Buffer
	if code := run(context.Background(), []string{"-list"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, c := range hostile.Cases(time.Now()) {
		if !strings.Contains(stdout.String(), c.Name) {
			t.Errorf("-list is missing %s", c.Name)
		}
	}
}

// TestStdout checks that stdout mode writes every case one stream can carry, and says why it skipped the rest.
func TestStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, c := range hostile.Cases(time.Now()) {
		switch {
		case c.StreamFatal() || c.Impersonates:
			if !strings.Contains(stderr.String(), "skipped "+c.Name) {
				t.Errorf("%s should be skipped on stdout", c.Name)
			}
		case !strings.Contains(stderr.String(), "sent "+c.Name):
			t.Errorf("%s was not sent", c.Name)
		}
	}
	if !strings.Contains(stdout.String(), "hostile-teleport") {
		t.Error("stdout carries no attack")
	}
}

// listener accepts connections, mutual TLS when cfg is set, and records the client name of each and what it sent.
type listener struct {
	mu    sync.Mutex
	conns []string // one entry per connection: the client's certificate name, then ": ", then the bytes received
}

func (l *listener) serve(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	var ln net.Listener
	var err error
	if cfg != nil {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", cfg)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				b, _ := io.ReadAll(conn)
				name := ""
				if tc, ok := conn.(*tls.Conn); ok {
					name = transport.PeerName(tc.ConnectionState())
				}
				l.mu.Lock()
				l.conns = append(l.conns, name+": "+string(b))
				l.mu.Unlock()
			}()
		}
	}()
	return ln.Addr().String()
}

func (l *listener) received() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.conns...)
}

// TestTCP sends every case over plain TCP: one connection each, and the impersonation case skipped for want of a
// second identity.
func TestTCP(t *testing.T) {
	var l listener
	addr := l.serve(t, nil)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-tcp", addr, "-delay", "1ms"}, io.Discard, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, &stderr)
	}
	want := len(hostile.Cases(time.Now())) - 1
	deadline := time.Now().Add(5 * time.Second)
	for len(l.received()) < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(l.received()); got != want {
		t.Errorf("%d connections, want %d", got, want)
	}
	if !strings.Contains(stderr.String(), "skipped impersonation") {
		t.Errorf("stderr = %q, want the impersonation case skipped", stderr.String())
	}
}

// TestImpersonation sends the impersonation case over mutual TLS: the setup on the client's identity, the attack
// on the intruder's.
func TestImpersonation(t *testing.T) {
	f, err := devcert.Generate(t.TempDir(), []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := transport.ServerTLS(f.ServerCert, f.ServerKey, f.CA)
	if err != nil {
		t.Fatal(err)
	}
	var l listener
	addr := l.serve(t, cfg)
	args := []string{"-tcp", addr, "-case", "impersonation", "-delay", "1ms",
		"-tls-cert", f.ClientCert, "-tls-key", f.ClientKey, "-tls-ca", f.CA,
		"-intruder-cert", f.IntruderCert, "-intruder-key", f.IntruderKey}
	if code := run(context.Background(), args, io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(l.received()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := strings.Join(l.received(), "\n")
	if !strings.Contains(got, "takgw-dev-client: <event") || !strings.Contains(got, "takgw-dev-intruder: <event") {
		t.Errorf("want one connection from each identity, got:\n%s", got)
	}
}

// TestFlags checks the flag rules.
func TestFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-tls-cert", "c.pem"},
		{"-tcp", "127.0.0.1:1", "-tls-cert", "c.pem"},
		{"-intruder-cert", "i.pem", "-intruder-key", "i.key"},
		{"-tcp", "127.0.0.1:1", "-intruder-cert", "i.pem", "-intruder-key", "i.key"},
		{"-tcp", "127.0.0.1:1", "-tls-server-name", "gateway"},
		{"-tcp", "127.0.0.1:1", "-tls-cert", "missing.pem", "-tls-key", "missing.key", "-tls-ca", "missing-ca.pem"},
		{"-case", "no-such-case"},
		{"extra"},
	} {
		if code := run(context.Background(), args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// TestStopped checks that a stop before the last case is a failure, not a clean run: not every case was sent.
func TestStopped(t *testing.T) {
	var l listener
	addr := l.serve(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	if code := run(ctx, []string{"-tcp", addr, "-delay", "1ms"}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "stopped before") {
		t.Errorf("exit %d, stderr %q; want 1 and a note that it stopped early", code, stderr.String())
	}
}
