package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRandomQueryExpr(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"empty", "/random", ""},
		{"plain spaces", "/random?q=s:golang%20u%3E200%20t:img", "s:golang u>200 t:img"},
		{"ampersand separators", "/random?q=s:golang&u%3E1000&t:img", "s:golang u>1000 t:img"},
		{"plus encoded spaces", "/random?q=s:golang+u%3E200", "s:golang u>200"},
		{"no q prefix", "/random?s:golang&u%3E1000", "s:golang u>1000"},
		{"percent-encoded literal ampersand", "/random?q=flair:a%26b", "flair:a&b"},
		{"quoted multiword", "/random?q=f:%22male+only%22&u>10", `f:"male only" u>10`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if got := randomQueryExpr(r); got != tt.want {
				t.Errorf("randomQueryExpr(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
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
