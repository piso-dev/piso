package server

import (
	"encoding/json"
	"testing"

	"piso/gateway/internal/gitaccess"
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
	if out.Connections[0].ID != "git" || out.Connections[0].Status != "none" {
		t.Fatalf("%+v", out.Connections[0])
	}
	if out.Connections[1].ID != "ai-boost" || out.Connections[2].ID != "hammerspace" {
		t.Fatalf("%+v", out.Connections)
	}
}

func TestConnectionsGitOK(t *testing.T) {
	s := testServer(t)
	if err := s.Store.AddSecret(store.SecretRec{ID: "git1", Name: "Git", Kind: gitaccess.Kind, Placeholder: "piso_git1", Value: "token", Git: &gitaccess.Config{Provider: "github", BaseURL: "https://github.com", TokenType: "user"}}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/connections", nil)
	var out struct {
		Connections []connectionView `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Connections[0].Status != "ok" {
		t.Fatalf("%+v", out.Connections[0])
	}
}
