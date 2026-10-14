package technocore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

type fakeCoord struct {
	mu         sync.Mutex
	gateways   []remoteGateway
	posts      int
	puts       int
	resourceID string
	bodies     []map[string]any
}

func (f *fakeCoord) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/gateways":
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"gateways": f.gateways})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/secrets":
			f.mu.Lock()
			defer f.mu.Unlock()
			f.posts++
			f.record(r)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource_id":     f.resourceID,
				"content_version": 1,
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/secrets/"+f.resourceID:
			f.mu.Lock()
			defer f.mu.Unlock()
			f.puts++
			f.record(r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource_id":     f.resourceID,
				"content_version": 2,
			})
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeCoord) counts() (posts, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.puts
}

func (f *fakeCoord) postCount() int {
	posts, _ := f.counts()
	return posts
}

func (f *fakeCoord) setGateways(gws []remoteGateway) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateways = gws
}

// record keeps the latest publish body. Callers hold f.mu.
func (f *fakeCoord) record(r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		f.bodies = append(f.bodies, body)
	}
}

func (f *fakeCoord) lastBody(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("no publish body captured")
	}
	return f.bodies[len(f.bodies)-1]
}

func enrolledShareEnv(t *testing.T, srvURL string) (*store.Store, string, tc.Identity) {
	t.Helper()
	st := testStore(t)
	dataDir := t.TempDir()
	id, err := tc.Generate(srvURL, "gw")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	id.PrincipalID = "11111111-1111-1111-1111-111111111111"
	if err := tc.Save(tc.Path(dataDir), id); err != nil {
		t.Fatal(err)
	}
	return st, dataDir, id
}

func TestPublishAllSecretsSkipsUnchanged(t *testing.T) {
	coord := &fakeCoord{resourceID: "rid-stable"}
	srv := httptest.NewServer(coord.handler())
	defer srv.Close()

	st, dataDir, id := enrolledShareEnv(t, srv.URL)
	coord.setGateways([]remoteGateway{{
		PrincipalID:      id.PrincipalID,
		Name:             "gw",
		Status:           "active",
		EncryptionPubKey: id.EncryptionPublicKey,
		Fingerprint:      id.Fingerprint(),
	}})

	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	if err := st.AddSecret(store.SecretRec{
		ID:          "local1",
		Name:        "GitHub PAT",
		Placeholder: "piso_github_pat_aaa",
		Value:       "ghp_secret",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}
	if coord.postCount() != 1 {
		t.Fatalf("first publish posts=%d", coord.postCount())
	}
	got, ok := st.SecretByID("local1")
	if !ok || got.TechnocoreID != "rid-stable" || got.PublishedHash == "" {
		t.Fatalf("not stamped: %+v ok=%v", got, ok)
	}

	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}
	if coord.postCount() != 1 {
		t.Fatalf("unchanged republish posted again: posts=%d", coord.postCount())
	}
}

func TestPublishAllSecretsRepublishesOnNewRecipient(t *testing.T) {
	coord := &fakeCoord{resourceID: "rid-stable"}
	srv := httptest.NewServer(coord.handler())
	defer srv.Close()

	st, dataDir, id := enrolledShareEnv(t, srv.URL)
	other, err := tc.Generate(srv.URL, "other")
	if err != nil {
		t.Fatal(err)
	}
	coord.setGateways([]remoteGateway{{
		PrincipalID:      id.PrincipalID,
		Name:             "gw",
		Status:           "active",
		EncryptionPubKey: id.EncryptionPublicKey,
	}})

	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	if err := st.AddSecret(store.SecretRec{
		ID:          "local1",
		Name:        "GitHub PAT",
		Placeholder: "piso_github_pat_aaa",
		Value:       "ghp_secret",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}

	coord.setGateways([]remoteGateway{
		{
			PrincipalID:      id.PrincipalID,
			Name:             "gw",
			Status:           "active",
			EncryptionPubKey: id.EncryptionPublicKey,
		},
		{
			PrincipalID:      "22222222-2222-2222-2222-222222222222",
			Name:             "other",
			Status:           "active",
			EncryptionPubKey: other.EncryptionPublicKey,
		},
	})
	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}
	if coord.postCount() != 2 {
		t.Fatalf("new recipient should republish: posts=%d", coord.postCount())
	}
}

