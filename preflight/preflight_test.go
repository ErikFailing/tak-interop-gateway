package preflight_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// checkTime is the fixed clock every test checks against.
var checkTime = time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)

// fixedClock returns a clock stopped at t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// goodTrack is a complete track entity, shaped like the integration guide's worked example, that breaks no rule.
func goodTrack() entity.Entity {
	return entity.Entity{
		EntityID:   new("9c32e7ee-9d63-5364-95cc-e64c9ffe4f32"),
		IsLive:     new(true),
		ExpiryTime: new(checkTime.Add(2 * time.Minute)),
		Aliases:    &entity.Aliases{Name: new("SIM-SAR-1")},
		MilView:    &entity.MilView{Disposition: new(entity.DispositionFriendly), Environment: new(entity.EnvironmentLand)},
		Ontology:   &entity.Ontology{Template: new(entity.TemplateTrack), PlatformType: new("a-f-G-U-C")},
		Location: &entity.Location{Position: &entity.Position{
			LatitudeDegrees: new(40.3428), LongitudeDegrees: new(-105.6836), AltitudeHaeMeters: new(2900.0),
		}},
		Symbology: &entity.Symbology{MilStd2525C: &entity.MilStd2525C{SIDC: new("SFGPUC---------")}},
		Provenance: &entity.Provenance{
			IntegrationName: new("tak-interop-gateway"), DataType: new("cot"), SourceID: new("u-1"),
			SourceUpdateTime: new(checkTime), SourceDescription: new("sar-feed"),
		},
		Tracked:            &entity.Tracked{TrackQualityWrapper: new(2)},
		DataClassification: &entity.Classification{Default: &entity.ClassificationInformation{Level: new(entity.ClassificationUnclassified)}},
	}
}

// asAsset turns the good track into a complete asset: the template, a health roll-up with one component, and one
// sensor. An asset has no track quality, so that goes too.
func asAsset(e *entity.Entity) {
	e.Ontology.Template = new(entity.TemplateAsset)
	e.Tracked = nil
	e.Health = &entity.Health{
		HealthStatus: new(entity.HealthHealthy), ConnectionStatus: new(entity.ConnectionOnline),
		Components: []entity.ComponentHealth{{
			ID: new("input"), Name: new("Input"), Health: new(entity.HealthHealthy),
			Messages: []entity.ComponentMessage{{Status: new(entity.HealthHealthy), Message: new("reading")}},
		}},
	}
	e.Sensors = &entity.Sensors{Sensors: []entity.Sensor{{
		SensorID: new("cot-feed"), SensorType: new(entity.SensorTypeTransponder), OperationalState: new(entity.SensorOperational),
	}}}
}

// hit is one expected violation: which rule, on which field.
type hit struct{ rule, field string }

// hits reduces violations to their rule and field, which is what the table tests compare.
func hits(vs []preflight.Violation) []hit {
	var out []hit
	for _, v := range vs {
		out = append(out, hit{v.Rule.ID, v.Field})
	}
	return out
}

// TestGoodTrackPasses checks that the reference entity breaks nothing, both as a value and as JSON.
func TestGoodTrackPasses(t *testing.T) {
	if vs := preflight.NewChecker(fixedClock(checkTime)).Check(goodTrack()); vs != nil {
		t.Fatalf("Check found violations on the good track: %v", vs)
	}
	data, err := json.Marshal(goodTrack())
	if err != nil {
		t.Fatal(err)
	}
	vs, err := preflight.NewChecker(fixedClock(checkTime)).CheckJSON(data)
	if err != nil || vs != nil {
		t.Fatalf("CheckJSON(good track) = %v, %v; want no violations", vs, err)
	}
}

