package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

func (h *Handler) registerGithub() {
	h.Register(Tool{
		Name:        "github_repositories",
		Description: "List sorted, unique HTTPS repository URLs across all installed GitHub Apps available to this worker. Fetches every repository page; complete=false and errors identify installations that could not be listed. No credentials are returned.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		Call:        h.toolGithubRepositories,
	})
}

type githubRepositoryError struct {
	AppID   string `json:"appId"`
	Account string `json:"account"`
	Message string `json:"message"`
}

type githubRepositoriesResult struct {
	Repositories []string                `json:"repositories"`
	Complete     bool                    `json:"complete"`
	Errors       []githubRepositoryError `json:"errors"`
}

func (h *Handler) toolGithubRepositories(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	if len(args) != 0 {
		var in map[string]json.RawMessage
		if err := json.Unmarshal(args, &in); err != nil || len(in) != 0 {
			return nil, &rpcError{Code: codeInvalidParams, Message: "github_repositories takes no arguments"}
		}
	}
	if caller.InternetDisabled {
		return nil, &rpcError{Code: codeServer, Message: "internet access is disabled for this worker"}
	}
	if h.Store == nil || h.GithubRepos == nil {
		return nil, &rpcError{Code: codeInternal, Message: "github_repositories unavailable"}
	}

	out := githubRepositoriesResult{
		Repositories: []string{}, Complete: true, Errors: []githubRepositoryError{},
	}
	seen := map[string]bool{}
	// Bound the entire enumeration, not just an individual page or app.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, app := range h.Store.GithubApps() {
		sec, ok := h.Store.SecretByID(app.SecretID)
		if !ok || sec.Kind != model.SecretKindGithubApp || !store.SecretVisibleToWorker(sec, caller.Slug) || app.Status() != store.GithubAppStatusOK {
			continue
		}
		for _, inst := range app.Installations {
			// Pin the installation even if account names are duplicated in state.
			candidate := app
			candidate.Installations = []store.GithubInstallation{inst}
			repos, err := h.GithubRepos(ctx, candidate, inst.Account)
			if err != nil {
				out.Complete = false
				// Upstream error bodies may contain credentials. Never relay them.
				out.Errors = append(out.Errors, githubRepositoryError{
					AppID: app.ID, Account: inst.Account, Message: "repository listing failed; check GitHub App access or retry",
				})
				continue
			}
			for _, name := range repos {
				if name == "" {
					continue
				}
				url := "https://github.com/" + name
				if !seen[url] {
					seen[url] = true
					out.Repositories = append(out.Repositories, url)
				}
			}
		}
	}
	sort.Strings(out.Repositories)
	return out, nil
}