func TestPublishAllSecretsRotatesStampedSecret(t *testing.T) {
	coord := &fakeCoord{resourceID: "rid-stable"}
	srv := httptest.NewServer(coord.handler())
	defer srv.Close()

	st, dataDir, id := enrolledShareEnv(t, srv.URL)
	coord.setGateways([]remoteGateway{{
		PrincipalID:      id.PrincipalID,
		Name:             "gw",
		Status:           "active",
		EncryptionPubKey: id.EncryptionPublicKey,
	}})
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	if err := st.AddSecret(store.SecretRec{
		ID:          "local1",
		Name:        "GitHub PAT",
		Placeholder: "piso_github_pat_aaa",
		Value:       "ghp_secret",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}
	got, _ := st.SecretByID("local1")
	got.Value = "ghp_rotated"
	if err := st.ReplaceSecret(got); err != nil {
		t.Fatal(err)
	}
	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}
	posts, puts := coord.counts()
	if posts != 1 || puts != 1 {
		t.Fatalf("rotation should PUT the stamped resource: posts=%d puts=%d", posts, puts)
	}
	got, _ = st.SecretByID("local1")
	if got.PublishedHash != plaintextHash("ghp_rotated") {
		t.Fatalf("hash not restamped: %+v", got)
	}
}

// A gateway-only credential is not substituted into worker traffic, but it is
// still replicated: gateways are replicas of one entity, so the flag travels
// with the ciphertext instead of suppressing the publish.
func TestPublishAllSecretsReplicatesGatewayOnly(t *testing.T) {
	coord := &fakeCoord{resourceID: "rid-tg"}
	srv := httptest.NewServer(coord.handler())
	defer srv.Close()

	st, dataDir, id := enrolledShareEnv(t, srv.URL)
	coord.setGateways([]remoteGateway{{
		PrincipalID:      id.PrincipalID,
		Name:             "gw",
		Status:           "active",
		EncryptionPubKey: id.EncryptionPublicKey,
		Fingerprint:      id.Fingerprint(),
	}})

	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	// The Telegram bot token shape: gateway-only, and no placeholder at all.
	if err := st.AddSecret(store.SecretRec{
		ID:          "sec_tg",
		Name:        "Telegram bot",
		Kind:        "telegram",
		GatewayOnly: true,
		Value:       "123:abc",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}
	if coord.postCount() != 1 {
		t.Fatalf("gateway-only secret was not published: posts=%d", coord.postCount())
	}
	body := coord.lastBody(t)
	meta, _ := body["metadata"].(map[string]any)
	if meta["gatewayOnly"] != true {
		t.Fatalf("gatewayOnly did not travel: %+v", meta)
	}
	if meta["placeholder"] != "" {
		t.Fatalf("gateway-only secret published a placeholder: %+v", meta)
	}
	if _, ok := body["base_version"]; ok {
		t.Fatalf("never-published secret claimed a base version: %v", body["base_version"])
	}
	// A secret with no placeholder is still stamped by value so the next pass
	// skips it instead of re-publishing forever.
	got, ok := st.SecretByID("sec_tg")
	if !ok || got.TechnocoreID != "rid-tg" || got.ContentVersion != 1 {
		t.Fatalf("not stamped: %+v ok=%v", got, ok)
	}
}

// An edit to an already-published secret carries the version it is based on so
// the coordinator can reject a writer that is behind instead of overwriting a
// newer value.
func TestPublishAllSecretsSendsBaseVersion(t *testing.T) {
	coord := &fakeCoord{resourceID: "rid-stable"}
	srv := httptest.NewServer(coord.handler())
	defer srv.Close()

	st, dataDir, id := enrolledShareEnv(t, srv.URL)
	coord.setGateways([]remoteGateway{{
		PrincipalID:      id.PrincipalID,
		Name:             "gw",
		Status:           "active",
		EncryptionPubKey: id.EncryptionPublicKey,
		Fingerprint:      id.Fingerprint(),
	}})

	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	if err := st.AddSecret(store.SecretRec{
		ID:             "local1",
		Name:           "GitHub PAT",
		Placeholder:    "piso_github_pat_aaa",
		Value:          "ghp_new",
		TechnocoreID:   "rid-stable",
		PublishedHash:  plaintextHash("ghp_old"),
		ContentVersion: 7,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := publishAllSecrets(st, dataDir); err != nil {
		t.Fatal(err)
	}
	body := coord.lastBody(t)
	if body["base_version"] != float64(7) {
		t.Fatalf("base_version not sent: %+v", body["base_version"])
	}
}

func TestSameRecipientsIgnoresOrder(t *testing.T) {
	if !sameRecipients([]string{"b", "a"}, []string{"a", "b"}) {
		t.Fatal("expected equal")
	}
	if sameRecipients([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("expected unequal")
	}
}
