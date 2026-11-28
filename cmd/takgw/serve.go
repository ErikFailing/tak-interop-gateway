package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/publish/latticesdk"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// serveUsage heads the flag list for `takgw serve -h`.
const serveUsage = `usage: takgw serve -source NAME (-stdin | -listen HOST:PORT) [flags]

Runs the gateway: reads a CoT stream, maps each event to a Lattice entity, drops repeats, late arrivals and unchanged
reports, checks what's left with Entity Preflight, and publishes it. By default each entity is written as one JSON
line to -out, the format "takgw preflight" reads. With -lattice it is published to the Lattice environment named by
LATTICE_ENDPOINT, ENVIRONMENT_TOKEN and, for Sandboxes, SANDBOXES_TOKEN.

-listen accepts plain TCP with no authentication, so anyone who can reach the port can inject tracks. Bind it to a
loopback address or a trusted network; TLS with client certificates is still to come.

Exit status: 0 after a clean stop (end of stdin, or Ctrl-C), 1 for a runtime failure (a malformed stdin stream, a
listener or output error, or credentials the platform refuses), 2 for a usage error. With -lattice, an entity the
platform rejects is counted and logged, and the gateway carries on.

flags:
`

// frameQueue is how many framed messages may wait for the pipeline. When it is full, readers stop reading, so a fast
// sender is slowed to the gateway's pace by TCP itself rather than by unbounded memory.
const frameQueue = 256

// counters tallies what happened to every message, for the summary line.
type counters struct {
	messages, published, parse, mapped, rejected, publishErr, streamErr int
	dropped                                                             map[string]int
}

// serveConfig is everything runServe reads from its flags.
type serveConfig struct {
	pipeline    pipeline.Config
	stdin       bool
	listen      string
	out         string
	lattice     bool
	maxConns    int
	idleTimeout time.Duration
	verbose     bool
}

