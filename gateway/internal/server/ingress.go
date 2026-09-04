package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"piso/gateway/internal/store"
)

const defaultIngressHostPort = 8082

var workerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,78}$`)

// ingressView is the dashboard/worker JSON for one pending (or already-live) plan.
type ingressView struct {
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Worker  string `json:"worker"`
	Slug    string `json:"slug,omitempty"`
	Port    int    `json:"port"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Created bool   `json:"created,omitempty"`
	Note    string `json:"note,omitempty"`
}

type ingressCancelIn struct {
	Worker string `json:"worker"`
	Kind   string `json:"kind"`
}

func (s *Server) handleListIngress(w http.ResponseWriter, r *http.Request) {
	pending := s.Store.PendingIngress()
	out := make([]ingressView, 0, len(pending))
	for _, rec := range pending {
		out = append(out, viewIngress(rec, false))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateIngress(w http.ResponseWriter, r *http.Request) {
	var in store.IngressRequestRec
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	in.Kind = strings.TrimSpace(in.Kind)
	in.Note = strings.TrimSpace(in.Note)
	if in.Kind == "" {
		in.Kind = store.IngressKindPlanning
	}
	if in.Kind != store.IngressKindPlanning {
		writeJSON(w, 400, map[string]string{"error": "kind must be planning"})
		return
	}
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if in.Port == 0 {
		in.Port = store.DefaultPlanningPort
	}
	if in.Port < 1 || in.Port > 65535 {
		writeJSON(w, 400, map[string]string{"error": "invalid port"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	in.Name = planningRouteName(in.Slug)
	in.Status = store.IngressStatusPending

	in.ID = "ing_" + randID()
	rec, created, err := s.Store.UpsertPendingIngress(in)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, viewIngress(rec, created))
}

func (s *Server) handleApproveIngress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, route, err := s.Store.ApproveIngress(id, "route_"+randID())
	if err != nil {
		if errors.Is(err, store.ErrIngressNotFound) {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		if errors.Is(err, store.ErrIngressLabelTaken) {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	url := ingressPublicURL(route.Name)
	writeJSON(w, 200, map[string]any{
		"request": viewIngress(rec, false),
		"route":   route,
		"url":     url,
	})
}

func (s *Server) handleDismissIngress(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DismissIngress(r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrIngressNotFound) {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleCancelIngress(w http.ResponseWriter, r *http.Request) {
	var in ingressCancelIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	n := s.Store.CancelPendingIngress(in.Worker, strings.TrimSpace(in.Kind))
	writeJSON(w, 200, map[string]int{"cancelled": n})
}

func viewIngress(rec store.IngressRequestRec, created bool) ingressView {
	return ingressView{
		ID: rec.ID, Kind: rec.Kind, Status: rec.Status, Worker: rec.Worker,
		Slug: rec.Slug, Port: rec.Port, Name: rec.Name, Note: rec.Note,
		URL: ingressPublicURL(rec.Name), Created: created,
	}
}

func planningRouteName(slug string) string {
	label := sanitizeDNSLabel(slug)
	if label == "" {
		label = "worker"
	}
	name := "plan-" + label
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

func slugFromWorker(worker string) string {
	const prefix = "piso-worker-"
	if strings.HasPrefix(worker, prefix) {
		return worker[len(prefix):]
	}
	return worker
}

func sanitizeDNSLabel(raw string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func ingressHostPort() int {
	v := strings.TrimSpace(os.Getenv("PISO_INGRESS_PORT"))
	if v == "" {
		return defaultIngressHostPort
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return defaultIngressHostPort
	}
	return n
}

func ingressPublicURL(name string) string {
	// Apex piso.local is already in /etc/hosts. Subdomains are not
	// (hosts has no wildcards), so Open plan uses the apex plus ?route=.
	port := ingressHostPort()
	q := url.QueryEscape(name)
	if port == 80 {
		return fmt.Sprintf("http://piso.local/?route=%s", q)
	}
	return fmt.Sprintf("http://piso.local:%d/?route=%s", port, q)
}

const (
	ingressRouteCookie = "piso_route"
	ingressRouteQuery  = "route"
)

func ingressHostname(host string) string {
	host = strings.ToLower(host)
	if i := strings.LastIndex(host, "]"); i >= 0 {
		// [::1]:8082
		if i+1 < len(host) && host[i+1] == ':' {
			return host[:i+1]
		}
		return host
	}
	if i := strings.Index(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

func validIngressLabel(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0 && i < len(name)-1)
		if !ok {
			return false
		}
	}
	return true
}

func isIngressApex(host string) bool {
	switch host {
	case "piso.local", "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return false
	}
}

// ingressRouteName is the label to look up: ?route=, cookie on the apex, or
// the first label of name.piso.local.
func ingressRouteName(r *http.Request) string {
	if q := strings.TrimSpace(r.URL.Query().Get(ingressRouteQuery)); validIngressLabel(q) {
		return q
	}
	host := ingressHostname(r.Host)
	if isIngressApex(host) {
		if c, err := r.Cookie(ingressRouteCookie); err == nil && validIngressLabel(c.Value) {
			return c.Value
		}
		return ""
	}
	name := host
	if i := strings.Index(host, "."); i >= 0 {
		name = host[:i]
	}
	if !validIngressLabel(name) {
		return ""
	}
	return name
}
