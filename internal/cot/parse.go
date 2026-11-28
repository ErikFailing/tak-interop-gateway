package cot

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"time"
	"unicode"
)

// Errors returned by Parse. Each failure wraps one of these, so callers can test with errors.Is and still get the
// detail in the message.
var (
	ErrTooLarge         = errors.New("cot: message too large")
	ErrTooDeep          = errors.New("cot: elements nested too deeply")
	ErrTooManyElements  = errors.New("cot: too many elements")
	ErrMalformed        = errors.New("cot: malformed XML")
	ErrDirective        = errors.New("cot: XML directives (such as DOCTYPE) are not allowed")
	ErrProcInst         = errors.New("cot: unexpected processing instruction")
	ErrNotEvent         = errors.New("cot: root element must be an unnamespaced <event>")
	ErrStrayData        = errors.New("cot: data outside the <event> element")
	ErrDuplicateAttr    = errors.New("cot: duplicate attribute")
	ErrDuplicateElement = errors.New("cot: duplicate element")
	ErrMissing          = errors.New("cot: required attribute or element missing")
	ErrInvalidField     = errors.New("cot: invalid field value")
)

// Limits bounds how much work Parse does on one message, so a hostile or broken sender can't make it do a lot. A field
// left at zero, or set below it, takes its value from DefaultLimits, so Limits{MaxBytes: 1 << 20} raises the size
// limit and keeps the others.
type Limits struct {
	MaxBytes    int // the largest message accepted
	MaxDepth    int // the deepest element nesting accepted, counting <event> as 1
	MaxElements int // the most elements accepted in one message
}

// orDefaults fills every unset field from DefaultLimits.
func (lim Limits) orDefaults() Limits {
	if lim.MaxBytes <= 0 {
		lim.MaxBytes = DefaultLimits.MaxBytes
	}
	if lim.MaxDepth <= 0 {
		lim.MaxDepth = DefaultLimits.MaxDepth
	}
	if lim.MaxElements <= 0 {
		lim.MaxElements = DefaultLimits.MaxElements
	}
	return lim
}

// DefaultLimits are generous for real CoT. A typical position report is under 1 KiB, and the element cap leaves room
// for routes and shapes with many <link> points.
var DefaultLimits = Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxElements: 2048}

// typeGrammar matches CoT type and how strings: alphanumeric atoms joined by single hyphens, such as a-f-G-U-C or
// h-g-i-g-o.
var typeGrammar = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// Size and range bounds for validated fields. They are far outside anything real data uses, and they keep a single
// field from ballooning when it is re-marshaled.
const (
	maxIDLen       = 256
	maxTypeLen     = 64
	maxMeasurement = 1e8 // meters, for hae, ce and le; well above the 9999999 "unknown" sentinel
	maxSpeed       = 1e5 // meters per second
)

// eventAttrs and pointAttrs are the attributes CoT requires on <event> and <point>.
var (
	eventAttrs = []string{"version", "uid", "type", "time", "start", "stale", "how"}
	pointAttrs = []string{"lat", "lon", "hae", "ce", "le"}
)

// singletons are the elements that may appear at most once under each parent. They are the ones the model decodes
// into a single field: given two, xml.Unmarshal silently keeps the last while other parsers commonly keep the first,
// so two systems reading the same message would disagree about what it says.
var singletons = map[string]map[string]bool{
	"event":  {"point": true, "detail": true},
	"detail": {"contact": true, "__group": true, "track": true, "remarks": true},
}

// Parse parses one CoT event with DefaultLimits.
func Parse(data []byte) (Event, error) {
	return DefaultLimits.Parse(data)
}

