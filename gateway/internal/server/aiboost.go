package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"piso/gateway/internal/aiboost"
)

func (s *Server) boostClient() (*aiboost.Client, error) {
	s.boostMu.Lock()
	defer s.boostMu.Unlock()
	if s.boost == nil {
		if s.DataDir == "" {
			return nil, errors.New("gateway data directory unavailable")
		}
		c, err := aiboost.Open(filepath.Join(s.DataDir, "aiboost"), s.HTTP)
		if err != nil {
			return nil, errors.New("AI Boost vault unavailable")
		}
		s.boost = c
	}
	return s.boost, nil
}
func (s *Server) boostConnection() connectionView {
	out := connectionView{ID: "ai-boost", Name: "AI Boost", Status: "none", Detail: "Not connected"}
	if s.DataDir == "" {
		return out
	}
	c, err := s.boostClient()
	if err != nil {
		out.Status = "error"
		out.Detail = "AI Boost vault unavailable"
		return out
	}
	v := c.Summary()
	out.Status = v.Status
	switch v.Status {
	case "ok":
		out.Detail = "Connected · read and suggest only"
	case "needs-auth":
		out.Detail = "Reconnect needed"
	}
	return out
}
func (s *Server) registerAIBoost(mux *http.ServeMux) {
	// The surrounding control handler is host-only. CrossOriginProtection also
	// rejects browser CSRF; workers never get these routes on WorkerHandler.
	csrf := http.NewCrossOriginProtection()
	mount := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, csrf.Handler(h)) }
	mount("GET /api/v1/ai-boost", s.handleBoostStatus)
	mount("POST /api/v1/ai-boost/register", s.handleBoostRegister)
	mount("POST /api/v1/ai-boost/authorize", s.handleBoostAuthorize)
	mount("POST /api/v1/ai-boost/disconnect", s.handleBoostDisconnect)
}
func boostHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
func (s *Server) handleBoostStatus(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	c, err := s.boostClient()
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	writeJSON(w, 200, c.Summary())
}
func (s *Server) handleBoostRegister(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	var in struct {
		BaseURL     string `json:"baseUrl"`
		RedirectURI string `json:"redirectUri"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil {
		http.Error(w, "baseUrl and redirectUri required", 400)
		return
	}
	c, err := s.boostClient()
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	if err = c.Configure(r.Context(), in.BaseURL, in.RedirectURI); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, c.Summary())
}
func (s *Server) handleBoostAuthorize(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	c, err := s.boostClient()
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	summary := c.Summary()
	if !summary.Configured {
		http.Error(w, "Configure AI Boost first", 400)
		return
	}
	ticket, err := randomURLToken(32)
	if err != nil {
		http.Error(w, "Sign-in unavailable", 500)
		return
	}
	s.boostMu.Lock()
	if s.boostTickets == nil {
		s.boostTickets = map[string]time.Time{}
	}
	for k, expiry := range s.boostTickets {
		if time.Now().After(expiry) {
			delete(s.boostTickets, k)
		}
	}
	if len(s.boostTickets) >= 32 {
		s.boostMu.Unlock()
		http.Error(w, "Too many sign-ins", 429)
		return
	}
	s.boostTickets[ticket] = time.Now().Add(time.Minute)
	s.boostMu.Unlock()
	u, _ := url.Parse(summary.RedirectURI)
	u.Path = "/oauth/ai-boost/start"
	u.RawQuery = url.Values{"ticket": {ticket}}.Encode()
	writeJSON(w, 200, map[string]string{"url": u.String()})
}
func (s *Server) handleBoostOAuth(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	c, err := s.boostClient()
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	summary := c.Summary()
	u, err := url.Parse(summary.RedirectURI)
	if err != nil || !summary.Configured || !strings.EqualFold(r.Host, u.Host) {
		http.Error(w, "Use the configured AI Boost callback origin", 400)
		return
	}
	if r.URL.Path == "/oauth/ai-boost/start" {
		ticket := r.URL.Query().Get("ticket")
		s.boostMu.Lock()
		expiry, ok := s.boostTickets[ticket]
		delete(s.boostTickets, ticket)
		s.boostMu.Unlock()
		if !ok || !time.Now().Before(expiry) {
			http.Error(w, "Start sign-in from Connections", 400)
			return
		}
		browser, err := randomURLToken(32)
		if err != nil {
			http.Error(w, "Sign-in unavailable", 500)
			return
		}
		auth, err := c.Start(browser)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "piso_aiboost_oauth", Value: browser, Path: "/oauth/ai-boost/", HttpOnly: true, Secure: u.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: 600})
		http.Redirect(w, r, auth, 302)
		return
	}
	if r.URL.Path != aiboost.CallbackPath {
		http.NotFound(w, r)
		return
	}
	cookie, _ := r.Cookie("piso_aiboost_oauth")
	browser := ""
	if cookie != nil {
		browser = cookie.Value
	}
	q := r.URL.Query()
	err = c.Callback(r.Context(), q.Get("state"), browser, q.Get("code"), q.Get("error"))
	http.SetCookie(w, &http.Cookie{Name: "piso_aiboost_oauth", Value: "", Path: "/oauth/ai-boost/", HttpOnly: true, Secure: u.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("AI Boost connected. Close this tab and return to Connections in the Piso dashboard."))
}
func (s *Server) handleBoostDisconnect(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	c, err := s.boostClient()
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	s.boostMu.Lock()
	s.boostTickets = nil
	s.boostMu.Unlock()
	if err = c.Disconnect(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}
