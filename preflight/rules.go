package preflight

import (
	"fmt"
	"math"
	"strings"
	"time"

	lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/jsonpath"
)

// SDKVersion is the lattice-sdk-go version the rules were checked against. The enum rule asks this SDK which values
// exist, and the known-fields rule asks it which fields do. A test keeps it equal to the version in go.mod, so an SDK
// upgrade fails the build until someone rechecks the rules and the pinned source links.
const SDKVersion = "v5.5.0"

// The public documents the rules encode, pinned to the commit or tag they were read at.
const (
	guideCommit     = "7a376d8188a874e2dfeccc85847edbc153aac562"
	guideBase       = "https://github.com/anduril/lattice-sdk-ai/blob/" + guideCommit + "/lattice-plugin/skills/lattice-sdk-integration/references/"
	docEntityFields = guideBase + "entity-fields.md"
	docEnums        = guideBase + "enums.md"
	docSDKTypes     = "https://github.com/anduril/lattice-sdk-go/blob/" + SDKVersion + "/types.go"
)

// The sections rules cite. The guide is Anduril's integration skill for coding agents; the SDK's field comments state
// a few rules the guide doesn't.
var (
	srcLiveFloor      = Source{"Integration guide: Required for a live entity", docEntityFields + "#required-for-a-live-entity"}
	srcByTemplate     = Source{"Integration guide: Required by template", docEntityFields + "#required-by-template"}
	srcProvenance     = Source{"Integration guide: Provenance", docEntityFields + "#provenance-provenance"}
	srcChecklist      = Source{"Integration guide: Production-quality fields", docEntityFields + "#production-quality-fields"}
	srcSymbology      = Source{"Integration guide: Symbology", docEntityFields + "#symbology--symbology"}
	srcClassification = Source{"Integration guide: Data classification", docEntityFields + "#data-classification--data_classification"}
	srcTrackQuality   = Source{"Integration guide: Track quality", docEntityFields + "#track-quality--tracked"}
	srcAssetFields    = Source{"Integration guide: TEMPLATE_ASSET additional fields", docEntityFields + "#template_asset-additional-fields"}
	srcEnumTrap       = Source{"Integration guide: the Any-typed enum trap", docEnums}
	srcChoosingValues = Source{"Integration guide: Choosing values", docEnums + "#choosing-values"}
	srcSDKEntityID    = Source{"Go SDK " + SDKVersion + ": Entity.EntityID", docSDKTypes + "#L4579-L4582"}
	srcSDKIsLive      = Source{"Go SDK " + SDKVersion + ": Entity.IsLive", docSDKTypes + "#L4587-L4590"}
	srcSDKExpiry      = Source{"Go SDK " + SDKVersion + ": Entity.ExpiryTime and NoExpiry", docSDKTypes + "#L4597-L4611"}
	srcSDKPosition    = Source{"Go SDK " + SDKVersion + ": Position", docSDKTypes + "#L13679-L13687"}
	srcSDKUpdateTime  = Source{"Go SDK " + SDKVersion + ": Provenance.SourceUpdateTime", docSDKTypes + "#L14672-L14676"}
	srcSDKTracked     = Source{"Go SDK " + SDKVersion + ": Tracked.TrackQualityWrapper", docSDKTypes + "#L18210-L18211"}
	srcSDKTypes       = Source{"Go SDK " + SDKVersion + ": types.go", docSDKTypes}
)

// maxExpiry is the server's bound: the expiry must be less than this far after the current time.
const maxExpiry = 30 * 24 * time.Hour

// finding is one problem a rule's check found, before it is attached to the rule.
type finding struct {
	field  string
	detail string
}

// found is the one-finding result most checks return.
func found(field, format string, args ...any) []finding {
	return []finding{{field: field, detail: fmt.Sprintf(format, args...)}}
}

// rule pairs a Rule with its check. The check sees the Checker for the stateful rules, and the time Check read once.
type rule struct {
	Rule
	check func(c *Checker, e entity.Entity, now time.Time) []finding // nil for known-fields, which CheckJSON applies
}