// Parse parses one CoT event within these limits. It accepts only a single, well-formed, UTF-8 <event> document with
// the required attributes and sane field values, and returns an error wrapping one of the Err values otherwise.
func (lim Limits) Parse(data []byte) (Event, error) {
	// 1: Check the size before doing any parsing work at all.
	lim = lim.orDefaults()
	if len(data) > lim.MaxBytes {
		return Event{}, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, len(data), lim.MaxBytes)
	}
	// 2: Scan the tokens to enforce the structure: limits, a single <event> root, no directives, no duplicates.
	if err := lim.scan(data); err != nil {
		return Event{}, err
	}
	// 3: The structure is known to be safe and as expected, so decode it into the model. A number or time the model's
	// own types refuse is an invalid field; anything else the decoder refuses is malformed.
	var ev Event
	if err := xml.Unmarshal(data, &ev); err != nil {
		if errors.Is(err, ErrInvalidField) {
			return Event{}, err
		}
		return Event{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	// 4: Check the decoded values.
	if err := ev.validate(); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// scan walks the document token by token and enforces everything about its shape. encoding/xml leaves several of
// these to the caller: it does not reject duplicate attributes, and xml.Unmarshal stops after the first element and
// ignores anything that follows, so a document with two events would otherwise parse as the first one.
func (lim Limits) scan(data []byte) error {
	// CharsetReader is left nil, so a document declaring a non-UTF-8 encoding fails instead of being transcoded.
	d := xml.NewDecoder(bytes.NewReader(data))
	depth, elements := 0, 0
	sawRoot, rootClosed, sawXMLDecl, sawPoint := false, false, false, false
	// path holds the names of the open elements, and seen[i] the singleton children already met under path[i].
	var path []string
	var seen []map[string]bool
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrMalformed, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if rootClosed {
				return fmt.Errorf("%w: a second element <%s>", ErrStrayData, t.Name.Local)
			}
			depth++
			elements++
			if depth > lim.MaxDepth {
				return fmt.Errorf("%w: depth %d, limit %d", ErrTooDeep, depth, lim.MaxDepth)
			}
			if elements > lim.MaxElements {
				return fmt.Errorf("%w: limit %d", ErrTooManyElements, lim.MaxElements)
			}
			if err := uniqueAttrs(t); err != nil {
				return err
			}
			if depth == 1 {
				if t.Name.Local != "event" || t.Name.Space != "" {
					return fmt.Errorf("%w: got <%s>", ErrNotEvent, t.Name.Local)
				}
				sawRoot = true
				if err := requireAttrs(t, eventAttrs); err != nil {
					return err
				}
			}
			if depth >= 2 {
				parent := path[len(path)-1]
				if singletons[parent][t.Name.Local] {
					if seen[len(seen)-1][t.Name.Local] {
						return fmt.Errorf("%w: a second <%s> in <%s>", ErrDuplicateElement, t.Name.Local, parent)
					}
					seen[len(seen)-1][t.Name.Local] = true
				}
			}
			if depth == 2 && t.Name.Local == "point" {
				sawPoint = true
				if err := requireAttrs(t, pointAttrs); err != nil {
					return err
				}
			}
			path = append(path, t.Name.Local)
			seen = append(seen, map[string]bool{})
		case xml.EndElement:
			depth--
			path = path[:len(path)-1]
			seen = seen[:len(seen)-1]
			if depth == 0 {
				rootClosed = true
			}
		case xml.CharData:
			if depth == 0 && !IsXMLSpace(t) {
				return fmt.Errorf("%w: text outside the <event> element", ErrStrayData)
			}
		case xml.Directive:
			return ErrDirective
		case xml.ProcInst:
			// Only the XML declaration, once, before the root.
			if t.Target != "xml" || sawRoot || sawXMLDecl {
				return fmt.Errorf("%w: <?%s?>", ErrProcInst, t.Target)
			}
			sawXMLDecl = true
		case xml.Comment:
			// Comments are harmless and allowed anywhere.
		}
	}
	if !sawRoot {
		return fmt.Errorf("%w: no element found", ErrNotEvent)
	}
	if !sawPoint {
		return fmt.Errorf("%w: <point>", ErrMissing)
	}
	return nil
}

// modelAttrs are the attributes the model decodes, by element. xml.Unmarshal matches an attribute by its local name
// alone, in any namespace, including the xmlns prefix of a namespace declaration, so xmlns:uid="b" on <event> would
// fill UID while a namespace-aware reader sees no uid attribute there at all.
var modelAttrs = map[string]map[string]bool{
	"event":   {"version": true, "uid": true, "type": true, "time": true, "start": true, "stale": true, "how": true},
	"point":   {"lat": true, "lon": true, "hae": true, "ce": true, "le": true},
	"contact": {"callsign": true},
	"__group": {"name": true, "role": true},
	"track":   {"course": true, "speed": true},
}

// uniqueAttrs rejects an element that repeats an attribute's local name, and, on an element the model decodes, any
// namespaced attribute (a namespace declaration included) whose local name the model reads. Either way two parsers
// could read different values: xml.Unmarshal keeps the last of uid="a" and x:uid="b", and takes xmlns:uid="b" for a uid.
func uniqueAttrs(t xml.StartElement) error {
	seen := make(map[string]bool, len(t.Attr))
	for _, a := range t.Attr {
		if a.Name.Space == "" && a.Name.Local == "xmlns" {
			continue // the default namespace declaration: not data, and no attribute shares its name
		}
		if a.Name.Space != "" && modelAttrs[t.Name.Local][a.Name.Local] {
			return fmt.Errorf("%w: %s:%s on <%s> could be read as the CoT attribute %q", ErrDuplicateAttr,
				a.Name.Space, a.Name.Local, t.Name.Local, a.Name.Local)
		}
		if seen[a.Name.Local] {
			return fmt.Errorf("%w: %q on <%s>", ErrDuplicateAttr, a.Name.Local, t.Name.Local)
		}
		seen[a.Name.Local] = true
	}
	return nil
}

