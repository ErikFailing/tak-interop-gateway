package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/health"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/publish/latticesdk"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// serveUsage heads the flag list for `takgw serve -h`.
const serveUsage = `usage: takgw serve -source NAME (-stdin | -listen HOST:PORT | -connect HOST:PORT) [flags]

Runs the gateway: reads CoT, maps each event to a Lattice entity, drops repeats, late arrivals and unchanged reports,
checks what's left with Entity Preflight, and publishes it. By default each entity is written as one JSON line to
-out, the format "takgw preflight" reads. With -lattice it is published to the Lattice environment named by
LATTICE_ENDPOINT, ENVIRONMENT_TOKEN and, for Sandboxes, SANDBOXES_TOKEN.

Where the CoT comes from:
  -stdin             one stream on standard input, until it ends.
  -listen HOST:PORT  streams from TAK clients or servers that connect in. With -tls-cert, -tls-key and -tls-ca, only
                     clients presenting a certificate signed by -tls-ca are accepted (mutual TLS). Without them it is
                     plain TCP with no authentication: anyone who can reach the port can inject tracks.
  -connect HOST:PORT a TAK server's streaming TLS port (often 8089), connected to as a client with -tls-cert and
                     -tls-key, trusting only -tls-ca for the server. Reconnects with backoff. It never writes to the
                     server. Certificate files are PEM; convert a TAK .p12 with openssl.

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
	connect     string
	tls         *tls.Config    // the listener's or the dialer's TLS; nil for plain TCP
	health      *health.Config // the gateway's own asset; nil when -health-position isn't given
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
	frames := make(chan frame, frameQueue)
	s := &server{log: log, frames: frames, maxConns: cfg.maxConns, idleTimeout: cfg.idleTimeout}
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	var readers sync.WaitGroup
	readers.Add(1)
	switch {
	case cfg.stdin:
		go func() {
			defer readers.Done()
			if err := s.readStream(readCtx, stdin, "stdin", "stdin"); err != nil {
				log.Error("stdin stream", "err", err)
				s.streamFailed()
			}
		}()
	case cfg.connect != "":
		log.Info("connecting", "addr", cfg.connect)
		go func() {
			defer readers.Done()
			s.connectLoop(readCtx, cfg.connect, cfg.tls)
		}()
	default:
		ln, err := net.Listen("tcp", cfg.listen)
		if err != nil {
			fmt.Fprintf(stderr, "takgw serve: listen: %v\n", err)
			return 1
		}
		if cfg.tls != nil {
			ln = tls.NewListener(ln, cfg.tls)
			log.Info("listening with mutual TLS", "addr", ln.Addr().String())
		} else {
			warnIfExposed(log, ln.Addr())
			log.Info("listening", "addr", ln.Addr().String())
		}
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
	publishFailed := func(id string, err error) bool {
		if fatal(err) {
			log.Error("publish failed, stopping", "err", err)
			code = 1
			return false
		}
		log.Warn("publish failed", "entity", id, "err", err)
		return true
	}
	process := func(ctx context.Context, f frame) bool {
		c.messages++
		res, err := p.Process(ctx, f.data, f.origin)
		if count(&c, log, res, err) {
			return publishFailed(idOf(res.Entity), err)
		}
		return true
	}

	// The gateway's own asset goes out at once, then every interval, and once more, as offline, after a clean stop. It
	// is checked by preflight like any entity, and goes through the same publisher.
	self := func(ctx context.Context, e entity.Entity) bool {
		if vs := preflight.NewChecker(time.Now).Check(e); preflight.HasErrors(vs) {
			log.Error("the gateway's own asset fails preflight; not publishing it", "findings", fmt.Sprint(vs))
			return true
		}
		if err := cfg.pipeline.Publisher.Publish(ctx, e); err != nil {
			c.publishErr++
			return publishFailed(idOf(e), err)
		}
		return true
	}
	var monitor *health.Monitor
	var tick <-chan time.Time
	if cfg.health != nil {
		var err error
		if monitor, err = health.New(*cfg.health); err != nil {
			fmt.Fprintf(stderr, "takgw serve: %v\n", err)
			return 2
		}
		ticker := time.NewTicker(cfg.health.Interval)
		defer ticker.Stop()
		tick = ticker.C
		log.Info("publishing the gateway's own asset", "entity", monitor.ID(), "every", cfg.health.Interval)
		if !self(ctx, monitor.Snapshot(time.Now(), c.totals(), s.input(cfg))) {
			return code
		}
	}
loop:
	for {
		select {
		case f, ok := <-frames:
			if !ok || !process(ctx, f) {
				break loop
			}
		case <-tick:
			if !self(ctx, monitor.Snapshot(time.Now(), c.totals(), s.input(cfg))) {
				break loop
			}
		case <-ctx.Done():
			// On a stop, finish what has already been received, but don't wait for more. Those publishes must not
			// inherit the cancellation, or an adapter that honors its context would fail them all.
			drain := context.WithoutCancel(ctx)
			for {
				select {
				case f, ok := <-frames:
					if !ok || !process(drain, f) {
						break loop
					}
				default:
					break loop
				}
			}
		}
	}

	// 4: Stop the readers. Network readers always finish once their connections are closed, so wait for them, and
	// their last log lines, before summarizing. A stdin reader may be blocked in a read nothing can interrupt; at a
	// clean end of input it has already finished, and otherwise the process is about to exit anyway.
	stopReading()
	if !cfg.stdin {
		readers.Wait()
	}

	// 5: Say goodbye on the operating picture, unless publishing is what failed.
	if monitor != nil && code == 0 && self(context.WithoutCancel(ctx), monitor.Offline(time.Now())) {
		log.Info("published the gateway's own asset as offline", "entity", monitor.ID())
	}

	// 6: Summarize. A malformed stdin stream is a failure; a bad TCP connection is only that connection's problem.
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
	state.ErrImplausible: "implausible", state.ErrOriginConflict: "origin-conflict",
}

// totals turns the counters into what the health monitor reads.
func (c *counters) totals() health.Totals {
	return health.Totals{
		Messages: c.messages, Published: c.published, Unparsable: c.parse, Rejected: c.rejected,
		PublishErrors: c.publishErr, Implausible: c.dropped["implausible"], OriginConflicts: c.dropped["origin-conflict"],
		Full: c.dropped["full"],
	}
}

// drops formats the drop counts as "3 dropped (repeated 1, unchanged 2)", in a fixed order.
func drops(m map[string]int) string {
	n := 0
	var parts []string
	for _, reason := range []string{"repeated", "out-of-order", "unchanged", "full", "implausible", "origin-conflict"} {
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
		switch reason {
		case "full":
			log.Warn("dropped a new entity: the state stage is full", "entity", id)
		case "implausible", "origin-conflict":
			// Feed integrity: these are the drops an operator should see, whatever the log level.
			log.Warn("dropped a suspect report", "entity", id, "name", name(res.Entity), "reason", reason, "err", err)
		default:
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
	listen := fs.String("listen", "", "accept CoT streams on this host:port: mutual TLS with the -tls flags, otherwise plain TCP")
	connect := fs.String("connect", "", "connect to a TAK server's streaming TLS port at this host:port (needs the -tls flags)")
	tlsCert := fs.String("tls-cert", "", "this side's certificate, PEM")
	tlsKey := fs.String("tls-key", "", "the private key for -tls-cert, PEM")
	tlsCA := fs.String("tls-ca", "", "the authority, PEM, that the other side's certificate must be signed by")
	tlsServerName := fs.String("tls-server-name", "", "with -connect, the name the server's certificate must carry, if not the host dialed")
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
	healthPosition := fs.String("health-position", "", "publish the gateway itself as an asset at lat,lon[,hae]: where it runs")
	healthName := fs.String("health-name", "", `the gateway asset's display name (default "takgw (SOURCE)")`)
	healthInterval := fs.Duration("health-interval", health.MaxInterval, "how often to republish the gateway asset, at most 30s")
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
	case howMany(*useStdin, *listen != "", *connect != "") != 1:
		return usage("choose exactly one of -stdin, -listen or -connect")
	case *maxConns < 1:
		return usage("-max-conns must be at least 1")
	case *idle <= 0:
		return usage("-idle-timeout must be positive")
	case *lattice && flagSet(fs, "out"):
		return usage("choose -lattice or -out, not both")
	}

	// TLS: all three files or none, only for a network input, and always for -connect.
	tlsFlags := howMany(*tlsCert != "", *tlsKey != "", *tlsCA != "")
	var tlsConfig *tls.Config
	switch {
	case tlsFlags != 0 && tlsFlags != 3:
		return usage("-tls-cert, -tls-key and -tls-ca go together: give all three")
	case tlsFlags == 3 && *useStdin:
		return usage("the -tls flags need -listen or -connect")
	case tlsFlags == 0 && *connect != "":
		return usage("-connect needs -tls-cert, -tls-key and -tls-ca: TAK servers take mutual TLS")
	case *tlsServerName != "" && *connect == "":
		return usage("-tls-server-name needs -connect")
	case tlsFlags == 3 && *connect != "":
		var err error
		if tlsConfig, err = transport.ClientTLS(*tlsCert, *tlsKey, *tlsCA, *tlsServerName); err != nil {
			return usage("%v", err)
		}
	case tlsFlags == 3:
		var err error
		if tlsConfig, err = transport.ServerTLS(*tlsCert, *tlsKey, *tlsCA); err != nil {
			return usage("%v", err)
		}
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
		stdin: *useStdin, listen: *listen, connect: *connect, tls: tlsConfig, out: *out, lattice: *lattice,
		maxConns: *maxConns, idleTimeout: *idle, verbose: *verbose,
	}

	// The gateway's own asset, when asked for.
	switch {
	case *healthPosition != "":
		lat, lon, hae, err := parsePosition(*healthPosition)
		if err != nil {
			return usage("-health-position: %v", err)
		}
		cfg.health = &health.Config{
			Source: *source, Name: *healthName, Latitude: lat, Longitude: lon, Altitude: hae, Interval: *healthInterval,
			IntegrationName: *integration, Simulated: *simulated, Classification: level, Input: inputName(cfg),
		}
		if _, err := health.New(*cfg.health); err != nil {
			return usage("%v", err)
		}
	case *healthName != "" || flagSet(fs, "health-interval"):
		return usage("-health-name and -health-interval need -health-position")
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

// parsePosition reads "lat,lon" or "lat,lon,hae" in degrees and meters. health.New checks the ranges.
func parsePosition(s string) (lat, lon float64, hae *float64, err error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, nil, fmt.Errorf("want lat,lon or lat,lon,hae, got %q", s)
	}
	var v [3]float64
	for i, p := range parts {
		if v[i], err = strconv.ParseFloat(strings.TrimSpace(p), 64); err != nil {
			return 0, 0, nil, fmt.Errorf("%q is not a number", p)
		}
	}
	if len(parts) == 3 {
		if math.IsNaN(v[2]) || math.IsInf(v[2], 0) {
			return 0, 0, nil, fmt.Errorf("altitude %q is not a finite number", parts[2])
		}
		hae = new(v[2])
	}
	return v[0], v[1], hae, nil
}

