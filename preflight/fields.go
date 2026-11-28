package preflight

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"

	lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/erikfailing/tak-interop-gateway/internal/jsonpath"
)

// Errors CheckJSON returns when it can't read a document. Each failure wraps ErrInvalidJSON, and a duplicate key also
// wraps ErrDuplicateKey.
var (
	ErrInvalidJSON  = errors.New("preflight: invalid entity JSON")
	ErrDuplicateKey = errors.New("preflight: duplicate JSON key")
)

// maxDepth bounds how deeply a document may nest. A real entity nests well under 16 levels.
const maxDepth = 64

// entityType is the SDK's Entity, the schema the known-fields rule walks a document against.
var entityType = reflect.TypeFor[lattice.Entity]()

// walker reads one JSON document token by token, following the matching SDK type alongside it.
type walker struct {
	dec      *json.Decoder
	unknown  []finding // keys the SDK doesn't declare, in document order
	miscased []finding // keys that match a declared field only if case is ignored
}

// walkDocument checks that data is exactly one JSON object with no duplicate keys, and returns findings for the keys
// the SDK's Entity type doesn't declare at that place: those it doesn't declare at all, and those that match a
// declared field only if case is ignored. Duplicate keys are an error, not a finding: encoding/json keeps the last
// copy and other decoders keep the first, so the checker and the server could read different entities.
func walkDocument(data []byte) (unknown, miscased []finding, err error) {
	// 1: The document must open with an object.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // numbers are skipped, so there's no reason to convert them
	w := &walker{dec: dec}
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, invalid(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("%w: the document is not a JSON object", ErrInvalidJSON)
	}

	// 2: Walk it, then insist nothing follows.
	if err := w.object("", entityType, 1); err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%w: data after the entity object", ErrInvalidJSON)
	}
	return w.unknown, w.miscased, nil
}

// invalid wraps a decoder error.
func invalid(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF // EOF inside the document means it was cut short
	}
	return fmt.Errorf("%w: %w", ErrInvalidJSON, err)
}

// value walks one JSON value. t is its SDK type, or nil when the schema below this point is unknown, in which case
// only the duplicate-key rule applies.
func (w *walker) value(path string, t reflect.Type, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: nested more than %d levels deep at %s", ErrInvalidJSON, maxDepth, path)
	}
	tok, err := w.dec.Token()
	if err != nil {
		return invalid(err)
	}
	switch tok {
	case json.Delim('{'):
		return w.object(path, t, depth+1)
	case json.Delim('['):
		return w.array(path, t, depth+1)
	}
	return nil // a scalar has no keys to check
}

// object walks the members of an object whose opening brace has been read.
func (w *walker) object(path string, t reflect.Type, depth int) error {
	// 1: Work out what the object's keys should be. A struct names its fields; a map's keys are data.
	t = deref(t)
	var fields map[string]reflect.Type
	var elem reflect.Type
	isStruct := t != nil && t.Kind() == reflect.Struct
	switch {
	case isStruct:
		fields = jsonFields(t)
	case t != nil && t.Kind() == reflect.Map:
		elem = t.Elem()
	}

	// 2: Walk each member. A key repeated exactly is a duplicate anywhere. In a struct, keys that differ only in case
	// are duplicates too, because encoding/json matches field names case-insensitively and would fold them together.
	seen := map[string]bool{}
	for w.dec.More() {
		tok, err := w.dec.Token()
		if err != nil {
			return invalid(err)
		}
		key := tok.(string) // the decoder only yields string keys inside an object
		child := jsonpath.Key(path, key)
		dup := key
		if isStruct {
			dup = foldKey(key)
		}
		if seen[dup] {
			return fmt.Errorf("%w: %w: %s", ErrInvalidJSON, ErrDuplicateKey, child)
		}
		seen[dup] = true

		// 3: Find the member's type. A key that matches a field only if case is ignored is a finding of its own, and is
		// walked as that field, since encoding/json will decode it as one. Any other undeclared key is a finding, and its
		// value is walked without a schema.
		var ct reflect.Type
		switch {
		case isStruct:
			if ft, ok := fields[key]; ok {
				ct = ft
			} else if name, ok := caseMatch(key, fields); ok {
				ct = fields[name]
				w.miscased = append(w.miscased, finding{child, fmt.Sprintf("differs only in case from %q, the field's "+
					"JSON name: encoding/json and the Go SDK read it as that field, but a case-sensitive reader drops it", name)})
			} else {
				w.unknown = append(w.unknown, finding{child, unknownDetail(t, key, fields)})
			}
		case elem != nil:
			ct = elem
		}
		if err := w.value(child, ct, depth); err != nil {
			return err
		}
	}

	// 4: Consume the closing brace.
	if _, err := w.dec.Token(); err != nil {
		return invalid(err)
	}
	return nil
}

