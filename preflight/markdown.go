package preflight

import (
	"fmt"
	"strings"
)

// severityHeading names each severity's section in the rules page.
var severityHeading = map[Severity]string{Error: "Errors", Warning: "Warnings", Info: "Notes"}

// severityIntro is the paragraph under each severity's heading in the rules page.
var severityIntro = map[Severity]string{
	Error:   "The documentation says the server rejects or discards the entity, or that the field is required, or the value is outside what the field can mean. `takgw preflight` exits 1 when any entity breaks one.",
	Warning: "The documentation's production-quality checklist, or a mistake it warns is accepted and then silently lost. `takgw preflight -strict` exits 1 on these too.",
	Info:    "Notes about coverage: requirements this checker does not model, and so cannot vouch for.",
}

// RulesMarkdown renders every rule as the Markdown page docs/rules.md. The page is generated so it can't drift from
// the code: a test fails when the committed page differs, and `go test ./preflight -update` rewrites it.
func RulesMarkdown() string {
	var b strings.Builder
	b.WriteString("# Entity Preflight rules\n\n")
	b.WriteString("<!-- Generated from preflight/rules.go by `go test ./preflight -update`. Do not edit by hand. -->\n\n")
	fmt.Fprintf(&b, "Entity Preflight checks a candidate Lattice entity against these rules before it is published. They "+
		"encode Anduril's public integration guide and the field comments in the Go SDK, and were checked against "+
		"lattice-sdk-go %s. Every link is pinned to the commit or tag the rule was read from. The enum rule asks the "+
		"installed SDK which values exist, and the known-fields rule asks it which fields do, so neither keeps a copy "+
		"of Anduril's lists.\n\n", SDKVersion)
	b.WriteString("The documentation can lag the server, so passing preflight is necessary, not sufficient. " +
		"The planned `takgw readback` publishes to a real environment and reads the entity back, which is the final word.\n")
	for _, sev := range []Severity{Error, Warning, Info} {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n\n", severityHeading[sev], severityIntro[sev])
		b.WriteString("| Rule | Requires | Sources |\n|---|---|---|\n")
		for _, r := range Rules() {
			if r.Severity != sev {
				continue
			}
			links := make([]string, len(r.Sources))
			for i, s := range r.Sources {
				links[i] = fmt.Sprintf("[%s](%s)", s.Title, s.URL)
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.ID, cell(r.Summary), strings.Join(links, "<br>"))
		}
	}
	return b.String()
}

// cell escapes text for a Markdown table cell.
func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
