package state_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
)

// t0 is when every test starts.
var t0 = time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

// report builds an entity with the fields the Tracker reads: ID, source update time, expiry, and a latitude so
// content can change.
func report(id string, updated time.Time, lat float64, expiry time.Time) entity.Entity {
	return entity.Entity{
		EntityID:   new(id),
		ExpiryTime: new(expiry),
		Location:   &entity.Location{Position: &entity.Position{LatitudeDegrees: new(lat), LongitudeDegrees: new(-105.0)}},
		Provenance: &entity.Provenance{IntegrationName: new("test"), SourceUpdateTime: new(updated)},
	}
}

// newTracker returns a Tracker with cfg, failing the test on error.
func newTracker(t *testing.T, cfg state.Config) *state.Tracker {
	t.Helper()
	tr, err := state.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// admit checks one Admit result.
func admit(t *testing.T, tr *state.Tracker, e entity.Entity, now time.Time, want error) {
	t.Helper()
	if got := tr.Admit(e, "", now); !errors.Is(got, want) || (want == nil && got != nil) {
		t.Errorf("Admit = %v, want %v", got, want)
	}
}

// TestOrdering checks that only a report newer than the last published one gets through.
func TestOrdering(t *testing.T) {
	tr := newTracker(t, state.Config{})
	first := report("a", t0, 40, t0.Add(2*time.Minute))
	admit(t, tr, first, t0, nil)
	tr.Record(first, "", t0)
	admit(t, tr, first, t0.Add(time.Second), state.ErrRepeated)
	admit(t, tr, report("a", t0.Add(-time.Second), 40.0001, t0.Add(2*time.Minute)), t0.Add(time.Second), state.ErrOutOfOrder)
	admit(t, tr, report("a", t0.Add(time.Second), 40.0001, t0.Add(2*time.Minute)), t0.Add(time.Second), nil)
	admit(t, tr, report("b", t0, 40, t0.Add(2*time.Minute)), t0, nil) // another entity has its own history
}

// TestUnchanged checks that a newer report with the same content waits for the refresh, unless the published copy
// would expire first.
func TestUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		expiry time.Time     // of the published copy
		lat    float64       // of the new report; 40 is unchanged
		after  time.Duration // from the publish to the new report
		want   error
	}{
		{"unchanged, refresh not due", t0.Add(2 * time.Minute), 40, 5 * time.Second, state.ErrUnchanged},
		{"unchanged, refresh due", t0.Add(2 * time.Minute), 40, state.DefaultRefresh, nil},
		{"unchanged, but the copy expires within the refresh", t0.Add(40 * time.Second), 40, 15 * time.Second, nil},
		{"changed", t0.Add(2 * time.Minute), 40.0001, 5 * time.Second, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker(t, state.Config{})
			tr.Record(report("a", t0, 40, tt.expiry), "", t0)
			now := t0.Add(tt.after)
			// The new report has a new time and a new expiry; only the content decides "unchanged".
			admit(t, tr, report("a", now, tt.lat, now.Add(2*time.Minute)), now, tt.want)
		})
	}
}

// TestRefreshDisabled checks that a negative Refresh publishes every newer report, changed or not.
func TestRefreshDisabled(t *testing.T) {
	tr := newTracker(t, state.Config{Refresh: -1})
	tr.Record(report("a", t0, 40, t0.Add(2*time.Minute)), "", t0)
	admit(t, tr, report("a", t0.Add(time.Second), 40, t0.Add(2*time.Minute)), t0.Add(time.Second), nil)
	admit(t, tr, report("a", t0, 40, t0.Add(2*time.Minute)), t0.Add(time.Second), state.ErrRepeated) // ordering still holds
}

