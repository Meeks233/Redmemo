package handler

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipMiddleware transparently gzips responses for clients that accept it,
// for text payloads only (HTML/CSS/JS/JSON/XML/SVG). Pre-compressed binary —
// images, video, audio, woff2 fonts — passes through untouched.
//
// In the public deploy nginx fronts the app and already gzips, but the
// homelab/docker-run path hits the Go server directly with no compression in
// front; this middleware closes that gap. When nginx IS in front it sees the
// Content-Encoding header on the upstream response and forwards it as-is
// rather than re-compressing, which is strictly cheaper than the previous
// "decompress nothing → compress everything" flow.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !clientAcceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}

func clientAcceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		// Strip any q-value before comparing — clients commonly send
		// "gzip, deflate, br" or "gzip;q=1.0, identity;q=0.5".
		name := strings.TrimSpace(enc)
		if i := strings.IndexByte(name, ';'); i >= 0 {
			name = name[:i]
		}
		if name == "gzip" {
			return true
		}
	}
	return false
}

// gzipResponseWriter buffers the first WriteHeader/Write to decide whether
// the body is worth gzipping (text-ish content types, not already encoded).
// Once decided, it either streams through a *gzip.Writer or falls back to the
// underlying writer untouched.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	decided  bool
	bypass   bool
	status   int
	wroteHdr bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	w.status = code
	// Don't flush headers yet — we may need to add Content-Encoding and drop
	// Content-Length once we decide to compress.
}

func (w *gzipResponseWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	h := w.ResponseWriter.Header()
	// Already encoded upstream (e.g. media proxy forwarding a gzipped CDN
	// response) — leave it alone.
	if h.Get("Content-Encoding") != "" {
		w.bypass = true
	} else if !shouldCompressType(h.Get("Content-Type")) {
		w.bypass = true
	}
	if w.bypass {
		w.flushHeaders()
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	// Original length no longer matches the wire bytes.
	h.Del("Content-Length")
	w.flushHeaders()
	w.gz = gzipPool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)
}

func (w *gzipResponseWriter) flushHeaders() {
	if w.wroteHdr {
		return
	}
	w.wroteHdr = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.decided {
		// If the handler hasn't set a Content-Type yet, sniff from the body
		// the same way net/http does so our decision matches what the client
		// will ultimately see.
		h := w.ResponseWriter.Header()
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", http.DetectContentType(p))
		}
		w.decide()
	}
	if w.bypass {
		return w.ResponseWriter.Write(p)
	}
	return w.gz.Write(p)
}

func (w *gzipResponseWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipResponseWriter) Close() {
	if !w.decided {
		// Handler wrote no body — flush headers as-is.
		w.flushHeaders()
		return
	}
	if w.gz != nil {
		_ = w.gz.Close()
		gzipPool.Put(w.gz)
		w.gz = nil
	}
}

// shouldCompressType returns true for text-ish MIME types. gzip on already
// compressed bytes (images, video, audio, woff2, gzip) wastes CPU and can
// even inflate size slightly.
func shouldCompressType(ct string) bool {
	if ct == "" {
		return false
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json",
		"application/javascript",
		"application/xml",
		"application/xhtml+xml",
		"application/manifest+json",
		"application/opensearchdescription+xml",
		"image/svg+xml":
		return true
	}
	return false
}

var gzipPool = sync.Pool{
	New: func() any {
		// Level 5 is the standard sweet spot: ~95% of the size of best
		// compression at a fraction of the CPU. nginx defaults to 1, but
		// our templ output is small (<100KB typically) so going higher
		// barely costs anything per request.
		w, _ := gzip.NewWriterLevel(io.Discard, 5)
		return w
	},
}
