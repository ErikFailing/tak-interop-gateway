package cot

import (
	"encoding/xml"
	"testing"
	"time"
)

// sampleEvent is a fully populated event with fixed values, shared by the tests below.
func sampleEvent() Event {
	ts := Timestamp(time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC))
	return Event{
		Version: Version,
		UID:     "cotgen-sar-1",
		Type:    "a-f-G-U-C",
		Time:    ts,
		Start:   ts,
		Stale:   Timestamp(time.Date(2026, 11, 27, 16, 2, 0, 0, time.UTC)),
		How:     "m-g",
		Point:   Point{Lat: 40.3428, Lon: -105.6836, HAE: Unknown, CE: 10, LE: Unknown},
		Detail: &Detail{
			Contact: &Contact{Callsign: "SIM-SAR-1"},
			Group:   &Group{Name: "Cyan", Role: "Team Member"},
			Track:   &Track{Course: 90, Speed: 1.4},
			Remarks: &Remarks{Text: "Simulated"},
		},
	}
}

// TestMarshalGolden pins the exact XML this project writes, so a change to the format is a deliberate one.
func TestMarshalGolden(t *testing.T) {
	got, err := xml.Marshal(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	want := `<event version="2.0" uid="cotgen-sar-1" type="a-f-G-U-C" time="2026-11-27T16:00:00.000Z" ` +
		`start="2026-11-27T16:00:00.000Z" stale="2026-11-27T16:02:00.000Z" how="m-g">` +
		`<point lat="40.3428" lon="-105.6836" hae="9999999" ce="10" le="9999999"></point>` +
		`<detail><contact callsign="SIM-SAR-1"></contact><__group name="Cyan" role="Team Member"></__group>` +
		`<track course="90" speed="1.4"></track><remarks>Simulated</remarks></detail></event>`
	if string(got) != want {
		t.Errorf("marshal mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestRoundTrip checks that marshal, unmarshal and marshal again gives identical bytes.
func TestRoundTrip(t *testing.T) {
	first, err := xml.Marshal(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	var parsed Event
	if err := xml.Unmarshal(first, &parsed); err != nil {
		t.Fatal(err)
	}
	second, err := xml.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("round trip changed the output\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestNumberNoExponent covers the reason Number exists: large and small values must never use exponent notation.
func TestNumberNoExponent(t *testing.T) {
	tests := []struct {
		in   Number
		want string
	}{
		{Unknown, "9999999"},
		{0.000001, "0.000001"},
		{-105.6836, "-105.6836"},
		{0, "0"},
	}
	for _, tt := range tests {
		attr, err := tt.in.MarshalXMLAttr(xml.Name{Local: "n"})
		if err != nil {
			t.Fatal(err)
		}
		if attr.Value != tt.want {
			t.Errorf("Number(%v) = %q, want %q", float64(tt.in), attr.Value, tt.want)
		}
	}
}

// TestUnmarshalBadAttr checks that malformed numbers and timestamps are reported as errors, not silently zeroed.
func TestUnmarshalBadAttr(t *testing.T) {
	tests := []struct {
		name string
		xml  string
	}{
		{"bad number", `<event><point lat="north" lon="0" hae="0" ce="0" le="0"/></event>`},
		{"bad time", `<event time="yesterday"><point lat="0" lon="0" hae="0" ce="0" le="0"/></event>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e Event
			if err := xml.Unmarshal([]byte(tt.xml), &e); err == nil {
				t.Error("want an error, got nil")
			}
		})
	}
}
