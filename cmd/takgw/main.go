// Command takgw is TAK Interop Gateway: it translates TAK Cursor-on-Target traffic into Lattice entities.
//
// Usage:
//
//	takgw serve      run the gateway: CoT in, entities out as JSON lines or to Lattice
//	takgw preflight  check entity JSON lines against the documented acceptance rules
//	takgw readback   publish test entities and confirm what comes back
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

// usage is printed for help, for a missing command and for an unknown command.
const usage = `usage: takgw <command> [flags]

commands:
  serve      run the gateway: CoT in, entities out as JSON lines or to Lattice
  preflight  check entity JSON lines against the documented acceptance rules
  readback   publish test entities, read them back, and report what changed

Run "takgw <command> -h" for a command's flags.
`

func main() {
	// Cancel the context on Ctrl-C, so serve stops cleanly and prints its summary.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches one subcommand and returns the process exit code. It takes its context and streams as parameters so
// tests can stop it, supply input and capture output.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(ctx, args[1:], stdin, stdout, stderr)
	case "preflight":
		return runPreflight(args[1:], stdin, stdout, stderr)
	case "readback":
		return runReadback(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "takgw: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
