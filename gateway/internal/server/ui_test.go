package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestApexServesDashboardAndFavicon pins the piso.local surface: the dashboard
// advertises an icon and the icon is actually served, not a 404 like before.
func TestApexServesDashboardAndFavicon(t *testing.T) {
	s := testServer(t)
	h := s.WebHandler()

	dashboard := httptest.NewRequest(http.MethodGet, "http://piso.local/", nil)
	dashboard.Host = "piso.local"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, dashboard)
	if w.Code != 200 {
		t.Fatalf("dashboard status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `rel="icon"`) {
		t.Fatalf("dashboard does not advertise an icon")
	}

	for _, path := range []string{"/favicon.svg", "/favicon.ico"} {
		req := httptest.NewRequest(http.MethodGet, "http://piso.local"+path, nil)
		req.Host = "piso.local"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s status %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
			t.Fatalf("%s content type %q", path, got)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<svg") {
			t.Fatalf("%s is not an svg: %q", path, body)
		}
		if !strings.Contains(body, "#f3cf83") {
			t.Fatalf("%s is missing the piso mark: %q", path, body)
		}
	}
}

// TestApexUnknownPath404 is the regression guard: the favicon must not turn the
// apex catch-all into an open proxy or a directory listing.
func TestApexUnknownPath404(t *testing.T) {
	s := testServer(t)
	h := s.WebHandler()
	req := httptest.NewRequest(http.MethodGet, "http://piso.local/nope.txt", nil)
	req.Host = "piso.local"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown apex path status %d", w.Code)
	}
}
