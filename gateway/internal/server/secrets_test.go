package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postSecret posts a secret-create body to the control-plane API.
func postSecret(t *testing.T, h http.Handler, body any) *httptest.ResponseRecorder {
	return doJSON(t, h, "POST", "/api/v1/secrets", body)
}

func TestPostSecretSeedsRulesAndRetried(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()
	w := postSecret(t, ch, map[string]any{
		"name": "k1", "placeholder": "piso_k1_aaa", "value": "v",
		"allowedHosts": []string{"api.test"},
	})
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "\"retried\":[]") {
		t.Fatalf("missing retried: %s", w.Body.String())
	}
	rw := doJSON(t, ch, "GET", "/api/v1/rules", nil)
	body := rw.Body.String()
	if rw.Code != 200 || !strings.Contains(body, `"placeholder":"piso_k1_aaa"`) || !strings.Contains(body, `"host":"api.test"`) {
		t.Fatalf("rules: %d %s", rw.Code, body)
	}
}

func TestPostSecretWithoutHostsGetsStarRule(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()
	w := postSecret(t, ch, map[string]any{"name": "k2", "placeholder": "piso_k2_bbb", "value": "v"})
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	rw := doJSON(t, ch, "GET", "/api/v1/rules", nil)
	body := rw.Body.String()
	if rw.Code != 200 || !strings.Contains(body, `"placeholder":"piso_k2_bbb"`) || !strings.Contains(body, `"host":"*"`) {
		t.Fatalf("rules: %d %s", rw.Code, body)
	}
}

func TestPutSecretResyncsRules(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()
	created := postSecret(t, ch, map[string]any{
		"name": "k3", "placeholder": "piso_k3_ccc", "value": "v",
		"allowedHosts": []string{"a.test"},
	})
	if created.Code != 201 {
		t.Fatalf("create %d %s", created.Code, created.Body.String())
	}
	var parsed struct {
		Secret struct {
			ID string `json:"id"`
		} `json:"secret"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(parsed.Secret.ID, "sec_") {
		t.Fatalf("no secret id: %s", created.Body.String())
	}
	// Re-scope the secret to another host via PUT: the old rule must go away.
	u := doJSON(t, ch, "PUT", "/api/v1/secrets/" + parsed.Secret.ID, map[string]any{
		"name": "k3", "allowedHosts": []string{"b.test"},
	})
	if u.Code != 200 {
		t.Fatalf("put %d %s", u.Code, u.Body.String())
	}
	if !strings.Contains(u.Body.String(), "\"retried\":[]") {
		t.Fatalf("put missing retried: %s", u.Body.String())
	}
	rw := doJSON(t, ch, "GET", "/api/v1/rules", nil)
	body := rw.Body.String()
	if !strings.Contains(body, `"host":"b.test"`) || strings.Contains(body, `"host":"a.test"`) {
		t.Fatalf("rules after put: %s", body)
	}
}