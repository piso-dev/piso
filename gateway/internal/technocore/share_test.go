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
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource_id":     f.resourceID,
				"content_version": 1,
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/secrets/"+f.resourceID:
			f.mu.Lock()
			defer f.mu.Unlock()
			f.puts++
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

func TestSameRecipientsIgnoresOrder(t *testing.T) {
	if !sameRecipients([]string{"b", "a"}, []string{"a", "b"}) {
		t.Fatal("expected equal")
	}
	if sameRecipients([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("expected unequal")
	}
}