// array walks the elements of an array whose opening bracket has been read.
func (w *walker) array(path string, t reflect.Type, depth int) error {
	t = deref(t)
	var elem reflect.Type
	if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
		elem = t.Elem()
	}
	for i := 0; w.dec.More(); i++ {
		if err := w.value(jsonpath.Index(path, i), elem, depth); err != nil {
			return err
		}
	}
	if _, err := w.dec.Token(); err != nil {
		return invalid(err)
	}
	return nil
}

// caseMatch returns the declared field key matches when case is ignored, as encoding/json matches. The names are
// sorted so the answer is the same on every run.
func caseMatch(key string, fields map[string]reflect.Type) (string, bool) {
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if foldKey(name) == foldKey(key) {
			return name, true
		}
	}
	return "", false
}

// unknownDetail explains an undeclared key, suggesting the declared field it most likely meant.
func unknownDetail(t reflect.Type, key string, fields map[string]reflect.Type) string {
	// 1: The commonest mistake left, once case is ruled out, is a snake_case name, as the Python SDK and the guide's
	// examples spell fields. The names are sorted so the suggestion is the same on every run.
	names := slices.Sorted(maps.Keys(fields))
	squashed := strings.ReplaceAll(foldKey(key), "_", "")
	for _, name := range names {
		if foldKey(name) == squashed {
			return fmt.Sprintf("is not a JSON name the SDK declares; the REST API spells this field %q", name)
		}
	}

	// 2: Otherwise it's simply not a field there.
	return fmt.Sprintf("is not a field of %s in lattice-sdk-go %s, so it would be dropped", t.Name(), SDKVersion)
}

// fieldCache maps each SDK struct type to its JSON field names, computed once per type.
var fieldCache sync.Map // reflect.Type to map[string]reflect.Type

// jsonFields returns the JSON names of a struct's fields, following encoding/json's rules for the cases the SDK uses:
// the name in the json tag (or the Go name), "-" skipped, unexported fields skipped, and embedded structs' fields
// promoted unless an outer field has the same name.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	if m, ok := fieldCache.Load(t); ok {
		return m.(map[string]reflect.Type)
	}
	m := map[string]reflect.Type{}
	var outer []reflect.StructField
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
			continue
		case f.Anonymous && name == "":
			// 1: Promote an embedded struct's fields first, so the outer struct's own fields win a name clash.
			if et := deref(f.Type); et.Kind() == reflect.Struct {
				maps.Copy(m, jsonFields(et))
			}
		case f.IsExported():
			outer = append(outer, f)
		}
	}
	for _, f := range outer {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		m[name] = f.Type
	}
	fieldCache.Store(t, m)
	return m
}

// deref strips pointers from t; it returns nil for nil.
func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// foldKey maps s to a canonical form shared by every string strings.EqualFold considers equal, so case-insensitive
// duplicates can be found with a map instead of comparing every pair of keys.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			lowest = min(lowest, f)
		}
		b.WriteRune(lowest)
	}
	return b.String()
}
