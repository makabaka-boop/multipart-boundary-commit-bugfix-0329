package api

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func feedMatcher(t *testing.T, boundary string, data []byte, split int) error {
	t.Helper()
	m := newFramingMatcher(boundary)
	for offset := 0; offset < len(data); offset += split {
		end := offset + split
		if end > len(data) {
			end = len(data)
		}
		for _, b := range data[offset:end] {
			m.write(b)
			if m.err != nil {
				return m.err
			}
		}
	}
	return m.finish()
}

func TestFramingMatcher(t *testing.T) {
	valid := []byte("preamble\r\n--X\r\nheaders\r\n\r\nmeta\r\n" +
		"--X\r\nheaders\r\n\r\nfile content\r\n--X--\r\n")
	validLWSP := []byte("file content\r\n--X-- \t \r\n")
	validEOFAtMarker := []byte("file content\r\n--X--")
	validBareLF := []byte("meta\n--X\n\nfile\n--X-- \n")
	ordinaryBoundaryOnly := []byte("meta\r\n--X\r\nheaders\r\n\r\nfile\r\n--X")
	ordinaryBoundaryWithCRLF := []byte("meta\r\n--X\r\n\r\nfile\r\n--X\r\n")
	trailingGarbage := append(append([]byte{}, valid...), []byte("epilogue")...)
	paddingGarbage := []byte("file\r\n--X-- x\r\n")
	partialCloseBoundary := []byte("file\r\n--X-")

	cases := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{"valid canonical body", valid, false},
		{"valid linear whitespace on closing line", validLWSP, false},
		{"valid EOF immediately after closing marker", validEOFAtMarker, false},
		{"valid bare LF delimiters", validBareLF, false},
		{"ordinary boundary at EOF", ordinaryBoundaryOnly, true},
		{"ordinary boundary and CRLF at EOF", ordinaryBoundaryWithCRLF, true},
		{"trailing garbage after close", trailingGarbage, true},
		{"non-whitespace after closing padding", paddingGarbage, true},
		{"partial close boundary at EOF", partialCloseBoundary, true},
	}

	for _, tc := range cases {
		for _, split := range []int{1, 3, 32, len(tc.data)} {
			if split > len(tc.data) {
				split = len(tc.data)
			}
			err := feedMatcher(t, "X", tc.data, split)
			if tc.wantErr && !errors.Is(err, errMalformedMultipartFraming) {
				t.Errorf("%s split=%d: got %v, want malformed framing", tc.name, split, err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%s split=%d: unexpected error %v", tc.name, split, err)
			}
		}
	}
}

func TestFramingReaderChecksEOF(t *testing.T) {
	body := "\r\n--X\r\n\r\nmeta\r\n--X\r\n\r\nfile\r\n--X--\r\n"
	f := newFramingReader(strings.NewReader(body), "X")
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("valid body: %v", err)
	}
	if string(data) != body {
		t.Fatal("reader did not preserve the original body")
	}

	f = newFramingReader(strings.NewReader(strings.TrimSuffix(body, "--\r\n")), "X")
	if _, err := io.ReadAll(f); !errors.Is(err, errMalformedMultipartFraming) {
		t.Fatalf("ordinary boundary at EOF: got %v, want malformed framing", err)
	}
}
