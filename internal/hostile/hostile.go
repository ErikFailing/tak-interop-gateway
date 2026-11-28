// Package hostile is a catalogue of hostile and broken CoT input, each paired with the exact outcome the gateway must
// give it. A test runs every case through the real framer and pipeline, so the catalogue is a regression suite for the
// gateway's defenses, and cmd/hostile-sender fires the same cases at a running gateway, for a demonstration.
//
// Every case uses its own uids, so the cases can share one stream and one gateway without affecting each other.
package hostile

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// Case is one hostile input and what the gateway must do with it.
type Case struct {
	Name        string // short and unique, such as "teleport"
	Description string // what the attack is, in a sentence
	Expect      string // what the gateway should do, for a person watching its log
	// Setup are messages to send first, from the setup origin, that must all be published: the state the attack
	// needs, such as the track it will try to impersonate.
	Setup [][]byte
	// Attack is the hostile input itself: one message, or a stream fragment for the stream-level cases.
	Attack []byte
	// Impersonates is true when the attack must come from a different origin than the setup, such as another
	// client certificate.
	Impersonates bool
	// Stage and Cause are the error the gateway must refuse the attack with: a pipeline stage sentinel and that
	// stage's own reason. For a stream-level attack, Stage is nil and Cause is the framer's sentinel, and the attack
	// ends the stream. When both are nil the attack is accepted, and Check says what must hold about the result.
	Stage, Cause error
	Check        func(entity.Entity) error
}

// StreamFatal reports whether the case ends the stream it is sent on, as the framer's errors do.
func (c Case) StreamFatal() bool {
	return c.Stage == nil && c.Cause != nil
}

// event builds an ordinary friendly ground unit's report at at, stale two minutes later, from the given uid.
func event(uid string, at time.Time) cot.Event {
	return cot.Event{
		Version: cot.Version, UID: uid, Type: "a-f-G-U-C", How: "m-g",
		Time: cot.Timestamp(at), Start: cot.Timestamp(at), Stale: cot.Timestamp(at.Add(2 * time.Minute)),
		Point:  cot.Point{Lat: 40.3428, Lon: -105.6836, HAE: cot.Unknown, CE: 10, LE: cot.Unknown},
		Detail: &cot.Detail{Contact: &cot.Contact{Callsign: "HOSTILE-TEST"}},
	}
}

// marshal encodes an event. Every event here is valid for encoding/xml, so an error is a bug in this package.
func marshal(ev cot.Event) []byte {
	b, err := xml.Marshal(ev)
	if err != nil {
		panic(fmt.Sprintf("hostile: marshal: %v", err))
	}
	return b
}

// edit applies one textual change to an event's XML, for attacks encoding/xml won't write. It panics if old isn't
// there, which would be a bug in this package.
func edit(b []byte, old, replacement string) []byte {
	s := string(b)
	if !strings.Contains(s, old) {
		panic(fmt.Sprintf("hostile: %q not found in %s", old, s))
	}
	return []byte(strings.Replace(s, old, replacement, 1))
}

