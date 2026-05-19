package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// reqTimer records per-request segment timings and exposes them through the
// Server-Timing response header. Browser DevTools (Network panel) renders that
// header natively as a timing breakdown, so we get per-request profiling with
// zero client-side JS — consistent with the no-JS project goal.
//
// Usage:
//
//	t := newReqTimer()
//	... work ...
//	t.mark("cache")
//	... work ...
//	t.mark("render")
//	t.writeHeader(w) // MUST precede the first w.Write
type reqTimer struct {
	start time.Time
	last  time.Time
	spans []timingSpan
}

type timingSpan struct {
	name string
	dur  time.Duration
}

func newReqTimer() *reqTimer {
	now := time.Now()
	return &reqTimer{start: now, last: now}
}

// mark closes the current segment under name and starts the next one.
func (t *reqTimer) mark(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.spans = append(t.spans, timingSpan{name: name, dur: now.Sub(t.last)})
	t.last = now
}

// writeHeader emits the Server-Timing header. It must be called before the
// first w.Write, because writing flushes the header block.
func (t *reqTimer) writeHeader(w http.ResponseWriter) {
	if t == nil || len(t.spans) == 0 {
		return
	}
	parts := make([]string, 0, len(t.spans)+1)
	for i, s := range t.spans {
		parts = append(parts, "s"+strconv.Itoa(i)+
			`;desc="`+s.name+`";dur=`+millis(s.dur))
	}
	parts = append(parts, "total;dur="+millis(time.Since(t.start)))
	w.Header().Set("Server-Timing", strings.Join(parts, ", "))
}

// millis formats a duration as milliseconds with microsecond precision.
func millis(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 3, 64)
}
