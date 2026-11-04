package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"piso/gateway/internal/gitaccess"
	"piso/gateway/internal/store"
)

func (h *Handler) registerGit() {
	h.Register(Tool{Name: "user_repositories", Description: "List HTTPS repositories discoverable with Git tokens assigned to this worker. Returns connection identifiers and operator-provided capability hints (unverified, not enforced by Piso). Pagination failures return complete=false and errors. A listing does not prove write access. No credentials are returned.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, Call: h.toolUserRepositories})
}
func (h *Handler) toolUserRepositories(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if len(args) > 0 {
		var in map[string]json.RawMessage
		if json.Unmarshal(args, &in) != nil || len(in) != 0 {
			return nil, &rpcError{Code: codeInvalidParams, Message: "user_repositories takes no arguments"}
		}
	}
	if caller.InternetDisabled {
		return nil, &rpcError{Code: codeServer, Message: "internet access is disabled for this worker"}
	}
	if h.Store == nil {
		return nil, &rpcError{Code: codeInternal, Message: "repository discovery unavailable"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := gitaccess.EmptyListing()
	seen := map[string]bool{}
	for _, rec := range h.Store.Secrets() {
		if rec.Kind != gitaccess.Kind || rec.Git == nil || !store.SecretVisibleToWorker(rec, caller.Slug) {
			continue
		}
		listing := gitaccess.List(ctx, h.GitHTTPClient, *rec.Git, rec.Value, rec.ID)
		out.Complete = out.Complete && listing.Complete
		out.Errors = append(out.Errors, listing.Errors...)
		out.Hints[rec.ID] = rec.Git.Hints
		for _, repo := range listing.Repositories {
			key := repo.ConnectionID + "\x00" + repo.CloneURL
			if !seen[key] {
				seen[key] = true
				out.Repositories = append(out.Repositories, repo)
			}
		}
	}
	sort.Slice(out.Repositories, func(i, j int) bool {
		a, b := out.Repositories[i], out.Repositories[j]
		if a.CloneURL == b.CloneURL {
			return a.ConnectionID < b.ConnectionID
		}
		return a.CloneURL < b.CloneURL
	})
	return out, nil
}
