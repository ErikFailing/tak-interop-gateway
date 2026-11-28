package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/latticefake"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/publish/latticesdk"
	"github.com/erikfailing/tak-interop-gateway/preflight"
	"github.com/erikfailing/tak-interop-gateway/readback"
)

// readbackUsage heads the flag list for `takgw readback -h`.
const readbackUsage = `usage: takgw readback [-live] [flags]

Publishes the gateway's own test entities, all marked simulated and built by the real parser and mapper, reads each
one back in a separate request, and reports every field that came back different from what was sent. A field the
platform dropped or changed fails the run; a field it added, such as createdTime, is reported but allowed.

Without -live it runs against an in-process fake of the Lattice REST API, through the real SDK adapter, so it shows
the whole path working with no environment. With -live it uses the environment named by LATTICE_ENDPOINT,
ENVIRONMENT_TOKEN and, for Sandboxes, SANDBOXES_TOKEN: use a development sandbox you own. The test entities expire
after -expiry.

Exit status: 0 when every entity came back as sent, 1 when any didn't or the run failed, 2 for a usage or config
error.

flags:
`

// newLiveClient builds the SDK adapter for the environment in the process environment. Tests replace it with one
// pointed at a fake.
var newLiveClient = func() (*latticesdk.Client, error) {
	cfg, err := latticesdk.ConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	return latticesdk.New(cfg)
}

