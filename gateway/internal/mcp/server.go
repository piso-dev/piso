// Package mcp is the gateway side of the encrypted worker MCP channel.
// POST /mcp opens, verifies, binds the caller, and dispatches. Failures
// before dispatch are bare HTTP statuses with an empty body.
package mcp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/model"
	"piso/gateway/internal/patterns"
	"piso/gateway/internal/scanner"
	"piso/gateway/internal/store"
	"piso/internal/mcpchannel"
	tc "piso/internal/technocore"
)

// Handler serves POST /mcp and GET /mcp/key.
type Handler struct {
	Store    *store.Store
	Patterns *patterns.Compiled
	DataDir  string
	// Identity loads the gateway identity. Tests inject a fixed one.
	Identity func() (tc.Identity, error)
	// Now is injectable for freshness tests.
	Now func() time.Time
	// OnCredential is called when a write tool's arguments match a credential
	// pattern. The pattern id is logged; the value is not.
	OnCredential func(worker, patternID string)

	nonces    *nonceCache
	tools     map[string]Tool
	toolNames []string
	// Helpers are piso/ methods. They are not listed in tools/list.
	Helpers map[string]HelperFunc
	// Upsert registers a remote MCP server. The server package injects it.
	Upsert func(worker, slug, name, rawURL string) (store.McpServerRec, bool, error)
	// CoordinatorDo is the signed coordinator client. Tests inject a fake.
	CoordinatorDo CoordinatorDo
	// GithubRepos lists repository full_names using gateway-owned credentials.
	GithubRepos func(context.Context, store.GithubAppRec, string) ([]string, error)
	// AI Boost is authoritative; these use gateway credentials, not coordinator grants.
	BoostList            func(context.Context, string, string) (aiboost.Page, error)
	BoostGet             func(context.Context, string) (aiboost.Booster, error)
	BoostSuggest         func(context.Context, aiboost.Draft, string) (aiboost.Suggestion, error)
	BoostEditSuggestion  func(context.Context, string, aiboost.SuggestionEdit) (aiboost.Suggestion, error)
	BoostListSuggestions func(context.Context, string, string) (aiboost.SuggestionPage, error)
	BoostGetSuggestion   func(context.Context, string) (aiboost.Suggestion, error)
	BoostSuggestVersion  func(context.Context, aiboost.VersionDraft, string) (aiboost.Suggestion, error)
}

// Tool is one MCP tool. Caller always comes from the envelope.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Write is true when arguments are scanned for credential patterns.
	Write bool
	Call  func(caller store.WorkerRec, args json.RawMessage) (any, *rpcError)
}

// New builds a handler with the phase-1 tool list.
func New(st *store.Store, pat *patterns.Compiled, dataDir string) *Handler {
	h := &Handler{
		Store:    st,
		Patterns: pat,
		DataDir:  dataDir,
		Now:      time.Now,
		nonces:   newNonceCache(2 * tc.AuthMaxSkew),
		tools:    map[string]Tool{},
	}
	h.Identity = func() (tc.Identity, error) {
		return tc.Load(tc.Path(dataDir))
	}
	h.Helpers = map[string]HelperFunc{}
	h.Register(Tool{
		Name:        "whoami",
		Description: "Return the worker bound to this channel and the gateway fingerprint.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Call:        h.toolWhoami,
	})
	h.Register(Tool{
		Name:        "mcp_add",
		Description: "Register a remote MCP server with the gateway. Returns the placeholder the worker writes into mcp.json.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
				"url":  map[string]any{"type": "string"},
			},
			"required": []string{"url"},
		},
		Write: true,
		Call:  h.toolMCPAdd,
	})
	h.registerInbox()
	h.registerBoard()
	h.registerIdentity()
	h.registerGithub()
	h.registerBoost()
	h.registerIngress()
	h.registerAsks()
	return h
}

// Register adds or replaces a tool. First registration order is tools/list order.
func (h *Handler) Register(t Tool) {
	if _, ok := h.tools[t.Name]; !ok {
		h.toolNames = append(h.toolNames, t.Name)
	}
	h.tools[t.Name] = t
}

// ServeHTTP dispatches GET /mcp/key and POST /mcp.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/mcp/key":
		h.serveKey(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/mcp":
		h.serveMCP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) serveKey(w http.ResponseWriter, r *http.Request) {
	id, pin, err := h.pin()
	if err != nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":     pin.Version,
		"x25519":      pin.X25519,
		"keyId":       pin.KeyID,
		"signing":     pin.Signing,
		"status":      id.Status,
		"fingerprint": id.Fingerprint(),
	})
}

