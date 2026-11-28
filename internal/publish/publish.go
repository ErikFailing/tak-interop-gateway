// Package publish defines the Publisher the pipeline sends entities through, and the adapters behind it. They are
// the only code that hands entities to the outside world: Memory keeps them for tests, JSONL writes them out for
// offline runs (and for `takgw preflight` to check), and the SDK adapter in internal/publish/latticesdk sends them to
// Lattice.
package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/erikfailing/tak-interop-gateway/entity"
)

// Publisher sends one entity. An error means the entity was not delivered, so the caller must not record it as sent.
type Publisher interface {
	Publish(ctx context.Context, e entity.Entity) error
}

// Memory keeps every entity it is given, in order. It is the in-process fake: tests read back exactly what the
// pipeline would have sent. It is safe for concurrent use.
type Memory struct {
	mu       sync.Mutex
	entities []entity.Entity
}

// Publish appends e. It never fails.
func (m *Memory) Publish(_ context.Context, e entity.Entity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entities = append(m.entities, e)
	return nil
}

// Entities returns everything published so far, in order. The slice is a copy, but the entities in it share their
// pointer fields with what was published, so a caller must treat them as read-only.
func (m *Memory) Entities() []entity.Entity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]entity.Entity(nil), m.entities...)
}

// JSONL writes each entity as one line of JSON in the REST API's shape, the format `takgw preflight` reads. Each
// entity is written with a single Write call as soon as it is published, so a reader on a pipe sees it at once. It is
// safe for concurrent use.
type JSONL struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONL returns a JSONL publisher that writes to w.
func NewJSONL(w io.Writer) *JSONL {
	return &JSONL{w: w}
}

// Publish writes e and a newline.
func (j *JSONL) Publish(_ context.Context, e entity.Entity) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("publish: encode entity: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("publish: write: %w", err)
	}
	return nil
}