// ruleKnownFields is applied to the raw document by CheckJSON, so it is named here for CheckJSON to attach.
var ruleKnownFields = Rule{
	ID:       "known-fields",
	Severity: Warning,
	Summary:  "Every JSON key is a field the installed Go SDK declares at that place, spelled exactly; an unknown key is accepted and silently dropped.",
	Sources:  []Source{srcEnumTrap, srcSDKTypes},
}

// ruleFieldNameCase is applied to the raw document by CheckJSON too: like known-fields, it needs the keys as written.
var ruleFieldNameCase = Rule{
	ID:       "field-name-case",
	Severity: Error,
	Summary:  "Every JSON key that names an SDK field is spelled in exactly the field's case. encoding/json and the Go SDK match keys case-insensitively, but the REST API's names are case-sensitive, so a key like EntityId is read here and dropped there, and the other rules can't vouch for it.",
	Sources:  []Source{srcEnumTrap, srcSDKTypes},
}

// rules is every rule in report order: the two raw-document rules first, then the errors, warnings and notes the
// model's rules find, each group roughly in the order a publisher fills the fields.
var rules = []rule{
	{ruleKnownFields, nil},
	{ruleFieldNameCase, nil},
	{Rule{
		ID: "entity-id", Severity: Error,
		Summary: "entityId is set and non-empty. It is the upsert key, so it must also be stable across republishes.",
		Sources: []Source{srcLiveFloor, srcSDKEntityID},
	}, checkEntityID},
	{Rule{
		ID: "is-live", Severity: Error,
		Summary: "isLive is true.",
		Sources: []Source{srcLiveFloor, srcSDKIsLive},
	}, checkIsLive},
	{Rule{
		ID: "expiry-set", Severity: Error,
		Summary: "expiryTime is set, unless noExpiry is true.",
		Sources: []Source{srcLiveFloor, srcSDKExpiry},
	}, checkExpirySet},
	{Rule{
		ID: "expiry-in-future", Severity: Error,
		Summary: "expiryTime is after the current time.",
		Sources: []Source{srcLiveFloor, srcSDKExpiry},
	}, checkExpiryInFuture},
	{Rule{
		ID: "expiry-within-30-days", Severity: Error,
		Summary: "expiryTime is less than 30 days after the current time.",
		Sources: []Source{srcLiveFloor, srcSDKExpiry},
	}, checkExpiryWithin30Days},
	{Rule{
		ID: "provenance-integration-name", Severity: Error,
		Summary: "provenance.integrationName is set and non-empty.",
		Sources: []Source{srcLiveFloor, srcProvenance},
	}, checkIntegrationName},
	{Rule{
		ID: "provenance-data-type", Severity: Error,
		Summary: "provenance.dataType is set and non-empty. The guide's field table lists only integrationName as required, but its example server error is \"provenance.dataType must be set\", so this follows the server.",
		Sources: []Source{srcLiveFloor, srcProvenance},
	}, checkDataType},
	{Rule{
		ID: "display-name", Severity: Error,
		Summary: "aliases.name is set and non-empty: it is the display name operators see.",
		Sources: []Source{srcLiveFloor, srcByTemplate},
	}, checkDisplayName},
	{Rule{
		ID: "enum-values", Severity: Error,
		Summary: "Every enum field the model carries (milView disposition and environment, ontology.template, the classification level, and an asset's health and sensor statuses and sensor types) holds a value the installed SDK knows, and not an INVALID sentinel. The server rejects an unknown value with UNMARSHAL_ERROR.",
		Sources: []Source{srcEnumTrap, srcChoosingValues, srcSDKTypes},
	}, checkEnums},
	{Rule{
		ID: "template-fields", Severity: Error,
		Summary: "A TEMPLATE_TRACK or TEMPLATE_ASSET entity has ontology.platformType, location.position latitude and longitude, and milView disposition and environment.",
		Sources: []Source{srcByTemplate},
	}, checkTemplateFields},
	{Rule{
		ID: "position-range", Severity: Error,
		Summary: "location.position is a WGS84 position: latitude within [-90, 90], longitude within [-180, 180], and finite numbers throughout.",
		Sources: []Source{srcSDKPosition},
	}, checkPositionRange},
	{Rule{
		ID: "track-quality-range", Severity: Error,
		Summary: "tracked.trackQualityWrapper, when set, is from 0 to 15.",
		Sources: []Source{srcSDKTracked, srcTrackQuality},
	}, checkTrackQualityRange},
	{Rule{
		ID: "source-update-time-changes", Severity: Error,
		Summary: "provenance.sourceUpdateTime differs from the same entity's previous update; Entity Manager discards an update whose time didn't change. Checked across the entities given, in order.",
		Sources: []Source{srcSDKUpdateTime, srcProvenance},
	}, checkUpdateTimeChanges},
	{Rule{
		ID: "source-update-time-set", Severity: Warning,
		Summary: "provenance.sourceUpdateTime is set. Lattice keys update events off it.",
		Sources: []Source{srcProvenance, srcSDKUpdateTime},
	}, checkUpdateTimeSet},
	{Rule{
		ID: "source-update-time-increases", Severity: Warning,
		Summary: "provenance.sourceUpdateTime is not earlier than the same entity's previous update. The documentation doesn't say how the server treats a time that moves backward, so this is a warning.",
		Sources: []Source{srcSDKUpdateTime},
	}, checkUpdateTimeIncreases},
	{Rule{
		ID: "template-set", Severity: Warning,
		Summary: "ontology.template is set. Without it, the per-template rules can't be checked.",
		Sources: []Source{srcByTemplate},
	}, checkTemplateSet},
	{Rule{
		ID: "track-quality-set", Severity: Warning,
		Summary: "A TEMPLATE_TRACK entity sets tracked.trackQualityWrapper; the guide calls it required for a well-formed track and suggests 2 when the source has no quality signal.",
		Sources: []Source{srcTrackQuality, srcChecklist},
	}, checkTrackQualitySet},
	{Rule{
		ID: "symbology-set", Severity: Warning,
		Summary: "symbology.milStd2525C.sidc is set. The guide's checklist says always.",
		Sources: []Source{srcChecklist, srcSymbology},
	}, checkSymbologySet},
	{Rule{
		ID: "sidc-format", Severity: Warning,
		Summary: "symbology.milStd2525C.sidc is 15 characters of upper-case letters, digits and dashes.",
		Sources: []Source{srcSymbology},
	}, checkSIDCFormat},
	{Rule{
		ID: "sidc-matches-milview", Severity: Warning,
		Summary: "In a warfighting SIDC (first character S), the affiliation (second character) agrees with milView.disposition and the battle dimension (third) with milView.environment. The guide's mapping is extended with each disposition's other MIL-STD-2525C identities, such as P for pending and J for joker.",
		Sources: []Source{srcSymbology},
	}, checkSIDCMatchesMilView},
	{Rule{
		ID: "classification-set", Severity: Warning,
		Summary: "dataClassification.default.level is set. The guide's checklist says always, with CLASSIFICATION_LEVELS_UNCLASSIFIED if nothing else.",
		Sources: []Source{srcChecklist, srcClassification},
	}, checkClassificationSet},
	{Rule{
		ID: "asset-health-set", Severity: Warning,
		Summary: "A TEMPLATE_ASSET entity sets health.healthStatus and health.connectionStatus. The guide's checklist says always.",
		Sources: []Source{srcChecklist, srcAssetFields},
	}, checkAssetHealth},
	{Rule{
		ID: "asset-sensors-set", Severity: Warning,
		Summary: "A TEMPLATE_ASSET entity declares at least one sensor with a sensorType. The guide's checklist says always: at least the asset's primary sensing capability.",
		Sources: []Source{srcChecklist, srcAssetFields},
	}, checkAssetSensors},
	{Rule{
		ID: "unchecked-template-rules", Severity: Info,
		Summary: "TEMPLATE_GEO needs geoDetails and geoShape, which this checker does not model, so those were not checked.",
		Sources: []Source{srcByTemplate},
	}, checkUncheckedTemplates},
}

