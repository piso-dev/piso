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
- `/workspace` is the shared project/files area on the host (rw, shared with
  `piso attach`), not a home for additional clones. `/root/.pi/agent` is the
  persistent, exec-allowed agent volume for sessions, tools and worker-owned
  checkouts; see **Git checkout storage**.
- **Model/API egress goes through the gateway**, and you hold only `piso_…`
  placeholders. Keep it that way: the gateway is how the operator sees what
  left the machine and how real credentials stay out of your hands. Do not
  deliberately route model traffic around it.
- Git over HTTPS with the `gh` credential helper works. From the host network
  namespace (`nsenter -t 1 -n`) the host's own git/ssh setup applies, and
  direct `ssh`/`git@…` may work where it cannot from the container network.

## Git checkout storage

Use `${PISO_WORKBENCH:-$HOME/.pi/agent/workbench}/checkouts/<host>/<owner>/<repo>`
for additional worker-owned clones, outside the shared project in /workspace.
The default is /root/.pi/agent/workbench/checkouts on the persistent,
exec-allowed agent volume. Use /workspace directly only when the task explicitly
targets the assigned shared project; do not nest additional clones there.

- Initialize with `workbench path checkouts` (creates and prints the root).
  In shell examples set
  `checkout_root="${PISO_WORKBENCH:-$HOME/.pi/agent/workbench}/checkouts"`;
  create the host/owner parent with `mkdir -p` before cloning over HTTPS.
- Derive host from the listed clone URL's lowercase hostname (include a
  non-default port as `hostname_port`). Use provider-reported owner and repo
  names, without the trailing `.git` suffix; preserve their case. These are
  literal directory components, not the normalized fork-name slug. Reject empty,
  `.`, `..` or slash-containing components. For deployment prefixes, nested
  groups or any ambiguous mapping, ask for an explicit unique path under
  checkouts rather than silently colliding. Verify the remote URL before reuse;
  a matching directory name alone is not repository identity.
- Leave checkouts for follow-up tasks. All `workbench clean` modes (including
  `--all` and age-based scratch cleanup) preserve `checkouts/`. Run
  `workbench status` for usage; never put durable repos under `tmp/`.
- Do not automatically move or delete existing clones. With explicit migration
  authorization, verify the destination is absent, record branch, HEAD, remotes,
  working-tree state and unpushed commits, then move the checkout intact and
  compare before/after. For cross-filesystem moves, copy and verify before
  removing the source. Preserve local work and report the new path in handoffs.
- Host-assisted clones must also end up here, not as another repo in /workspace.
  The worker's /root/.pi/agent path is not necessarily a host path. Ask the host
  to identify the correct worker container/agent-volume mapping and clone into
  that verified mount, or use an explicitly agreed temporary transfer location
  with a copy/verification/cleanup plan. Never guess a host volume path. Keep
  `host_request.resultPath` as a separate /workspace/... status file visible
  through the shared mount; do not use the checkout itself as the result path.

## Where to install things

Two destinations, pick deliberately:

- **Durable / machine-level** → host paths under `/host` (`/host/usr/local/bin`,
  `/host/opt/...`, an apt package via `nsenter -t 1 -a`). Survives recreate.
- **Throwaway toolchain for this task** → `eval "$(workbench env)"` and
  `workbench install go@1.25.0`, `workbench go install …`, `workbench scratch`.
  Lives on the agent volume; `workbench status` / `workbench clean --caches`
  keep disk pressure down.

Never install toolchains into any checkout (a repository does not want a
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

## You are the host

On a ship you *are* the machine: do the work, then report it in your
`task_update` or activity line. Reach for `host_request` only when the step
genuinely belongs to another box — something that has to happen on the
*gateway* host, which you cannot reach from here. It files a tracked ask a
person resolves from the dashboard, so pass `taskId` (plus `attemptId`,
`requestId`, `summary`, `checkpoint`) whenever you hold one: that is what
blocks the task and ends your lease instead of only announcing the need.

Provide `explanation` (why, effects, prerequisites, working directory and exact
run instructions) and `script`: either `{kind:"inline", filename:"action.sh",
language:"bash", contents:"..."}` with complete contents (at most 20000 UTF-8
bytes), or `{kind:"file", path:"...", language:"bash"}` referencing an existing
host file. Use a verified path on the gateway host, not a path local to this
ship. Keep `resultPath` separate as the output rendezvous. The dashboard offers
a top-right Copy button and internally scrolling script panel; copying does
not execute or complete anything. Legacy `command` is still accepted.

## Interactive sessions suppress the task loop

This worker pulls tasks by itself (`piso-task-watch`), and it claims nothing
while any live pi session exists — including an attached one sitting idle at a
prompt. So `piso attach` is break-glass, not the operating mode. Whoever
attaches lands in `/workspace`, the shared project/files area. Additional clones
are under the workbench as described in **Git checkout storage**; change cwd to
the task's checkout. Use `--shell` for poking at the box rather than a full
session, and exit when done so the queue drains again.

## GitHub clone fallback
Still worth knowing, since the proxy can deny a clone: prepare a fallback with
`gh repo clone` and the parsed `OWNER/REPO` (strip a trailing `/` and `.git`),
preserving the requested destination:

```bash
checkout_root="${PISO_WORKBENCH:-$HOME/.pi/agent/workbench}/checkouts"
mkdir -p "$checkout_root/github.com/example-org"
gh repo clone example-org/service "$checkout_root/github.com/example-org/service"
```

Try the parsed-name form once; do not loop, and never delete a partial clone or
an existing destination automatically. If the proxy or auth genuinely denies it,
use an authorized host-side HTTPS credential setup rather than retrying HTTP
or rewriting remotes to SSH. Follow **Git checkout storage** for verified
host/worker path mapping; never assume the two namespaces share agent paths.

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