func (h *Handler) serveMCP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpchannel.MaxRequest+1))
	if err != nil {
		bare(w, http.StatusBadRequest)
		return
	}
	if len(body) > mcpchannel.MaxRequest {
		bare(w, http.StatusRequestEntityTooLarge)
		return
	}
	id, pin, err := h.pin()
	if err != nil {
		bare(w, http.StatusServiceUnavailable)
		return
	}
	version, keyID, err := mcpchannel.ParseOuter(body)
	if err != nil || version != mcpchannel.Version {
		bare(w, http.StatusBadRequest)
		return
	}
	if hex.EncodeToString(keyID) != strings.ToLower(pin.KeyID) {
		bare(w, http.StatusMisdirectedRequest)
		return
	}
	gwPriv, err := hex.DecodeString(id.EncryptionPrivateKey)
	if err != nil {
		bare(w, http.StatusServiceUnavailable)
		return
	}
	gwPub, err := pin.X25519Bytes()
	if err != nil {
		bare(w, http.StatusServiceUnavailable)
		return
	}
	opened, err := mcpchannel.OpenRequest(gwPriv, body)
	if err != nil {
		bare(w, http.StatusBadRequest)
		return
	}
	now := h.Now()
	if now.IsZero() {
		now = time.Now()
	}
	skew := now.Sub(time.Unix(opened.TS, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > tc.AuthMaxSkew {
		bare(w, http.StatusConflict)
		return
	}
	if h.nonces.Add(mcpchannel.ReplayKey(opened.Pub, opened.Nonce), now) {
		bare(w, http.StatusConflict)
		return
	}
	if !opened.Verify(gwPub) {
		bare(w, http.StatusUnauthorized)
		return
	}
	caller, ok := h.bindCaller(r, hex.EncodeToString(opened.Pub))
	if !ok {
		bare(w, http.StatusUnauthorized)
		return
	}
	h.dispatch(w, r, opened, caller)
}

// bindCaller applies local trust-on-first-use or the remote coordinator key.
// Tool arguments never choose the caller.
func (h *Handler) bindCaller(r *http.Request, pub string) (store.WorkerRec, bool) {
	pub = strings.ToLower(pub)
	if tok := bearer(r); tok != "" {
		sum := sha256Hex(tok)
		rec, ok := h.Store.WorkerByTokenHash(sum)
		if !ok || !rec.Remote {
			return store.WorkerRec{}, false
		}
		if !strings.EqualFold(rec.Pubkey, pub) {
			return store.WorkerRec{}, false
		}
		return rec, true
	}
	ip := requestIP(r)
	rec, ok := h.Store.WorkerByIP(ip)
	if !ok || rec.Remote {
		return store.WorkerRec{}, false
	}
	if rec.ChannelPubkey == "" {
		bound, err := h.Store.BindChannelKey(rec.Name, pub)
		if err != nil {
			log.Printf("mcp: bind %s: %v", rec.Name, err)
			return store.WorkerRec{}, false
		}
		log.Printf("mcp: bound %s key %s", rec.Name, mcpchannel.Fingerprint([]byte(pub)))
		return bound, true
	}
	if strings.EqualFold(rec.ChannelPubkey, pub) {
		return rec, true
	}
	h.raiseRebind(rec, pub)
	return store.WorkerRec{}, false
}

func (h *Handler) raiseRebind(rec store.WorkerRec, pub string) {
	note := rec.ChannelPubkey + " → " + pub
	_, _, err := h.Store.UpsertPendingIngress(store.IngressRequestRec{
		ID:     "ing_" + randHex(8),
		Kind:   store.IngressKindRebind,
		Status: store.IngressStatusPending,
		Worker: rec.Name,
		Slug:   rec.Slug,
		Name:   "rebind-" + rec.Slug,
		Note:   note,
		Port:   1,
	})
	if err != nil {
		log.Printf("mcp: rebind inbox %s: %v", rec.Name, err)
		return
	}
	log.Printf("mcp: %s presented a new key", rec.Name)
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request, opened *mcpchannel.Request, caller store.WorkerRec) {
	var req rpcRequest
	if err := json.Unmarshal(opened.Content, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		h.seal(w, opened, caller, rpcFail(req.ID, codeInvalidRequest, "invalid request"), "invalid")
		return
	}
	if isBatch(opened.Content) {
		h.seal(w, opened, caller, rpcFail(req.ID, codeInvalidRequest, "batches are not supported"), "batch")
		return
	}
	if req.Method == "notifications/initialized" || req.isNotification() && strings.HasPrefix(req.Method, "notifications/") {
		h.logCall(caller, req.Method, "ok", len(opened.Content))
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var resp rpcResponse
	switch req.Method {
	case "initialize":
		resp = rpcOK(req.ID, h.initializeResult(req.Params))
	case "ping":
		resp = rpcOK(req.ID, map[string]any{})
	case "tools/list":
		resp = rpcOK(req.ID, map[string]any{"tools": h.toolList()})
	case "resources/list":
		resp = rpcOK(req.ID, map[string]any{"resources": []any{}})
	case "prompts/list":
		resp = rpcOK(req.ID, map[string]any{"prompts": []any{}})
	case "tools/call":
		resp = h.callTool(caller, req)
	default:
		if strings.HasPrefix(req.Method, "piso/") {
			resp = h.callHelper(caller, req)
		} else {
			resp = rpcFail(req.ID, codeMethodNotFound, "method not found")
		}
	}
	outcome := "ok"
	if resp.Error != nil {
		outcome = resp.Error.Message
	}
	h.seal(w, opened, caller, resp, outcome)
}

func (h *Handler) seal(w http.ResponseWriter, opened *mcpchannel.Request, caller store.WorkerRec, resp rpcResponse, outcome string) {
	raw := marshalRPC(resp)
	if len(raw) > mcpchannel.MaxResponse {
		bare(w, http.StatusRequestEntityTooLarge)
		return
	}
	sealed, err := opened.SealResponse(raw)
	if err != nil {
		log.Printf("mcp: seal %s: %v", caller.Name, err)
		bare(w, http.StatusInternalServerError)
		return
	}
	method := outcome
	h.logCall(caller, method, outcome, len(raw))
	w.Header().Set("Content-Type", mcpchannel.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sealed)
}

func (h *Handler) logCall(caller store.WorkerRec, method, outcome string, n int) {
	log.Printf("mcp: worker=%s method=%s outcome=%s bytes=%d", caller.Slug, method, outcome, n)
	if h.Store == nil {
		return
	}
	action := "allow"
	if outcome != "ok" && outcome != "" {
		action = "error"
	}
	h.Store.AppendLog(store.Record{
		ID:      "mcp_" + randHex(6),
		Worker:  caller.Name,
		Slug:    caller.Slug,
		Ts:      time.Now().UTC(),
		Method:  "POST",
		Scheme:  "mcp",
		Host:    "gateway",
		Path:    "/mcp",
		Status:  200,
		Action:  action,
		Reasons: []string{method},
	})
}

func (h *Handler) initializeResult(params json.RawMessage) map[string]any {
	version := ProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	if supportedVersions[p.ProtocolVersion] {
		version = p.ProtocolVersion
	}
	id, _, _ := h.pin()
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":        "piso",
			"version":     "1",
			"fingerprint": id.Fingerprint(),
		},
		"instructions": "piso gateway channel. Tools act as the worker bound to this connection.",
	}
}

