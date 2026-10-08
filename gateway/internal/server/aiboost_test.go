package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBoostBrowserFlow(t *testing.T) {
	s, _, gwPub, _ := mcpFixture(t)
	const callback = "https://piso.example/oauth/ai-boost/callback"
	var exchanged bool
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register/":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"client_id": "test-client", "client_secret": "sensitive-client", "redirect_uris": []string{callback}})
		case "/oauth/token/":
			r.ParseForm()
			if r.Form.Get("redirect_uri") != callback || r.Form.Get("code_verifier") == "" || r.Form.Get("code") != "one-time-code" {
				t.Error("incorrect exchange", r.Form)
			}
			exchanged = true
			io.WriteString(w, `{"access_token":"sensitive-access","refresh_token":"sensitive-refresh","token_type":"Bearer","scope":"boosters:read boosters:suggest","expires_in":900,"refresh_token_expires_in":2592000}`)
		case "/api/library/boosters/22222222-2222-2222-2222-222222222222/":
			if r.Header.Get("Authorization") != "Bearer sensitive-access" {
				t.Error("worker read did not use gateway token")
			}
			io.WriteString(w, `{"id":"22222222-2222-2222-2222-222222222222","name":"Guidance","version":"v1","content":{"format":"markdown","body":"Worker guidance"}}`)
		case "/api/library/boosters/":
			if r.Header.Get("Authorization") != "Bearer sensitive-access" {
				t.Error("stored token was not used")
			}
			io.WriteString(w, `{"items":[],"next":null,"previous":null}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	s.HTTP = upstream.Client()
	h := s.WebHandler()
	reg := doJSON(t, h, "POST", "/api/v1/ai-boost/register", map[string]string{"baseUrl": upstream.URL, "redirectUri": callback})
	if reg.Code != 200 {
		t.Fatal(reg.Code, reg.Body.String())
	}
	start := doJSON(t, h, "POST", "/api/v1/ai-boost/authorize", map[string]string{})
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body.String())
	}
	var launch struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(start.Body.Bytes(), &launch) != nil {
		t.Fatal("missing start URL")
	}
	req := httptest.NewRequest("GET", launch.URL, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe binding cookie")
	}
	authorize, _ := url.Parse(w.Header().Get("Location"))
	if authorize.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE")
	}
	cb := callback + "?code=one-time-code&state=" + url.QueryEscape(authorize.Query().Get("state"))
	req = httptest.NewRequest("GET", cb, nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !exchanged {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sensitive-") {
		t.Fatal("callback leaked credentials")
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, req)
	if replay.Code != 400 {
		t.Fatal("state replay accepted")
	}
	status := doJSON(t, h, "GET", "/api/v1/ai-boost", nil)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"status":"ok"`) || strings.Contains(status.Body.String(), "sensitive-") {
		t.Fatal(status.Body.String())
	}
	connections := doJSON(t, h, "GET", "/api/v1/connections", nil)
	if !strings.Contains(connections.Body.String(), "read and suggest only") {
		t.Fatal(connections.Body.String())
	}
	disk, err := os.ReadFile(filepath.Join(s.DataDir, "aiboost", "aiboost.enc"))
	if err != nil || strings.Contains(string(disk), "sensitive-") {
		t.Fatal("vault not encrypted", err)
	}
	// Generic secret lists, placeholders and state export cannot see this vault.
	if len(s.Store.Secrets()) != 0 {
		t.Fatal("AI Boost tokens entered general vault")
	}
	if _, err := s.boost.List(context.Background(), "Go", ""); err != nil {
		t.Fatal("authenticated library request", err)
	}
	// Real encrypted worker channel -> coordinator authorization -> gateway vault -> provider.
	worker := registerLocal(t, s, "192.168.107.50")
	worker.PrincipalID = "envelope-principal"
	if _, err := s.Store.UpsertWorker(worker); err != nil {
		t.Fatal(err)
	}
	channel := s.mcpHandler()
	channel.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		if method != "GET" || path != "/api/v1/boosters/11111111-1111-1111-1111-111111111111" || query.Get("worker") != worker.PrincipalID {
			t.Fatalf("unexpected coordinator call: %s %s %v", method, path, query)
		}
		return 200, []byte(`{"id":"11111111-1111-1111-1111-111111111111","aiBoostId":"22222222-2222-2222-2222-222222222222"}`), nil
	}
	pub, priv := workerKeys(t)
	sealed, response := sealCall(t, gwPub, pub, priv, "tools/call", map[string]any{
		"name": "boost_get", "arguments": map[string]any{"id": "11111111-1111-1111-1111-111111111111"},
	}, time.Now())
	result := postMCP(t, channel, "192.168.107.50", sealed, nil)
	if result.Code != 200 {
		t.Fatal(result.Code)
	}
	plaintext, err := response.OpenResponse(result.Body.Bytes())
	if err != nil || !strings.Contains(string(plaintext), "Worker guidance") || strings.Contains(string(plaintext), "sensitive-") {
		t.Fatalf("worker response: %s (%v)", plaintext, err)
	}
	disconnect := doJSON(t, h, "POST", "/api/v1/ai-boost/disconnect", map[string]string{})
	if disconnect.Code != 204 {
		t.Fatal(disconnect.Code)
	}
	if s.boost.Summary().Status != "none" {
		t.Fatal("not disconnected")
	}
}
func TestBoostControlCSRFAndWorkerBoundary(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	for _, path := range []string{"register", "authorize", "disconnect"} {
		req := httptest.NewRequest("POST", "https://piso.example/api/v1/ai-boost/"+path, strings.NewReader(`{}`))
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		s.ControlHandler().ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("CSRF %s: %d", path, w.Code)
		}
	}
	s.DenyPeer = func(string) bool { return true }
	for _, path := range []string{"/api/v1/ai-boost", "/oauth/ai-boost/callback?state=x"} {
		w := doJSON(t, s.ControlHandler(), "GET", path, nil)
		if w.Code != 403 {
			t.Fatalf("worker reached control %s", path)
		}
	}
}
func TestBoostStartRequiresOneUseTicket(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	w := doJSON(t, s.ControlHandler(), "GET", "/oauth/ai-boost/start", nil)
	if w.Code == 302 {
		t.Fatal("unconfigured start allowed")
	}
}
