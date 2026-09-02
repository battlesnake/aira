---
{"schema":1,"id":"AIRA-41","project":"aira","title":"worker-admit ledger grant survives an abnormally-killed admit relay","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
---
Found by Fable build-review (final gate, AIRA-30). Distinct from AIRA-39
(daemon restart) and AIRA-40 (result-pipe EOF) -- this is a THIRD, separate
gap in the same worker-admit ledger area, not covered by either.

**The gap.** A worker's ledger entry (`internal/daemon/worker_admit.go`,
`job.grants`) is freed by its admit-relay CONNECTION dying (socket EOF ->
`releaseWorkerGrant`), but relay death is not coupled to WORKER death. An
abnormally-killed relay (the worker itself still alive, still under its
granted `memory.max`) silently removes the grant from the aggregate-cap
guard's `committed` sum, reopening the Σ(live worker caps) + supervisor >
ceiling over-commit window Goal 2 exists to close.

**Failure scenario.** A stray `pkill`/watchdog/user action kills one
worker's `aira worker-admit` relay process while the supervisor and the
worker itself keep running (the worker has no reason to notice -- its own
cgroup placement and cap are already established). The daemon releases
the grant on socket EOF, admits a replacement worker against the now-
freed capacity, and both workers then grow toward their own (still
intact) caps -- the outer scope's `memory.oom.group` kills the entire run.

**Candidate fix directions (not designed here).**
- Have `releaseWorkerGrant` check the granted scope's `cgroup.procs`
  population before dropping the ledger entry -- a populated scope at
  release time is always the abnormal case, since `_retire_worker`
  (supervisor.py) only rmdirs a scope AFTER `waitpid` confirms the worker
  itself is gone.
- Or: document this as an accepted Slice 1 gap if the fix is judged not
  worth the added complexity (architectural-simplicity call for the
  owner).

Per the project rule that coverage gaps are written down and accepted by
reviewers, never silent -- this ticket is that record.

relates AIRA-30, AIRA-38, AIRA-39.
