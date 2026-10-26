# piso

> **Public mirror.** This repository is a read-only mirror published from an
> upstream private repository; pull requests cannot be merged here. See
> [CONTRIBUTING.md](https://github.com/piso-dev/piso/blob/main/CONTRIBUTING.md).

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
git clone https://github.com/piso-dev/piso.git && cd piso
make install                 # sudo if PREFIX=/usr/local is not writable
# or: make install PREFIX=$HOME/.local   # then ensure ~/.local/bin is on PATH
cd /path/to/your/project
piso up          # ensure gateway + worker for the current dir
                 # (name = git origin repo, else folder; --name to override)
piso attach      # enter the worker, run pi (waits for extension seeding)
piso attach --new  # start a fresh session instead of restoring
piso attach --session <id>  # open a specific session
piso dashboard   # open http://piso.local
piso expose 5173 --name preview   # reverse-proxy a worker dev server
```

`make install` (including `sudo make install` over an existing copy) replaces `$(PREFIX)/bin/piso` and `$(PREFIX)/share/piso`, then runs `piso setup --rebuild` as the login user. That imports any leftover repo `.piso` files that `~/.piso` does not already have, rebuilds the gateway image, recreates the container, and migrates `state.json` on startup. Live secrets in `~/.piso` are kept. Gateway secrets/CA/logs live in `~/.piso` (`PISO_DATA` overrides). Override the share tree with `PISO_HOME`. Secrets filled in the dashboard **Blocked secrets** modal are stored in the gateway; the next `piso attach` exports placeholders only (`ANTHROPIC_API_KEY=piso_…`) from `~/.piso/placeholders.env`. Existing workers need one `piso up` to mount that file.

### Cloud-ship install (worker only)

On a machine that will only run a launched worker — its own box, never the
gateway host — use `make install-worker` instead:

```bash
sudo make install-worker     # CLI + share tree; no gateway image, no gateway or
                             # monitor container, no hosts-sync daemon
piso worker launch --name proj --server https://gateway.example.com
piso attach
```

It copies the same share tree as `make install` (so `piso up` still works if
the box is later promoted) but stops after `piso setup --no-monitor`, which
records the opt-out in `~/.piso/host.json`. The first `piso worker launch` still
bakes the `piso-worker` image on this host; nothing else is built.

### Piso model providers

The dashboard manages a stack of model providers. Each profile pairs a
secret, an API base URL, and an optional default model; one is active and is
written as `defaultProvider`/`defaultModel` in the shared `settings.json`.
Every profile is written into the shared `models.json` under the `piso`
namespace, so workers can switch with `/model` as well. Requests carry only
the profile's `piso_…` placeholder; the gateway substitutes the real
credential.

Piso's bundled pi extension registers every profile and refreshes its catalog
from `<baseUrl>/models` when pi's `/model` selector opens. Successful catalogs
are cached in the worker's persistent `~/.pi/agent/models-store.json` and are
also written back to `models.json` as an offline fallback. A catalog fetch is
advisory: saving or activating a provider never waits on it, and a provider
with no reachable catalog still saves and activates. When no default model is
known, the extension resolves the first registered model at session start.

That first-model fallback applies only to a profile with **no** saved default.
A saved default the session's catalog does not contain is never swapped for
"whatever came first": the session keeps its current model and logs one
warning naming the missing `provider/model`, which is what a stale default
looks like after a provider's catalog changes.

`piso up` copies the host pi profile's settings (`theme`, `tools`, `prompts`,
`packages`, …) into the shared profile, but never its model selection: if the
shared profile names a dashboard provider, the host's `defaultProvider`,
`defaultModel`, and `enabledModels` are dropped, because `enabledModels`
outranks `defaultModel` in pi's start-up resolution. A machine with its own pi
install therefore cannot change which model a worker starts on. `piso attach`
warns when the shared profile's default disagrees with the dashboard; run
`piso up` to re-sync.

The dashboard loads a provider's catalog as soon as its API base URL and
secret are present, even in the Add provider dialog before it is saved, so the
default model can be picked on the first pass. The same loaded list is kept on
the provider card instead of reporting a pending catalog.

The shared profile is a read-only mount. A running session re-reads it every
15 seconds and applies provider or default changes at its next idle boundary,
so selecting a provider does not require a restart or `/reload`.

### Per-worker overrides

The active provider is the default for every worker. When one worker needs a
different model, the dashboard's **Worker overrides** list pins that worker to
another profile from the same stack — it does not add a provider, a key, or a
second profile. A worker with no override keeps following the active provider,
so an empty override list is exactly the previous behaviour.

The override is keyed by the worker **slug** and stores only a provider id, so
there is at most one per worker: adding a second replaces the first, and
deleting a worker drops its row (a later project that claims the same slug
starts clean). It lives in `state.json` and is never synced to Technocore, the
same way a per-worker internet toggle is local to this gateway.

Workers read their own answer from `GET /api/v1/worker/model` on the same
15-second tick that re-reads the shared profile, and cache it in
`~/.pi/agent/piso-model.json`. That is why an override needs no container
restart and also reaches remote ("cloud-ship") workers, whose `/etc/piso` the
gateway cannot write: the cache carries the last answer, so a worker that boots
while the gateway is down still starts on its pinned provider and corrects
itself on the next successful poll. Precedence is override → active provider →
first registered model, resolved in one place on the gateway.

The provider must already be in the stack, and its secret must be granted to
that worker: an override whose placeholder the worker never receives could
only fail with a 403, so the dashboard refuses the pick and names the fix
(**Access → Secrets**). If a pinned provider is later deleted the row stays
visible and marked, and the worker falls back to the active provider until the
provider is recreated or the override is removed.

### Windows

The worker and gateway are still Linux containers. Native `piso.exe` talks to Docker Desktop (Linux engine). Cloning inside WSL2 and following the Unix install above is simpler and faster for project bind-mounts; use this path when you want a Windows CLI.

Prereqs: Git for Windows, Go (see `go.mod`), Docker Desktop **Linux containers**, Windows Terminal.

```powershell
git clone https://github.com/piso-dev/piso.git
cd piso
.\scripts\install.ps1
cd \path\to\your\project
piso up
piso attach
piso dashboard
```

`install.ps1` builds `piso.exe`, copies it to `%LOCALAPPDATA%\piso\` (`bin\` + `share\piso\`), adds that bin dir to your user PATH, and runs `piso setup --rebuild`. Open a new terminal after the first install so PATH is picked up.

- First `piso up` bakes the Linux worker image (minutes) and may prompt Docker Desktop to share the drive that holds your project.
- Control port defaults to **8081** on Windows (port 80 is often reserved). Dashboard: `http://piso.local:8081` or `http://127.0.0.1:8081`.
- Writing `C:\Windows\System32\drivers\etc\hosts` needs an elevated terminal (`piso sync`). Without it, `piso up` still starts; use the loopback URL.
- There is no hosts-sync Windows service yet; re-run elevated `piso sync` when auto-expose adds a route.
- For `https://*.piso.local` in Edge/Chrome, trust the MITM CA:
  `certutil -addstore -user Root $env:USERPROFILE\.piso\ca.crt`
- Bind-mounting `C:\...` into Linux is slower than a WSL ext4 path. Prefer WSL checkouts if pi feels disk-bound.
- `.gitattributes` forces LF for shell scripts copied into the Linux image. Do not recrlf them.

### Project visibility

`piso down` stops the current worker and unregisters it from the gateway, removing its project row on the dashboard's next refresh. Activity history is retained; `piso up` registers the project again. If the gateway cannot be reached, the CLI reports that removal failed; retry `piso down` once it is available.

Use **Hide** on an individual project to remove it from the default stack without stopping its worker. Select **Show → Hidden projects** to find hidden projects and **Restore** them. Visibility is stored in the gateway and shared across browsers; it does not dismiss requests in **Needs you**. Unregistering a worker clears its visibility preference, so a later `piso up` starts visible again.

The dashboard is **http://piso.local** (host port 80 → container 8081). `piso up` adds `127.0.0.1 piso.local` to `/etc/hosts` when it can; otherwise it prints the line to add. If port 80 is taken:

```bash
piso up --ctrl-port 8081    # then http://piso.local:8081
```

`--proxy-port` and `--ingress-port` work the same way. A taken port is a hard error, not a silent remap. Chosen ports are saved in `~/.piso/ports.json`.

### Hosts sync daemon (auto subdomains)

`piso up` also converges the host on a global hosts-sync service so every
**<slug>-<port>.piso.local** resolves the moment a worker server starts — no
manual `piso expose` or hosts lines. It is managed as root (`sudo make install`
restarts it with the new binary; `make uninstall` stops it):

```bash
piso sync daemon-status    # running? (no sudo needed)
piso sync daemon-restart   # (re)install/start; piso up does this automatically
```

On macOS it is a launchd service (`com.piso.sync`, survives reboots); elsewhere
a nohup+pidfile fallback is used. The daemon only needs `/etc/hosts` write
access and the gateway's control-plane port — workers do not need to run for it
to stay alive.