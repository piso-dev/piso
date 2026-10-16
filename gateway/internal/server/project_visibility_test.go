package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestProjectVisibilityAPI(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()
	if res := doJSON(t, ch, "POST", "/api/v1/workers", map[string]any{"name": "piso-worker-demo", "slug": "demo"}); res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	for _, hidden := range []bool{true, false} {
		res := doJSON(t, ch, "POST", "/api/v1/projects/demo/visibility", map[string]any{"hidden": hidden})
		if res.Code != 200 {
			t.Fatalf("visibility: %d %s", res.Code, res.Body.String())
		}
		rec, _ := s.Store.WorkerByName("piso-worker-demo")
		if rec.ProjectHidden != hidden {
			t.Fatal("preference not updated")
		}
	}
	for _, body := range []any{map[string]any{}, map[string]any{"hidden": "yes"}} {
		if res := doJSON(t, ch, "POST", "/api/v1/projects/demo/visibility", body); res.Code != 400 {
			t.Fatalf("invalid body: %d", res.Code)
		}
	}
	if res := doJSON(t, ch, "POST", "/api/v1/projects/missing/visibility", map[string]any{"hidden": true}); res.Code != 404 {
		t.Fatalf("missing project: %d", res.Code)
	}
	for i := 0; i < 2; i++ {
		if res := doJSON(t, ch, "DELETE", "/api/v1/workers/piso-worker-demo", nil); res.Code != 200 {
			t.Fatalf("delete: %d", res.Code)
		}
	}
	if res := doJSON(t, ch, "GET", "/api/v1/workers", nil); strings.Contains(res.Body.String(), "demo") {
		t.Fatal("deleted worker listed")
	}
	if res := doJSON(t, ch, "DELETE", "/api/v1/workers/invalid!!", nil); res.Code != 400 {
		t.Fatalf("invalid name: %d", res.Code)
	}
}

func TestProjectManagementIsHostOnly(t *testing.T) {
	s := testServer(t)
	for _, route := range []struct{ method, path string }{
		{"DELETE", "/api/v1/workers/piso-worker-demo"},
		{"POST", "/api/v1/projects/demo/visibility"},
	} {
		res := doJSON(t, s.WorkerHandler(), route.method, route.path, map[string]any{"hidden": true})
		if res.Code != http.StatusNotFound && res.Code != http.StatusMethodNotAllowed {
			t.Fatalf("worker can manage projects: %d", res.Code)
		}
	}
}
