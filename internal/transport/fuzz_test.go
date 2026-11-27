package transport_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/transport"
)

// kind names which class of error ended a stream, so runs can be compared without comparing error text.
func kind(err error) string {
	switch {
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, transport.ErrFrameTooLarge):
		return "too large"
	case errors.Is(err, transport.ErrTruncated):
		return "truncated"
	case errors.Is(err, transport.ErrMalformedStream):
		return "malformed"
	default:
		return "other: " + err.Error()
	}
}

// FuzzFramer checks the framer against four properties for any input:
//
//  1. Chunking doesn't matter: the same bytes delivered whole, a byte at a time, in halves, or with EOF attached to
//     the last data give the same frames and the same ending.
//  2. Frames are the input: joined in order they are a prefix of the stream, so nothing is invented or reordered.
//  3. No frame is larger than the limit.
//  4. Framer and parser agree: any frame cot.Parse accepts, framed again on its own, comes back as exactly itself.
//
// Run it with: go test -fuzz=FuzzFramer ./internal/transport
func FuzzFramer(f *testing.F) {
	events := generated(f, 4)
	var stream bytes.Buffer
	for i, ev := range events {
		stream.WriteString(separators[i%len(separators)])
		stream.Write(ev)
	}
	f.Add(stream.Bytes())
	f.Add(events[0])
	f.Add(append(append([]byte{}, events[0]...), []byte(" stray ")...))
	f.Add(events[0][:len(events[0])/2])
	f.Add([]byte(`<event uid="x"/><!-- </event> --><event><remarks><![CDATA[</event>]]></remarks></event>`))
	f.Add([]byte(""))

	// A small limit lets the fuzzer reach the size-limit path without huge inputs.
	const maxBytes = 4096
	f.Fuzz(func(t *testing.T, data []byte) {
		whole, wholeErr := collect(bytes.NewReader(data), maxBytes)
		for name, chunk := range chunkers {
			got, err := collect(chunk(bytes.NewReader(data)), maxBytes)
			if kind(err) != kind(wholeErr) || len(got) != len(whole) {
				t.Fatalf("%s: %d frames ending %q, whole: %d frames ending %q", name, len(got), kind(err), len(whole), kind(wholeErr))
			}
			for i := range got {
				if !bytes.Equal(got[i], whole[i]) {
					t.Fatalf("%s: frame %d differs\n got: %q\nwant: %q", name, i, got[i], whole[i])
				}
			}
		}
		if joined := bytes.Join(whole, nil); !bytes.HasPrefix(data, joined) {
			t.Fatalf("frames are not a prefix of the input\ninput:  %q\nframes: %q", data, joined)
		}
		for i, fr := range whole {
			if len(fr) > maxBytes {
				t.Fatalf("frame %d is %d bytes, over the %d limit", i, len(fr), maxBytes)
			}
			if _, err := cot.Parse(fr); err != nil {
				continue
			}
			again, err := collect(bytes.NewReader(fr), maxBytes)
			if !errors.Is(err, io.EOF) || len(again) != 1 || !bytes.Equal(again[0], fr) {
				t.Fatalf("a frame the parser accepts does not frame back to itself: %d frames, %v\nframe: %q", len(again), err, fr)
			}
		}
	})
}
