# piso

Isolate an AI coding agent (pi) inside a Docker worker, with all egress gated
through a MITM gateway that substitutes fake credentials for real ones and
blocks anything that looks like a real credential.

```
you (terminal: piso / host browser)
   │                 │  1 published port on gateway (TLS via its CA)
   ▼                 ▼
GATEWAY  ── control plane (UI: secrets, patterns, log; API: expose, retry)
   │  egress MITM (CONNECT → substitute piso_ placeholders / block real creds)
   │  ingress  (name.piso.local → worker:port)
   ▼
WORKER (pi, Bun)  ── named volume ~/.pi/agent (sessions persist)
   └─ mount: project dir (rw)     piso_vpc is internal ⇒ gateway is the only egress
```

## Core guarantees

- **The worker never holds real credentials.** Only `piso_...` placeholders.
  Real values live in the gateway's secrets file (host-mounted, gateway-only).
- **Any request that carries a real credential — or anything that *looks* like
  one — is blocked and flagged.** Detection uses a comprehensive, updatable
  regex library (plus exact matches against known real values) so a leaked key
  can't ride out even if the worker never saw it in the vault.
- **Blocked requests are first-class objects** with an id, a captured request,
  and a retry button: add a secret/exception, retry, done.
- **Everything is gated**: the worker's only egress is the gateway (`piso_vpc`
  is a Docker internal network — off-bridge forwarding is dropped), and its
  only ingress is the gateway's reverse proxy.

## Layout

```
cli/     Go CLI: piso          (up, attach, expose, logs, secrets, dashboard)
gateway/ Go MITM proxy + control plane + request log + web UI
worker/  Docker image: pi/Bun + CA trust + hardening
compose/ docker-compose (worker + gateway, piso_vpc internal)
```

## Quick start

```bash
git clone <this-repo> && cd piso
make install                 # sudo if PREFIX=/usr/local is not writable
# or: make install PREFIX=$HOME/.local   # then ensure ~/.local/bin is on PATH
cd /path/to/your/project
piso up          # ensure gateway + worker for the current dir
piso attach      # enter the worker, run pi
piso dashboard   # open http://piso.local
piso expose 5173 --name preview   # reverse-proxy a worker dev server
```

`make install` (including `sudo make install` over an existing copy) replaces `$(PREFIX)/bin/piso` and `$(PREFIX)/share/piso`, then runs `piso setup --rebuild` as the login user. That imports any leftover repo `.piso` files that `~/.piso` does not already have, rebuilds the gateway image, recreates the container, and migrates `state.json` on startup. Live secrets in `~/.piso` are kept. Gateway secrets/CA/logs live in `~/.piso` (`PISO_DATA` overrides). Override the share tree with `PISO_HOME`. Secrets filled in the dashboard **Blocked secrets** modal are stored in the gateway; the next `piso attach` exports placeholders only (`ANTHROPIC_API_KEY=piso_…`) from `~/.piso/placeholders.env`. Existing workers need one `piso up` to mount that file.

The dashboard is **http://piso.local** (host port 80 → container 8081). `piso up` adds `127.0.0.1 piso.local` to `/etc/hosts` when it can; otherwise it prints the line to add. If port 80 is taken:

```bash
piso up --ctrl-port 8081    # then http://piso.local:8081
```

`--proxy-port` and `--ingress-port` work the same way. A taken port is a hard error, not a silent remap. Chosen ports are saved in `~/.piso/ports.json`.

See `docs/design.md` for the threat model and the decision pipeline.