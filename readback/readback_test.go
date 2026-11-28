package readback_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/readback"
)

// TestDiff checks each kind of difference, and the normalizations a server may apply that are not differences.
func TestDiff(t *testing.T) {
	tests := []struct {
		name      string
		sent, got string
		want      []string // each difference's String()
	}{
		{"identical", `{"a":1,"b":{"c":"x"}}`, `{"b":{"c":"x"},"a":1}`, nil},
		{"dropped", `{"a":1,"b":{"c":"x","d":true}}`, `{"a":1,"b":{"c":"x"}}`, []string{"dropped b.d (sent true)"}},
		{"changed", `{"a":"SFGPUC---------"}`, `{"a":"SFGP-----------"}`, []string{`changed a: sent "SFGPUC---------", got "SFGP-----------"`}},
		{"added", `{"a":1}`, `{"a":1,"createdTime":"2026-11-27T16:00:00Z"}`, []string{`added createdTime: "2026-11-27T16:00:00Z"`}},
		{"same instant, written differently", `{"t":"2026-11-27T16:00:00.000Z"}`, `{"t":"2026-11-27T09:00:00-07:00"}`, nil},
		{"different instants", `{"t":"2026-11-27T16:00:00Z"}`, `{"t":"2026-11-27T16:00:01Z"}`, []string{`changed t: sent "2026-11-27T16:00:00Z", got "2026-11-27T16:00:01Z"`}},
		{"same number, written differently", `{"n":1.30}`, `{"n":1.3}`, nil},
		{"number rounded", `{"n":40.3428123}`, `{"n":40.3428}`, []string{"changed n: sent 40.3428123, got 40.3428"}},
		{"type changed", `{"a":{"b":1}}`, `{"a":"b"}`, []string{`changed a: sent {"b":1}, got "b"`}},
		{"number became a string", `{"a":1}`, `{"a":"1"}`, []string{`changed a: sent 1, got "1"`}},
		{"array element dropped", `{"a":[1,2]}`, `{"a":[1]}`, []string{"dropped a[1] (sent 2)"}},
		{"array element added", `{"a":[1]}`, `{"a":[1,2]}`, []string{"added a[1]: 2"}},
		{"odd key", `{"a.b":1}`, `{}`, []string{`dropped ["a.b"] (sent 1)`}},
		{"null came back", `{"a":1}`, `{"a":null}`, []string{"changed a: sent 1, got null"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diffs, err := readback.Diff([]byte(tt.sent), []byte(tt.got))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range diffs {
				got = append(got, d.String())
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Diff = %q, want %q", got, tt.want)
			}
		})
	}
	for _, bad := range []string{"", "null", "[]", "{} {}", "{"} {
		if _, err := readback.Diff([]byte(bad), []byte(`{}`)); err == nil {
			t.Errorf("Diff(%q, {}) accepted a document that isn't one JSON object", bad)
		}
	}
}

// memory is a Client that stores entities as JSON, with hooks to simulate failures and a platform that rewrites.
type memory struct {
	stored     map[string][]byte
	publishErr error
	getErr     error
	rewrite    func(map[string]any)
}

func (m *memory) Publish(_ context.Context, e entity.Entity) error {
	if m.publishErr != nil {
		return m.publishErr
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if m.rewrite != nil {
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			return err
		}
		m.rewrite(doc)
		if b, err = json.Marshal(doc); err != nil {
			return err
		}
	}
	if m.stored == nil {
		m.stored = map[string][]byte{}
	}
	m.stored[*e.EntityID] = b
	return nil
}

func (m *memory) Get(_ context.Context, id string) ([]byte, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.stored[id], nil
}

// sample returns a small entity with the given ID.
func sample(id string) entity.Entity {
	return entity.Entity{EntityID: new(id), IsLive: new(true), Aliases: &entity.Aliases{Name: new("UNIT-" + id)}}
}

// TestRun checks the readback of a set of entities through each outcome.
func TestRun(t *testing.T) {
	ctx := context.Background()
	t.Run("everything comes back", func(t *testing.T) {
		rs := readback.Run(ctx, &memory{}, []entity.Entity{sample("a"), sample("b")}, 0)
		if len(rs) != 2 || !rs[0].Passed() || !rs[1].Passed() || rs[0].Name != "UNIT-a" || rs[1].EntityID != "b" {
			t.Errorf("results = %+v", rs)
		}
	})
	t.Run("a field is dropped", func(t *testing.T) {
		m := &memory{rewrite: func(doc map[string]any) { delete(doc, "aliases") }}
		rs := readback.Run(ctx, m, []entity.Entity{sample("a")}, 0)
		if rs[0].Passed() || len(rs[0].Differences) != 1 || rs[0].Differences[0].Kind != readback.Dropped {
			t.Errorf("result = %+v", rs[0])
		}
	})
	t.Run("a field is added", func(t *testing.T) {
		m := &memory{rewrite: func(doc map[string]any) { doc["createdTime"] = "2026-11-27T16:00:00Z" }}
		if rs := readback.Run(ctx, m, []entity.Entity{sample("a")}, 0); !rs[0].Passed() || len(rs[0].Differences) != 1 {
			t.Errorf("an added field should be reported but pass: %+v", rs[0])
		}
	})
	t.Run("publish fails", func(t *testing.T) {
		rs := readback.Run(ctx, &memory{publishErr: errors.New("refused")}, []entity.Entity{sample("a")}, 0)
		if rs[0].Passed() || rs[0].Err == nil {
			t.Errorf("result = %+v", rs[0])
		}
	})
	t.Run("read back fails", func(t *testing.T) {
		rs := readback.Run(ctx, &memory{getErr: errors.New("gone")}, []entity.Entity{sample("a")}, 0)
		if rs[0].Passed() || rs[0].Err == nil {
			t.Errorf("result = %+v", rs[0])
		}
	})
	t.Run("no entity ID", func(t *testing.T) {
		rs := readback.Run(ctx, &memory{}, []entity.Entity{{IsLive: new(true)}}, 0)
		if rs[0].Passed() || rs[0].Err == nil {
			t.Errorf("result = %+v", rs[0])
		}
	})
	t.Run("a cancelled context cuts the wait short", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		start := time.Now()
		readback.Run(cancelled, &memory{}, []entity.Entity{sample("a")}, time.Hour)
		if time.Since(start) > 10*time.Second {
			t.Error("Run waited despite a cancelled context")
		}
	})
}
