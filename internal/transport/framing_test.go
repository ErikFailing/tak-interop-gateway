package transport_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// limit is the frame size used throughout, matching the parser's default.
var limit = cot.DefaultLimits.MaxBytes

// chunkers are ways of delivering the same bytes in different read sizes, the way TCP can split a stream anywhere.
var chunkers = map[string]func(io.Reader) io.Reader{
	"whole":    func(r io.Reader) io.Reader { return r },
	"one byte": iotest.OneByteReader,
	"half":     iotest.HalfReader,
	"data+eof": iotest.DataErrReader,
}

// collect reads frames until Next returns an error, and returns the frames and that error.
func collect(r io.Reader, maxBytes int) ([][]byte, error) {
	f := transport.NewFramer(r, maxBytes)
	var frames [][]byte
	for {
		fr, err := f.Next()
		if err != nil {
			return frames, err
		}
		frames = append(frames, fr)
	}
}

// generated returns n marshaled events from a fixed cot-gen scenario.
func generated(t testing.TB, n int) [][]byte {
	t.Helper()
	s, err := cotgen.New(cotgen.Config{
		Units: 3, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836},
		Start: time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC), Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for len(out) < n {
		for _, ev := range s.Next() {
			b, err := xml.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, b)
		}
	}
	return out[:n]
}

// separators are what real streams put between events.
var separators = []string{"", "\n", "\r\n\t ", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`, "<!-- next -->\n"}

// TestRecoversEveryEvent joins events with each kind of separator, delivers them with each chunker, and checks that
// exactly the original events come back, in order, and each one parses.
func TestRecoversEveryEvent(t *testing.T) {
	events := generated(t, 8)
	for _, sep := range separators {
		var stream bytes.Buffer
		for _, ev := range events {
			stream.WriteString(sep)
			stream.Write(ev)
		}
		stream.WriteString("\n")
		for name, chunk := range chunkers {
			t.Run(fmt.Sprintf("sep %q, %s", sep, name), func(t *testing.T) {
				frames, err := collect(chunk(bytes.NewReader(stream.Bytes())), limit)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("stream ended with %v, want io.EOF", err)
				}
				if len(frames) != len(events) {
					t.Fatalf("got %d frames, want %d", len(frames), len(events))
				}
				for i, fr := range frames {
					want := sep + string(events[i])
					if string(fr) != want {
						t.Errorf("frame %d = %q, want %q", i, fr, want)
					}
					if _, err := cot.Parse(fr); err != nil {
						t.Errorf("frame %d does not parse: %v", i, err)
					}
				}
			})
		}
	}
}

// TestRandomConcatenations is a seeded property test: random numbers of events, random separators, random chunking.
func TestRandomConcatenations(t *testing.T) {
	all := generated(t, 12)
	rng := rand.New(rand.NewPCG(7, 7))
	names := []string{"whole", "one byte", "half", "data+eof"}
	for trial := range 300 {
		n := 1 + rng.IntN(len(all))
		var stream bytes.Buffer
		for _, ev := range all[:n] {
			stream.WriteString(separators[rng.IntN(len(separators))])
			stream.Write(ev)
		}
		chunk := chunkers[names[rng.IntN(len(names))]]
		frames, err := collect(chunk(bytes.NewReader(stream.Bytes())), limit)
		if !errors.Is(err, io.EOF) || len(frames) != n {
			t.Fatalf("trial %d: %d frames and %v, want %d frames and io.EOF", trial, len(frames), err, n)
		}
		if !bytes.Equal(bytes.Join(frames, nil), stream.Bytes()) {
			t.Fatalf("trial %d: frames don't add back up to the stream", trial)
		}
	}
}

// TestBoundariesNeedTheTokenizer covers the inputs where searching for "</event>" would cut in the wrong place.
func TestBoundariesNeedTheTokenizer(t *testing.T) {
	base := string(generated(t, 1)[0])
	inDetail := func(s string) string { return strings.Replace(base, "<detail>", "<detail>"+s, 1) }
	tests := []struct {
		name  string
		event string
	}{
		{"</event> inside a comment", inDetail(`<!-- </event> -->`)},
		{"</event> inside CDATA", strings.Replace(base, "<remarks>", "<remarks><![CDATA[</event>]]>", 1)},
		{"a nested <event> element", inDetail(`<event uid="inner"><point/></event>`)},
		{"/> and > inside an attribute value", strings.Replace(base, `callsign="SIM-SAR-1"`, `callsign="a/>b>c"`, 1)},
		{"a self-closing root", `<event uid="x"/>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := tt.event + "\n" + base
			frames, err := collect(iotest.OneByteReader(strings.NewReader(stream)), limit)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v", err)
			}
			if len(frames) != 2 || string(frames[0]) != tt.event || string(frames[1]) != "\n"+base {
				t.Fatalf("got %d frames %q", len(frames), frames)
			}
		})
	}
}

// TestStrayTextIsItsOwnFrame checks that junk between events costs only the junk.
func TestStrayTextIsItsOwnFrame(t *testing.T) {
	events := generated(t, 2)
	stream := string(events[0]) + " not xml " + string(events[1])
	frames, err := collect(strings.NewReader(stream), limit)
	if !errors.Is(err, io.EOF) || len(frames) != 3 {
		t.Fatalf("got %d frames and %v, want 3 and io.EOF", len(frames), err)
	}
	if _, err := cot.Parse(frames[1]); err == nil {
		t.Error("the stray text frame parsed as an event")
	}
	for _, i := range []int{0, 2} {
		if _, err := cot.Parse(frames[i]); err != nil {
			t.Errorf("frame %d should parse: %v", i, err)
		}
	}
}

func TestStreamErrors(t *testing.T) {
	ev := string(generated(t, 1)[0])
	tests := []struct {
		name   string
		stream string
		max    int
		want   error
	}{
		{"empty stream", "", limit, io.EOF},
		{"only whitespace", " \n\t", limit, io.EOF},
		{"too large", ev, 100, transport.ErrFrameTooLarge},
		{"cut off mid-event", ev[:len(ev)-10], limit, transport.ErrTruncated},
		{"stray end tag", "</event>", limit, transport.ErrMalformedStream},
		{"mismatched end tag", `<event><point></event>`, limit, transport.ErrMalformedStream},
		{"non-UTF-8 encoding", `<?xml version="1.0" encoding="ISO-8859-1"?>` + ev, limit, transport.ErrMalformedStream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := transport.NewFramer(strings.NewReader(tt.stream), tt.max)
			_, err := f.Next()
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if _, again := f.Next(); !errors.Is(again, tt.want) {
				t.Errorf("error is not sticky: second call returned %v", again)
			}
		})
	}
}

// TestReadErrorsPassThrough checks that a failing connection is reported as itself, after the frames before it.
func TestReadErrorsPassThrough(t *testing.T) {
	boom := errors.New("connection reset")
	ev := string(generated(t, 1)[0])
	r := io.MultiReader(strings.NewReader(ev), iotest.ErrReader(boom))
	frames, err := collect(r, limit)
	if len(frames) != 1 {
		t.Errorf("got %d frames before the error, want 1", len(frames))
	}
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want an error wrapping %v", err, boom)
	}
	if errors.Is(err, transport.ErrMalformedStream) {
		t.Error("a read failure was reported as a malformed stream")
	}
}