func (h *Handler) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(h.toolNames))
	for _, name := range h.toolNames {
		t, ok := h.tools[name]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return out
}

func (h *Handler) callTool(caller store.WorkerRec, req rpcRequest) rpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		return rpcFail(req.ID, codeInvalidParams, "name required")
	}
	t, ok := h.tools[p.Name]
	if !ok {
		return rpcFail(req.ID, codeMethodNotFound, "unknown tool")
	}
	if t.Write && h.blockedCredential(caller, p.Arguments) {
		return rpcFail(req.ID, codeServer, "credential pattern blocked")
	}
	result, rpcErr := t.Call(caller, p.Arguments)
	if rpcErr != nil {
		return rpcFail(req.ID, rpcErr.Code, rpcErr.Message)
	}
	raw, _ := json.Marshal(result)
	return rpcOK(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(raw)}},
	})
}

func (h *Handler) blockedCredential(caller store.WorkerRec, args json.RawMessage) bool {
	if h.Patterns == nil || len(args) == 0 {
		return false
	}
	var res scanner.Result
	if h.Patterns != nil {
		for _, m := range h.Patterns.Match(args) {
			res.PatternHits = append(res.PatternHits, model.Finding{
				Kind: model.FindingPatternSecret, PatternID: m.PatternID, Token: m.Sample, Location: "body",
			})
		}
	}
	if !res.LooksCredential() {
		return false
	}
	id := res.PatternHits[0].PatternID
	log.Printf("mcp: blocked credential pattern %s from %s", id, caller.Name)
	if h.OnCredential != nil {
		h.OnCredential(caller.Name, id)
	}
	if h.Store != nil {
		h.Store.AppendLog(store.Record{
			ID:      "mcp_" + randHex(6),
			Worker:  caller.Name,
			Slug:    caller.Slug,
			Ts:      time.Now().UTC(),
			Method:  "POST",
			Scheme:  "mcp",
			Host:    "gateway",
			Path:    "/mcp",
			Status:  200,
			Action:  "block",
			Reasons: []string{"credential pattern " + id},
			Findings: []store.Finding{{
				Kind: string(model.FindingPatternSecret), PatternID: id, Token: res.PatternHits[0].Token, Location: "body",
			}},
		})
	}
	return true
}

