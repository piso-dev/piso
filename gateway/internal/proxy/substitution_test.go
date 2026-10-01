package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/model"
	"piso/gateway/internal/scanner"
	"piso/gateway/internal/store"
)

func TestApplySubstitutionRewritesBasicAuth(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir+"/state.json", dir+"/log.jsonl", dir+"/patterns.json", dir+"/activities.db", 10)
	if err != nil {
		t.Fatal(err)
	}
	const ph = "piso_gh_5ef1739b3cf1"
	const real = "github_pat_REAL0123456789abcdefgh"
	if err := st.AddSecret(store.SecretRec{
		ID: "s1", Name: "github", Placeholder: ph, Value: real,
	}); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("ExampleUser:" + ph))
	req, err := http.NewRequest("GET", "https://github.com/ExampleUser/o.git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic "+b64)
	applySubstitution(req, []byte{}, model.Decision{Substituted: []string{ph}}, st, scanner.Result{}, nil)
	got := req.Header.Get("Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ExampleUser:"+real))
	if got != want {
		t.Fatalf("basic header: want %q got %q", want, got)
	}
}

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
	applySubstitution(req, body, model.Decision{Substituted: []string{ph}}, st, scanner.Result{}, nil)
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

// A git smart-HTTP protocol-v2 body is pkt-line framed and starts with a
// 4-hex length prefix (e.g. "0014command=ls-refs\n"). Its leading '0' is
// a valid JSON number, so a lenient decoder accepts the prefix and ignores
// the rest, silently truncating the payload to one byte. This must refuse
// the JSON path so the body is forwarded byte for byte.
func TestSubstituteJSONSkippingChatPktBody(t *testing.T) {
	body := []byte("0014command=ls-refs\n0014agent=git/2.39.5\n0000")
	if out, ok := substituteJSONSkippingChat(body, map[string]string{"piso_gh_abcdef": "real"}); ok {
		t.Fatalf("pkt-line body accepted as JSON: %q", string(out))
	}
}

// A standalone JSON number (or any non-object/array value) must never be
// rewritten into a 1-byte document by the chat-substitution path.
func TestSubstituteJSONSkippingChatRejectsPrimitive(t *testing.T) {
	for _, b := range [][]byte{[]byte("0"), []byte("0014"), []byte("\n\t0\n"), []byte("\"bare string\"")} {
		if out, ok := substituteJSONSkippingChat(b, map[string]string{"x": "y"}); ok {
			t.Fatalf("primitive body accepted as JSON: %q -> %q", string(b), string(out))
		}
	}
}

// End to end: applySubstitution on a git upload-pack/ls-refs request must
// leave the pkt-line body byte-identical (this is the clone-through-gateway
// regression).
func TestApplySubstitutionPreservesPktBody(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir+"/state.json", dir+"/log.jsonl", dir+"/patterns.json", dir+"/activities.db", 10)
	if err != nil {
		t.Fatal(err)
	}
	const ph = "piso_gh_abcdef"
	if err := st.AddSecret(store.SecretRec{
		ID: "s1", Name: "github", Placeholder: ph, Value: "github_pat_REAL0123456789abcdefgh",
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte("0014command=ls-refs\n0014agent=git/2.39.5\n0000")
	req, err := http.NewRequest("POST", "https://github.com/ExampleUser/o.git/git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	applySubstitution(req, body, model.Decision{Substituted: []string{ph}}, st, scanner.Result{}, nil)
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("pkt body mangled: want %d bytes got %d bytes", len(body), len(got))
	}
}

type fakeMinter struct{ token string }

func (f fakeMinter) Mint(placeholder, host, path, body string) (string, error) {
	return f.token, nil
}
func (f fakeMinter) LiveTokens() []string { return []string{f.token} }

func TestApplySubstitutionMintsGithubApp(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir+"/state.json", dir+"/log.jsonl", dir+"/patterns.json", dir+"/activities.db", 10)
	if err != nil {
		t.Fatal(err)
	}
	const ph = "piso_github_app_testxx"
	if err := st.AddSecret(store.SecretRec{
		ID: "s1", Name: "gha", Placeholder: ph, Kind: model.SecretKindGithubApp,
	}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/complex-gh/piso", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ph)
	applySubstitution(req, nil, model.Decision{Substituted: []string{ph}}, st, scanner.Result{}, fakeMinter{token: "ghs_minted"})
	if got := req.Header.Get("Authorization"); got != "Bearer ghs_minted" {
		t.Fatalf("got %q", got)
	}
}