// empty reports whether an optional string is missing or empty, and says which for the detail.
func empty(s *string) (string, bool) {
	switch {
	case s == nil:
		return "is not set", true
	case *s == "":
		return "is empty", true
	}
	return "", false
}

// template returns ontology.template, or "" when it isn't set.
func template(e entity.Entity) entity.Template {
	if e.Ontology == nil || e.Ontology.Template == nil {
		return ""
	}
	return *e.Ontology.Template
}

// updateTime returns provenance.sourceUpdateTime, or false when it isn't set.
func updateTime(e entity.Entity) (time.Time, bool) {
	if e.Provenance == nil || e.Provenance.SourceUpdateTime == nil {
		return time.Time{}, false
	}
	return *e.Provenance.SourceUpdateTime, true
}

// stamp formats a time for a detail message.
func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func checkEntityID(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if why, bad := empty(e.EntityID); bad {
		return found("entityId", "%s", why)
	}
	return nil
}

func checkIsLive(_ *Checker, e entity.Entity, _ time.Time) []finding {
	switch {
	case e.IsLive == nil:
		return found("isLive", "is not set")
	case !*e.IsLive:
		return found("isLive", "is false; a publish sets it to true")
	}
	return nil
}

func checkExpirySet(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if e.ExpiryTime == nil && (e.NoExpiry == nil || !*e.NoExpiry) {
		return found("expiryTime", "is not set, and noExpiry is not true")
	}
	return nil
}

