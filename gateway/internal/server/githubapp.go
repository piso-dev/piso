package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"piso/gateway/internal/githubapp"
	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

const githubAppLabel = "github-app"

func githubAppHost(r *http.Request) bool {
	return ingressHostname(r.Host) == githubAppLabel+".piso.local"
}

func githubAppPublicBase() string {
	port := ingressHostPort()
	if port == 80 {
		return "http://127.0.0.1"
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func githubAppRedirectURI() string {
	return githubAppPublicBase() + "/github-app/callback"
}

func githubAppSetupURI() string {
	return githubAppPublicBase() + "/github-app/setup"
}

func githubAppHomepage() string {
	port := ingressHostPort()
	if port == 80 {
		return "http://piso.local"
	}
	return fmt.Sprintf("http://piso.local:%d", port)
}

func (s *Server) githubClient() *githubapp.Client {
	return githubapp.NewClient(s.httpClient())
}

func (s *Server) registerGithubApp(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/github-app", s.handleListGithubApps)
	mux.HandleFunc("GET /api/v1/github-app/pending", s.handlePendingGithubApps)
	mux.HandleFunc("POST /api/v1/github-app", s.handlePutGithubApp)
	mux.HandleFunc("POST /api/v1/github-app/manifest", s.handleGithubManifest)
	mux.HandleFunc("GET /api/v1/github-app/{id}/install", s.handleGithubInstallURL)
	mux.HandleFunc("POST /api/v1/github-app/{id}/installations", s.handleAddGithubInstallation)
	mux.HandleFunc("DELETE /api/v1/github-app/{id}/installations/{iid}", s.handleDeleteGithubInstallation)
	mux.HandleFunc("POST /api/v1/github-app/{id}/refresh", s.handleRefreshGithubApp)
	mux.HandleFunc("DELETE /api/v1/github-app/{id}", s.handleDeleteGithubApp)
}

func (s *Server) handleListGithubApps(w http.ResponseWriter, r *http.Request) {
	recs := s.Store.GithubApps()
	out := make([]model.GithubApp, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.ToSummary())
	}
	writeJSON(w, 200, out)
}

func (s *Server) handlePendingGithubApps(w http.ResponseWriter, r *http.Request) {
	recs := s.Store.PendingGithubApps()
	out := make([]model.GithubApp, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.ToSummary())
	}
	writeJSON(w, 200, out)
}

func (s *Server) handlePutGithubApp(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string `json:"name"`
		AppID         int64  `json:"appId"`
		Slug          string `json:"slug"`
		PrivateKey    string `json:"privateKey"`
		Placeholder   string `json:"placeholder"`
		Installations []struct {
			ID          int64  `json:"id"`
			Account     string `json:"account"`
			AccountType string `json:"accountType"`
		} `json:"installations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.PrivateKey = strings.TrimSpace(in.PrivateKey)
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "piso-technocore"
	}
	if in.AppID == 0 || in.PrivateKey == "" {
		writeJSON(w, 400, map[string]string{"error": "appId and privateKey required"})
		return
	}
	if _, err := githubapp.ParseRSAPrivateKey([]byte(in.PrivateKey)); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid private key: " + err.Error()})
		return
	}
	rec, err := s.ensureGithubApp(in.Name, in.Placeholder)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	rec.AppID = in.AppID
	rec.Slug = strings.TrimSpace(in.Slug)
	rec.PrivateKey = in.PrivateKey
	if len(in.Installations) > 0 {
		inst := make([]store.GithubInstallation, 0, len(in.Installations))
		for _, i := range in.Installations {
			if i.ID == 0 || strings.TrimSpace(i.Account) == "" {
				continue
			}
			inst = append(inst, store.GithubInstallation{
				ID: i.ID, Account: strings.ToLower(strings.TrimSpace(i.Account)),
				AccountType: i.AccountType,
			})
		}
		rec.Installations = inst
	}
	saved, err := s.Store.UpsertGithubApp(rec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.replayGithub(r, saved)
	writeJSON(w, 200, saved.ToSummary())
}

func (s *Server) handleGithubManifest(w http.ResponseWriter, r *http.Request) {
	rec, err := s.ensureGithubApp("piso-technocore", "")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	state, err := randomURLToken(24)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	rec.ManifestState = state
	if _, err := s.Store.UpsertGithubApp(rec); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	mf := githubapp.NewManifest(rec.Name, githubAppHomepage(), githubAppRedirectURI(), githubAppSetupURI())
	raw, err := mf.JSON()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	action := "https://github.com/settings/apps/new?state=" + url.QueryEscape(state)
	writeJSON(w, 200, map[string]any{
		"id": rec.ID, "action": action, "state": state, "manifest": json.RawMessage(raw),
		"placeholder": rec.Placeholder, "status": rec.Status(),
	})
}

func (s *Server) handleGithubInstallURL(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Store.GithubAppByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	if rec.Slug == "" {
		writeJSON(w, 400, map[string]string{"error": "app slug unknown; finish manifest or set slug"})
		return
	}
	writeJSON(w, 200, map[string]string{
		"url": "https://github.com/apps/" + rec.Slug + "/installations/new",
	})
}

func (s *Server) handleAddGithubInstallation(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Store.GithubAppByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	var in struct {
		ID          int64  `json:"id"`
		Account     string `json:"account"`
		AccountType string `json:"accountType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if in.ID == 0 || strings.TrimSpace(in.Account) == "" {
		writeJSON(w, 400, map[string]string{"error": "id and account required"})
		return
	}
	rec = upsertInstallation(rec, store.GithubInstallation{
		ID: in.ID, Account: strings.ToLower(strings.TrimSpace(in.Account)), AccountType: in.AccountType,
	})
	saved, err := s.Store.UpsertGithubApp(rec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.replayGithub(r, saved)
	writeJSON(w, 200, saved.ToSummary())
}

