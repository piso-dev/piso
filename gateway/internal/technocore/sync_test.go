package technocore

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(
		filepath.Join(dir, "state.json"),
		filepath.Join(dir, "log.jsonl"),
		filepath.Join(dir, "patterns.json"),
		filepath.Join(dir, "activities.db"),
		20,
	)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSyncSecretsKeepsVaultOnHTTPError(t *testing.T) {
	st := testStore(t)
	if err := st.AddSecret(store.SecretRec{
		ID:          "tc_keepme",
		Name:        "keep",
		Placeholder: "piso_keep",
		Value:       "secret",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sync/secrets" {
			t.Errorf("path %s", r.URL.Path)
		}
		http.Error(w, "migrating", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	id, err := tc.Generate(srv.URL, "box")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	if err := syncSecrets(st, id); err == nil {
		t.Fatal("expected sync error")
	}
	got, ok := st.SecretByID("tc_keepme")
	if !ok || got.Value != "secret" {
		t.Fatalf("vault wiped on non-200: %+v ok=%v", got, ok)
	}
}

func TestSyncSecretsRejectsUnparseableBody(t *testing.T) {
	st := testStore(t)
	if err := st.AddSecret(store.SecretRec{
		ID:          "tc_keepme",
		Name:        "keep",
		Placeholder: "piso_keep",
		Value:       "secret",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	id, err := tc.Generate(srv.URL, "box")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	if err := syncSecrets(st, id); err == nil {
		t.Fatal("expected parse error")
	}
	if _, ok := st.SecretByID("tc_keepme"); !ok {
		t.Fatal("vault wiped on bad JSON")
	}
}
