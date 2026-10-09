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
	editCalls := 0
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
			io.WriteString(w, `{"access_token":"sensitive-access","refresh_token":"sensitive-refresh","token_type":"Bearer","scope":"boosters:read boosters:suggest suggestions:review","expires_in":900,"refresh_token_expires_in":2592000}`)
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
		case "/api/booster-suggestions/11111111-1111-1111-1111-111111111111/", "/api/booster-suggestions/11111111-1111-1111-1111-111111111111/approve/":
			status := "submitted"
			if r.Method == "POST" {
				if r.Header.Get("Authorization") != "Bearer sensitive-access" {
					t.Error("gateway token missing")
				}
				var d struct {
					ExpectedRevision int `json:"expectedRevision"`
				}
				json.NewDecoder(r.Body).Decode(&d)
				if d.ExpectedRevision != 3 {
					w.WriteHeader(409)
					return
				}
				status = "converted"
			} else if r.Header.Get("Authorization") != "Bearer sensitive-access" {
				t.Error("agent token missing")
			}
			writeJSON(w, 200, map[string]any{"id": "11111111-1111-1111-1111-111111111111", "status": status, "revision": 3, "content": map[string]string{"format": "markdown", "body": "# Review"}})
		case "/api/booster-suggestions/33333333-3333-3333-3333-333333333333/":
			editCalls++
			if r.Method != "PATCH" || r.Header.Get("Authorization") != "Bearer sensitive-access" || r.Header.Get("Idempotency-Key") != "" || r.URL.RawQuery != "" {
				t.Error("incorrect edit request")
			}
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 3 || payload["expectedRevision"] != float64(1) || payload["name"] != "Updated suggestion" || payload["changeSummary"] != "Clarify name" {
				t.Error("incorrect edit payload", payload)
			}
			io.WriteString(w, `{"id":"33333333-3333-3333-3333-333333333333","status":"submitted","revision":2,"name":"Updated suggestion","description":"Original description","content":{"format":"markdown","body":"# Original body"},"suggestedTags":[],"rationale":"Useful","destination":{"tagId":"library-id","name":"Library","visibility":"private"}}`)
		case "/api/booster-suggestions/":
			if r.Method == "GET" {
				io.WriteString(w, `{"items":[],"next":null,"previous":null}`)
				return
			}
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer sensitive-access" || r.Header.Get("Idempotency-Key") != "stable-submission" {
				t.Error("incorrect suggestion request")
			}
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["name"] != "Go" {
				t.Error("incorrect suggestion payload")
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"suggestion-id","status":"submitted","revision":1}`)
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
	if !strings.Contains(connections.Body.String(), "worker suggestions and dashboard review") {
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
	// Real encrypted worker channel -> gateway vault -> provider, without coordinator pairing.
	registerLocal(t, s, "192.168.107.50")
	channel := s.mcpHandler()
	channel.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		t.Fatalf("boost called coordinator: %s %s", method, path)
		return 0, nil, nil
	}
	pub, priv := workerKeys(t)
	sealed, response := sealCall(t, gwPub, pub, priv, "tools/call", map[string]any{
		"name": "boost_get", "arguments": map[string]any{"id": "22222222-2222-2222-2222-222222222222"},
	}, time.Now())
	result := postMCP(t, channel, "192.168.107.50", sealed, nil)
	if result.Code != 200 {
		t.Fatal(result.Code)
	}
	plaintext, err := response.OpenResponse(result.Body.Bytes())
	if err != nil || !strings.Contains(string(plaintext), "Worker guidance") || strings.Contains(string(plaintext), "sensitive-") {
		t.Fatalf("worker response: %s (%v)", plaintext, err)
	}
	for _, call := range []struct {
		name     string
		args     map[string]any
		expected string
	}{
		{"boost_search", map[string]any{"q": "Go"}, "nextCursor"},
		{"boost_suggest", map[string]any{"name": "Go", "description": "Go guidance", "rationale": "Useful", "content": map[string]any{"format": "markdown", "body": "# Go"}, "idempotencyKey": "stable-submission"}, "suggestion-id"},
	} {
		sealed, response := sealCall(t, gwPub, pub, priv, "tools/call", map[string]any{"name": call.name, "arguments": call.args}, time.Now())
		result := postMCP(t, channel, "192.168.107.50", sealed, nil)
		if result.Code != 200 {
			t.Fatal(result.Code)
		}
		plaintext, err := response.OpenResponse(result.Body.Bytes())
		if err != nil || !strings.Contains(string(plaintext), call.expected) || strings.Contains(string(plaintext), "sensitive-") {
			t.Fatalf("%s: %s (%v)", call.name, plaintext, err)
		}
	}
	// Exercise the new tool through the actual encrypted handler and HTTP client.
	sealed, response = sealCall(t, gwPub, pub, priv, "tools/call", map[string]any{
		"name": "boost_edit_suggestion", "arguments": map[string]any{
			"id": "33333333-3333-3333-3333-333333333333", "expectedRevision": 1,
			"name": "Updated suggestion", "changeSummary": "Clarify name",
		},
	}, time.Now())
	result = postMCP(t, channel, "192.168.107.50", sealed, nil)
	if result.Code != 200 {
		t.Fatal(result.Code)
	}
	plaintext, err = response.OpenResponse(result.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var edited struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(plaintext, &edited) != nil || len(edited.Error) != 0 || len(edited.Result.Content) != 1 || edited.Result.Content[0].Type != "text" {
		t.Fatalf("invalid edit RPC response: %s", plaintext)
	}
	const wantEdit = `{"id":"33333333-3333-3333-3333-333333333333","status":"submitted","revision":2,"name":"Updated suggestion","description":"Original description","content":{"format":"markdown","body":"# Original body"},"suggestedTags":[],"rationale":"Useful","destination":{"tagId":"library-id","name":"Library","visibility":"private"}}`
	if edited.Result.Content[0].Text != wantEdit || editCalls != 1 {
		t.Fatalf("edit response = %s, calls = %d", edited.Result.Content[0].Text, editCalls)
	}
	// Dashboard decisions use the same gateway credential but are never worker tools.
	for _, path := range []string{"/api/v1/ai-boost", "/api/v1/ai-boost/suggestions", "/api/v1/ai-boost/suggestions/11111111-1111-1111-1111-111111111111"} {
		got := doJSON(t, h, "GET", path, nil)
		if got.Code != 200 || strings.Contains(got.Body.String(), "sensitive-access") {
			t.Fatal(path, got.Code, got.Body.String())
		}
	}
	decisionPath := "/api/v1/ai-boost/suggestions/11111111-1111-1111-1111-111111111111/approve"
	stale := doJSON(t, h, "POST", decisionPath, map[string]any{"expectedRevision": 2})
	if stale.Code != 409 {
		t.Fatal(stale.Code, stale.Body.String())
	}
	invalid := doJSON(t, h, "POST", decisionPath, map[string]any{"expectedRevision": 3, "content": "overwrite"})
	if invalid.Code != 400 {
		t.Fatal(invalid.Code)
	}
	approved := doJSON(t, h, "POST", decisionPath, map[string]any{"expectedRevision": 3, "feedback": "Good"})
	if approved.Code != 200 || !strings.Contains(approved.Body.String(), "converted") {
		t.Fatal(approved.Code, approved.Body.String())
	}
	workerResult := doJSON(t, s.WorkerHandler(), "POST", decisionPath, map[string]any{"expectedRevision": 3})
	if workerResult.Code == 200 {
		t.Fatal("worker reached review route")
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
	for _, path := range []string{"register", "authorize", "disconnect", "suggestions/11111111-1111-1111-1111-111111111111/approve", "suggestions/11111111-1111-1111-1111-111111111111/reject"} {
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
	for _, path := range []string{"/api/v1/ai-boost", "/api/v1/ai-boost/suggestions", "/api/v1/ai-boost/suggestions/11111111-1111-1111-1111-111111111111", "/oauth/ai-boost/callback?state=x"} {
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
