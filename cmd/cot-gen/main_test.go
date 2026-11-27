package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
)

// fixedStart makes every run deterministic.
const fixedStart = "-start=2026-11-27T16:00:00Z"

// lines splits output into non-empty lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-ticks=2", "-units=3", fixedStart}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}
	got := lines(stdout.String())
	if want := 2 * (3 + 1); len(got) != want { // two ticks of three members plus the marker
		t.Fatalf("got %d lines, want %d", len(got), want)
	}
	for _, l := range got {
		var ev cot.Event
		if err := xml.Unmarshal([]byte(l), &ev); err != nil {
			t.Errorf("line does not parse as a CoT event: %v\n%s", err, l)
		}
	}
}

func TestDeterministicRuns(t *testing.T) {
	var a, b bytes.Buffer
	args := []string{"-ticks=3", fixedStart}
	run(context.Background(), args, &a, io.Discard)
	run(context.Background(), args, &b, io.Discard)
	if a.String() != b.String() {
		t.Error("two runs with the same flags produced different output")
	}
}

func TestBadArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"-bogus"}},
		{"bad origin", []string{"-origin=north"}},
		{"origin out of range", []string{"-origin=91,0"}},
		{"bad start", []string{"-start=yesterday"}},
		{"negative ticks", []string{"-ticks=-1"}},
		{"forever without realtime", []string{"-ticks=0"}},
		{"zero units", []string{"-units=0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(context.Background(), tt.args, io.Discard, &stderr); code != 2 {
				t.Errorf("exit code %d, want 2 (stderr: %s)", code, stderr.String())
			}
		})
	}
}

func TestHelp(t *testing.T) {
	if code := run(context.Background(), []string{"-h"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
}

// TestTCP sends a run to a local listener, standing in for a TAK server, and checks what arrives.
func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			received <- ""
			return
		}
		defer conn.Close()
		b, _ := io.ReadAll(conn) // run closes the connection when it finishes
		received <- string(b)
	}()

	args := []string{"-ticks=1", "-units=2", fixedStart, "-tcp=" + ln.Addr().String()}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Error("with -tcp, nothing should go to stdout")
	}
	if got := lines(<-received); len(got) != 3 {
		t.Errorf("listener got %d events, want 3", len(got))
	}
}

func TestTCPConnectFailure(t *testing.T) {
	// Grab a free port, then close it, so nothing is listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if code := run(context.Background(), []string{"-ticks=1", "-tcp=" + addr}, io.Discard, io.Discard); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
}
