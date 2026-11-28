package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/devcert"
)

// devcertsUsage heads the flag list for `takgw devcerts -h`.
const devcertsUsage = `usage: takgw devcerts [-dir DIR] [-hosts NAMES]

Writes a throwaway certificate authority, a server certificate and a client certificate, for trying mutual TLS
locally between takgw serve and cot-gen. Not for production: the keys are unencrypted, the certificates last 30 days,
and nothing else trusts the authority. A real deployment uses certificates from its TAK server's authority. Existing
files are never overwritten.

flags:
`

// runDevcerts implements `takgw devcerts` and returns the exit status.
func runDevcerts(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devcerts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "devcerts", "directory to write into; created if needed")
	hosts := fs.String("hosts", "localhost,127.0.0.1,::1", "comma-separated names and addresses the server certificate is valid for")
	fs.Usage = func() {
		fmt.Fprint(stderr, devcertsUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "takgw devcerts: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	var names []string
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			names = append(names, h)
		}
	}
	f, err := devcert.Generate(*dir, names, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "takgw devcerts: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, `wrote a development CA and certificates, valid for 30 days, to %s:
  authority  %s
  server     %s  %s  (for %s)
  client     %s  %s

try them:
  takgw serve -source demo -simulated -listen 127.0.0.1:8089 -tls-cert %s -tls-key %s -tls-ca %s
  cot-gen -tcp 127.0.0.1:8089 -realtime -ticks 0 -tls-cert %s -tls-key %s -tls-ca %s
`, *dir, f.CA, f.ServerCert, f.ServerKey, strings.Join(names, ", "), f.ClientCert, f.ClientKey,
		f.ServerCert, f.ServerKey, f.CA, f.ClientCert, f.ClientKey, f.CA)
	return 0
}
