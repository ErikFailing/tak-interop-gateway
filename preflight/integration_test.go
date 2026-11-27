package preflight_test

import (
	"encoding/json"
	"encoding/xml"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// feed is the mapping config the integration tests use: a simulated feed at the documented default track quality.
var feed = mapping.Config{Source: "cotgen", Simulated: true, TrackQuality: mapping.DefaultTrackQuality}

// TestPipelinePasses runs the whole offline pipeline, cot-gen to XML to parser to mapper to JSON to preflight, over a
// scenario and requires zero violations of any severity. A single Checker sees every tick in order, so this also
// proves each entity's source update time moves forward between ticks.
func TestPipelinePasses(t *testing.T) {
	start := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	s, err := cotgen.New(cotgen.Config{
		Units: 3, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: start,
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	c := preflight.NewChecker(func() time.Time { return now })
	checked := 0
	for tick := range 20 {
		for _, ev := range s.Next() {
			now = time.Time(ev.Start) // check each report the moment it is sent
			e := mapped(t, ev, now)
			data, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			vs, err := c.CheckJSON(data)
			if err != nil {
				t.Fatalf("tick %d, %s: %v", tick, ev.UID, err)
			}
			for _, v := range vs {
				t.Errorf("tick %d, %s: %s", tick, ev.UID, v)
			}
			checked++
		}
	}
	if checked != 20*4 {
		t.Errorf("checked %d entities, want %d (3 units and the LKP marker, 20 ticks)", checked, 20*4)
	}
}

// TestEveryAffiliationAndDimension maps one event of each CoT affiliation and battle dimension, plus a type with no
// dimension and one that doesn't transliterate, and requires that preflight accepts every entity. In particular the
// mapper's SIDC must agree with its own disposition and environment.
func TestEveryAffiliationAndDimension(t *testing.T) {
	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	types := []string{"a-f", "a-h-G-UC", "a-f-G-U-C-I"}
	for _, aff := range "fhunpasjkox" {
		for _, dim := range "PAGFSUXZ" {
			types = append(types, "a-"+string(aff)+"-"+string(dim))
		}
	}
	for _, typ := range types {
		ev := cot.Event{
			Version: cot.Version, UID: "uid-" + typ, Type: typ, How: "m-g",
			Time: cot.Timestamp(now), Start: cot.Timestamp(now), Stale: cot.Timestamp(now.Add(time.Minute)),
			Point: cot.Point{Lat: 40, Lon: -105, HAE: cot.Unknown, CE: cot.Unknown, LE: cot.Unknown},
		}
		e := mapped(t, ev, now)
		for _, v := range preflight.NewChecker(fixedClock(now)).Check(e) {
			t.Errorf("%s: %s", typ, v)
		}
	}
}

// mapped round-trips an event through XML and the parser, then maps it, failing the test on any error.
func mapped(t *testing.T, ev cot.Event, now time.Time) entity.Entity {
	t.Helper()
	b, err := xml.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cot.Parse(b)
	if err != nil {
		t.Fatalf("%s: parse: %v", ev.UID, err)
	}
	e, err := mapping.Map(parsed, feed, now)
	if err != nil {
		t.Fatalf("%s: map: %v", ev.UID, err)
	}
	return e
}