func checkExpiryInFuture(_ *Checker, e entity.Entity, now time.Time) []finding {
	if e.ExpiryTime != nil && !e.ExpiryTime.After(now) {
		return found("expiryTime", "%s is not after the check time %s", stamp(*e.ExpiryTime), stamp(now))
	}
	return nil
}

func checkExpiryWithin30Days(_ *Checker, e entity.Entity, now time.Time) []finding {
	if e.ExpiryTime != nil && !e.ExpiryTime.Before(now.Add(maxExpiry)) {
		return found("expiryTime", "%s is not less than 30 days after the check time %s", stamp(*e.ExpiryTime), stamp(now))
	}
	return nil
}

func checkIntegrationName(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if e.Provenance == nil {
		return found("provenance", "is not set")
	}
	if why, bad := empty(e.Provenance.IntegrationName); bad {
		return found("provenance.integrationName", "%s", why)
	}
	return nil
}

func checkDataType(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if e.Provenance == nil {
		return found("provenance", "is not set")
	}
	if why, bad := empty(e.Provenance.DataType); bad {
		return found("provenance.dataType", "%s", why)
	}
	return nil
}

func checkDisplayName(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if e.Aliases == nil {
		return found("aliases.name", "is not set")
	}
	if why, bad := empty(e.Aliases.Name); bad {
		return found("aliases.name", "%s", why)
	}
	return nil
}

