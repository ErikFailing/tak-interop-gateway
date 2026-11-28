package mapping

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
)

// base is the instant every test event is reported at.
var base = time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

// testEvent is a friendly ground unit with a callsign, heading north at 2 m/s, valid for two minutes.
func testEvent() cot.Event {
	return cot.Event{
		Version: cot.Version, UID: "u-1", Type: "a-f-G-U-C", How: "m-g",
		Time: cot.Timestamp(base), Start: cot.Timestamp(base), Stale: cot.Timestamp(base.Add(2 * time.Minute)),
		Point: cot.Point{Lat: 40.3428, Lon: -105.6836, HAE: cot.Unknown, CE: 10, LE: cot.Unknown},
		Detail: &cot.Detail{
			Contact: &cot.Contact{Callsign: "SAR-1"},
			Track:   &cot.Track{Course: 0, Speed: 2},
		},
	}
}

// testConfig is a simulated feed.
func testConfig() Config {
	return Config{Source: "sar-feed", Simulated: true, TrackQuality: 7}
}

// TestUUIDv5KnownAnswers checks the UUIDv5 code against values computed independently with Python's uuid module:
// the RFC 9562 example, and this project's own ID scheme.
func TestUUIDv5KnownAnswers(t *testing.T) {
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got, want := UUIDv5(dns, "www.example.com"), "2ed6657d-e927-568b-95e1-2665a8aea6a2"; got != want {
		t.Errorf("RFC example: got %s, want %s", got, want)
	}
	tests := []struct{ source, uid, want string }{
		{"sar-feed", "u-1", "9c32e7ee-9d63-5364-95cc-e64c9ffe4f32"},
		{"other-feed", "u-1", "5a3edc7a-3ddd-583d-8590-ec2c815f8d34"},
		{"sar-feed", "u-2", "66d729fd-5a26-517d-907d-28dd79cb1020"},
	}
	for _, tt := range tests {
		if got := entityID(tt.source, tt.uid); got != tt.want {
			t.Errorf("entityID(%q, %q) = %s, want %s", tt.source, tt.uid, got, tt.want)
		}
	}
}

