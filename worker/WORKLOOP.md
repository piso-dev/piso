# Opening moves (every task, before you plan or edit)

Discovery is step 1. Run these probes first, in **one batch** — they are cheap
and they change the plan. Do not skip them because the task "looks familiar."
Do not start editing until you have done them or recorded why one does not
apply.

1. `capabilities` — the live boundaries: `internetEnabled` and the
   placeholder names this worker may see. If the call fails, treat
   capabilities as **blocked**, not open. Internet off ⇒ steps 2 and 4 are
   out of scope; say so and continue locally.
2. `github_repositories` (no arguments) — the HTTPS repos this worker can
   reach. Match the task against the list. For each repo the task names or
   clearly needs, clone it over HTTPS into `/workspace`:
   `git clone https://github.com/<org>/<repo> /workspace/<name>`
   If the repo is already under `/workspace`, `git fetch` / `git pull`
   instead. Do not clone the whole list. If `complete:false`, record the
   `errors` and move on — never retry in a loop.
3. `directives_list` (no arguments) — the account's active standing
   directives. They are development preferences, not work: they shape how you
   plan and edit, and they are already yours to follow. A label-scoped
   directive is not in this list; it arrives with its task in `inbox_get`.
   Each has a short code (`D1`, `D2`, …). Cite it by code when it changed what
   you did — "following D4" in a checkpoint, an update, or a handoff — so a
   human can read the board without the full text in front of them. Cite only
   the ones that actually shaped the work; do not list the rest.
4. `boost_search` with the task's key nouns. On a hit, `boost_get` it and
   follow the guidance. On `needs-auth` / `unpaired`, skip and continue;
   never raise it as a task blocker. Check a booster because it plausibly
   matches — do not paste one in just because it exists.
5. Unattended only (`PISO_TASK_ID` set): `inbox_get(PISO_TASK_ID)` — its
   directives, checkpoint, and feedback override anything the local
   conversation remembers.

Before your first edit, state in **one line** which repos, directives (by
code), and boosters you checked (or why none applied). Include that line in your
`task_update` handoff. Self-reporting keeps the reflex honest and lets the
board see the discovery, not just the edit.

**Why this is first:** the most common failure is building net-new what
already exists in a reachable repo, a curated booster, or a standing
directive. Discovery is cheaper than reimplementation.

# Scope you were not given

If discovery turns up work the task did not ask for, propose it — do not widen
your own task. Call `task_propose(requestId, title, body, acceptanceCriteria)`:
it creates a real task in the `proposed` state that no worker can claim until a
human approves it. Reuse the same `requestId` on a retry so you cannot create a
second copy.

Propose when the work is separate, checkable, and someone else could pick it up
later: a broken test you noticed, a missing guard, a follow-up the reviewer will
ask for. Fix it in place instead when it is a two-line consequence of your own
change and the task's acceptance criteria already cover it.

Your proposal stays yours while it is undecided: `task_propose_edit` fixes a
title or body you got wrong, and `task_propose_withdraw` takes it back (with a
reason). Both stop working the moment a human decides — after approval the task
is in the queue and you claim it like any other work.

Do not propose what you were asked to do, and do not propose a standing
preference — directives are the human's to write, and `directives_list` is
read-only for you. Mention the proposal in your handoff by its id so the human
can find it on the board.
