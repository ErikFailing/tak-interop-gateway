package preflight

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/erikfailing/tak-interop-gateway/entity"
)

// Severity says how strongly the documentation backs a rule.
type Severity int

const (
	// Error: the documentation says the server rejects or discards the entity, that the field is required, or the
	// value is outside what the field can mean (a latitude of 91, say).
	Error Severity = iota
	// Warning: the documentation's production-quality checklist, or a mistake the documentation warns is accepted and
	// then silently lost. The entity may be accepted, but it is incomplete or wrong for operational use.
	Warning
	// Info: a note about coverage, such as requirements this checker does not model and so cannot check.
	Info
)

// String returns "error", "warning" or "info".
func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// Source is a public document a rule encodes.
type Source struct {
	Title string // what the link points at, such as "Integration guide: Required for a live entity"
	URL   string // pinned to a commit or tag, so it shows the text the rule was written from
}

// Rule is one documented acceptance rule.
type Rule struct {
	ID       string   // stable identifier, such as "expiry-in-future"
	Severity Severity // how strongly the documentation backs it
	Summary  string   // what the rule requires, in one sentence
	Sources  []Source // the documents it encodes
}

// Violation is one broken rule on one entity.
type Violation struct {
	Rule   Rule
	Field  string // the JSON path of the offending field, such as "provenance.dataType"
	Detail string // what was wrong, specifically
}

// String formats the violation as "severity rule-id field: detail".
func (v Violation) String() string {
	return fmt.Sprintf("%s %s %s: %s", v.Rule.Severity, v.Rule.ID, v.Field, v.Detail)
}

// Checker checks entities against the rules. It remembers each entity's last provenance.sourceUpdateTime, so checking
// a stream of entities in publish order also catches an update the server would discard because its time didn't
// change. A Checker is not safe for concurrent use.
type Checker struct {
	now  func() time.Time
	last map[string]time.Time // entityId to the sourceUpdateTime of its previous update
}

// NewChecker returns a Checker that reads the current time from now, which tests and CI can fix for repeatable
// results; nil means time.Now.
func NewChecker(now func() time.Time) *Checker {
	if now == nil {
		now = time.Now
	}
	return &Checker{now: now, last: map[string]time.Time{}}
}

// Check checks one entity on its own, at now, with no history: the stateful source-update-time rules can't fire. It
// is for a caller that keeps its own history, or checks one entity at a time.
func Check(e entity.Entity, now time.Time) []Violation {
	return NewChecker(func() time.Time { return now }).Check(e)
}

// Check returns every rule the entity breaks, in rule order, or nil if it breaks none. The rules that need the raw
// document, known-fields and field-name-case, only CheckJSON applies.
func (c *Checker) Check(e entity.Entity) []Violation {
	// 1: Apply every rule against one reading of the clock, so the expiry rules agree with each other.
	now := c.now()
	var out []Violation
	for _, r := range rules {
		if r.check == nil {
			continue
		}
		for _, f := range r.check(c, e, now) {
			out = append(out, Violation{Rule: r.clone(), Field: f.field, Detail: f.detail})
		}
	}

	// 2: Remember this update's time, after the rules ran so the stateful ones compared it with the previous one, and
	// only for an entity the server would accept: one it rejects never happened, so a corrected copy with the same
	// time is not a repeat.
	if !HasErrors(out) && e.EntityID != nil && *e.EntityID != "" && e.Provenance != nil && e.Provenance.SourceUpdateTime != nil {
		c.last[*e.EntityID] = *e.Provenance.SourceUpdateTime
	}
	return out
}

// CheckJSON checks one entity given as a JSON object in the REST API's shape, the form `takgw preflight` reads. It
// applies every rule, including known-fields and field-name-case. It returns an error wrapping ErrInvalidJSON, and no
// violations, when the document can't be read unambiguously: malformed JSON, anything but a single object, a duplicate
// key, a value of the wrong type for any field the SDK declares, or a date-time that isn't an RFC 3339 string, the only
// form the API's protobuf Timestamps take, though the Go SDK reads others.
func (c *Checker) CheckJSON(data []byte) ([]Violation, error) {
	// 1: Walk the raw document against the SDK's types. This rejects what can't be read unambiguously and notes keys the
	// SDK doesn't declare, which encoding/json would otherwise drop without a word.
	unknown, miscased, err := walkDocument(data)
	if err != nil {
		return nil, err
	}

	// 2: Decode into the SDK's own type, which knows every field's type, so a wrong type on a field the model doesn't
	// carry is caught too; then into the model, for the rules. The walk already proved the document is one well-formed
	// object, so a failure here is a type mismatch, such as a string where a number belongs.
	var sdk lattice.Entity
	if err := json.Unmarshal(data, &sdk); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	var e entity.Entity
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	var out []Violation
	for _, f := range unknown {
		out = append(out, Violation{Rule: ruleKnownFields.clone(), Field: f.field, Detail: f.detail})
	}
	for _, f := range miscased {
		out = append(out, Violation{Rule: ruleFieldNameCase.clone(), Field: f.field, Detail: f.detail})
	}
	return append(out, c.Check(e)...), nil
}

// Rules returns every rule, in the order Check and CheckJSON report them. The slice and every rule in it are copies,
// so a caller can't change what the package reports.
func Rules() []Rule {
	out := make([]Rule, len(rules))
	for i, r := range rules {
		out[i] = r.clone()
	}
	return out
}

// clone copies a rule with its own Sources, which would otherwise share the package's backing array.
func (r Rule) clone() Rule {
	r.Sources = slices.Clone(r.Sources)
	return r
}

// HasErrors reports whether any violation has Error severity.
func HasErrors(vs []Violation) bool {
	for _, v := range vs {
		if v.Rule.Severity == Error {
			return true
		}
	}
	return false
}
