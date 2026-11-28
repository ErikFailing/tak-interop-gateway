package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cli"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/health"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// frame is one framed CoT message and where it came from.
type frame struct {
	data []byte
	// origin identifies the sender, for the state stage's origin binding: the client certificate's identity under
	// mutual TLS, the remote address's IP on plain TCP, "stdin", or the TAK server's address for -connect, where every
	// report arrives on the server's one connection.
	origin string
}

// The limits on a -listen connection before it is verified. A TLS client gets handshakeTimeout to finish its handshake,
// client certificate and all, and only then takes one of the -max-conns slots, so clients that connect and stall can't
// lock verified ones out. The handshakes in progress are capped as well, in all and from any one IP address, so
// stalling them can't exhaust the gateway instead.
const (
	handshakeTimeout   = 5 * time.Second
	maxHandshakes      = 64
	maxHandshakesPerIP = 8
)

// dialTimeout bounds connecting to a TAK server, TLS handshake included.
const dialTimeout = 10 * time.Second

// The reconnect backoff for -connect: it starts at minBackoff, doubles after each failed attempt up to maxBackoff, and
// starts over after a connection that stayed up for resetBackoff. They're variables so tests can shorten them.
var (
	minBackoff   = time.Second
	maxBackoff   = time.Minute
	resetBackoff = time.Minute
)

// maxAcceptDelay caps the wait between retries when Accept fails, such as when the process is out of file descriptors.
const maxAcceptDelay = time.Second

// connState is where a -connect connection stands, for the health asset.
type connState int

const (
	connecting   connState = iota // the first attempt hasn't finished yet
	connected                     // the connection is up
	reconnecting                  // the connection failed or ended, and the next attempt is waiting or under way
)

// ingest is the gateway's input side: it reads CoT from stdin, from TAK clients that connect in or from a TAK server it
// connects out to, frames it, and queues each message for the pipeline goroutine. Its methods are safe to call from
// any goroutine.
type ingest struct {
	log         *slog.Logger
	frames      chan<- frame
	listenAddr  string // the address listened on, with -listen
	connectAddr string // the TAK server, with -connect
	maxConns    int
	idleTimeout time.Duration

	// warnings limits the warnings for refused connections and failed handshakes, which anyone who can reach the port
	// can cause.
	warnings throttle

	mu             sync.Mutex
	streamErrors   int            // streams that ended with an error
	suspectStreams int            // of those, the ones ended by input the framer couldn't follow, or a failed handshake
	conns          int            // verified connections open, with -listen
	handshakes     int            // TLS handshakes in progress, with -listen
	handshakesByIP map[string]int // the same, by remote IP address
	state          connState      // with -connect
}

// streamFailed counts a stream that ended with an error. Input the framer couldn't follow, such as an oversized
// message or broken XML, is suspect; a timeout or a dropped connection isn't.
func (in *ingest) streamFailed(err error) {
	suspect := errors.Is(err, transport.ErrFrameTooLarge) || errors.Is(err, transport.ErrMalformedStream)
	in.mu.Lock()
	defer in.mu.Unlock()
	in.streamErrors++
	if suspect {
		in.suspectStreams++
	}
}

// handshakeFailed counts a failed TLS handshake as a stream error, and as suspect: on a mutual-TLS port it is a client
// without a valid certificate, or one that stalled.
func (in *ingest) handshakeFailed() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.streamErrors++
	in.suspectStreams++
}

// streamCounts returns how many streams ended with an error, and how many of those were suspect.
func (in *ingest) streamCounts() (failed, suspect int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.streamErrors, in.suspectStreams
}

// adjustConns changes the count of open connections.
func (in *ingest) adjustConns(delta int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.conns += delta
}

// setState records where the TAK server connection stands.
func (in *ingest) setState(s connState) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.state = s
}

// input reports the feed's state for the health asset.
func (in *ingest) input() health.Input {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch {
	case in.connectAddr != "" && in.state == connecting:
		return health.Input{Starting: true, Detail: "connecting to " + in.connectAddr}
	case in.connectAddr != "" && in.state == connected:
		return health.Input{Up: true, Detail: "connected to " + in.connectAddr}
	case in.connectAddr != "":
		return health.Input{Up: false, Detail: "not connected to " + in.connectAddr + "; reconnecting"}
	case in.listenAddr != "":
		return health.Input{Up: true, Detail: fmt.Sprintf("listening on %s, %d connections open", in.listenAddr, in.conns)}
	}
	return health.Input{Up: true, Detail: "reading stdin"}
}

