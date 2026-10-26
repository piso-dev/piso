package server

import (
	"fmt"
	"net/http"
	"strings"
)

// The two browser pages the auth gate can serve. They are deliberately small and
// self-contained: they must render before any session exists, so they cannot
// rely on the dashboard bundle or on a build step.

const authPageStyle = `
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;
  background:radial-gradient(1200px 600px at 50% -10%,#1b2430,#0b0e13 60%);
  color:#e6edf3;font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
main{width:min(440px,92vw);padding:32px 30px 28px;border-radius:16px;
  background:#11161dcc;border:1px solid #26303c;
  box-shadow:0 24px 60px -20px #000c,inset 0 1px 0 #ffffff0d}
h1{margin:0 0 6px;font-size:19px;letter-spacing:-.01em}
p{margin:0 0 20px;color:#9aa7b4;font-size:13.5px}
button{width:100%;padding:12px 14px;margin:0 0 10px;border-radius:10px;cursor:pointer;
  font:inherit;font-weight:560;border:1px solid #2c3742;background:#1b232d;color:#e6edf3}
button:hover:not(:disabled){background:#222c37;border-color:#3a4757}
button:disabled{opacity:.45;cursor:not-allowed}
button.primary{background:#2f6f4f;border-color:#3d8a63;color:#eafff3}
button.primary:hover:not(:disabled){background:#37805b}
hr{border:0;border-top:1px solid #232d38;margin:18px 0}
code{font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;background:#0b0f14;
  border:1px solid #222c36;border-radius:6px;padding:1px 5px;color:#b7c6d4}
#msg{margin:14px 0 0;font-size:13px;min-height:1.4em;color:#f0b072;word-break:break-word}
#msg.ok{color:#7fd6a3}
input{width:100%;padding:10px 12px;margin:0 0 10px;border-radius:9px;
  border:1px solid #2c3742;background:#0b0f14;color:#e6edf3;font:inherit}
.note{font-size:12px;color:#78868f;margin:14px 0 0}
`

// authPageHTML renders the login or enroll page. The link parameters are passed
// through untouched so the enroll flow can re-verify them on every step.
func authPageHTML(mode, enrollQuery string) string {
	var body strings.Builder
	switch mode {
	case "enroll":
		body.WriteString(`<h1>Add a credential</h1>
<p>This link is single use and expires in ten minutes. Register a passkey or
add the nostr key your signer holds.</p>
<button id="passkey" class="primary">Register a passkey</button>
<button id="nostr">Add this nostr signer</button>
<input id="label" placeholder="device label (optional)">
<div id="msg"></div>
<p class="note">Whichever you add first becomes the dashboard login. Add the
other later with <code>piso gateway auth enroll</code>.</p>`)
	case "denied":
		body.WriteString(`<h1>Link not valid</h1>
<p>This enrollment link is expired, already used, or was not minted for this
gateway. Mint a new one on the host:</p>
<button id="copy" class="primary">Show the command</button>
<div id="msg"></div>`)
	default:
		body.WriteString(`<h1>piso gateway</h1>
<p>Sign in to reach the dashboard.</p>
<button id="passkey" class="primary">Sign in with a passkey</button>
<button id="nostr">Sign in with nostr</button>
<div id="msg"></div>
<p class="note">No credential yet? Run <code>piso gateway auth enroll</code> on
the gateway host.</p>`)
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>piso gateway</title><style>%s</style></head>
<body><main>%s</main>
<script>%s</script></body></html>`, authPageStyle, body.String(), authPageJS(mode, enrollQuery))
}

// authPageJS is the whole client: no framework, no build, no dependency.
func authPageJS(mode, enrollQuery string) string {
	return `
const MODE = ` + jsString(mode) + `;
const LINK = ` + jsString(enrollQuery) + `;
const msg = (t, ok) => { const m = document.getElementById('msg'); m.textContent = t; m.className = ok ? 'ok' : ''; };
const go = (u) => { location.href = u; };

async function postJSON(url, body) {
  const res = await fetch(url + (LINK ? (url.includes('?') ? '&' : '?') + LINK : ''), {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {})
  });
  const text = await res.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch (e) { data = { error: text }; }
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
  return data;
}

// --- passkey ---------------------------------------------------------------
const b64u = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf)))
  .replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
const ub64u = (s) => {
  const pad = s.replace(/-/g,'+').replace(/_/g,'/');
  const bin = atob(pad + '='.repeat((4 - pad.length % 4) % 4));
  return Uint8Array.from(bin, c => c.charCodeAt(0));
};

