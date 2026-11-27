// Package cotgen generates a synthetic, clearly labeled search-and-rescue scenario as Cursor-on-Target events, for
// testing the gateway locally. A search team walks north abreast while a fixed marker shows the missing person's last
// known position. The output depends only on the Config, so a given seed and start time always produce the same
// events.
package cotgen

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
)

// Label goes in every generated event's remarks so nobody mistakes the data for a real report.
const Label = "Simulated data from cot-gen (tak-interop-gateway). Not a real unit or person."

const (
	// metersPerDegreeLat is the approximate length of one degree of latitude.
	metersPerDegreeLat = 111_320.0
	// laneSpacing is the gap in meters between team members walking abreast.
	laneSpacing = 20.0
	// baseCourse is the direction the team sweeps, in degrees from true north.
	baseCourse = 0.0
	// lkpDistance is how far north of the origin, in meters, the last known position marker sits.
	lkpDistance = 300.0
)

// LatLon is a WGS84 position in degrees.
type LatLon struct {
	Lat, Lon float64
}

// Config describes a scenario.
type Config struct {
	Units    int           // search team members, walking abreast
	Origin   LatLon        // where the westernmost team member starts
	Start    time.Time     // timestamp of the first tick
	Interval time.Duration // simulated time between ticks
	Stale    time.Duration // how long each report stays valid after its timestamp
	Seed     uint64        // drives all randomness; the same seed gives the same scenario
}

// Validate reports the first problem with the config, or nil.
func (c Config) Validate() error {
	switch {
	case c.Units < 1 || c.Units > 100:
		return fmt.Errorf("units must be between 1 and 100, got %d", c.Units)
	case math.IsNaN(c.Origin.Lat) || c.Origin.Lat < -85 || c.Origin.Lat > 85:
		// The flat-earth step in offset breaks down near the poles.
		return fmt.Errorf("origin latitude must be between -85 and 85, got %v", c.Origin.Lat)
	case math.IsNaN(c.Origin.Lon) || c.Origin.Lon < -180 || c.Origin.Lon > 180:
		return fmt.Errorf("origin longitude must be between -180 and 180, got %v", c.Origin.Lon)
	case c.Start.IsZero():
		return fmt.Errorf("start time must be set")
	case c.Interval <= 0:
		return fmt.Errorf("interval must be positive, got %v", c.Interval)
	case c.Stale <= 0:
		return fmt.Errorf("stale must be positive, got %v", c.Stale)
	}
	return nil
}

// member is one search team member walking a straight search lane.
type member struct {
	uid, callsign string
	pos           LatLon
	course        float64 // degrees from true north
	speed         float64 // meters per second
}

// Scenario holds the moving state between ticks. Create one with New and call Next once per tick.
type Scenario struct {
	cfg  Config
	rng  *rand.Rand
	now  time.Time
	team []member
	lkp  LatLon
}

// New builds a scenario: the team lined up abreast at the origin, and the last known position marker to their north.
func New(cfg Config) (*Scenario, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Scenario{
		cfg: cfg,
		// PCG needs two seed words; derive the second from the first so one -seed flag is enough.
		rng: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)),
		now: cfg.Start.UTC(),
		lkp: offset(cfg.Origin, 0, lkpDistance),
	}
	for i := range cfg.Units {
		s.team = append(s.team, member{
			uid:      fmt.Sprintf("cotgen-sar-%d", i+1),
			callsign: fmt.Sprintf("SIM-SAR-%d", i+1),
			pos:      offset(cfg.Origin, 90, float64(i)*laneSpacing), // each member one lane further east
			course:   baseCourse,
			speed:    1.2 + 0.4*s.rng.Float64(), // a walking pace, 1.2 to 1.6 m/s
		})
	}
	return s, nil
}

// Next returns the events for the current tick, one per team member plus the last known position marker, and then
// moves the team forward by one interval.
func (s *Scenario) Next() []cot.Event {
	events := make([]cot.Event, 0, len(s.team)+1)
	for _, m := range s.team {
		events = append(events, s.memberEvent(m))
	}
	events = append(events, s.lkpEvent())
	s.advance()
	return events
}

// advance walks every member along its course for one interval, then picks a new course a few degrees either side
// of the sweep direction, so the tracks wander like real walkers but the team stays abreast.
func (s *Scenario) advance() {
	dt := s.cfg.Interval.Seconds()
	for i := range s.team {
		m := &s.team[i]
		m.pos = offset(m.pos, m.course, m.speed*dt)
		m.course = normalize(baseCourse + s.rng.Float64()*10 - 5)
	}
	s.now = s.now.Add(s.cfg.Interval)
}

// memberEvent is a team member's position report.
func (s *Scenario) memberEvent(m member) cot.Event {
	return cot.Event{
		Version: cot.Version,
		UID:     m.uid,
		Type:    "a-f-G-U-C", // a friendly ground unit, ATAK's default type for a user's own position
		Time:    cot.Timestamp(s.now),
		Start:   cot.Timestamp(s.now),
		Stale:   cot.Timestamp(s.now.Add(s.cfg.Stale)),
		How:     "m-g", // a machine-generated GPS position
		Point: cot.Point{
			Lat: round7(m.pos.Lat), Lon: round7(m.pos.Lon),
			HAE: cot.Unknown, CE: 10, LE: cot.Unknown,
		},
		Detail: &cot.Detail{
			Contact: &cot.Contact{Callsign: m.callsign},
			Group:   &cot.Group{Name: "Cyan", Role: "Team Member"},
			Track:   &cot.Track{Course: cot.Number(math.Round(m.course*10) / 10), Speed: cot.Number(math.Round(m.speed*100) / 100)},
			Remarks: &cot.Remarks{Text: Label},
		},
	}
}

// lkpEvent is the fixed marker for the missing person's last known position.
func (s *Scenario) lkpEvent() cot.Event {
	return cot.Event{
		Version: cot.Version,
		UID:     "cotgen-lkp-1",
		Type:    "a-u-G", // a ground thing whose affiliation is unknown
		Time:    cot.Timestamp(s.now),
		Start:   cot.Timestamp(s.now),
		Stale:   cot.Timestamp(s.now.Add(s.cfg.Stale)),
		How:     "h-e", // a human estimate
		Point: cot.Point{
			Lat: round7(s.lkp.Lat), Lon: round7(s.lkp.Lon),
			HAE: cot.Unknown, CE: 50, LE: cot.Unknown,
		},
		Detail: &cot.Detail{
			Contact: &cot.Contact{Callsign: "SIM-LKP"},
			Remarks: &cot.Remarks{Text: Label},
		},
	}
}

// offset returns the position distance meters from p along course degrees from true north. It treats the earth as
// flat, which is accurate to well under a meter over the few hundred meters a search team covers.
func offset(p LatLon, course, distance float64) LatLon {
	rad := course * math.Pi / 180
	dLat := distance * math.Cos(rad) / metersPerDegreeLat
	dLon := distance * math.Sin(rad) / (metersPerDegreeLat * math.Cos(p.Lat*math.Pi/180))
	return LatLon{Lat: p.Lat + dLat, Lon: wrapLon(p.Lon + dLon)}
}

// normalize maps a course into [0, 360).
func normalize(deg float64) float64 {
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	return deg
}

// wrapLon maps a longitude into [-180, 180), for a team that walks across the antimeridian.
func wrapLon(lon float64) float64 {
	return math.Mod(lon+540, 360) - 180
}

// round7 rounds degrees to 7 decimal places (about 1 cm) so the XML isn't full of floating-point noise.
func round7(v float64) cot.Number {
	return cot.Number(math.Round(v*1e7) / 1e7)
}
