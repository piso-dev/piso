# Unattended task loop

When `PISO_TASK_ID` is set, you were started for **one** task. This is not a
chat. When it is unset, these unattended-only rules do not apply.

## Start and recover

- Call `whoami` and `capabilities`, then `inbox_get(PISO_TASK_ID)` on every
  start, including when resuming a local session. The task's directives,
  checkpoint, feedback, waits, and artifacts are authoritative; the local
  conversation may be stale or absent.
- Claim that task with `task_claim(id, requestId)`. A successful claim returns
  `attemptId` and `leaseExpiresAt`. A conflict/not-runnable result means exit.
  Never claim another task in this process. Do not invent worker assignments.
- Use the returned `attemptId` on every execution write. Use a fresh unique
  `requestId` per logical mutation; retry a transport failure with the SAME
  requestId and identical arguments. Never reuse an old claim receipt to
  start a new execution.
- Renew with `task_renew` well before expiry (at least every five minutes for
  the default thirty-minute lease). Do not start a blocking command that can
  outlive the lease: use bounded/polled commands and renew between polls.
  If renewal fails or the lease expires, stop side effects and exit. A stale
  attempt cannot write task state even if its old local process is still up.

## Work and handoff

- Persist findings and a handoff with `task_update`. Include repository URL,
  branch, commit, PR, completed work, failed approaches, and concrete next
  steps. Another eligible worker must be able to continue without this session.
- Project/repo labels describe context; they do not grant access. Use existing
  `/workspace` work or an explicitly supplied HTTPS repository URL. If missing
  or inaccessible, ask; never infer credentials or fall back to SSH.
- An unsuccessful approach is an update, not necessarily a failed task.
- Do not push to the default branch. A PR normally means `task_wait` with
  reason `review`, its URL/branch/SHA as artifacts, and a checkpoint—not
  `task_complete`. Completion means acceptance criteria have actually been met.

## Stop explicitly, then exit

- `task_wait`: summary + checkpoint + reason (`review`, `needs_input`,
  `external_dependency`, `stuck`), and the question or required next action.
  This atomically persists the wait and ends the lease. Do not poll while waiting.
- For authorization, use `approval_request` with taskId, attemptId, requestId,
  summary, and checkpoint. For an external human action, use
  `human_action_request` with those fields. They also atomically wait and end
  execution; do not make a second task_wait call afterward.
- `task_release`: checkpoint and summary; work remains runnable for someone else.
- `task_complete`: summary and supporting artifacts/evidence.
- `task_fail`: terminal failure of the goal, with summary and evidence.
  Uncertainty is `task_wait(reason=stuck)`, not terminal failure.
- After any successful stopping operation, exit. Do not sit at a prompt.
- Comments alone do not unblock work or approve an action. Read resolved
  requests in task context before proceeding. Rejection means adapt, not
  permission; never repeatedly file the same rejected ask.
- Board events are advisory, not lifecycle transitions. Posting `waiting` on
  the board does not remove a task from the runnable inbox.
- `hammerspace_*` and `boost_*` are advisory. Skip them on `unpaired` or
  `needs-auth` rather than blocking unrelated work.
