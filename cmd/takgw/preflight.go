package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// maxLine is the longest JSONL line preflight reads. A real entity is a few kilobytes.
const maxLine = 1 << 20

// preflightUsage heads the flag list for `takgw preflight -h`.
const preflightUsage = `usage: takgw preflight [flags] [file ...]

Checks Lattice entities, one JSON object per line, against the documented acceptance rules. It reads the files in
order, or stdin when there are none or a file is "-". One checker sees every line, so an update whose source update
time didn't change is caught across files too.

Exit status: 0 when every entity passes, 1 when any breaks an error rule (or, with -strict, a warning rule), and 2
for a usage error or input that isn't one entity per line.

flags:
`

// tally counts what a preflight run found, for the summary and the exit status.
type tally struct {
	entities, errors, warnings, unreadable int
}

// runPreflight implements `takgw preflight` and returns the exit status.
func runPreflight(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// 1: Parse the flags.
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nowFlag := fs.String("now", "", "check against this RFC 3339 time instead of the clock, for repeatable results")
	strict := fs.Bool("strict", false, "exit 1 on warnings as well as errors")
	verbose := fs.Bool("v", false, "also print info notes, and an ok line for each entity that passes")
	listRules := fs.Bool("rules", false, "print the rules as Markdown and exit")
	fs.Usage = func() {
		fmt.Fprint(stderr, preflightUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *listRules {
		fmt.Fprint(stdout, preflight.RulesMarkdown())
		return 0
	}
	clock := time.Now
	if *nowFlag != "" {
		at, err := time.Parse(time.RFC3339Nano, *nowFlag)
		if err != nil {
			fmt.Fprintf(stderr, "takgw preflight: -now: %v\n", err)
			return 2
		}
		clock = func() time.Time { return at }
	}

	// 2: Check every input with one Checker, so the stateful rules span them all.
	inputs := fs.Args()
	if len(inputs) == 0 {
		inputs = []string{"-"}
	}
	c := preflight.NewChecker(clock)
	var t tally
	for _, name := range inputs {
		if err := checkInput(c, name, stdin, stdout, &t, *verbose); err != nil {
			fmt.Fprintf(stderr, "takgw preflight: %v\n", err)
			t.unreadable++
		}
	}

	// 3: Summarize, and choose the exit status.
	fmt.Fprintf(stderr, "takgw preflight: %d entities checked: %d errors, %d warnings, %d unreadable\n",
		t.entities, t.errors, t.warnings, t.unreadable)
	switch {
	case t.unreadable > 0:
		return 2
	case t.errors > 0 || (*strict && t.warnings > 0):
		return 1
	}
	return 0
}

// checkInput checks every line of one input. A line that isn't an entity document is reported and counted, and the
// rest of the input is still checked; an input that can't be read at all is returned as an error.
func checkInput(c *preflight.Checker, name string, stdin io.Reader, stdout io.Writer, t *tally, verbose bool) error {
	// 1: Open the input. "-" is stdin, which is shown as "stdin".
	r, label := stdin, "stdin"
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close() // read-only, so a close error can't lose data
		r, label = f, name
	}

	// 2: Check each non-blank line.
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for line := 1; sc.Scan(); line++ {
		data := bytes.TrimSpace(sc.Bytes())
		if len(data) == 0 {
			continue
		}
		vs, err := c.CheckJSON(data)
		if err != nil {
			fmt.Fprintf(stdout, "%s:%d: %v\n", label, line, err)
			t.unreadable++
			continue
		}
		t.entities++
		shown := 0
		for _, v := range vs {
			switch v.Rule.Severity {
			case preflight.Error:
				t.errors++
			case preflight.Warning:
				t.warnings++
			case preflight.Info:
				if !verbose {
					continue
				}
			}
			fmt.Fprintf(stdout, "%s:%d: %s\n", label, line, v)
			shown++
		}
		if verbose && shown == 0 {
			fmt.Fprintf(stdout, "%s:%d: ok\n", label, line)
		}
	}

	// 3: A scanner error ends this input: a line over maxLine, or a read failure.
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}
