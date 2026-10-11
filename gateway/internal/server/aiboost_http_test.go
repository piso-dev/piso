package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
)

// This uses actual HTTP listeners and a cookie jar, rather than invoking
// handlers directly, to verify the browser's start/callback cookie path.
func TestBoostHTTPRoundTrip(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	gateway := httptest.NewServer(s.WebHandler())
	defer gateway.Close()
	callback := gateway.URL + "/oauth/ai-boost/callback"
	var tokenCalls int
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register/":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"client_id": "piso", "client_secret": "secret", "redirect_uris": []string{callback}})
		case "/oauth/token/":
			tokenCalls++
			io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","scope":"boosters:read drafts:write drafts:review","expires_in":900,"refresh_token_expires_in":2592000}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	s.HTTP = provider.Client()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, payload any) *http.Response {
		t.Helper()
		body, _ := json.Marshal(payload)
		resp, err := browser.Post(gateway.URL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post("/api/v1/ai-boost/register", map[string]string{"baseUrl": provider.URL, "redirectUri": callback})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("registration", resp.StatusCode)
	}
	resp = post("/api/v1/ai-boost/authorize", map[string]string{})
	var start struct {
		URL string `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	resp, err := browser.Get(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatal("start", resp.StatusCode)
	}
	auth, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = browser.Get(callback + "?code=code&state=" + url.QueryEscape(auth.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || tokenCalls != 1 {
		t.Fatal("callback", resp.StatusCode, tokenCalls)
	}
	resp, err = browser.Get(gateway.URL + "/api/v1/ai-boost")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if status.Status != "ok" {
		t.Fatal("not connected", status)
	}
	// The one-use launch ticket cannot set another browser cookie.
	resp, err = browser.Get(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal("launch ticket replay accepted")
	}
}
