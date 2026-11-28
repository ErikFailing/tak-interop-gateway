package readback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
)

// Client publishes an entity and reads one back.
type Client interface {
	Publish(ctx context.Context, e entity.Entity) error
	// Get returns the entity as JSON in the REST API's shape.
	Get(ctx context.Context, id string) ([]byte, error)
}

// Kind says how one field differs.
type Kind int

const (
	// Dropped: sent, but missing from what came back.
	Dropped Kind = iota
	// Changed: in both, with different values.
	Changed
	// Added: not sent, but present in what came back, such as a createdTime the server filled in. Added fields are
	// reported but don't fail a readback: the platform is allowed to add.
	Added
)

// String returns "dropped", "changed" or "added".
func (k Kind) String() string {
	switch k {
	case Dropped:
		return "dropped"
	case Changed:
		return "changed"
	default:
		return "added"
	}
}

// Difference is one field that came back different.
type Difference struct {
	Path string // the JSON path, such as "symbology.milStd2525C.sidc"
	Kind Kind
	Sent string // the value sent, as JSON; empty for Added
	Got  string // the value read back, as JSON; empty for Dropped
}

// String formats the difference for a report.
func (d Difference) String() string {
	switch d.Kind {
	case Dropped:
		return fmt.Sprintf("dropped %s (sent %s)", d.Path, d.Sent)
	case Changed:
		return fmt.Sprintf("changed %s: sent %s, got %s", d.Path, d.Sent, d.Got)
	default:
		return fmt.Sprintf("added %s: %s", d.Path, d.Got)
	}
}

// Diff compares two entities given as JSON objects, field by field. Two timestamps that name the same instant are
// equal however they are written, and so are two numbers with the same value, because a server may reformat both.
// Differences come in a fixed order: depth first, keys sorted.
func Diff(sent, got []byte) ([]Difference, error) {
	a, err := decode(sent)
	if err != nil {
		return nil, fmt.Errorf("readback: the sent entity: %w", err)
	}
	b, err := decode(got)
	if err != nil {
		return nil, fmt.Errorf("readback: the entity read back: %w", err)
	}
	var out []Difference
	compare("", a, b, &out)
	return out, nil
}

// decode reads one JSON object, keeping numbers exact.
func decode(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON object")
	}
	return m, nil
}

// compare appends the differences between a and b, found at path.
func compare(path string, a, b any, out *[]Difference) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, Difference{Path: path, Kind: Changed, Sent: show(a), Got: show(b)})
			return
		}
		for _, k := range slices.Sorted(maps.Keys(av)) {
			if bval, ok := bv[k]; ok {
				compare(join(path, k), av[k], bval, out)
			} else {
				*out = append(*out, Difference{Path: join(path, k), Kind: Dropped, Sent: show(av[k])})
			}
		}
		for _, k := range slices.Sorted(maps.Keys(bv)) {
			if _, ok := av[k]; !ok {
				*out = append(*out, Difference{Path: join(path, k), Kind: Added, Got: show(bv[k])})
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, Difference{Path: path, Kind: Changed, Sent: show(a), Got: show(b)})
			return
		}
		for i := range max(len(av), len(bv)) {
			p := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i >= len(bv):
				*out = append(*out, Difference{Path: p, Kind: Dropped, Sent: show(av[i])})
			case i >= len(av):
				*out = append(*out, Difference{Path: p, Kind: Added, Got: show(bv[i])})
			default:
				compare(p, av[i], bv[i], out)
			}
		}
	default:
		if !sameScalar(a, b) {
			*out = append(*out, Difference{Path: path, Kind: Changed, Sent: show(a), Got: show(b)})
		}
	}
}

// sameScalar compares two JSON scalars: numbers by value, strings exactly or as equal RFC 3339 instants, and booleans
// and null directly.
func sameScalar(a, b any) bool {
	switch av := a.(type) {
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			return false
		}
		fa, errA := av.Float64()
		fb, errB := bv.Float64()
		if errA == nil && errB == nil {
			return fa == fb
		}
		return av == bv
	case string:
		bv, ok := b.(string)
		if !ok {
			return false
		}
		if av == bv {
			return true
		}
		ta, errA := time.Parse(time.RFC3339Nano, av)
		tb, errB := time.Parse(time.RFC3339Nano, bv)
		return errA == nil && errB == nil && ta.Equal(tb)
	}
	return a == b
}

// show renders a value as compact JSON for a report.
func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// join appends key to a JSON path: dotted for a plain key, and quoted in brackets for any other, so every path is
// non-empty and reads back unambiguously.
func join(path, key string) string {
	plain := key != ""
	for i := range len(key) {
		if b := key[i]; b != '_' && (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') {
			plain = false
		}
	}
	switch {
	case !plain:
		return path + "[" + strconv.Quote(key) + "]"
	case path == "":
		return key
	}
	return path + "." + key
}

// Result is the readback of one entity.
type Result struct {
	EntityID    string
	Name        string       // the display name, for reports
	Differences []Difference // empty when the entity came back exactly as sent
	Err         error        // set when publishing or reading back failed; then there are no Differences
}

// Passed reports whether the entity was published, read back, and came back with nothing dropped or changed.
func (r Result) Passed() bool {
	if r.Err != nil {
		return false
	}
	for _, d := range r.Differences {
		if d.Kind != Added {
			return false
		}
	}
	return true
}

// Run publishes every entity, waits, then reads each one back and compares it with what was sent. The wait gives a
// real platform time to settle; the read happens in a separate request, so the answer reflects what was stored, not
// the publish call's echo. The context bounds the whole run. Every entity must have an entityId.
func Run(ctx context.Context, c Client, entities []entity.Entity, wait time.Duration) []Result {
	// 1: Publish everything first, so one entity's read doesn't race the next one's publish.
	results := make([]Result, len(entities))
	sent := make([][]byte, len(entities))
	for i, e := range entities {
		r := &results[i]
		if e.EntityID != nil {
			r.EntityID = *e.EntityID
		}
		if e.Aliases != nil && e.Aliases.Name != nil {
			r.Name = *e.Aliases.Name
		}
		b, err := json.Marshal(e)
		switch {
		case r.EntityID == "":
			r.Err = errors.New("readback: the entity has no entityId")
		case err != nil:
			r.Err = fmt.Errorf("readback: encode: %w", err)
		default:
			sent[i] = b
			if err := c.Publish(ctx, e); err != nil {
				r.Err = fmt.Errorf("readback: publish: %w", err)
			}
		}
	}

	// 2: Wait, unless the run is cancelled first.
	select {
	case <-time.After(wait):
	case <-ctx.Done():
	}

	// 3: Read back and compare each entity that was published.
	for i := range results {
		r := &results[i]
		if r.Err != nil {
			continue
		}
		got, err := c.Get(ctx, r.EntityID)
		if err != nil {
			r.Err = fmt.Errorf("readback: read back: %w", err)
			continue
		}
		if r.Differences, err = Diff(sent[i], got); err != nil {
			r.Err = err
		}
	}
	return results
}
