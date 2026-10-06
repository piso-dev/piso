package server

import (
	"encoding/json"
	"testing"

	"piso/gateway/internal/store"
)

func TestConnectionsIncludesThreeServices(t *testing.T) {
	s := testServer(t)
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/connections", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out struct {
		Connections []connectionView `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Connections) != 3 {
		t.Fatalf("%v", out.Connections)
	}
	if out.Connections[0].ID != "github" || out.Connections[0].Status != "none" {
		t.Fatalf("%+v", out.Connections[0])
	}
	if out.Connections[1].ID != "ai-boost" || out.Connections[2].ID != "hammerspace" {
		t.Fatalf("%+v", out.Connections)
	}
}

func TestConnectionsGithubOK(t *testing.T) {
	s := testServer(t)
	if _, err := s.Store.UpsertGithubApp(store.GithubAppRec{
		ID: "g1", Name: "piso", AppID: 1, PrivateKey: "pem",
		Installations: []store.GithubInstallation{{ID: 9, Account: "org"}},
	}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/connections", nil)
	var out struct {
		Connections []connectionView `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Connections[0].Status != store.GithubAppStatusOK {
		t.Fatalf("%+v", out.Connections[0])
	}
}
