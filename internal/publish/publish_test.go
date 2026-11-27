package publish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
)

// both adapters must satisfy the interface.
var (
	_ publish.Publisher = (*publish.Memory)(nil)
	_ publish.Publisher = (*publish.JSONL)(nil)
)

// named returns an entity with just an ID.
func named(id string) entity.Entity {
	return entity.Entity{EntityID: new(id)}
}

// TestMemoryKeepsOrder checks that Memory returns what it was given, in order, and that the slice it returns is a
// copy.
func TestMemoryKeepsOrder(t *testing.T) {
	var m publish.Memory
	for _, id := range []string{"a", "b", "c"} {
		if err := m.Publish(context.Background(), named(id)); err != nil {
			t.Fatal(err)
		}
	}
	got := m.Entities()
	if len(got) != 3 || *got[0].EntityID != "a" || *got[2].EntityID != "c" {
		t.Fatalf("Entities = %v", got)
	}
	got[0] = named("changed")
	if *m.Entities()[0].EntityID != "a" {
		t.Error("changing the returned slice changed the publisher's copy")
	}
}

// TestMemoryConcurrent checks that concurrent publishes are all kept. Run with -race.
func TestMemoryConcurrent(t *testing.T) {
	var m publish.Memory
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { _ = m.Publish(context.Background(), named("x")) })
	}
	wg.Wait()
	if n := len(m.Entities()); n != 50 {
		t.Errorf("kept %d entities, want 50", n)
	}
}

// TestJSONLLines checks that each entity becomes one line of JSON that decodes back to it.
func TestJSONLLines(t *testing.T) {
	var buf bytes.Buffer
	j := publish.NewJSONL(&buf)
	for _, id := range []string{"a", "b"} {
		if err := j.Publish(context.Background(), named(id)); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), buf.String())
	}
	var e entity.Entity
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil || *e.EntityID != "b" {
		t.Errorf("line 2 = %q, decoded %v, %v", lines[1], e.EntityID, err)
	}
}

// failWriter fails every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestJSONLErrors checks that a write failure and an unencodable entity are both reported.
func TestJSONLErrors(t *testing.T) {
	if err := publish.NewJSONL(failWriter{}).Publish(context.Background(), named("a")); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("write failure: err %v", err)
	}
	nan := entity.Entity{Location: &entity.Location{Position: &entity.Position{LatitudeDegrees: new(math.NaN())}}}
	if err := publish.NewJSONL(&bytes.Buffer{}).Publish(context.Background(), nan); err == nil {
		t.Error("a NaN latitude encoded without error")
	}
}
