// Package devcert writes a throwaway certificate authority, a server certificate and two client certificates, for trying
// and testing the gateway's mutual TLS locally. It is not for production: the keys are written unencrypted, the
// certificates last 30 days, and nothing else trusts the authority. A real deployment uses certificates issued by its
// TAK server's own authority.
package devcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Validity is how long the generated certificates last.
const Validity = 30 * 24 * time.Hour

// Files are the paths Generate wrote.
type Files struct {
	CA, ServerCert, ServerKey, ClientCert, ClientKey string
	// IntruderCert and IntruderKey are a second client identity from the same authority, for showing that one
	// client can't take over another's tracks.
	IntruderCert, IntruderKey string
}

// Generate writes ca.pem, server.pem, server.key, client.pem, client.key, intruder.pem and intruder.key into dir, which
// it creates if needed. The
// server certificate is valid for hosts, each a DNS name or an IP address. Generate never overwrites a file: if any
// already exists, it writes nothing and returns an error.
func Generate(dir string, hosts []string, now time.Time) (Files, error) {
	// 1: Check every target before writing any, so a refusal leaves the directory as it was.
	f := Files{
		CA: filepath.Join(dir, "ca.pem"), ServerCert: filepath.Join(dir, "server.pem"), ServerKey: filepath.Join(dir, "server.key"),
		ClientCert: filepath.Join(dir, "client.pem"), ClientKey: filepath.Join(dir, "client.key"),
		IntruderCert: filepath.Join(dir, "intruder.pem"), IntruderKey: filepath.Join(dir, "intruder.key"),
	}
	if len(hosts) == 0 {
		return Files{}, errors.New("devcert: the server certificate needs at least one host")
	}
	for _, p := range []string{f.CA, f.ServerCert, f.ServerKey, f.ClientCert, f.ClientKey, f.IntruderCert, f.IntruderKey} {
		if _, err := os.Stat(p); err == nil {
			return Files{}, fmt.Errorf("devcert: %s already exists; not overwriting", p)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Files{}, err
	}

	// 2: The authority, then a server and a client certificate it signs.
	notBefore, notAfter := now.Add(-time.Hour), now.Add(Validity) // an hour of slack for a skewed clock
	caKey, caDER, err := issue(&x509.Certificate{
		Subject:               pkix.Name{CommonName: "takgw development CA (not for production)"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}, nil, nil)
	if err != nil {
		return Files{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Files{}, err
	}
	server := &x509.Certificate{
		Subject:     pkix.Name{CommonName: hosts[0]},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			server.IPAddresses = append(server.IPAddresses, ip)
		} else {
			server.DNSNames = append(server.DNSNames, h)
		}
	}
	serverKey, serverDER, err := issue(server, caCert, caKey)
	if err != nil {
		return Files{}, err
	}
	client := func(name string) (*ecdsa.PrivateKey, []byte, error) {
		return issue(&x509.Certificate{
			Subject:     pkix.Name{CommonName: name},
			NotBefore:   notBefore,
			NotAfter:    notAfter,
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}, caCert, caKey)
	}
	clientKey, clientDER, err := client("takgw-dev-client")
	if err != nil {
		return Files{}, err
	}
	intruderKey, intruderDER, err := client("takgw-dev-intruder")
	if err != nil {
		return Files{}, err
	}

	// 3: Write them. Keys are readable by the owner only. O_EXCL keeps a file that appeared since step 1 safe.
	for _, w := range []struct {
		path, kind string
		value      any // a DER certificate, or a private key
		mode       os.FileMode
	}{
		{f.CA, "CERTIFICATE", caDER, 0o644},
		{f.ServerCert, "CERTIFICATE", serverDER, 0o644},
		{f.ServerKey, "PRIVATE KEY", serverKey, 0o600},
		{f.ClientCert, "CERTIFICATE", clientDER, 0o644},
		{f.ClientKey, "PRIVATE KEY", clientKey, 0o600},
		{f.IntruderCert, "CERTIFICATE", intruderDER, 0o644},
		{f.IntruderKey, "PRIVATE KEY", intruderKey, 0o600},
	} {
		if err := writePEM(w.path, w.kind, w.value, w.mode); err != nil {
			return Files{}, err
		}
	}
	return f, nil
}

// issue makes a P-256 key and a certificate for it from template, signed by parent and parentKey, or self-signed when
// parent is nil. It returns the key and the certificate in DER.
func issue(template, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	template.SerialNumber = serial
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, nil, fmt.Errorf("devcert: sign %q: %w", template.Subject.CommonName, err)
	}
	return key, der, nil
}

// writePEM writes one PEM block to a new file. A private key is passed as *ecdsa.PrivateKey and encoded as PKCS #8.
func writePEM(path, kind string, v any, mode os.FileMode) error {
	var der []byte
	switch v := v.(type) {
	case []byte:
		der = v
	case *ecdsa.PrivateKey:
		b, err := x509.MarshalPKCS8PrivateKey(v)
		if err != nil {
			return err
		}
		der = b
	default:
		return fmt.Errorf("devcert: cannot encode a %T", v)
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("devcert: %w", err)
	}
	if err := pem.Encode(out, &pem.Block{Type: kind, Bytes: der}); err != nil {
		_ = out.Close()
		return fmt.Errorf("devcert: write %s: %w", path, err)
	}
	return out.Close()
}
