package pipeline_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// fuzzNow is the fixed clock of every FuzzPipeline run, one second into the seed scenario, so the seeds' reports are
// current and a mutated time can make one stale.
var fuzzNow = start.Add(time.Second)

// The sentinels each stage may wrap. A refusal that wraps none of its stage's sentinels is an untyped error, which a
// caller couldn't classify.
var (
	parseCauses = []error{
		cot.ErrTooLarge, cot.ErrTooDeep, cot.ErrTooManyElements, cot.ErrMalformed, cot.ErrDirective, cot.ErrProcInst,
		cot.ErrNotEvent, cot.ErrTrailingData, cot.ErrDuplicateAttr, cot.ErrDuplicateElement, cot.ErrMissing,
		cot.ErrInvalidField,
	}
	mapCauses     = []error{mapping.ErrNotAnAtom, mapping.ErrUnsupported, mapping.ErrExpired}
	framerCauses  = []error{transport.ErrFrameTooLarge, transport.ErrMalformedStream, transport.ErrTruncated}
	stageSentinel = []error{pipeline.ErrParse, pipeline.ErrMap, pipeline.ErrRejected}
)

// FuzzPipeline is the pipeline oracle. It treats the input as a TCP stream from a TAK feed: the framer splits it, and
// each message goes through the parser, the mapper and Entity Preflight, with one pipeline for the whole stream. It
// goes beyond "does not crash" by checking what comes out:
//
//   - Anything published passes Entity Preflight with no finding at all, except an out-of-order warning (input can
//     arrive out of order; dropping it is the state stage's job). Its JSON, checked from scratch by CheckJSON, has no
//     finding either, so the wire form uses only field names the SDK declares.
//   - The only reason preflight may reject an entity is an unchanged source update time: a repeated report. Any other
//     error-severity rule means the mapper built an entity the server would refuse, which is a mapper bug.
//   - Every refusal wraps exactly one stage sentinel, plus a sentinel of that stage's own.
//   - Running the same stream through a fresh pipeline gives exactly the same outcomes.
//
// Run it with: go test -fuzz=FuzzPipeline ./internal/pipeline
func FuzzPipeline(f *testing.F) {
	// 1: Seeds: a scenario stream, a repeated report, an out-of-order pair, every affiliation, and noise between
	// messages, all as they would arrive on the wire.
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
			b, err := xml.Marshal(ev)
			if err != nil {
				f.Fatal(err)
			}
			scenario = append(append(scenario, b...), '\n')
		}
	}
	one := message(f, "u-1", "a-f-G-U-C", start)
	later := message(f, "u-1", "a-f-G-U-C", start.Add(5*time.Second))
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
	} {
		f.Add(seed)
	}

	// 2: The oracle.
	f.Fuzz(func(t *testing.T, stream []byte) {
		first := runStream(t, stream)
		if second := runStream(t, stream); !slices.Equal(first, second) {
			t.Fatalf("not deterministic\nfirst:  %q\nsecond: %q", first, second)
		}
	})
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
		b, err := xml.Marshal(ev)
		if err != nil {
			f.Fatal(err)
		}
		out = append(append(out, b...), '\n')
	}
	return out
}

// runStream frames a stream and processes every message with one fresh pipeline, checking each outcome. It returns a
// line per outcome, for the determinism check.
func runStream(t *testing.T, stream []byte) []string {
	clock := func() time.Time { return fuzzNow }
	p, err := pipeline.New(feed, cot.DefaultLimits, clock)
	if err != nil {
		t.Fatal(err)
	}
	fr := transport.NewFramer(bytes.NewReader(stream), cot.DefaultLimits.MaxBytes)
	var outcomes []string
	for {
		msg, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return outcomes
		}
		if err != nil {
			if !wrapsAny(err, framerCauses) {
				t.Fatalf("framer returned an untyped error: %v", err)
			}
			return append(outcomes, "framer: "+err.Error()) // framer errors end the stream, as they end a connection
		}
		res, err := p.Process(msg)
		outcomes = append(outcomes, outcome(t, clock, res, err))
	}
}

// outcome checks one Process result against the oracle and describes it in one line.
func outcome(t *testing.T, clock func() time.Time, res pipeline.Result, err error) string {
	t.Helper()

	// 1: A refusal names exactly one stage.
	if err != nil && !wrapsOne(err, stageSentinel) {
		t.Fatalf("error does not wrap exactly one stage sentinel: %v", err)
	}
	switch {
	case err == nil:
		// 2: Published: no finding but an out-of-order warning, and a wire form that passes from scratch.
		for _, v := range res.Violations {
			if v.Rule.ID != "source-update-time-increases" {
				t.Fatalf("published an entity with a finding the mapper should never cause: %s", v)
			}
		}
		data, err := json.Marshal(res.Entity)
		if err != nil {
			t.Fatalf("published entity does not marshal: %v", err)
		}
		vs, err := preflight.NewChecker(clock).CheckJSON(data)
		if err != nil || vs != nil {
			t.Fatalf("published entity's JSON fails preflight: %v %v\n%s", err, vs, data)
		}
		return "published " + string(data)
	case errors.Is(err, pipeline.ErrRejected):
		// 3: Rejected: only ever for a repeated report.
		for _, v := range res.Violations {
			if v.Rule.Severity == preflight.Error && v.Rule.ID != "source-update-time-changes" {
				t.Fatalf("the mapper built an entity the server would reject: %s", v)
			}
		}
		return "rejected " + err.Error()
	case errors.Is(err, pipeline.ErrParse):
		if !wrapsAny(err, parseCauses) {
			t.Fatalf("parse refusal without a cot sentinel: %v", err)
		}
		return "parse " + err.Error()
	default: // ErrMap
		if !wrapsAny(err, mapCauses) {
			t.Fatalf("map refusal without a mapping sentinel: %v", err)
		}
		return "map " + err.Error()
	}
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