func checkEnums(_ *Checker, e entity.Entity, _ time.Time) []finding {
	// 1: Each enum field that is set must parse with the SDK's own constructor. Asking the installed SDK, instead of
	// keeping a copy of the lists, keeps this rule in step with the SDK the project builds against.
	var out []finding
	if e.MilView != nil && e.MilView.Disposition != nil {
		out = append(out, checkEnum("milView.disposition", string(*e.MilView.Disposition), lattice.NewMilViewDispositionFromString, "")...)
	}
	if e.MilView != nil && e.MilView.Environment != nil {
		out = append(out, checkEnum("milView.environment", string(*e.MilView.Environment), lattice.NewMilViewEnvironmentFromString, "")...)
	}

	// 2: Two of the enums have an INVALID member that parses but is never a real value; the guide says never to use it.
	if t := template(e); e.Ontology != nil && e.Ontology.Template != nil {
		out = append(out, checkEnum("ontology.template", string(t), lattice.NewOntologyTemplateFromString, lattice.OntologyTemplateTemplateInvalid)...)
	}
	if c := e.DataClassification; c != nil && c.Default != nil && c.Default.Level != nil {
		out = append(out, checkEnum("dataClassification.default.level", string(*c.Default.Level), lattice.NewClassificationInformationLevelFromString, lattice.ClassificationInformationLevelClassificationLevelsInvalid)...)
	}

	// 3: An asset's health and sensors carry their own enums, every one with an INVALID sentinel.
	if h := e.Health; h != nil {
		if h.HealthStatus != nil {
			out = append(out, checkEnum("health.healthStatus", string(*h.HealthStatus), lattice.NewHealthHealthStatusFromString, lattice.HealthHealthStatusHealthStatusInvalid)...)
		}
		if h.ConnectionStatus != nil {
			out = append(out, checkEnum("health.connectionStatus", string(*h.ConnectionStatus), lattice.NewHealthConnectionStatusFromString, lattice.HealthConnectionStatusConnectionStatusInvalid)...)
		}
		for i, c := range h.Components {
			if c.Health != nil {
				out = append(out, checkEnum(jsonpath.Key(jsonpath.Index("health.components", i), "health"), string(*c.Health), lattice.NewComponentHealthHealthFromString, lattice.ComponentHealthHealthHealthStatusInvalid)...)
			}
			for j, m := range c.Messages {
				if m.Status != nil {
					out = append(out, checkEnum(jsonpath.Key(jsonpath.Index(jsonpath.Key(jsonpath.Index("health.components", i), "messages"), j), "status"), string(*m.Status), lattice.NewComponentMessageStatusFromString, lattice.ComponentMessageStatusHealthStatusInvalid)...)
				}
			}
		}
	}
	if s := e.Sensors; s != nil {
		for i, sensor := range s.Sensors {
			if sensor.SensorType != nil {
				out = append(out, checkEnum(jsonpath.Key(jsonpath.Index("sensors.sensors", i), "sensorType"), string(*sensor.SensorType), lattice.NewSensorSensorTypeFromString, lattice.SensorSensorTypeSensorTypeInvalid)...)
			}
			if sensor.OperationalState != nil {
				out = append(out, checkEnum(jsonpath.Key(jsonpath.Index("sensors.sensors", i), "operationalState"), string(*sensor.OperationalState), lattice.NewSensorOperationalStateFromString, lattice.SensorOperationalStateOperationalStateInvalid)...)
			}
		}
	}
	return out
}

// CheckClassification returns an error if level is not a classification level the installed SDK knows, or is the
// INVALID sentinel. A gateway can check its configured level once at startup this way, instead of having every entity
// rejected later by the enum-values rule.
func CheckClassification(level entity.ClassificationLevel) error {
	fs := checkEnum("dataClassification.default.level", string(level), lattice.NewClassificationInformationLevelFromString,
		lattice.ClassificationInformationLevelClassificationLevelsInvalid)
	if fs != nil {
		return fmt.Errorf("preflight: classification level %s", fs[0].detail)
	}
	return nil
}

// checkEnum checks one enum value with the SDK constructor parse, and against sentinel when the enum has one.
func checkEnum[T ~string](field, value string, parse func(string) (T, error), sentinel T) []finding {
	v, err := parse(value)
	switch {
	case err != nil:
		return found(field, "%q is not a value of this enum in lattice-sdk-go %s", value, SDKVersion)
	case sentinel != "" && v == sentinel:
		return found(field, "%q is the enum's invalid sentinel, never a real value", value)
	}
	return nil
}

func checkTemplateFields(_ *Checker, e entity.Entity, _ time.Time) []finding {
	// 1: Only tracks and assets have this set of required components.
	if t := template(e); t != entity.TemplateTrack && t != entity.TemplateAsset {
		return nil
	}

	// 2: Report each missing field on its own, so the fix for each is obvious.
	var out []finding
	if e.Ontology == nil {
		out = append(out, finding{"ontology.platformType", "is not set"})
	} else if why, bad := empty(e.Ontology.PlatformType); bad {
		out = append(out, finding{"ontology.platformType", why})
	}
	var pos *entity.Position
	if e.Location != nil {
		pos = e.Location.Position
	}
	if pos == nil || pos.LatitudeDegrees == nil {
		out = append(out, finding{"location.position.latitudeDegrees", "is not set"})
	}
	if pos == nil || pos.LongitudeDegrees == nil {
		out = append(out, finding{"location.position.longitudeDegrees", "is not set"})
	}
	if e.MilView == nil || e.MilView.Disposition == nil {
		out = append(out, finding{"milView.disposition", "is not set"})
	}
	if e.MilView == nil || e.MilView.Environment == nil {
		out = append(out, finding{"milView.environment", "is not set"})
	}
	return out
}

