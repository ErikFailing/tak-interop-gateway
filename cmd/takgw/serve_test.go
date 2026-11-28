package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
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

// startAcceptLoop runs an acceptLoop on a fresh loopback listener and returns its address and a function that stops
// it and waits for it.
func startAcceptLoop(t *testing.T, s *server) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		s.acceptLoop(ctx, ln)
		close(stopped)
	}()
	return ln.Addr().String(), func() {
		cancel()
		<-stopped
	}
}

// closedByServer reports whether the server closes conn within five seconds, without sending anything.
func closedByServer(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	return errors.Is(err, io.EOF)
}

// TestConnectionLimit checks that a connection beyond -max-conns is closed at once.
func TestConnectionLimit(t *testing.T) {
	frames := make(chan []byte, 8)
	s := &server{log: slog.New(slog.DiscardHandler), frames: frames, maxConns: 1, idleTimeout: time.Minute}
	addr, stop := startAcceptLoop(t, s)
	defer stop()
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Make sure the first connection holds the only slot before the second arrives.
	if _, err := io.WriteString(first, scenario(t, 1)); err != nil {
		t.Fatal(err)
	}
	<-frames
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !closedByServer(second) {
		t.Error("the connection over the limit was not closed")
	}
}

// TestIdleTimeout checks that a connection that sends nothing is closed after -idle-timeout.
func TestIdleTimeout(t *testing.T) {
	s := &server{log: slog.New(slog.DiscardHandler), frames: make(chan []byte, 8), maxConns: 4, idleTimeout: 100 * time.Millisecond}
	addr, stop := startAcceptLoop(t, s)
	defer stop()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !closedByServer(conn) {
		t.Error("the idle connection was not closed")
	}
	waitFor(t, "the timeout to count as a stream error", func() bool { return s.streamErrors() == 1 })
}
