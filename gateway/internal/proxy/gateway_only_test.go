package proxy

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"piso/gateway/internal/model"
	"piso/gateway/internal/policy"
	"piso/gateway/internal/scanner"
	"piso/gateway/internal/store"
	"strings"
	"testing"
)

func TestGatewayOnlyHTTPProxyBlocksEvenWithWildcardRule(t *testing.T) {
	for _, gatewayOnly := range []bool{false, true} {
		h, st := testHandler(t)
		sec := store.SecretRec{ID: "s", Placeholder: "piso_github_test", Value: "test-real-value", GatewayOnly: gatewayOnly, Workers: []string{"allowed"}}
		if err := st.AddSecret(sec); err != nil {
			t.Fatal(err)
		}
		if err := st.AddRule(store.RuleRec{ID: "r", SecretID: "s", Placeholder: sec.Placeholder, Host: "*"}); err != nil {
			t.Fatal(err)
		}
		for _, worker := range []string{"allowed", "other", ""} {
			h.Worker = func(*http.Request) string { return worker }
			calls := 0
			h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ExampleUser:"+sec.Value))
				if req.Header.Get("Authorization") != want {
					t.Fatal("missing Basic Auth substitution")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})}
			req := httptest.NewRequest("GET", "http://api.github.com/ExampleUser/piso/info/refs?service=git-upload-pack", nil)
			req.SetBasicAuth("ExampleUser", sec.Placeholder)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if gatewayOnly || worker != "allowed" {
				if calls != 0 || w.Code != 407 {
					t.Fatalf("gatewayOnly=%v worker=%q status=%d calls=%d", gatewayOnly, worker, w.Code, calls)
				}
			} else if calls != 1 || w.Code != 200 {
				t.Fatalf("allowed request: %d calls %d", w.Code, calls)
			}
		}
	}
}

func TestGatewayOnlyAndWorkerScopeSurviveProxySnapshot(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "state.json"), "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		gatewayOnly bool
		workers     []string
		worker      string
		allow       bool
	}{
		{"gateway nil", true, nil, "alpha", false},
		{"gateway wildcard", true, []string{"*"}, "alpha", false},
		{"gateway named", true, []string{"alpha"}, "alpha", false},
		{"gateway anonymous", true, nil, "", false},
		{"scoped denied", false, []string{"beta"}, "alpha", false},
		{"scoped allowed", false, []string{"alpha"}, "alpha", true},
		{"legacy all", false, nil, "alpha", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec := store.SecretRec{ID: "s", Name: "test", Placeholder: "piso_test", Value: "test-value", GatewayOnly: tc.gatewayOnly, Workers: tc.workers}
			if _, ok := st.SecretByID("s"); ok {
				err = st.ReplaceSecret(sec)
			} else {
				err = st.AddSecret(sec)
			}
			if err != nil {
				t.Fatal(err)
			}
			d := policy.Decide(policy.Input{Host: "api.github.com", Method: "GET", Worker: tc.worker,
				SecretByPlaceholder: placeholderMap(st), Rules: []model.Rule{{ID: "r", SecretID: "s", Placeholder: "piso_test", Host: "*"}},
				Scan: scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_test", Location: "header", Field: "Authorization"}}},
			})
			if (d.Action == model.ActionSubstitute) != tc.allow {
				t.Fatalf("decision %+v", d)
			}
			if !tc.allow && d.Action != model.ActionBlock {
				t.Fatalf("must block: %+v", d)
			}
		})
	}
}