func checkPositionRange(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if e.Location == nil || e.Location.Position == nil {
		return nil
	}
	p := e.Location.Position
	var out []finding
	if p.LatitudeDegrees != nil && !within(*p.LatitudeDegrees, -90, 90) {
		out = append(out, finding{"location.position.latitudeDegrees", fmt.Sprintf("%v is outside [-90, 90]", *p.LatitudeDegrees)})
	}
	if p.LongitudeDegrees != nil && !within(*p.LongitudeDegrees, -180, 180) {
		out = append(out, finding{"location.position.longitudeDegrees", fmt.Sprintf("%v is outside [-180, 180]", *p.LongitudeDegrees)})
	}
	if p.AltitudeHaeMeters != nil && (math.IsNaN(*p.AltitudeHaeMeters) || math.IsInf(*p.AltitudeHaeMeters, 0)) {
		out = append(out, finding{"location.position.altitudeHaeMeters", fmt.Sprintf("%v is not a finite number", *p.AltitudeHaeMeters)})
	}
	return out
}

// within reports whether v is in [lo, hi]. It is false for NaN, which compares false with everything, so a range check
// written with it also rejects NaN.
func within(v, lo, hi float64) bool {
	return v >= lo && v <= hi
}

func checkTrackQualityRange(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if e.Tracked != nil && e.Tracked.TrackQualityWrapper != nil {
		if q := *e.Tracked.TrackQualityWrapper; q < 0 || q > 15 {
			return found("tracked.trackQualityWrapper", "%d is outside 0 to 15", q)
		}
	}
	return nil
}

func checkUpdateTimeChanges(c *Checker, e entity.Entity, _ time.Time) []finding {
	t, ok := updateTime(e)
	if !ok || e.EntityID == nil {
		return nil
	}
	if prev, seen := c.last[*e.EntityID]; seen && t.Equal(prev) {
		return found("provenance.sourceUpdateTime", "%s is the same as this entity's previous update", stamp(t))
	}
	return nil
}

func checkUpdateTimeSet(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if _, ok := updateTime(e); !ok {
		return found("provenance.sourceUpdateTime", "is not set")
	}
	return nil
}

func checkUpdateTimeIncreases(c *Checker, e entity.Entity, _ time.Time) []finding {
	t, ok := updateTime(e)
	if !ok || e.EntityID == nil {
		return nil
	}
	if prev, seen := c.last[*e.EntityID]; seen && t.Before(prev) {
		return found("provenance.sourceUpdateTime", "%s is earlier than this entity's previous update, %s", stamp(t), stamp(prev))
	}
	return nil
}

func checkTemplateSet(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if e.Ontology == nil || e.Ontology.Template == nil {
		return found("ontology.template", "is not set")
	}
	return nil
}

func checkTrackQualitySet(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if template(e) == entity.TemplateTrack && (e.Tracked == nil || e.Tracked.TrackQualityWrapper == nil) {
		return found("tracked.trackQualityWrapper", "is not set on a TEMPLATE_TRACK entity")
	}
	return nil
}

// sidc returns symbology.milStd2525C.sidc, or "" when it isn't set.
func sidc(e entity.Entity) string {
	if e.Symbology == nil || e.Symbology.MilStd2525C == nil || e.Symbology.MilStd2525C.SIDC == nil {
		return ""
	}
	return *e.Symbology.MilStd2525C.SIDC
}

func checkSymbologySet(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if sidc(e) == "" {
		return found("symbology.milStd2525C.sidc", "is not set")
	}
	return nil
}

// wellFormedSIDC reports whether s is 15 characters of upper-case ASCII letters, digits and dashes.
func wellFormedSIDC(s string) bool {
	if len(s) != 15 {
		return false
	}
	for i := range len(s) {
		if !sidcChar(s[i]) {
			return false
		}
	}
	return true
}

// sidcChar reports whether b may appear in a SIDC: an upper-case ASCII letter, a digit, or the dash for an unset field.
func sidcChar(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-'
}

