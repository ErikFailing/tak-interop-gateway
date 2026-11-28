package devcert_test

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/devcert"
)

// load parses the certificate in a PEM file.
func load(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("%s holds no certificate", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestGenerate checks that the certificates chain to the authority, for the right names and uses, and that the keys
// are private to their owner.
func TestGenerate(t *testing.T) {
	now := time.Now()
	f, err := devcert.Generate(t.TempDir(), []string{"localhost", "127.0.0.1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	ca := load(t, f.CA)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	server, client := load(t, f.ServerCert), load(t, f.ClientCert)
	for _, name := range []string{"localhost", "127.0.0.1"} {
		if _, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("server certificate for %s: %v", name, err)
		}
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: "example.com"}); err == nil {
		t.Error("the server certificate verified for a host it wasn't issued for")
	}
	if _, err := client.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate: %v", err)
	}
	if !server.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) || server.DNSNames[0] != "localhost" {
		t.Errorf("server names = %v %v", server.DNSNames, server.IPAddresses)
	}
	if got := server.NotAfter.Sub(now); got > devcert.Validity || got < devcert.Validity-time.Minute {
		t.Errorf("valid for %v after now, want %v", got, devcert.Validity)
	}
	if runtime.GOOS != "windows" {
		for _, key := range []string{f.ServerKey, f.ClientKey} {
			if info, err := os.Stat(key); err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v, err %v; want 0600", key, info.Mode().Perm(), err)
			}
		}
	}
}

// TestNoOverwrite checks that Generate refuses to replace any existing file, and writes nothing when it refuses.
// TestFailedWriteCleansUp checks that a write failing partway removes the files already written, so the retry, which
// refuses to overwrite anything, can succeed.
func TestFailedWriteCleansUp(t *testing.T) {
	dir := t.TempDir()
	restore := devcert.FailWritesAfter(3)
	_, err := devcert.Generate(dir, []string{"localhost"}, time.Now())
	restore()
	if err == nil {
		t.Fatal("Generate succeeded despite a failed write")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the failed run left %v behind", names)
	}
	if _, err := devcert.Generate(dir, []string{"localhost"}, time.Now()); err != nil {
		t.Errorf("the retry failed: %v", err)
	}
}

func TestNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "client.key"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := devcert.Generate(dir, []string{"localhost"}, time.Now()); err == nil {
		t.Fatal("Generate overwrote an existing file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "client.key")); string(b) != "keep me" {
		t.Error("the existing file changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err == nil {
		t.Error("Generate wrote some files before refusing")
	}
	if _, err := devcert.Generate(t.TempDir(), nil, time.Now()); err == nil {
		t.Error("Generate accepted no hosts")
	}
}
