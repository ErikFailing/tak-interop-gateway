package pipeline_test

import (
	"context"
	"encoding/xml"
	"errors"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
)

// start is when every test scenario begins.
var start = time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

// feed is the mapping config the tests use.
var feed = mapping.Config{Source: "test-feed", Simulated: true, TrackQuality: mapping.DefaultTrackQuality}

// message builds one valid CoT message: a unit of the given type reporting at the given time from a fixed position,
// stale two minutes later.
func message(tb testing.TB, uid, typ string, at time.Time) []byte {
	tb.Helper()
	b, err := xml.Marshal(cot.Event{
		Version: cot.Version, UID: uid, Type: typ, How: "m-g",
		Time: cot.Timestamp(at), Start: cot.Timestamp(at), Stale: cot.Timestamp(at.Add(2 * time.Minute)),
		Point:  cot.Point{Lat: 40.3428, Lon: -105.6836, HAE: cot.Unknown, CE: 10, LE: cot.Unknown},
		Detail: &cot.Detail{Contact: &cot.Contact{Callsign: "UNIT-" + uid}},
	})
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// newPipeline returns a pipeline whose clock is whatever *now holds, publishing to the returned in-memory publisher.
func newPipeline(t *testing.T, now *time.Time, st state.Config) (*pipeline.Pipeline, *publish.Memory) {
	t.Helper()
	mem := &publish.Memory{}
	p, err := pipeline.New(pipeline.Config{Mapping: feed, State: st, Publisher: mem, Clock: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	return p, mem
}

// TestScenarioPublishes runs a cot-gen scenario through one pipeline. The walking team changes every tick, so every
// member report is published; the last-known-position marker never moves, so after its first publish it is held
// back until the refresh is due, 30 seconds (six ticks) later.
func TestScenarioPublishes(t *testing.T) {
	s, err := cotgen.New(cotgen.Config{
		Units: 3, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: start,
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	p, mem := newPipeline(t, &now, state.Config{})
	lkpPublished := 0
	for tick := range 10 {
		for _, ev := range s.Next() {
			b, err := xml.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			now = time.Time(ev.Start)
			res, err := p.Process(context.Background(), b, "feed")
			switch {
			case err == nil && res.Violations == nil:
				if ev.UID == "cotgen-lkp-1" {
					lkpPublished++
				}
			case errors.Is(err, state.ErrUnchanged) && ev.UID == "cotgen-lkp-1":
			default:
				t.Fatalf("tick %d, %s: err %v, violations %v", tick, ev.UID, err, res.Violations)
			}
		}
	}
	if got, want := len(mem.Entities()), 3*10+2; got != want {
		t.Errorf("published %d entities, want %d (30 member reports, and the marker at 0 s and 30 s)", got, want)
	}
	if lkpPublished != 2 {
		t.Errorf("the marker was published %d times, want 2", lkpPublished)
	}
}

// TestStateDrops checks that a repeated report and a late one are dropped before they reach the publisher.
func TestStateDrops(t *testing.T) {
	now := start.Add(10 * time.Second)
	p, mem := newPipeline(t, &now, state.Config{})
	ctx := context.Background()
	later := message(t, "u-1", "a-f-G-U-C", start.Add(5*time.Second))
	if _, err := p.Process(ctx, later, "feed"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		msg  []byte
		want error
	}{
		{"repeated", later, state.ErrRepeated},
		{"out of order", message(t, "u-1", "a-f-G-U-C", start), state.ErrOutOfOrder},
	} {
		res, err := p.Process(ctx, tt.msg, "feed")
		if !errors.Is(err, pipeline.ErrDropped) || !errors.Is(err, tt.want) {
			t.Errorf("%s: err %v, want ErrDropped and %v", tt.name, err, tt.want)
		}
		if res.Entity.EntityID == nil {
			t.Errorf("%s: a dropped report should still carry its entity, for logging", tt.name)
		}
	}
	if n := len(mem.Entities()); n != 1 {
		t.Errorf("published %d entities, want 1", n)
	}
}

// flaky fails its first publish and accepts the rest.
type flaky struct {
	failed bool
	publish.Memory
}

func (f *flaky) Publish(ctx context.Context, e entity.Entity) error {
	if !f.failed {
		f.failed = true
		return errors.New("connection reset")
	}
	return f.Memory.Publish(ctx, e)
}

// TestFailedPublishIsNotRecorded checks that a report whose publish failed is not counted as sent: the same report
// arriving again, as a rebroadcast would, goes out instead of being dropped as a repeat.
func TestFailedPublishIsNotRecorded(t *testing.T) {
	pub := &flaky{}
	p, err := pipeline.New(pipeline.Config{Mapping: feed, Publisher: pub, Clock: func() time.Time { return start }})
	if err != nil {
		t.Fatal(err)
	}
	msg := message(t, "u-1", "a-f-G-U-C", start)
	if _, err := p.Process(context.Background(), msg, "feed"); !errors.Is(err, pipeline.ErrPublish) {
		t.Fatalf("first attempt: err %v, want ErrPublish", err)
	}
	if _, err := p.Process(context.Background(), msg, "feed"); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if n := len(pub.Entities()); n != 1 {
		t.Errorf("published %d entities, want 1", n)
	}
}

// TestRejectedByPreflight checks the preflight gate with a feed whose classification the SDK doesn't know: the mapper
// can't check it, because it doesn't import the SDK, so preflight is what stops it. takgw serve checks the level at
// startup with preflight.CheckClassification, so this is the gate for library callers that skip that check.
func TestRejectedByPreflight(t *testing.T) {
	cfg := feed
	cfg.Classification = "SECRET"
	mem := &publish.Memory{}
	p, err := pipeline.New(pipeline.Config{Mapping: cfg, Publisher: mem, Clock: func() time.Time { return start }})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Process(context.Background(), message(t, "u-1", "a-f-G-U-C", start), "feed")
	if !errors.Is(err, pipeline.ErrRejected) || len(res.Violations) == 0 || res.Violations[0].Rule.ID != "enum-values" {
		t.Errorf("err %v, violations %v; want ErrRejected for enum-values", err, res.Violations)
	}
	if n := len(mem.Entities()); n != 0 {
		t.Errorf("published %d entities, want 0", n)
	}
	// A rejected report isn't recorded either, so a corrected one can follow.
	if p.NumTracked() != 0 {
		t.Errorf("NumTracked = %d, want 0", p.NumTracked())
	}
}

// TestStageErrors checks that each refusal names its stage and keeps the stage's own reason.
func TestStageErrors(t *testing.T) {
	tests := []struct {
		name  string
		msg   []byte
		stage error
		cause error
	}{
		{"stray text", []byte("hello"), pipeline.ErrParse, cot.ErrStrayData},
		{"malformed XML", []byte("<event"), pipeline.ErrParse, cot.ErrMalformed},
		{"not an event", []byte(`<chat/>`), pipeline.ErrParse, cot.ErrNotEvent},
		{"not an atom", message(t, "u-1", "b-m-p-s-p-i", start), pipeline.ErrMap, mapping.ErrNotAnAtom},
		{"unknown affiliation", message(t, "u-1", "a-q-G", start), pipeline.ErrMap, mapping.ErrUnsupported},
		{"already stale", message(t, "u-1", "a-f-G", start.Add(-time.Hour)), pipeline.ErrMap, mapping.ErrExpired},
		{"too many entities", message(t, "u-2", "a-f-G", start), pipeline.ErrDropped, state.ErrFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := start
			p, _ := newPipeline(t, &now, state.Config{MaxEntities: 1})
			if _, err := p.Process(context.Background(), message(t, "u-1", "a-f-G", start), "feed"); err != nil {
				t.Fatal(err) // fills the one slot
			}
			_, err := p.Process(context.Background(), tt.msg, "feed")
			if !errors.Is(err, tt.stage) || !errors.Is(err, tt.cause) {
				t.Errorf("err = %v, want both %v and %v", err, tt.stage, tt.cause)
			}
		})
	}
}

// TestIntegrityDrops checks the two feed-integrity drops end to end: a ground unit 70 km away a second later, and
// another origin claiming a live track.
func TestIntegrityDrops(t *testing.T) {
	now := start.Add(time.Second)
	p, mem := newPipeline(t, &now, state.Config{})
	ctx := context.Background()
	if _, err := p.Process(ctx, message(t, "u-1", "a-f-G-U-C", start), "unit-a"); err != nil {
		t.Fatal(err)
	}
	var jump cot.Event
	if err := xml.Unmarshal(message(t, "u-1", "a-f-G-U-C", start.Add(time.Second)), &jump); err != nil {
		t.Fatal(err)
	}
	jump.Point.Lat += 0.63
	far, err := xml.Marshal(jump)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process(ctx, far, "unit-a"); !errors.Is(err, pipeline.ErrDropped) || !errors.Is(err, state.ErrImplausible) {
		t.Errorf("teleport: err %v, want ErrDropped and ErrImplausible", err)
	}
	if _, err := p.Process(ctx, message(t, "u-1", "a-f-G-U-C", start.Add(2*time.Second)), "intruder"); !errors.Is(err, state.ErrOriginConflict) {
		t.Errorf("impersonation: err %v, want ErrOriginConflict", err)
	}
	if n := len(mem.Entities()); n != 1 {
		t.Errorf("published %d entities, want only the first report", n)
	}
}

// TestBadConfigFailsAtStartup checks that New, not the first message, reports a bad config.
func TestBadConfigFailsAtStartup(t *testing.T) {
	mem := &publish.Memory{}
	if _, err := pipeline.New(pipeline.Config{Publisher: mem}); !errors.Is(err, mapping.ErrConfig) {
		t.Errorf("empty mapping config: err %v, want mapping.ErrConfig", err)
	}
	if _, err := pipeline.New(pipeline.Config{Mapping: feed, Publisher: mem, State: state.Config{MaxEntities: -1}}); !errors.Is(err, state.ErrConfig) {
		t.Errorf("negative MaxEntities: err %v, want state.ErrConfig", err)
	}
	if _, err := pipeline.New(pipeline.Config{Mapping: feed}); err == nil {
		t.Error("no publisher: want an error")
	}
}

// TestClockReadOncePerMessage uses a clock that jumps an hour on every read. A report stale two minutes after it was
// sent passes only if every stage saw the same reading; if preflight read the clock again, the expiry would already
// be in its past.
func TestClockReadOncePerMessage(t *testing.T) {
	reads := 0
	clock := func() time.Time {
		reads++
		return start.Add(time.Duration(reads-1) * time.Hour)
	}
	p, err := pipeline.New(pipeline.Config{Mapping: feed, Publisher: &publish.Memory{}, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process(context.Background(), message(t, "u-1", "a-f-G-U-C", start), "feed"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reads != 1 {
		t.Errorf("the clock was read %d times for one message, want 1", reads)
	}
}
