package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestBoostConnectAutomaticallyRegisters(t *testing.T) {
	for _, port := range []string{"80", "9080"} {
		t.Run(port, func(t *testing.T) {
			t.Setenv("PISO_INGRESS_PORT", port)
			s := testServer(t)
			s.DataDir = t.TempDir()
			callback := "http://piso.local/oauth/ai-boost/callback"
			if port != "80" {
				callback = "http://piso.local:" + port + "/oauth/ai-boost/callback"
			}
			registrations := 0
			s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Scheme != "https" || r.URL.Host != "api.ai-boost.io" || r.URL.Path != "/oauth/register/" || r.Method != "POST" {
					t.Fatalf("unexpected provider request: %s %s", r.Method, r.URL)
				}
				var body struct {
					ClientName   string   `json:"client_name"`
					RedirectURIs []string `json:"redirect_uris"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ClientName != "Piso" || len(body.RedirectURIs) != 1 || body.RedirectURIs[0] != callback {
					t.Fatalf("wrong registration metadata: %+v", body)
				}
				registrations++
				w.WriteHeader(201)
				json.NewEncoder(w).Encode(map[string]any{"client_id": "auto-client", "client_secret": "private-test-secret", "redirect_uris": body.RedirectURIs})
			})}}
			// No /register call or configuration supplied by the dashboard.
			for i := 0; i < 2; i++ {
				r := httptest.NewRequest("POST", "http://piso.local/api/v1/ai-boost/authorize", nil)
				r.Header.Set("X-Forwarded-Host", "attacker.example")
				r.Header.Set("X-Forwarded-Proto", "https")
				w := httptest.NewRecorder()
				s.WebHandler().ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("connect: %d %s", w.Code, w.Body.String())
				}
				var launch struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &launch); err != nil {
					t.Fatal(err)
				}
				start, _ := url.Parse(launch.URL)
				expected, _ := url.Parse(callback)
				if start.Scheme != expected.Scheme || start.Host != expected.Host || start.Path != "/oauth/ai-boost/start" || start.Query().Get("ticket") == "" {
					t.Fatalf("wrong launch URL: %s", launch.URL)
				}
				w = httptest.NewRecorder()
				s.WebHandler().ServeHTTP(w, httptest.NewRequest("GET", launch.URL, nil))
				if w.Code != 302 {
					t.Fatalf("start: %d %s", w.Code, w.Body.String())
				}
				auth, _ := url.Parse(w.Header().Get("Location"))
				if auth.Scheme != "https" || auth.Host != "api.ai-boost.io" || auth.Path != "/oauth/authorize/" || auth.Query().Get("redirect_uri") != callback || auth.Query().Get("client_id") != "auto-client" || auth.Query().Get("code_challenge_method") != "S256" {
					t.Fatalf("incorrect authorization URL: %s", auth)
				}
			}
			if registrations != 1 {
				t.Fatalf("registration must be reused, got %d calls", registrations)
			}
		})
	}
}

func TestBoostAutomaticRegistrationFailureDoesNotLaunch(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_client_metadata"}`, 400)
	})}}
	w := doJSON(t, s.WebHandler(), "POST", "/api/v1/ai-boost/authorize", map[string]string{})
	if w.Code != 502 {
		t.Fatalf("want 502, got %d", w.Code)
	}
	c, err := s.boostClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.Summary().Configured || len(s.boostTickets) != 0 {
		t.Fatal("failed registration configured client or issued launch ticket")
	}
}
