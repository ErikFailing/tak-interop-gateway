package mapping

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
)

// Errors returned by Map. Each failure wraps one of these.
var (
	ErrConfig      = errors.New("mapping: invalid config")
	ErrNotAnAtom   = errors.New("mapping: only atom events (type a-...) map to entities")
	ErrUnsupported = errors.New("mapping: CoT type has an affiliation or battle dimension this mapper does not know")
	ErrExpired     = errors.New("mapping: event is already stale")
)

const (
	// DefaultIntegrationName is provenance.integrationName when Config leaves it empty.
	DefaultIntegrationName = "tak-interop-gateway"
	// DataType is provenance.dataType for everything this mapper emits.
	DataType = "cot"
	// MaxExpiry keeps expiryTime a minute inside the server's rule that it be less than 30 days out.
	MaxExpiry = 30*24*time.Hour - time.Minute
)

// namespace is the UUIDv5 namespace for every entity ID this project derives (09d9f5aa-ca07-45b9-82f7-8b1498793c9d).
// It was generated once and must never change, or every entity would get a new ID.
var namespace = [16]byte{0x09, 0xd9, 0xf5, 0xaa, 0xca, 0x07, 0x45, 0xb9, 0x82, 0xf7, 0x8b, 0x14, 0x98, 0x79, 0x3c, 0x9d}

// DefaultTrackQuality is the "conservative default" Anduril's integration guide recommends when a source gives no
// quality signal, which CoT doesn't.
const DefaultTrackQuality = 2

// Config describes one CoT feed.
type Config struct {
	Source          string // names the feed; part of every entity ID, so the same CoT uid from two feeds is two entities
	IntegrationName string // provenance.integrationName; DefaultIntegrationName if empty
	Simulated       bool   // set indicators.simulated on every entity from this feed
	// TrackQuality is tracked.trackQualityWrapper for every track, 0 to 15. CoT carries no quality signal, so this is a
	// per-feed policy; DefaultTrackQuality is the documented conservative choice.
	TrackQuality int
	// Classification is dataClassification.default.level for every entity from this feed. It is per feed because a
	// classification has to come from whoever operates the feed, not from a gateway default; when it's left empty,
	// ClassificationUnclassified is used, the level the integration guide says to fall back to.
	Classification entity.ClassificationLevel
}

// Validate reports the first problem with the config, or nil. Map checks it on every call; a caller holding a config
// for a long-running feed can check it once at startup instead, and fail before any traffic arrives.
func (c Config) Validate() error {
	if c.Source == "" || len(c.Source) > 256 || strings.IndexFunc(c.Source, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: source must be 1-256 characters with no control characters", ErrConfig)
	}
	if len(c.IntegrationName) > 256 || strings.IndexFunc(c.IntegrationName, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: integration name must be at most 256 characters with no control characters", ErrConfig)
	}
	if c.TrackQuality < 0 || c.TrackQuality > 15 {
		return fmt.Errorf("%w: track quality must be 0 to 15, got %d", ErrConfig, c.TrackQuality)
	}
	return nil
}

