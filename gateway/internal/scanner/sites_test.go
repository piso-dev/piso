package scanner

import "testing"

// The substitution allowlist must classify every provider's model-input field
// as payload. This is the regression suite for the reflection leak: a real key
// in one of these fields reaches the model verbatim.
func TestJSONFieldIsPayloadNotCredentialSite(t *testing.T) {
	payload := []string{
		"messages[0].content",
		"messages[2].content[0].text",
		"messages[1].tool_calls[0].function.arguments",
		"system",
		"system.content",
		"input",
		"input[0].content",
		"input.messages[0].content",
		"instructions",
		"contents[0].parts[0].text",
		"candidates[0].content.parts[0].text",
		"prompt",
		"prompts[0].text",
		"tools[0].description",
		"tools[0].function.description",
		"tool.description",
		"reasoning.content",
		"attachments[0].text",
		// An injected prompt could place the token in a credential-sounding
		// field inside a message; ancestry must still win.
		"messages[0].api_key",
		"input[0].api_key",
		"system.token",
	}
	for _, p := range payload {
		if JSONFieldIsCredentialSite(p) {
			t.Errorf("payload field %q classified as credential site", p)
		}
	}
}

func TestJSONFieldIsCredentialSite(t *testing.T) {
	sites := []string{
		"api_key",
		"apiKey",
		"API-Key",
		"auth.token",
		"token",
		"access_token",
		"client_secret",
		"password",
		"private_key",
		"credentials[0].token",
		"config.api_key",
		"authentication.authorization",
	}
	for _, p := range sites {
		if !JSONFieldIsCredentialSite(p) {
			t.Errorf("credential field %q not classified as a site", p)
		}
	}
	if JSONFieldIsCredentialSite("") {
		t.Error("empty path must not be a site")
	}
	if JSONFieldIsCredentialSite("model") {
		t.Error("unknown field must default to payload")
	}
}

func TestQueryParamIsCredentialSite(t *testing.T) {
	for _, k := range []string{"token", "api_key", "access_token", "signature"} {
		if !QueryParamIsCredentialSite(k) {
			t.Errorf("%q should be a credential parameter", k)
		}
	}
	for _, k := range []string{"", "q", "query", "page", "callback", "state", "format"} {
		if QueryParamIsCredentialSite(k) {
			t.Errorf("%q should be payload", k)
		}
	}
}

func TestHeaderIsCredentialSite(t *testing.T) {
	for _, h := range []string{"Authorization", "X-Api-Key", "X-Auth-Token", "Cookie"} {
		if !HeaderIsCredentialSite(h) {
			t.Errorf("%q should be a credential header", h)
		}
	}
	for _, h := range []string{"User-Agent", "Referer", "X-Title", "Accept", "Content-Type", "X-Piso-Reason"} {
		if HeaderIsCredentialSite(h) {
			t.Errorf("%q should be payload/transport", h)
		}
	}
}

func TestIsPayloadFieldLocations(t *testing.T) {
	cases := []struct {
		location, field string
		want            bool
	}{
		{"json-body", "messages[0].content", true},
		{"json-body", "api_key", false},
		{"authorization", "Authorization", false},
		{"x-api-key", "X-Api-Key", false},
		{"query", "q", true},
		{"query", "token", false},
		{"form", "comment", true},
		{"form", "password", false},
		{"path", "", false},
		{"header:User-Agent", "User-Agent", true},
		{"header:X-Api-Key", "X-Api-Key", false},
		// Opaque body: unattributed, so policy treats it as a possible
		// credential and applySubstitution refuses to blind-replace it.
		{"body", "", false},
	}
	for _, c := range cases {
		if got := IsPayloadField(c.location, c.field); got != c.want {
			t.Errorf("IsPayloadField(%q,%q) = %v, want %v", c.location, c.field, got, c.want)
		}
	}
}