// TestJSONGolden pins the exact wire shape: Lattice REST field names, and only the fields that are set.
func TestJSONGolden(t *testing.T) {
	e, err := Map(testEvent(), testConfig(), base.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"entityId":"9c32e7ee-9d63-5364-95cc-e64c9ffe4f32","isLive":true,"expiryTime":"2026-11-27T16:02:00Z",` +
		`"aliases":{"name":"SAR-1"},"milView":{"disposition":"DISPOSITION_FRIENDLY","environment":"ENVIRONMENT_LAND"},` +
		`"ontology":{"template":"TEMPLATE_TRACK","platformType":"a-f-G-U-C"},` +
		`"location":{"position":{"latitudeDegrees":40.3428,"longitudeDegrees":-105.6836},"velocityEnu":{"e":0,"n":2}},` +
		`"locationUncertainty":{"positionErrorEllipse":{"semiMajorAxisM":10,"semiMinorAxisM":10,"orientationD":0}},` +
		`"symbology":{"milStd2525C":{"sidc":"SFGPUC---------"}},` +
		`"provenance":{"integrationName":"tak-interop-gateway","dataType":"cot","sourceId":"u-1",` +
		`"sourceUpdateTime":"2026-11-27T16:00:00Z","sourceDescription":"sar-feed"},` +
		`"indicators":{"simulated":true},"tracked":{"trackQualityWrapper":7},` +
		`"dataClassification":{"default":{"level":"CLASSIFICATION_LEVELS_UNCLASSIFIED"}}}`
	if string(got) != want {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestDisposition(t *testing.T) {
	tests := []struct {
		letter   string
		want     entity.Disposition
		exercise bool
	}{
		{"f", entity.DispositionFriendly, false},
		{"a", entity.DispositionAssumedFriendly, false},
		{"n", entity.DispositionNeutral, false},
		{"u", entity.DispositionUnknown, false},
		{"o", entity.DispositionUnknown, false},
		{"x", entity.DispositionUnknown, false},
		{"p", entity.DispositionPending, false},
		{"s", entity.DispositionSuspicious, false},
		{"h", entity.DispositionHostile, false},
		{"j", entity.DispositionSuspicious, true}, // joker: an exercise track acting as a suspect
		{"k", entity.DispositionHostile, true},    // faker: an exercise track acting as a hostile
		{"F", entity.DispositionFriendly, false},
	}
	for _, tt := range tests {
		d, ex, ok := dispositionFor(tt.letter)
		if !ok || d != tt.want || ex != tt.exercise {
			t.Errorf("dispositionFor(%q) = %v, %v, %v; want %v, %v, true", tt.letter, d, ex, ok, tt.want, tt.exercise)
		}
	}
	if _, _, ok := dispositionFor("z"); ok {
		t.Error("dispositionFor accepted an unknown letter")
	}
}

func TestEnvironment(t *testing.T) {
	tests := map[string]entity.Environment{
		"P": entity.EnvironmentSpace, "A": entity.EnvironmentAir, "G": entity.EnvironmentLand,
		"F": entity.EnvironmentLand, "S": entity.EnvironmentSurface, "U": entity.EnvironmentSubSurface,
		"X": entity.EnvironmentUnknown, "Z": entity.EnvironmentUnknown, "g": entity.EnvironmentLand,
	}
	for letter, want := range tests {
		if got, ok := environmentFor(letter); !ok || got != want {
			t.Errorf("environmentFor(%q) = %v, %v; want %v", letter, got, ok, want)
		}
	}
	if _, ok := environmentFor("Q"); ok {
		t.Error("environmentFor accepted an unknown letter")
	}
}

func TestSIDC(t *testing.T) {
	tests := []struct {
		cotType string
		want    string // "" means no SIDC
	}{
		{"a-f-G-U-C", "SFGPUC---------"}, // the same SIDC Anduril's own samples use
		{"a-h-A", "SHAP-----------"},
		{"a-u-G", "SUGP-----------"},
		{"a-f-G-U-C-I", "SFGPUCI--------"},
		{"a-j-G-U-C", "SJGPUC---------"},
		{"a-o-G", "SUGP-----------"}, // "none specified" has no 2525C identity, so unknown
		{"a-f-G-1-2-3-4-5-6", "SFGP123456-----"},
		{"a-f", ""},                 // no battle dimension
		{"a-f-G-UC", ""},            // a multi-character atom doesn't transliterate
		{"a-f-G-1-2-3-4-5-6-7", ""}, // more than six function characters
		{"a-ff-G", ""},              // a multi-character affiliation
	}
	for _, tt := range tests {
		got, ok := sidcFor(splitType(tt.cotType))
		if tt.want == "" {
			if ok {
				t.Errorf("sidcFor(%s) = %q, want none", tt.cotType, got)
			}
			continue
		}
		if !ok || got != tt.want {
			t.Errorf("sidcFor(%s) = %q, %v; want %q", tt.cotType, got, ok, tt.want)
		}
		if len(got) != 15 {
			t.Errorf("sidcFor(%s) is %d characters, want 15", tt.cotType, len(got))
		}
	}
}

// splitType mirrors how Map splits a type into atoms.
func splitType(s string) []string {
	var out []string
	start := 0
	for i := range len(s) + 1 {
		if i == len(s) || s[i] == '-' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

func TestMapRefuses(t *testing.T) {
	now := base.Add(30 * time.Second)
	withType := func(typ string) cot.Event { e := testEvent(); e.Type = typ; return e }
	staleAt := func(d time.Duration) cot.Event { e := testEvent(); e.Stale = cot.Timestamp(base.Add(d)); return e }
	tests := []struct {
		name string
		ev   cot.Event
		cfg  Config
		want error
	}{
		{"a chat message", withType("b-t-f"), testConfig(), ErrNotAnAtom},
		{"a drawing", withType("u-d-f"), testConfig(), ErrNotAnAtom},
		{"an empty type", withType(""), testConfig(), ErrNotAnAtom},
		{"an atom with no affiliation", withType("a"), testConfig(), ErrUnsupported},
		{"an unknown affiliation", withType("a-z-G"), testConfig(), ErrUnsupported},
		{"an unknown battle dimension", withType("a-f-Q"), testConfig(), ErrUnsupported},
		{"already stale", staleAt(10 * time.Second), testConfig(), ErrExpired},
		{"stale exactly now", staleAt(30 * time.Second), testConfig(), ErrExpired},
		{"no source", testEvent(), Config{TrackQuality: 7}, ErrConfig},
		{"a control character in the source", testEvent(), Config{Source: "a\x00b"}, ErrConfig},
		{"track quality out of range", testEvent(), Config{Source: "s", TrackQuality: 16}, ErrConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Map(tt.ev, tt.cfg, now); !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
}

// TestSymbologyAlwaysSet checks the fallback: the integration guide lists a SIDC as always required, so a type that
// doesn't transliterate still gets a generic symbol from its affiliation and battle dimension.
func TestSymbologyAlwaysSet(t *testing.T) {
	now := base.Add(30 * time.Second)
	tests := map[string]string{
		"a-f-G-U-C": "SFGPUC---------", // transliterates in full
		"a-f":       "SFZP-----------", // no battle dimension: unknown
		"a-h-A-UC":  "SHAP-----------", // a multi-character function atom: function left unspecified
		"a-x-S":     "SUSP-----------", // "other" has no 2525C identity: unknown
	}
	for typ, want := range tests {
		e := testEvent()
		e.Type = typ
		got, err := Map(e, testConfig(), now)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if got.Symbology == nil || *got.Symbology.MilStd2525C.SIDC != want {
			t.Errorf("%s: SIDC %v, want %s", typ, got.Symbology, want)
		}
	}
}

// TestClassificationFromConfig checks that the feed's classification, not the default, is what the entity carries.
func TestClassificationFromConfig(t *testing.T) {
	now := base.Add(30 * time.Second)
	cfg := testConfig()
	cfg.Classification = "CLASSIFICATION_LEVELS_CONTROLLED_UNCLASSIFIED"
	got, err := Map(testEvent(), cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if *got.DataClassification.Default.Level != cfg.Classification {
		t.Errorf("classification %v, want the feed's %v", *got.DataClassification.Default.Level, cfg.Classification)
	}
}

func TestExpiryClamp(t *testing.T) {
	now := base
	e := testEvent()
	e.Stale = cot.Timestamp(base.Add(60 * 24 * time.Hour))
	got, err := Map(e, testConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(MaxExpiry); !got.ExpiryTime.Equal(want) {
		t.Errorf("expiry = %v, want it clamped to %v", *got.ExpiryTime, want)
	}
}

func TestOptionalFields(t *testing.T) {
	now := base.Add(30 * time.Second)
	e := testEvent()
	e.Detail = nil // no callsign, no track
	e.Point.HAE = 1600
	e.Point.CE = cot.Unknown
	cfg := testConfig()
	cfg.Simulated = false
	got, err := Map(e, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Aliases.Name != "u-1" {
		t.Errorf("name = %q, want the uid when there's no callsign", *got.Aliases.Name)
	}
	if got.Location.Position.AltitudeHaeMeters == nil || *got.Location.Position.AltitudeHaeMeters != 1600 {
		t.Error("a known altitude was not mapped")
	}
	if got.Location.VelocityEnu != nil {
		t.Error("velocity was set without a track")
	}
	if got.LocationUncertainty != nil {
		t.Error("uncertainty was set from an unknown ce")
	}
	if got.Indicators != nil {
		t.Error("indicators were set with nothing to indicate")
	}

	// Velocity: due east at 2 m/s is all east.
	e = testEvent()
	e.Detail.Track = &cot.Track{Course: 90, Speed: 2}
	got, err = Map(e, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Location.VelocityEnu; math.Abs(*v.E-2) > 1e-9 || math.Abs(*v.N) > 1e-9 || v.U != nil {
		t.Errorf("velocity = (%v, %v, %v), want (2, 0, unset)", *v.E, *v.N, v.U)
	}

	// The exercise flag comes from the joker and faker affiliations.
	e = testEvent()
	e.Type = "a-k-A"
	got, err = Map(e, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Indicators == nil || got.Indicators.Exercise == nil || !*got.Indicators.Exercise {
		t.Error("a faker did not get the exercise flag")
	}
	if *got.MilView.Disposition != entity.DispositionHostile {
		t.Errorf("a faker mapped to %v, want hostile", *got.MilView.Disposition)
	}
}

// TestCotGenScenario maps a whole generated scenario, after a marshal and parse round trip, and checks every entity
// carries what the server requires of a live track.
func TestCotGenScenario(t *testing.T) {
	s, err := cotgen.New(cotgen.Config{
		Units: 3, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: base,
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for range 3 {
		for _, generated := range s.Next() {
			raw, err := xml.Marshal(generated)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := cot.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			e, err := Map(ev, testConfig(), time.Time(ev.Time))
			if err != nil {
				t.Fatalf("%s: %v", ev.UID, err)
			}
			ids[*e.EntityID] = true
			switch {
			case e.EntityID == nil, e.IsLive == nil || !*e.IsLive, e.ExpiryTime == nil:
				t.Errorf("%s: missing id, live flag or expiry", ev.UID)
			case e.Aliases == nil || e.Aliases.Name == nil || *e.Aliases.Name == "":
				t.Errorf("%s: missing display name", ev.UID)
			case e.Provenance == nil || e.Provenance.IntegrationName == nil || e.Provenance.DataType == nil || e.Provenance.SourceUpdateTime == nil:
				t.Errorf("%s: incomplete provenance", ev.UID)
			case e.MilView == nil || e.Ontology == nil || e.Location == nil || e.Tracked == nil:
				t.Errorf("%s: missing a component a track needs", ev.UID)
			}
			wantDisposition := entity.DispositionFriendly
			if ev.UID == "cotgen-lkp-1" {
				wantDisposition = entity.DispositionUnknown
			}
			if *e.MilView.Disposition != wantDisposition {
				t.Errorf("%s: disposition %v, want %v", ev.UID, *e.MilView.Disposition, wantDisposition)
			}
		}
	}
	if len(ids) != 4 { // three team members and the marker, the same IDs every tick
		t.Errorf("got %d distinct entity IDs over three ticks, want 4", len(ids))
	}
}
