package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestModelPin(t *testing.T) {
	h, st := testHandler(t)
	if err := st.UpsertPiProviderActive(store.PiProviderRec{ID: "default", BaseURL: "https://api.example.test/v1", DefaultModel: "chosen"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProvider(store.PiProviderRec{ID: "pinned", BaseURL: "https://api.example.test/v1", DefaultModel: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetModelOverride("worker-1", "pinned"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, slug, method, path, body, reason string }{
		{"default", "worker-2", "POST", "/v1/chat/completions", `{"model":"chosen"}`, ""},
		{"missing", "worker-2", "POST", "/v1/chat/completions", `{}`, "model-required"},
		{"malformed", "worker-2", "POST", "/v1/responses", `{`, "model-required"},
		{"mismatch", "worker-2", "POST", "/v1/messages", `{"model":"other"}`, "model-mismatch"},
		{"override", "worker-1", "POST", "/v1/chat/completions", `{"model":"other"}`, ""},
		{"override mismatch", "worker-1", "POST", "/v1/chat/completions", `{"model":"chosen"}`, "model-mismatch"},
		{"catalog", "worker-1", "GET", "/v1/models", "", ""},
		{"unrelated", "worker-1", "POST", "/upload", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, "https://api.example.test"+tc.path, strings.NewReader(tc.body))
			req = req.WithContext(context.WithValue(req.Context(), workerIdentityKey{}, tc.slug))
			_, reason := h.modelPin(req, []byte(tc.body))
			if reason != tc.reason {
				t.Fatalf("reason %q, want %q", reason, tc.reason)
			}
		})
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
