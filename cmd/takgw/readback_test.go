package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/latticefake"
	"github.com/erikfailing/tak-interop-gateway/internal/publish/latticesdk"
)

// useLive points newLiveClient at handler for the rest of the test, as if it were a live environment reached with
// the given credentials.
func useLive(t *testing.T, handler http.Handler, token, sandbox string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	prev := newLiveClient
	newLiveClient = func() (*latticesdk.Client, error) {
		return latticesdk.New(latticesdk.Config{BaseURL: srv.URL, Token: token, SandboxToken: sandbox})
	}
	t.Cleanup(func() { newLiveClient = prev })
}

// TestReadbackOffline runs readback against its built-in fake: every test entity comes back as sent.
func TestReadbackOffline(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"readback"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, &stdout, &stderr)
	}
	for _, want := range []string{"offline", "6 entities: 6 passed, 0 failed", "SIM-READBACK-VESSEL"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"readback", "-v"}, nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "added createdTime") {
		t.Errorf("-v: exit %d, stdout %q; want each added createdTime listed", code, stdout.String())
	}
}

// TestReadbackLive runs readback in live mode against fakes standing in for an environment.
func TestReadbackLive(t *testing.T) {
	tests := []struct {
		name       string
		fake       *latticefake.Server
		wantCode   int
		wantStdout []string
	}{
		{"everything comes back", &latticefake.Server{Token: "t", SandboxToken: "s"}, 0,
			[]string{"live, publishing 6 simulated entities", "6 passed, 0 failed"}},
		{"the platform drops a field", &latticefake.Server{Token: "t", SandboxToken: "s", Mutate: func(e map[string]any) { delete(e, "symbology") }}, 1,
			[]string{"FAIL", `dropped symbology (sent {"milStd2525C":`, "0 passed, 6 failed"}},
		{"the platform refuses the credentials", &latticefake.Server{Token: "other", SandboxToken: "s"}, 1,
			[]string{"server refused the credentials", "0 passed, 6 failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useLive(t, tt.fake, "t", "s")
			var stdout, stderr bytes.Buffer
			code := runReadback(context.Background(), []string{"-live", "-wait", "0s"}, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, &stdout, &stderr)
			}
			for _, want := range tt.wantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
				}
			}
		})
	}
}

// TestReadbackUsage checks the flag and config errors.
func TestReadbackUsage(t *testing.T) {
	prev := newLiveClient
	newLiveClient = func() (*latticesdk.Client, error) {
		return nil, fmt.Errorf("%w: LATTICE_ENDPOINT is not set", latticesdk.ErrConfig)
	}
	t.Cleanup(func() { newLiveClient = prev })
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"-live"}, "LATTICE_ENDPOINT is not set"},
		{[]string{"-expiry", "1s"}, "-expiry"},
		{[]string{"-expiry", "720h"}, "-expiry"},
		{[]string{"-wait", "-1s"}, "-wait"},
		{[]string{"extra"}, "unexpected arguments"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runReadback(context.Background(), tt.args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("%v: exit %d, stderr %q; want 2 and %q", tt.args, code, stderr.String(), tt.want)
		}
	}
}

// TestTestEntities checks the readback set: distinct IDs, all simulated, all expiring when asked.
func TestTestEntities(t *testing.T) {
	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	es, err := testEntities("takgw-readback", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range es {
		switch {
		case seen[*e.EntityID]:
			t.Errorf("duplicate ID %s", *e.EntityID)
		case e.Indicators == nil || e.Indicators.Simulated == nil || !*e.Indicators.Simulated:
			t.Errorf("%s is not marked simulated", *e.EntityID)
		case !e.ExpiryTime.Equal(now.Add(time.Minute)):
			t.Errorf("%s expires at %v, want %v", *e.EntityID, e.ExpiryTime, now.Add(time.Minute))
		}
		seen[*e.EntityID] = true
	}
	if len(es) != 6 {
		t.Errorf("%d test entities, want 6", len(es))
	}
}

// TestServeLattice runs serve with -lattice against fakes standing in for an environment.
func TestServeLattice(t *testing.T) {
	t.Run("publishes", func(t *testing.T) {
		fake := &latticefake.Server{Token: "t", SandboxToken: "s"}
		useLive(t, fake, "t", "s")
		var stderr bytes.Buffer
		code := runServe(context.Background(), []string{"-source", "demo", "-stdin", "-lattice", "-simulated"}, strings.NewReader(scenario(t, 2)), nil, &stderr)
		if code != 0 || !strings.Contains(stderr.String(), "10 messages: 9 published") {
			t.Errorf("exit %d, stderr %q", code, stderr.String())
		}
		if fake.Requests() != 9 {
			t.Errorf("the fake received %d publishes, want 9", fake.Requests())
		}
	})
	t.Run("refused credentials stop the gateway", func(t *testing.T) {
		useLive(t, &latticefake.Server{Token: "other"}, "t", "")
		var stderr bytes.Buffer
		code := runServe(context.Background(), []string{"-source", "demo", "-stdin", "-lattice"}, strings.NewReader(scenario(t, 2)), nil, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "publish failed, stopping") || !strings.Contains(stderr.String(), "1 messages") {
			t.Errorf("exit %d, stderr %q; want a stop after the first message", code, stderr.String())
		}
	})
	t.Run("a rejected entity doesn't stop the gateway", func(t *testing.T) {
		rejectAll := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message":"provenance.dataType must be set"}`)
		})
		useLive(t, rejectAll, "t", "")
		var stderr bytes.Buffer
		code := runServe(context.Background(), []string{"-source", "demo", "-stdin", "-lattice"}, strings.NewReader(scenario(t, 1)), nil, &stderr)
		if code != 0 || !strings.Contains(stderr.String(), "5 publish errors") || !strings.Contains(stderr.String(), "rejected the request as invalid") {
			t.Errorf("exit %d, stderr %q; want five logged rejections and a clean exit", code, stderr.String())
		}
	})
	t.Run("-lattice and -out together", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := runServe(context.Background(), []string{"-source", "demo", "-stdin", "-lattice", "-out", "x.jsonl"}, strings.NewReader(""), nil, &stderr); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
	})
	t.Run("no environment configured", func(t *testing.T) {
		prev := newLiveClient
		newLiveClient = func() (*latticesdk.Client, error) { return nil, errors.New("LATTICE_ENDPOINT is not set") }
		t.Cleanup(func() { newLiveClient = prev })
		var stderr bytes.Buffer
		if code := runServe(context.Background(), []string{"-source", "demo", "-stdin", "-lattice"}, strings.NewReader(""), nil, &stderr); code != 2 || !strings.Contains(stderr.String(), "LATTICE_ENDPOINT") {
			t.Errorf("exit %d, stderr %q", code, stderr.String())
		}
	})
}