// Cases returns the catalogue, with every report stamped at now.
func Cases(now time.Time) []Case {
	uid := func(name string) string { return "hostile-" + name }
	base := func(name string) []byte { return marshal(event(uid(name), now)) }
	cases := []Case{
		{
			Name: "replay", Description: "A captured report is sent again, unchanged.",
			Expect: "dropped by state: the report repeats the last published update time",
			Setup:  [][]byte{base("replay")}, Attack: base("replay"),
			Stage: pipeline.ErrDropped, Cause: state.ErrRepeated,
		},
		{
			Name: "rewind", Description: "An older report arrives after a newer one, to drag the track back.",
			Expect: "dropped by state: older than the last published update",
			Setup:  [][]byte{marshal(event(uid("rewind"), now.Add(5*time.Second)))}, Attack: base("rewind"),
			Stage: pipeline.ErrDropped, Cause: state.ErrOutOfOrder,
		},
		{
			Name: "teleport", Description: "A ground unit's next report, a second later, puts it 55 km away.",
			Expect: "dropped by state as implausible, logged as a warning",
			Setup:  [][]byte{base("teleport")},
			Attack: func() []byte {
				ev := event(uid("teleport"), now.Add(time.Second))
				ev.Point.Lat += 0.5
				return marshal(ev)
			}(),
			Stage: pipeline.ErrDropped, Cause: state.ErrImplausible,
		},
		{
			Name: "impersonation", Description: "A second sender claims a live unit's uid, to take over its track.",
			Expect: "dropped by state as an origin conflict, logged as a warning (needs a second identity to send from)",
			Setup:  [][]byte{base("impersonation")},
			Attack: func() []byte {
				ev := event(uid("impersonation"), now.Add(time.Second))
				ev.Point.Lat += 0.001
				return marshal(ev)
			}(),
			Impersonates: true, Stage: pipeline.ErrDropped, Cause: state.ErrOriginConflict,
		},
		{
			Name: "far-future-stale", Description: "A report claims to stay valid until 2099.",
			Expect: "published, with its expiry clamped to just under 30 days",
			Attack: func() []byte {
				ev := event(uid("far-future-stale"), now)
				ev.Stale = cot.Timestamp(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
				return marshal(ev)
			}(),
			Check: func(e entity.Entity) error {
				// The gateway clamps against its own clock, which runs a little behind the report's stamp, so check
				// the server's bound: under 30 days out.
				if limit := now.Add(30 * 24 * time.Hour); e.ExpiryTime == nil || !e.ExpiryTime.Before(limit) {
					return fmt.Errorf("expiry %v is not before %v", e.ExpiryTime, limit)
				}
				return nil
			},
		},
		{
			Name: "nan-coordinates", Description: "A report's latitude is NaN, to poison distance and range checks.",
			Expect: "refused by the parser: invalid field value",
			Attack: edit(base("nan-coordinates"), `lat="40.3428"`, `lat="NaN"`),
			Stage:  pipeline.ErrParse, Cause: cot.ErrInvalidField,
		},
		{
			Name: "latitude-out-of-range", Description: "A report's latitude is 91 degrees.",
			Expect: "refused by the parser: invalid field value",
			Attack: edit(base("latitude-out-of-range"), `lat="40.3428"`, `lat="91"`),
			Stage:  pipeline.ErrParse, Cause: cot.ErrInvalidField,
		},
		{
			Name: "duplicate-uid-attribute", Description: "A report carries two uid attributes, hoping two parsers read different ones.",
			Expect: "refused by the parser: duplicate attribute",
			Attack: edit(base("duplicate-uid-attribute"), ` type=`, ` uid="someone-else" type=`),
			Stage:  pipeline.ErrParse, Cause: cot.ErrDuplicateAttr,
		},
		{
			Name: "duplicate-point", Description: "A report carries two positions, hoping two parsers read different ones.",
			Expect: "refused by the parser: duplicate element",
			Attack: edit(base("duplicate-point"), `<detail>`, `<point lat="0" lon="0" hae="0" ce="0" le="0"></point><detail>`),
			Stage:  pipeline.ErrParse, Cause: cot.ErrDuplicateElement,
		},
		{
			Name: "deep-nesting", Description: "A report nests 100 elements deep, to exhaust a recursive parser.",
			Expect: "refused by the parser: elements nested too deeply",
			Attack: edit(base("deep-nesting"), `<detail>`, "<detail>"+strings.Repeat("<x>", 100)+strings.Repeat("</x>", 100)),
			Stage:  pipeline.ErrParse, Cause: cot.ErrTooDeep,
		},
		{
			Name: "doctype", Description: "A report declares a DTD with nested entities: the billion-laughs setup.",
			Expect: "refused by the parser: XML directives are not allowed",
			Attack: append([]byte(`<!DOCTYPE event [<!ENTITY a "lol"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;">]>`), base("doctype")...),
			Stage:  pipeline.ErrParse, Cause: cot.ErrDirective,
		},
		{
			Name: "control-character", Description: "A callsign hides a control character, to corrupt displays and logs.",
			Expect: "refused by the parser: invalid field value",
			Attack: edit(base("control-character"), `callsign="HOSTILE-TEST"`, `callsign="HOSTILE&#9;TEST"`),
			Stage:  pipeline.ErrParse, Cause: cot.ErrInvalidField,
		},
		{
			Name: "namespaced-root", Description: "The root element is in a foreign namespace, to slip past a name check.",
			Expect: "refused by the parser: root element must be an unnamespaced <event>",
			Attack: edit(edit(base("namespaced-root"), `<event `, `<x:event xmlns:x="urn:evil" `), `</event>`, `</x:event>`),
			Stage:  pipeline.ErrParse, Cause: cot.ErrNotEvent,
		},
		{
			Name: "stale-before-start", Description: "A report goes stale before it starts.",
			Expect: "refused by the parser: invalid field value",
			Attack: func() []byte {
				ev := event(uid("stale-before-start"), now)
				ev.Stale = cot.Timestamp(now.Add(-time.Minute))
				return marshal(ev)
			}(),
			Stage: pipeline.ErrParse, Cause: cot.ErrInvalidField,
		},
		{
			Name: "already-stale", Description: "A report that went stale an hour ago.",
			Expect: "refused by the mapper: already stale",
			Attack: func() []byte {
				ev := event(uid("already-stale"), now.Add(-2*time.Hour))
				return marshal(ev)
			}(),
			Stage: pipeline.ErrMap, Cause: mapping.ErrExpired,
		},
		{
			Name: "unknown-affiliation", Description: "A type with an affiliation letter MIL-STD-2525 doesn't define.",
			Expect: "refused by the mapper: unknown affiliation",
			Attack: edit(base("unknown-affiliation"), `type="a-f-G-U-C"`, `type="a-q-G-U-C"`),
			Stage:  pipeline.ErrMap, Cause: mapping.ErrUnsupported,
		},
		{
			Name: "garbage", Description: "Text that isn't XML at all, between two messages.",
			Expect: "refused by the parser, and the next message still gets through",
			Attack: []byte("GET / HTTP/1.1\r\n\r\n"),
			Stage:  pipeline.ErrParse, Cause: cot.ErrTrailingData,
		},
		{
			Name: "entity-reference", Description: "A report uses an entity the document never defines, the billion-laughs payload.",
			Expect: "ends the stream: the framer can't follow it, and nothing is expanded",
			Attack: edit(base("entity-reference"), `callsign="HOSTILE-TEST"`, `callsign="&lol9;"`),
			Cause:  transport.ErrMalformedStream,
		},
		{
			Name: "oversized", Description: "A 100 KiB report, past the 64 KiB message limit.",
			Expect: "ends the stream: message too large",
			Attack: edit(base("oversized"), `<detail>`, `<detail><remarks>`+strings.Repeat("A", 100<<10)+`</remarks>`),
			Cause:  transport.ErrFrameTooLarge,
		},
	}
	return cases
}

// ErrUnknownCase is returned by Find for a name the catalogue doesn't have.
var ErrUnknownCase = errors.New("hostile: no such case")

// Find returns the named case.
func Find(cases []Case, name string) (Case, error) {
	for _, c := range cases {
		if c.Name == name {
			return c, nil
		}
	}
	return Case{}, fmt.Errorf("%w: %q", ErrUnknownCase, name)
}
