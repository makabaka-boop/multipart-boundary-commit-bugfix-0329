package api

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestStrictMultipartReaderAcceptsCompleteMessage(t *testing.T) {
	boundary := "B"
	body := "--B\r\n" +
		"Content-Disposition: form-data; name=\"meta\"\r\n\r\n" +
		"{}\r\n--B\r\n" +
		"Content-Disposition: form-data; name=\"file\"\r\n\r\n" +
		"abc\r\n--B--\r\n"

	for _, chunk := range []int{1, 2, 3, 7, 4096} {
		t.Run("chunks", func(t *testing.T) {
			r := newStrictMultipartReader(&chunkReader{data: []byte(body), n: chunk}, boundary)
			data, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(data, []byte(body)) {
				t.Fatalf("reader changed body bytes")
			}
		})
	}
}

func TestStrictMultipartReaderRejectsBadTermination(t *testing.T) {
	boundary := "B"
	cases := map[string]string{
		"ordinary boundary at EOF": "--B\r\nh1\r\n--B",
		"missing closing CRLF":     "--B\r\nh1\r\n--B--",
		"epilogue":                 "--B\r\nh1\r\n--B--\r\nextra",
		"wrong final dash":         "--B\r\nh1\r\n--B-x\r\n",
		"bare LF":                  "--B\r\nh1\r\n--B--\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := newStrictMultipartReader(bytes.NewReader([]byte(body)), boundary)
			_, err := io.ReadAll(r)
			if !errors.Is(err, strictFramingError) {
				t.Fatalf("err=%v, want strict framing error", err)
			}
		})
	}
}

type chunkReader struct {
	data []byte
	n    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}
