// Package cli holds what the project's commands share: a context that the first Ctrl-C or SIGTERM cancels, after which
// a second one ends the process at once, a sleep that a stop cuts short, and the TLS flags of a command that dials a
// server.
package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// SignalContext returns a context cancelled by the first interrupt (Ctrl-C) or SIGTERM, the signal service managers
// and container runtimes send to stop a process. After that first signal the default handling comes back, so a second
// one ends the process even if a clean stop is stuck. Call stop when the command ends.
func SignalContext() (ctx context.Context, stop context.CancelFunc) {
	ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// Sleep waits for d, or until ctx is done, and reports whether the whole wait passed. A command that paces itself uses
// it in place of time.Sleep, so a stop doesn't wait out the pause.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ClientTLS is the set of flags for dialing a server over mutual TLS: this side's certificate and key, the authority
// the server's certificate must chain to, and optionally the name it must carry.
type ClientTLS struct {
	Cert, Key, CA, ServerName string
}

// Register adds -tls-cert, -tls-key, -tls-ca and -tls-server-name to fs. addrFlag names the flag that gives the
// address they apply to, for the help text.
func (c *ClientTLS) Register(fs *flag.FlagSet, addrFlag string) {
	fs.StringVar(&c.Cert, "tls-cert", "", "with -"+addrFlag+", connect with mutual TLS presenting this certificate, PEM")
	fs.StringVar(&c.Key, "tls-key", "", "the private key for -tls-cert, PEM")
	fs.StringVar(&c.CA, "tls-ca", "", "the authority, PEM, the server's certificate must be signed by")
	fs.StringVar(&c.ServerName, "tls-server-name", "", "the name the server's certificate must carry, if not the host dialed")
}

// Enabled reports whether TLS was asked for.
func (c ClientTLS) Enabled() bool {
	return c.Cert != "" || c.Key != "" || c.CA != "" || c.ServerName != ""
}

// Check enforces the flags' rules: the certificate, key and authority come together; a server name needs them; and
// any of them needs an address to dial (hasAddr). So asking for TLS can never fall back to plain TCP.
func (c ClientTLS) Check(hasAddr bool) error {
	n := 0
	for _, f := range []string{c.Cert, c.Key, c.CA} {
		if f != "" {
			n++
		}
	}
	switch {
	case n != 0 && n != 3:
		return errors.New("-tls-cert, -tls-key and -tls-ca go together: give all three")
	case c.ServerName != "" && n != 3:
		return errors.New("-tls-server-name needs -tls-cert, -tls-key and -tls-ca")
	case c.Enabled() && !hasAddr:
		return errors.New("the -tls flags need an address to connect to")
	}
	return nil
}

// Config builds the TLS config, or returns nil when TLS wasn't asked for. Call Check first.
func (c ClientTLS) Config() (*tls.Config, error) {
	if !c.Enabled() {
		return nil, nil
	}
	return transport.ClientTLS(c.Cert, c.Key, c.CA, c.ServerName)
}
