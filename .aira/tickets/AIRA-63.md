---
{"schema":1,"id":"AIRA-63","project":"aira","title":"worker-admit has no concurrency bound, so its wait ceiling must stay far below the shared 24h ceiling","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","daemon"],"hold":false,"relations":[]}
---
## Observation (verified in source)

`admitConnection` is gated by `admitSlots` (`internal/daemon/admit.go`, capacity `admitGlobalMax = 1024`), so a long admission wait costs one of a bounded number of slots.

`workerAdmitConnection` has **no such gate** — `admitSlots` appears nowhere in `internal/daemon/worker_admit.go` — yet it retains a connection and a polling goroutine for the entire wait (`worker_admit.go:356-430`).

## Why it matters now

AIRA-58 replaced a hardcoded 30-minute clamp with a shared `runner.AdmitWaitCeiling = 24h`. That ceiling was deliberately **not** applied to `worker-admit`: raising an unbounded path's wait ceiling by 48x would permit unboundedly many concurrent retained connections and goroutines. `worker-admit` therefore keeps its own 30-minute ceiling (`workerAdmitWaitCeilingMs`), and `TestWorkerAdmitCeilingStaysBelowTheSharedAdmitCeiling` fails if someone later "unifies" the two for consistency.

That test is a guard, not a fix. The underlying asymmetry remains.

## Suggested direction

Give `worker-admit` a concurrency bound (either share `admitSlots` or add its own), then the ceilings can be unified and the guard test relaxed. Until then the split ceiling is deliberate and must stay.

Not a live problem: the only caller, the aitest supervisor, uses waits of ~300s, two orders of magnitude inside the current bound.

Raised during AIRA-58/AIRA-59 (plan `docs/superpowers/specs/2026-09-03-aira58-59-admission-wait-and-freeze-plan.md` §4.1, §8).
