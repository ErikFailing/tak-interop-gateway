package hostile_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/hostile"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/pipeline"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// now is when every case is stamped and checked.
var now = time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

// TestCases runs every case through the real framer and pipeline, on its own stream, and requires exactly the
// outcome it names.
func TestCases(t *testing.T) {
	cases := hostile.Cases(now)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			// 1: A fresh pipeline, as one gateway feed.
			mem := &publish.Memory{}
			p, err := pipeline.New(pipeline.Config{
				Mapping:   mapping.Config{Source: "hostile-test", TrackQuality: mapping.DefaultTrackQuality},
				Publisher: mem, Clock: func() time.Time { return now.Add(500 * time.Millisecond) },
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()

			// 2: The setup must all be published.
			for _, msg := range c.Setup {
				if _, err := p.Process(ctx, msg, "sender"); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}

			// 3: The attack goes through the framer, as it would off a socket.
			origin := "sender"
			if c.Impersonates {
				origin = "intruder"
			}
			fr := transport.NewFramer(bytes.NewReader(c.Attack), cot.DefaultLimits.MaxBytes)
			msg, err := fr.Next()
			if c.StreamFatal() {
				if !errors.Is(err, c.Cause) {
					t.Fatalf("framer: err %v, want %v", err, c.Cause)
				}
				return
			}
			if err != nil {
				t.Fatalf("framer: %v", err)
			}
			res, err := p.Process(ctx, msg, origin)

			// 4: Exactly the named outcome.
			if c.Stage == nil {
				if err != nil {
					t.Fatalf("want it accepted, got %v", err)
				}
				if c.Check != nil {
					if err := c.Check(res.Entity); err != nil {
						t.Error(err)
					}
				}
				return
			}
			if !errors.Is(err, c.Stage) || !errors.Is(err, c.Cause) {
				t.Fatalf("err = %v, want %v and %v", err, c.Stage, c.Cause)
			}
			if n := len(mem.Entities()); n != len(c.Setup) {
				t.Errorf("published %d entities, want only the %d from the setup", n, len(c.Setup))
			}
			if _, err := fr.Next(); !errors.Is(err, io.EOF) {
				t.Errorf("the attack framed into more than one message: %v", err)
			}
		})
	}
}

// TestCatalogue checks the catalogue itself: unique names, each with a description, an expectation and an outcome,
// and Find.
func TestCatalogue(t *testing.T) {
	cases := hostile.Cases(now)
	seen := map[string]bool{}
	for _, c := range cases {
		if c.Name == "" || seen[c.Name] || c.Description == "" || c.Expect == "" || len(c.Attack) == 0 {
			t.Errorf("incomplete or duplicate case %+v", c.Name)
		}
		if c.Stage == nil && c.Cause == nil && c.Check == nil {
			t.Errorf("%s: no outcome to check", c.Name)
		}
		seen[c.Name] = true
	}
	if _, err := hostile.Find(cases, "teleport"); err != nil {
		t.Error(err)
	}
	if _, err := hostile.Find(cases, "nope"); !errors.Is(err, hostile.ErrUnknownCase) {
		t.Errorf("Find(nope) = %v", err)
	}
}
