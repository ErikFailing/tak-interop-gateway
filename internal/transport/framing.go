package transport

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

// Errors returned by Framer.Next. All three end the stream: once one is returned, every later call returns it again,
// because there is no safe way to guess where the next message starts after the tokenizer has lost its place.
var (
	ErrFrameTooLarge   = errors.New("transport: CoT message exceeds the size limit")
	ErrMalformedStream = errors.New("transport: malformed CoT stream")
	ErrTruncated       = errors.New("transport: stream ended in the middle of a message")
)

// Framer splits a stream of back-to-back CoT events, as a TAK server sends them over TCP, into one message per event.
//
// It finds the boundaries with encoding/xml's tokenizer, the same one cot.Parse uses, and tracks element depth: a
// frame ends when the root element closes. Searching the bytes for "</event>" would be wrong, because that text can
// sit inside a comment or a CDATA section, and an <event> element can be nested inside another one's detail. Sharing a
// tokenizer with the parser also means the two can't disagree about where an element ends.
//
// Each frame is the exact bytes of one root element, plus anything that came before it since the previous frame
// (whitespace, an XML declaration, comments). Non-whitespace text between root elements is returned as a frame of its
// own, which cot.Parse rejects, so one bad message doesn't take the next good one down with it.
//
// Framer does no validation beyond what tokenizing needs. Every frame still has to go through cot.Parse.
type Framer struct {
	rec   *recorder
	dec   *xml.Decoder
	start int64 // stream offset where the current frame began
	err   error // the first fatal error, returned forever after
}

// NewFramer reads from r and returns frames of at most maxBytes each.
func NewFramer(r io.Reader, maxBytes int) *Framer {
	rec := &recorder{r: bufio.NewReader(r), max: maxBytes}
	// rec implements io.ByteReader, so the decoder reads it byte by byte with no buffering of its own, and every
	// byte it consumes passes through rec. Strict stays on and CharsetReader stays nil, as in cot.Parse.
	return &Framer{rec: rec, dec: xml.NewDecoder(rec)}
}

// Next returns the next frame. At the end of a clean stream it returns io.EOF. After a fatal error it returns that
// error on every call.
func (f *Framer) Next() ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	depth := 0
	for {
		tok, err := f.dec.Token()
		if err != nil {
			f.err = f.classify(err)
			return nil, f.err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
			if depth == 0 {
				return f.cut(), nil
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) > 0 {
				return f.cut(), nil
			}
		}
	}
}

// cut returns the bytes from the start of the current frame up to the decoder's position, and keeps whatever the
// decoder read past that point (it can read one byte ahead) as the start of the next frame.
func (f *Framer) cut() []byte {
	end := f.dec.InputOffset()
	n := int(end - f.start)
	frame := make([]byte, n)
	copy(frame, f.rec.buf[:n])
	f.rec.buf = f.rec.buf[:copy(f.rec.buf, f.rec.buf[n:])]
	f.start = end
	return frame
}

// classify turns a decoder error into the error Next reports.
func (f *Framer) classify(err error) error {
	switch {
	case errors.Is(err, ErrFrameTooLarge):
		return fmt.Errorf("%w: limit %d bytes", ErrFrameTooLarge, f.rec.max)
	case f.rec.readErr != nil:
		// The underlying reader failed (a reset connection, a timeout): report that, not a format problem.
		return fmt.Errorf("transport: read: %w", f.rec.readErr)
	case errors.Is(err, io.EOF):
		// The decoder only reports a bare EOF between tokens at the top level, which is a clean end.
		return io.EOF
	case f.rec.eof:
		// Any other error once the reader has hit EOF means the stream stopped partway through a message.
		return fmt.Errorf("%w: %w", ErrTruncated, err)
	default:
		return fmt.Errorf("%w: %w", ErrMalformedStream, err)
	}
}

// recorder hands the decoder one byte at a time and keeps a copy of everything read since the current frame began,
// refusing to read past the frame size limit.
type recorder struct {
	r       *bufio.Reader
	buf     []byte
	max     int
	eof     bool  // the reader reported a clean end
	readErr error // the reader failed with something other than EOF
}

// ReadByte implements io.ByteReader.
func (rc *recorder) ReadByte() (byte, error) {
	if len(rc.buf) >= rc.max {
		return 0, ErrFrameTooLarge
	}
	b, err := rc.r.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			rc.eof = true
		} else {
			rc.readErr = err
		}
		return 0, err
	}
	rc.buf = append(rc.buf, b)
	return b, nil
}

// Read implements io.Reader. The decoder only calls ReadByte, but xml.NewDecoder takes an io.Reader.
func (rc *recorder) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b, err := rc.ReadByte()
	if err != nil {
		return 0, err
	}
	p[0] = b
	return 1, nil
}
