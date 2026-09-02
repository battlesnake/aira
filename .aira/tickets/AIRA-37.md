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