func (s *Server) handleDeleteGithubInstallation(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Store.GithubAppByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad installation id"})
		return
	}
	out := rec.Installations[:0]
	for _, i := range rec.Installations {
		if i.ID != iid {
			out = append(out, i)
		}
	}
	rec.Installations = out
	saved, err := s.Store.UpsertGithubApp(rec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, saved.ToSummary())
}

func (s *Server) handleRefreshGithubApp(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Store.GithubAppByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	if rec.PrivateKey == "" || rec.AppID == 0 {
		writeJSON(w, 400, map[string]string{"error": "app not registered"})
		return
	}
	cli := s.githubClient()
	for i, inst := range rec.Installations {
		fresh, err := cli.FetchInstallation(rec, inst.ID)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		repos, err := cli.ListRepos(rec, fresh.Account)
		if err == nil {
			fresh.Repos = repos
		}
		rec.Installations[i] = fresh
	}
	saved, err := s.Store.UpsertGithubApp(rec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, saved.ToSummary())
}

func (s *Server) handleDeleteGithubApp(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteGithubApp(r.PathValue("id")); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleGithubAppHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/github-app/callback" || r.URL.Path == "/callback":
		s.handleGithubManifestCallback(w, r)
	case r.URL.Path == "/github-app/setup" || r.URL.Path == "/setup":
		s.handleGithubSetupCallback(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleGithubManifestCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}
	rec, ok := s.Store.GithubAppByState(state)
	if !ok {
		http.Error(w, "unknown state", http.StatusBadRequest)
		return
	}
	appID, slug, pem, clientID, clientSecret, hookSecret, err := s.githubClient().ConvertManifest(code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rec.AppID = appID
	rec.Slug = slug
	rec.PrivateKey = pem
	rec.ClientID = clientID
	rec.ClientSecret = clientSecret
	rec.WebhookSecret = hookSecret
	rec.ManifestState = ""
	if _, err := s.Store.UpsertGithubApp(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	next := "https://github.com/apps/" + url.PathEscape(slug) + "/installations/new"
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleGithubSetupCallback(w http.ResponseWriter, r *http.Request) {
	iid, _ := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if iid == 0 {
		http.Redirect(w, r, githubAppHomepage()+"/#github", http.StatusFound)
		return
	}
	apps := s.Store.GithubApps()
	if len(apps) == 0 {
		http.Redirect(w, r, githubAppHomepage()+"/#github", http.StatusFound)
		return
	}
	rec := apps[0]
	for _, a := range apps {
		if a.PrivateKey != "" {
			rec = a
			break
		}
	}
	inst, err := s.githubClient().FetchInstallation(rec, iid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if repos, err := s.githubClient().ListRepos(rec, inst.Account); err == nil {
		inst.Repos = repos
	}
	rec = upsertInstallation(rec, inst)
	saved, err := s.Store.UpsertGithubApp(rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.replayGithub(r, saved)
	http.Redirect(w, r, githubAppHomepage()+"/#github", http.StatusFound)
}

func (s *Server) ensureGithubApp(name, placeholder string) (store.GithubAppRec, error) {
	if apps := s.Store.GithubApps(); len(apps) > 0 {
		return apps[0], nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "piso-technocore"
	}
	if placeholder == "" {
		placeholder = "piso_github_app_" + sanitizeMCPName(name)
		if s.Store.PlaceholderTaken(placeholder) {
			placeholder = placeholder + "_" + randID()
		}
	}
	if !strings.HasPrefix(placeholder, model.PlaceholderPrefix) {
		return store.GithubAppRec{}, fmt.Errorf("placeholder must start with piso_")
	}
	envKey := "GH_TOKEN"
	if _, taken := s.Store.SecretByEnvKey(envKey); taken {
		envKey = store.DeriveEnvKey("GITHUB_APP_" + name)
	}
	now := time.Now().UTC()
	sec := store.SecretRec{
		ID:           "sec_" + randID(),
		Name:         name,
		Placeholder:  placeholder,
		Kind:         model.SecretKindGithubApp,
		EnvKey:       envKey,
		AllowedHosts: append([]string(nil), githubapp.AllowedHosts...),
		CreatedAt:    now,
	}
	if err := s.Store.AddSecret(sec); err != nil {
		return store.GithubAppRec{}, err
	}
	if err := s.Store.SyncRulesForSecret(sec); err != nil {
		return store.GithubAppRec{}, err
	}
	rec := store.GithubAppRec{
		ID: "gha_" + randID(), SecretID: sec.ID, Name: name,
		Placeholder: placeholder, CreatedAt: now, UpdatedAt: now,
	}
	return s.Store.UpsertGithubApp(rec)
}

func (s *Server) replayGithub(r *http.Request, rec store.GithubAppRec) {
	if rec.Placeholder == "" {
		return
	}
	seen := map[string]bool{}
	for _, h := range githubapp.AllowedHosts {
		s.replayMatches(r.Context(), seen, rec.Placeholder, h)
	}
}

func upsertInstallation(rec store.GithubAppRec, inst store.GithubInstallation) store.GithubAppRec {
	for i, existing := range rec.Installations {
		if existing.ID == inst.ID || strings.EqualFold(existing.Account, inst.Account) {
			if inst.Repos == nil {
				inst.Repos = existing.Repos
			}
			rec.Installations[i] = inst
			return rec
		}
	}
	rec.Installations = append(rec.Installations, inst)
	return rec
}