// TestAdmitChangesNothing checks that only Record moves state, so a report that fails later in the pipeline isn't
// counted as sent.
func TestAdmitChangesNothing(t *testing.T) {
	tr := newTracker(t, state.Config{})
	e := report("a", t0, 40, t0.Add(2*time.Minute))
	admit(t, tr, e, t0, nil)
	admit(t, tr, e, t0, nil)
	if tr.Len() != 0 {
		t.Errorf("Len = %d after Admit alone, want 0", tr.Len())
	}
}

// TestFull checks the entity limit, and that expired entities are forgotten to make room.
func TestFull(t *testing.T) {
	tr := newTracker(t, state.Config{MaxEntities: 2})
	tr.Record(report("a", t0, 40, t0.Add(time.Minute)), "", t0)
	tr.Record(report("b", t0, 40, t0.Add(2*time.Minute)), "", t0)
	admit(t, tr, report("c", t0, 40, t0.Add(2*time.Minute)), t0, state.ErrFull)
	admit(t, tr, report("a", t0.Add(time.Second), 40.0001, t0.Add(2*time.Minute)), t0.Add(time.Second), nil) // known entities still pass

	// After a's expiry passes, a sweep frees its slot.
	later := t0.Add(90 * time.Second)
	admit(t, tr, report("c", later, 40, later.Add(2*time.Minute)), later, nil)
	if tr.Len() != 1 {
		t.Errorf("Len = %d after the sweep, want 1 (only b is still live)", tr.Len())
	}

	// A second sweep within the same second is skipped, so a flood of new uids costs one scan a second.
	tr.Record(report("c", later, 40, later.Add(2*time.Minute)), "", later)
	tr.Record(report("d", later, 40, later.Add(2*time.Minute)), "", later) // over the limit: Record always records
	admit(t, tr, report("e", later, 40, later.Add(2*time.Minute)), later.Add(500*time.Millisecond), state.ErrFull)
}

// TestUnkeyedReportsPass checks that a report without an ID or a source update time is admitted and never recorded,
// leaving preflight to reject it with the reason.
func TestUnkeyedReportsPass(t *testing.T) {
	tr := newTracker(t, state.Config{})
	e := report("a", t0, 40, t0.Add(time.Minute))
	e.EntityID = nil
	admit(t, tr, e, t0, nil)
	tr.Record(e, "", t0)
	e = report("a", t0, 40, t0.Add(time.Minute))
	e.Provenance = nil
	admit(t, tr, e, t0, nil)
	tr.Record(e, "", t0)
	if tr.Len() != 0 {
		t.Errorf("Len = %d, want 0", tr.Len())
	}
}

// TestUndigestibleCountsAsChanged checks that an entity that can't be summarized is published rather than
// suppressed: suppressing it would be the riskier mistake.
func TestUndigestibleCountsAsChanged(t *testing.T) {
	tr := newTracker(t, state.Config{})
	tr.Record(report("a", t0, math.NaN(), t0.Add(2*time.Minute)), "", t0)
	admit(t, tr, report("a", t0.Add(time.Second), math.NaN(), t0.Add(2*time.Minute)), t0.Add(time.Second), nil)
}

// moving is a report in a given environment at a given position and uncertainty.
func moving(updated time.Time, lat, lon, ce float64, env entity.Environment) entity.Entity {
	e := report("a", updated, lat, updated.Add(2*time.Minute))
	e.Location.Position.LongitudeDegrees = new(lon)
	e.MilView = &entity.MilView{Environment: new(env)}
	if ce > 0 {
		e.LocationUncertainty = &entity.LocationUncertainty{PositionErrorEllipse: &entity.ErrorEllipse{SemiMajorAxisM: new(ce)}}
	}
	return e
}

