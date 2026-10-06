package server

import (
	"encoding/json"
	"testing"
)

func TestAsksListUnpairedIsEmpty(t *testing.T) {
	s := testServer(t)
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/asks", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out struct {
		Asks []any `json:"asks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Asks == nil {
		t.Fatal("asks null")
	}
	if len(out.Asks) != 0 {
		t.Fatalf("%v", out.Asks)
	}
}
