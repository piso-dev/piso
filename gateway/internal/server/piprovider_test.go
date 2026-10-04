package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestPutPiProviderWritesPlaceholderNotValue(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "pi-profile")
	t.Setenv("PISO_PI_PROFILE", profile)
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	const real = "sk-test-FAKE-not-written"
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_local", Name: "AIML", Placeholder: "piso_aimlapi", Value: real,
		TechnocoreID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, DataDir: dir}
	body := []byte(`{"secretId":"11111111-1111-1111-1111-111111111111","baseUrl":"https://api.example.test/v1"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handlePutPiProvider(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var view piProviderView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Applied || view.Placeholder != "piso_aimlapi" {
		t.Fatalf("view: %+v", view)
	}
	if strings.Contains(rec.Body.String(), real) {
		t.Fatalf("response leaked the secret: %s", rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(profile, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, real) {
		t.Fatalf("models.json contains the real key: %s", got)
	}
	if !strings.Contains(got, `"apiKey": "piso_aimlapi"`) || !strings.Contains(got, `"models": []`) {
		t.Fatalf("profile: %s", got)
	}
	if st.PiProvider().SecretID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("stored pointer: %+v", st.PiProvider())
	}
}

func TestPutPiProviderRejectsBadURL(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", strings.NewReader(`{"secretId":"x","baseUrl":"not a url"}`))
	rec := httptest.NewRecorder()
	s.handlePutPiProvider(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d", rec.Code)
	}
}
