package pipeline_test

import (
	"encoding/xml"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// start is when every test scenario begins.
var start = time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

// feed is the mapping config the tests use.
var feed = mapping.Config{Source: "test-feed", Simulated: true, TrackQuality: mapping.DefaultTrackQuality}

// message builds one valid CoT message: a friendly ground unit reporting at the given time, stale two minutes later.
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

// newPipeline returns a pipeline whose clock is whatever *now holds.
func newPipeline(t *testing.T, now *time.Time) *pipeline.Pipeline {
	t.Helper()
	p, err := pipeline.New(feed, cot.DefaultLimits, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ids lists the rule IDs of some violations.
func ids(vs []preflight.Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Rule.ID)
	}
	return out
}

// TestScenarioPublishes runs a cot-gen scenario through one pipeline and requires every message to come out ready to
// publish, with no findings at all.
func TestScenarioPublishes(t *testing.T) {
	s, err := cotgen.New(cotgen.Config{
		Units: 3, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: start,
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	p := newPipeline(t, &now)
	for tick := range 10 {
		for _, ev := range s.Next() {
			b, err := xml.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			now = time.Time(ev.Start)
			res, err := p.Process(b)
			if err != nil || res.Violations != nil {
				t.Fatalf("tick %d, %s: err %v, violations %v", tick, ev.UID, err, res.Violations)
			}
		}
	}
}

// TestRepeatedReportIsRejected checks that a report repeated with the same time, which TAK networks do when they
// rebroadcast, is held back: the server would discard it anyway.
func TestRepeatedReportIsRejected(t *testing.T) {
	now := start
	p := newPipeline(t, &now)
	msg := message(t, "u-1", "a-f-G-U-C", start)
	if _, err := p.Process(msg); err != nil {
		t.Fatalf("first report: %v", err)
	}
	res, err := p.Process(msg)
	if !errors.Is(err, pipeline.ErrRejected) {
		t.Fatalf("repeated report: err %v, want ErrRejected", err)
	}
	if res.Entity.EntityID == nil || !slices.Contains(ids(res.Violations), "source-update-time-changes") {
		t.Errorf("a rejection should still carry the entity and the reason: %+v", res)
	}
}

// TestOutOfOrderReportWarns checks that an older report after a newer one is passed with a warning. Dropping it is a
// state decision, which belongs to the state stage, not to preflight.
func TestOutOfOrderReportWarns(t *testing.T) {
	now := start.Add(10 * time.Second)
	p := newPipeline(t, &now)
	if _, err := p.Process(message(t, "u-1", "a-f-G-U-C", start.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	res, err := p.Process(message(t, "u-1", "a-f-G-U-C", start))
	if err != nil {
		t.Fatalf("out-of-order report: %v", err)
	}
	if got := ids(res.Violations); !slices.Equal(got, []string{"source-update-time-increases"}) {
		t.Errorf("violations = %v, want only source-update-time-increases", got)
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
		{"stray text", []byte("hello"), pipeline.ErrParse, cot.ErrTrailingData},
		{"malformed XML", []byte("<event"), pipeline.ErrParse, cot.ErrMalformed},
		{"not an event", []byte(`<chat/>`), pipeline.ErrParse, cot.ErrNotEvent},
		{"not an atom", message(t, "u-1", "b-m-p-s-p-i", start), pipeline.ErrMap, mapping.ErrNotAnAtom},
		{"unknown affiliation", message(t, "u-1", "a-q-G", start), pipeline.ErrMap, mapping.ErrUnsupported},
		{"already stale", message(t, "u-1", "a-f-G", start.Add(-time.Hour)), pipeline.ErrMap, mapping.ErrExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := start
			_, err := newPipeline(t, &now).Process(tt.msg)
			if !errors.Is(err, tt.stage) || !errors.Is(err, tt.cause) {
				t.Errorf("err = %v, want both %v and %v", err, tt.stage, tt.cause)
			}
		})
	}
}

// TestBadConfigFailsAtStartup checks that New, not the first message, reports a bad config.
func TestBadConfigFailsAtStartup(t *testing.T) {
	if _, err := pipeline.New(mapping.Config{}, cot.DefaultLimits, time.Now); !errors.Is(err, mapping.ErrConfig) {
		t.Errorf("New with an empty config: err %v, want ErrConfig", err)
	}
}

// TestClockReadOncePerMessage uses a clock that jumps an hour on every read. A report stale two minutes after it was
// sent passes only if the mapper and the checker saw the same reading; if the checker read the clock again, the
// expiry would already be in its past.
func TestClockReadOncePerMessage(t *testing.T) {
	reads := 0
	clock := func() time.Time {
		reads++
		return start.Add(time.Duration(reads-1) * time.Hour)
	}
	p, err := pipeline.New(feed, cot.DefaultLimits, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process(message(t, "u-1", "a-f-G-U-C", start)); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reads != 1 {
		t.Errorf("the clock was read %d times for one message, want 1", reads)
	}
}
