package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// fuzzNow is the fixed clock of every FuzzPipeline run, one second into the seed scenario, so the seeds' reports are
// current and a mutated time can make one stale. With the clock fixed, nothing published ever expires during a run,
// so the state stage never forgets an entity; the reference model below relies on that.
var fuzzNow = start.Add(time.Second)

// fuzzState is the state config under fuzzing: a small entity limit so ErrFull is reachable from short inputs.
var fuzzState = state.Config{Refresh: 30 * time.Second, MaxEntities: 8}

// The sentinels each stage may wrap. A refusal that wraps none of its stage's sentinels is an untyped error, which a
// caller couldn't classify.
var (
	parseCauses = []error{
		cot.ErrTooLarge, cot.ErrTooDeep, cot.ErrTooManyElements, cot.ErrMalformed, cot.ErrDirective, cot.ErrProcInst,
		cot.ErrNotEvent, cot.ErrStrayData, cot.ErrDuplicateAttr, cot.ErrDuplicateElement, cot.ErrMissing,
		cot.ErrInvalidField,
	}
	mapCauses     = []error{mapping.ErrNotAnAtom, mapping.ErrUnsupported, mapping.ErrExpired}
	framerCauses  = []error{transport.ErrFrameTooLarge, transport.ErrMalformedStream, transport.ErrTruncated}
	stageSentinel = []error{pipeline.ErrParse, pipeline.ErrMap, pipeline.ErrDropped, pipeline.ErrRejected, pipeline.ErrPublish}
	dropReasons   = map[error]string{
		state.ErrRepeated: "repeated", state.ErrOutOfOrder: "out of order", state.ErrUnchanged: "unchanged", state.ErrFull: "full",
		state.ErrImplausible: "implausible", state.ErrOriginConflict: "origin conflict", state.ErrFutureTime: "future time",
	}
)

// FuzzPipeline is the pipeline oracle. It treats the input as a TCP stream from a TAK feed: the framer splits it, and
// each message goes through the parser, the mapper, the state stage, Entity Preflight and the in-memory publisher,
// with one pipeline for the whole stream. It goes beyond "does not crash" by checking what comes out:
//
//   - Every entity published passes Entity Preflight with no finding of any severity, and so does its JSON, checked
//     from scratch by CheckJSON, so the wire form uses only field names the SDK declares.
//   - Preflight never rejects anything: the mapper never builds an entity the server would refuse, and the state
//     stage removes repeats before preflight sees them.
//   - Each report the mapper accepts is published or dropped exactly as an independent reference model of the state
//     policy predicts, with the same reason. So published updates strictly increase per entity, nothing stamped
//     too far ahead is published, no published move is faster than the stricter of its old and new environment
//     allows, and nothing changed is ever suppressed. Every message comes from one origin, so the origin binding is
//     covered by its unit tests instead.
//   - Every refusal wraps exactly one stage sentinel, plus a sentinel of that stage's own.
//   - The publisher received exactly the entities reported as published, in order.
//   - Running the same stream through a fresh pipeline gives exactly the same outcomes.
//
// Run it with: go test -fuzz=FuzzPipeline -fuzzminimizetime=5s ./internal/pipeline
//
// The 5-second minimization budget matters here. Go's fuzzer shrinks each input that finds new coverage before keeping
// it, for up to a minute by default, and a worker shrinking an input isn't fuzzing. An exec of this target runs a whole
// stream twice, so shrinking a stream of a few kilobytes takes seconds, and idle workers take shrinking jobs as readily
// as 100-millisecond fuzzing ones: with the default, every worker ends up shrinking and fuzzing stops. In a 60-second
// run from the seeds alone, the default left no job finishing for 54 seconds and kept 2 new inputs; with 5 seconds,
// the same run kept 140.
func FuzzPipeline(f *testing.F) {
	// 1: Seeds: a scenario stream, a repeated report, an out-of-order pair, every affiliation (more uids than the
	// entity limit), noise between messages, the mapper's less common branches, an unchanged report whose published
	// copy is about to expire, a jump, a jump retyped into space, and a report stamped an hour ahead.
	s, err := cotgen.New(cotgen.Config{
		Units: 2, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: start,
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 1,
	})
	if err != nil {
		f.Fatal(err)
	}
	var scenario []byte
	for range 2 {
		for _, ev := range s.Next() {
			scenario = append(append(scenario, marshal(f, ev)...), '\n')
		}
	}
	one := message(f, "u-1", "a-f-G-U-C", start)
	later := message(f, "u-1", "a-f-G-U-C", start.Add(5*time.Second))
	teleport := cot.Event{
		Version: cot.Version, UID: "u-1", Type: "a-f-G-U-C", How: "m-g",
		Time: cot.Timestamp(start.Add(time.Second)), Start: cot.Timestamp(start.Add(time.Second)),
		Stale: cot.Timestamp(start.Add(2 * time.Minute)),
		Point: cot.Point{Lat: 40.5, Lon: -105.6836, HAE: cot.Unknown, CE: 10, LE: cot.Unknown},
	}
	toSpace := teleport
	toSpace.Type = "a-f-P" // space has no limit of its own, so only the old environment's stops this
	var affiliations []byte
	for _, typ := range []string{"a-f-G", "a-h-A", "a-u-S", "a-n-U", "a-p-P", "a-a-F", "a-s-X", "a-j-Z", "a-k-G", "a-o", "a-x-G-UC"} {
		affiliations = append(affiliations, message(f, "uid-"+typ, typ, start)...)
	}
	for _, seed := range [][]byte{
		scenario,
		slices.Concat(one, []byte("\n"), one),
		slices.Concat(later, one),
		affiliations,
		slices.Concat(one, []byte(" junk "), later),
		branchSeeds(f),
		expiringSeed(f),
		slices.Concat(one, []byte("\n"), marshal(f, teleport)), // a ground unit 17 km away a second later
		slices.Concat(one, []byte("\n"), marshal(f, toSpace)),
		message(f, "u-future", "a-f-G-U-C", start.Add(time.Hour)),
	} {
		f.Add(seed)
	}

	// 2: The oracle.
	f.Fuzz(func(t *testing.T, stream []byte) {
		checked := map[string]bool{}
		first := runStream(t, stream, checked)
		if second := runStream(t, stream, checked); !slices.Equal(first, second) {
			t.Fatalf("not deterministic\nfirst:  %q\nsecond: %q", first, second)
		}
	})
}

