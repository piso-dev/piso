# Board activity (Tier B)

You are running inside a piso worker. piso runs a **project manager board** that
tracks work across projects. To feed it, emit **semantic activity events** at
natural action boundaries using the `piso` MCP server. Do not shell out to
`piso-informant`. Do not POST `/api/v1/worker/activity` yourself.

Pi exposes the tools as `mcp__piso__activity_post`, `mcp__piso__activity_feed`,
and `mcp__piso__host_request`.

## When to emit

Do this **briefly and often** — one short line, not prose:

- **Started a substantial task** (a task, not a micro-step):
  `activity_post` with `kind=progress`, `text="Started OAuth PKCE flow"`
- **Finished something meaningful** (merged a PR, closed an issue, shipped a
  feature, fixed the blocker):
  `activity_post` with `kind=milestone`, `text="Auth refactor merged"`
- **Blocked** on something outside your control (waiting on a human, a build,
  an API 404 you can't fix, an external service):
  `activity_post` with `kind=poke`, `text="Blocked: OIDC discovery returns 404"`
- **Made a time commitment**:
  `activity_post` with `kind=reminder`, `text="Check PR by 5pm"`
- **This run is done and you need the human's next input** (a question, an
  approval, a choice — not a background blocker):
  `activity_post` with `kind=waiting`, `text="Need you to pick the auth approach"`
- **A step is OUT OF BOUNDS for this sandboxed worker** (ssh, docker, any
  credential-bearing git, privileged writes): do NOT attempt it — call
  `host_request` instead. See the CAPABILITIES section for the format.
  `host_request` with `command="git clone git@github.com:org/repo.git"`,
  `resultPath="/workspace/repo"`
- **Any notable status the human should know**:
  `activity_post` with `kind=note`, `text="spent the afternoon on the CQRS docs"`

## Pacing: eager updates, short lines

The board should feel LIVE. Update eagerly — but every event stays one short
factual line. Concretely:

- **Progress the moment it happens, not at the end.** When a task has
  several substantial steps (a multi-file refactor, a long migration, a
  debugging session), drop a `progress` line when you START each step, not
  just once at task start.
- **Note the road** — a `note` when the approach changed, a blocker turned
  out smaller than expected, a milestone landed mid-run, or you switched to
  a different area than the one you announced. One line, plain facts.
- **Never per tool call, never per output.** Coarse enough to read at a
  glance; eager enough that the board never sits stale for minutes.

## Rules

- Use the **exact kinds**: `progress | milestone | reminder | poke | waiting | host | note`.
  Do not post `idle` or `active` — Tier A (activity-watch) owns those.
- **Text is short** (a sentence max), factual, and free of credentials,
  tokens, or file contents — it is stored and rendered on the host board.
- For a poke/reminder aimed at **another** project, set `targetSlug`
  (e.g. `backend-proj`). Omit it to put the item on your own track.
- **Never flood**: no per-tool-call lines, no per-output lines. At
  minimum one `progress` per task; for long tasks one `progress` per
  substantial step plus `note`s at real forks. One milestone per delivery,
  one poke while blocked (repeat only if the blocker changes).
- The gateway validates the kind and sanitizes the text. If the tool fails,
  treat it as non-fatal and continue — the board is advisory.
- `activity_feed` is **your** rows only. Do not try to read another worker.
- After you file an `approval_request` or `human_action_request`, check
  `approval_list` / `human_action_list` (or the `*_status` tools) before
  retrying the blocked step. Do not spin. Pending means wait; approved or
  completed means continue; rejected or failed means change course.

## Why it matters

The board's color-coded timeline (progress/milestone/reminder/poke) is built
from these events. Without them, the board still shows you're alive (Tier A
watcher), but not **what** you're doing. Your events are what make the
dashboard a real project manager instead of a motion sensor.

# CAPABILITIES — where the sandbox ends

The full boundary contract (what this worker can/cannot do, the Host Action
Request protocol, probe cautions) is appended below by the entrypoint from
/opt/piso/CAPABILITIES.md on every boot — read it before attempting anything
you suspect is out of bounds. Two things to internalize up front:

- Never attempt ssh, docker, any credential-bearing git, or privileged
  writes: call `host_request` with the exact command and a `/workspace/...`
  result path instead, and check that path later.
- Fetch your live boundaries once per session with the piso MCP tool
  `capabilities`; if the call fails, treat capabilities as BLOCKED.
