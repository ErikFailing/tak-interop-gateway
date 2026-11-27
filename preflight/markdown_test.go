package preflight_test

import (
	"flag"
	"os"
	"testing"

	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// update rewrites docs/rules.md from the rules instead of comparing against it: go test ./preflight -update
var update = flag.Bool("update", false, "rewrite docs/rules.md from the rules")

// rulesPage is the committed rules page, relative to this package.
const rulesPage = "../docs/rules.md"

// TestRulesPage keeps docs/rules.md equal to what the rules render, so the published list can't drift from the code.
func TestRulesPage(t *testing.T) {
	got := preflight.RulesMarkdown()
	if *update {
		if err := os.WriteFile(rulesPage, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(rulesPage)
	if err != nil {
		t.Fatalf("%v (run go test ./preflight -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s is out of date; run go test ./preflight -update and commit the result", rulesPage)
	}
}
