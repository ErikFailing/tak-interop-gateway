// Command hostile-sender fires a catalogue of hostile and broken Cursor-on-Target input at a running gateway, to show
// how it defends itself. Each case says what the gateway should do with it; watch the gateway's log, and its own asset
// if it publishes one, to see it happen. Every case comes from internal/hostile, whose tests hold the gateway to the
// same expectations.
//
// Examples:
//
//	hostile-sender -list                                     # what each case is
//	hostile-sender | takgw serve -stdin -source demo -v      # the cases one stream can carry, through stdin
//	hostile-sender -tcp 127.0.0.1:8089 -tls-cert client.pem -tls-key client.key -tls-ca ca.pem \
//	  -intruder-cert intruder.pem -intruder-key intruder.key  # every case, one connection each, over mutual TLS
//
// Only point it at a gateway you run: it exists to be refused.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/hostile"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// options are the parsed flags.
type options struct {
	tcp, cert, key, ca, serverName, intruderCert, intruderKey string
	delay                                                     time.Duration
}

// run parses the flags and sends the cases, returning the exit status: 0 when every case was sent, 1 when a
// connection failed, 2 for bad arguments.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// 1: Parse the flags.
	fs := flag.NewFlagSet("hostile-sender", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.tcp, "tcp", "", "send to this host:port, one connection per case, instead of stdout")
	fs.StringVar(&o.cert, "tls-cert", "", "with -tcp, connect with mutual TLS presenting this certificate, PEM")
	fs.StringVar(&o.key, "tls-key", "", "the private key for -tls-cert, PEM")
	fs.StringVar(&o.ca, "tls-ca", "", "the authority, PEM, the gateway's certificate must be signed by")
	fs.StringVar(&o.serverName, "tls-server-name", "", "the name the gateway's certificate must carry, if not the host dialed")
	fs.StringVar(&o.intruderCert, "intruder-cert", "", "a second client certificate, for the impersonation case's attack")
	fs.StringVar(&o.intruderKey, "intruder-key", "", "the private key for -intruder-cert")
	fs.DurationVar(&o.delay, "delay", 300*time.Millisecond, "with -tcp, the pause between cases, and between a case's setup and its attack")
	list := fs.Bool("list", false, "list the cases and exit")
	only := fs.String("case", "", "send only this case")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	tlsSet := (o.cert != "") || (o.key != "") || (o.ca != "")
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "hostile-sender: unexpected arguments: %v\n", fs.Args())
		return 2
	case tlsSet && (o.cert == "" || o.key == "" || o.ca == ""):
		fmt.Fprintln(stderr, "hostile-sender: -tls-cert, -tls-key and -tls-ca go together")
		return 2
	case (tlsSet || o.intruderCert != "" || o.intruderKey != "") && o.tcp == "":
		fmt.Fprintln(stderr, "hostile-sender: the TLS flags need -tcp")
		return 2
	case (o.intruderCert == "") != (o.intruderKey == "") || (o.intruderCert != "" && !tlsSet):
		fmt.Fprintln(stderr, "hostile-sender: -intruder-cert and -intruder-key go together, with the -tls flags")
		return 2
	}

	// 2: Pick the cases.
	cases := hostile.Cases(time.Now().UTC())
	if *list {
		for _, c := range cases {
			fmt.Fprintf(stdout, "%-24s %s\n%24s expect: %s\n", c.Name, c.Description, "", c.Expect)
		}
		return 0
	}
	if *only != "" {
		c, err := hostile.Find(cases, *only)
		if err != nil {
			fmt.Fprintf(stderr, "hostile-sender: %v (-list shows them)\n", err)
			return 2
		}
		cases = []hostile.Case{c}
	}

	// 3: Send them.
	if o.tcp == "" {
		return toStdout(cases, stdout, stderr)
	}
	return toTCP(ctx, cases, o, stderr)
}

// toStdout writes every case one stream can carry, as one stream: the stream-ending cases would cut off the rest,
// and the impersonation case needs a second identity, which one stream doesn't have.
func toStdout(cases []hostile.Case, stdout, stderr io.Writer) int {
	for _, c := range cases {
		if c.StreamFatal() || c.Impersonates {
			fmt.Fprintf(stderr, "skipped %s: it needs -tcp\n", c.Name)
			continue
		}
		for _, msg := range c.Setup {
			_, _ = stdout.Write(append(msg, '\n'))
		}
		_, _ = stdout.Write(append(c.Attack, '\n'))
		fmt.Fprintf(stderr, "sent %s: %s\n    expect: %s\n", c.Name, c.Description, c.Expect)
	}
	return 0
}

// toTCP sends each case on its own connection. The impersonation case sends its setup on one identity's connection
// and, while that stays open, its attack on the intruder's.
func toTCP(ctx context.Context, cases []hostile.Case, o options, stderr io.Writer) int {
	// 1: How to connect as each identity.
	dial := func(cert, key string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 5 * time.Second}
		if cert == "" {
			return d.DialContext(ctx, "tcp", o.tcp)
		}
		cfg, err := transport.ClientTLS(cert, key, o.ca, o.serverName)
		if err != nil {
			return nil, err
		}
		return (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", o.tcp)
	}
	send := func(conn net.Conn, msgs ...[]byte) {
		for _, m := range msgs {
			// A stream-ending case may be cut off mid-write by the gateway closing the connection, which is the point.
			if _, err := conn.Write(append(m, '\n')); err != nil {
				return
			}
		}
	}

	// 2: One case at a time.
	for i, c := range cases {
		if i > 0 {
			select {
			case <-time.After(o.delay):
			case <-ctx.Done():
				return 0
			}
		}
		if c.Impersonates && o.intruderCert == "" {
			fmt.Fprintf(stderr, "skipped %s: it needs -intruder-cert and -intruder-key, a second identity\n", c.Name)
			continue
		}
		conn, err := dial(o.cert, o.key)
		if err != nil {
			fmt.Fprintf(stderr, "hostile-sender: connect: %v\n", err)
			return 1
		}
		if c.Impersonates {
			send(conn, c.Setup...)
			time.Sleep(o.delay) // let the gateway publish the victim's track first
			intruder, err := dial(o.intruderCert, o.intruderKey)
			if err != nil {
				_ = conn.Close()
				fmt.Fprintf(stderr, "hostile-sender: connect as the intruder: %v\n", err)
				return 1
			}
			send(intruder, c.Attack)
			_ = intruder.Close()
		} else {
			send(conn, slices.Concat(c.Setup, [][]byte{c.Attack})...)
		}
		_ = conn.Close()
		fmt.Fprintf(stderr, "sent %s: %s\n    expect: %s\n", c.Name, c.Description, c.Expect)
	}
	return 0
}
