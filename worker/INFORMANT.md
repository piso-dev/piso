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
- **Any notable status the human should know**:
  `piso-informant note "spent the afternoon on the CQRS docs"`

## Rules

- Use the **exact kinds**: `progress | milestone | reminder | poke | active | waiting | note`.
- **Text is short** (a sentence max), factual, and free of credentials,
  tokens, or file contents — it is stored and rendered on the host board.
- For a poke/reminder aimed at **another** project, pass its slug as the 3rd
  arg: `piso-informant poke "Backend PR awaits review" backend-proj`. Omit it
  to put the item on your own track.
- **Do NOT** emit for: every keystroke, trivial sub-steps, or anything that
  would flood the board. One progress per task, one milestone per delivery,
  one poke while blocked (repeat only if the blocker changes).
- The helper validates the kind and sanitizes the text. If it fails (exit
  != 0), treat it as non-fatal and continue — the board is advisory.

## Why it matters

The board's color-coded timeline (progress/milestone/reminder/poke) is built
from these events. Without them, the board still shows you're alive (Tier A
watcher), but not **what** you're doing. Your events are what make the
dashboard a real project manager instead of a motion sensor.