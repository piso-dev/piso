package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"piso/gateway/internal/store"
)

func TestMCPReauthorizeAfterRegistryLoss(t *testing.T) {
	registrations := 0
	activeClient := ""
	challenge := ""
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/register":
			registrations++
			activeClient = fmt.Sprintf("client-%d", registrations)
			json.NewEncoder(w).Encode(map[string]string{"client_id": activeClient, "client_secret": "test-secret"})
		case "/authorize":
			if activeClient == "" || r.URL.Query().Get("client_id") != activeClient {
				http.Error(w, "Client Not Registered", http.StatusBadRequest)
				return
			}
			challenge = r.URL.Query().Get("code_challenge")
			if challenge == "" || r.URL.Query().Get("code_challenge_method") != "S256" {
				t.Error("missing PKCE challenge")
			}
			u, err := url.Parse(r.URL.Query().Get("redirect_uri"))
			if err != nil {
				t.Error(err)
				return
			}
			q := u.Query()
			q.Set("state", r.URL.Query().Get("state"))
			q.Set("code", "test-code")
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusFound)
		case "/token":
			r.ParseForm()
			id, secret, ok := r.BasicAuth()
			if !ok || id != activeClient || secret != "test-secret" || r.Form.Get("client_id") != activeClient || r.Form.Get("code") != "test-code" || pkceS256(r.Form.Get("code_verifier")) != challenge {
				http.Error(w, "invalid token request", 400)
				return
			}
			// No refresh token: the old registration's token must not survive.
			json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer as.Close()
	s := testServer(t)
	s.HTTP = as.Client()
	browser := as.Client()
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	rec, _, err := s.upsertMCP("piso-worker-demo", "demo", "boost", "https://mcp.example.com/mcp", "", "")
	if err != nil {
		t.Fatal(err)
	}
	rec.AuthURL = as.URL + "/authorize"
	rec.TokenURL = as.URL + "/token"
	rec.RegisterURL = as.URL + "/register"
	rec.ClientID = "forgotten-client"
	rec.ClientSecret = "old-secret"
	rec.RefreshToken = "old-refresh"
	rec.Status = store.McpStatusOK
	if err := s.Store.ReplaceMcpServer(rec); err != nil {
		t.Fatal(err)
	}
	for i, step := range []string{"reauthorize", "after-revoke"} {
		activeClient = "" // Simulate upstream registry loss, preserving gateway state.
		if step == "after-revoke" {
			r := doJSON(t, s.ControlHandler(), "POST", "/api/v1/mcp/"+rec.ID+"/revoke", map[string]any{})
			if r.Code != 200 {
				t.Fatalf("revoke: %d", r.Code)
			}
		}
		r := doJSON(t, s.ControlHandler(), "POST", "/api/v1/mcp/"+rec.ID+"/authorize", map[string]any{})
		if r.Code != 200 {
			t.Fatalf("authorize: %d %s", r.Code, r.Body.String())
		}
		var out struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		resp, err := browser.Get(out.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("%s: authorization failed: %d", step, resp.StatusCode)
		}
		if registrations != i+1 {
			t.Fatalf("registration count: %d", registrations)
		}
		cb := httptest.NewRequest("GET", resp.Header.Get("Location"), nil)
		result := httptest.NewRecorder()
		s.WebHandler().ServeHTTP(result, cb)
		if result.Code != 200 {
			t.Fatalf("callback: %d %s", result.Code, result.Body.String())
		}
		stored, _ := s.Store.McpByID(rec.ID)
		sec, _ := s.Store.SecretByID(stored.SecretID)
		if stored.Status != store.McpStatusOK || stored.RefreshToken != "" || stored.ClientID != activeClient || sec.Value != "new-access" || stored.OAuthState != "" || stored.Placeholder != rec.Placeholder {
			t.Fatal("new registration/token not stored correctly")
		}
	}
}

func TestMCPFailedReregistrationPreservesCredentials(t *testing.T) {
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer as.Close()
	s := testServer(t)
	s.HTTP = as.Client()
	rec, _, err := s.upsertMCP("piso-worker-demo", "demo", "boost", "https://mcp.example.com/mcp", "", "")
	if err != nil {
		t.Fatal(err)
	}
	rec.AuthURL, rec.TokenURL, rec.RegisterURL = as.URL+"/authorize", as.URL+"/token", as.URL+"/register"
	rec.ClientID, rec.ClientSecret, rec.RefreshToken = "old-id", "old-secret", "old-refresh"
	if err := s.Store.ReplaceMcpServer(rec); err != nil {
		t.Fatal(err)
	}
	r := doJSON(t, s.ControlHandler(), "POST", "/api/v1/mcp/"+rec.ID+"/authorize", map[string]any{})
	if r.Code != 502 {
		t.Fatalf("want 502, got %d", r.Code)
	}
	stored, _ := s.Store.McpByID(rec.ID)
	if stored.ClientID != rec.ClientID || stored.ClientSecret != rec.ClientSecret || stored.RefreshToken != rec.RefreshToken {
		t.Fatal("failed registration changed credentials")
	}
}
