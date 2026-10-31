package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

// pinFixture is one provider stack plus an optional per-worker override:
// "default" is the account default and serves the approved set, "pinned" is a
// second provider with one model, and worker-1 is pinned to it.
func pinFixture(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID:             "default",
		BaseURL:        "https://api.example.test/v1",
		DefaultModel:   "chosen",
		ApprovedModels: []string{"chosen", "sibling"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProvider(store.PiProviderRec{ID: "pinned", BaseURL: "https://api.example.test/v1", DefaultModel: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetModelOverride("worker-1", "pinned", ""); err != nil {
		t.Fatal(err)
	}
	return h, st
}

func pinRequest(t *testing.T, slug, method, path, body string) *http.Request {
	t.Helper()
	req, _ := http.NewRequest(method, "https://api.example.test"+path, strings.NewReader(body))
	return req.WithContext(context.WithValue(req.Context(), workerIdentityKey{}, slug))
}

func TestModelPin(t *testing.T) {
	h, _ := pinFixture(t)
	for _, tc := range []struct{ name, slug, method, path, body, reason string }{
		{"default", "worker-2", "POST", "/v1/chat/completions", `{"model":"chosen"}`, ""},
		{"approved sibling", "worker-2", "POST", "/v1/chat/completions", `{"model":"sibling"}`, ""},
		{"padded model id", "worker-2", "POST", "/v1/chat/completions", `{"model":"  chosen  "}`, ""},
		{"not approved", "worker-2", "POST", "/v1/chat/completions", `{"model":"exotic"}`, pinReasonNotApproved},
		{"not approved, preview variant", "worker-2", "POST", "/v1/chat/completions", `{"model":"chosen-preview"}`, pinReasonNotApproved},
		{"missing", "worker-2", "POST", "/v1/chat/completions", `{}`, pinReasonRequired},
		{"blank", "worker-2", "POST", "/v1/chat/completions", `{"model":"   "}`, pinReasonRequired},
		{"malformed", "worker-2", "POST", "/v1/responses", `{`, pinReasonRequired},
		{"override", "worker-1", "POST", "/v1/chat/completions", `{"model":"other"}`, ""},
		{"override not approved", "worker-1", "POST", "/v1/chat/completions", `{"model":"chosen"}`, pinReasonNotApproved},
		{"catalog", "worker-1", "GET", "/v1/models", "", ""},
		{"unrelated", "worker-1", "POST", "/upload", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.modelPin(pinRequest(t, tc.slug, tc.method, tc.path, tc.body), []byte(tc.body))
			if got.Reason != tc.reason {
				t.Fatalf("reason %q, want %q (message %q)", got.Reason, tc.reason, got.Message)
			}
		})
	}
}

// A refusal has to say what was asked for and what would have worked: the bare
// code is what made the original 403 unactionable.
func TestModelPinRefusalExplainsItself(t *testing.T) {
	h, _ := pinFixture(t)
	got := h.modelPin(pinRequest(t, "worker-2", "POST", "/v1/chat/completions", `{"model":"unbiased/pareto-26.10"}`), []byte(`{"model":"unbiased/pareto-26.10"}`))
	if got.Reason != pinReasonNotApproved {
		t.Fatalf("reason %q", got.Reason)
	}
	if got.Model != "unbiased/pareto-26.10" {
		t.Fatalf("verdict dropped the requested model: %q", got.Model)
	}
	for _, want := range []string{"unbiased/pareto-26.10", `"chosen"`, `"sibling"`, "default"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("message %q does not mention %q", got.Message, want)
		}
	}
	if header := got.Header().Get("X-Piso-Reason"); header != pinReasonNotApproved {
		t.Fatalf("X-Piso-Reason %q", header)
	}
	if header := got.Header().Get("X-Piso-Allowed-Models"); header != "chosen,sibling" {
		t.Fatalf("X-Piso-Allowed-Models %q", header)
	}
}

// An empty approved list keeps the previous behaviour exactly: the default
// model and nothing else. Omitting the new field must never widen a provider.
func TestModelPinEmptyApprovedListNarrowsToDefault(t *testing.T) {
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID:           "default",
		BaseURL:      "https://api.example.test/v1",
		DefaultModel: "chosen",
		Models:       []string{"chosen", "sibling"},
	}); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"chosen": "", "sibling": pinReasonNotApproved} {
		body := `{"model":"` + model + `"}`
		if got := h.modelPin(pinRequest(t, "worker-2", "POST", "/v1/chat/completions", body), []byte(body)); got.Reason != want {
			t.Fatalf("%s: reason %q want %q", model, got.Reason, want)
		}
	}
}

