package cot

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// validEvent is a complete, valid event. The rejection cases below each change one thing about it.
const validEvent = `<event version="2.0" uid="u-1" type="a-f-G-U-C" time="2026-11-27T16:00:00.000Z" ` +
	`start="2026-11-27T16:00:00.000Z" stale="2026-11-27T16:02:00.000Z" how="m-g">` +
	`<point lat="40.3428" lon="-105.6836" hae="9999999" ce="10" le="9999999"/>` +
	`<detail><contact callsign="SAR-1"/><track course="90" speed="1.4"/></detail></event>`

// with returns validEvent with the first occurrence of old replaced by new, failing the test if old isn't there.
func with(t *testing.T, old, new string) string {
	t.Helper()
	if !strings.Contains(validEvent, old) {
		t.Fatalf("test bug: %q not in validEvent", old)
	}
	return strings.Replace(validEvent, old, new, 1)
}

func TestParseAccepts(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"the base event", validEvent},
		{"an XML declaration", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + validEvent},
		{"comments and surrounding whitespace", "\n<!-- hi -->\n" + validEvent + "\n<!-- bye -->\n"},
		{"unknown detail elements", with(t, `<detail>`, `<detail><takv platform="x"/><status battery="80"/>`)},
		{"repeated unknown elements, such as route points", with(t, `<detail>`, `<detail><link point="1,1"/><link point="2,2"/>`)},
		{"a contact element nested elsewhere", with(t, `<detail>`, `<detail><x><contact/><contact/></x>`)},
		{"no detail at all", with(t, `<detail><contact callsign="SAR-1"/><track course="90" speed="1.4"/></detail>`, ``)},
		{"a timezone offset", with(t, `time="2026-11-27T16:00:00.000Z"`, `time="2026-11-27T18:00:00+02:00"`)},
		{"negative height", with(t, `hae="9999999"`, `hae="-400"`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.in)); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

func TestParseValues(t *testing.T) {
	ev, err := Parse([]byte(validEvent))
	if err != nil {
		t.Fatal(err)
	}
	if ev.UID != "u-1" || ev.Type != "a-f-G-U-C" || ev.How != "m-g" {
		t.Errorf("identity fields wrong: %+v", ev)
	}
	if ev.Point.Lat != 40.3428 || ev.Point.Lon != -105.6836 || ev.Point.HAE != Unknown {
		t.Errorf("point wrong: %+v", ev.Point)
	}
	if want := time.Date(2026, 11, 27, 16, 2, 0, 0, time.UTC); !time.Time(ev.Stale).Equal(want) {
		t.Errorf("stale = %v, want %v", time.Time(ev.Stale), want)
	}
	if ev.Detail == nil || ev.Detail.Contact == nil || ev.Detail.Contact.Callsign != "SAR-1" {
		t.Errorf("callsign missing: %+v", ev.Detail)
	}
}

