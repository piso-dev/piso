package technocore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func TestSyncPiProviderDefaultCompatibility(t *testing.T) {
	for _, tt := range []struct{ name, field, want string }{
		{"older coordinator omits field", "", "unbiased/pareto-26.10-preview"},
		{"explicit clear", `,"defaultModel":""`, ""},
		{"new selection", `,"defaultModel":"new-model"`, "new-model"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := testStore(t)
			dir := t.TempDir()
			t.Setenv("PISO_PI_PROFILE", filepath.Join(dir, "pi-profile"))
			if err := st.AddSecret(store.SecretRec{ID: "secret", Placeholder: "piso_test", Value: "fake"}); err != nil {
				t.Fatal(err)
			}
			if err := st.SetPiProvider(&store.PiProviderRec{SecretID: "secret", BaseURL: "https://example.test/v1", DefaultModel: "unbiased/pareto-26.10-preview", UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"metadata":{"secretId":"secret","baseUrl":"https://example.test/v1"%s},"updated_at":"2026-01-02T00:00:00Z"}`, tt.field)
			}))
			defer srv.Close()
			old := serverHTTP
			serverHTTP = srv.Client()
			defer func() { serverHTTP = old }()
			id, err := tc.Generate(srv.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := syncPiProvider(st, dir, id); err != nil {
				t.Fatal(err)
			}
			if got := st.PiProvider().DefaultModel; got != tt.want {
				t.Fatalf("saved default = %q, want %q", got, tt.want)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "pi-profile", "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if _, ok := settings["defaultModel"]; ok {
					t.Fatalf("default not cleared: %s", raw)
				}
			} else if settings["defaultModel"] != tt.want {
				t.Fatalf("worker default: %s", raw)
			}
		})
	}
}
