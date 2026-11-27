// Command takgw is TAK Interop Gateway: it translates TAK Cursor-on-Target traffic into Lattice entities.
//
// Usage:
//
//	takgw serve      run the gateway
//	takgw preflight  validate entity JSON read from stdin
//	takgw readback   publish test entities and confirm what comes back
//
// None of the subcommands is implemented yet.
package main

import (
	"fmt"
	"io"
	"os"
)

// usage is printed for help, for a missing command and for an unknown command.
const usage = `usage: takgw <command>

commands:
  serve      run the gateway (not implemented yet)
  preflight  validate entity JSON from stdin (not implemented yet)
  readback   round-trip check of test entities (not implemented yet)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one subcommand and returns the process exit code. It takes its output streams as parameters so tests
// can capture them.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve", "preflight", "readback":
		fmt.Fprintf(stderr, "takgw %s: not implemented yet\n", args[0])
		return 1
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "takgw: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