// readStream frames one stream and queues each message, until the stream ends or ctx is done. It returns nil at a
// clean end of the stream, and the framer's error otherwise.
func (in *ingest) readStream(ctx context.Context, r io.Reader, name, origin string) error {
	fr := transport.NewFramer(r, cot.DefaultLimits.MaxBytes)
	for {
		msg, err := fr.Next()
		if errors.Is(err, io.EOF) {
			in.log.Debug("stream ended", "stream", name)
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case in.frames <- frame{data: msg, origin: origin}:
		case <-ctx.Done():
			return nil
		}
	}
}

// acceptLoop accepts connections until ctx is done, and reads each in its own goroutine. It returns once every
// connection it accepted has closed.
func (in *ingest) acceptLoop(ctx context.Context, ln net.Listener) {
	// 1: Closing the listener is what unblocks Accept on a stop. Each connection closes itself the same way.
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var conns sync.WaitGroup
	defer conns.Wait()

	// 2: Accept. A failure other than the listener closing may pass, such as running out of file descriptors, so wait
	// a little, longer each time, and try again.
	slots := make(chan struct{}, in.maxConns)
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			delay = min(max(2*delay, 5*time.Millisecond), maxAcceptDelay)
			in.log.Error("accept failed; retrying", "err", err, "in", delay)
			if !cli.Sleep(ctx, delay) {
				return
			}
			continue
		}
		delay = 0
		conns.Go(func() { in.serveConn(ctx, conn, slots) })
	}
}

// serveConn reads one connection until it ends, goes idle or sends something unframeable. A TLS connection must first
// finish its handshake; only then does it take one of the slots, or get refused because they are all taken.
func (in *ingest) serveConn(ctx context.Context, conn net.Conn, slots chan struct{}) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // a stop unblocks the read
	defer stop()

	// 1: Complete a TLS handshake, and work out the connection's origin: its certificate's identity under mutual TLS,
	// or else its IP address, without the port, so a client that reconnects keeps its origin.
	remote := conn.RemoteAddr().String()
	ip, _, _ := net.SplitHostPort(remote)
	origin := ip
	attrs := []any{"remote", remote}
	if tc, ok := conn.(*tls.Conn); ok {
		if !in.handshake(ctx, tc, ip) {
			return
		}
		state := tc.ConnectionState()
		if id := transport.PeerIdentity(state); id != "" {
			origin = id
		}
		attrs = append(attrs, "client", transport.PeerName(state))
	}

	// 2: Take a slot, or refuse the connection at once, so a flood can't hold readers open.
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		in.warnLimited("refused a connection: at the -max-conns limit", "remote", remote)
		return
	}

	// 3: Read until the connection ends.
	in.adjustConns(1)
	defer in.adjustConns(-1)
	in.log.Info("connection opened", attrs...)
	err := in.readStream(ctx, &idleReader{conn: conn, timeout: in.idleTimeout}, remote, origin)
	if err != nil && ctx.Err() == nil {
		in.log.Warn("connection closed on error", "remote", remote, "err", err)
		in.streamFailed(err)
		return
	}
	in.log.Info("connection closed", "remote", remote)
}

// handshake completes a TLS handshake within handshakeTimeout, if there is room for one more in progress, and reports
// whether it succeeded.
func (in *ingest) handshake(ctx context.Context, tc *tls.Conn, ip string) bool {
	remote := tc.RemoteAddr().String()
	if !in.startHandshake(ip) {
		in.warnLimited("refused a connection: too many TLS handshakes in progress", "remote", remote)
		return false
	}
	defer in.endHandshake(ip)
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		if ctx.Err() == nil {
			in.warnLimited("TLS handshake failed", "remote", remote, "err", err)
			in.handshakeFailed()
		}
		return false
	}
	return true
}

