# CAPABILITIES — this ship is yours

This file is appended to AGENTS.md on every boot by the entrypoint, in place of
`CAPABILITIES.md`. It applies when `PISO_HULL=cloud_ship`: this container runs
on its **own machine**, which exists only to run you.

> **You are root on this machine. There is no host-side step to forward. If
> the machine can do it, do it.**

Read the rest for *how*, not for permission.

## What you have

- **The host filesystem, read-write, at `/host`.** Every host path exists
  there: `/host/etc`, `/host/home`, `/host/var/lib/docker`, `/host/root`. The
  container's own `/` is a disposable image root — editing it changes nothing
  after the next recreate, so prefer `/host/...` when a change should survive.
- **The host's namespaces via `nsenter`.** `pid: host` means you can see host
  processes, and:
  ```bash
  nsenter -t 1 -a -- bash     # full handoff: host mount/uts/ipc/net/pid
  nsenter -t 1 -n -- ss -ltnp # host network view only
  nsenter -t 1 -m -- chroot /host systemctl status piso  # host services
  ```
  Prefer `nsenter -t 1 -a` for anything that assumes it is "on the machine"
  (`systemctl`, `apt`, `ip`, `iptables`, host `docker`): it *is* the machine,
  so no path or namespace juggling is needed.
- **The Docker socket.** `/var/run/docker.sock` is mounted, so `docker ps`,
  `docker compose` and building images on this host all work. Run expensive or
  long-lived workloads as sibling containers rather than fighting the
  constraints of this one.
- **Writable container root.** No read-only rootfs, no dropped caps, no
  no-new-privileges. `apt-get install`, `pip`, `npm -g` all work inside the
  container; they are wiped on recreate, which is why durable state belongs on
  `/host` or on the agent volume.
- `/workspace` is a scratch workspace on the host (rw, shared with
  `piso attach`), **not** one project: tasks clone their repository to
  `/workspace/<owner>/<repo>` and work there. `/root/.pi/agent` is the
  persistent agent volume (sessions, skills, extensions, logs) — the one
  exec-allowed writable dir that is *not* the host.
- **Model/API egress goes through the gateway**, and you hold only `piso_…`
  placeholders. Keep it that way: the gateway is how the operator sees what
  left the machine and how real credentials stay out of your hands. Do not
  deliberately route model traffic around it.
- Git over HTTPS with the `gh` credential helper works. From the host network
  namespace (`nsenter -t 1 -n`) the host's own git/ssh setup applies, and
  direct `ssh`/`git@…` may work where it cannot from the container network.

## Where to install things

Two destinations, pick deliberately:

- **Durable / machine-level** → host paths under `/host` (`/host/usr/local/bin`,
  `/host/opt/...`, an apt package via `nsenter -t 1 -a`). Survives recreate.
- **Throwaway toolchain for this task** → `eval "$(workbench env)"` and
  `workbench install go@1.25.0`, `workbench go install …`, `workbench scratch`.
  Lives on the agent volume; `workbench status` / `workbench clean --caches`
  keep disk pressure down.

Never install into a checkout under `/workspace` (a repository does not want a
random toolchain in it) and never leave builds in `/tmp` or `/dev/shm` (tmpfs,
mounted **noexec** — a binary unpacked there dies with exit 126 and looks
broken).

## Boundaries that remain (and why)

- **Real credentials.** You still only ever hold `piso_…` placeholders; the
  real values live in the gateway's vault on another machine. There is nothing
  to find on the ship, and no reason to look for one.
- **Model/API egress through the gateway** — as above. This is the product's
  whole point: not to limit what you *do*, but to keep an auditable choke point
  on what leaves toward the model providers.
- **Other people's data on this machine.** Root means you *can* read any
  secret, cloud instance-metadata credential, or other user's key present on
  the host. Do that only when the task genuinely requires it, and say so in
  your handoff.
- **Do not fight the container from inside.** `docker exec` into yourself,
  editing `compose/*.yaml` for the running worker, or restarting
  `piso-worker-<slug>` are all self-inflicted outages. Change the host around
  it, or ask.

## Do not emit Host Action Requests

`CAPABILITIES.md` describes a `host_request` protocol because a local worker
cannot touch its host. On a ship that is inverted: you *are* the host, so a
host request is a stall, not a handoff. Do the work, then report it in your
`task_update`/activity line.

Use `kind=host` / `host_request` only when the machine genuinely cannot do it
itself — e.g. something that must happen on the *gateway* box.

## Interactive sessions suppress the task loop

This worker pulls tasks by itself (`piso-task-watch`), and it claims nothing
while any live pi session exists — including an attached one sitting idle at a
prompt. So `piso attach` is break-glass, not the operating mode. Whoever
attaches lands in `/workspace`, the scratch root; the work is one level down,
under `<owner>/<repo>`. Use `--shell` for poking at the box rather than a full
session, and exit when done so the queue drains again.

## GitHub clone fallback
Still worth knowing, since the proxy can deny a clone: prepare a fallback with
`gh repo clone` and the parsed `OWNER/REPO` (strip a trailing `/` and `.git`),
preserving the requested destination:

```bash
gh repo clone lunar9-dev/v1-mono /workspace/v1-mono
```

Try the parsed-name form once; do not loop, and never delete a partial clone or
an existing destination automatically. If the proxy or auth genuinely denies it,
open an SSH path from the host namespace instead of retrying HTTP.

## Live boundaries (fetch once per session)

Call the piso MCP tool `capabilities`. It returns `internetEnabled` and
`scopedPlaceholders`. If it fails, treat those as **blocked** — the gateway is
what you need for model calls, and its answer is the truth about which
placeholders you hold.

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
already up), then call `activity_post` with `kind=waiting` and
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

A hung command still costs you the task, even on your own machine. Wrap
open-ended network probes in `timeout 3`:

```bash
timeout 3 curl -sI https://example.com     # through the gateway proxy
timeout 3 git ls-remote https://github.com/org/repo.git
nsenter -t 1 -n -- ss -ltnp                # what the host is listening on
docker ps --format '{{.Names}}\t{{.Status}}'   # what else is running here
```