// inputName describes the feed, for the gateway asset's sensor.
func inputName(cfg serveConfig) string {
	switch {
	case cfg.connect != "":
		return "TAK server " + cfg.connect
	case cfg.listen != "" && cfg.tls != nil:
		return "TAK clients on " + cfg.listen + " (mutual TLS)"
	case cfg.listen != "":
		return "TAK clients on " + cfg.listen
	}
	return "stdin"
}

// howMany counts the true values.
func howMany(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
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

// frame is one framed CoT message and where it came from.
type frame struct {
	data []byte
	// origin identifies the sender, for the state stage's origin binding: the client certificate's name under mutual
	// TLS, the remote address's IP on plain TCP, "stdin", or the TAK server's address for -connect, where every
	// report arrives on the server's one connection.
	origin string
}

// server holds what the readers share.
type server struct {
	log         *slog.Logger
	frames      chan<- frame
	maxConns    int
	idleTimeout time.Duration

	mu        sync.Mutex
	streamErr int
	conns     int  // open connections, with -listen
	connected bool // whether the TAK server connection is up, with -connect
}

// adjustConns changes the count of open connections.
func (s *server) adjustConns(delta int) {
	s.mu.Lock()
	s.conns += delta
	s.mu.Unlock()
}

// setConnected records whether the TAK server connection is up.
func (s *server) setConnected(up bool) {
	s.mu.Lock()
	s.connected = up
	s.mu.Unlock()
}

// input reports the feed's state for the health asset.
func (s *server) input(cfg serveConfig) health.Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case cfg.connect != "" && s.connected:
		return health.Input{Up: true, Detail: "connected to " + cfg.connect}
	case cfg.connect != "":
		return health.Input{Up: false, Detail: "not connected to " + cfg.connect + "; reconnecting"}
	case cfg.listen != "":
		return health.Input{Up: true, Detail: fmt.Sprintf("listening on %s, %d connections open", cfg.listen, s.conns)}
	}
	return health.Input{Up: true, Detail: "reading stdin"}
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
func (s *server) readStream(ctx context.Context, r io.Reader, name, origin string) error {
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
		case s.frames <- frame{data: msg, origin: origin}:
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

// serveConn reads one connection until it ends, goes idle or sends something unframeable. A TLS connection must
// finish its handshake, client certificate and all, within handshakeTimeout, so a client that stalls mid-handshake
// can't hold one of the -max-conns slots for the whole idle timeout.
func (s *server) serveConn(ctx context.Context, conn net.Conn) {
	// 1: Complete a TLS handshake, and work out the connection's origin: its certificate's name under mutual TLS, or
	// else its IP address, without the port, so a client that reconnects keeps its origin.
	remote := conn.RemoteAddr().String()
	origin, _, _ := net.SplitHostPort(remote)
	attrs := []any{"remote", remote}
	if tc, ok := conn.(*tls.Conn); ok {
		hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("TLS handshake failed", "remote", remote, "err", err)
				s.streamFailed()
			}
			return
		}
		origin = "cert:" + transport.PeerName(tc.ConnectionState())
		attrs = append(attrs, "client", transport.PeerName(tc.ConnectionState()))
	}

	// 2: Read until the connection ends.
	s.adjustConns(1)
	defer s.adjustConns(-1)
	s.log.Info("connection opened", attrs...)
	err := s.readStream(ctx, &idleReader{conn: conn, timeout: s.idleTimeout}, remote, origin)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("connection closed on error", "remote", remote, "err", err)
		s.streamFailed()
		return
	}
	s.log.Info("connection closed", "remote", remote)
}