// TestRules breaks one thing at a time and checks that exactly the expected rules fire, on the expected fields. Every
// rule must be exercised here or in the stateful or JSON tests.
func TestRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(e *entity.Entity)
		want   []hit
	}{
		{"no entity ID", func(e *entity.Entity) { e.EntityID = nil }, []hit{{"entity-id", "entityId"}}},
		{"empty entity ID", func(e *entity.Entity) { e.EntityID = new("") }, []hit{{"entity-id", "entityId"}}},
		{"isLive missing", func(e *entity.Entity) { e.IsLive = nil }, []hit{{"is-live", "isLive"}}},
		{"isLive false", func(e *entity.Entity) { e.IsLive = new(false) }, []hit{{"is-live", "isLive"}}},
		{"no expiry", func(e *entity.Entity) { e.ExpiryTime = nil }, []hit{{"expiry-set", "expiryTime"}}},
		{"no expiry with noExpiry", func(e *entity.Entity) { e.ExpiryTime, e.NoExpiry = nil, new(true) }, nil},
		{"no expiry with noExpiry false", func(e *entity.Entity) { e.ExpiryTime, e.NoExpiry = nil, new(false) }, []hit{{"expiry-set", "expiryTime"}}},
		{"expiry now", func(e *entity.Entity) { e.ExpiryTime = new(checkTime) }, []hit{{"expiry-in-future", "expiryTime"}}},
		{"expiry past", func(e *entity.Entity) { e.ExpiryTime = new(checkTime.Add(-time.Second)) }, []hit{{"expiry-in-future", "expiryTime"}}},
		{"expiry 30 days", func(e *entity.Entity) { e.ExpiryTime = new(checkTime.Add(30 * 24 * time.Hour)) }, []hit{{"expiry-within-30-days", "expiryTime"}}},
		{"expiry just under 30 days", func(e *entity.Entity) { e.ExpiryTime = new(checkTime.Add(30*24*time.Hour - time.Nanosecond)) }, nil},
		{"no provenance", func(e *entity.Entity) { e.Provenance = nil }, []hit{
			{"provenance-integration-name", "provenance"}, {"provenance-data-type", "provenance"},
			{"source-update-time-set", "provenance.sourceUpdateTime"},
		}},
		{"no integration name", func(e *entity.Entity) { e.Provenance.IntegrationName = nil }, []hit{{"provenance-integration-name", "provenance.integrationName"}}},
		{"empty data type", func(e *entity.Entity) { e.Provenance.DataType = new("") }, []hit{{"provenance-data-type", "provenance.dataType"}}},
		{"no source update time", func(e *entity.Entity) { e.Provenance.SourceUpdateTime = nil }, []hit{{"source-update-time-set", "provenance.sourceUpdateTime"}}},
		{"no aliases", func(e *entity.Entity) { e.Aliases = nil }, []hit{{"display-name", "aliases.name"}}},
		{"empty name", func(e *entity.Entity) { e.Aliases.Name = new("") }, []hit{{"display-name", "aliases.name"}}},
		{"bare disposition literal", func(e *entity.Entity) { e.MilView.Disposition = new(entity.Disposition("FRIENDLY")) }, []hit{{"enum-values", "milView.disposition"}}},
		{"bad environment literal", func(e *entity.Entity) { e.MilView.Environment = new(entity.Environment("LAND")) }, []hit{{"enum-values", "milView.environment"}}},
		{"invalid template sentinel", func(e *entity.Entity) { e.Ontology.Template = new(entity.Template("TEMPLATE_INVALID")) }, []hit{{"enum-values", "ontology.template"}}},
		{"unknown template", func(e *entity.Entity) { e.Ontology.Template = new(entity.Template("TRACK")) }, []hit{{"enum-values", "ontology.template"}}},
		{"invalid classification sentinel", func(e *entity.Entity) {
			e.DataClassification.Default.Level = new(entity.ClassificationLevel("CLASSIFICATION_LEVELS_INVALID"))
		}, []hit{{"enum-values", "dataClassification.default.level"}}},
		{"bare classification literal", func(e *entity.Entity) {
			e.DataClassification.Default.Level = new(entity.ClassificationLevel("UNCLASSIFIED"))
		}, []hit{{"enum-values", "dataClassification.default.level"}}},
		{"no platform type", func(e *entity.Entity) { e.Ontology.PlatformType = nil }, []hit{{"template-fields", "ontology.platformType"}}},
		{"no location", func(e *entity.Entity) { e.Location = nil }, []hit{
			{"template-fields", "location.position.latitudeDegrees"}, {"template-fields", "location.position.longitudeDegrees"},
		}},
		{"no milView", func(e *entity.Entity) { e.MilView = nil }, []hit{
			{"template-fields", "milView.disposition"}, {"template-fields", "milView.environment"},
		}},
		{"latitude 91", func(e *entity.Entity) { e.Location.Position.LatitudeDegrees = new(91.0) }, []hit{{"position-range", "location.position.latitudeDegrees"}}},
		{"latitude NaN", func(e *entity.Entity) { e.Location.Position.LatitudeDegrees = new(math.NaN()) }, []hit{{"position-range", "location.position.latitudeDegrees"}}},
		{"longitude -181", func(e *entity.Entity) { e.Location.Position.LongitudeDegrees = new(-181.0) }, []hit{{"position-range", "location.position.longitudeDegrees"}}},
		{"altitude infinite", func(e *entity.Entity) { e.Location.Position.AltitudeHaeMeters = new(math.Inf(1)) }, []hit{{"position-range", "location.position.altitudeHaeMeters"}}},
		{"track quality 16", func(e *entity.Entity) { e.Tracked.TrackQualityWrapper = new(16) }, []hit{{"track-quality-range", "tracked.trackQualityWrapper"}}},
		{"track quality -1", func(e *entity.Entity) { e.Tracked.TrackQualityWrapper = new(-1) }, []hit{{"track-quality-range", "tracked.trackQualityWrapper"}}},
		{"track quality 0 is fine", func(e *entity.Entity) { e.Tracked.TrackQualityWrapper = new(0) }, nil},
		{"track quality 15 is fine", func(e *entity.Entity) { e.Tracked.TrackQualityWrapper = new(15) }, nil},
		{"no track quality", func(e *entity.Entity) { e.Tracked = nil }, []hit{{"track-quality-set", "tracked.trackQualityWrapper"}}},
		{"no template", func(e *entity.Entity) { e.Ontology.Template = nil }, []hit{{"template-set", "ontology.template"}}},
		{"no symbology", func(e *entity.Entity) { e.Symbology = nil }, []hit{{"symbology-set", "symbology.milStd2525C.sidc"}}},
		{"short SIDC", func(e *entity.Entity) { e.Symbology.MilStd2525C.SIDC = new("SFGPUC") }, []hit{{"sidc-format", "symbology.milStd2525C.sidc"}}},
		{"lower-case SIDC", func(e *entity.Entity) { e.Symbology.MilStd2525C.SIDC = new("sfgpuc---------") }, []hit{{"sidc-format", "symbology.milStd2525C.sidc"}}},
		{"SIDC affiliation disagrees", func(e *entity.Entity) { e.Symbology.MilStd2525C.SIDC = new("SHGPUC---------") }, []hit{{"sidc-matches-milview", "symbology.milStd2525C.sidc"}}},
		{"SIDC dimension disagrees", func(e *entity.Entity) { e.MilView.Environment = new(entity.EnvironmentAir) }, []hit{{"sidc-matches-milview", "symbology.milStd2525C.sidc"}}},
		{"SIDC both disagree", func(e *entity.Entity) { e.Symbology.MilStd2525C.SIDC = new("SHAP-----------") }, []hit{
			{"sidc-matches-milview", "symbology.milStd2525C.sidc"}, {"sidc-matches-milview", "symbology.milStd2525C.sidc"},
		}},
		{"exercise SIDC agrees", func(e *entity.Entity) {
			e.MilView.Disposition = new(entity.DispositionSuspicious)
			e.Symbology.MilStd2525C.SIDC = new("SJGPUC---------")
		}, nil},
		{"pending SIDC may use U or P", func(e *entity.Entity) {
			e.MilView.Disposition = new(entity.DispositionPending)
			e.Symbology.MilStd2525C.SIDC = new("SUGPUC---------")
		}, nil},
		{"non-warfighting SIDC isn't compared", func(e *entity.Entity) { e.Symbology.MilStd2525C.SIDC = new("GHGPUC---------") }, nil},
		{"no classification", func(e *entity.Entity) { e.DataClassification = nil }, []hit{{"classification-set", "dataClassification.default.level"}}},
		{"empty classification", func(e *entity.Entity) { e.DataClassification.Default.Level = new(entity.ClassificationLevel("")) }, []hit{
			{"enum-values", "dataClassification.default.level"}, {"classification-set", "dataClassification.default.level"},
		}},
		{"geo template", func(e *entity.Entity) { e.Ontology.Template = new(entity.TemplateGeo) }, []hit{{"unchecked-template-rules", "ontology.template"}}},
		{"asset without health or sensors", func(e *entity.Entity) { e.Ontology.Template = new(entity.TemplateAsset) }, []hit{
			{"asset-health-set", "health.healthStatus"}, {"asset-health-set", "health.connectionStatus"}, {"asset-sensors-set", "sensors.sensors"},
		}},
		{"a complete asset", func(e *entity.Entity) { asAsset(e) }, nil},
		{"asset template still needs track fields", func(e *entity.Entity) {
			asAsset(e)
			e.Ontology.PlatformType = nil
		}, []hit{{"template-fields", "ontology.platformType"}}},
		{"asset with a sensor but no type", func(e *entity.Entity) {
			asAsset(e)
			e.Sensors.Sensors[0].SensorType = nil
		}, []hit{{"asset-sensors-set", "sensors.sensors"}}},
		{"bare sensor type literal", func(e *entity.Entity) {
			asAsset(e)
			e.Sensors.Sensors[0].SensorType = new(entity.SensorType("RADAR")) // the guide's own example of the trap
		}, []hit{{"enum-values", "sensors.sensors[0].sensorType"}}},
		{"bare health literals", func(e *entity.Entity) {
			asAsset(e)
			e.Health.HealthStatus = new(entity.HealthStatus("HEALTHY"))
			e.Health.Components[0].Messages[0].Status = new(entity.HealthStatus("OK"))
			e.Sensors.Sensors[0].OperationalState = new(entity.OperationalState("OPERATIONAL"))
		}, []hit{
			{"enum-values", "health.healthStatus"}, {"enum-values", "health.components[0].messages[0].status"},
			{"enum-values", "sensors.sensors[0].operationalState"},
		}},
		{"health sentinels", func(e *entity.Entity) {
			asAsset(e)
			e.Health.ConnectionStatus = new(entity.ConnectionStatus("CONNECTION_STATUS_INVALID"))
			e.Health.Components[0].Health = new(entity.HealthStatus("HEALTH_STATUS_INVALID"))
		}, []hit{{"enum-values", "health.connectionStatus"}, {"enum-values", "health.components[0].health"}}},
	}
	covered := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := goodTrack()
			tt.mutate(&e)
			got := hits(preflight.NewChecker(fixedClock(checkTime)).Check(e))
			if !slices.Equal(got, tt.want) {
				t.Errorf("violations = %v, want %v", got, tt.want)
			}
		})
		for _, h := range tt.want {
			covered[h.rule] = true
		}
	}

	// The stateful rules and the two that need the raw document have their own tests below.
	for _, id := range []string{"known-fields", "field-name-case", "source-update-time-changes", "source-update-time-increases"} {
		covered[id] = true
	}
	for _, r := range preflight.Rules() {
		if !covered[r.ID] {
			t.Errorf("no test case breaks rule %s", r.ID)
		}
	}
}

