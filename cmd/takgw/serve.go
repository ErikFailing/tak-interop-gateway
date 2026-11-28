package main

// serve.go runs the gateway's pipeline goroutine. serve_flags.go reads its flags, and ingest.go is its input side.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/health"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/publish/latticesdk"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// frameQueue is how many framed messages may wait for the pipeline. When it is full, readers stop reading, so a fast
// sender is slowed to the gateway's pace by TCP itself rather than by unbounded memory.
const frameQueue = 256

// stopGrace is how long publishes may carry on after a stop, for the ones under way and the messages already queued.
// offlineTimeout bounds the gateway asset's last publish, as offline, after that.
const (
	stopGrace      = 5 * time.Second
	offlineTimeout = 5 * time.Second
)

// errSelfInvalid means the gateway's own asset failed preflight: a bug in the gateway, not a problem with its input.
var errSelfInvalid = errors.New("the gateway's own asset fails preflight")

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
	var monitor *health.Monitor
	if cfg.health != nil {
		if monitor, err = health.New(*cfg.health); err != nil {
			fmt.Fprintf(stderr, "takgw serve: %v\n", err)
			return 2
		}
	}

	// 2: Publishes outlive a stop by stopGrace: a stop finishes what has already been received, and those publishes
	// must not inherit the cancellation, or an adapter that honors its context would fail them all. After the grace
	// period they are cancelled too, so a platform that has stopped answering can't hold the stop up.
	pubCtx, cancelPub := graceContext(ctx, stopGrace)
	defer cancelPub()

	// 3: Bind the listener, without accepting yet, so a port that can't be had fails before anything is published.
	frames := make(chan frame, frameQueue)
	in := &ingest{log: log, frames: frames, connectAddr: cfg.connect, maxConns: cfg.maxConns, idleTimeout: cfg.idleTimeout}
	var ln net.Listener
	if cfg.listen != "" {
		if ln, err = net.Listen("tcp", cfg.listen); err != nil {
			fmt.Fprintf(stderr, "takgw serve: listen: %v\n", err)
			return 1
		}
		defer ln.Close() // the accept loop closes it on a stop; this covers returning before the loop starts
		in.listenAddr = ln.Addr().String()
		if cfg.tls != nil {
			ln = tls.NewListener(ln, cfg.tls)
			log.Info("listening with mutual TLS", "addr", in.listenAddr)
		} else {
			warnIfExposed(log, ln.Addr())
			log.Info("listening", "addr", in.listenAddr)
		}
	}

	// 4: What a failed publish does: a fatal one stops the gateway with status 1, and any other is logged.
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

	// 5: The gateway's own asset goes through the same publisher as the tracks, after a preflight check of its own.
	self := func(ctx context.Context, e entity.Entity) error {
		if vs := preflight.Check(e, time.Now()); preflight.HasErrors(vs) {
			return fmt.Errorf("%w: %v", errSelfInvalid, vs)
		}
		if err := cfg.pipeline.Publisher.Publish(ctx, e); err != nil {
			c.publishErrors++
			return err
		}
		return nil
	}
	// selfFailed handles a failed publish of the asset, and reports whether the gateway should carry on.
	selfFailed := func(err error) bool {
		if errors.Is(err, errSelfInvalid) {
			log.Error("not publishing the gateway's own asset", "err", err)
			return true
		}
		return publishFailed(monitor.ID(), err)
	}
	snapshot := func() entity.Entity {
		_, suspect := in.streamCounts()
		return monitor.Snapshot(time.Now(), c.totals(suspect), in.input())
	}

	// 6: The asset goes out once now, before any input is read, so credentials the platform refuses stop the gateway
	// before it accepts a single connection. Then it goes out every interval.
	var tick <-chan time.Time
	if monitor != nil {
		log.Info("publishing the gateway's own asset", "entity", monitor.ID(), "every", cfg.health.Interval)
		if err := self(pubCtx, snapshot()); err != nil && !selfFailed(err) {
			return code
		}
		ticker := time.NewTicker(cfg.health.Interval)
		defer ticker.Stop()
		tick = ticker.C
	}

	// 7: Start the readers. Each sends framed messages to one queue, which only this goroutine reads.
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	var readers sync.WaitGroup
	switch {
	case cfg.stdin:
		readers.Go(func() {
			if err := in.readStream(readCtx, stdin, "stdin", "stdin"); err != nil {
				log.Error("stdin stream", "err", err)
				in.streamFailed(err)
			}
		})
	case cfg.connect != "":
		log.Info("connecting", "addr", cfg.connect)
		readers.Go(func() { in.connectLoop(readCtx, cfg.tls) })
	default:
		readers.Go(func() { in.acceptLoop(readCtx, ln) })
	}
	go func() {
		readers.Wait()
		close(frames)
	}()

	// 8: Process messages in arrival order until the readers are done, the context ends, or a publish fails fatally.
	process := func(f frame) bool {
		c.messages++
		res, err := p.Process(pubCtx, f.data, f.origin)
		if c.count(log, res, err) {
			return publishFailed(idOf(res.Entity), err)
		}
		return true
	}
