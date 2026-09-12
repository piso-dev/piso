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
  capabilities, activity, context, ports.
- `/root/.pi/agent` (and `/var` equivalents) persist: sessions, skills,
  extensions, logs.

## What NEVER works here (do not attempt)

- **ssh/scp** of any kind — no keys, no route, and an unproxied ssh/git will
  **hang**, not fail fast. This is the biggest time-waster in this sandbox.
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
2. **Surface it on the board** so it survives this session:
   `piso-informant host "git clone git@github.com:org/repo.git → /workspace/repo"`
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

## Probing cautiously

If you must verify a boundary yourself, wrap the probe in `timeout 3` — an
unproxied ssh/git connection will hang indefinitely in this network:

```bash
timeout 3 git ls-remote git@github.com:org/repo.git   # will hang→timeout; don't bother
timeout 3 curl -sI https://example.com                # fine through the proxy
```