// TestSourceUpdateTime checks the stateful rules across a sequence of updates.
func TestSourceUpdateTime(t *testing.T) {
	c := preflight.NewChecker(fixedClock(checkTime))
	update := func(id string, at time.Time) []hit {
		e := goodTrack()
		e.EntityID = new(id)
		e.Provenance.SourceUpdateTime = new(at)
		return hits(c.Check(e))
	}
	steps := []struct {
		name string
		id   string
		at   time.Time
		want []hit
	}{
		{"first update", "a", checkTime, nil},
		{"later update", "a", checkTime.Add(time.Second), nil},
		{"unchanged time", "a", checkTime.Add(time.Second), []hit{{"source-update-time-changes", "provenance.sourceUpdateTime"}}},
		{"earlier time", "a", checkTime, []hit{{"source-update-time-increases", "provenance.sourceUpdateTime"}}},
		{"another entity has its own history", "b", checkTime, nil},
		{"same instant in another zone is unchanged", "b", checkTime.In(time.FixedZone("UTC-7", -7*3600)), []hit{{"source-update-time-changes", "provenance.sourceUpdateTime"}}},
	}
	for _, s := range steps {
		if got := update(s.id, s.at); !slices.Equal(got, s.want) {
			t.Errorf("%s: violations = %v, want %v", s.name, got, s.want)
		}
	}
}

