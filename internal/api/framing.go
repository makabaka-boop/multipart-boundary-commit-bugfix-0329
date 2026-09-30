package api

import (
	"errors"
	"fmt"
	"io"
)

// strictFramingError marks messages produced by strictMultipartReader so they
// are reported as bad requests rather than request-body transport failures.
var strictFramingError = errors.New("malformed multipart framing")

// validMultipartBoundary applies the bchars/bchars-nospace grammar from
// RFC 2046. The standard library panics on some impossible boundary values,
// so they must be rejected before constructing a multipart.Reader.
func validMultipartBoundary(b string) bool {
	if len(b) < 1 || len(b) > 69 {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
		default:
			switch c {
			case '\'', '(', ')', '+', '_', ',', '-', '.', '/',
				':', '=', '?':
			default:
				return false
			}
		}
	}
	return true
}

// multipart framing scanner states.
const (
	frameStart = iota
	frameBody
	frameDelimCR
	frameBoundaryCR
	frameBoundaryPadding
	frameBoundaryCloseDash
	frameBoundaryClosePadding
	frameAfterCloseCR
	frameDone
)

// strictMultipartReader observes the entire multipart message while still
// passing it through to the streaming multipart parser. The stdlib parser
// intentionally accepts the optional MIME epilogue and may treat an ordinary
// boundary at EOF as though it were the terminating boundary. This reader adds
// the service's stricter invariant: the last delimiter must be "--boundary--",
// followed only by CRLF (and permitted delimiter transport padding).
type strictMultipartReader struct {
	r        io.Reader
	pattern  []byte
	prefix   []int
	state    int
	matched  int
	finished bool
	err      error
}

func newStrictMultipartReader(r io.Reader, boundary string) *strictMultipartReader {
	pattern := append([]byte("\r\n--"), boundary...)
	return &strictMultipartReader{
		r:       r,
		pattern: pattern,
		prefix:  kmpPrefix(pattern),
		state:   frameStart,
		// The opening boundary is the same match, but logically occurs at the
		// beginning of the stream rather than after CRLF. Skip those two bytes.
		matched: 2,
	}
}

func kmpPrefix(p []byte) []int {
	pi := make([]int, len(p))
	for i := 1; i < len(p); i++ {
		j := pi[i-1]
		for j > 0 && p[i] != p[j] {
			j = pi[j-1]
		}
		if p[i] == p[j] {
			j++
		}
		pi[i] = j
	}
	return pi
}

func (s *strictMultipartReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	if s.finished {
		return 0, io.EOF
	}

	n, err := s.r.Read(p)
	if n > 0 {
		s.scan(p[:n])
	}
	if s.err != nil {
		// Keep the bytes just read available to the multipart parser; it can
		// produce its own error, while the next read reports the framing error
		// if the parser keeps consuming.
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		s.finishAtEOF()
		if s.err != nil {
			return n, nil
		}
		s.finished = true
		return n, io.EOF
	}
	if err != nil {
		s.err = err
		return n, err
	}
	return n, nil
}

// scan drives the small state machine one byte at a time. A KMP matcher avoids
// retaining a growing copy while the file content streams past.
func (s *strictMultipartReader) scan(p []byte) {
	for _, c := range p {
		switch s.state {
		case frameStart:
			// The request has no MIME preamble: the first bytes must be the
			// opening boundary (logically "\r\n--boundary" at position zero).
			s.matchStart(c)

		case frameBody:
			s.matchDelimiter(c, frameBody)

		case frameDelimCR:
			// KMP matched "\r\n--boundary"; the byte after that decides whether
			// this starts another part or ends the multipart message.
			switch c {
			case ' ', '\t':
				s.state = frameBoundaryPadding
			case '\r':
				s.state = frameBoundaryCR
			case '-':
				s.state = frameBoundaryCloseDash
			default:
				s.fail("multipart boundary is missing its line ending or closing marker")
			}

		case frameBoundaryPadding:
			switch c {
			case ' ', '\t':
			case '\r':
				s.state = frameBoundaryCR
			case '-':
				s.state = frameBoundaryCloseDash
			default:
				s.fail("invalid character after multipart boundary delimiter")
			}

		case frameBoundaryCR:
			if c != '\n' {
				s.fail("multipart boundary must use CRLF line endings")
				return
			}
			s.matched = 0
			s.state = frameBody

		case frameBoundaryCloseDash:
			if c != '-' {
				s.fail("multipart closing boundary must end with \"--\"")
				return
			}
			s.state = frameBoundaryClosePadding

		case frameBoundaryClosePadding:
			switch c {
			case ' ', '\t':
				// RFC 2046 permits transport padding after boundary delimiters.
			case '\r':
				s.state = frameAfterCloseCR
			default:
				s.fail("unexpected data after multipart closing boundary")
			}

		case frameAfterCloseCR:
			if c != '\n' {
				s.fail("multipart closing boundary must use CRLF line endings")
				return
			}
			s.state = frameDone

		case frameDone:
			s.fail("unexpected data after multipart closing boundary")
		}
		if s.err != nil {
			return
		}
	}
}

// matchStart matches the opening "--boundary" (pattern positions 2 onward).
func (s *strictMultipartReader) matchStart(c byte) {
	if c == s.pattern[s.matched] {
		s.matched++
		if s.matched == len(s.pattern) {
			s.matched = 0
			s.state = frameDelimCR
		}
		return
	}
	s.fail("multipart body must begin with the boundary delimiter")
}

// matchDelimiter feeds c to the KMP matcher for "\r\n--boundary".
func (s *strictMultipartReader) matchDelimiter(c byte, fallback int) {
	for s.matched > 0 && c != s.pattern[s.matched] {
		s.matched = s.prefix[s.matched-1]
	}
	if c == s.pattern[s.matched] {
		s.matched++
	}
	if s.matched == len(s.pattern) {
		s.matched = 0
		s.state = frameDelimCR
		return
	}
	s.state = fallback
}

func (s *strictMultipartReader) fail(msg string) {
	s.err = fmt.Errorf("%s: %w", msg, strictFramingError)
}

func (s *strictMultipartReader) finishAtEOF() {
	switch s.state {
	case frameStart:
		s.fail("truncated multipart body: missing opening boundary")
	case frameBody, frameDelimCR:
		s.fail("truncated multipart body: missing closing boundary")
	case frameBoundaryCR:
		s.fail("truncated multipart boundary: missing LF")
	case frameBoundaryCloseDash:
		s.fail("truncated multipart closing boundary: missing final dash")
	case frameBoundaryClosePadding:
		// RFC 2046 requires CRLF after the closing delimiter.
		s.fail("truncated multipart closing boundary: missing final CRLF")
	case frameAfterCloseCR:
		s.fail("truncated multipart closing boundary: missing LF")
	case frameDone:
		// The message ended immediately after its required final CRLF.
	}
}
