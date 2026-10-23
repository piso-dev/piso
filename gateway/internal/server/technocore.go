package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
	tc "piso/internal/technocore"
)

func (s *Server) registerTechnocore(mux *http.ServeMux) {
	// The surrounding control handler is host-only (loopback ingress), and
	// CrossOriginProtection rejects browser CSRF. The confirm route in
	// particular must never be reachable from a worker or a random origin:
	// it is the human consent step that mints the principal.
	csrf := http.NewCrossOriginProtection()
	mount := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, csrf.Handler(h)) }

	mount("GET /api/v1/technocore", s.handleTechnocoreStatus)
	mount("POST /api/v1/technocore/pair", s.handleTechnocorePair)
	mount("POST /api/v1/technocore/pair/confirm", s.handleTechnocoreConfirm)
	mount("POST /api/v1/technocore/pair/decline", s.handleTechnocoreDecline)
	mount("POST /api/v1/technocore/pair/cancel", s.handleTechnocoreCancel)
	mount("GET /api/v1/technocore/pair/qr.svg", s.handleTechnocoreQR)
	mount("POST /api/v1/technocore/share", s.handleTechnocoreShare)
	mount("POST /api/v1/technocore/publish", s.handleTechnocorePublish)
}

func (s *Server) handleTechnocoreStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.technocoreStatus())
}

// technocoreStatus is Snapshot plus the vault counts the dashboard shows once
// the gateway is live.
func (s *Server) technocoreStatus() technocore.Status {
	out := technocore.Snapshot(s.dataDir())
	if s.Store != nil {
		out.Secrets = secretCounts(s.Store)
	}
	return out
}

func secretCounts(st *store.Store) technocore.Counts {
	var c technocore.Counts
	for _, rec := range st.Secrets() {
		c.Total++
		if strings.HasPrefix(rec.ID, "tc_") {
			c.Inbound++
		}
		switch {
		case rec.TechnocoreID != "" || rec.PublishedHash != "":
			c.Published++
		case rec.Value != "" && !rec.Revoked:
			c.Pending++
		}
	}
	return c
}