// TestRejectedUpdateIsNotRemembered checks that a Checker remembers an update's time only when the server would accept
// the entity: a rejected update never happened, so a corrected copy with the same time is not a repeat.
func TestRejectedUpdateIsNotRemembered(t *testing.T) {
	c := preflight.NewChecker(fixedClock(checkTime))
	broken := goodTrack()
	broken.IsLive = new(false)
	if !preflight.HasErrors(c.Check(broken)) {
		t.Fatal("the broken entity passed")
	}
	if vs := c.Check(goodTrack()); vs != nil {
		t.Errorf("the corrected copy, with the rejected one's time: %v, want no findings", vs)
	}
	if got := hits(c.Check(goodTrack())); !slices.Equal(got, []hit{{"source-update-time-changes", "provenance.sourceUpdateTime"}}) {
		t.Errorf("a real repeat after an accepted update: %v", got)
	}
}

// TestStatelessCheck checks preflight.Check: the same findings as a fresh Checker, and no history, so the same
// entity twice is not a repeat. A nil clock means the real one.
func TestStatelessCheck(t *testing.T) {
	broken := goodTrack()
	broken.Aliases = nil
	want := hits(preflight.NewChecker(fixedClock(checkTime)).Check(broken))
	if got := hits(preflight.Check(broken, checkTime)); !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("Check = %v, want %v", got, want)
	}
	if vs := preflight.Check(goodTrack(), checkTime); vs != nil {
		t.Errorf("the first check: %v", vs)
	}
	if vs := preflight.Check(goodTrack(), checkTime); vs != nil {
		t.Errorf("the second check of the same update: %v, want no history", vs)
	}
	e := goodTrack()
	e.ExpiryTime = new(time.Now().Add(time.Minute))
	e.Provenance.SourceUpdateTime = new(time.Now())
	if vs := preflight.NewChecker(nil).Check(e); preflight.HasErrors(vs) {
		t.Errorf("with the real clock: %v", vs)
	}
}

