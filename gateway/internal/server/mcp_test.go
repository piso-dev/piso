package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestWorkerAnnounceMCP(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	w := doJSON(t, wh, "POST", "/api/v1/worker/mcp", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo", "name": "ai-boost",
		"url": "https://mcp.example.com/mcp",
	})
	if w.Code != 201 {
		t.Fatalf("announce %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Name        string `json:"name"`
		Placeholder string `json:"placeholder"`
		URL         string `json:"url"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "ai-boost" || out.Placeholder != "piso_mcp_demo_ai-boost" {
		t.Fatalf("got %+v", out)
	}
	if out.Status != store.McpStatusNeedsAuth {
		t.Fatalf("status %s", out.Status)
	}
	if strings.Contains(w.Body.String(), "clientSecret") || strings.Contains(w.Body.String(), "refreshToken") {
		t.Fatal("secrets leaked in announce response")
	}
	again := doJSON(t, wh, "POST", "/api/v1/worker/mcp", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo", "name": "ai-boost",
		"url": "https://mcp.example.com/mcp",
	})
	if again.Code != 200 {
		t.Fatalf("idempotent %d %s", again.Code, again.Body.String())
	}
	var out2 struct {
		Placeholder string `json:"placeholder"`
	}
	json.Unmarshal(again.Body.Bytes(), &out2)
	if out2.Placeholder != out.Placeholder {
		t.Fatal("placeholder rotated on re-announce")
	}
}

func TestWorkerAnnounceMCPRejectsHTTP(t *testing.T) {
	s := testServer(t)
	w := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/mcp", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo",
		"url": "http://mcp.example.com/mcp",
	})
	if w.Code != 400 {
		t.Fatalf("want 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestWorkerAnnounceMCPRejectsCredentials(t *testing.T) {
	s := testServer(t)
	w := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/mcp", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo",
		"url": "https://user:pass@mcp.example.com/mcp",
	})
	if w.Code != 400 {
		t.Fatalf("want 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestMCPListOmitsSecrets(t *testing.T) {
	s := testServer(t)
	doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/mcp", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo", "name": "ai-boost",
		"url": "https://mcp.example.com/mcp",
	})
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/mcp", nil)
	if w.Code != 200 {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "clientSecret") || strings.Contains(w.Body.String(), "access_token") {
		t.Fatal("secrets in list")
	}
	pend := doJSON(t, s.ControlHandler(), "GET", "/api/v1/mcp/pending", nil)
	if pend.Code != 200 || !strings.Contains(pend.Body.String(), "needs-auth") {
		t.Fatalf("pending %d %s", pend.Code, pend.Body.String())
	}
}

func TestMCPOAuthCallbackStoresToken(t *testing.T) {
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "oauth-protected-resource"):
			json.NewEncoder(w).Encode(map[string]any{
				"resource":              "https://mcp.example.com/mcp",
				"authorization_servers": []string{"http://" + r.Host},
			})
		case strings.Contains(r.URL.Path, "oauth-authorization-server"):
			base := "http://" + r.Host
			json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 base,
				"authorization_endpoint": base + "/authorize",
				"token_endpoint":         base + "/token",
				"registration_endpoint":  base + "/register",
			})
		case r.URL.Path == "/register" && r.Method == "POST":
			json.NewEncoder(w).Encode(map[string]any{
				"client_id": "cid", "client_secret": "csecret",
			})
		case r.URL.Path == "/token" && r.Method == "POST":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "grant_type=authorization_code") {
				t.Errorf("token body %s", body)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "tok-real-access", "refresh_token": "tok-refresh",
				"expires_in": 3600, "token_type": "Bearer",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(as.Close)

	s := testServer(t)
	s.HTTP = as.Client()
	w := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/mcp", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo", "name": "boost",
		"url": "https://mcp.example.com/mcp",
	})
	if w.Code != 201 {
		t.Fatalf("announce %d %s", w.Code, w.Body.String())
	}
	var announced struct {
		Mcp struct {
			ID string `json:"id"`
		} `json:"mcp"`
	}
	json.Unmarshal(w.Body.Bytes(), &announced)
	rec, _ := s.Store.McpByID(announced.Mcp.ID)
	rec.URL = as.URL + "/mcp"
	rec.Host = "mcp.example.com"
	if err := s.Store.ReplaceMcpServer(rec); err != nil {
		t.Fatal(err)
	}

	auth := doJSON(t, s.ControlHandler(), "POST", "/api/v1/mcp/"+announced.Mcp.ID+"/authorize", map[string]any{})
	if auth.Code != 200 {
		t.Fatalf("authorize %d %s", auth.Code, auth.Body.String())
	}
	var authOut struct {
		URL string `json:"url"`
	}
	json.Unmarshal(auth.Body.Bytes(), &authOut)
	if !strings.Contains(authOut.URL, "code_challenge") || !strings.Contains(authOut.URL, "client_id=cid") {
		t.Fatalf("authorize url %s", authOut.URL)
	}
	if strings.Contains(auth.Body.String(), "csecret") {
		t.Fatal("client secret leaked")
	}
	rec, _ = s.Store.McpByID(announced.Mcp.ID)
	cb := httptest.NewRequest("GET", "/callback?code=abc&state="+rec.OAuthState, nil)
	cb.Host = "mcp-oauth.piso.local"
	rr := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(rr, cb)
	if rr.Code != 200 {
		t.Fatalf("callback %d %s", rr.Code, rr.Body.String())
	}
	rec, _ = s.Store.McpByID(announced.Mcp.ID)
	if rec.Status != store.McpStatusOK {
		t.Fatalf("status %s", rec.Status)
	}
	sec, ok := s.Store.SecretByID(rec.SecretID)
	if !ok || sec.Value != "tok-real-access" {
		t.Fatalf("secret %+v ok=%v", sec, ok)
	}
	list := doJSON(t, s.ControlHandler(), "GET", "/api/v1/mcp", nil)
	if strings.Contains(list.Body.String(), "tok-real-access") || strings.Contains(list.Body.String(), "tok-refresh") || strings.Contains(list.Body.String(), "csecret") {
		t.Fatalf("token leaked in list: %s", list.Body.String())
	}
}