// handshakeTimeout bounds a TLS handshake, in either direction.
const handshakeTimeout = 10 * time.Second

// The reconnect backoff for -connect: it starts at minBackoff, doubles after each failed attempt up to maxBackoff, and
// starts over after a connection that stayed up for resetBackoff. They're variables so tests can shorten them.
var (
	minBackoff   = time.Second
	maxBackoff   = time.Minute
	resetBackoff = time.Minute
)

// connectLoop keeps a connection to a TAK server open until ctx is done, reconnecting with backoff whenever it fails
// or the server closes it. Each failed attempt counts as a stream error.
func (s *server) connectLoop(ctx context.Context, addr string, cfg *tls.Config) {
	delay := minBackoff
	for {
		// 1: Connect and read until the connection ends.
		started := time.Now()
		err := s.connectOnce(ctx, addr, cfg)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.streamFailed()
		}

		// 2: Back off before trying again, unless the connection had been healthy for a while.
		if time.Since(started) >= resetBackoff {
			delay = minBackoff
		}
		wait := jitter(delay)
		s.log.Warn("TAK server connection ended; reconnecting", "addr", addr, "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		delay = min(2*delay, maxBackoff)
	}
}

// connectOnce dials the server, completes the TLS handshake, and reads until the connection ends. It returns nil when
// the server closed the connection cleanly.
func (s *server) connectOnce(ctx context.Context, addr string, cfg *tls.Config) error {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: handshakeTimeout}, Config: cfg}
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	conn, err := d.DialContext(hctx, "tcp", addr) // returns once the handshake is done
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // a stop unblocks the read
	defer stop()
	s.log.Info("connected", "addr", addr, "server", transport.PeerName(conn.(*tls.Conn).ConnectionState()))
	s.setConnected(true)
	defer s.setConnected(false)
	return s.readStream(ctx, &idleReader{conn: conn, timeout: s.idleTimeout}, addr, addr)
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