// Map converts one parsed CoT event into a candidate entity. now is passed in, rather than read from the clock, so the
// result is deterministic. Map refuses, instead of guessing, when the event doesn't map cleanly: a non-atom type, an
// affiliation or dimension letter it doesn't know, or a report that has already gone stale.
func Map(ev cot.Event, cfg Config, now time.Time) (entity.Entity, error) {
	// 1: Check the config and the CoT type.
	if err := cfg.Validate(); err != nil {
		return entity.Entity{}, err
	}
	atoms := strings.Split(ev.Type, "-")
	if atoms[0] != "a" {
		return entity.Entity{}, fmt.Errorf("%w: type %q", ErrNotAnAtom, ev.Type)
	}
	if len(atoms) < 2 {
		return entity.Entity{}, fmt.Errorf("%w: type %q has no affiliation", ErrUnsupported, ev.Type)
	}
	disposition, exercise, ok := dispositionFor(atoms[1])
	if !ok {
		return entity.Entity{}, fmt.Errorf("%w: affiliation %q in %q", ErrUnsupported, atoms[1], ev.Type)
	}
	environment := entity.EnvironmentUnknown
	if len(atoms) >= 3 {
		if environment, ok = environmentFor(atoms[2]); !ok {
			return entity.Entity{}, fmt.Errorf("%w: battle dimension %q in %q", ErrUnsupported, atoms[2], ev.Type)
		}
	}

	// 2: The report must still be valid, and Lattice wants the expiry under 30 days out.
	expiry := time.Time(ev.Stale).UTC()
	if !expiry.After(now) {
		return entity.Entity{}, fmt.Errorf("%w: stale %s is not after %s", ErrExpired, expiry.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if limit := now.Add(MaxExpiry); expiry.After(limit) {
		expiry = limit
	}

	// 3: Build the entity.
	integration := cfg.IntegrationName
	if integration == "" {
		integration = DefaultIntegrationName
	}
	name := ev.UID // a non-empty display name is required; the uid stands in when there's no callsign
	if ev.Detail != nil && ev.Detail.Contact != nil && strings.TrimSpace(ev.Detail.Contact.Callsign) != "" {
		name = strings.TrimSpace(ev.Detail.Contact.Callsign)
	}
	e := entity.Entity{
		EntityID:   new(entityID(cfg.Source, ev.UID)),
		IsLive:     new(true),
		ExpiryTime: new(expiry),
		Aliases:    &entity.Aliases{Name: new(name)},
		MilView:    &entity.MilView{Disposition: new(disposition), Environment: new(environment)},
		Ontology:   &entity.Ontology{Template: new(entity.TemplateTrack), PlatformType: new(ev.Type)},
		Location:   location(ev),
		Provenance: &entity.Provenance{
			IntegrationName:   new(integration),
			DataType:          new(DataType),
			SourceID:          new(ev.UID),
			SourceUpdateTime:  new(time.Time(ev.Time).UTC()),
			SourceDescription: new(cfg.Source),
		},
		Tracked: &entity.Tracked{TrackQualityWrapper: new(cfg.TrackQuality)},
	}
	if ce := float64(ev.Point.CE); ev.Point.CE != cot.Unknown && ce > 0 {
		// A circle of radius ce. Probability is left unset on purpose: CoT producers don't agree on what confidence
		// level ce represents, and claiming one would be exactly the kind of silent misstatement this project avoids.
		e.LocationUncertainty = &entity.LocationUncertainty{PositionErrorEllipse: &entity.ErrorEllipse{
			SemiMajorAxisM: new(ce), SemiMinorAxisM: new(ce), OrientationD: new(0.0),
		}}
	}
	// The integration guide lists a SIDC as "always (derive from disposition + environment)", so when the full type
	// doesn't transliterate, fall back to a generic symbol built from the affiliation and battle dimension alone.
	sidc, ok := sidcFor(atoms)
	if !ok {
		sidc = genericSIDC(atoms)
	}
	e.Symbology = &entity.Symbology{MilStd2525C: &entity.MilStd2525C{SIDC: new(sidc)}}
	classification := cfg.Classification
	if classification == "" {
		classification = entity.ClassificationUnclassified
	}
	e.DataClassification = &entity.Classification{Default: &entity.ClassificationInformation{Level: new(classification)}}
	if cfg.Simulated || exercise {
		e.Indicators = &entity.Indicators{}
		if cfg.Simulated {
			e.Indicators.Simulated = new(true)
		}
		if exercise {
			e.Indicators.Exercise = new(true)
		}
	}
	return e, nil
}

// dispositionFor maps a CoT affiliation letter to a disposition. Joker and faker are MIL-STD-2525 exercise
// identities, an exercise track acting as a suspect or as a hostile, so they map to suspicious and hostile with the
// exercise flag set. "None specified" and "other" carry no affiliation, so they map to unknown.
func dispositionFor(letter string) (d entity.Disposition, exercise, ok bool) {
	switch strings.ToLower(letter) {
	case "f":
		return entity.DispositionFriendly, false, true
	case "a":
		return entity.DispositionAssumedFriendly, false, true
	case "n":
		return entity.DispositionNeutral, false, true
	case "u", "o", "x":
		return entity.DispositionUnknown, false, true
	case "p":
		return entity.DispositionPending, false, true
	case "s":
		return entity.DispositionSuspicious, false, true
	case "h":
		return entity.DispositionHostile, false, true
	case "j":
		return entity.DispositionSuspicious, true, true
	case "k":
		return entity.DispositionHostile, true, true
	}
	return "", false, false
}

// environmentFor maps a CoT battle dimension letter to an environment. Special operations forces (F) operate on land.
func environmentFor(letter string) (entity.Environment, bool) {
	switch strings.ToUpper(letter) {
	case "P":
		return entity.EnvironmentSpace, true
	case "A":
		return entity.EnvironmentAir, true
	case "G", "F":
		return entity.EnvironmentLand, true
	case "S":
		return entity.EnvironmentSurface, true
	case "U":
		return entity.EnvironmentSubSurface, true
	case "X", "Z":
		return entity.EnvironmentUnknown, true
	}
	return "", false
}

// sidcFor builds a 15-character MIL-STD-2525C SIDC from a CoT atom type, whose atoms follow the same hierarchy:
// coding scheme S (warfighting), the affiliation, the battle dimension, status P (present), six function characters
// taken from the remaining atoms, then five dashes for the unset modifier, country and order-of-battle fields. So
// a-f-G-U-C becomes SFGPUC---------. It returns false when the type doesn't transliterate cleanly: no battle dimension,
// more than six function atoms, or an atom that isn't one character. Map then falls back to genericSIDC.
func sidcFor(atoms []string) (string, bool) {
	if len(atoms) < 3 || len(atoms) > 3+6 {
		return "", false
	}
	for _, atom := range atoms[1:] {
		if len(atom) != 1 || !isAlnum(atom[0]) {
			return "", false
		}
	}
	affiliation := strings.ToUpper(atoms[1])
	if affiliation == "O" || affiliation == "X" {
		affiliation = "U" // 2525C has no "none" or "other" identity
	}
	var b strings.Builder
	b.WriteString("S" + affiliation + strings.ToUpper(atoms[2]) + "P")
	for _, atom := range atoms[3:] {
		b.WriteString(strings.ToUpper(atom))
	}
	for b.Len() < 15 {
		b.WriteByte('-')
	}
	return b.String(), true
}

// genericSIDC is the fallback symbol when the full type doesn't transliterate: the affiliation and battle dimension
// with the function left unspecified, so a-f becomes SFZP----------- and a-f-G-UC becomes SFGP-----------. Map only
// calls it after the affiliation and dimension letters have been validated.
func genericSIDC(atoms []string) string {
	affiliation := strings.ToUpper(atoms[1])
	if affiliation == "O" || affiliation == "X" {
		affiliation = "U"
	}
	dimension := "Z" // unknown
	if len(atoms) >= 3 {
		dimension = strings.ToUpper(atoms[2])
	}
	return "S" + affiliation + dimension + "P" + strings.Repeat("-", 11)
}

// isAlnum reports whether b is an ASCII letter or digit.
func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// location is the position, with altitude only when CoT knows it, and velocity when the event has a track.
func location(ev cot.Event) *entity.Location {
	loc := &entity.Location{Position: &entity.Position{
		LatitudeDegrees:  new(float64(ev.Point.Lat)),
		LongitudeDegrees: new(float64(ev.Point.Lon)),
	}}
	if ev.Point.HAE != cot.Unknown {
		loc.Position.AltitudeHaeMeters = new(float64(ev.Point.HAE))
	}
	if ev.Detail != nil && ev.Detail.Track != nil {
		// Course is degrees from true north, so east is speed times sin(course) and north is speed times cos(course).
		// Up is left unset: CoT's track says nothing about vertical speed, and zero would be a claim.
		rad := float64(ev.Detail.Track.Course) * math.Pi / 180
		speed := float64(ev.Detail.Track.Speed)
		loc.VelocityEnu = &entity.ENU{E: new(speed * math.Sin(rad)), N: new(speed * math.Cos(rad))}
	}
	return loc
}

// entityID derives a deterministic UUIDv5 from the feed and the CoT uid. The NUL separator keeps ("ab", "c") and
// ("a", "bc") apart; neither part can contain one, since both are checked for control characters.
func entityID(source, uid string) string {
	return UUIDv5(namespace, source+"\x00"+uid)
}

// UUIDv5 implements RFC 9562 section 5.5: the first 16 bytes of SHA-1(namespace || name), with the version and variant
// bits set. SHA-1 is what the RFC specifies for version 5; it is used here for stable naming, not for security.
func UUIDv5(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // the RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
