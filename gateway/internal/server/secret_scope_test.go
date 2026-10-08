package server

import (
	"encoding/json"
	"testing"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

func TestSecretWorkerScopeUpdates(t *testing.T) {
	s := testServer(t)
	h := s.ControlHandler()
	created := postSecret(t, h, map[string]any{
		"name": "scope", "placeholder": "piso_scope_test", "value": "test-value",
		"gatewayOnly": true, "allowedHosts": []string{"api.test"},
	})
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var out struct {
		Secret model.SecretSummary `json:"secret"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	id := out.Secret.ID
	for _, tc := range []struct {
		name        string
		body        map[string]any
		gatewayOnly bool
		alpha, beta bool
	}{
		{"omitted scope stays gateway-only", map[string]any{"name": "scope"}, true, false, false},
		{"select alpha", map[string]any{"gatewayOnly": false, "workers": []string{"alpha"}}, false, true, false},
		{"omitted scope stays narrow", map[string]any{"name": "scope"}, false, true, false},
		{"null scope stays narrow", map[string]any{"workers": nil, "gatewayOnly": nil}, false, true, false},
		{"all workers", map[string]any{"gatewayOnly": false, "workers": []string{}}, false, true, true},
		{"no workers", map[string]any{"gatewayOnly": true, "workers": []string{}}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["allowedHosts"] = []string{"api.test"}
			w := doJSON(t, h, "PUT", "/api/v1/secrets/"+id, tc.body)
			if w.Code != 200 {
				t.Fatalf("update: %d %s", w.Code, w.Body.String())
			}
			rec, ok := s.Store.SecretByID(id)
			if !ok || rec.GatewayOnly != tc.gatewayOnly {
				t.Fatalf("gatewayOnly = %v, want %v", rec.GatewayOnly, tc.gatewayOnly)
			}
			if rec.Value != "test-value" {
				t.Fatal("scope edit changed credential")
			}
			if store.SecretVisibleToWorker(rec, "alpha") != tc.alpha || store.SecretVisibleToWorker(rec, "beta") != tc.beta {
				t.Fatal("unexpected worker access")
			}
			rules := doJSON(t, h, "GET", "/api/v1/rules", nil)
			var rs []model.Rule
			if err := json.Unmarshal(rules.Body.Bytes(), &rs); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range rs {
				if r.Placeholder == rec.Placeholder {
					found = true
				}
			}
			if found == tc.gatewayOnly {
				t.Fatalf("substitution rules present = %v for gatewayOnly = %v", found, tc.gatewayOnly)
			}
		})
	}
}