// TestImplausibleJump checks the speed check: by environment, against the source's own clock, with the position
// uncertainty allowed for. One thousandth of a degree of latitude is about 111 m.
func TestImplausibleJump(t *testing.T) {
	tests := []struct {
		name string
		to   entity.Entity // from (40, -105) at t0 in the same environment, with no uncertainty
		want error
	}{
		{"walking pace", moving(t0.Add(10*time.Second), 40.0001, -105, 0, entity.EnvironmentLand), nil},
		{"a ground unit teleporting 11 km in a second", moving(t0.Add(time.Second), 40.1, -105, 0, entity.EnvironmentLand), state.ErrImplausible},
		{"the same distance by air in 20 s is fine", moving(t0.Add(20*time.Second), 40.1, -105, 0, entity.EnvironmentAir), nil},
		{"the same distance after an hour off", moving(t0.Add(time.Hour), 40.1, -105, 0, entity.EnvironmentLand), nil},
		{"a boat at 222 m/s", moving(t0.Add(time.Second), 40.002, -105, 0, entity.EnvironmentSurface), state.ErrImplausible},
		{"uncertainty covers the jump", moving(t0.Add(time.Second), 40.002, -105, 200, entity.EnvironmentSurface), nil},
		{"space has no limit", moving(t0.Add(time.Second), 41, -105, 0, entity.EnvironmentSpace), nil},
		{"unknown environment gets the air limit", moving(t0.Add(time.Second), 41, -105, 0, entity.EnvironmentUnknown), state.ErrImplausible},
		{"a millisecond apart counts as a second", moving(t0.Add(time.Millisecond), 40.0001, -105, 0, entity.EnvironmentLand), nil},
		{"the far side of the world", moving(t0.Add(time.Second), 40, 75, 0, entity.EnvironmentLand), state.ErrImplausible},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker(t, state.Config{})
			tr.Record(moving(t0, 40, -105, 0, *tt.to.MilView.Environment), "", t0)
			at := *tt.to.Provenance.SourceUpdateTime // checked when it arrives, so it is never from the future
			admit(t, tr, tt.to, at, tt.want)
		})
	}

	// Seventeen meters across the antimeridian is a short step, not a trip around the world.
	tr := newTracker(t, state.Config{})
	tr.Record(moving(t0, 40, 179.9999, 0, entity.EnvironmentLand), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 40, -179.9999, 0, entity.EnvironmentLand), t0.Add(time.Second), nil)

	// A custom limit, and no limit at all.
	tr = newTracker(t, state.Config{MaxSpeed: map[entity.Environment]float64{entity.EnvironmentLand: 5}})
	tr.Record(moving(t0, 40, -105, 0, entity.EnvironmentLand), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 40.0001, -105, 0, entity.EnvironmentLand), t0.Add(time.Second), state.ErrImplausible)
	tr = newTracker(t, state.Config{MaxSpeed: map[entity.Environment]float64{}})
	tr.Record(moving(t0, 40, -105, 0, entity.EnvironmentLand), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 41, -105, 0, entity.EnvironmentLand), t0.Add(time.Second), nil)
}

// TestImpliedSpeed pins the arithmetic against distances worked out by hand on a sphere of the WGS84 mean radius:
// a degree along the equator, a degree of longitude at 60 degrees north (half as far), a degree of latitude from the
// pole, and two "different" points at the pole, which are the same point.
func TestImpliedSpeed(t *testing.T) {
	tests := []struct {
		name string
		a, b state.Fix
		dt   time.Duration
		want float64 // m/s
	}{
		{"along the equator", state.Fix{Lat: 0, Lon: 0}, state.Fix{Lat: 0, Lon: 1}, 100 * time.Second, 1111.95},
		{"at 60 degrees north", state.Fix{Lat: 60, Lon: 0}, state.Fix{Lat: 60, Lon: 1}, 100 * time.Second, 555.97},
		{"from the pole", state.Fix{Lat: 90, Lon: 0}, state.Fix{Lat: 89, Lon: 0}, 100 * time.Second, 1111.95},
		{"around the pole", state.Fix{Lat: 90, Lon: 0}, state.Fix{Lat: 90, Lon: 180}, time.Second, 0},
		{"uncertainty excuses part of it", state.Fix{Lat: 0, Lon: 0, CE: 500}, state.Fix{Lat: 0, Lon: 1, CE: 500}, 100 * time.Second, 1101.95},
		// Each report's uncertainty excuses at most a kilometer, or a sender could claim enough to move anywhere.
		{"uncertainty is capped", state.Fix{Lat: 0, Lon: 0, CE: 60_000}, state.Fix{Lat: 0, Lon: 1, CE: 60_000}, 100 * time.Second, 1091.95},
		{"uncertainty covers a short step", state.Fix{Lat: 0, Lon: 0, CE: 600}, state.Fix{Lat: 0, Lon: 0.01, CE: 600}, time.Second, 0},
	}
	for _, tt := range tests {
		if v := state.ImpliedSpeed(tt.a, tt.b, tt.dt); math.Abs(v-tt.want) > 0.05 {
			t.Errorf("%s: ImpliedSpeed = %.2f m/s, want %.2f", tt.name, v, tt.want)
		}
	}
}

