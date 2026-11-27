// Command takgw is TAK Interop Gateway: it translates TAK Cursor-on-Target traffic into Lattice entities.
//
// Usage:
//
//	takgw serve      run the gateway
//	takgw preflight  check entity JSON lines against the documented acceptance rules
//	takgw readback   publish test entities and confirm what comes back
//
// preflight is implemented; serve and readback are not yet.
package main

import (
	"fmt"
	"io"
	"os"
)

// usage is printed for help, for a missing command and for an unknown command.
const usage = `usage: takgw <command> [flags]

commands:
  serve      run the gateway (not implemented yet)
  preflight  check entity JSON lines against the documented acceptance rules
  readback   round-trip check of test entities (not implemented yet)

Run "takgw <command> -h" for a command's flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches one subcommand and returns the process exit code. It takes its streams as parameters so tests can
// supply input and capture output.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "preflight":
		return runPreflight(args[1:], stdin, stdout, stderr)
	case "serve", "readback":
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
