package technocore

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestApplyProviderPointerWritesDataDirProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", "")
	t.Setenv("PISO_PI_PROFILE", "")
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
	if err := st.AddSecret(store.SecretRec{
		ID: "tc_rid", Name: "AIML", Placeholder: "piso_aimlapi", Value: "not-written",
		TechnocoreID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPiProvider(&store.PiProviderRec{
		SecretID: "11111111-1111-1111-1111-111111111111",
		BaseURL:  "https://api.example.test/v1",
	}); err != nil {
		t.Fatal(err)
	}
	applyProviderPointer(st, dir)
	raw, err := os.ReadFile(filepath.Join(dir, "pi-profile", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"apiKey": "piso_aimlapi"`) || strings.Contains(string(raw), "not-written") {
		t.Fatalf("profile: %s", raw)
	}
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
	if err := syncSecrets(st, t.TempDir(), id); err == nil {
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
	if err := syncSecrets(st, t.TempDir(), id); err == nil {
		t.Fatal("expected parse error")
	}
	if _, ok := st.SecretByID("tc_keepme"); !ok {
		t.Fatal("vault wiped on bad JSON")
	}
}

func testEnvelope(t *testing.T, id tc.Identity, resourceID, name, placeholder, value string) envelope {
	t.Helper()
	dek, nonce, ct, err := tc.SealPayload([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	wrap, err := tc.WrapDEK(id.EncryptionPublicKey, dek)
	if err != nil {
		t.Fatal(err)
	}
	return envelope{
		ResourceID:     resourceID,
		Name:           name,
		Metadata:       map[string]any{"placeholder": placeholder, "envKey": "K"},
		Algorithm:      tc.EnvelopeAlg,
		ContentHash:    plaintextHash(value),
		Ciphertext:     base64.StdEncoding.EncodeToString(ct),
		Nonce:          base64.StdEncoding.EncodeToString(nonce),
		WrappedDataKey: base64.StdEncoding.EncodeToString(wrap),
	}
}

func TestApplyEnvelopeAdoptsMatchingValueDifferentPlaceholder(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID:          "local1",
		Name:        "GitHub PAT",
		Placeholder: "piso_github_pat_aaa",
		Value:       "ghp_secret",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	obj := testEnvelope(t, id, "rid-1", "GitHub PAT", "piso_github_pat_bbb", "ghp_secret")
	kept, err := applyEnvelope(st, id, obj)
	if err != nil {
		t.Fatal(err)
	}
	if kept != "local1" {
		t.Fatalf("kept %s", kept)
	}
	if len(st.Secrets()) != 1 {
		t.Fatalf("duplicated local row: %d", len(st.Secrets()))
	}
	got, _ := st.SecretByID("local1")
	if got.Placeholder != "piso_github_pat_aaa" {
		t.Fatalf("placeholder rewritten: %s", got.Placeholder)
	}
	if got.TechnocoreID != "rid-1" {
		t.Fatalf("not stamped: %+v", got)
	}
}

func TestApplyEnvelopeStampsMatchingPlaceholder(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID:          "local1",
		Name:        "GitHub PAT",
		Placeholder: "piso_github_pat_aaa",
		Value:       "ghp_secret",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	obj := testEnvelope(t, id, "rid-1", "GitHub PAT", "piso_github_pat_aaa", "ghp_secret")
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	got, _ := st.SecretByID("local1")
	if got.TechnocoreID != "rid-1" || got.PublishedHash == "" {
		t.Fatalf("not stamped: %+v", got)
	}
	if _, ok := st.SecretByID("tc_rid-1"); ok {
		t.Fatal("added a tc_ duplicate")
	}
}

func TestApplyEnvelopeConflictDifferentValue(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID:          "local1",
		Name:        "GitHub PAT",
		Placeholder: "piso_github_pat_aaa",
		Value:       "old-value",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// Same content_version as the local row: the local copy is an edit the
	// coordinator has not seen yet, so it is kept rather than clobbered.
	obj := testEnvelope(t, id, "rid-1", "GitHub PAT", "piso_github_pat_aaa", "new-value")
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	got, _ := st.SecretByID("local1")
	if got.Value != "old-value" {
		t.Fatalf("local overwritten: %+v", got)
	}
}

func TestApplyEnvelopeNewerVersionWins(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID:             "local1",
		Name:           "GitHub PAT",
		Placeholder:    "piso_github_pat_aaa",
		Value:          "old-value",
		TechnocoreID:   "rid-1",
		ContentVersion: 1,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	obj := testEnvelope(t, id, "rid-1", "GitHub PAT", "piso_github_pat_aaa", "new-value")
	obj.ContentVersion = 2
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	got, _ := st.SecretByID("local1")
	if got.Value != "new-value" || got.ContentVersion != 2 {
		t.Fatalf("newer envelope did not win: %+v", got)
	}
}

func TestApplyEnvelopeStaleVersionIsIgnored(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID:             "local1",
		Name:           "GitHub PAT",
		Placeholder:    "piso_github_pat_aaa",
		Value:          "newer-value",
		TechnocoreID:   "rid-1",
		PublishedHash:  plaintextHash("newer-value"),
		ContentVersion: 5,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	obj := testEnvelope(t, id, "rid-1", "GitHub PAT", "piso_github_pat_aaa", "stale-value")
	obj.ContentVersion = 4
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	got, _ := st.SecretByID("local1")
	if got.Value != "newer-value" || got.ContentVersion != 5 {
		t.Fatalf("stale envelope rolled local back: %+v", got)
	}
}

func TestApplyEnvelopeRevokedTombstones(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID:             "local1",
		Name:           "GitHub PAT",
		Placeholder:    "piso_github_pat_aaa",
		Value:          "ghp_secret",
		TechnocoreID:   "rid-1",
		PublishedHash:  plaintextHash("ghp_secret"),
		ContentVersion: 1,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	obj := testEnvelope(t, id, "rid-1", "GitHub PAT", "piso_github_pat_aaa", "ghp_secret")
	obj.ContentVersion = 2
	obj.Status = statusRevoked
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	got, ok := st.SecretByID("local1")
	if !ok || !got.Revoked || got.Value != "" || got.ContentVersion != 2 {
		t.Fatalf("tombstone not applied: %+v", got)
	}
	if recs := st.Secrets(); len(recs) != 0 {
		t.Fatalf("revoked secret still listed: %+v", recs)
	}
	// A stale republish of the old value must not resurrect it.
	stale := testEnvelope(t, id, "rid-1", "GitHub PAT", "piso_github_pat_aaa", "ghp_secret")
	stale.ContentVersion = 1
	if _, err := applyEnvelope(st, id, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.SecretByID("local1"); !got.Revoked {
		t.Fatalf("stale envelope resurrected a tombstone: %+v", got)
	}
}

func TestApplyEnvelopeGatewayOnlyKeepsNoPlaceholder(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	obj := testEnvelope(t, id, "rid-9", "Telegram bot", "", "123:abc")
	obj.ContentVersion = 1
	obj.Metadata["gatewayOnly"] = true
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	got, ok := st.SecretByID("tc_rid-9")
	if !ok || !got.GatewayOnly {
		t.Fatalf("gateway-only flag lost: %+v", got)
	}
	if got.Placeholder != "" {
		t.Fatalf("invented a placeholder for a gateway-only secret: %q", got.Placeholder)
	}
}