// TestFutureTime checks that a report stamped further ahead than clocks drift is refused, even for a known entity, and
// that it changes nothing: the honest report after it still gets through.
func TestFutureTime(t *testing.T) {
	tr := newTracker(t, state.Config{})
	admit(t, tr, report("a", t0.Add(4*time.Minute), 40, t0.Add(10*time.Minute)), t0, nil) // within the skew
	admit(t, tr, report("a", t0.Add(6*time.Minute), 40, t0.Add(10*time.Minute)), t0, state.ErrFutureTime)
	tr.Record(report("a", t0, 40, t0.Add(2*time.Minute)), "", t0)
	admit(t, tr, report("a", t0.Add(time.Hour), 40.1, t0.Add(2*time.Hour)), t0.Add(time.Second), state.ErrFutureTime)
	admit(t, tr, report("a", t0.Add(time.Second), 40.0001, t0.Add(2*time.Minute)), t0.Add(time.Second), nil)

	// A custom skew, and none.
	tr = newTracker(t, state.Config{MaxSkew: time.Second})
	admit(t, tr, report("a", t0.Add(2*time.Second), 40, t0.Add(time.Minute)), t0, state.ErrFutureTime)
	tr = newTracker(t, state.Config{MaxSkew: -1})
	admit(t, tr, report("a", t0.Add(time.Hour), 40, t0.Add(2*time.Hour)), t0, nil)
}

// TestExpiredRecordForgotten checks that once an entity's published copy has expired, its history no longer decides
// anything: the server has forgotten the entity too.
func TestExpiredRecordForgotten(t *testing.T) {
	tr := newTracker(t, state.Config{})
	tr.Record(report("a", t0, 40, t0.Add(time.Minute)), "unit-a", t0)
	later := t0.Add(70 * time.Second)
	// Older than the last publish, and from another origin within the hold, yet admitted: the record expired.
	if err := tr.Admit(report("a", t0.Add(-time.Second), 41, later.Add(time.Minute)), "unit-b", later); err != nil {
		t.Errorf("Admit after the expiry = %v, want nil", err)
	}
	if tr.Len() != 0 {
		t.Errorf("Len = %d, want the expired record gone", tr.Len())
	}
}

// TestRetypeCantEscapeTheLimit checks that the speed check uses the stricter of the old and new environment's limits:
// a ground unit retyped as a satellite, which has no limit, still can't jump.
func TestRetypeCantEscapeTheLimit(t *testing.T) {
	tr := newTracker(t, state.Config{})
	tr.Record(moving(t0, 40, -105, 0, entity.EnvironmentLand), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 41, -105, 0, entity.EnvironmentSpace), t0.Add(time.Second), state.ErrImplausible)

	// And the other way: a satellite retyped as a ground unit is held to the ground limit.
	tr = newTracker(t, state.Config{})
	tr.Record(moving(t0, 40, -105, 0, entity.EnvironmentSpace), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 41, -105, 0, entity.EnvironmentLand), t0.Add(time.Second), state.ErrImplausible)

	// Two space reports stay unlimited.
	tr = newTracker(t, state.Config{})
	tr.Record(moving(t0, 40, -105, 0, entity.EnvironmentSpace), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 41, -105, 0, entity.EnvironmentSpace), t0.Add(time.Second), nil)
}