// marshal encodes one event, failing the test on error.
func marshal(tb testing.TB, ev cot.Event) []byte {
	tb.Helper()
	b, err := xml.Marshal(ev)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// branchSeeds is one stream that reaches each mapper branch the other seeds don't: a stale time decades out (static
// markers often use one, and the mapper must clamp it under 30 days), a track with course and speed, no detail at all
// (so the uid stands in for the name), and a blank callsign. A mutation test showed the clamp needs its own seed: the
// fuzzer didn't stretch a two-minute stale past 30 days on its own within a minute.
func branchSeeds(f *testing.F) []byte {
	base := cot.Event{
		Version: cot.Version, Type: "a-h-G-U-C", How: "h-e",
		Time: cot.Timestamp(start), Start: cot.Timestamp(start), Stale: cot.Timestamp(start.Add(2 * time.Minute)),
		Point: cot.Point{Lat: -33.5, Lon: 151.2, HAE: 12.5, CE: cot.Unknown, LE: cot.Unknown},
	}
	farStale, track, bare, blank := base, base, base, base
	farStale.UID = "marker-far-stale"
	farStale.Stale = cot.Timestamp(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	track.UID = "track-moving"
	track.Detail = &cot.Detail{Track: &cot.Track{Course: 271.5, Speed: 3.2}}
	bare.UID = "no-detail"
	blank.UID = "blank-callsign"
	blank.Detail = &cot.Detail{Contact: &cot.Contact{Callsign: "   "}}
	var out []byte
	for _, ev := range []cot.Event{farStale, track, bare, blank} {
		out = append(append(out, marshal(f, ev)...), '\n')
	}
	return out
}

// expiringSeed is an unchanged report sent twice, a second apart, whose stale time is only 20 seconds out: inside the
// 30-second refresh, so the state stage must republish it rather than let the published copy lapse.
func expiringSeed(f *testing.F) []byte {
	ev := cot.Event{
		Version: cot.Version, UID: "expiring", Type: "a-n-S", How: "m-g",
		Time: cot.Timestamp(start), Start: cot.Timestamp(start), Stale: cot.Timestamp(fuzzNow.Add(20 * time.Second)),
		Point: cot.Point{Lat: 10, Lon: 20, HAE: cot.Unknown, CE: cot.Unknown, LE: cot.Unknown},
	}
	again := ev
	again.Time = cot.Timestamp(start.Add(time.Second))
	return slices.Concat(marshal(f, ev), []byte("\n"), marshal(f, again))
}

// published is what the reference model remembers about an entity's last publish.
type published struct {
	updated time.Time
	expiry  time.Time
	content string             // the entity's JSON with its two time fields cleared
	fix     state.Fix          // its position and uncertainty radius
	env     entity.Environment // its environment
}

// model is an independent statement of the state stage's policy, for a fixed clock: nothing expires during a run,
// so nothing is ever forgotten.
type model struct {
	last map[string]published
}

// expect says what the pipeline should do with a mapped entity: "published", or the reason it is dropped.
func (m *model) expect(e entity.Entity) string {
	r, seen := m.last[*e.EntityID]
	updated := *e.Provenance.SourceUpdateTime
	switch {
	case updated.After(fuzzNow.Add(state.DefaultMaxSkew)):
		return "future time"
	case !seen && len(m.last) >= fuzzState.MaxEntities:
		return "full"
	case !seen:
		return "published"
	case updated.Equal(r.updated):
		return "repeated"
	case updated.Before(r.updated):
		return "out of order"
	case tooFast(r, e, updated):
		return "implausible"
	case content(e) == r.content && r.expiry.Sub(fuzzNow) > fuzzState.Refresh:
		// Published at fuzzNow, checked at fuzzNow: zero time since the publish, so the refresh is never due.
		return "unchanged"
	}
	return "published"
}

// fixOf is the position and uncertainty radius the mapper gave an entity: it always sets a position, and sets the
// ellipse's semi-major axis to the CoT ce when ce is known.
func fixOf(e entity.Entity) state.Fix {
	f := state.Fix{Lat: *e.Location.Position.LatitudeDegrees, Lon: *e.Location.Position.LongitudeDegrees}
	if u := e.LocationUncertainty; u != nil {
		f.CE = *u.PositionErrorEllipse.SemiMajorAxisM
	}
	return f
}

// tooFast says whether moving from the last published fix to e's, over the source's own clock, beats the stricter of
// the default limits for the old and the new environment. The distance arithmetic is the state package's, pinned by
// TestImpliedSpeed's known answers; the decision is restated here.
func tooFast(r published, e entity.Entity, updated time.Time) bool {
	limit := stricter(state.DefaultMaxSpeed[r.env], state.DefaultMaxSpeed[*e.MilView.Environment]) // the mapper always sets it
	return limit > 0 && state.ImpliedSpeed(r.fix, fixOf(e), updated.Sub(r.updated)) > limit
}

// stricter returns the lower of two speed limits, ignoring one of zero or less, which means no limit.
func stricter(a, b float64) float64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return min(a, b)
}

// content is an entity's JSON with expiryTime and provenance.sourceUpdateTime cleared: what "unchanged" compares.
func content(e entity.Entity) string {
	e.ExpiryTime = nil
	p := *e.Provenance
	p.SourceUpdateTime = nil
	e.Provenance = &p
	b, _ := json.Marshal(e) // the mapper never produces a NaN or an infinity, the only things that fail to marshal
	return string(b)
}

// runStream frames a stream and processes every message with one fresh pipeline, checking each outcome. It returns a
// line per outcome, for the determinism check. checked holds the published JSON already passed by CheckJSON in this
// exec, so the determinism run, which must publish the same JSON, doesn't pay to check it again: CheckJSON is most of
// an exec's time.
func runStream(t *testing.T, stream []byte, checked map[string]bool) []string {
	clock := func() time.Time { return fuzzNow }
	mem := &publish.Memory{}
	p, err := pipeline.New(pipeline.Config{Mapping: feed, State: fuzzState, Publisher: mem, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	ref := &model{last: map[string]published{}}
	fr := transport.NewFramer(bytes.NewReader(stream), cot.DefaultLimits.MaxBytes)
	var outcomes []string
	var sent []entity.Entity
	for {
		msg, err := fr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !wrapsAny(err, framerCauses) {
				t.Fatalf("framer returned an untyped error: %v", err)
			}
			outcomes = append(outcomes, "framer: "+err.Error()) // framer errors end the stream, as they end a connection
			break
		}
		res, err := p.Process(context.Background(), msg, "feed")
		line := outcome(t, ref, clock, res, err, checked)
		if err == nil {
			sent = append(sent, res.Entity)
		}
		outcomes = append(outcomes, line)
	}

	// The publisher saw exactly what was reported as published, in the same order.
	got := mem.Entities()
	if len(got) != len(sent) {
		t.Fatalf("the publisher received %d entities, but %d were reported published", len(got), len(sent))
	}
	for i := range got {
		if a, b := mustJSON(t, got[i]), mustJSON(t, sent[i]); a != b {
			t.Fatalf("publish %d differs from the reported entity\npublisher: %s\nreported:  %s", i, a, b)
		}
	}
	return outcomes
}

// outcome checks one Process result against the oracle and describes it in one line.
func outcome(t *testing.T, ref *model, clock func() time.Time, res pipeline.Result, err error, checked map[string]bool) string {
	t.Helper()

	// 1: A refusal names exactly one stage. Parse and map refusals happen before the state stage, so the model
	// doesn't see them.
	if err != nil && !wrapsOne(err, stageSentinel) {
		t.Fatalf("error does not wrap exactly one stage sentinel: %v", err)
	}
	switch {
	case errors.Is(err, pipeline.ErrParse):
		if !wrapsAny(err, parseCauses) {
			t.Fatalf("parse refusal without a cot sentinel: %v", err)
		}
		return "parse " + err.Error()
	case errors.Is(err, pipeline.ErrMap):
		if !wrapsAny(err, mapCauses) {
			t.Fatalf("map refusal without a mapping sentinel: %v", err)
		}
		return "map " + err.Error()
	case errors.Is(err, pipeline.ErrRejected):
		t.Fatalf("preflight rejected a mapped entity, which the mapper and the state stage should make impossible: %v", err)
	case errors.Is(err, pipeline.ErrPublish):
		t.Fatalf("the in-memory publisher never fails, yet: %v", err)
	}

	// 2: The mapper accepted the report, so the state stage decided its fate. It must match the reference model.
	want := ref.expect(res.Entity)
	got := "published"
	if err != nil {
		got = "unknown drop reason"
		for sentinel, reason := range dropReasons {
			if errors.Is(err, sentinel) {
				got = reason
			}
		}
	}
	if got != want {
		t.Fatalf("the state stage said %q, the reference model %q, for\n%s", got, want, mustJSON(t, res.Entity))
	}
	if err != nil {
		return "dropped " + err.Error()
	}

	// 3: Published: no finding at all, as a value or as JSON checked from scratch.
	if res.Violations != nil {
		t.Fatalf("published an entity with findings: %v", res.Violations)
	}
	data := mustJSON(t, res.Entity)
	if !checked[data] {
		if vs, err := preflight.NewChecker(clock).CheckJSON([]byte(data)); err != nil || vs != nil {
			t.Fatalf("published entity's JSON fails preflight: %v %v\n%s", err, vs, data)
		}
		checked[data] = true
	}
	ref.last[*res.Entity.EntityID] = published{
		updated: *res.Entity.Provenance.SourceUpdateTime, expiry: *res.Entity.ExpiryTime, content: content(res.Entity),
		fix: fixOf(res.Entity), env: *res.Entity.MilView.Environment,
	}
	return "published " + data
}

// mustJSON encodes an entity, failing the test on error.
func mustJSON(t *testing.T, e entity.Entity) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("entity does not marshal: %v", err)
	}
	return string(b)
}

// wrapsAny reports whether err wraps at least one of the given sentinels.
func wrapsAny(err error, sentinels []error) bool {
	return slices.ContainsFunc(sentinels, func(s error) bool { return errors.Is(err, s) })
}

// wrapsOne reports whether err wraps exactly one of the given sentinels.
func wrapsOne(err error, sentinels []error) bool {
	n := 0
	for _, s := range sentinels {
		if errors.Is(err, s) {
			n++
		}
	}
	return n == 1
}
