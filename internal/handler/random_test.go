package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseFlexibleTime(t *testing.T) {
	t.Run("unix seconds", func(t *testing.T) {
		got, err := parseFlexibleTime("1700000000")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Unix() != 1700000000 {
			t.Errorf("Unix = %d, want 1700000000", got.Unix())
		}
	})
	t.Run("date only", func(t *testing.T) {
		got, err := parseFlexibleTime("2026-05-16")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Year() != 2026 || got.Month() != time.May || got.Day() != 16 {
			t.Errorf("parsed date = %v", got)
		}
	})
	t.Run("RFC3339", func(t *testing.T) {
		got, err := parseFlexibleTime("2026-05-16T12:30:00Z")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Hour() != 12 || got.Minute() != 30 {
			t.Errorf("parsed time = %v", got)
		}
	})
	t.Run("invalid string", func(t *testing.T) {
		if _, err := parseFlexibleTime("not-a-date"); err == nil {
			t.Error("expected an error for an unparseable string")
		}
	})
	t.Run("empty string", func(t *testing.T) {
		if _, err := parseFlexibleTime(""); err == nil {
			t.Error("expected an error for an empty string")
		}
	})
}

func TestWriteRandomError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRandomError(rec, http.StatusBadRequest, "something went wrong")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body["error"] != "something went wrong" {
		t.Errorf("error field = %q", body["error"])
	}
}

// TestHandleRandom_InvalidParams exercises the query-parameter validation,
// which bails with 400 before the (DB-backed) post store is ever touched —
// so a bare Handler with no store is sufficient.
func TestHandleRandom_InvalidParams(t *testing.T) {
	h := &Handler{}
	cases := []struct {
		query   string
		wantErr string
	}{
		{"min_score=abc", "invalid min_score"},
		{"after=notadate", "invalid after"},
		{"before=2026-13-99", "invalid before"},
		{"nsfw=banana", "invalid nsfw"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/random?"+c.query, nil)
		h.handleRandom(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.query, rec.Code)
			continue
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: response not JSON: %v", c.query, err)
			continue
		}
		if body["error"] != c.wantErr {
			t.Errorf("%s: error = %q, want %q", c.query, body["error"], c.wantErr)
		}
	}
}
