---
{"schema":1,"id":"AIRA-328","project":"aira","title":"confine --kill must resolve a QUEUED job (cancel the waiter) by name, supervisor pid and scope id","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.41","labels":["rant-triage"],"hold":false,"relations":[]}
---
About 35 rants (RANT-61,66,71,75,79..101,103,110,113..127). aira confine --list shows a queued job with name+supervisor_pid+scope_id but --kill answers E_CONFINE_NOT_FOUND because KillConfine (internal/runner/confine_manage.go) resolves against cgroup scopes only and a queued job has none. --front already resolves queued waiters. Fix: daemon-side cancel of the queued waiter with the same ownership guard (--steal for another owner); ledger/queue invariants and the exclusive gate must hold; the client supervisor must exit with a distinct, honest trailer. Correctness-critical: full two-loop.

## Accepted gaps and deviations (as built)

- Trailer: a cancelled queued job prints the never-ran line `confine: ran=no code=U_ADMIT_CANCELLED ... admission=cancelled` and exits 3. It has no `terminated-by=` facet, because nothing ran.
- `--kill <name>` when a queued job and a running job share that name is now refused `E_SELECTOR_AMBIGUOUS` (before AIRA-328 it killed the running one). Use the scope id.
- The queued cancel is resolved in one locked section and reads nothing from disk. Ambiguity counts only the daemon's granted waiters: a same-name running job that survived a daemon restart and has not yet re-declared its lease is NOT counted, in real mode as well as ci-shim mode. The disk scan that used to cover it was removed, because its out-of-lock window could let the cancel fall through and kill the running twin.
- Pre-existing on master, unchanged: with no queued match, `--kill` falls through to the scope kill. A job granted moments ago, whose scope does not exist yet, is invisible to it, so `--kill <name>` shared with a running job can kill the running one. When the selector names one granted job and the scope kill finds nothing, the answer is `U_CONFINE_NOT_LAUNCHED` (retry), not `E_CONFINE_NOT_FOUND`.
- An aitest worker's queued claim is never cancelled. By its exact scope id `--kill` refuses `E_CONFINE_KILL_WORKER_CLAIM` and names the suite to kill; by name or pid it is not selected (so `--kill <supervisor-pid>` still reaches the suite). The claim stays queued.
- A cancelled scope id is refused for 30 minutes after the cancel (time-based, not count-based), so a client that lost the cancel reply cannot reconnect and run. The record is in memory: a daemon restart forgets it.
- Client receivers of `U_ADMIT_CANCELLED` (`--detach`, the `--exclusive` / `aira drain wait` wait, the ci-shim) were verified by reading the code only, not by an end-to-end run.
- Deferred: a faster cancel of a RUNNING job, batch kill, kill-by-owner.
- Cutover needs the client install AND a daemon restart. Post-cutover smoke: one queued cancel, one `--detach` queued cancel followed by `--status`, one `aira drain wait` cancel.
