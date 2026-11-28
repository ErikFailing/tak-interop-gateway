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
		to   entity.Entity // from (40, -105) at t0, with no uncertainty
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
			tr.Record(moving(t0, 40, -105, 0, entity.EnvironmentLand), "", t0)
			admit(t, tr, tt.to, t0.Add(time.Second), tt.want)
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

// TestImpliedSpeed pins the arithmetic.
func TestImpliedSpeed(t *testing.T) {
	a, b := state.Fix{Lat: 0, Lon: 0}, state.Fix{Lat: 0, Lon: 1} // one degree of longitude on the equator: about 111.2 km
	if v := state.ImpliedSpeed(a, b, 100*time.Second); math.Abs(v-1111.95) > 1 {
		t.Errorf("ImpliedSpeed = %v m/s, want about 1111.95", v)
	}
	a.CE, b.CE = 60_000, 60_000
	if v := state.ImpliedSpeed(a, b, time.Second); v != 0 {
		t.Errorf("with the uncertainty covering the distance, ImpliedSpeed = %v, want 0", v)
	}
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
}