// requireAttrs checks that every named attribute is present, unnamespaced. Presence has to be checked here, on the raw
// tokens, because after decoding a missing lat looks exactly like lat="0".
func requireAttrs(t xml.StartElement, names []string) error {
	for _, name := range names {
		found := false
		for _, a := range t.Attr {
			if a.Name.Space == "" && a.Name.Local == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %q on <%s>", ErrMissing, name, t.Name.Local)
		}
	}
	return nil
}

// xmlSpace is the whitespace XML allows outside the root element: space, tab, carriage return and line feed. Unicode
// spaces such as a no-break space are not whitespace to XML, so text made of them is data.
const xmlSpace = " \t\r\n"

// IsXMLSpace reports whether b is empty or only XML whitespace.
func IsXMLSpace(b []byte) bool {
	return len(bytes.Trim(b, xmlSpace)) == 0
}

// minYear and maxYear bound the years a timestamp may name, in UTC: the four-digit years RFC 3339 can write. A time
// outside them parses, but marshals to text no RFC 3339 parser accepts, so it couldn't be passed on.
const (
	minYear = 1
	maxYear = 9999
)

// checkTime rejects a timestamp whose UTC year RFC 3339 can't write.
func checkTime(name string, t Timestamp) error {
	if y := time.Time(t).UTC().Year(); y < minYear || y > maxYear {
		return fmt.Errorf("%w: %s is in the year %d, outside %d to %d", ErrInvalidField, name, y, minYear, maxYear)
	}
	return nil
}

// validate checks the decoded values. Structure was already enforced by scan.
func (e Event) validate() error {
	if err := checkText("version", e.Version, 16, true); err != nil {
		return err
	}
	if err := checkText("uid", e.UID, maxIDLen, true); err != nil {
		return err
	}
	for _, f := range []struct{ name, value string }{{"type", e.Type}, {"how", e.How}} {
		if len(f.value) > maxTypeLen || !typeGrammar.MatchString(f.value) {
			return fmt.Errorf("%w: %s %q", ErrInvalidField, f.name, f.value)
		}
	}
	for _, f := range []struct {
		name string
		t    Timestamp
	}{{"time", e.Time}, {"start", e.Start}, {"stale", e.Stale}} {
		if err := checkTime(f.name, f.t); err != nil {
			return err
		}
	}
	if time.Time(e.Stale).Before(time.Time(e.Start)) {
		return fmt.Errorf("%w: stale is before start", ErrInvalidField)
	}
	p := e.Point
	if err := checkRange("lat", float64(p.Lat), -90, 90); err != nil {
		return err
	}
	if err := checkRange("lon", float64(p.Lon), -180, 180); err != nil {
		return err
	}
	if err := checkRange("hae", float64(p.HAE), -maxMeasurement, maxMeasurement); err != nil {
		return err
	}
	if err := checkRange("ce", float64(p.CE), 0, maxMeasurement); err != nil {
		return err
	}
	if err := checkRange("le", float64(p.LE), 0, maxMeasurement); err != nil {
		return err
	}
	if e.Detail == nil {
		return nil
	}
	if c := e.Detail.Contact; c != nil {
		if err := checkText("callsign", c.Callsign, maxIDLen, false); err != nil {
			return err
		}
	}
	if tr := e.Detail.Track; tr != nil {
		if err := checkRange("course", float64(tr.Course), 0, 360); err != nil {
			return err
		}
		if err := checkRange("speed", float64(tr.Speed), 0, maxSpeed); err != nil {
			return err
		}
	}
	return nil
}

// checkRange rejects NaN, the infinities and anything outside [lo, hi].
func checkRange(name string, v, lo, hi float64) error {
	if math.IsNaN(v) || v < lo || v > hi {
		return fmt.Errorf("%w: %s %v not in [%v, %v]", ErrInvalidField, name, v, lo, hi)
	}
	return nil
}

// checkText rejects control characters and over-long values, and an empty value when required is true.
func checkText(name, v string, maxLen int, required bool) error {
	if required && v == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidField, name)
	}
	if len(v) > maxLen {
		return fmt.Errorf("%w: %s is %d bytes, limit %d", ErrInvalidField, name, len(v), maxLen)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidField, name)
		}
	}
	return nil
}
