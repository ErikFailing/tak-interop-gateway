// Command cot-gen writes a synthetic, clearly labeled search-and-rescue scenario as Cursor-on-Target events, one XML
// event per line, to stdout or to a TCP listener such as a local TAK server. Every event is marked as simulated.
//
// Examples:
//
//	cot-gen -ticks 3                                # three ticks to stdout, all at once
//	cot-gen -tcp 127.0.0.1:8087 -realtime -ticks 0  # stream to a local TAK server until Ctrl-C
package main

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
)

func main() {
	// Cancel the context on Ctrl-C so a realtime stream stops cleanly between ticks.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the flags, builds the scenario and writes it out, returning the process exit code: 0 on success, 1 for
// a runtime failure, 2 for bad arguments. It takes its streams as parameters so tests can capture them.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cot-gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	units := fs.Int("units", 4, "search team members")
	ticks := fs.Int("ticks", 10, "ticks to generate; 0 means until interrupted (needs -realtime)")
	interval := fs.Duration("interval", 5*time.Second, "simulated time between ticks")
	stale := fs.Duration("stale", 2*time.Minute, "how long each report stays valid")
	origin := fs.String("origin", "40.3428,-105.6836", "where the team starts, as lat,lon")
	seed := fs.Uint64("seed", 1, "random seed; the same seed gives the same scenario")
	start := fs.String("start", "", "timestamp of the first tick in RFC 3339 (default: now)")
	tcpAddr := fs.String("tcp", "", "send to this host:port over TCP instead of stdout")
	realtime := fs.Bool("realtime", false, "wait -interval between ticks instead of writing them all at once")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// 1: Turn the flags into a scenario config, rejecting combinations that make no sense.
	if *ticks < 0 {
		return fail(stderr, 2, "-ticks must be 0 or more")
	}
	if *ticks == 0 && !*realtime {
		return fail(stderr, 2, "-ticks 0 runs until interrupted, so it needs -realtime")
	}
	pos, err := parseLatLon(*origin)
	if err != nil {
		return fail(stderr, 2, "-origin: %v", err)
	}
	startTime := time.Now().UTC()
	if *start != "" {
		if startTime, err = time.Parse(time.RFC3339, *start); err != nil {
			return fail(stderr, 2, "-start: %v", err)
		}
	}
	scenario, err := cotgen.New(cotgen.Config{
		Units: *units, Origin: pos, Start: startTime, Interval: *interval, Stale: *stale, Seed: *seed,
	})
	if err != nil {
		return fail(stderr, 2, "%v", err)
	}

	// 2: Pick the destination. A TCP connection gets the same newline-separated events as stdout.
	out := stdout
	if *tcpAddr != "" {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", *tcpAddr)
		if err != nil {
			return fail(stderr, 1, "connect: %v", err)
		}
		defer conn.Close()
		out = conn
	}
	w := bufio.NewWriter(out)

	// 3: Write tick after tick, flushing each one so a realtime stream arrives as it happens.
	var pace <-chan time.Time
	if *realtime {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		pace = ticker.C
	}
	for i := 0; *ticks == 0 || i < *ticks; i++ {
		if i > 0 && pace != nil {
			select {
			case <-ctx.Done():
				return 0 // interrupted between ticks: a clean stop, not an error
			case <-pace:
			}
		}
		for _, ev := range scenario.Next() {
			b, err := xml.Marshal(ev)
			if err != nil {
				return fail(stderr, 1, "marshal: %v", err)
			}
			// A bufio.Writer remembers its first error and returns it from Flush, so checking once per tick is enough.
			w.Write(b)
			w.WriteByte('\n')
		}
		if err := w.Flush(); err != nil {
			return fail(stderr, 1, "write: %v", err)
		}
	}
	return 0
}

// parseLatLon reads "lat,lon" in degrees. Range checks happen in cotgen.Config.Validate.
func parseLatLon(s string) (cotgen.LatLon, error) {
	latStr, lonStr, ok := strings.Cut(s, ",")
	if !ok {
		return cotgen.LatLon{}, fmt.Errorf("want lat,lon, got %q", s)
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(latStr), 64)
	if err != nil {
		return cotgen.LatLon{}, fmt.Errorf("latitude: %w", err)
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(lonStr), 64)
	if err != nil {
		return cotgen.LatLon{}, fmt.Errorf("longitude: %w", err)
	}
	return cotgen.LatLon{Lat: lat, Lon: lon}, nil
}

// fail prints a message prefixed with the command name and returns code, so error paths in run stay one line each.
func fail(stderr io.Writer, code int, format string, args ...any) int {
	fmt.Fprintf(stderr, "cot-gen: "+format+"\n", args...)
	return code
}