func TestParseRejects(t *testing.T) {
	deep := "<detail>" + strings.Repeat("<x>", 40) + strings.Repeat("</x>", 40) + "</detail>"
	many := "<detail>" + strings.Repeat("<x/>", 3000) + "</detail>"
	tests := []struct {
		name string
		in   string
		want error
	}{
		// Limits.
		{"too large", validEvent + strings.Repeat(" ", DefaultLimits.MaxBytes), ErrTooLarge},
		{"too deep", with(t, `<detail><contact callsign="SAR-1"/><track course="90" speed="1.4"/></detail>`, deep), ErrTooDeep},
		{"too many elements", with(t, `<detail><contact callsign="SAR-1"/><track course="90" speed="1.4"/></detail>`, many), ErrTooManyElements},
		// Structure.
		{"empty input", ``, ErrNotEvent},
		{"not XML", `hello`, ErrStrayData},
		{"unclosed element", strings.TrimSuffix(validEvent, `</event>`), ErrMalformed},
		{"a DOCTYPE", `<!DOCTYPE event>` + validEvent, ErrDirective},
		{"another processing instruction", `<?foo bar?>` + validEvent, ErrProcInst},
		{"an XML declaration after the root", validEvent + `<?xml version="1.0"?>`, ErrProcInst},
		{"a non-UTF-8 encoding", `<?xml version="1.0" encoding="ISO-8859-1"?>` + validEvent, ErrMalformed},
		{"wrong root", `<message uid="x"/>`, ErrNotEvent},
		{"namespaced root", with(t, `<event `, `<event xmlns="urn:x" `), ErrNotEvent},
		{"two events", validEvent + validEvent, ErrStrayData},
		{"text after the event", validEvent + "trailing", ErrStrayData},
		{"duplicate attribute", with(t, `uid="u-1"`, `uid="u-1" uid="u-2"`), ErrDuplicateAttr},
		{"duplicate attribute in another namespace", with(t, `uid="u-1"`, `uid="u-1" xmlns:x="urn:x" x:uid="u-2"`), ErrDuplicateAttr},
		{"two point elements", with(t, `<detail>`, `<point lat="1" lon="1" hae="0" ce="0" le="0"/><detail>`), ErrDuplicateElement},
		{"two detail elements", with(t, `</event>`, `<detail></detail></event>`), ErrDuplicateElement},
		{"two contacts", with(t, `<contact callsign="SAR-1"/>`, `<contact callsign="A"/><contact callsign="B"/>`), ErrDuplicateElement},
		{"two tracks", with(t, `<track course="90" speed="1.4"/>`, `<track course="90" speed="1.4"/><track course="1" speed="1"/>`), ErrDuplicateElement},
		{"no point", with(t, `<point lat="40.3428" lon="-105.6836" hae="9999999" ce="10" le="9999999"/>`, ``), ErrMissing},
		{"no uid", with(t, ` uid="u-1"`, ``), ErrMissing},
		{"no lat", with(t, ` lat="40.3428"`, ``), ErrMissing},
		// Field values.
		{"empty uid", with(t, `uid="u-1"`, `uid=""`), ErrInvalidField},
		{"control character XML allows (tab) in uid", with(t, `uid="u-1"`, `uid="u&#x9;1"`), ErrInvalidField},
		{"control character XML forbids in uid", with(t, `uid="u-1"`, `uid="u&#x1;1"`), ErrMalformed},
		{"bad type grammar", with(t, `type="a-f-G-U-C"`, `type="a--f"`), ErrInvalidField},
		{"bad how grammar", with(t, `how="m-g"`, `how="m g"`), ErrInvalidField},
		{"latitude out of range", with(t, `lat="40.3428"`, `lat="91"`), ErrInvalidField},
		{"latitude NaN", with(t, `lat="40.3428"`, `lat="NaN"`), ErrInvalidField},
		{"height infinite", with(t, `hae="9999999"`, `hae="Inf"`), ErrInvalidField},
		{"negative circular error", with(t, `ce="10"`, `ce="-1"`), ErrInvalidField},
		{"stale before start", with(t, `stale="2026-11-27T16:02:00.000Z"`, `stale="2026-11-27T15:00:00.000Z"`), ErrInvalidField},
		{"course out of range", with(t, `course="90"`, `course="400"`), ErrInvalidField},
		{"negative speed", with(t, `speed="1.4"`, `speed="-1"`), ErrInvalidField},
		{"unparseable number", with(t, `lat="40.3428"`, `lat="north"`), ErrInvalidField},
		// Go reads these as numbers; CoT, an XML Schema decimal, doesn't, and another parser wouldn't either.
		{"hexadecimal float", with(t, `lat="40.3428"`, `lat="0x1p-2"`), ErrInvalidField},
		{"digit separators", with(t, `lat="40.3428"`, `lat="4_0.3428"`), ErrInvalidField},
		{"unparseable time", with(t, `time="2026-11-27T16:00:00.000Z"`, `time="noon"`), ErrMalformed},
		{"year zero", with(t, `time="2026-11-27T16:00:00.000Z"`, `time="0000-11-27T16:00:00.000Z"`), ErrInvalidField},
		// encoding/xml matches attributes by local name in any namespace, xmlns: declarations included, so a namespaced
		// attribute with a CoT attribute's name could be read as it: it is refused, with or without the real one.
		{"namespace declaration named like a CoT attribute", with(t, `uid="u-1"`, `uid="u-1" xmlns:uid="urn:x"`), ErrDuplicateAttr},
		{"namespaced attribute standing in for a missing one", with(t, ` lat="40.3428"`, ` xmlns:x="urn:x" x:lat="1"`), ErrDuplicateAttr},
		{"namespaced attribute on a child element", with(t, `callsign="SAR-1"`, `callsign="SAR-1" xmlns:x="urn:x" x:callsign="EVIL"`), ErrDuplicateAttr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.in))
			if !errors.Is(err, tt.want) {
				t.Errorf("got error %v, want one wrapping %v", err, tt.want)
			}
		})
	}
}

// TestLimitsAreConfigurable checks that a caller's tighter limits are the ones applied, and that a limit left at zero
// means its default rather than no limit at all.
func TestLimitsAreConfigurable(t *testing.T) {
	tight := Limits{MaxBytes: 100, MaxDepth: 32, MaxElements: 2048}
	if _, err := tight.Parse([]byte(validEvent)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
	if _, err := (Limits{MaxDepth: 2}).Parse([]byte(validEvent)); !errors.Is(err, ErrTooDeep) {
		t.Errorf("only MaxDepth set: got %v, want ErrTooDeep", err)
	}
	if _, err := (Limits{}).Parse([]byte(validEvent)); err != nil {
		t.Errorf("zero Limits: got %v, want the defaults", err)
	}
	huge := with(t, `<detail>`, `<detail><remarks>`+strings.Repeat("A", DefaultLimits.MaxBytes)+`</remarks>`)
	if _, err := (Limits{MaxDepth: 32}).Parse([]byte(huge)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("MaxBytes left at zero: got %v, want the default size limit", err)
	}
}

// TestIsXMLSpace checks the whitespace XML itself allows between elements, which the framer skips: a vertical tab or
// a no-break space isn't, so it is stray data a parser must refuse.
func TestIsXMLSpace(t *testing.T) {
	for in, want := range map[string]bool{"": true, " \t\r\n": true, "\n\n": true, "x": false, " x ": false, "\v": false, " ": false} {
		if got := IsXMLSpace([]byte(in)); got != want {
			t.Errorf("IsXMLSpace(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestForeignAttributesAllowed checks that a namespaced attribute the model doesn't read, the way TAK extensions
// annotate elements, is still accepted.
func TestForeignAttributesAllowed(t *testing.T) {
	in := with(t, `uid="u-1"`, `uid="u-1" xmlns:x="urn:x" x:extension="1"`)
	if _, err := Parse([]byte(in)); err != nil {
		t.Errorf("Parse = %v, want an unread namespaced attribute accepted", err)
	}
}