func checkSIDCFormat(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if s := sidc(e); s != "" && !wellFormedSIDC(s) {
		return found("symbology.milStd2525C.sidc", "%q is not 15 characters of A-Z, 0-9 and -", s)
	}
	return nil
}

// sidcAffiliations lists, for each disposition, the MIL-STD-2525C standard identities (SIDC position 2) that agree with
// it. The first letter of each is the guide's own mapping where it gives one (pending maps to U there), and the rest
// are the same identity's other 2525C letters: the exercise forms, and none-specified for unknown.
var sidcAffiliations = map[entity.Disposition]string{
	entity.DispositionFriendly:        "FD",
	entity.DispositionHostile:         "HK",
	entity.DispositionNeutral:         "NL",
	entity.DispositionUnknown:         "UWO",
	entity.DispositionPending:         "UPG",
	entity.DispositionAssumedFriendly: "AM",
	entity.DispositionSuspicious:      "SJ",
}

// sidcDimensions lists, for each environment, the battle dimensions (SIDC position 3) that agree with it. The guide
// gives air, surface, land and subsurface; space, special operations forces (land) and unknown complete the set.
var sidcDimensions = map[entity.Environment]string{
	entity.EnvironmentAir:        "A",
	entity.EnvironmentSurface:    "S",
	entity.EnvironmentLand:       "GF",
	entity.EnvironmentSubSurface: "U",
	entity.EnvironmentSpace:      "P",
	entity.EnvironmentUnknown:    "ZX",
}

func checkSIDCMatchesMilView(_ *Checker, e entity.Entity, _ time.Time) []finding {
	// 1: Only a well-formed warfighting SIDC encodes affiliation and dimension at these positions; sidc-format reports
	// a malformed one.
	s := sidc(e)
	if !wellFormedSIDC(s) || s[0] != 'S' || e.MilView == nil {
		return nil
	}

	// 2: Compare each position with its milView field, when the field holds a value this table knows. enum-values
	// reports a value that isn't one.
	var out []finding
	if d := e.MilView.Disposition; d != nil {
		if ok, known := sidcAffiliations[*d]; known && !strings.ContainsRune(ok, rune(s[1])) {
			out = append(out, finding{"symbology.milStd2525C.sidc", fmt.Sprintf("affiliation %q disagrees with milView.disposition %s, which expects one of %q", s[1], *d, ok)})
		}
	}
	if env := e.MilView.Environment; env != nil {
		if ok, known := sidcDimensions[*env]; known && !strings.ContainsRune(ok, rune(s[2])) {
			out = append(out, finding{"symbology.milStd2525C.sidc", fmt.Sprintf("battle dimension %q disagrees with milView.environment %s, which expects one of %q", s[2], *env, ok)})
		}
	}
	return out
}

func checkClassificationSet(_ *Checker, e entity.Entity, _ time.Time) []finding {
	c := e.DataClassification
	if c == nil || c.Default == nil || c.Default.Level == nil || *c.Default.Level == "" {
		return found("dataClassification.default.level", "is not set")
	}
	return nil
}

func checkAssetHealth(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if template(e) != entity.TemplateAsset {
		return nil
	}
	var out []finding
	if e.Health == nil || e.Health.HealthStatus == nil {
		out = append(out, finding{"health.healthStatus", "is not set on a TEMPLATE_ASSET entity"})
	}
	if e.Health == nil || e.Health.ConnectionStatus == nil {
		out = append(out, finding{"health.connectionStatus", "is not set on a TEMPLATE_ASSET entity"})
	}
	return out
}

func checkAssetSensors(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if template(e) != entity.TemplateAsset {
		return nil
	}
	if e.Sensors != nil {
		for _, s := range e.Sensors.Sensors {
			if s.SensorType != nil {
				return nil
			}
		}
	}
	return found("sensors.sensors", "no sensor with a sensorType on a TEMPLATE_ASSET entity")
}

func checkUncheckedTemplates(_ *Checker, e entity.Entity, _ time.Time) []finding {
	if template(e) == entity.TemplateGeo {
		return found("ontology.template", "TEMPLATE_GEO also needs geoDetails and geoShape (and location for an ellipse), which this checker does not model")
	}
	return nil
}
