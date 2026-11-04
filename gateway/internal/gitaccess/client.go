package gitaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Repository struct {
	Name         string `json:"name"`
	CloneURL     string `json:"cloneURL"`
	ConnectionID string `json:"connectionId"`
}
type ListingError struct {
	ConnectionID string `json:"connectionId"`
	Message      string `json:"message"`
}
type Listing struct {
	Repositories []Repository                 `json:"repositories"`
	Complete     bool                         `json:"complete"`
	Errors       []ListingError               `json:"errors"`
	Hints        map[string]map[string]string `json:"capabilityHints"`
}

func EmptyListing() Listing {
	return Listing{Repositories: []Repository{}, Complete: true, Errors: []ListingError{}, Hints: map[string]map[string]string{}}
}

// List uses a pinned API origin and constructs pagination locally. Neither
// redirects nor server-supplied clone/pagination URLs can redirect credentials.
// Errors never include request headers, credential values, or response bodies.
func List(ctx context.Context, client *http.Client, c Config, token, id string) Listing {
	out := EmptyListing()
	out.Hints[id] = c.Hints
	fail := func(message string) Listing {
		out.Complete = false
		out.Errors = append(out.Errors, ListingError{id, message})
		return out
	}
	normalized, _, err := c.Normalize()
	if err != nil {
		return fail("invalid Git connection configuration")
	}
	c = normalized
	if client == nil {
		client = http.DefaultClient
	}
	pinned := *client
	pinned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	path := "/user/repos"
	if c.Provider == "github" && c.TokenType == "installation" {
		path = "/installation/repositories"
	}
	for page := 1; page <= 1000; page++ {
		query := fmt.Sprintf("?page=%d&per_page=100", page)
		if c.Provider == "gitea" {
			query = fmt.Sprintf("?page=%d&limit=100", page)
		}
		req, err := http.NewRequestWithContext(ctx, "GET", c.APIBase()+path+query, nil)
		if err != nil {
			return fail("invalid repository endpoint")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "piso-git-discovery")
		resp, err := pinned.Do(req)
		if err != nil {
			return fail("repository listing failed; check connectivity or retry")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fail(fmt.Sprintf("repository listing returned HTTP %d; check the token or retry", resp.StatusCode))
		}
		if readErr != nil || len(raw) > 8<<20 {
			return fail("repository response could not be read")
		}
		type wireRepo struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
		}
		repos := []wireRepo{}
		if c.TokenType == "installation" {
			var envelope struct {
				Repositories []wireRepo `json:"repositories"`
			}
			err = json.Unmarshal(raw, &envelope)
			repos = envelope.Repositories
		} else {
			err = json.Unmarshal(raw, &repos)
		}
		if err != nil {
			return fail("invalid repository response")
		}
		for _, repo := range repos {
			// Build a credential-free HTTPS URL on the configured instance. Do not
			// trust clone_url, which can contain credentials or a different host.
			parts := strings.Split(repo.FullName, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
				continue
			}
			name := repo.FullName
			out.Repositories = append(out.Repositories, Repository{name, c.BaseURL + "/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + ".git", id})
		}
		// Page-size termination works for both providers without following Link
		// URLs. A full final page incurs one extra request for an empty page.
		if len(repos) < 100 {
			return out
		}
	}
	return fail("repository pagination exceeded the safety limit")
}