// runServe implements `takgw serve` and returns the exit status.
func runServe(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfg, code := parseServeFlags(args, stderr)
	if code >= 0 {
		return code
	}
	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	// 1: Choose the publisher, and which of its failures stop the gateway. Writing to a file or a pipe fails for good
	// once it fails, so any failure stops. Lattice can reject one entity and accept the next, so only refused
	// credentials, which fail every request, stop.
	fatal := func(error) bool { return true }
	if cfg.lattice {
		client, err := newLiveClient()
		if err != nil {
			fmt.Fprintf(stderr, "takgw serve: -lattice: %v\n", err)
			return 2
		}
		cfg.pipeline.Publisher = client
		fatal = func(err error) bool { return errors.Is(err, latticesdk.ErrUnauthorized) }
	} else {
		out := stdout
		if cfg.out != "-" {
			f, err := os.OpenFile(cfg.out, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err != nil {
				fmt.Fprintf(stderr, "takgw serve: -out: %v\n", err)
				return 1
			}
			defer f.Close() // every entity is written in one Write as it's published, so there's nothing left to flush
			out = f
		}
		cfg.pipeline.Publisher = publish.NewJSONL(out)
	}
	p, err := pipeline.New(cfg.pipeline)
	if err != nil {
		fmt.Fprintf(stderr, "takgw serve: %v\n", err)
		return 2
	}

	// 2: Start the readers. Each sends framed messages to one queue, which only the pipeline goroutine reads.
	frames := make(chan []byte, frameQueue)
	s := &server{log: log, frames: frames, maxConns: cfg.maxConns, idleTimeout: cfg.idleTimeout}
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	var readers sync.WaitGroup
	readers.Add(1)
	if cfg.stdin {
		go func() {
			defer readers.Done()
			if err := s.readStream(readCtx, stdin, "stdin"); err != nil {
				log.Error("stdin stream", "err", err)
				s.streamFailed()
			}
		}()
	} else {
		ln, err := net.Listen("tcp", cfg.listen)
		if err != nil {
			fmt.Fprintf(stderr, "takgw serve: listen: %v\n", err)
			return 1
		}
		warnIfExposed(log, ln.Addr())
		log.Info("listening", "addr", ln.Addr().String())
		go func() {
			defer readers.Done()
			s.acceptLoop(readCtx, ln)
		}()
	}
	go func() {
		readers.Wait()
		close(frames)
	}()

	// 3: Process messages in arrival order until the readers are done or the context ends, or a fatal publish failure.
	c := counters{dropped: map[string]int{}}
	code = 0
	process := func(ctx context.Context, msg []byte) bool {
		c.messages++
		res, err := p.Process(ctx, msg)
		if count(&c, log, res, err) {
			if fatal(err) {
				log.Error("publish failed, stopping", "err", err)
				code = 1
				return false
			}
			log.Warn("publish failed", "entity", idOf(res.Entity), "err", err)
		}
		return true
	}
loop:
	for {
		select {
		case msg, ok := <-frames:
			if !ok || !process(ctx, msg) {
				break loop
			}
		case <-ctx.Done():
			// On a stop, finish what has already been received, but don't wait for more. Those publishes must not
			// inherit the cancellation, or an adapter that honors its context would fail them all.
			drain := context.WithoutCancel(ctx)
			for {
				select {
				case msg, ok := <-frames:
					if !ok || !process(drain, msg) {
						break loop
					}
				default:
					break loop
				}
			}
		}
	}

	// 4: Stop the readers. TCP readers always finish once the listener and connections are closed, so wait for them,
	// and their last log lines, before summarizing. A stdin reader may be blocked in a read nothing can interrupt; at
	// a clean end of input it has already finished, and otherwise the process is about to exit anyway.
	stopReading()
	if !cfg.stdin {
		readers.Wait()
	}

	// 5: Summarize. A malformed stdin stream is a failure; a bad TCP connection is only that connection's problem.
	c.streamErr = s.streamErrors()
	if cfg.stdin && c.streamErr > 0 && code == 0 {
		code = 1
	}
	fmt.Fprintf(stderr, "takgw serve: %d messages: %d published, %s, %d unparsable, %d unmappable, "+
		"%d rejected by preflight, %d publish errors, %d stream errors; %d entities tracked\n",
		c.messages, c.published, drops(c.dropped), c.parse, c.mapped, c.rejected, c.publishErr, c.streamErr, p.Tracked())
	return code
}

// dropReasons names each state-stage reason in logs and the summary.
var dropReasons = map[error]string{
	state.ErrRepeated: "repeated", state.ErrOutOfOrder: "out-of-order", state.ErrUnchanged: "unchanged", state.ErrFull: "full",
}

// drops formats the drop counts as "3 dropped (repeated 1, unchanged 2)", in a fixed order.
func drops(m map[string]int) string {
	n := 0
	var parts []string
	for _, reason := range []string{"repeated", "out-of-order", "unchanged", "full"} {
		if m[reason] > 0 {
			n += m[reason]
			parts = append(parts, fmt.Sprintf("%s %d", reason, m[reason]))
		}
	}
	if n == 0 {
		return "0 dropped"
	}
	return fmt.Sprintf("%d dropped (%s)", n, strings.Join(parts, ", "))
}

// count records one outcome and logs it at debug level, or warns for what an operator should see. It returns true
// if publishing failed.
func count(c *counters, log *slog.Logger, res pipeline.Result, err error) bool {
	id := idOf(res.Entity)
	switch {
	case err == nil:
		c.published++
		for _, v := range res.Violations {
			log.Warn("published with a finding", "entity", id, "finding", v.String())
		}
		log.Debug("published", "entity", id, "name", name(res.Entity))
	case errors.Is(err, pipeline.ErrDropped):
		reason := "other"
		for sentinel, r := range dropReasons {
			if errors.Is(err, sentinel) {
				reason = r
			}
		}
		c.dropped[reason]++
		if reason == "full" {
			log.Warn("dropped a new entity: the state stage is full", "entity", id)
		} else {
			log.Debug("dropped", "entity", id, "reason", reason)
		}
	case errors.Is(err, pipeline.ErrParse):
		c.parse++
		log.Debug("unparsable message", "err", err)
	case errors.Is(err, pipeline.ErrMap):
		c.mapped++
		log.Debug("unmappable event", "err", err)
	case errors.Is(err, pipeline.ErrRejected):
		c.rejected++
		log.Warn("rejected by preflight", "entity", id, "err", err)
	case errors.Is(err, pipeline.ErrPublish):
		c.publishErr++
		return true
	}
	return false
}

// idOf is an entity's ID, for logs.
func idOf(e entity.Entity) string {
	if e.EntityID == nil {
		return ""
	}
	return *e.EntityID
}

// name is an entity's display name, for logs.
func name(e entity.Entity) string {
	if e.Aliases == nil || e.Aliases.Name == nil {
		return ""
	}
	return *e.Aliases.Name
}

// parseServeFlags reads the flags. It returns the config and -1, or an exit status when runServe should stop now.
func parseServeFlags(args []string, stderr io.Writer) (serveConfig, int) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "name of this feed (required); it is part of every entity ID, so keep it stable")
	useStdin := fs.Bool("stdin", false, "read one CoT stream from stdin, and stop at its end")
	listen := fs.String("listen", "", "accept CoT streams over plain TCP on this host:port")
	out := fs.String("out", "-", `append published entities as JSON lines to this file; "-" is stdout`)
	lattice := fs.Bool("lattice", false, "publish to the Lattice environment in LATTICE_ENDPOINT, ENVIRONMENT_TOKEN and SANDBOXES_TOKEN instead of -out")
	simulated := fs.Bool("simulated", false, "mark every entity as simulated (do this for test data such as cot-gen's)")
	trackQuality := fs.Int("track-quality", mapping.DefaultTrackQuality, "tracked.trackQualityWrapper for every track, 0 to 15")
	classification := fs.String("classification", string(entity.ClassificationUnclassified), "dataClassification level for every entity")
	integration := fs.String("integration-name", mapping.DefaultIntegrationName, "provenance.integrationName")
	refresh := fs.Duration("refresh", state.DefaultRefresh, "republish an unchanged entity this often; negative publishes every report")
	maxEntities := fs.Int("max-entities", state.DefaultMaxEntities, "how many entities to track at once")
	maxConns := fs.Int("max-conns", 16, "with -listen, how many connections at once")
	idle := fs.Duration("idle-timeout", 5*time.Minute, "with -listen, close a connection silent for this long")
	verbose := fs.Bool("v", false, "log every message's outcome")
	fs.Usage = func() {
		fmt.Fprint(stderr, serveUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return serveConfig{}, 0
		}
		return serveConfig{}, 2
	}

	// Check what the pipeline can't: the input choice, the connection limits, and the classification against the SDK.
	usage := func(format string, a ...any) (serveConfig, int) {
		fmt.Fprintf(stderr, "takgw serve: "+format+"\n", a...)
		return serveConfig{}, 2
	}
	switch {
	case fs.NArg() > 0:
		return usage("unexpected arguments: %v", fs.Args())
	case *source == "":
		return usage("-source is required: it names the feed, and is part of every entity ID")
	case *useStdin == (*listen != ""):
		return usage("choose exactly one of -stdin or -listen")
	case *maxConns < 1:
		return usage("-max-conns must be at least 1")
	case *idle <= 0:
		return usage("-idle-timeout must be positive")
	case *lattice && flagSet(fs, "out"):
		return usage("choose -lattice or -out, not both")
	}
	level := entity.ClassificationLevel(*classification)
	if err := preflight.CheckClassification(level); err != nil {
		return usage("-classification: %v", err)
	}
	cfg := serveConfig{
		pipeline: pipeline.Config{
			Mapping: mapping.Config{
				Source: *source, IntegrationName: *integration, Simulated: *simulated,
				TrackQuality: *trackQuality, Classification: level,
			},
			Limits: cot.DefaultLimits,
			State:  state.Config{Refresh: *refresh, MaxEntities: *maxEntities},
		},
		stdin: *useStdin, listen: *listen, out: *out, lattice: *lattice, maxConns: *maxConns, idleTimeout: *idle,
		verbose: *verbose,
	}

	// The pipeline checks these again, but checking now keeps a usage error from creating the -out file first.
	if err := cfg.pipeline.Mapping.Validate(); err != nil {
		return usage("%v", err)
	}
	if _, err := state.New(cfg.pipeline.State); err != nil {
		return usage("%v", err)
	}
	return cfg, -1
}

