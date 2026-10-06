# CAPABILITIES — where this sandbox ends

This file is appended to AGENTS.md on every boot by the entrypoint. Read it
before acting on anything that might be out of bounds. The rule that governs
everything below is simple:

> **You are a guest inside a hardened worker. For any step the host must do,
> never fumble through a failing attempt — emit a Host Action Request and
> move on.**

## What works here

- `/workspace` is the host's project directory, rw, **mount-shared**: anything
  the host drops under `/workspace` is visible to you, and anything you write
  there is visible to the host.
- HTTP(S) egress is proxied through the gateway. Real credentials are never
  available; the proxy substitutes `piso_…` placeholders or blocks/ flags the
  request. Public fetches (package registries, docs, public git over https)
  generally work.
- Any dev-server port you bind is automatically published as
  `http://<slug>-<port>.piso.local` (or `?route=` on the apex) — no host
  ceremony needed.
- The worker API at `$GATEWAY_URL` (:8083) is reachable: health, checkin,
  capabilities, activity, context, ports, mcp.
- Gateway tools are the `piso` MCP server (`piso-mcp` on stdio). That relay
  seals every call; do not POST plaintext JSON-RPC to `$GATEWAY_URL/mcp`.
- `/root/.pi/agent` (and `/var` equivalents) persist: sessions, skills,
  extensions, logs.

## What NEVER works here (do not attempt)

- **ssh/scp** of any kind — no keys, no route, and an unproxied ssh/git will
  **hang**, not fail fast. This is the biggest time-waster in this sandbox.
- **`pi mcp login` / in-worker MCP OAuth.** The callback is
  `http://127.0.0.1:<port>/callback` inside this container; the host browser
  cannot finish it. A real Bearer is 407'd. Use `piso-mcp-add` (below).
- **git clone/push/pull over ssh**, or any git operation requiring
  credentials the substitution layer cannot satisfy.
- **docker / podman / any container control**.
- **Real credentials**: you only ever hold `piso_…` placeholders; you cannot
  mint, fetch, or use real values.
- **Privileged operations**: writes outside `/workspace` + `/root/.pi/agent`,
  system service management, binds below port 1024 that need privilege.
- Anything requiring the **host filesystem** beyond the `/workspace` mount.

## Host Action Request protocol

For any out-of-bounds step, do all of the following:

1. **In your reply**, emit a request the human can act on, with the exact
   command and a rendezvous path for the result (always under `/workspace`):
   ```
   HOST ACTION REQUEST: clone the private repo
     COMMAND: cd /abs/path && git clone git@github.com:org/repo.git
     RESULT:  /workspace/repo   (then I can continue with the vendoring)
   ```
2. **Surface it on the board** so it survives this session — call the piso
   MCP tool `host_request` with `command` set to the host command and
   `resultPath` set to the `/workspace/...` rendezvous (or `activity_post`
   with `kind=host` and the same one-line text).
3. **Rendezvous**: later, check the RESULT path. If it exists, continue from
   there. If it does not, the host has not done it yet — re-ask (a new
   `host` event or a `poke` to the project), never block, never retry the
   attempt yourself.
4. Keep the request **one line** for the board; the details live in your
   reply text.

## Live boundaries (fetch once per session)

Two things change at runtime and only the gateway knows them:

```bash
curl -s "$GATEWAY_URL/api/v1/worker/capabilities?worker=$PISO_WORKER_NAME"
```

- `internetEnabled` — the operator can flip the kill-switch per worker.
- `scopedPlaceholders` — which `piso_…` placeholders your model may actually
  see (secret scoping). A step that needs an unscoped secret is out of
  bounds, period.

If the fetch fails or a field is unclear, treat the capability as **BLOCKED**,
not open.

## MCP servers

When the user asks to add, connect, or sign in to a remote MCP server, run:

```bash
piso-mcp-add https://example.com/mcp [name]
```

That is the whole install. It POSTs the URL to `$GATEWAY_URL/api/v1/worker/mcp`
and writes `~/.pi/agent/mcp.json` with
`headers.Authorization: Bearer piso_mcp_…`. The gateway keeps the real OAuth
token. You only ever hold the placeholder.

**Never** `pi mcp add`, `pi mcp login`, or `/mcp login`. A `url`-only
`mcp.json` entry starts the same in-worker OAuth. Do not write one. Do not
use `auth` / `bearerToken` — current pi ignores those and still starts OAuth
unless an `Authorization` header is set. Do not paste tokens or open the
OAuth URL yourself.

If the command prints `needs-auth`, `expired`, or `revoked`, tell the human
to Authorize that server on the dashboard MCP tab (the Authorize chip is
already up), then:

Call `activity_post` with `kind=waiting` and
`text="Authorize <name> on the dashboard MCP tab"`.


A connect before they sign in is held (`407 mcp-auth-required`) and replays
after. This session will not see the new server until `/reload` or the next
session. Re-running `piso-mcp-add` with the same URL refreshes status; it
does not mint a second secret.

If `piso-mcp-add` is not on PATH, do those two steps by hand — still never
`pi mcp login`:

1. `POST $GATEWAY_URL/api/v1/worker/mcp` with
   `{"worker":"$PISO_WORKER_NAME","slug":"$PISO_WORKER_SLUG","url":"<https url>","name":"<optional>"}`.
2. Write the returned `name` under `mcpServers` in `~/.pi/agent/mcp.json`,
   with `url` and `headers.Authorization` set to `Bearer <placeholder>`.
   Refuse anything that is not a `piso_mcp_…` placeholder.

## Probing cautiously

If you must verify a boundary yourself, wrap the probe in `timeout 3` — an
unproxied ssh/git connection will hang indefinitely in this network:

```bash
timeout 3 git ls-remote git@github.com:org/repo.git   # will hang→timeout; don't bother
timeout 3 curl -sI https://example.com                # fine through the proxy
```