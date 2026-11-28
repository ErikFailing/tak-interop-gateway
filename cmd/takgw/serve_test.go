package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
)

// scenario returns ticks of cot-gen's default scenario (four walkers and the last-known-position marker), starting
// now so no report is stale, as the newline-separated stream cot-gen writes.
func scenario(t *testing.T, ticks int) string {
	t.Helper()
	s, err := cotgen.New(cotgen.Config{
		Units: 4, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: time.Now().UTC(),
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for range ticks {
		for _, ev := range s.Next() {
			x, err := xml.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(x)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestServeStdin runs three ticks through serve and then feeds its output to preflight: everything serve publishes
// must pass. The walkers move every tick, so all 12 of their reports go out; the marker doesn't, so only its first
// report does, and the other two are held back as unchanged.
func TestServeStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"serve", "-source", "demo", "-stdin", "-simulated"},
		strings.NewReader(scenario(t, 3)), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, &stderr)
	}
	if want := "15 messages: 13 published, 2 dropped (unchanged 2)"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if n := strings.Count(stdout.String(), "\n"); n != 13 {
		t.Errorf("wrote %d entity lines, want 13", n)
	}

	// The output is the input format of takgw preflight, and must pass it cleanly, even with -strict.
	var pout, perr bytes.Buffer
	if code := run(context.Background(), []string{"preflight", "-strict"}, &stdout, &pout, &perr); code != 0 {
		t.Errorf("preflight on serve's output: exit %d\nstdout: %s\nstderr: %s", code, &pout, &perr)
	}
}

// TestServeUsage checks the flag errors, each of which must stop serve before it opens anything.
func TestServeUsage(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{"no source", []string{"-stdin"}, 2, "-source is required"},
		{"no input", []string{"-source", "s"}, 2, "exactly one of -stdin, -listen or -connect"},
		{"two inputs", []string{"-source", "s", "-stdin", "-listen", "127.0.0.1:0"}, 2, "exactly one of -stdin, -listen or -connect"},
		{"unknown classification", []string{"-source", "s", "-stdin", "-classification", "SECRET"}, 2, `-classification: preflight: classification level "SECRET"`},
		{"track quality out of range", []string{"-source", "s", "-stdin", "-track-quality", "16"}, 2, "track quality must be 0 to 15"},
		{"negative max entities", []string{"-source", "s", "-stdin", "-max-entities", "-1"}, 2, "max entities must not be negative"},
		{"no connections", []string{"-source", "s", "-listen", "127.0.0.1:0", "-max-conns", "0"}, 2, "-max-conns"},
		{"no idle timeout", []string{"-source", "s", "-listen", "127.0.0.1:0", "-idle-timeout", "0s"}, 2, "-idle-timeout"},
		{"connection limit without -listen", []string{"-source", "s", "-stdin", "-max-conns", "4"}, 2, "-max-conns needs -listen"},
		{"idle timeout on stdin", []string{"-source", "s", "-stdin", "-idle-timeout", "1m"}, 2, "-idle-timeout needs -listen or -connect"},
		{"stray argument", []string{"-source", "s", "-stdin", "extra"}, 2, "unexpected arguments"},
		{"unknown flag", []string{"-bogus"}, 2, "flag provided but not defined"},
		{"help", []string{"-h"}, 0, "usage: takgw serve"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := runServe(context.Background(), tt.args, strings.NewReader(""), &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", got, tt.wantCode, &stderr)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// TestServeMalformedStdin checks that a stdin stream the framer can't follow fails the run, after publishing what came
// before it.
func TestServeMalformedStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := scenario(t, 1) + `<event version="2.0"`
	code := runServe(context.Background(), []string{"-source", "demo", "-stdin"}, strings.NewReader(in), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit %d, want 1\nstderr: %s", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "5 published") || !strings.Contains(stderr.String(), "1 stream errors") {
		t.Errorf("stderr = %q, want 5 published and 1 stream error", stderr.String())
	}
}

// failWriter fails every write, like a closed pipe.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestServePublishFailureStops checks that serve stops at the first failed publish, rather than carrying on with an
// output that is gone.
func TestServePublishFailureStops(t *testing.T) {
	var stderr bytes.Buffer
	code := runServe(context.Background(), []string{"-source", "demo", "-stdin"}, strings.NewReader(scenario(t, 2)), failWriter{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "publish failed, stopping") || !strings.Contains(stderr.String(), "1 messages") {
		t.Errorf("exit %d, stderr %q; want exit 1 after the first message", code, stderr.String())
	}
}

// TestServeHealthPublishFailure checks that when the gateway's own asset can't be published at startup, serve stops
// before reading any input, and never claims to have published the asset as offline.
func TestServeHealthPublishFailure(t *testing.T) {
	var stderr bytes.Buffer
	code := runServe(context.Background(), []string{"-source", "demo", "-stdin", "-health-position", "40,-105"},
		strings.NewReader(scenario(t, 1)), failWriter{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "publish failed, stopping") || strings.Contains(stderr.String(), "as offline") {
		t.Errorf("exit %d, stderr %q; want exit 1, a publish failure and no offline publish", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "messages:") {
		t.Errorf("stderr = %q: serve read input after its first publish failed", stderr.String())
	}
}

// TestGraceContext checks that publishes outlive a stop by the grace period, and no longer.
func TestGraceContext(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	gctx, cancel := graceContext(ctx, 100*time.Millisecond)
	defer cancel()
	stop()
	if gctx.Err() != nil {
		t.Fatal("the grace context ended with the stop")
	}
	select {
	case <-gctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the grace context outlived its grace period")
	}

	// cancel ends it at once, stop or no stop.
	gctx, cancel = graceContext(context.Background(), time.Hour)
	cancel()
	if gctx.Err() == nil {
		t.Error("cancel didn't end the grace context")
	}
}

// TestSummary checks the summary line: every count, and the drops in a fixed order whatever order they happened in.
func TestSummary(t *testing.T) {
	c := counters{messages: 20, published: 9, unparsable: 1, unmappable: 2, rejected: 3, publishErrors: 1,
		dropped: map[string]int{"origin-conflict": 1, "unchanged": 2, "repeated": 1}}
	want := "20 messages: 9 published, 4 dropped (repeated 1, unchanged 2, origin-conflict 1), 1 unparsable, " +
		"2 unmappable, 3 rejected by preflight, 1 publish errors, 4 stream errors; 7 entities tracked"
	if got := c.summary(4, 7); got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
	if got := (&counters{dropped: map[string]int{}}).drops(); got != "0 dropped" {
		t.Errorf("drops = %q, want 0 dropped", got)
	}
}

// TestDropReasons checks that every state-stage sentinel has a name, so none is counted as "other".
func TestDropReasons(t *testing.T) {
	for _, err := range []error{state.ErrRepeated, state.ErrOutOfOrder, state.ErrUnchanged, state.ErrFull,
		state.ErrFutureTime, state.ErrImplausible, state.ErrOriginConflict} {
		c := counters{dropped: map[string]int{}}
		c.count(slog.New(slog.DiscardHandler), pipeline.Result{}, fmt.Errorf("%w: %w", pipeline.ErrDropped, err))
		if c.dropped[otherDrop] != 0 {
			t.Errorf("%v has no name in dropReasons", err)
		}
	}
}

// TestServeOutputError checks that an output file that can't be opened is a runtime failure.
func TestServeOutputError(t *testing.T) {
	var stderr bytes.Buffer
	code := runServe(context.Background(), []string{"-source", "demo", "-stdin", "-out", t.TempDir()}, strings.NewReader(""), io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "-out") {
		t.Errorf("exit %d, stderr %q; want exit 1 for a directory as -out", code, stderr.String())
	}
}

// syncBuffer is a bytes.Buffer safe to write from serve's goroutines while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or ten seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeListen runs serve on a TCP port, sends it a scenario over a connection, and stops it with the context,
// as Ctrl-C would.
func TestServeListen(t *testing.T) {
	// 1: Find a free port. Another process could take it before serve does, which would fail this test loudly.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	// 2: Start serve, connect once it is listening, and send two ticks.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int)
	go func() {
		done <- runServe(ctx, []string{"-source", "demo", "-listen", addr, "-simulated"}, strings.NewReader(""), &stdout, &stderr)
	}()
	var conn net.Conn
	waitFor(t, "serve to listen", func() bool {
		conn, err = net.Dial("tcp", addr)
		return err == nil
	})
	if _, err := io.WriteString(conn, scenario(t, 2)); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	// 3: Ten reports came in; nine go out (the marker's second report is unchanged). Once the connection has closed,
	// every report is queued, and a stop finishes the queue, so the summary counts all ten.
	waitFor(t, "the connection to close", func() bool { return strings.Contains(stderr.String(), "connection closed") })
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d\nstderr: %s", code, stderr.String())
	}
	if n := strings.Count(stdout.String(), "\n"); n != 9 {
		t.Errorf("wrote %d entity lines, want 9", n)
	}
	for _, want := range []string{"listening", "connection opened", "10 messages: 9 published, 1 dropped (unchanged 1)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}

// TestServeListenInUse checks that a port serve can't bind is a runtime failure.
func TestServeListenInUse(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	var stderr bytes.Buffer
	if code := runServe(context.Background(), []string{"-source", "demo", "-listen", taken.Addr().String()}, strings.NewReader(""), io.Discard, &stderr); code != 1 {
		t.Errorf("exit %d, want 1\nstderr: %s", code, &stderr)
	}
}