loop:
	for {
		select {
		case f, ok := <-frames:
			if !ok || !process(f) {
				break loop
			}
		case <-tick:
			if err := self(pubCtx, snapshot()); err != nil && !selfFailed(err) {
				break loop
			}
		case <-ctx.Done():
			// On a stop, finish what has already been received, within the grace period, but don't wait for more.
			for pubCtx.Err() == nil {
				select {
				case f, ok := <-frames:
					if !ok || !process(f) {
						break loop
					}
				default:
					break loop
				}
			}
			break loop
		}
	}

	// 9: Stop the readers. Network readers always finish once their connections are closed, so wait for them, and
	// their last log lines, before summarizing. A stdin reader may be blocked in a read nothing can interrupt; at a
	// clean end of input it has already finished, and otherwise the process is about to exit anyway.
	stopReading()
	if !cfg.stdin {
		readers.Wait()
	}
	if n := len(frames); n > 0 {
		log.Warn("stopped with messages received but not processed", "count", n)
	}

	// 10: Say goodbye on the operating picture, unless publishing is what failed.
	if monitor != nil && code == 0 {
		offCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), offlineTimeout)
		err := self(offCtx, monitor.Offline(time.Now()))
		cancel()
		if err == nil {
			log.Info("published the gateway's own asset as offline", "entity", monitor.ID())
		} else {
			selfFailed(err)
		}
	}

	// 11: Summarize. A malformed stdin stream is a failure; a bad TCP connection is only that connection's problem.
	streamErrors, _ := in.streamCounts()
	if cfg.stdin && streamErrors > 0 && code == 0 {
		code = 1
	}
	fmt.Fprintf(stderr, "takgw serve: %s\n", c.summary(streamErrors, p.NumTracked()))
	return code
}

// graceContext returns a context that outlives ctx by grace: it is cancelled grace after ctx is, or when cancel is
// called, whichever comes first.
func graceContext(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	gctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(grace, cancel) })
	return gctx, func() {
		stop()
		cancel()
	}
}

// dropReason is one reason the state stage drops a report: its name in logs and the summary, and the warning an
// operator should see whatever the log level, or "" for routine traffic shaping, logged only with -v.
type dropReason struct {
	err  error
	name string
	warn string
}

// dropReasons lists every state-stage drop, in the order the summary gives them.
var dropReasons = []dropReason{
	{state.ErrRepeated, "repeated", ""},
	{state.ErrOutOfOrder, "out-of-order", ""},
	{state.ErrUnchanged, "unchanged", ""},
	{state.ErrFull, "entity-limit", "dropped a new entity: already tracking -max-entities"},
	{state.ErrFutureTime, "future-time", "dropped a suspect report"},
	{state.ErrImplausible, "implausible", "dropped a suspect report"},
	{state.ErrOriginConflict, "origin-conflict", "dropped a suspect report"},
}

// otherDrop names a drop that no dropReason matches, which would mean a new state-stage sentinel missing from the list.
const otherDrop = "other"

// counters tallies what happened to every message, for the summary line and the health asset. Only the pipeline
// goroutine touches it.
type counters struct {
	messages, published, unparsable, unmappable, rejected, publishErrors int
	dropped                                                              map[string]int // by dropReason name
}

// count records one outcome and logs it at debug level, or warns for what an operator should see. It returns true
// if publishing failed, which the caller decides what to do about.
func (c *counters) count(log *slog.Logger, res pipeline.Result, err error) (publishFailed bool) {
	id := idOf(res.Entity)
	switch {
	case err == nil:
		c.published++
		for _, v := range res.Violations {
			log.Warn("published with a finding", "entity", id, "finding", v.String())
		}
		log.Debug("published", "entity", id, "name", name(res.Entity))
	case errors.Is(err, pipeline.ErrDropped):
		r := dropReason{name: otherDrop}
		for _, d := range dropReasons {
			if errors.Is(err, d.err) {
				r = d
				break
			}
		}
		c.dropped[r.name]++
		if r.warn != "" {
			log.Warn(r.warn, "entity", id, "name", name(res.Entity), "reason", r.name, "err", err)
		} else {
			log.Debug("dropped", "entity", id, "reason", r.name)
		}
	case errors.Is(err, pipeline.ErrParse):
		c.unparsable++
		log.Debug("unparsable message", "err", err)
	case errors.Is(err, pipeline.ErrMap):
		c.unmappable++
		log.Debug("unmappable event", "err", err)
	case errors.Is(err, pipeline.ErrRejected):
		c.rejected++
		log.Warn("rejected by preflight", "entity", id, "err", err)
	case errors.Is(err, pipeline.ErrPublish):
		c.publishErrors++
		return true
	}
	return false
}

// totals turns the counters into what the health monitor reads. suspectStreams comes from the readers.
func (c *counters) totals(suspectStreams int) health.Totals {
	return health.Totals{
		Messages: c.messages, Published: c.published, Unparsable: c.unparsable, Rejected: c.rejected,
		PublishErrors: c.publishErrors, Implausible: c.dropped["implausible"], OriginConflicts: c.dropped["origin-conflict"],
		FutureTimes: c.dropped["future-time"], EntityLimit: c.dropped["entity-limit"], SuspectStreams: suspectStreams,
	}
}

// summary is the line serve prints when it stops.
func (c *counters) summary(streamErrors, tracked int) string {
	return fmt.Sprintf("%d messages: %d published, %s, %d unparsable, %d unmappable, %d rejected by preflight, "+
		"%d publish errors, %d stream errors; %d entities tracked",
		c.messages, c.published, c.drops(), c.unparsable, c.unmappable, c.rejected, c.publishErrors, streamErrors, tracked)
}

// drops formats the drop counts as "3 dropped (repeated 1, unchanged 2)", in dropReasons order.
func (c *counters) drops() string {
	n := 0
	var parts []string
	for _, reason := range append(reasonNames(), otherDrop) {
		if k := c.dropped[reason]; k > 0 {
			n += k
			parts = append(parts, fmt.Sprintf("%s %d", reason, k))
		}
	}
	if n == 0 {
		return "0 dropped"
	}
	return fmt.Sprintf("%d dropped (%s)", n, strings.Join(parts, ", "))
}

// reasonNames returns the dropReasons' names, in order.
func reasonNames() []string {
	names := make([]string, len(dropReasons))
	for i, d := range dropReasons {
		names[i] = d.name
	}
	return names
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
