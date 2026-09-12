package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/model"
	"piso/gateway/internal/scanner"
	"piso/gateway/internal/store"
)

func TestSubstituteJSONSkippingChatLeavesMessages(t *testing.T) {
	const ph = "piso_gh_abcdef"
	const real = "github_pat_THIS_MUST_NOT_ENTER_MESSAGES"
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"use ` + ph + `"}],"api_key":"` + ph + `"}`)
	out, ok := substituteJSONSkippingChat(body, map[string]string{ph: real})
	if !ok {
		t.Fatal("expected JSON rewrite")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewrite not JSON: %v %s", err, out)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages: %#v", got["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].(string)
	if content != "use "+ph {
		t.Fatalf("messages[] was rewritten: %q", content)
	}
	if strings.Contains(content, real) {
		t.Fatal("real secret leaked into messages[]")
	}
	if got["api_key"] != real {
		t.Fatalf("non-chat field want %q got %#v", real, got["api_key"])
	}
}

func TestSubstituteJSONSkippingChatToolCallArguments(t *testing.T) {
	const ph = "piso_gh_abcdef"
	body := []byte(`{"messages":[{"tool_calls":[{"function":{"arguments":"Bearer ` + ph + `"}}]}]}`)
	out, ok := substituteJSONSkippingChat(body, map[string]string{ph: "github_pat_LEAK"})
	if !ok {
		t.Fatal("expected JSON rewrite")
	}
	if strings.Contains(string(out), "github_pat_LEAK") {
		t.Fatalf("tool_calls arguments rewritten: %s", out)
	}
	if !strings.Contains(string(out), ph) {
		t.Fatalf("placeholder stripped from tool_calls: %s", out)
	}
}

func TestApplySubstitutionRewritesHeaderNotMessages(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir+"/state.json", dir+"/log.jsonl", dir+"/patterns.json", dir+"/activities.db", 10)
	if err != nil {
		t.Fatal(err)
	}
	const ph = "piso_routstr_abc"
	const real = "sk-real-provider-key-value"
	if err := st.AddSecret(store.SecretRec{
		ID: "s1", Name: "routstr", Placeholder: ph, Value: real,
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"token ` + ph + `"}]}`)
	req, err := http.NewRequest("POST", "https://routstr.ft.hn/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ph)
	req.Header.Set("Content-Type", "application/json")
	applySubstitution(req, body, model.Decision{Substituted: []string{ph}}, st, scanner.Result{})
	if got := req.Header.Get("Authorization"); got != "Bearer "+real {
		t.Fatalf("header: %q", got)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), real) {
		t.Fatalf("real key leaked into body: %s", got)
	}
	if !strings.Contains(string(got), ph) {
		t.Fatalf("placeholder missing from messages: %s", got)
	}
}

func TestSubstituteJSONSkippingChatNonJSON(t *testing.T) {
	if _, ok := substituteJSONSkippingChat([]byte("not json piso_x_abcdef"), map[string]string{"piso_x_abcdef": "real"}); ok {
		t.Fatal("non-JSON must not claim rewrite")
	}
}
