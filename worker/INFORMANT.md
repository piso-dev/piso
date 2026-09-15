# piso-informant convention (Tier B activity)

You are running inside a piso worker. piso runs a **project manager board** that
tracks work across projects. To feed it, emit **semantic activity events** at
natural action boundaries using the `piso-informant` helper. It is on PATH in
the worker and POSTs to the gateway's activity store (your trusted worker API).

## When to emit

Do this **briefly and often** — one short line, not prose:

- **Started a substantial task** (a task, not a micro-step):
  `piso-informant progress "Started OAuth PKCE flow"`
- **Finished something meaningful** (merged a PR, closed an issue, shipped a
  feature, fixed the blocker):
  `piso-informant milestone "Auth refactor merged"`
- **Blocked** on something outside your control (waiting on a human, a build,
  an API 404 you can't fix, an external service):
  `piso-informant poke "Blocked: OIDC discovery returns 404"`
- **Made a time commitment**:
  `piso-informant reminder "Check PR by 5pm"`
- **This run is done and you need the human's next input** (a question, an
  approval, a choice — not a background blocker):
  `piso-informant waiting "Need you to pick the auth approach"`
- **A step is OUT OF BOUNDS for this sandboxed worker** (ssh, docker, any
  credential-bearing git, privileged writes): do NOT attempt it — emit a
  Host Action Request. See the CAPABILITIES section below for the format and
  the boundary list.
  `piso-informant host "git clone git@github.com:org/repo.git · result → /workspace/repo"`
- **Any notable status the human should know**:
  `piso-informant note "spent the afternoon on the CQRS docs"`

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

- Use the **exact kinds**: `progress | milestone | reminder | poke | active | waiting | note`.
- **Text is short** (a sentence max), factual, and free of credentials,
  tokens, or file contents — it is stored and rendered on the host board.
- For a poke/reminder aimed at **another** project, pass its slug as the 3rd
  arg: `piso-informant poke "Backend PR awaits review" backend-proj`. Omit it
  to put the item on your own track.
- **Never flood**: no per-tool-call lines, no per-output lines. At
  minimum one `progress` per task; for long tasks one `progress` per
  substantial step plus `note`s at real forks. One milestone per delivery,
  one poke while blocked (repeat only if the blocker changes).
- The helper validates the kind and sanitizes the text. If it fails (exit
  != 0), treat it as non-fatal and continue — the board is advisory.

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
  writes: emit `piso-informant host "<exact command> · result → /workspace/..."`
  instead and check for the result at the rendezvous path later.
- Fetch your live boundaries once per session with
  `curl -s "$GATEWAY_URL/api/v1/worker/capabilities?worker=$PISO_WORKER_NAME"`;
  if the fetch fails, treat capabilities as BLOCKED.