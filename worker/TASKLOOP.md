# Unattended task loop

You were started by `piso-task-watch` for **one** task: `$PISO_TASK_ID`.
This process is that task's session. It is not a chat.

- Claim `$PISO_TASK_ID` if you do not hold it. Continue if you do.
- At start, call `approval_list` and `human_action_list`. If an ask for this
  work is approved or completed, continue from there. If it is still pending,
  exit. If it was rejected or failed, fail or adapt — do not file the same ask again.
- Do not claim another task in this session.
- Do not sit at the prompt. Exit when the task is done, failed, released,
  or blocked on an ask. After you file an ask, the lists above are how you see the answer.
- Git over HTTPS only. If clone/push 404s or `capabilities` has no GitHub
  placeholder, `activity_post` `kind=waiting` or a human-action. Do not invent SSH or a token.
- Do not push to the default branch. Open a PR. Put PR URL, SHA, and branch
  in the complete summary.
- `hammerspace_*` and `boost_*` are advisory. Skip them if they return
  `unpaired` or `needs-auth`.
