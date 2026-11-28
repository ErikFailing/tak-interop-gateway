package transport_test

import (
	"crypto/tls"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/devcert"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// certs writes a fresh development authority and certificates for 127.0.0.1 and localhost.
func certs(t *testing.T) devcert.Files {
	t.Helper()
	f, err := devcert.Generate(t.TempDir(), []string{"localhost", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// handshake runs one TLS handshake between a server and a client config over loopback and returns the client name
// the server saw, and each side's error.
func handshake(t *testing.T, server, client *tls.Config) (clientName string, serverErr, clientErr error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer conn.Close()
		tc := conn.(*tls.Conn)
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		if serverErr = tc.Handshake(); serverErr == nil {
			clientName = transport.PeerName(tc.ConnectionState())
			_, _ = tc.Write([]byte("ok")) // so the client's read below completes
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), client)
	if err == nil {
		// In TLS 1.3 the client can finish before the server has checked its certificate; a read surfaces the verdict.
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = io.ReadFull(conn, make([]byte, 2))
		_ = conn.Close()
	}
	<-done
	return clientName, serverErr, err
}

// TestMutualTLS checks who gets in.
func TestMutualTLS(t *testing.T) {
	ours, theirs := certs(t), certs(t) // two unrelated authorities
	server, err := transport.ServerTLS(ours.ServerCert, ours.ServerKey, ours.CA)
	if err != nil {
		t.Fatal(err)
	}
	client := func(f devcert.Files, ca, name string) *tls.Config {
		c, err := transport.ClientTLS(f.ClientCert, f.ClientKey, ca, name)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("our client", func(t *testing.T) {
		name, serverErr, clientErr := handshake(t, server, client(ours, ours.CA, "127.0.0.1"))
		if serverErr != nil || clientErr != nil || name != "takgw-dev-client" {
			t.Errorf("server %v, client %v, name %q", serverErr, clientErr, name)
		}
	})
	t.Run("server name override", func(t *testing.T) {
		if _, serverErr, clientErr := handshake(t, server, client(ours, ours.CA, "localhost")); serverErr != nil || clientErr != nil {
			t.Errorf("server %v, client %v", serverErr, clientErr)
		}
	})
	t.Run("a name the certificate doesn't carry", func(t *testing.T) {
		if _, _, clientErr := handshake(t, server, client(ours, ours.CA, "tak.example.com")); clientErr == nil {
			t.Error("the client accepted a server certificate for another name")
		}
	})
	t.Run("a client with no certificate", func(t *testing.T) {
		bare := &tls.Config{RootCAs: client(ours, ours.CA, "").RootCAs, ServerName: "127.0.0.1"}
		if _, serverErr, _ := handshake(t, server, bare); serverErr == nil {
			t.Error("the server accepted a client without a certificate")
		}
	})
	t.Run("a client certificate from another authority", func(t *testing.T) {
		if _, serverErr, _ := handshake(t, server, client(theirs, ours.CA, "127.0.0.1")); serverErr == nil {
			t.Error("the server accepted a client certificate it doesn't trust")
		}
	})
	t.Run("a server the client doesn't trust", func(t *testing.T) {
		if _, _, clientErr := handshake(t, server, client(ours, theirs.CA, "127.0.0.1")); clientErr == nil {
			t.Error("the client accepted a server certificate it doesn't trust")
		}
	})
	t.Run("TLS 1.1", func(t *testing.T) {
		old := client(ours, ours.CA, "127.0.0.1")
		old.MinVersion, old.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
		if _, serverErr, _ := handshake(t, server, old); serverErr == nil {
			t.Error("the server accepted TLS 1.1")
		}
	})
}

// TestTLSConfigErrors checks that each bad file is reported, not discovered at the first handshake.
func TestTLSConfigErrors(t *testing.T) {
	f := certs(t)
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name             string
		cert, key, caPEM string
	}{
		{"missing certificate", filepath.Join(t.TempDir(), "nope.pem"), f.ServerKey, f.CA},
		{"key doesn't match", f.ServerCert, f.ClientKey, f.CA},
		{"missing CA", f.ServerCert, f.ServerKey, filepath.Join(t.TempDir(), "nope.pem")},
		{"CA file with no certificates", f.ServerCert, f.ServerKey, empty},
	}
	for _, tt := range tests {
		if _, err := transport.ServerTLS(tt.cert, tt.key, tt.caPEM); !errors.Is(err, transport.ErrTLSConfig) {
			t.Errorf("ServerTLS, %s: err %v", tt.name, err)
		}
		if _, err := transport.ClientTLS(tt.cert, tt.key, tt.caPEM, ""); !errors.Is(err, transport.ErrTLSConfig) {
			t.Errorf("ClientTLS, %s: err %v", tt.name, err)
		}
	}
	if transport.PeerName(tls.ConnectionState{}) != "" {
		t.Error("PeerName without a certificate should be empty")
	}
}
