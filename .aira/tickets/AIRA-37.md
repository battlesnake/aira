---
{"schema":1,"id":"AIRA-37","project":"aira","title":"aitest Slice 1 build-review follow-ups: atfork ordering, pool over-spawn, connection deadlines, misc hardening","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-38","to":"AIRA-37"}]}
---
Lower-priority findings from AIRA-30's adversarial build-review (Sol +
Fable) not fixed in the initial pass — the two P0s and the two
highest-confidence P1s (both verified live against real pytest) were
fixed immediately; these are the remainder, explicitly rated lower
urgency by the review synthesis ("fold in, lower urgency").

**P1 (disputed severity between Sol and Fable, worth an audit, not a
rushed fix):** `worker.py`'s `fork_worker` docstring claims the
pre-placement window is "pure interpreter overhead" with no risk of
arbitrary code running before `place_self()`. Sol found and reproduced
that Python executes registered `after_in_child` atfork handlers BEFORE
`os.fork()` returns to Python — so a third-party library's atfork
callback genuinely can run in that window, contradicting the comment's
literal claim. Fable's independent review called the same code path
correct. Reviewer-synthesis nuance: even if a callback runs early, any
memory it touches is still hierarchically charged to the outer scope
(not literally unbounded) — the sharper risk is a callback trying to
acquire a lock held by another thread at fork time (a fork-across-threads
deadlock hazard), not a containment break per se. Action: audit whether
this process registers any atfork handlers of its own (it shouldn't) and
whether any imported library plausibly does; correct the docstring's
"pure interpreter overhead" claim either way, since it's demonstrably not
quite accurate as stated.

**P1 (Sol only, not disputed, just lower urgency):** the fallback-pool
and confined-pool startup spawn loops in `run()`/`_spawn_fallback_worker`
check `if not self.queue: break` but the queue isn't actually decremented
until the LATER `_dispatch_to_idle_workers()` call — so with (e.g.) one
real test and `--aitest-workers=4`, all 4 workers get forked/admitted
before any dispatch happens, wasting fork+admission overhead. Not a
correctness bug (unused workers cleanly retire via the normal
end-of-run `__stop__` path) but real waste, and combined with AIRA-30's
new aggregate-cap guard, needless over-spawning could make hitting that
cap on the confined path more likely than necessary. Fix: track a local
"tests still needing a worker" counter decremented per spawn, or dispatch
incrementally rather than spawn-then-dispatch in two separate passes.

**P2s (bundle):**
- `cmd/aira/main.go`'s `runWorkerAdmitCommand` sets no context deadline,
  and `RequestWorkerAdmit`'s dial never sets a connection deadline either
  — a wedged daemon that accepts a connection but never replies can hang
  the CLI indefinitely, and transitively hang the whole Python supervisor
  (its `process.stdout.readline()` blocks on it). Bound with the
  request's own `max_wait` plus reasonable slack, mirroring how
  `admitThroughDaemon` already derives its own transport deadline from
  the capped max-wait (`admission_linux.go`).
- `WorkerAdmitResponse.waited_ms` is defined on the wire type but never
  actually populated by `evaluateWorkerAdmit`/`workerAdmitConnection` —
  every response reports zero wait even when the poll loop genuinely
  waited. Populate it from the actual elapsed time in the poll loop.
- `CreateWorkerScope` doesn't remove the scope directory it just created
  if writing the memory cap fails partway through; the CLI similarly
  doesn't clean up after a grant-output-write failure. Both leak an empty
  cgroup dir on a rare failure path — add best-effort cleanup on the
  failure branch, matching the "best-effort, log-and-continue" convention
  already used elsewhere in this codebase.
- Malformed `AIRA_AITEST_WORKER_*` env ints (`worker.py`'s
  `_should_recycle`, reading `AIRA_AITEST_WORKER_MAX_SECONDS` etc.) raise
  AFTER a test has run but BEFORE its result is flushed, losing that
  result and repeating per worker until the suite drifts to unevaluated.
  Parse defensively with a fallback to the compiled default on a
  malformed value, logging once, rather than raising mid-loop.

Not urgent enough to block AIRA-30's merge (all four categories are
either disputed-but-low-impact, pure efficiency, or narrow failure-path
polish) — tracked here so they aren't silently dropped. relates AIRA-30.

## Re-scoped (2026-09-04, backlog-remediation Phase 0, plan section 2) — 4 of 6 already fixed

Each item below was re-verified against current source rather than inherited from
the plan's snapshot.

**CLOSED as already fixed:**

- *Worker-admit connection deadlines.* `RequestWorkerAdmit`
  (`internal/runner/worker_admit_client_linux.go:74-81`) now derives a transport
  deadline from the request's own `MaxWait` plus a fixed grace margin, honouring
  an earlier caller deadline if one is present — exactly the fix this item asked
  for, mirroring `admitThroughDaemon`.
- *`WorkerAdmitResponse.waited_ms` never populated.* Populated at
  `internal/daemon/worker_admit.go:443` from the poll loop's real elapsed time.
- *`CreateWorkerScope` leaks the directory on a cap-write failure.* It removes
  the scope directory on the failure branch
  (`internal/runner/worker_scope_linux.go`).
- *Malformed `AIRA_AITEST_WORKER_*` ints raise mid-loop.* `worker.py`'s
  `_env_int`/`_env_float` (`:226-240`) parse defensively, warn once, and fall
  back to the compiled default.

**STILL OPEN — the two genuine residues, both moved to Phase 2 (plan section 4):**

1. ~~*The atfork docstring.*~~ **FIXED 2026-09-05 (backlog-remediation Phase 2).**
   `fork_worker`'s "pure interpreter overhead" claim is retracted outright, and
   the audit the ticket asked for is recorded in the docstring with real
   registrants rather than a "shouldn't happen" assurance: aitest registers no
   `after_in_child` handler of its own, but `logging`/`threading`/`random` all do
   at import (pytest imports all three) and AIRA's own `aira_xdist_governor`
   registers one at module scope — which is how forked aitest workers disable
   that governor (AIRA-92), so real code runs in this window on a co-registered
   run. The two bounds that survive (before any *test* code; still charged to the
   outer scope's hierarchical cap) are kept and the sharper risk is named as the
   fork-across-threads lock hazard, not a containment break. The same false claim
   in `docs/superpowers/specs/2026-09-01-aitest-design.md` §3.3 was corrected in
   the same commit, and the ordering fact is pinned by
   `test_atfork_after_in_child_handlers_run_before_fork_returns`.
2. *Worker-dispatch over-spawn.* `supervisor.py:1131` still reads
   `if not self.queue: return` with no in-flight counter, so nothing prevents
   spawning more workers than there is queued work.

Only those two remain in scope for this ticket.