func (h *Handler) toolWhoami(caller store.WorkerRec, _ json.RawMessage) (any, *rpcError) {
	id, _, err := h.pin()
	if err != nil {
		return nil, &rpcError{Code: codeInternal, Message: "identity unavailable"}
	}
	pub := caller.ChannelPubkey
	if pub == "" {
		pub = caller.Pubkey
	}
	fp := pub
	if raw, err := hex.DecodeString(pub); err == nil {
		fp = mcpchannel.Fingerprint(raw)
	}
	return map[string]any{
		"worker":             caller.Name,
		"slug":               caller.Slug,
		"channelFingerprint": fp,
		"gatewayFingerprint": id.Fingerprint(),
	}, nil
}

func (h *Handler) toolMCPAdd(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.URL) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "url required"}
	}
	if h.Upsert == nil {
		return nil, &errNoUpsert
	}
	rec, created, err := h.Upsert(caller.Name, caller.Slug, in.Name, in.URL)
	if err != nil {
		return nil, &rpcError{Code: codeServer, Message: err.Error()}
	}
	return map[string]any{
		"name":        rec.Name,
		"placeholder": rec.Placeholder,
		"status":      rec.Status,
		"created":     created,
	}, nil
}

// Upsert registers a remote MCP server. The server package injects upsertMCP.
var errNoUpsert = rpcError{Code: codeInternal, Message: "mcp_add unavailable"}

// HelperFunc handles a piso/ method. Not listed in tools/list.
type HelperFunc func(caller store.WorkerRec, params json.RawMessage) (any, error)

// Helpers are piso/ methods for shell wrappers. Phase 1 registers none of
// the watchers; the map exists so later phases can attach them.
func (h *Handler) callHelper(caller store.WorkerRec, req rpcRequest) rpcResponse {
	if h.Helpers == nil {
		return rpcFail(req.ID, codeMethodNotFound, "method not found")
	}
	fn := h.Helpers[req.Method]
	if fn == nil {
		return rpcFail(req.ID, codeMethodNotFound, "method not found")
	}
	out, err := fn(caller, req.Params)
	if err != nil {
		return rpcFail(req.ID, codeServer, err.Error())
	}
	return rpcOK(req.ID, out)
}

// Helpers is the piso/ method table.
func (h *Handler) pin() (tc.Identity, mcpchannel.Pin, error) {
	load := h.Identity
	if load == nil {
		load = func() (tc.Identity, error) { return tc.Load(tc.Path(h.DataDir)) }
	}
	id, err := load()
	if err != nil {
		return tc.Identity{}, mcpchannel.Pin{}, err
	}
	enc, err := hex.DecodeString(id.EncryptionPublicKey)
	if err != nil {
		return tc.Identity{}, mcpchannel.Pin{}, err
	}
	sign, err := hex.DecodeString(id.SigningPublicKey)
	if err != nil {
		return tc.Identity{}, mcpchannel.Pin{}, err
	}
	return id, mcpchannel.PinFromPublic(enc, sign), nil
}

func bare(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

func isBatch(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\n', '\r', '\t':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

func bearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Piso-Worker-Token"))
}

func requestIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}
