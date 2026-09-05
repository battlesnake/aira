---
{"schema":1,"id":"AIRA-105","project":"aira","title":"Adopted-scope self-heal latency after a supervisor dies — reported live, not independently reproduced","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","dogfood"],"hold":false,"relations":[]}
---
Reported by peer session `field` (2026-09-05), investigated live by the coordinating session. Recorded honestly: the specific instance field found had already cleared by the time it was investigated, so this is a credible report with partial independent grounding, not a reproduced-and-confirmed bug.

## What field reported

`aira confine --list` showed `slice reserve: 62725860K granted / 63232M ceiling` with a queued waiter, while the machine had ~45GB genuinely free. One "adopted scope" in the table: `SUPERVISOR-PID 4096382`, `LIVE no`, `LEAF-PROCS 0`, RSS 3.36GB, held under an `@dr-`-prefixed scope id (differs from the `job-` prefix on healthy scopes, suggesting it arrived via post-restart adoption rather than a normal launch). `ps -p 4096382` returned nothing — the supervisor was confirmed dead. field did not touch it (correctly, per this project's blast-radius discipline) and reported rather than acting.

## What the coordinating session found on investigation, minutes later

By the time this was investigated, the visible job roster had already moved on (different jobs entirely — the `4096382` scope was no longer listed). The "adopted scopes" total at that moment (`52420120576` bytes) turned out to correspond EXACTLY to the summed `Cap` of two different, independently-verified-alive jobs (`ps -p` confirmed both running) — i.e., what was visible at investigation time was legitimate conservative reservation accounting for two real, long-running jobs, not a leak. A daemon restart (done for an unrelated deploy) also did not reveal any additional stale entries: immediately post-restart `adopted_jobs: 0`, and it climbed back to a figure matching real live jobs within seconds as they re-registered.

**So the specific dead-`4096382` entry field found was not independently reproduced here** — it had already self-healed (consistent with the AIRA-74 precedent's own "self-heals as jobs finish" design) by the time this investigation started, a window of at least several minutes. That is the actual open question this ticket records:

## The open question

How long can an adopted-scope ledger entry survive after its underlying supervisor has died, before self-healing removes it? field's report — RSS still shown (3.36GB), scope still counted against the ceiling, supervisor confirmed dead via `ps` — implies at least minutes, long enough to produce a real queued-waiter block on a machine with substantial free RAM. The reconstruction code (`internal/daemon/admit.go`, the `adopted`/`adoptedJobs` loop around the `refreshInterval`-gated rescan) re-derives this from a live cgroup scan each refresh cycle, which should in principle notice a scope has become unpopulated/gone — but the actual latency and the exact trigger for removal were not traced against source as part of this report; that tracing is this ticket's own scope.

## Also found, unrelated, already cleaned up

An orphaned, empty test-artifact cgroup (`aira103probe`) was sitting directly under the real `aira.slice` — left behind by AIRA-103's build agent's own real-cgroup testing, contrary to that build's explicit instruction to use an isolated test slice/cgroup, never the shared one. It was empty (0 processes) and has been `rmdir`'d directly by the coordinating session as a trivial, safe cleanup — not itself connected to the adopted-scope question above, but worth a note in AIRA-103's own follow-up that its test isolation should be checked, in case other real-cgroup tests in that PR have the same leak (harmless when empty, but pollutes the shared slice's directory listing for anyone inspecting it, as it did here).

## Not scoped or built here

Whether the self-heal latency is acceptable as-is, needs a tighter refresh interval, needs an active "supervisor liveness" check rather than waiting for the next scan, or some other direction — needs tracing the actual code path and, ideally, a way to reproduce the dead-supervisor-still-adopted window on demand (kill a supervisor mid-test, observe how many scan cycles pass before it clears) rather than relying on an incidental live observation.
