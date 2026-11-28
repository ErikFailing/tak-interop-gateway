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
	"slices"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cli"
	"github.com/erikfailing/tak-interop-gateway/internal/hostile"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

func main() {
	ctx, stop := cli.SignalContext()
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// dialTimeout bounds each connection to the gateway, TLS handshake included.
const dialTimeout = 5 * time.Second

// options are the parsed flags.
type options struct {
	tcp                       string
	tls                       cli.ClientTLS
	intruderCert, intruderKey string
	delay                     time.Duration
}

// run parses the flags and sends the cases, returning the exit status: 0 when every case was sent, 1 when a
// connection failed or the run was stopped before the end, 2 for bad arguments.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// 1: Parse the flags.
	fs := flag.NewFlagSet("hostile-sender", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.tcp, "tcp", "", "send to this host:port, one connection per case, instead of stdout")
	o.tls.Register(fs, "tcp")
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
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "hostile-sender: "+format+"\n", a...)
		return 2
	}
	if fs.NArg() > 0 {
		return usage("unexpected arguments: %v", fs.Args())
	}
	if err := o.tls.Check(o.tcp != ""); err != nil {
		return usage("%v", err)
	}
	if (o.intruderCert == "") != (o.intruderKey == "") || (o.intruderCert != "" && !o.tls.Enabled()) {
		return usage("-intruder-cert and -intruder-key go together, with -tcp and the -tls flags")
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
			return usage("%v (-list shows them)", err)
		}
		cases = []hostile.Case{c}
	}

	// 3: Load the certificates, and send.
	if o.tcp == "" {
		return toStdout(cases, stdout, stderr)
	}
	client, err := o.tls.Config()
	if err != nil {
		return usage("%v", err)
	}
	var intruder *tls.Config
	if o.intruderCert != "" {
		id := cli.ClientTLS{Cert: o.intruderCert, Key: o.intruderKey, CA: o.tls.CA, ServerName: o.tls.ServerName}
		if intruder, err = id.Config(); err != nil {
			return usage("intruder: %v", err)
		}
	}
	return toTCP(ctx, cases, o, client, intruder, stderr)
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

// toTCP sends each case on its own connection, as the client identity (or over plain TCP when client is nil). The
// impersonation case sends its setup on the client's connection and, while that stays open, its attack on the
// intruder's.
func toTCP(ctx context.Context, cases []hostile.Case, o options, client, intruder *tls.Config, stderr io.Writer) int {
	send := func(conn net.Conn, msgs ...[]byte) {
		for _, m := range msgs {
			// A stream-ending case may be cut off mid-write by the gateway closing the connection, which is the point.
			if _, err := conn.Write(append(m, '\n')); err != nil {
				return
			}
		}
	}
	stopped := func() int {
		fmt.Fprintln(stderr, "hostile-sender: stopped before every case was sent")
		return 1
	}

	for i, c := range cases {
		if i > 0 && !cli.Sleep(ctx, o.delay) {
			return stopped()
		}
		if c.Impersonates && intruder == nil {
			fmt.Fprintf(stderr, "skipped %s: it needs -intruder-cert and -intruder-key, a second identity\n", c.Name)
			continue
		}
		conn, err := transport.Dial(ctx, o.tcp, client, dialTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return stopped()
			}
			fmt.Fprintf(stderr, "hostile-sender: connect: %v\n", err)
			return 1
		}
		if c.Impersonates {
			send(conn, c.Setup...)
			if !cli.Sleep(ctx, o.delay) { // let the gateway publish the victim's track first
				_ = conn.Close()
				return stopped()
			}
			attacker, err := transport.Dial(ctx, o.tcp, intruder, dialTimeout)
			if err != nil {
				_ = conn.Close()
				if ctx.Err() != nil {
					return stopped()
				}
				fmt.Fprintf(stderr, "hostile-sender: connect as the intruder: %v\n", err)
				return 1
			}
			send(attacker, c.Attack)
			_ = attacker.Close()
		} else {
			send(conn, slices.Concat(c.Setup, [][]byte{c.Attack})...)
		}
		_ = conn.Close()
		fmt.Fprintf(stderr, "sent %s: %s\n    expect: %s\n", c.Name, c.Description, c.Expect)
	}
	return 0
}