// flagSet reports whether the named flag was given on the command line, as opposed to left at its default.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// server holds what the readers share.
type server struct {
	log         *slog.Logger
	frames      chan<- []byte
	maxConns    int
	idleTimeout time.Duration

	mu        sync.Mutex
	streamErr int
}

// streamFailed counts a stream that ended with an error.
func (s *server) streamFailed() {
	s.mu.Lock()
	s.streamErr++
	s.mu.Unlock()
}

// streamErrors returns how many streams ended with an error.
func (s *server) streamErrors() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamErr
}

// readStream frames one stream and queues each message, until the stream ends or ctx is done. It returns nil at a
// clean end of the stream, and the framer's error otherwise.
func (s *server) readStream(ctx context.Context, r io.Reader, name string) error {
	fr := transport.NewFramer(r, cot.DefaultLimits.MaxBytes)
	for {
		msg, err := fr.Next()
		if errors.Is(err, io.EOF) {
			s.log.Debug("stream ended", "stream", name)
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case s.frames <- msg:
		case <-ctx.Done():
			return nil
		}
	}
}

// acceptLoop accepts connections until ctx is done, reading each in its own goroutine, at most maxConns at once.
func (s *server) acceptLoop(ctx context.Context, ln net.Listener) {
	// 1: Closing the listener is what unblocks Accept on shutdown; closing each connection unblocks its reader.
	var conns sync.Map // net.Conn to struct{}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		conns.Range(func(c, _ any) bool {
			_ = c.(net.Conn).Close()
			return true
		})
	}()

	// 2: Accept, refusing connections over the limit at once so a flood can't hold readers open.
	slots := make(chan struct{}, s.maxConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("accept", "err", err)
			}
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			s.log.Warn("refused a connection: at the -max-conns limit", "remote", conn.RemoteAddr().String())
			_ = conn.Close()
			continue
		}
		conns.Store(conn, struct{}{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer conns.Delete(conn)
			defer conn.Close()
			s.serveConn(ctx, conn)
		}()
	}
}

// serveConn reads one connection until it ends, goes idle or sends something unframeable.
func (s *server) serveConn(ctx context.Context, conn net.Conn) {
	remote := conn.RemoteAddr().String()
	s.log.Info("connection opened", "remote", remote)
	err := s.readStream(ctx, &idleReader{conn: conn, timeout: s.idleTimeout}, remote)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("connection closed on error", "remote", remote, "err", err)
		s.streamFailed()
		return
	}
	s.log.Info("connection closed", "remote", remote)
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
