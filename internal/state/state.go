// Package state decides, per entity, whether a freshly mapped report should be published at all. TAK networks
// rebroadcast, so the same report can arrive twice and an older one can arrive after a newer one. The Tracker drops
// both, so a track never moves backward. It also holds back a report that changes nothing until a refresh is due, so
// stationary units don't flood the platform, while still republishing in time to keep each entity from expiring.
//
// It also guards the feed's integrity. A report whose position implies a speed nothing in its environment can reach
// (a GPS glitch, a replayed track, a spoofed position) is dropped. And each entity is bound to the origin it is being
// published from, such as the client certificate of the connection it arrived on, so while that origin keeps
// reporting, another connection can't take the track over by sending the same uid.
//
// The Tracker's memory is bounded: it forgets entities whose last published expiry has passed, and refuses new
// entities beyond a fixed count, so a feed that invents endless uids can't grow it without limit.
package state

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
)

// Why Admit held a report back. Repeats, late arrivals and unchanged reports are normal traffic, not failures, so
// callers count them rather than log them as errors. An implausible jump or an origin conflict is worth an operator's
// attention.
var (
	ErrRepeated       = errors.New("state: the report repeats the last published update time")
	ErrOutOfOrder     = errors.New("state: the report is older than the last published update")
	ErrUnchanged      = errors.New("state: nothing changed since the last publish, and no refresh is due yet")
	ErrFull           = errors.New("state: already tracking the maximum number of entities")
	ErrImplausible    = errors.New("state: the report implies a speed nothing in its environment can reach")
	ErrOriginConflict = errors.New("state: the entity is being published from another origin")
	ErrConfig         = errors.New("state: invalid config")
)

const (
	// DefaultRefresh is how often an unchanged entity is republished. It is well inside the two-minute stale time
	// TAK clients commonly use, so a stationary unit stays live.
	DefaultRefresh = 30 * time.Second
	// DefaultMaxEntities bounds the Tracker's memory: about 150 bytes per entity.
	DefaultMaxEntities = 100_000
	// DefaultOriginHold is how long an entity stays bound to its origin after that origin last published it. It is
	// three refreshes, so a live but stationary unit, republished every refresh, keeps its binding, while a device
	// that reconnects from a new address gets its track back after a short silence. An attacker can't squat a uid:
	// the binding lasts only while they keep sending.
	DefaultOriginHold = 3 * DefaultRefresh
	// minInterval is the shortest time between two reports the speed check divides by, so two reports stamped a
	// millisecond apart, with a meter of GPS jitter, don't look like a jet.
	minInterval = time.Second
	// earthRadius is the mean radius of the WGS84 ellipsoid, in meters: plenty for a plausibility bound.
	earthRadius = 6_371_008.8
	// sweepEvery limits how often a full Tracker scans for expired entities, so a flood of new uids at the limit
	// costs one scan a second rather than one per message.
	sweepEvery = time.Second
)

// DefaultMaxSpeed is the fastest, in meters per second, anything in each environment is taken to move. Each limit is
// a generous bound on the fastest things there, so only a physically impossible jump trips it: about Mach 3.2 in the
// air, 500 km/h on land, 97 knots on the surface, 49 knots below it. Space has no limit, and an unknown environment
// gets the air limit.
var DefaultMaxSpeed = map[entity.Environment]float64{
	entity.EnvironmentAir:        1100,
	entity.EnvironmentLand:       140,
	entity.EnvironmentSurface:    50,
	entity.EnvironmentSubSurface: 25,
	entity.EnvironmentUnknown:    1100,
}

// Config tunes a Tracker. The zero value means the defaults.
type Config struct {
	// Refresh is the longest an unchanged entity goes without a republish. Zero means DefaultRefresh. To publish
	// every report, set it to a negative value.
	Refresh time.Duration
	// MaxEntities is how many entities the Tracker remembers at once. Zero means DefaultMaxEntities.
	MaxEntities int
	// OriginHold is how long an entity stays bound to its origin after its last publish. Zero means
	// DefaultOriginHold; a negative value turns origin binding off.
	OriginHold time.Duration
	// MaxSpeed sets the plausibility limit per environment, in meters per second; nil means DefaultMaxSpeed. An
	// environment missing from the map, or with a limit of zero or less, has no limit.
	MaxSpeed map[entity.Environment]float64
}

