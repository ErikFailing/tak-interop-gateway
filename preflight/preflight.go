package preflight

import (
	"encoding/json"
	"fmt"
	"time"

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
// results.
func NewChecker(now func() time.Time) *Checker {
	return &Checker{now: now, last: map[string]time.Time{}}
}

// Check returns every rule the entity breaks, in rule order, or nil if it breaks none. The known-fields rule needs the
// raw document, so only CheckJSON applies it.
func (c *Checker) Check(e entity.Entity) []Violation {
	// 1: Apply every rule against one reading of the clock, so the expiry rules agree with each other.
	now := c.now()
	var out []Violation
	for _, r := range rules {
		if r.check == nil {
			continue
		}
		for _, f := range r.check(c, e, now) {
			out = append(out, Violation{Rule: r.Rule, Field: f.field, Detail: f.detail})
		}
	}

	// 2: Remember this update's time only after the rules ran, so the stateful rules compared it with the previous one.
	if e.EntityID != nil && e.Provenance != nil && e.Provenance.SourceUpdateTime != nil {
		c.last[*e.EntityID] = *e.Provenance.SourceUpdateTime
	}
	return out
}

// CheckJSON checks one entity given as a JSON object in the REST API's shape, the form `takgw preflight` reads. It
// applies every rule, including known-fields. It returns an error wrapping ErrInvalidJSON, and no violations, when the
// document can't be read unambiguously: malformed JSON, anything but a single object, a duplicate key, or a value of
// the wrong type for a field the model knows.
func (c *Checker) CheckJSON(data []byte) ([]Violation, error) {
	// 1: Walk the raw document against the SDK's types. This rejects what can't be read unambiguously and notes keys the
	// SDK doesn't declare, which encoding/json would otherwise drop without a word.
	unknown, err := walkDocument(data)
	if err != nil {
		return nil, err
	}

	// 2: Decode into the model and apply the entity rules. The walk already proved the document is one well-formed
	// object, so a failure here is a type mismatch, such as a string where a number belongs.
	var e entity.Entity
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	var out []Violation
	for _, f := range unknown {
		out = append(out, Violation{Rule: ruleKnownFields, Field: f.field, Detail: f.detail})
	}
	return append(out, c.Check(e)...), nil
}

// Rules returns every rule, in the order Check and CheckJSON report them.
func Rules() []Rule {
	out := make([]Rule, len(rules))
	for i, r := range rules {
		out[i] = r.Rule
	}
	return out
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