// The default is always usable, so an approved list that omits it narrows but
// can never make the provider unable to serve its own start state.
func TestModelPinDefaultIsAlwaysAllowed(t *testing.T) {
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID:             "default",
		BaseURL:        "https://api.example.test/v1",
		DefaultModel:   "chosen",
		ApprovedModels: []string{"other"},
	}); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"other": "", "chosen": "", "exotic": pinReasonNotApproved} {
		body := `{"model":"` + model + `"}`
		if got := h.modelPin(pinRequest(t, "worker-2", "POST", "/v1/chat/completions", body), []byte(body)); got.Reason != want {
			t.Fatalf("%s: reason %q want %q", model, got.Reason, want)
		}
	}
}

// A provider with no approved list, no default, and no catalog has nothing to
// judge against; that is unresolved, not a rejection, so the caller is told to
// configure the provider rather than to pick a different model.
func TestModelPinUnresolved(t *testing.T) {
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{ID: "default", BaseURL: "https://api.example.test/v1"}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"chosen"}`
	got := h.modelPin(pinRequest(t, "worker-2", "POST", "/v1/chat/completions", body), []byte(body))
	if got.Reason != pinReasonUnresolved {
		t.Fatalf("reason %q want %q", got.Reason, pinReasonUnresolved)
	}
}

// A worker pinned to one provider must not reach the host of another: the
// secret for that host is not the one its pin resolves, so the request cannot
// be authenticated and must not be forwarded. Only hosts a profile owns are
// checked, which is why this needs two managed providers.
func TestModelPinProviderMismatch(t *testing.T) {
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{ID: "default", BaseURL: "https://api.example.test/v1", DefaultModel: "chosen"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProvider(store.PiProviderRec{ID: "pinned", BaseURL: "https://other.example.test/v1", DefaultModel: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetModelOverride("worker-1", "pinned", ""); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"other"}`
	// worker-1's pin points at other.example.test; this request goes to the
	// account default's host instead.
	got := h.modelPin(pinRequest(t, "worker-1", "POST", "/v1/chat/completions", body), []byte(body))
	if got.Reason != pinReasonProvider {
		t.Fatalf("reason %q want %q", got.Reason, pinReasonProvider)
	}
	if !strings.Contains(got.Message, "other.example.test") {
		t.Fatalf("message does not name the expected host: %q", got.Message)
	}
}

func TestModelPinBlocksBeforeForwarding(t *testing.T) {
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{ID: "default", BaseURL: "https://api.example.test/v1", DefaultModel: "chosen"}); err != nil {
		t.Fatal(err)
	}
	forwarded := 0
	h.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		forwarded++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	for _, body := range []string{`{}`, `{"model":"wrong"}`, `{"model":"chosen"}`} {
		req, _ := http.NewRequest("POST", "https://api.example.test/v1/chat/completions", strings.NewReader(body))
		resp, _ := h.process(context.Background(), req, "https")
		resp.Body.Close()
		want := 403
		if strings.Contains(body, "chosen") {
			want = 200
		}
		if resp.StatusCode != want {
			t.Fatalf("status %d want %d", resp.StatusCode, want)
		}
	}
	if forwarded != 1 {
		t.Fatalf("forwarded %d requests", forwarded)
	}
	records := st.Records(10)
	if records[0].Model != "chosen" {
		t.Fatalf("request model not recorded: %+v", records[0])
	}
}

// The 403 must carry the reason and the readable message out to the client, not
// just the log.
func TestModelPinRefusalResponse(t *testing.T) {
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID:             "default",
		BaseURL:        "https://api.example.test/v1",
		DefaultModel:   "chosen",
		ApprovedModels: []string{"chosen", "sibling"},
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"unbiased/pareto-26.10"}`
	req, _ := http.NewRequest("POST", "https://api.example.test/v1/chat/completions", strings.NewReader(body))
	resp, _ := h.process(context.Background(), req, "https")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Piso-Reason"); got != pinReasonNotApproved {
		t.Fatalf("X-Piso-Reason %q", got)
	}
	if got := resp.Header.Get("X-Piso-Allowed-Models"); got != "chosen,sibling" {
		t.Fatalf("X-Piso-Allowed-Models %q", got)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "unbiased/pareto-26.10") || !strings.Contains(string(raw), "chosen") {
		t.Fatalf("body %q", raw)
	}
	records := st.Records(10)
	if records[0].Model != "unbiased/pareto-26.10" {
		t.Fatalf("refused request model not recorded: %+v", records[0])
	}
}