// Fix is a position report reduced to what the speed check needs.
type Fix struct {
	Lat, Lon float64 // WGS84 degrees
	CE       float64 // the radius, in meters, the true position is likely within; 0 if unknown
}

// record is what the Tracker remembers about one entity's last publish.
type record struct {
	updated   time.Time // provenance.sourceUpdateTime
	published time.Time // when the gateway published it, by the gateway's clock
	expiry    time.Time // its expiryTime: the server forgets the entity then, so the Tracker can too
	content   [32]byte  // a digest of everything but the time fields, for change detection
	origin    string    // where it was published from
	fix       Fix       // its position, when hasFix
	hasFix    bool
}

// Tracker remembers the last published state of each entity. It is not safe for concurrent use; one Tracker serves
// one ordered stream of reports.
type Tracker struct {
	refresh   time.Duration
	max       int
	hold      time.Duration
	maxSpeed  map[entity.Environment]float64
	entities  map[string]record
	lastSweep time.Time
}

// New returns an empty Tracker.
func New(cfg Config) (*Tracker, error) {
	if cfg.MaxEntities < 0 {
		return nil, fmt.Errorf("%w: max entities must not be negative, got %d", ErrConfig, cfg.MaxEntities)
	}
	t := &Tracker{refresh: cfg.Refresh, max: cfg.MaxEntities, hold: cfg.OriginHold, maxSpeed: cfg.MaxSpeed,
		entities: map[string]record{}}
	if t.refresh == 0 {
		t.refresh = DefaultRefresh
	}
	if t.max == 0 {
		t.max = DefaultMaxEntities
	}
	if t.hold == 0 {
		t.hold = DefaultOriginHold
	}
	if t.maxSpeed == nil {
		t.maxSpeed = DefaultMaxSpeed
	}
	return t, nil
}

// Len returns how many entities the Tracker currently remembers.
func (t *Tracker) Len() int {
	return len(t.entities)
}

// Admit decides whether e, arriving from origin, should be published at now. It returns nil to publish, or an error
// wrapping one of the sentinels above to hold the report back. Admit changes nothing; call Record once the entity has
// actually been published, so a report that then fails preflight or publishing doesn't count as sent. An entity
// without an ID or a source update time is admitted, so preflight can reject it with the reason.
func (t *Tracker) Admit(e entity.Entity, origin string, now time.Time) error {
	// 1: An entity seen for the first time is admitted if there's room.
	id, updated, ok := key(e)
	if !ok {
		return nil
	}
	r, seen := t.entities[id]
	if !seen {
		if len(t.entities) >= t.max {
			t.sweep(now)
		}
		if len(t.entities) >= t.max {
			return ErrFull
		}
		return nil
	}

	// 2: While its origin is still publishing it, nobody else may.
	if t.hold > 0 && origin != r.origin && now.Sub(r.published) < t.hold {
		return fmt.Errorf("%w: it is bound to %q until %s passes without a report from there", ErrOriginConflict, r.origin, t.hold)
	}

	// 3: A report must be newer than the last one published, or it is a rebroadcast or a late arrival.
	switch {
	case updated.Equal(r.updated):
		return ErrRepeated
	case updated.Before(r.updated):
		return ErrOutOfOrder
	}

	// 4: The move since the last publish must be physically possible, judged by the source's own clock.
	if f, ok := fixOf(e); ok && r.hasFix {
		dt := updated.Sub(r.updated)
		if limit := t.maxSpeed[environmentOf(e)]; limit > 0 {
			if v := ImpliedSpeed(r.fix, f, dt); v > limit {
				return fmt.Errorf("%w: moving %.0f m in %s means at least %.0f m/s, over the %.0f m/s limit for %s",
					ErrImplausible, distance(r.fix, f), dt, v, limit, environmentOf(e))
			}
		}
	}

	// 5: A newer report that changes nothing waits for the refresh, unless the published copy would expire first.
	if d, ok := digest(e); ok && t.refresh > 0 && d == r.content && now.Sub(r.published) < t.refresh && r.expiry.Sub(now) > t.refresh {
		return ErrUnchanged
	}
	return nil
}

