package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesHTML(t *testing.T) {
	h := Handler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if rr.Body.Len() == 0 {
		t.Error("empty body")
	}
}

func TestHandlerSPAFallback(t *testing.T) {
	h := Handler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/missing-route", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("SPA fallback Content-Type = %q, want text/html", ct)
	}
}
