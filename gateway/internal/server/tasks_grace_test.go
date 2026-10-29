package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tc "piso/internal/technocore"
)

func TestTaskGraceProxyPaths(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/control/tasks/t1/view":
			// The heartbeat carries the empty JSON object the page sends; the
			// proxy must forward the body, not drop it.
			raw, _ := io.ReadAll(r.Body)
			if strings.TrimSpace(string(raw)) != "{}" {
				t.Errorf("view body: %q", raw)
			}
			io.WriteString(w, `{"id":"t1","graceHeld":"viewing"}`)
		case "DELETE /api/v1/control/tasks/t1/view":
			io.WriteString(w, `{"status":"ok"}`)
		case "POST /api/v1/control/tasks/t1/available":
			io.WriteString(w, `{"id":"t1","graceHeld":null,"releasedAt":"2024-01-01T00:00:00Z"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer coord.Close()
	id, _ := tc.Generate(coord.URL, "gateway")
	id.Status = tc.StatusActive
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	beat := doJSON(t, s.ControlHandler(), "POST", "/api/v1/tasks/t1/view", map[string]string{})
	if beat.Code != 200 || !strings.Contains(beat.Body.String(), `"graceHeld":"viewing"`) {
		t.Fatalf("beat: %d %s", beat.Code, beat.Body)
	}
	left := doJSON(t, s.ControlHandler(), "DELETE", "/api/v1/tasks/t1/view", nil)
	if left.Code != 200 {
		t.Fatalf("leave: %d %s", left.Code, left.Body)
	}
	released := doJSON(t, s.ControlHandler(), "POST", "/api/v1/tasks/t1/available", map[string]string{})
	if released.Code != 200 || !strings.Contains(released.Body.String(), `"releasedAt"`) {
		t.Fatalf("available: %d %s", released.Code, released.Body)
	}
}

func TestTaskGraceIsDashboardOnly(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	// A worker never sees the grace controls: holding a task is a human's
	// decision, and a worker that could release its own hold would defeat it.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/tasks/t1/view"},
		{"DELETE", "/api/v1/tasks/t1/view"},
		{"POST", "/api/v1/tasks/t1/available"},
	} {
		w := doJSON(t, s.WorkerHandler(), c.method, c.path, map[string]string{})
		if w.Code == 200 {
			t.Fatalf("worker exposed %s %s", c.method, c.path)
		}
	}
}