// startHandshake takes a place for one more handshake in progress from ip, if the limits allow it.
func (in *ingest) startHandshake(ip string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.handshakes >= maxHandshakes || in.handshakesByIP[ip] >= maxHandshakesPerIP {
		return false
	}
	if in.handshakesByIP == nil {
		in.handshakesByIP = map[string]int{}
	}
	in.handshakes++
	in.handshakesByIP[ip]++
	return true
}

// endHandshake gives back the place startHandshake took.
func (in *ingest) endHandshake(ip string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.handshakes--
	in.handshakesByIP[ip]--
	if in.handshakesByIP[ip] == 0 {
		delete(in.handshakesByIP, ip)
	}
}

// warnLimited logs a warning, unless too many have been logged lately; see throttle.
func (in *ingest) warnLimited(msg string, args ...any) {
	ok, held := in.warnings.allow(time.Now())
	if !ok {
		return
	}
	if held > 0 {
		args = append(args, "held_back", held)
	}
	in.log.Warn(msg, args...)
}

// The warning limit: at most warnBurst warnings in any warnWindow.
const (
	warnWindow = 10 * time.Second
	warnBurst  = 10
)

// throttle limits a kind of warning to warnBurst per warnWindow, so a flood of bad connections can't flood the log as
// well. The first warning logged after some were held back says how many. It is safe for concurrent use.
type throttle struct {
	mu    sync.Mutex
	start time.Time // when the current window began
	n     int       // warnings logged in it
	held  int       // warnings held back since the last one logged
}

// allow reports whether a warning at now may be logged, and if so, how many were held back before it.
func (t *throttle) allow(now time.Time) (ok bool, held int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.start) >= warnWindow {
		t.start, t.n = now, 0
	}
	if t.n >= warnBurst {
		t.held++
		return false, 0
	}
	t.n++
	held, t.held = t.held, 0
	return true, held
}

// connectLoop keeps a connection to the TAK server open until ctx is done, reconnecting with backoff whenever it fails
// or the server closes it. Each failed attempt counts as a stream error.
func (in *ingest) connectLoop(ctx context.Context, cfg *tls.Config) {
	addr := in.connectAddr
	delay := minBackoff
	for {
		// 1: Connect and read until the connection ends.
		started := time.Now()
		err := in.connectOnce(ctx, addr, cfg)
		if ctx.Err() != nil {
			return
		}
		in.setState(reconnecting)
		if err != nil {
			in.streamFailed(err)
		}

		// 2: Back off before trying again, unless the connection had been healthy for a while.
		if time.Since(started) >= resetBackoff {
			delay = minBackoff
		}
		wait := jitter(delay)
		in.log.Warn("TAK server connection ended; reconnecting", "addr", addr, "err", err, "in", wait.Round(time.Millisecond))
		if !cli.Sleep(ctx, wait) {
			return
		}
		delay = min(2*delay, maxBackoff)
	}
}

// connectOnce dials the server, completes the TLS handshake, and reads until the connection ends. It returns nil when
// the server closed the connection cleanly.
func (in *ingest) connectOnce(ctx context.Context, addr string, cfg *tls.Config) error {
	conn, err := transport.Dial(ctx, addr, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // a stop unblocks the read
	defer stop()
	attrs := []any{"addr", addr}
	if tc, ok := conn.(*tls.Conn); ok {
		attrs = append(attrs, "server", transport.PeerName(tc.ConnectionState()))
	}
	in.log.Info("connected", attrs...)
	in.setState(connected)
	return in.readStream(ctx, &idleReader{conn: conn, timeout: in.idleTimeout}, addr, addr)
}

// jitter spreads d by up to 20% either way, so gateways that lost the same server don't all reconnect in step.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// idleReader pushes a connection's read deadline forward before every read, so a connection that sends nothing for
// the timeout fails its read and is closed, while a busy one stays open indefinitely.
type idleReader struct {
	conn    net.Conn
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	if err := r.conn.SetReadDeadline(time.Now().Add(r.timeout)); err != nil {
		return 0, err
	}
	return r.conn.Read(p)
}

// warnIfExposed warns when the listener accepts connections from beyond this machine, because plain TCP CoT has no
// authentication.
func warnIfExposed(log *slog.Logger, addr net.Addr) {
	if tcp, ok := addr.(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		log.Warn("listening beyond loopback with no authentication: anyone who can reach this port can inject tracks",
			"addr", addr.String())
	}
}