async function passkeyLogin() {
  const opts = await postJSON('/auth/passkey/login/begin');
  const pk = opts.publicKey || opts;
  pk.challenge = ub64u(pk.challenge);
  (pk.allowCredentials || []).forEach(c => { c.id = ub64u(c.id); });
  const cred = await navigator.credentials.get({ publicKey: pk });
  const payload = {
    id: cred.id, rawId: b64u(cred.rawId), type: cred.type,
    response: {
      clientDataJSON: b64u(cred.response.clientDataJSON),
      authenticatorData: b64u(cred.response.authenticatorData),
      signature: b64u(cred.response.signature),
      userHandle: cred.response.userHandle ? b64u(cred.response.userHandle) : null
    },
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
  };
  await postJSON('/auth/passkey/login/finish', payload);
  go('/');
}

async function passkeyRegister() {
  const opts = await postJSON('/auth/passkey/register/begin', { label: labelValue() });
  const pk = opts.publicKey || opts;
  pk.challenge = ub64u(pk.challenge);
  pk.user.id = ub64u(pk.user.id);
  (pk.excludeCredentials || []).forEach(c => { c.id = ub64u(c.id); });
  const cred = await navigator.credentials.create({ publicKey: pk });
  const payload = {
    id: cred.id, rawId: b64u(cred.rawId), type: cred.type,
    response: {
      clientDataJSON: b64u(cred.response.clientDataJSON),
      attestationObject: b64u(cred.response.attestationObject),
      transports: cred.response.getTransports ? cred.response.getTransports() : []
    },
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
  };
  await postJSON('/auth/passkey/register/finish', payload);
  go('/');
}

function labelValue() {
  const el = document.getElementById('label');
  return el ? el.value : '';
}

// --- nostr -----------------------------------------------------------------
function requireSigner() {
  if (!window.nostr) throw new Error('no nostr signer in this browser (install a NIP-07 extension)');
  return window.nostr;
}

async function signedEvent(purpose) {
  const nostr = requireSigner();
  const ch = await (await fetch('/auth/nostr/challenge?purpose=' + purpose, { credentials: 'same-origin' })).json();
  const event = {
    kind: ch.kind, created_at: Math.floor(Date.now() / 1000), content: '',
    tags: [['challenge', ch.challenge], ['domain', ch.domain]]
  };
  const signed = await nostr.signEvent(event);
  return signed;
}

async function nostrLogin() {
  await postJSON('/auth/nostr/verify', { event: await signedEvent('login') });
  go('/');
}

async function nostrEnroll() {
  const event = await signedEvent('enroll');
  await postJSON('/auth/nostr/enroll', { event: event, label: labelValue() });
  go('/');
}

// --- wiring ----------------------------------------------------------------
const on = (id, fn) => {
  const el = document.getElementById(id);
  if (!el) return;
  el.addEventListener('click', async () => {
    el.disabled = true;
    try { await fn(); }
    catch (e) {
      msg(e.message || String(e));
      el.disabled = false;
    }
  });
};

on('passkey', async () => {
  msg('waiting for your authenticator…');
  if (MODE === 'enroll') await passkeyRegister(); else await passkeyLogin();
});
on('nostr', async () => {
  msg('waiting for your signer…');
  if (MODE === 'enroll') await nostrEnroll(); else await nostrLogin();
});
on('copy', async () => {
  const cmd = 'piso gateway auth enroll';
  try { await navigator.clipboard.writeText(cmd); msg('copied: ' + cmd, true); }
  catch (e) { msg(cmd, true); }
});
`
}

// jsString renders a Go string as a quoted JS string literal.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '<':
			b.WriteString(`\u003c`)
		case '>':
			b.WriteString(`\u003e`)
		case '&':
			b.WriteString(`\u0026`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (s *Server) writeAuthPage(w http.ResponseWriter, mode, enrollQuery string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(authPageHTML(mode, enrollQuery)))
}

// serveLoginPage renders the login page for an unauthenticated navigation.
func (s *Server) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	s.writeAuthPage(w, "login", "")
}

// serveAuthPage renders the enroll or denied page, carrying the link through.
func (s *Server) serveAuthPage(w http.ResponseWriter, r *http.Request, mode string) {
	s.writeAuthPage(w, mode, r.URL.RawQuery)
}