// TestHugeUncertaintyCantExcuseAJump checks the uncertainty cap end to end: claiming to be uncertain by 20,000 km
// doesn't let a ground unit cross a continent in a second.
func TestHugeUncertaintyCantExcuseAJump(t *testing.T) {
	tr := newTracker(t, state.Config{})
	tr.Record(moving(t0, 40, -105, 20_000_000, entity.EnvironmentLand), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 40, -75, 20_000_000, entity.EnvironmentLand), t0.Add(time.Second), state.ErrImplausible)
}

// TestOriginBinding checks that while an entity's origin keeps publishing it, another origin can't, and that the
// binding lapses after the hold.
func TestOriginBinding(t *testing.T) {
	tr := newTracker(t, state.Config{})
	tr.Record(report("a", t0, 40, t0.Add(2*time.Minute)), "unit-a", t0)
	next := func(after time.Duration) entity.Entity {
		return report("a", t0.Add(after), 40.0001, t0.Add(after+2*time.Minute))
	}
	check := func(e entity.Entity, origin string, now time.Time, want error) {
		t.Helper()
		if got := tr.Admit(e, origin, now); !errors.Is(got, want) || (want == nil && got != nil) {
			t.Errorf("Admit from %q = %v, want %v", origin, got, want)
		}
	}
	check(next(5*time.Second), "unit-a", t0.Add(5*time.Second), nil)
	check(next(5*time.Second), "intruder", t0.Add(5*time.Second), state.ErrOriginConflict)
	check(next(80*time.Second), "intruder", t0.Add(80*time.Second), state.ErrOriginConflict)
	check(next(90*time.Second), "new-address", t0.Add(90*time.Second), nil) // the owner went quiet for the hold

	// Off, any origin may publish.
	tr = newTracker(t, state.Config{OriginHold: -1})
	tr.Record(report("a", t0, 40, t0.Add(2*time.Minute)), "unit-a", t0)
	check(next(5*time.Second), "intruder", t0.Add(5*time.Second), nil)
}

// TestConfig checks the config's validation and defaults.
func TestConfig(t *testing.T) {
	if _, err := state.New(state.Config{MaxEntities: -1}); !errors.Is(err, state.ErrConfig) {
		t.Errorf("negative MaxEntities: err %v, want ErrConfig", err)
	}
	if _, err := state.New(state.Config{Refresh: time.Minute, OriginHold: 30 * time.Second}); !errors.Is(err, state.ErrConfig) {
		t.Errorf("a hold shorter than the refresh: err %v, want ErrConfig", err)
	}
	for _, cfg := range []state.Config{{Refresh: -1}, {Refresh: -1, OriginHold: time.Second}, {OriginHold: -1}} {
		if _, err := state.New(cfg); err != nil {
			t.Errorf("%+v: %v", cfg, err)
		}
	}

	// The Tracker keeps its own copy of the speed limits: changing the caller's map afterwards changes nothing.
	limits := map[entity.Environment]float64{entity.EnvironmentLand: 5}
	tr := newTracker(t, state.Config{MaxSpeed: limits})
	limits[entity.EnvironmentLand] = 1e9
	tr.Record(moving(t0, 40, -105, 0, entity.EnvironmentLand), "", t0)
	admit(t, tr, moving(t0.Add(time.Second), 40.001, -105, 0, entity.EnvironmentLand), t0.Add(time.Second), state.ErrImplausible)
}