func (s *Server) handleTechnocorePair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ServerURL string `json:"serverUrl"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}

	path := tc.Path(s.dataDir())
	id, err := tc.Load(path)
	if err != nil {
		if !os.IsNotExist(err) {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		host, _ := os.Hostname()
		id, err = tc.LoadOrCreate(path, "", host)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	if id.Status == tc.StatusActive {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this gateway is already paired; run `piso gateway launch` to re-enroll it",
		})
		return
	}

	serverURL, err := normalizeCoordinatorURL(in.ServerURL)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = id.Name
	}
	if name == "" {
		host, _ := os.Hostname()
		name = host
	}
	if len(name) > 80 {
		writeJSON(w, 400, map[string]string{"error": "name is too long"})
		return
	}

	// Reuse a live request on the same coordinator so a double-click or a
	// page reload does not burn another slot in the coordinator's rate limit.
	if id.Status == tc.StatusPending && id.PairingID != "" &&
		strings.TrimRight(id.ServerURL, "/") == serverURL && !pairExpired(id.PairExpiresAt) {
		writeJSON(w, 200, s.technocoreStatus())
		return
	}

	if err := probeCoordinator(serverURL); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	id.ServerURL = serverURL
	id.Name = name
	if id.Placement != tc.PlacementStandalone {
		// The dashboard only ever enrolls a local gateway. Standalone is a
		// hosting decision, and that stays on the CLI.
		id.Placement = tc.PlacementLocal
	}
	if _, err := tc.StartPairing(path, id); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, s.technocoreStatus())
}

func (s *Server) handleTechnocoreConfirm(w http.ResponseWriter, r *http.Request) {
	path := tc.Path(s.dataDir())
	id, err := tc.Load(path)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "no pairing request"})
		return
	}
	if id.Status == tc.StatusActive {
		writeJSON(w, 200, s.technocoreStatus())
		return
	}
	if id.Status != tc.StatusPending || id.PairingID == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no pairing request is waiting"})
		return
	}
	if id.PairPhase != tc.PairStatusAwaitingGateway {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "approve on your phone first; this gateway has not been approved yet",
		})
		return
	}
	principal, err := tc.ConfirmPairing(id)
	if err != nil {
		var needsLogin *tc.NeedsLoginError
		if errors.As(err, &needsLogin) {
			// The phone approved without signing in. Nothing is minted yet;
			// the operator finishes it in a signed-in browser and this
			// gateway's poller sees the approval a moment later.
			confirm := strings.TrimRight(id.ServerURL, "/") + needsLogin.ConfirmURL
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      err.Error(),
				"needsLogin": true,
				"confirmUrl": confirm,
			})
			return
		}
		if err == tc.ErrPollToken {
			_ = technocore.ResetPairing(s.dataDir())
			writeJSON(w, http.StatusGone, map[string]string{
				"error": "the pairing expired; show a new code",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if _, err := tc.Update(path, func(cur *tc.Identity) error {
		if cur.PairingID != id.PairingID {
			return nil
		}
		cur.Status = tc.StatusActive
		cur.PrincipalID = principal
		cur.PairPhase = ""
		cur.PairExpiresAt = ""
		cur.PairApprover = ""
		cur.PairAccount = ""
		return nil
	}); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, s.technocoreStatus())
}

func (s *Server) handleTechnocoreDecline(w http.ResponseWriter, r *http.Request) {
	path := tc.Path(s.dataDir())
	id, err := tc.Load(path)
	if err != nil || id.PairingID == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no pairing request is waiting"})
		return
	}
	if id.Status == tc.StatusActive {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this gateway is already paired"})
		return
	}
	if err := tc.DeclinePairing(id); err != nil && err != tc.ErrPollToken {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if _, err := tc.Update(path, func(cur *tc.Identity) error {
		if cur.PairingID != id.PairingID {
			return nil
		}
		cur.Status = tc.StatusNew
		cur.PairingID = ""
		cur.PollToken = ""
		cur.PairPhase = ""
		cur.PairExpiresAt = ""
		cur.PairApprover = ""
		cur.PairAccount = ""
		return nil
	}); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, s.technocoreStatus())
}

func (s *Server) handleTechnocoreCancel(w http.ResponseWriter, r *http.Request) {
	// Cancel is local-only on purpose: the coordinator keeps the request until
	// it expires, and another device watching that page must not think this
	// gateway enrolled.
	if err := technocore.ResetPairing(s.dataDir()); err != nil {
		if err == technocore.ErrAlreadyPaired {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, s.technocoreStatus())
}

func (s *Server) handleTechnocoreQR(w http.ResponseWriter, r *http.Request) {
	st := s.technocoreStatus()
	if st.PairURL == "" {
		writeJSON(w, 404, map[string]string{"error": "no pairing code"})
		return
	}
	svg, err := qrSVG(st.PairURL)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(svg)
}

func (s *Server) handleTechnocoreShare(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string   `json:"name"`
		Kind         string   `json:"kind"`
		Placeholder  string   `json:"placeholder"`
		EnvKey       string   `json:"envKey"`
		AllowedHosts []string `json:"allowedHosts"`
		Value        string   `json:"value"`
		Recipients   []string `json:"recipients"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if in.Value == "" || in.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "name and value required"})
		return
	}
	out, err := technocore.Share(s.Store, s.dataDir(), technocore.ShareInput{
		Name:         in.Name,
		Kind:         in.Kind,
		Placeholder:  in.Placeholder,
		EnvKey:       in.EnvKey,
		AllowedHosts: in.AllowedHosts,
		Plaintext:    in.Value,
		Recipients:   in.Recipients,
	})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleTechnocorePublish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Placeholder string   `json:"placeholder"`
		Recipients  []string `json:"recipients"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if in.Placeholder == "" {
		writeJSON(w, 400, map[string]string{"error": "placeholder required"})
		return
	}
	out, err := technocore.PublishPlaceholder(s.Store, s.dataDir(), in.Placeholder, in.Recipients)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) dataDir() string {
	if s.DataDir != "" {
		return s.DataDir
	}
	return ".piso"
}

// normalizeCoordinatorURL accepts an http(s) origin with no path, query, or
// fragment. The pair page always lives at /pair/<uuid>/ relative to it.
func normalizeCoordinatorURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("enter the coordinator URL, for example https://piso.dev")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("that is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("the coordinator URL must start with http:// or https://")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("the coordinator URL needs a host")
	}
	if u.User != nil {
		return "", fmt.Errorf("remove the username from the URL")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("enter only the origin, for example https://piso.dev")
	}
	if err := guardAddress(u.Hostname()); err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

// guardAddress refuses cloud metadata and other link-local targets. A
// coordinator on a LAN or on this machine is legitimate (self-hosted), so
// private ranges stay allowed.
func guardAddress(host string) error {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "metadata" || lower == "metadata.google.internal" || lower == "instance-data" {
		return fmt.Errorf("that host is not allowed")
	}
	if ip := net.ParseIP(lower); ip != nil {
		if unsafeAddress(ip) {
			return fmt.Errorf("that address is not allowed")
		}
		return nil
	}
	return nil
}

func unsafeAddress(ip net.IP) bool {
	return ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

// probeCoordinator checks that the URL answers like a piso coordinator before
// the gateway mints a request against it. The failure is reported here rather
// than as a broken QR.
func probeCoordinator(base string) error {
	client := &http.Client{Timeout: 8 * time.Second}
	res, err := client.Get(base + "/health/")
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", base, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != 200 {
		return fmt.Errorf("%s answered %d; check the URL", base, res.StatusCode)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Status != "ok" {
		return fmt.Errorf("%s does not look like a piso coordinator", base)
	}
	return nil
}

func pairExpired(ts string) bool {
	if ts == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Now().After(t)
}
