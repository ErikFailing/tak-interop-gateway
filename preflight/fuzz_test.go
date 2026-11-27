package preflight_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// FuzzCheckJSON feeds arbitrary bytes to CheckJSON, which reads untrusted input in `takgw preflight`. It must never
// panic. It either refuses the document with ErrInvalidJSON, or returns violations that each name a real rule and a
// field, and checking the same document again with a fresh Checker gives exactly the same answer.
//
// Run it with: go test -fuzz=FuzzCheckJSON ./preflight
func FuzzCheckJSON(f *testing.F) {
	good, err := json.Marshal(goodTrack())
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{
		string(good), "{}", "[]", "null", `{"entityId":"a","entityId":"b"}`, `{"EntityId":"a"}`,
		`{"location":{"position":{"latitudeDegrees":91,"lat":1}}}`, `{"aliases":{"alternateIds":[{"typo":1}]}}`,
		`{"milView":{"disposition":"FRIENDLY"},"ontology":{"template":"TEMPLATE_INVALID"}}`,
	} {
		f.Add([]byte(seed))
	}
	ids := map[string]bool{}
	for _, r := range preflight.Rules() {
		ids[r.ID] = true
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		first, err := preflight.NewChecker(fixedClock(checkTime)).CheckJSON(data)
		if err != nil {
			if !errors.Is(err, preflight.ErrInvalidJSON) {
				t.Fatalf("error does not wrap ErrInvalidJSON: %v", err)
			}
			return
		}
		for _, v := range first {
			if !ids[v.Rule.ID] || v.Field == "" || v.Detail == "" {
				t.Fatalf("malformed violation: %+v", v)
			}
		}
		second, err := preflight.NewChecker(fixedClock(checkTime)).CheckJSON(data)
		if err != nil {
			t.Fatalf("second check failed: %v", err)
		}
		same := slices.EqualFunc(first, second, func(a, b preflight.Violation) bool { return a.String() == b.String() })
		if !same {
			t.Fatalf("not deterministic\nfirst:  %v\nsecond: %v", first, second)
		}
	})
}
