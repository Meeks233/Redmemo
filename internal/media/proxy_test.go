package media

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// testProxy returns a Proxy wired to a plain fhttp client (the httpDoer seam),
// suitable for talking to an httptest server.
func testProxy() *Proxy {
	return &Proxy{httpClient: &fhttp.Client{}, userAgentFn: func() string { return "test-agent" }}
}

// rangeServer serves body with full HTTP Range support and records every
// requested Range header, so a test can assert the download was actually
// chunked rather than pulled in one shot.
func rangeServer(t *testing.T, body []byte) (*httptest.Server, *[]string) {
	t.Helper()
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "clip.mp4", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &ranges
}

func makeBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i * 7) % 251)
	}
	return b
}

func withChunkSize(t *testing.T, size int64) {
	t.Helper()
	old := mediaChunkSize
	mediaChunkSize = size
	t.Cleanup(func() { mediaChunkSize = old })
}

func TestStreamRangedTo_ReassemblesAcrossChunks(t *testing.T) {
	body := makeBody(5000)
	srv, ranges := rangeServer(t, body)
	withChunkSize(t, 1000)

	var buf bytes.Buffer
	status, _, n, err := testProxy().streamRangedTo(context.Background(), srv.URL, 0, nil, &buf, nil)
	if err != nil {
		t.Fatalf("streamRangedTo: %v", err)
	}
	if status != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", status)
	}
	if n != int64(len(body)) {
		t.Errorf("written = %d, want %d", n, len(body))
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Error("reassembled body does not match source")
	}
	// 5000 bytes / 1000-byte chunks = 5 ranged requests.
	if len(*ranges) != 5 {
		t.Errorf("made %d range requests, want 5: %v", len(*ranges), *ranges)
	}
	if (*ranges)[0] != "bytes=0-999" {
		t.Errorf("first range = %q, want bytes=0-999", (*ranges)[0])
	}
}

// TestStreamRangedTo_RetriesMidBodyTruncation simulates an h2
// FLOW_CONTROL_ERROR style mid-body abort: the server closes the connection
// after writing only part of a ranged response. streamRangedTo must re-issue
// the Range from the new offset rather than ship a truncated body up to the
// already-committed reverseProxy response headers.
func TestStreamRangedTo_RetriesMidBodyTruncation(t *testing.T) {
	body := makeBody(5000)
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		// Honor the Range header so the retry path resumes at the
		// correct offset.
		rng := r.Header.Get("Range")
		start, end := int64(0), int64(len(body)-1)
		if rng != "" {
			var s, e int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &s, &e); err == nil {
				start, end = s, e
			} else if _, err := fmt.Sscanf(rng, "bytes=%d-", &s); err == nil {
				start = s
			}
		}
		if end >= int64(len(body)) {
			end = int64(len(body) - 1)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		// Only the FIRST attempt truncates — write half the requested
		// span then hijack and slam the connection.
		if attempts == 1 {
			half := (end - start + 1) / 2
			w.Write(body[start : start+half])
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijacker unsupported")
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.Write(body[start : end+1])
	}))
	defer srv.Close()
	withChunkSize(t, int64(len(body)))

	var buf bytes.Buffer
	status, _, n, err := testProxy().streamRangedTo(context.Background(), srv.URL, 0, nil, &buf, nil)
	if err != nil {
		t.Fatalf("streamRangedTo: %v", err)
	}
	if status != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", status)
	}
	if n != int64(len(body)) {
		t.Errorf("written = %d, want %d", n, len(body))
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Error("retried body does not match source")
	}
	if attempts < 2 {
		t.Errorf("server saw %d attempts, want >= 2 (retry path)", attempts)
	}
}

func TestStreamRangedTo_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	status, _, _, err := testProxy().streamRangedTo(context.Background(), srv.URL, 0, nil, &buf, nil)
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
}

func TestReverseProxy_RangeRequestServesFullExtent(t *testing.T) {
	body := makeBody(5000)
	srv, _ := rangeServer(t, body)
	withChunkSize(t, 1000)

	r := httptest.NewRequest("GET", "/vid/x", nil)
	r.Header.Set("Range", "bytes=0-")
	w := httptest.NewRecorder()

	testProxy().reverseProxy(w, r, srv.URL, true, 0)

	resp := w.Result()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-4999/5000" {
		t.Errorf("Content-Range = %q, want bytes 0-4999/5000", cr)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (noStore=true)", cc)
	}
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Errorf("streamed body length %d, want %d", w.Body.Len(), len(body))
	}
}

func TestReverseProxy_NoRangeServesWhole(t *testing.T) {
	body := makeBody(2500)
	srv, _ := rangeServer(t, body)
	withChunkSize(t, 1000)

	r := httptest.NewRequest("GET", "/vid/x", nil)
	w := httptest.NewRecorder()

	testProxy().reverseProxy(w, r, srv.URL, false, 0)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Errorf("streamed body length %d, want %d", w.Body.Len(), len(body))
	}
}

// TestReverseProxy_ThrottledServesFullExtent confirms the per-stream throttle
// (the temporary online-playback sample cap) still delivers the complete body
// across the prebuffer + continuation chunks — the cap shapes the rate, it does
// not truncate. The small body fits the bucket burst, so the test stays fast.
func TestReverseProxy_ThrottledServesFullExtent(t *testing.T) {
	body := makeBody(5000)
	srv, _ := rangeServer(t, body)
	withChunkSize(t, 1000)

	r := httptest.NewRequest("GET", "/vid/x", nil)
	r.Header.Set("Range", "bytes=0-")
	w := httptest.NewRecorder()

	testProxy().reverseProxy(w, r, srv.URL, true, 1<<20)

	resp := w.Result()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-4999/5000" {
		t.Errorf("Content-Range = %q, want bytes 0-4999/5000", cr)
	}
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Errorf("throttled stream delivered %d bytes, want full %d", w.Body.Len(), len(body))
	}
}

// TestLiveStreamWriter_UngatedAndCapped pins the two properties that keep a live
// preview from starving the background cache-fill: it is bound to the supplied
// per-stream bucket AND it is NOT registered on the priority gate (a gate slot
// would block every lower-priority background download for the preview's whole
// lifetime).
func TestLiveStreamWriter_UngatedAndCapped(t *testing.T) {
	pb := newTokenBucket(1<<20, 1<<19)
	var buf bytes.Buffer
	lw := newLiveStreamWriter(context.Background(), &buf, pb)

	if lw.gate != nil {
		t.Error("live stream writer holds a priority-gate slot; it must bypass the gate so background downloads keep draining the budget")
	}
	if lw.pb != pb {
		t.Error("live stream writer is not bound to the per-stream cap bucket")
	}
	if _, err := lw.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("got %q, want hello", buf.String())
	}
	lw.release() // no gate slot — must be a safe no-op
}

func TestParseContentRangeTotal(t *testing.T) {
	cases := map[string]int64{
		"bytes 0-8388607/23489656": 23489656,
		"bytes 0-999/*":            -1,
		"":                         -1,
		"garbage":                  -1,
	}
	for in, want := range cases {
		if got := parseContentRangeTotal(in); got != want {
			t.Errorf("parseContentRangeTotal(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseRangeStart(t *testing.T) {
	cases := map[string]int64{
		"bytes=0-":        0,
		"bytes=1000-":     1000,
		"bytes=1000-2000": 1000,
		"bytes=-500":      0, // suffix range → serve from start
		"":                0,
		"junk":            0,
	}
	for in, want := range cases {
		if got := parseRangeStart(in); got != want {
			t.Errorf("parseRangeStart(%q) = %d, want %d", in, got, want)
		}
	}
}
