package transport_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// TestPeerIdentity checks the identity the state stage binds tracks to: the common name when there is one, otherwise
// a fingerprint that tells two such certificates apart, and nothing without a certificate.
func TestPeerIdentity(t *testing.T) {
	state := func(cn, der string) tls.ConnectionState {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte(der), Subject: pkix.Name{CommonName: cn}}}}
	}
	if got := transport.PeerIdentity(state("unit-7", "der-1")); got != "cert:unit-7" {
		t.Errorf("with a common name: %q, want cert:unit-7", got)
	}
	a, b := transport.PeerIdentity(state("", "der-1")), transport.PeerIdentity(state("", "der-2"))
	if !strings.HasPrefix(a, "cert-sha256:") || len(a) != len("cert-sha256:")+64 {
		t.Errorf("without a common name: %q, want cert-sha256: and 64 hex digits", a)
	}
	if a == b {
		t.Errorf("two certificates without a common name share the identity %q", a)
	}
	if again := transport.PeerIdentity(state("", "der-1")); again != a {
		t.Errorf("the same certificate gave %q, then %q", a, again)
	}
	if got := transport.PeerIdentity(tls.ConnectionState{}); got != "" {
		t.Errorf("without a certificate: %q, want empty", got)
	}
}

// TestDial checks Dial over plain TCP, over mutual TLS with the handshake done when it returns, and that a server that
// never answers the handshake fails within the timeout.
func TestDial(t *testing.T) {
	ctx := context.Background()
	t.Run("plain TCP", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		conn, err := transport.Dial(ctx, ln.Addr().String(), nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, isTLS := conn.(*tls.Conn); isTLS {
			t.Error("a nil config gave a TLS connection")
		}
	})
	t.Run("mutual TLS", func(t *testing.T) {
		f := certs(t)
		server, err := transport.ServerTLS(f.ServerCert, f.ServerKey, f.CA)
		if err != nil {
			t.Fatal(err)
		}
		client, err := transport.ClientTLS(f.ClientCert, f.ClientKey, f.CA, "")
		if err != nil {
			t.Fatal(err)
		}
		ln, err := tls.Listen("tcp", "127.0.0.1:0", server)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			if c, err := ln.Accept(); err == nil {
				_ = c.(*tls.Conn).Handshake()
				defer c.Close()
				_, _ = c.Read(make([]byte, 1))
			}
		}()
		conn, err := transport.Dial(ctx, ln.Addr().String(), client, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		tc, ok := conn.(*tls.Conn)
		if !ok || !tc.ConnectionState().HandshakeComplete {
			t.Error("Dial returned before the TLS handshake was done")
		}
	})
	t.Run("a server that never answers the handshake", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0") // accepts, and says nothing
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		start := time.Now()
		_, err = transport.Dial(ctx, ln.Addr().String(), &tls.Config{ServerName: "x", MinVersion: tls.VersionTLS12}, 200*time.Millisecond)
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the deadline", err)
		}
		if waited := time.Since(start); waited > 3*time.Second {
			t.Errorf("Dial took %v with a 200ms timeout", waited)
		}
	})
}

// TestFramerDefaultLimit checks that a limit of zero means the default, not no limit at all.
func TestFramerDefaultLimit(t *testing.T) {
	big := "<event>" + strings.Repeat("A", transport.DefaultMaxFrame) + "</event>"
	if _, err := transport.NewFramer(strings.NewReader(big), 0).Next(); !errors.Is(err, transport.ErrFrameTooLarge) {
		t.Errorf("a frame past the default limit: err %v, want ErrFrameTooLarge", err)
	}
	small := "<event>x</event>"
	if msg, err := transport.NewFramer(strings.NewReader(small), 0).Next(); err != nil || string(msg) != small {
		t.Errorf("a small frame: %q, %v", msg, err)
	}
}
