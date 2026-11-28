package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/hostile"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// hostileStream is every hostile case one stream can carry, as hostile-sender writes it to stdout.
func hostileStream() []byte {
	var b []byte
	for _, c := range hostile.Cases(time.Now().UTC()) {
		if c.StreamFatal() || c.Impersonates {
			continue
		}
		for _, m := range c.Setup {
			b = append(append(b, m...), '\n')
		}
		b = append(append(b, c.Attack...), '\n')
	}
	return b
}

// TestServeHealth runs serve with its own asset on a short interval, feeds it hostile input, and requires the asset
// to turn to a warning while the input is processed, and to say offline after the stop.
func TestServeHealth(t *testing.T) {
	stdin, feed := io.Pipe()
	var stdout, stderr syncBuffer
	done := make(chan int)
	go func() {
		done <- runServe(context.Background(), []string{"-source", "demo", "-stdin",
			"-health-position", "40.3428,-105.6836,2900", "-health-interval", "50ms"}, stdin, &stdout, &stderr)
	}()
	waitFor(t, "the first, healthy asset", func() bool { return strings.Contains(stdout.String(), `"healthStatus":"HEALTH_STATUS_HEALTHY"`) })
	if _, err := feed.Write(hostileStream()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a warning", func() bool { return strings.Contains(stdout.String(), `"healthStatus":"HEALTH_STATUS_WARN"`) })
	_ = feed.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr.String())
	}
	out, log := stdout.String(), stderr.String()
	for _, want := range []string{`"healthStatus":"HEALTH_STATUS_OFFLINE"`, `"connectionStatus":"CONNECTION_STATUS_OFFLINE"`, `"template":"TEMPLATE_ASSET"`, "1 implausible jump"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s", want)
		}
	}
	for _, want := range []string{"publishing the gateway's own asset", "dropped a suspect report", "implausible 1", "published the gateway's own asset as offline"} {
		if !strings.Contains(log, want) {
			t.Errorf("stderr = %q, want it to contain %q", log, want)
		}
	}
}

// TestServeOriginConflict runs serve with a mutual-TLS listener: a client publishes a track, and a second identity
// from the same authority then claims it.
func TestServeOriginConflict(t *testing.T) {
	f := devCerts(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int)
	go func() {
		done <- runServe(ctx, []string{"-source", "demo", "-listen", addr,
			"-tls-cert", f.ServerCert, "-tls-key", f.ServerKey, "-tls-ca", f.CA}, strings.NewReader(""), &stdout, &stderr)
	}()

	// 1: The owner's report goes out.
	report := func(at time.Time, lat cot.Number) []byte {
		b, err := xml.Marshal(cot.Event{
			Version: cot.Version, UID: "victim-1", Type: "a-f-G-U-C", How: "m-g",
			Time: cot.Timestamp(at), Start: cot.Timestamp(at), Stale: cot.Timestamp(at.Add(2 * time.Minute)),
			Point: cot.Point{Lat: lat, Lon: -105.6836, HAE: cot.Unknown, CE: 10, LE: cot.Unknown},
		})
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	send := func(cert, key string, msg []byte) {
		cfg, err := transport.ClientTLS(cert, key, f.CA, "")
		if err != nil {
			t.Fatal(err)
		}
		var conn net.Conn
		waitFor(t, "serve to listen", func() bool {
			conn, err = tls.Dial("tcp", addr, cfg)
			return err == nil
		})
		if _, err := conn.Write(msg); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	now := time.Now().UTC()
	send(f.ClientCert, f.ClientKey, report(now, 40.3428))
	waitFor(t, "the owner's track", func() bool { return strings.Count(stdout.String(), "\n") == 1 })

	// 2: The intruder's newer report for the same uid is dropped.
	send(f.IntruderCert, f.IntruderKey, report(now.Add(time.Second), 40.3429))
	waitFor(t, "the conflict", func() bool { return strings.Contains(stderr.String(), "origin-conflict") })
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	if n := strings.Count(stdout.String(), "\n"); n != 1 {
		t.Errorf("published %d entities, want only the owner's", n)
	}
	for _, want := range []string{`bound to \"cert:takgw-dev-client\"`, "1 dropped (origin-conflict 1)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}

// TestServeHealthUsage checks the health flags.
func TestServeHealthUsage(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"-health-name", "x"}, "need -health-position"},
		{[]string{"-health-interval", "10s"}, "need -health-position"},
		{[]string{"-health-position", "north"}, "-health-position"},
		{[]string{"-health-position", "91,0"}, "not a WGS84"},
		{[]string{"-health-position", "40,-105,NaN"}, "not a finite number"},
		{[]string{"-health-position", "40,-105", "-health-interval", "1m"}, "at most 30s"},
	} {
		var stderr bytes.Buffer
		args := append([]string{"-source", "demo", "-stdin"}, tt.args...)
		if code := runServe(context.Background(), args, strings.NewReader(""), io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("%v: exit %d, stderr %q; want 2 and %q", tt.args, code, stderr.String(), tt.want)
		}
	}
}
