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

func TestTaskControlProxy(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Piso-Signature") == "" {
			t.Error("missing signature")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/control/tasks":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["id"] != "task-id" || body["title"] != "Create me" {
				t.Errorf("body: %v", body)
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"task-id","status":"open"}`)
		case "GET /api/v1/control/tasks":
			if r.URL.Query().Get("cursor") != "next:page" || r.URL.Query().Get("group") != "active" || r.URL.Query().Get("accountId") != "" {
				t.Errorf("query: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"tasks":[],"nextCursor":"more"}`)
		case "GET /api/v1/control/tasks/task-id":
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"not found"}`)
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
	created := doJSON(t, s.ControlHandler(), "POST", "/api/v1/tasks", map[string]string{"id": "task-id", "title": "Create me"})
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	list := doJSON(t, s.ControlHandler(), "GET", "/api/v1/tasks?cursor=next%3Apage&group=active&accountId=forged", nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"nextCursor":"more"`) {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	detail := doJSON(t, s.ControlHandler(), "GET", "/api/v1/tasks/task-id", nil)
	if detail.Code != 404 || !strings.Contains(detail.Body.String(), "not found") {
		t.Fatalf("detail: %d %s", detail.Code, detail.Body)
	}
}

func TestTaskControlBoundaryAndUnpaired(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	for _, path := range []string{"/api/v1/tasks", "/api/v1/tasks/task-id"} {
		w := doJSON(t, s.ControlHandler(), "GET", path, nil)
		if w.Code != 503 {
			t.Fatalf("unpaired: %d %s", w.Code, w.Body)
		}
		w = doJSON(t, s.WorkerHandler(), "GET", path, nil)
		if w.Code == 200 {
			t.Fatalf("worker exposed %s", path)
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/tasks", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	s.ControlHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("CSRF: %d", w.Code)
	}
	s.DenyPeer = func(string) bool { return true }
	w = doJSON(t, s.ControlHandler(), "POST", "/api/v1/tasks", map[string]string{"title": "forged"})
	if w.Code != 403 {
		t.Fatalf("worker control access: %d", w.Code)
	}
}
