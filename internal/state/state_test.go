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
	if got := tr.Admit(e, now); !errors.Is(got, want) || (want == nil && got != nil) {
		t.Errorf("Admit = %v, want %v", got, want)
	}
}

// TestOrdering checks that only a report newer than the last published one gets through.
func TestOrdering(t *testing.T) {
	tr := newTracker(t, state.Config{})
	first := report("a", t0, 40, t0.Add(2*time.Minute))
	admit(t, tr, first, t0, nil)
	tr.Record(first, t0)
	admit(t, tr, first, t0.Add(time.Second), state.ErrRepeated)
	admit(t, tr, report("a", t0.Add(-time.Second), 40.1, t0.Add(2*time.Minute)), t0.Add(time.Second), state.ErrOutOfOrder)
	admit(t, tr, report("a", t0.Add(time.Second), 40.1, t0.Add(2*time.Minute)), t0.Add(time.Second), nil)
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
			tr.Record(report("a", t0, 40, tt.expiry), t0)
			now := t0.Add(tt.after)
			// The new report has a new time and a new expiry; only the content decides "unchanged".
			admit(t, tr, report("a", now, tt.lat, now.Add(2*time.Minute)), now, tt.want)
		})
	}
}

// TestRefreshDisabled checks that a negative Refresh publishes every newer report, changed or not.
func TestRefreshDisabled(t *testing.T) {
	tr := newTracker(t, state.Config{Refresh: -1})
	tr.Record(report("a", t0, 40, t0.Add(2*time.Minute)), t0)
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
	tr.Record(report("a", t0, 40, t0.Add(time.Minute)), t0)
	tr.Record(report("b", t0, 40, t0.Add(2*time.Minute)), t0)
	admit(t, tr, report("c", t0, 40, t0.Add(2*time.Minute)), t0, state.ErrFull)
	admit(t, tr, report("a", t0.Add(time.Second), 41, t0.Add(2*time.Minute)), t0.Add(time.Second), nil) // known entities still pass

	// After a's expiry passes, a sweep frees its slot.
	later := t0.Add(90 * time.Second)
	admit(t, tr, report("c", later, 40, later.Add(2*time.Minute)), later, nil)
	if tr.Len() != 1 {
		t.Errorf("Len = %d after the sweep, want 1 (only b is still live)", tr.Len())
	}

	// A second sweep within the same second is skipped, so a flood of new uids costs one scan a second.
	tr.Record(report("c", later, 40, later.Add(2*time.Minute)), later)
	tr.Record(report("d", later, 40, later.Add(2*time.Minute)), later) // over the limit: Record always records
	admit(t, tr, report("e", later, 40, later.Add(2*time.Minute)), later.Add(500*time.Millisecond), state.ErrFull)
}

// TestUnkeyedReportsPass checks that a report without an ID or a source update time is admitted and never recorded,
// leaving preflight to reject it with the reason.
func TestUnkeyedReportsPass(t *testing.T) {
	tr := newTracker(t, state.Config{})
	e := report("a", t0, 40, t0.Add(time.Minute))
	e.EntityID = nil
	admit(t, tr, e, t0, nil)
	tr.Record(e, t0)
	e = report("a", t0, 40, t0.Add(time.Minute))
	e.Provenance = nil
	admit(t, tr, e, t0, nil)
	tr.Record(e, t0)
	if tr.Len() != 0 {
		t.Errorf("Len = %d, want 0", tr.Len())
	}
}

// TestUndigestibleCountsAsChanged checks that an entity that can't be summarized is published rather than
// suppressed: suppressing it would be the riskier mistake.
func TestUndigestibleCountsAsChanged(t *testing.T) {
	tr := newTracker(t, state.Config{})
	tr.Record(report("a", t0, math.NaN(), t0.Add(2*time.Minute)), t0)
	admit(t, tr, report("a", t0.Add(time.Second), math.NaN(), t0.Add(2*time.Minute)), t0.Add(time.Second), nil)
}

// TestConfig checks the config's validation and defaults.
func TestConfig(t *testing.T) {
	if _, err := state.New(state.Config{MaxEntities: -1}); !errors.Is(err, state.ErrConfig) {
		t.Errorf("negative MaxEntities: err %v, want ErrConfig", err)
	}
}
