package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// ErrTLSConfig wraps every problem building a TLS config: a missing file, a key that doesn't match its certificate,
// or a CA file with no certificates in it.
var ErrTLSConfig = errors.New("transport: invalid TLS config")

// minTLS is the oldest protocol version either side accepts. TAK Server and current TAK clients speak TLS 1.2 and 1.3.
const minTLS = tls.VersionTLS12

// ServerTLS builds the config for a listener that accepts only clients presenting a certificate signed by the
// authority in clientCAFile: mutual TLS, the way TAK servers authenticate their clients. The files are PEM; TAK's
// PKCS #12 bundles convert with openssl.
func ServerTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := loadKeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := loadPool(clientCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   minTLS,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}, nil
}

// ClientTLS builds the config for connecting to a TAK server: this side's certificate, and the authority the
// server's certificate must chain to. Only that authority is trusted, not the system's roots, because TAK servers run
// their own. serverName is the name the server's certificate must carry; empty means the host being dialed. There is
// deliberately no way to skip verification.
func ClientTLS(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	cert, err := loadKeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := loadPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   minTLS,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
	}, nil
}

// PeerName returns the common name of the certificate the other side presented, for logs, or "" if there was none.
func PeerName(state tls.ConnectionState) string {
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return state.PeerCertificates[0].Subject.CommonName
}

// loadKeyPair loads a certificate and its private key.
func loadKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%w: certificate %s with key %s: %w", ErrTLSConfig, certFile, keyFile, err)
	}
	return cert, nil
}

// loadPool loads the certificates in a PEM file as a pool of trusted authorities.
func loadPool(caFile string) (*x509.CertPool, error) {
	b, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTLSConfig, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%w: %s holds no PEM certificates", ErrTLSConfig, caFile)
	}
	return pool, nil
}
