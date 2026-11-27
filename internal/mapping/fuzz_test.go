package mapping_test

import (
	"encoding/json"
	"encoding/xml"
	"regexp"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
)

var (
	uuidV5Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	sidcPattern   = regexp.MustCompile(`^S[A-Z][A-Z]P[A-Z0-9-]{6}-{5}$`)
)

// FuzzMap feeds parser-accepted events to the mapper. Map must never panic on anything the parser accepts, and
// whatever it returns must hold these invariants: a well-formed UUIDv5 ID, live, an expiry after now and within the
// 30-day bound, a non-empty name, the event's own position, a SIDC that is either absent or well-formed, and JSON that
// survives a round trip unchanged.
//
// Run it with: go test -fuzz=FuzzMap ./internal/mapping
func FuzzMap(f *testing.F) {
	start := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	s, err := cotgen.New(cotgen.Config{
		Units: 2, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: start,
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 1,
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
	cfg := mapping.Config{Source: "fuzz-feed", Simulated: true, TrackQuality: 7}

	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := cot.Parse(data)
		if err != nil {
			return
		}
		now := time.Time(ev.Start)
		e, err := mapping.Map(ev, cfg, now)
		if err != nil {
			return // refusing is allowed; the invariants are about what Map returns
		}
		switch {
		case e.EntityID == nil || !uuidV5Pattern.MatchString(*e.EntityID):
			t.Fatalf("bad entity ID: %v", e.EntityID)
		case e.IsLive == nil || !*e.IsLive:
			t.Fatal("entity is not live")
		case e.ExpiryTime == nil || !e.ExpiryTime.After(now) || e.ExpiryTime.After(now.Add(mapping.MaxExpiry)):
			t.Fatalf("expiry %v is outside (now, now+MaxExpiry]", e.ExpiryTime)
		case e.Aliases == nil || e.Aliases.Name == nil || *e.Aliases.Name == "":
			t.Fatal("empty display name")
		case *e.Location.Position.LatitudeDegrees != float64(ev.Point.Lat) || *e.Location.Position.LongitudeDegrees != float64(ev.Point.Lon):
			t.Fatal("position was not copied faithfully")
		case e.Symbology != nil && !sidcPattern.MatchString(*e.Symbology.MilStd2525C.SIDC):
			t.Fatalf("malformed SIDC %q", *e.Symbology.MilStd2525C.SIDC)
		}
		first, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("entity does not marshal: %v", err)
		}
		var back entity.Entity
		if err := json.Unmarshal(first, &back); err != nil {
			t.Fatalf("entity JSON does not unmarshal: %v", err)
		}
		second, err := json.Marshal(back)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Fatalf("JSON round trip is not stable\nfirst:  %s\nsecond: %s", first, second)
		}
	})
}