// TestRulesAreCopies checks that a caller can't change what the package reports by editing what Rules returns.
func TestRulesAreCopies(t *testing.T) {
	rs := preflight.Rules()
	before := rs[0].Sources[0]
	rs[0].Sources[0] = preflight.Source{Title: "tampered"}
	rs[0].ID = "tampered"
	if again := preflight.Rules(); again[0].Sources[0] != before || again[0].ID == "tampered" {
		t.Error("editing Rules' result changed the package's rules")
	}
	broken := goodTrack()
	broken.EntityID = nil
	for _, v := range preflight.Check(broken, checkTime) {
		v.Rule.Sources[0] = preflight.Source{Title: "tampered"}
	}
	for _, v := range preflight.Check(broken, checkTime) {
		if v.Rule.Sources[0].Title == "tampered" {
			t.Fatal("editing a violation's rule changed the package's rules")
		}
	}
}

// TestCheckJSON checks the rules that need the raw document, and that documents which can't be read unambiguously
// are refused.
func TestCheckJSON(t *testing.T) {
	good, err := json.Marshal(goodTrack())
	if err != nil {
		t.Fatal(err)
	}
	// with returns the good track's JSON with one top-level member replaced, or removed when value is "".
	with := func(key, value string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		delete(m, key)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if value == "" {
			return string(b)
		}
		return strings.TrimSuffix(string(b), "}") + "," + value + "}"
	}

	tests := []struct {
		name       string
		doc        string
		want       []hit
		wantDetail string // a substring of the first violation's detail, when set
	}{
		{"SDK field the model doesn't use", with("", `"description":"a test entity","createdTime":"2026-11-27T17:00:00Z"`), nil, ""},
		{"time with an offset and nanoseconds", with("", `"createdTime":"2026-11-27T10:00:00.123456789-07:00"`), nil, ""},
		{"time left null", with("", `"createdTime":null`), nil, ""},
		{"misspelled top-level key", with("", `"isLiv":true`), []hit{{"known-fields", "isLiv"}}, "not a field of Entity"},
		// A key that differs from a field's name only in case is read as that field by encoding/json and the SDK, but
		// dropped by a case-sensitive reader, so it is its own error rather than an unknown field.
		{"wrong case", with("entityId", `"EntityId":"x"`), []hit{{"field-name-case", "EntityId"}}, `differs only in case from "entityId"`},
		{"wrong case, nested", with("aliases", `"aliases":{"Name":"x"}`), []hit{{"field-name-case", "aliases.Name"}}, "a case-sensitive reader drops it"},
		{"snake case", with("isLive", `"is_live":true`), []hit{{"known-fields", "is_live"}, {"is-live", "isLive"}}, `spells this field "isLive"`},
		{"nested unknown key", with("location", `"location":{"position":{"latitudeDegrees":1,"longitudeDegrees":2,"lat":1}}`), []hit{{"known-fields", "location.position.lat"}}, "not a field of Position"},
		{"unknown key inside an array element", with("aliases", `"aliases":{"name":"x","alternateIds":[{"typo":"x"}]}`), []hit{{"known-fields", "aliases.alternateIds[0].typo"}}, ""},
		// Found by FuzzCheckJSON: an empty key gave an empty field path. Unusual keys are quoted in brackets instead.
		{"empty key", with("", `"":1`), []hit{{"known-fields", `[""]`}}, ""},
		{"key with a dot", with("aliases", `"aliases":{"name":"x","a.b":1}`), []hit{{"known-fields", `aliases["a.b"]`}}, ""},
		{"empty object", "{}", []hit{
			{"entity-id", "entityId"}, {"is-live", "isLive"}, {"expiry-set", "expiryTime"},
			{"provenance-integration-name", "provenance"}, {"provenance-data-type", "provenance"}, {"display-name", "aliases.name"},
			{"source-update-time-set", "provenance.sourceUpdateTime"}, {"template-set", "ontology.template"},
			{"symbology-set", "symbology.milStd2525C.sidc"}, {"classification-set", "dataClassification.default.level"},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs, err := preflight.NewChecker(fixedClock(checkTime)).CheckJSON([]byte(tt.doc))
			if err != nil {
				t.Fatalf("CheckJSON error: %v", err)
			}
			if got := hits(vs); !slices.Equal(got, tt.want) {
				t.Fatalf("violations = %v, want %v", got, tt.want)
			}
			if tt.wantDetail != "" && !strings.Contains(vs[0].Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", vs[0].Detail, tt.wantDetail)
			}
		})
	}
}

