package api

import (
	"errors"
	"io"
)

// errMalformedMultipartFraming is returned by framingReader when the raw
// request body does not end in a multipart closing boundary, or contains bytes
// after that boundary.
var errMalformedMultipartFraming = errors.New("malformed multipart framing")

// framingReader observes every byte read from the request body. mime/multipart
// accepts both EOF immediately after an ordinary part boundary and an
// unread epilogue after the closing boundary, so this reader independently
// verifies where the parsed body really ends.
type framingReader struct {
	r       byteReader
	matcher *framingMatcher
}

func newFramingReader(r byteReader, boundary string) *framingReader {
	return &framingReader{
		r:       r,
		matcher: newFramingMatcher(boundary),
	}
}

func (f *framingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	for _, b := range p[:n] {
		f.matcher.write(b)
	}
	if f.matcher.err != nil {
		return n, f.matcher.err
	}
	if errors.Is(err, io.EOF) {
		if verr := f.matcher.finish(); verr != nil {
			return n, verr
		}
	}
	return n, err
}

// byteReader is the Read method set used by framingReader.
type byteReader interface {
	Read([]byte) (int, error)
}

const (
	framingScan = iota
	framingPadding
	framingCloseLF
	framingDone
)

type framingMatcher struct {
	needle []byte
	prefix []int
	state  int
	tail   int
	err    error
}

func newFramingMatcher(boundary string) *framingMatcher {
	// Starting at '\n' matches both canonical CRLF and the parser's lenient
	// bare-LF boundary handling. The state machine below validates the rest
	// of the final boundary line.
	needle := append([]byte("\n--"), boundary+"--"...)
	prefix := make([]int, len(needle))
	for i := 1; i < len(needle); i++ {
		j := prefix[i-1]
		for j > 0 && needle[i] != needle[j] {
			j = prefix[j-1]
		}
		if needle[i] == needle[j] {
			j++
		}
		prefix[i] = j
	}
	return &framingMatcher{needle: needle, prefix: prefix}
}

func (m *framingMatcher) write(b byte) {
	if m.err != nil {
		return
	}

	switch m.state {
	case framingScan:
		for m.tail > 0 && b != m.needle[m.tail] {
			m.tail = m.prefix[m.tail-1]
		}
		if b == m.needle[m.tail] {
			m.tail++
		}
		if m.tail == len(m.needle) {
			m.state = framingPadding
			m.tail = 0
		}
	case framingPadding:
		// mime/multipart permits linear whitespace on the delimiter line.
		switch b {
		case ' ', '\t':
		case '\r':
			m.state = framingCloseLF
		case '\n':
			m.state = framingDone
		default:
			m.err = errMalformedMultipartFraming
		}
	case framingCloseLF:
		if b != '\n' {
			m.err = errMalformedMultipartFraming
			return
		}
		m.state = framingDone
	case framingDone:
		// No epilogue, including one consisting only of whitespace.
		m.err = errMalformedMultipartFraming
	}
}

func (m *framingMatcher) finish() error {
	if m.err != nil {
		return m.err
	}
	// EOF immediately after the closing marker is accepted by mime/multipart.
	// A bare CR or an ordinary boundary is not.
	if m.state == framingPadding || m.state == framingDone {
		return nil
	}
	return errMalformedMultipartFraming
}
