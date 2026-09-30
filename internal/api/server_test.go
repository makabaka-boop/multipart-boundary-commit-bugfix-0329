package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"uploadsvc/internal/api"
	"uploadsvc/internal/storage"
)

func newTestServer(t *testing.T, faults bool) (*httptest.Server, *storage.Store) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ts := httptest.NewServer(api.NewServer(store, faults, nil).Routes())
	t.Cleanup(ts.Close)
	return ts, store
}

func buildBody(t *testing.T, metaJSON string, fileName string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mh := make(map[string][]string)
	mh["Content-Disposition"] = []string{`form-data; name="meta"`}
	mh["Content-Type"] = []string{"application/json"}
	w, err := mw.CreatePart(mh)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(metaJSON))
	fh := make(map[string][]string)
	fh["Content-Disposition"] = []string{
		`form-data; name="file"; filename="` + fileName + `"`}
	fh["Content-Type"] = []string{"application/octet-stream"}
	w, err = mw.CreatePart(fh)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(content)
	mw.Close()
	return &buf, mw.Boundary()
}

func post(t *testing.T, url string, body io.Reader, boundary string, hdr http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestUploadAndRetrieve(t *testing.T) {
	ts, _ := newTestServer(t, false)
	content := []byte("handler-level content")
	body, boundary := buildBody(t, `{"request_id":"h1","content_type":"text/plain"}`,
		"client-name.txt", content)
	resp := post(t, ts.URL+"/uploads", body, boundary, nil)
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var rec map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	resp.Body.Close()
	sum := sha256.Sum256(content)
	if rec["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("bad hash: %v", rec["sha256"])
	}

	// Idempotent replay -> 200.
	body, boundary = buildBody(t, `{"request_id":"h1"}`, "other.txt", content)
	resp = post(t, ts.URL+"/uploads", body, boundary, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("replay status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Different content same id -> 409.
	body, boundary = buildBody(t, `{"request_id":"h1"}`, "x.txt", []byte("DIFFERENT"))
	resp = post(t, ts.URL+"/uploads", body, boundary, nil)
	if resp.StatusCode != 409 {
		t.Fatalf("conflict status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFaultHeaderIgnoredWhenDisabled(t *testing.T) {
	ts, _ := newTestServer(t, false)
	body, boundary := buildBody(t, `{"request_id":"f1"}`, "x", []byte("hi"))
	resp := post(t, ts.URL+"/uploads", body, boundary,
		http.Header{"X-Upload-Fault": {"after_commit"}})
	if resp.StatusCode != 201 {
		t.Fatalf("fault header must be ignored in production, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMalformedMultipartFramingRejected(t *testing.T) {
	ts, _ := newTestServer(t, false)
	content := []byte("framing content")

	build := func(t *testing.T, id string, mutate func([]byte, string) []byte) ([]byte, string) {
		body, boundary := buildBody(t, `{"request_id":"`+id+`"}`, "x.txt", content)
		return mutate(body.Bytes(), boundary), boundary
	}

	missingClosingDelimiter := func(body []byte, boundary string) []byte {
		validEnd := []byte("\r\n--" + boundary + "--\r\n")
		if !bytes.HasSuffix(body, validEnd) {
			t.Fatalf("test body did not end with the multipart closing boundary")
		}
		return append(body[:len(body)-len(validEnd)], []byte("\r\n--"+boundary+"\r\n")...)
	}
	trailingGarbage := func(body []byte, boundary string) []byte {
		return append(body, []byte("unexpected epilogue")...)
	}

	check := func(t *testing.T, id string, mutate func([]byte, string) []byte) {
		body, boundary := build(t, id, mutate)
		resp := post(t, ts.URL+"/uploads", bytes.NewReader(body), boundary, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("malformed framing: got %d want 400", resp.StatusCode)
		}

		req, err := http.NewRequest(http.MethodGet, ts.URL+"/uploads/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		getResp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, getResp.Body)
		getResp.Body.Close()
		if getResp.StatusCode != 404 {
			t.Fatalf("rejected upload became readable: got %d want 404", getResp.StatusCode)
		}
	}

	t.Run("ordinary_boundary_without_closing_marker", func(t *testing.T) {
		check(t, "frame-missing-end", missingClosingDelimiter)
	})
	t.Run("content_after_closing_marker", func(t *testing.T) {
		check(t, "frame-trailing", trailingGarbage)
	})
}

func TestMalformedReplayFramingRejected(t *testing.T) {
	ts, _ := newTestServer(t, false)
	content := []byte("replay framing content")
	body, boundary := buildBody(t, `{"request_id":"frame-replay"}`, "x.txt", content)
	resp := post(t, ts.URL+"/uploads", body, boundary, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("initial upload: got %d want 201", resp.StatusCode)
	}
	resp.Body.Close()

	cases := map[string]func([]byte, string) []byte{
		"ordinary_boundary_without_closing_marker": func(body []byte, boundary string) []byte {
			validEnd := []byte("\r\n--" + boundary + "--\r\n")
			if !bytes.HasSuffix(body, validEnd) {
				t.Fatalf("test body did not end with the multipart closing boundary")
			}
			return append(body[:len(body)-len(validEnd)], []byte("\r\n--"+boundary+"\r\n")...)
		},
		"content_after_closing_marker": func(body []byte, _ string) []byte {
			return append(body, []byte("unexpected epilogue")...)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body, boundary = buildBody(t, `{"request_id":"frame-replay"}`, "other.txt", content)
			malformed := mutate(body.Bytes(), boundary)
			resp := post(t, ts.URL+"/uploads", bytes.NewReader(malformed), boundary, nil)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Fatalf("malformed replay: got %d want 400", resp.StatusCode)
			}
		})
	}
}

func TestUploadLargerThanMultipartPeekBuffer(t *testing.T) {
	ts, _ := newTestServer(t, false)
	content := bytes.Repeat([]byte("A"), 5000)
	body, boundary := buildBody(t, `{"request_id":"large-body"}`, "x.txt", content)
	resp := post(t, ts.URL+"/uploads", body, boundary, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
}

func TestDuplicateAndUnknownParts(t *testing.T) {
	ts, _ := newTestServer(t, false)

	send := func(t *testing.T, parts func(*multipart.Writer)) int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		parts(mw)
		mw.Close()
		req, _ := http.NewRequest("POST", ts.URL+"/uploads", &buf)
		req.Header.Set("Content-Type", "multipart/form-data; boundary="+mw.Boundary())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	status := send(t, func(mw *multipart.Writer) {
		w, _ := mw.CreateFormField("meta")
		_, _ = io.Copy(w, strings.NewReader(`{"request_id":"d1"}`))
		w, _ = mw.CreateFormField("file")
		_, _ = w.Write([]byte("a"))
		w, _ = mw.CreateFormField("file")
		_, _ = w.Write([]byte("b"))
	})
	if status != 400 {
		t.Fatalf("duplicate file: got %d want 400", status)
	}

	status = send(t, func(mw *multipart.Writer) {
		w, _ := mw.CreateFormField("meta")
		_, _ = io.Copy(w, strings.NewReader(`{"request_id":"d2"}`))
		w, _ = mw.CreateFormField("file")
		_, _ = w.Write([]byte("a"))
		w, _ = mw.CreateFormField("rogue")
		_, _ = w.Write([]byte("b"))
	})
	if status != 400 {
		t.Fatalf("unknown field: got %d want 400", status)
	}
}
