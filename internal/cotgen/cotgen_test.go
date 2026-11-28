package cotgen

import (
	"encoding/xml"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
)

// testConfig is a valid config with a fixed start time, so tests are deterministic.
func testConfig() Config {
	return Config{
		Units:    3,
		Origin:   LatLon{Lat: 40.3428, Lon: -105.6836},
		Start:    time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC),
		Interval: 5 * time.Second,
		Stale:    2 * time.Minute,
		Seed:     1,
	}
}

// render marshals ticks of a scenario into one string, for comparing whole runs.
func render(t *testing.T, cfg Config, ticks int) string {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for range ticks {
		for _, ev := range s.Next() {
			out, err := xml.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(out)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero units", func(c *Config) { c.Units = 0 }},
		{"too many units", func(c *Config) { c.Units = 101 }},
		{"latitude near the pole", func(c *Config) { c.Origin.Lat = 89 }},
		{"latitude NaN", func(c *Config) { c.Origin.Lat = math.NaN() }},
		{"longitude out of range", func(c *Config) { c.Origin.Lon = 181 }},
		{"no start", func(c *Config) { c.Start = time.Time{} }},
		{"zero interval", func(c *Config) { c.Interval = 0 }},
		{"negative stale", func(c *Config) { c.Stale = -time.Second }},
	}
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.mutate(&cfg)
			if cfg.Validate() == nil {
				t.Error("want an error, got nil")
			}
			if _, err := New(cfg); err == nil {
				t.Error("New accepted an invalid config")
			}
		})
	}
}

// TestDeterministic checks that the same config reproduces the same events and a different seed does not.
func TestDeterministic(t *testing.T) {
	cfg := testConfig()
	first, second := render(t, cfg, 5), render(t, cfg, 5)
	if first != second {
		t.Error("same config produced different output")
	}
	other := cfg
	other.Seed = 2
	if first == render(t, other, 5) {
		t.Error("different seeds produced identical output")
	}
}

// TestTickContents checks the event count, labels, timestamps, ranges and stable IDs across ticks.
func TestTickContents(t *testing.T) {
	cfg := testConfig()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstUIDs := map[string]bool{}
	for tick := range 4 {
		events := s.Next()
		if len(events) != cfg.Units+1 {
			t.Fatalf("tick %d: got %d events, want %d", tick, len(events), cfg.Units+1)
		}
		wantTime := cfg.Start.Add(time.Duration(tick) * cfg.Interval)
		uids := map[string]bool{}
		for _, ev := range events {
			if uids[ev.UID] {
				t.Errorf("tick %d: duplicate uid %q", tick, ev.UID)
			}
			uids[ev.UID] = true
			if got := time.Time(ev.Time); !got.Equal(wantTime) {
				t.Errorf("tick %d %s: time %v, want %v", tick, ev.UID, got, wantTime)
			}
			if got := time.Time(ev.Stale); !got.Equal(wantTime.Add(cfg.Stale)) {
				t.Errorf("tick %d %s: stale %v, want %v", tick, ev.UID, got, wantTime.Add(cfg.Stale))
			}
			if ev.Detail == nil || ev.Detail.Remarks == nil || ev.Detail.Remarks.Text != Label {
				t.Errorf("tick %d %s: missing the simulated-data label", tick, ev.UID)
			}
			lat, lon := float64(ev.Point.Lat), float64(ev.Point.Lon)
			if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
				t.Errorf("tick %d %s: position out of range: %v, %v", tick, ev.UID, lat, lon)
			}
		}
		if tick == 0 {
			firstUIDs = uids
		} else if len(uids) != len(firstUIDs) {
			t.Errorf("tick %d: uid set changed size", tick)
		} else {
			for uid := range uids {
				if !firstUIDs[uid] {
					t.Errorf("tick %d: new uid %q appeared", tick, uid)
				}
			}
		}
	}
}

// TestMovement checks that each team member moves its reported speed times the interval between ticks, within 1%,
// and that the last known position marker stays put.
func TestMovement(t *testing.T) {
	cfg := testConfig()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Next()
	after := s.Next()
	for i := range before {
		b, a := before[i], after[i]
		moved := haversine(float64(b.Point.Lat), float64(b.Point.Lon), float64(a.Point.Lat), float64(a.Point.Lon))
		if b.UID == "cotgen-lkp-1" {
			if moved > 0.01 {
				t.Errorf("last known position moved %.3f m", moved)
			}
			continue
		}
		want := float64(b.Detail.Track.Speed) * cfg.Interval.Seconds()
		if math.Abs(moved-want) > 0.01*want {
			t.Errorf("%s moved %.2f m, want about %.2f m", b.UID, moved, want)
		}
	}
}

// TestStaysInTheSearchArea checks that the walkers sweep their lanes back and forth rather than walking off for good:
// after a long run (about 13 km of walking each) every one is still within the 1 km lane of where it started, plus a
// little for the course's wander.
func TestStaysInTheSearchArea(t *testing.T) {
	s, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	first := s.Next()
	var last []cot.Event
	for range 2000 {
		last = s.Next()
	}
	for i := range first {
		d := haversine(float64(first[i].Point.Lat), float64(first[i].Point.Lon), float64(last[i].Point.Lat), float64(last[i].Point.Lon))
		if d > 1.2*laneLength {
			t.Errorf("%s is %.0f m from where it started, past its %.0f m lane", first[i].UID, d, laneLength)
		}
	}
}

// haversine is the great-circle distance in meters between two positions, an independent check on offset.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6_371_000.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := toRad(lat2-lat1), toRad(lon2-lon1)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadius * math.Asin(math.Sqrt(h))
}

func TestWrapAndNormalize(t *testing.T) {
	if got := wrapLon(181); math.Abs(got-(-179)) > 1e-9 {
		t.Errorf("wrapLon(181) = %v, want -179", got)
	}
	if got := wrapLon(-180); got != -180 {
		t.Errorf("wrapLon(-180) = %v, want -180", got)
	}
	if got := normalize(-5); got != 355 {
		t.Errorf("normalize(-5) = %v, want 355", got)
	}
	if got := normalize(725); got != 5 {
		t.Errorf("normalize(725) = %v, want 5", got)
	}
}
