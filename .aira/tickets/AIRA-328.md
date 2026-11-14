---
{"schema":1,"id":"AIRA-328","project":"aira","title":"confine --kill must resolve a QUEUED job (cancel the waiter) by name, supervisor pid and scope id","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.41","labels":["rant-triage"],"hold":false,"relations":[]}
---
About 35 rants (RANT-61,66,71,75,79..101,103,110,113..127). aira confine --list shows a queued job with name+supervisor_pid+scope_id but --kill answers E_CONFINE_NOT_FOUND because KillConfine (internal/runner/confine_manage.go) resolves against cgroup scopes only and a queued job has none. --front already resolves queued waiters. Fix: daemon-side cancel of the queued waiter with the same ownership guard (--steal for another owner); ledger/queue invariants and the exclusive gate must hold; the client supervisor must exit with a distinct, honest trailer. Correctness-critical: full two-loop.

## Accepted gaps and deviations (as built)

- Trailer: a cancelled queued job prints the never-ran line `confine: ran=no code=U_ADMIT_CANCELLED ... admission=cancelled` and exits 3. It has no `terminated-by=` facet, because nothing ran.
- `--kill <name>` when a queued job and a running job share that name is now refused `E_SELECTOR_AMBIGUOUS` (before AIRA-328 it killed the running one). Use the scope id.
- Ambiguity counts the daemon's granted waiters plus (real mode) the running scopes found on disk. A job surviving a daemon restart that has not yet re-declared its lease is counted only through its on-disk scope; in ci-shim mode there are no scopes, so it is not counted at all.
- An aitest worker's queued claim is never cancelled. By its exact scope id `--kill` refuses `E_CONFINE_KILL_WORKER_CLAIM` and names the suite to kill; by name or pid it is not selected (so `--kill <supervisor-pid>` still reaches the suite). The claim stays queued.
- A cancelled scope id is refused for 30 minutes after the cancel (time-based, not count-based), so a client that lost the cancel reply cannot reconnect and run. The record is in memory: a daemon restart forgets it.
- Client receivers of `U_ADMIT_CANCELLED` (`--detach`, the `--exclusive` / `aira drain wait` wait, the ci-shim) were verified by reading the code only, not by an end-to-end run.
- Deferred: a faster cancel of a RUNNING job, batch kill, kill-by-owner.
- Cutover needs the client install AND a daemon restart. Post-cutover smoke: one queued cancel, one `--detach` queued cancel followed by `--status`, one `aira drain wait` cancel.
