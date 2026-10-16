# Opening moves (every task, before you plan or edit)

Discovery is step 1. Run these probes first, in **one batch** — they are cheap
and they change the plan. Do not skip them because the task "looks familiar."
Do not start editing until you have done them or recorded why one does not
apply.

1. `capabilities` — the live boundaries: `internetEnabled` and the
   placeholder names this worker may see. If the call fails, treat
   capabilities as **blocked**, not open. Internet off ⇒ steps 2 and 3 are
   out of scope; say so and continue locally.
2. `github_repositories` (no arguments) — the HTTPS repos this worker can
   reach. Match the task against the list. For each repo the task names or
   clearly needs, clone it over HTTPS into `/workspace`:
   `git clone https://github.com/<org>/<repo> /workspace/<name>`
   If the repo is already under `/workspace`, `git fetch` / `git pull`
   instead. Do not clone the whole list. If `complete:false`, record the
   `errors` and move on — never retry in a loop.
3. `boost_search` with the task's key nouns. On a hit, `boost_get` it and
   follow the guidance. On `needs-auth` / `unpaired`, skip and continue;
   never raise it as a task blocker. Check a booster because it plausibly
   matches — do not paste one in just because it exists.
4. Unattended only (`PISO_TASK_ID` set): `inbox_get(PISO_TASK_ID)` — its
   directives, checkpoint, and feedback override anything the local
   conversation remembers.

Before your first edit, state in **one line** which repos and boosters you
checked (or why none applied). Include that line in your `task_update`
handoff. Self-reporting keeps the reflex honest and lets the board see the
discovery, not just the edit.

**Why this is first:** the most common failure is building net-new what
already exists in a reachable repo or a curated booster. Discovery is
cheaper than reimplementation.
