package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tc "piso/internal/technocore"
)

func TestDirectiveControlProxy(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/control/directives":
			io.WriteString(w, `{"directives":[{"id":"d1","code":"D1","title":"Read first"}]}`)
		case "POST /api/v1/control/directives":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["title"] != "Add one" {
				t.Errorf("body: %v", body)
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"d2","code":"D2"}`)
		case "DELETE /api/v1/control/directives/d1":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer coord.Close()
	id, _ := tc.Generate(coord.URL, "gateway")
	id.Status = tc.StatusActive
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	// The dashboard stores a bare array per collection, so the envelope is
	// unwrapped once here rather than in every reader.
	list := doJSON(t, s.ControlHandler(), "GET", "/api/v1/directives", nil)
	if list.Code != 200 || strings.TrimSpace(list.Body.String()) != `[{"id":"d1","code":"D1","title":"Read first"}]` {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	created := doJSON(t, s.ControlHandler(), "POST", "/api/v1/directives", map[string]string{"title": "Add one"})
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	removed := doJSON(t, s.ControlHandler(), "DELETE", "/api/v1/directives/d1", nil)
	if removed.Code != 204 {
		t.Fatalf("delete: %d %s", removed.Code, removed.Body)
	}
}

func TestDirectiveControlUnpaired(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	// Only PUT and DELETE carry an id on this collection; a GET of one directive
	// is not a route, so the unpaired guard is exercised through the collection
	// read and an id-bearing write.
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/directives"},
		{"PUT", "/api/v1/directives/d1"},
	} {
		w := doJSON(t, s.ControlHandler(), c.method, c.path, nil)
		if w.Code != 503 {
			t.Fatalf("unpaired %s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), `"code":"`+unpairedCode+`"`) {
			t.Fatalf("unpaired code: %s", w.Body)
		}
		w = doJSON(t, s.WorkerHandler(), c.method, c.path, nil)
		if w.Code == 200 {
			t.Fatalf("worker exposed %s %s", c.method, c.path)
		}
	}
}
