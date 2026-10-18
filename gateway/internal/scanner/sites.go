package scanner

import "strings"

// This file defines WHERE a placeholder is a credential site.
//
// The gateway substitutes a placeholder only at an explicit credential site.
// Everything else is payload: it is forwarded untouched and never carries a
// real secret. This is deliberately an allowlist. A denylist of known chat
// fields (the old IsChatContent) cannot cover every provider's model input:
// Anthropic `system`, OpenAI Responses `input`/`instructions`, Gemini
// `contents`, and `tools[].description` all carry model-visible text, and new
// fields keep appearing. Treating unknown fields as payload closes that class
// of reflection leak by default.

// payloadSegments are JSON object keys whose values are model/user payload.
// A credential-shaped leaf under any of these is still payload: an injected
// prompt could ask the agent to put the placeholder in
// `messages[0].api_key`, so ancestry matters, not just the leaf name.
var payloadSegments = map[string]bool{
	"message":          true,
	"messages":         true,
	"input":            true,
	"inputs":           true,
	"instructions":     true,
	"system":           true,
	"content":          true,
	"contents":         true,
	"prompt":           true,
	"prompts":          true,
	"text":             true,
	"tools":            true,
	"tool":             true,
	"toolcalls":        true,
	"function":         true,
	"functions":        true,
	"description":      true,
	"arguments":        true,
	"reasoning":        true,
	"reasoningcontent": true,
	"parts":            true,
	"candidates":       true,
	"imageurl":         true,
	"attachments":      true,
	"files":            true,
	"documents":        true,
	"audio":            true,
	"video":            true,
}

// credentialLeafNames are JSON keys / query keys / form keys that name a
// credential. The leaf must also be free of payload ancestors above.
var credentialLeafNames = map[string]bool{
	"apikey":          true,
	"apitoken":        true,
	"key":             true,
	"token":           true,
	"accesstoken":     true,
	"refreshtoken":    true,
	"idtoken":         true,
	"authtoken":       true,
	"bearertoken":     true,
	"sessiontoken":    true,
	"securitytoken":   true,
	"clientsecret":    true,
	"secret":          true,
	"secretkey":       true,
	"privatekey":      true,
	"signingkey":      true,
	"subscriptionkey": true,
	"password":        true,
	"passwd":          true,
	"passphrase":      true,
	"credential":      true,
	"credentials":     true,
	"authorization":   true,
	"auth":            true,
	"signature":       true,
	"sig":             true,
}

// payloadHeaders are headers whose values can reach model context or are
// transport metadata. A placeholder there is text, not a credential.
var payloadHeaders = map[string]bool{
	"useragent":         true,
	"referer":           true,
	"httpreferer":       true,
	"origin":            true,
	"from":              true,
	"xtitle":            true,
	"forwarded":         true,
	"xforwardedfor":     true,
	"xforwardedhost":    true,
	"xforwardedproto":   true,
	"xrealip":           true,
	"accept":            true,
	"acceptencoding":    true,
	"acceptlanguage":    true,
	"acceptcharset":     true,
	"cachecontrol":      true,
	"contenttype":       true,
	"contentlength":     true,
	"contentencoding":   true,
	"contentlanguage":   true,
	"connection":        true,
	"keepalive":         true,
	"range":             true,
	"te":                true,
	"trailer":           true,
	"upgrade":           true,
	"dnt":               true,
	"traceparent":       true,
	"tracestate":        true,
	"xrequestid":        true,
	"xcorrelationid":    true,
	"xpisorequestid":    true,
	"xpisodecision":     true,
	"xpisoreason":       true,
}

// normalizeKey lowercases and drops separators so api_key, API-Key and
// apiKey share one entry.
func normalizeKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '_', '-', '.', ' ', '\t':
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// jsonPathSegments splits a JSON field path into object-key segments,
// dropping array indices ("messages[0].content" -> messages, content).
func jsonPathSegments(path string) []string {
	parts := strings.Split(path, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if i := strings.IndexByte(p, '['); i >= 0 {
			p = p[:i]
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// JSONFieldIsCredentialSite reports whether a JSON string at path may be
// substituted. True only when the leaf key names a credential AND no
// ancestor segment is model/user payload.
func JSONFieldIsCredentialSite(path string) bool {
	segs := jsonPathSegments(path)
	if len(segs) == 0 {
		return false
	}
	for _, s := range segs {
		if payloadSegments[normalizeKey(s)] {
			return false
		}
	}
	return credentialLeafNames[normalizeKey(segs[len(segs)-1])]
}

// QueryParamIsCredentialSite reports whether a query/form parameter name may
// be substituted. Unknown parameters are payload, not credentials.
func QueryParamIsCredentialSite(name string) bool {
	if name == "" {
		return false
	}
	return credentialLeafNames[normalizeKey(name)]
}

// headerPayload reports whether a header value is payload/transport noise.
func headerPayload(name string) bool {
	return payloadHeaders[normalizeKey(name)]
}

// HeaderIsCredentialSite reports whether a header may carry a substituted
// credential. Payload/transport headers (User-Agent, Referer, X-Title, Accept,
// Content-*) are never rewritten.
func HeaderIsCredentialSite(name string) bool {
	if name == "" {
		return false
	}
	return !headerPayload(name)
}

// isFormContentType reports whether the body is
// application/x-www-form-urlencoded (parameters may carry credentials by
// name; policy handles the allowlist).
func isFormContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/x-www-form-urlencoded")
}

// IsPayloadField reports whether a finding's location/field pair is payload,
// i.e. NOT a place the gateway may write a real credential. Substitution only
// happens outside payload fields; a placeholder that appears only in payload
// is left as text (and does not require a rule).
func IsPayloadField(location, field string) bool {
	switch location {
	case "json-body":
		if field == "" {
			return true
		}
		return !JSONFieldIsCredentialSite(field)
	case "body":
		// Raw non-JSON body: the scanner could not attribute fields, so the
		// body as a whole is a credential candidate (and is never rewritten
		// blindly; see proxy.applySubstitution).
		return false
	case "form":
		return !QueryParamIsCredentialSite(field)
	case "query":
		return !QueryParamIsCredentialSite(field)
	case "path":
		return false
	case "authorization", "x-api-key", "cookie":
		return false
	}
	if strings.HasPrefix(location, "header:") {
		return headerPayload(field)
	}
	return false
}
