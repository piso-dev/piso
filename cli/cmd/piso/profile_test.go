package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncPiProfilePreservesDefault(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
		status           int
	}{
		{"gateway unavailable", `{}`, "unbiased/pareto-26.10-preview", 503},
		{"older gateway", `{"secretId":"test"}`, "unbiased/pareto-26.10-preview", 200},
		{"explicit clear", `{"defaultModel":""}`, "", 200},
		{"new default", `{"defaultModel":"new-model"}`, "new-model", 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			agent := filepath.Join(dir, "agent")
			dest := filepath.Join(dir, "pi-profile")
			t.Setenv("PISO_DATA", dir)
			t.Setenv("PI_AGENT_DIR", agent)
			for _, p := range []string{agent, dest} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{
				filepath.Join(agent, "settings.json"): `{"theme":"test"}`,
				filepath.Join(dest, "settings.json"):  `{"defaultProvider":"routstr","defaultModel":"unbiased/pareto-26.10-preview"}`,
				filepath.Join(dest, "models.json"):    `{"providers":{"routstr":{"apiKey":"piso_test","baseUrl":"https://example.test/v1"}}}`,
			}
			for p, body := range files {
				if err := os.WriteFile(p, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tt.status); w.Write([]byte(tt.body)) }))
			defer srv.Close()
			t.Setenv("PISO_GATEWAY", srv.URL)
			if err := syncPiProfile(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dest, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			if settings["theme"] != "test" {
				t.Fatalf("host settings lost: %s", raw)
			}
			if tt.want == "" {
				if _, ok := settings["defaultModel"]; ok {
					t.Fatalf("not cleared: %s", raw)
				}
			} else if settings["defaultModel"] != tt.want {
				t.Fatalf("worker default: %s", raw)
			}
		})
	}
}