// Record notes that e was published at now, from origin.
func (t *Tracker) Record(e entity.Entity, origin string, now time.Time) {
	id, updated, ok := key(e)
	if !ok {
		return
	}
	r := record{updated: updated, published: now, origin: origin}
	r.content, _ = digest(e) // without a digest, the zero value matches no later report, so the next one publishes
	r.fix, r.hasFix = fixOf(e)
	if e.ExpiryTime != nil {
		r.expiry = *e.ExpiryTime
	}
	t.entities[id] = r
}

// ImpliedSpeed is the slowest speed, in meters per second, that explains moving from a to b in dt: the great-circle
// distance, less both reports' position uncertainty, divided by dt, with dt taken as at least one second.
func ImpliedSpeed(a, b Fix, dt time.Duration) float64 {
	d := max(0, distance(a, b)-a.CE-b.CE)
	return d / max(dt, minInterval).Seconds()
}

// distance is the great-circle distance between two fixes in meters, by the haversine formula.
func distance(a, b Fix) float64 {
	rad := math.Pi / 180
	dLat, dLon := (b.Lat-a.Lat)*rad, (b.Lon-a.Lon)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadius * math.Asin(math.Sqrt(min(1, h)))
}

// fixOf returns the entity's position and its uncertainty radius, or false when it has no position.
func fixOf(e entity.Entity) (Fix, bool) {
	if e.Location == nil || e.Location.Position == nil || e.Location.Position.LatitudeDegrees == nil || e.Location.Position.LongitudeDegrees == nil {
		return Fix{}, false
	}
	f := Fix{Lat: *e.Location.Position.LatitudeDegrees, Lon: *e.Location.Position.LongitudeDegrees}
	if u := e.LocationUncertainty; u != nil && u.PositionErrorEllipse != nil && u.PositionErrorEllipse.SemiMajorAxisM != nil {
		f.CE = *u.PositionErrorEllipse.SemiMajorAxisM
	}
	return f, true
}

// environmentOf returns milView.environment, or ENVIRONMENT_UNKNOWN when it isn't set.
func environmentOf(e entity.Entity) entity.Environment {
	if e.MilView == nil || e.MilView.Environment == nil {
		return entity.EnvironmentUnknown
	}
	return *e.MilView.Environment
}

// sweep forgets every entity whose last published expiry has passed, at most once per sweepEvery.
func (t *Tracker) sweep(now time.Time) {
	if now.Sub(t.lastSweep) < sweepEvery {
		return
	}
	t.lastSweep = now
	for id, r := range t.entities {
		if !r.expiry.After(now) {
			delete(t.entities, id)
		}
	}
}

// key returns the entity's ID and source update time, the two fields the Tracker orders reports by.
func key(e entity.Entity) (id string, updated time.Time, ok bool) {
	if e.EntityID == nil || e.Provenance == nil || e.Provenance.SourceUpdateTime == nil {
		return "", time.Time{}, false
	}
	return *e.EntityID, *e.Provenance.SourceUpdateTime, true
}

// digest summarizes everything about e except the two fields that change on every report, expiryTime and
// provenance.sourceUpdateTime, so two reports with equal digests differ only in time. It returns false if e can't be
// summarized, and then Admit treats the report as changed: publishing is the safe side of that mistake.
func digest(e entity.Entity) ([32]byte, bool) {
	// 1: Clear the time fields on a copy. The copy is shallow, so provenance is copied before its field is cleared.
	e.ExpiryTime = nil
	if e.Provenance != nil {
		p := *e.Provenance
		p.SourceUpdateTime = nil
		e.Provenance = &p
	}

	// 2: Hash the JSON form, which is canonical for this struct: fixed field order, no maps. Only a NaN or an
	// infinity can make an entity fail to marshal, and the mapper never produces one.
	b, err := json.Marshal(e)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(b), true
}