// TestCheckJSONRefuses checks every way a document can be refused outright.
func TestCheckJSONRefuses(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantDup bool
	}{
		{"empty input", "", false},
		{"not an object", "[]", false},
		{"null", "null", false},
		{"a string", `"entity"`, false},
		{"truncated", `{"entityId":"a"`, false},
		{"two documents", "{} {}", false},
		{"trailing garbage", "{}x", false},
		{"wrong type", `{"isLive":"yes"}`, false},
		// description is a string in the SDK, and not a field of the model, so only the decode into the SDK's type sees
		// it. (Not createdTime: the SDK reads a number in a date-time field as Unix seconds.)
		{"wrong type on a field only the SDK has", `{"description":5}`, false},
		// Date-times the Go SDK reads but a protobuf Timestamp doesn't: the SDK and the server would disagree.
		{"time as Unix seconds", `{"createdTime":1764259200}`, false},
		{"time with no time zone", `{"createdTime":"2026-11-27T16:00:00"}`, false},
		{"time with a space for the T", `{"createdTime":"2026-11-27 16:00:00Z"}`, false},
		{"date with no time", `{"createdTime":"2026-11-27"}`, false},
		{"time with ten fractional digits", `{"createdTime":"2026-11-27T16:00:00.1234567890Z"}`, false},
		{"year zero", `{"createdTime":"0000-11-27T16:00:00Z"}`, false},
		{"time as an object", `{"createdTime":{"seconds":5}}`, false},
		{"nested time as Unix seconds", `{"health":{"updateTime":1764259200}}`, false},
		{"time in a model field with no time zone", `{"expiryTime":"2026-11-27T18:02:00"}`, false},
		{"syntax error", `{"entityId":}`, false},
		{"too deep", `{"description":` + strings.Repeat("[", 70) + strings.Repeat("]", 70) + `}`, false},
		{"duplicate key", `{"entityId":"a","entityId":"b"}`, true},
		{"duplicate key differing in case", `{"entityId":"a","EntityID":"b"}`, true},
		{"nested duplicate key", `{"aliases":{"name":"a","name":"b"}}`, true},
		{"duplicate key under an unknown key", `{"bogus":{"k":1,"k":2}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs, err := preflight.NewChecker(fixedClock(checkTime)).CheckJSON([]byte(tt.doc))
			if !errors.Is(err, preflight.ErrInvalidJSON) {
				t.Fatalf("CheckJSON(%q) = %v, %v; want ErrInvalidJSON", tt.doc, vs, err)
			}
			if got := errors.Is(err, preflight.ErrDuplicateKey); got != tt.wantDup {
				t.Errorf("errors.Is(err, ErrDuplicateKey) = %v, want %v (err: %v)", got, tt.wantDup, err)
			}
		})
	}
}

// TestModelMatchesSDK fills every field of the entity model and checks that the known-fields rule finds nothing. So
// every JSON name the project writes is one the SDK declares, and a field added to the model with a misspelled tag
// fails here.
func TestModelMatchesSDK(t *testing.T) {
	var e entity.Entity
	fill(reflect.ValueOf(&e).Elem())
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := preflight.NewChecker(fixedClock(checkTime)).CheckJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule.ID == "known-fields" {
			t.Errorf("model field the SDK doesn't declare: %s", v)
		}
	}
}

// fill sets every field reachable from v to a non-zero value, so it appears in the JSON.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(checkTime))
			return
		}
		for i := range v.NumField() {
			fill(v.Field(i))
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int:
		v.SetInt(1)
	case reflect.Float64:
		v.SetFloat(1)
	}
}

// TestRuleMetadata checks the rule list itself: unique kebab-case IDs, one-sentence summaries, and pinned sources.
func TestRuleMetadata(t *testing.T) {
	kebab := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	pinned := regexp.MustCompile(`^https://github\.com/anduril/(lattice-sdk-ai/blob/[0-9a-f]{40}|lattice-sdk-go/blob/v\d+\.\d+\.\d+)/`)
	seen := map[string]bool{}
	for _, r := range preflight.Rules() {
		if !kebab.MatchString(r.ID) || seen[r.ID] {
			t.Errorf("rule ID %q is not unique kebab-case", r.ID)
		}
		seen[r.ID] = true
		if r.Summary == "" || !strings.HasSuffix(r.Summary, ".") {
			t.Errorf("%s: summary %q should be a sentence", r.ID, r.Summary)
		}
		if len(r.Sources) == 0 {
			t.Errorf("%s: no sources", r.ID)
		}
		for _, s := range r.Sources {
			if s.Title == "" || !pinned.MatchString(s.URL) {
				t.Errorf("%s: source %+v is untitled or not pinned to a commit or tag", r.ID, s)
			}
		}
	}
}

// TestSDKVersionMatchesGoMod keeps SDKVersion, and the source links built from it, in step with the SDK the module
// actually builds against. When this fails after an SDK upgrade, recheck the rules and the pinned line numbers.
func TestSDKVersionMatchesGoMod(t *testing.T) {
	mod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	want := "github.com/anduril/lattice-sdk-go/v5 " + preflight.SDKVersion
	if !strings.Contains(string(mod), want) {
		t.Errorf("go.mod does not require %q; update SDKVersion and recheck the rules", want)
	}
}

// TestCheckClassification checks the startup check a gateway runs on its configured level.
func TestCheckClassification(t *testing.T) {
	for level, ok := range map[entity.ClassificationLevel]bool{
		entity.ClassificationUnclassified:               true,
		"CLASSIFICATION_LEVELS_CONTROLLED_UNCLASSIFIED": true,
		"UNCLASSIFIED":                       false,
		"CLASSIFICATION_LEVELS_INVALID":      false,
		"":                                   false,
		"classification_levels_unclassified": false,
	} {
		if err := preflight.CheckClassification(level); (err == nil) != ok {
			t.Errorf("CheckClassification(%q) = %v, want ok=%v", level, err, ok)
		}
	}
}

// TestViolationString pins the format the CLI prints.
func TestViolationString(t *testing.T) {
	e := goodTrack()
	e.IsLive = new(false)
	vs := preflight.NewChecker(fixedClock(checkTime)).Check(e)
	if len(vs) != 1 {
		t.Fatalf("got %v", vs)
	}
	if got, want := vs[0].String(), "error is-live isLive: is false; a publish sets it to true"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if !preflight.HasErrors(vs) {
		t.Error("HasErrors = false for an error violation")
	}
	e = goodTrack()
	e.Symbology = nil
	if vs := preflight.NewChecker(fixedClock(checkTime)).Check(e); preflight.HasErrors(vs) {
		t.Errorf("HasErrors = true for warnings only: %v", vs)
	}
}
