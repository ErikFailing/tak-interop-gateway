package cli_test

import (
	"context"
	"flag"
	"io"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cli"
)

// TestSleep checks that Sleep waits out its duration, and that a stop cuts it short.
func TestSleep(t *testing.T) {
	if !cli.Sleep(context.Background(), time.Millisecond) {
		t.Error("an uninterrupted sleep reported a stop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if cli.Sleep(ctx, time.Hour) {
		t.Error("a cancelled sleep reported finishing")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("a cancelled sleep took %v", waited)
	}
}

// TestClientTLSCheck checks the rules for the TLS flags: all three files together, a server name only with them, and
// always an address to dial, so asking for TLS can never fall back to plain TCP.
func TestClientTLSCheck(t *testing.T) {
	all := cli.ClientTLS{Cert: "c.pem", Key: "c.key", CA: "ca.pem"}
	tests := []struct {
		name    string
		flags   cli.ClientTLS
		hasAddr bool
		ok      bool
	}{
		{"no TLS, no address", cli.ClientTLS{}, false, true},
		{"no TLS, an address", cli.ClientTLS{}, true, true},
		{"all three", all, true, true},
		{"all three and a server name", cli.ClientTLS{Cert: "c.pem", Key: "c.key", CA: "ca.pem", ServerName: "gw"}, true, true},
		{"all three, no address", all, false, false},
		{"only a certificate", cli.ClientTLS{Cert: "c.pem"}, true, false},
		{"no authority", cli.ClientTLS{Cert: "c.pem", Key: "c.key"}, true, false},
		{"only a server name", cli.ClientTLS{ServerName: "gw"}, true, false},
	}
	for _, tt := range tests {
		if err := tt.flags.Check(tt.hasAddr); (err == nil) != tt.ok {
			t.Errorf("%s: Check = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

// TestClientTLSFlags checks that Register wires the flags, and that Config is nil, not an error, without them.
func TestClientTLSFlags(t *testing.T) {
	var c cli.ClientTLS
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.Register(fs, "tcp")
	if err := fs.Parse([]string{"-tls-cert", "c.pem", "-tls-key", "c.key", "-tls-ca", "ca.pem", "-tls-server-name", "gw"}); err != nil {
		t.Fatal(err)
	}
	if c != (cli.ClientTLS{Cert: "c.pem", Key: "c.key", CA: "ca.pem", ServerName: "gw"}) || !c.Enabled() {
		t.Errorf("parsed %+v", c)
	}
	if _, err := c.Config(); err == nil {
		t.Error("Config with missing files succeeded")
	}
	if cfg, err := (cli.ClientTLS{}).Config(); cfg != nil || err != nil {
		t.Errorf("Config without TLS = %v, %v; want nil, nil", cfg, err)
	}
}
