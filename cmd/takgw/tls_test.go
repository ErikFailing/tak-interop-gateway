package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/devcert"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// devCerts writes a fresh development authority and certificates for loopback.
func devCerts(t *testing.T) devcert.Files {
	t.Helper()
	f, err := devcert.Generate(t.TempDir(), []string{"localhost", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// freePort returns a loopback address nothing is listening on, for now.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// fastBackoff shortens the reconnect backoff for the rest of the test.
func fastBackoff(t *testing.T) {
	prevMin, prevMax := minBackoff, maxBackoff
	minBackoff, maxBackoff = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { minBackoff, maxBackoff = prevMin, prevMax })
}

// TestServeMutualTLS runs serve with a mutual-TLS listener: a client with a certificate from the right authority gets
// its reports published, and a client without one doesn't get past the handshake.
func TestServeMutualTLS(t *testing.T) {
	f := devCerts(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int)
	go func() {
		done <- runServe(ctx, []string{"-source", "demo", "-simulated", "-listen", addr,
			"-tls-cert", f.ServerCert, "-tls-key", f.ServerKey, "-tls-ca", f.CA}, strings.NewReader(""), &stdout, &stderr)
	}()

	// 1: A trusted client sends two ticks.
	cfg, err := transport.ClientTLS(f.ClientCert, f.ClientKey, f.CA, "")
	if err != nil {
		t.Fatal(err)
	}
	var conn net.Conn
	waitFor(t, "serve to listen", func() bool {
		conn, err = tls.Dial("tcp", addr, cfg)
		return err == nil
	})
	if _, err := io.WriteString(conn, scenario(t, 2)); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	waitFor(t, "the connection to close", func() bool { return strings.Contains(stderr.String(), "connection closed") })

	// 2: A client without a certificate, and one speaking plain TCP.
	bare, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: cfg.RootCAs})
	if err == nil {
		_, _ = bare.Read(make([]byte, 1)) // the server's refusal arrives on the first read in TLS 1.3
		_ = bare.Close()
	}
	plain, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(plain, scenario(t, 1))
	_ = plain.Close()
	waitFor(t, "two refused handshakes", func() bool { return strings.Count(stderr.String(), "TLS handshake failed") == 2 })

	// 3: Stop, and check that only the trusted client's reports went out.
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d\nstderr: %s", code, stderr.String())
	}
	for _, want := range []string{"listening with mutual TLS", "client=takgw-dev-client", "10 messages: 9 published", "2 stream errors"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "no authentication") {
		t.Error("serve warned about missing authentication on a mutual-TLS listener")
	}
}

// takServer stands in for a TAK server's streaming port: it requires a client certificate from ca, and sends every
// connection the same stream, then closes it, as a server restart would.
func takServer(t *testing.T, server devcert.Files, ca, stream string) string {
	t.Helper()
	cfg, err := transport.ServerTLS(server.ServerCert, server.ServerKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = io.WriteString(conn, stream)
			_ = conn.Close()
		}
	}()
	return ln.Addr().String()
}

// TestServeConnect connects serve to a TAK server stand-in that closes every connection after one tick. serve must
// reconnect, and the second connection's reports, repeats of the first, must be dropped.
func TestServeConnect(t *testing.T) {
	fastBackoff(t)
	f := devCerts(t)
	addr := takServer(t, f, f.CA, scenario(t, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int)
	go func() {
		done <- runServe(ctx, []string{"-source", "demo", "-simulated", "-connect", addr,
			"-tls-cert", f.ClientCert, "-tls-key", f.ClientKey, "-tls-ca", f.CA}, strings.NewReader(""), &stdout, &stderr)
	}()
	waitFor(t, "three connections", func() bool { return strings.Count(stderr.String(), "msg=connected") >= 3 })
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d\nstderr: %s", code, stderr.String())
	}
	if n := strings.Count(stdout.String(), "\n"); n != 5 {
		t.Errorf("published %d entities, want the first connection's 5", n)
	}
	for _, want := range []string{"server=localhost", "reconnecting", "dropped (repeated"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}

// TestServeConnectUntrusted points serve at a server whose certificate comes from another authority: it must refuse
// the connection, keep retrying, and stop cleanly.
func TestServeConnectUntrusted(t *testing.T) {
	fastBackoff(t)
	ours, theirs := devCerts(t), devCerts(t)
	addr := takServer(t, theirs, ours.CA, scenario(t, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int)
	go func() {
		done <- runServe(ctx, []string{"-source", "demo", "-connect", addr,
			"-tls-cert", ours.ClientCert, "-tls-key", ours.ClientKey, "-tls-ca", ours.CA}, strings.NewReader(""), &stdout, &stderr)
	}()
	waitFor(t, "two refused attempts", func() bool { return strings.Count(stderr.String(), "reconnecting") >= 2 })
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "certificate") || strings.Contains(stderr.String(), "msg=connected") || stdout.String() != "" {
		t.Errorf("want certificate errors and nothing published\nstderr: %s\nstdout: %s", stderr.String(), stdout.String())
	}
}

// TestServeTLSUsage checks the TLS flag rules.
func TestServeTLSUsage(t *testing.T) {
	f := devCerts(t)
	all := []string{"-tls-cert", f.ServerCert, "-tls-key", f.ServerKey, "-tls-ca", f.CA}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"only a certificate", []string{"-listen", "127.0.0.1:0", "-tls-cert", f.ServerCert}, "go together"},
		{"TLS on stdin", append([]string{"-stdin"}, all...), "need -listen or -connect"},
		{"connect without TLS", []string{"-connect", "127.0.0.1:1"}, "-connect needs"},
		{"server name without connect", append([]string{"-listen", "127.0.0.1:0", "-tls-server-name", "x"}, all...), "-tls-server-name needs -connect"},
		{"two inputs", append([]string{"-listen", "127.0.0.1:0", "-connect", "127.0.0.1:1"}, all...), "exactly one of"},
		{"key that doesn't match", []string{"-listen", "127.0.0.1:0", "-tls-cert", f.ServerCert, "-tls-key", f.ClientKey, "-tls-ca", f.CA}, "invalid TLS config"},
	} {
		var stderr bytes.Buffer
		args := append([]string{"-source", "demo"}, tt.args...)
		if code := runServe(context.Background(), args, strings.NewReader(""), io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("%s: exit %d, stderr %q; want 2 and %q", tt.name, code, stderr.String(), tt.want)
		}
	}
}

// TestDevcerts checks the command: it writes the files and says how to use them, and refuses to overwrite.
func TestDevcerts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"devcerts", "-dir", dir}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, &stderr)
	}
	for _, want := range []string{"ca.pem", "client.key", "takgw serve -source demo", "cot-gen -tcp"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
	if code := runDevcerts([]string{"-dir", dir}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "not overwriting") {
		t.Errorf("second run: exit %d, stderr %q", code, stderr.String())
	}
	if code := runDevcerts([]string{"extra"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("stray argument: exit %d, want 2", code)
	}
}
