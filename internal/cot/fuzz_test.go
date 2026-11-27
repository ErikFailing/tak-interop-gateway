// The fuzz target lives in the external test package cot_test so it can import cotgen for seeds; cotgen itself
// imports cot, so an in-package test importing it would be an import cycle.
package cot_test

import (
	"encoding/xml"
	"math"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
)

// FuzzParse checks two things about any input. Parse must never panic. And whatever Parse accepts must be stable:
// marshaling it and parsing the result has to succeed and reproduce the same bytes. A parser that accepts something it
// can't faithfully re-emit is exactly the kind of quiet disagreement this project exists to catch.
//
// Run it with: go test -fuzz=FuzzParse ./internal/cot
func FuzzParse(f *testing.F) {
	// Seeds: realistic events from cot-gen, plus a few hand-written shapes.
	s, err := cotgen.New(cotgen.Config{
		Units: 2, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836},
		Start: time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC), Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 1,
	})
	if err != nil {
		f.Fatal(err)
	}
	for _, ev := range s.Next() {
		b, err := xml.Marshal(ev)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" uid="x" type="a-u-G" ` +
		`time="2026-11-27T16:00:00Z" start="2026-11-27T16:00:00Z" stale="2026-11-27T16:05:00Z" how="h-e">` +
		`<point lat="0" lon="0" hae="9999999" ce="9999999" le="9999999"/><detail><remarks>it&apos;s "quoted"</remarks>` +
		`</detail></event>`))
	f.Add([]byte(`<event/>`))

	// Re-marshaling can legitimately grow a message, because the encoder writes some single characters as five-byte
	// character references, so the re-parse gets room for that and nothing else.
	relaxed := cot.DefaultLimits
	relaxed.MaxBytes = 8 * cot.DefaultLimits.MaxBytes

	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := cot.Parse(data)
		if err != nil {
			return // rejecting input is always allowed; the properties below are about what gets accepted
		}
		p := ev.Point
		if math.IsNaN(float64(p.Lat)) || p.Lat < -90 || p.Lat > 90 || math.IsNaN(float64(p.Lon)) || p.Lon < -180 || p.Lon > 180 {
			t.Fatalf("accepted an out-of-range position: %+v", p)
		}
		first, err := xml.Marshal(ev)
		if err != nil {
			t.Fatalf("accepted an event it cannot marshal: %v", err)
		}
		again, err := relaxed.Parse(first)
		if err != nil {
			t.Fatalf("rejected its own output: %v\ninput:  %q\noutput: %q", err, data, first)
		}
		second, err := xml.Marshal(again)
		if err != nil {
			t.Fatalf("second marshal failed: %v", err)
		}
		if string(first) != string(second) {
			t.Fatalf("round trip is not stable\nfirst:  %q\nsecond: %q", first, second)
		}
	})
}