// runReadback implements `takgw readback` and returns the exit status.
func runReadback(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// 1: Parse the flags.
	fs := flag.NewFlagSet("readback", flag.ContinueOnError)
	fs.SetOutput(stderr)
	live := fs.Bool("live", false, "use the environment in LATTICE_ENDPOINT, ENVIRONMENT_TOKEN and SANDBOXES_TOKEN instead of the in-process fake")
	source := fs.String("source", "takgw-readback", "feed name for the test entities; part of their IDs")
	wait := fs.Duration("wait", 2*time.Second, "with -live, how long to wait between publishing and reading back")
	expiry := fs.Duration("expiry", 2*time.Minute, "how long the test entities stay live")
	verbose := fs.Bool("v", false, "list every difference, including fields the platform added")
	fs.Usage = func() {
		fmt.Fprint(stderr, readbackUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "takgw readback: unexpected arguments: %v\n", fs.Args())
		return 2
	case *expiry < 10*time.Second || *expiry >= 30*24*time.Hour:
		fmt.Fprintln(stderr, "takgw readback: -expiry must be at least 10s and under 30 days")
		return 2
	case *wait < 0:
		fmt.Fprintln(stderr, "takgw readback: -wait must not be negative")
		return 2
	}

	// 2: Build the test entities, and refuse to publish any that preflight finds fault with.
	entities, err := testEntities(*source, time.Now().UTC(), *expiry)
	if err != nil {
		fmt.Fprintf(stderr, "takgw readback: %v\n", err)
		return 1
	}

	// 3: Connect to the environment, or start the fake.
	var client *latticesdk.Client
	if *live {
		if client, err = newLiveClient(); err != nil {
			fmt.Fprintf(stderr, "takgw readback: -live: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "takgw readback: live, publishing %d simulated entities that expire in %v\n", len(entities), *expiry)
	} else {
		var stop func()
		if client, stop, err = startFake(); err != nil {
			fmt.Fprintf(stderr, "takgw readback: %v\n", err)
			return 1
		}
		defer stop()
		*wait = 0 // the fake stores synchronously
		fmt.Fprintln(stdout, "takgw readback: offline, against an in-process fake of the Lattice REST API (-live for a real environment)")
	}

	// 4: Publish, read back, and report.
	results := readback.Run(ctx, client, entities, *wait)
	passed := 0
	for _, r := range results {
		report(stdout, r, *verbose)
		if r.Passed() {
			passed++
		}
	}
	fmt.Fprintf(stdout, "takgw readback: %d entities: %d passed, %d failed\n", len(results), passed, len(results)-passed)
	if passed != len(results) {
		return 1
	}
	return 0
}

// report prints one entity's result: a PASS or FAIL line, then its differences. Added fields are listed only with
// -v, and otherwise counted.
func report(w io.Writer, r readback.Result, verbose bool) {
	verdict := "PASS"
	if !r.Passed() {
		verdict = "FAIL"
	}
	added := 0
	for _, d := range r.Differences {
		if d.Kind == readback.Added {
			added++
		}
	}
	line := fmt.Sprintf("%s  %-22s %s", verdict, r.Name, r.EntityID)
	if added > 0 && !verbose {
		line += fmt.Sprintf("  (%d field(s) added by the platform; -v lists them)", added)
	}
	fmt.Fprintln(w, line)
	if r.Err != nil {
		fmt.Fprintf(w, "      %v\n", r.Err)
	}
	for _, d := range r.Differences {
		if d.Kind != readback.Added || verbose {
			fmt.Fprintf(w, "      %s\n", d)
		}
	}
}

// testEntities builds the gateway's own test entities: one tick of the cot-gen scenario, plus three events that reach
// mapper branches the scenario doesn't, all through the real parser and mapper, marked simulated, and stale after
// expiry. It fails if preflight finds anything at all, so readback never publishes an entity the gateway itself would
// hold back.
func testEntities(source string, now time.Time, expiry time.Duration) ([]entity.Entity, error) {
	// 1: The events: two walkers and the last-known-position marker, then the extras.
	s, err := cotgen.New(cotgen.Config{
		Units: 2, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: now,
		Interval: 5 * time.Second, Stale: expiry, Seed: 1,
	})
	if err != nil {
		return nil, err
	}
	events := s.Next()
	extra := func(uid, typ string, point cot.Point, detail *cot.Detail) cot.Event {
		return cot.Event{
			Version: cot.Version, UID: uid, Type: typ, How: "h-e",
			Time: cot.Timestamp(now), Start: cot.Timestamp(now), Stale: cot.Timestamp(now.Add(expiry)),
			Point: point, Detail: detail,
		}
	}
	events = append(events,
		// An exercise faker (exercise flag, hostile) in the air, with an altitude and no detail, so its uid is its name.
		extra("readback-faker-1", "a-k-A-M-F", cot.Point{Lat: 40.35, Lon: -105.69, HAE: 3200, CE: cot.Unknown, LE: cot.Unknown}, nil),
		// A type that doesn't transliterate, so it gets the generic symbol.
		extra("readback-generic-1", "a-f-G-UC", cot.Point{Lat: 40.34, Lon: -105.68, HAE: cot.Unknown, CE: 25, LE: cot.Unknown},
			&cot.Detail{Contact: &cot.Contact{Callsign: "SIM-READBACK-GENERIC"}}),
		// A neutral surface vessel under way, for a velocity pointing east.
		extra("readback-vessel-1", "a-n-S-X-M", cot.Point{Lat: 40.33, Lon: -105.67, HAE: 0, CE: 50, LE: cot.Unknown},
			&cot.Detail{Contact: &cot.Contact{Callsign: "SIM-READBACK-VESSEL"}, Track: &cot.Track{Course: 90, Speed: 5}}),
	)

	// 2: Through the parser and the mapper, as the gateway would take them, then preflight with no exceptions.
	cfg := mapping.Config{Source: source, Simulated: true, TrackQuality: mapping.DefaultTrackQuality}
	var out []entity.Entity
	for _, ev := range events {
		b, err := xml.Marshal(ev)
		if err != nil {
			return nil, err
		}
		parsed, err := cot.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("test event %s: %w", ev.UID, err)
		}
		e, err := mapping.Map(parsed, cfg, now)
		if err != nil {
			return nil, fmt.Errorf("test event %s: %w", ev.UID, err)
		}
		if vs := preflight.NewChecker(func() time.Time { return now }).Check(e); vs != nil {
			return nil, fmt.Errorf("test event %s fails preflight: %v", ev.UID, vs)
		}
		out = append(out, e)
	}
	return out, nil
}

// startFake serves the fake Lattice API on a loopback port, with random tokens, and returns an SDK adapter pointed at
// it. The random tokens mean the run proves the adapter sends both the bearer token and the Sandboxes token.
func startFake() (*latticesdk.Client, func(), error) {
	token, sandbox := randomToken(), randomToken()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("start the fake: %w", err)
	}
	srv := &http.Server{Handler: &latticefake.Server{Token: token, SandboxToken: sandbox}, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }() // Serve returns ErrServerClosed when stopped, which is the expected end
	client, err := latticesdk.New(latticesdk.Config{BaseURL: "http://" + ln.Addr().String(), Token: token, SandboxToken: sandbox})
	if err != nil {
		_ = srv.Close()
		return nil, nil, err
	}
	return client, func() { _ = srv.Close() }, nil
}

// randomToken returns 32 random hex characters.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
