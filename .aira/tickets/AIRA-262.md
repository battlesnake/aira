---
{"schema":1,"id":"AIRA-262","project":"aira","title":"aitest fail-fast leg marker — pool-abort + distinct exit code on a marked test's failure","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.18","labels":["aitest"],"hold":false,"relations":[]}
---

# aitest fail-fast leg marker — pool-abort + distinct exit code

## Problem (plain terms)

A CI gate (deploy's selfcheck fold, AIRA-260 Input 3) wants a *leg* of the
aitest pool to be a **fail-fast smoke leg**: if one designated critical test
fails, don't waste time running the rest of the suite — abort the whole pool at
once and tell the orchestrator (make/gate) to abort the branch build. Today a
failing test is just one `failed` among many; the pool runs the whole queue to
completion and exits `1` (indistinguishable from any ordinary failure).

## Seam (who owns what)

- **aitest (THIS ticket / mine):** a pytest marker `@pytest.mark.aira_failfast`.
  When a MARKED test's outcome is `failed`/`error`, the supervisor **aborts the
  pool** — stop admitting/dispatching, kill live workers — and the run exits with
  a **DISTINCT exit code** (not `1`). Ordering/verdicts of unmarked tests are
  untouched; a suite with zero `aira_failfast` marks is byte-identical to today.
- **deploy (their build):** the gate/make orchestration keys on that distinct
  exit code and aborts the branch. NOT this ticket.

## Design (grounded against source @ master c313680)

Mirrors the existing marker machinery (`aira_mem`/`aira_cpu`/`aira_time`) at
every hop. `aira_failfast` is a **presence** marker (boolean) — no positional
arg, no arg-parsing/warning path (unlike the int readers). Any args are ignored.

1. **Register** (`__init__.py` `pytest_configure`): `addinivalue_line("markers",
   "aira_failfast: ...")` alongside the others, so it never fails collection on
   an ordinary non-aitest run.
2. **Reader** (`__init__.py`): `_aira_failfast_for_item(item) ->
   item.get_closest_marker("aira_failfast") is not None`. Returns a bool; no
   warning tuple (presence-only).
3. **collect()** (`supervisor.py`): build `self._failfast = set()` of marked
   nodeids, alongside `cpu_need`/`time_cost`. Empty until collect() runs;
   `__init__` seeds `self._failfast = set()` and `self._failfast_triggered = None`.
4. **Detect** (`supervisor.py` `_drain_worker`, right after
   `self.results[nodeid] = outcome`, supervisor.py:2894 — BEFORE the recycle
   early-return so it fires regardless of recycling): `if outcome in ("failed",
   "error") and nodeid in self._failfast: self._failfast_triggered = nodeid`.
   Record only — the drain finishes cleanly (events already replayed, result
   recorded). "passed"/"skipped" never trigger; an UNMARKED failure never
   triggers.
5. **Abort** (`supervisor.py` `run()`): in the dispatch loop, right after
   `_service_ready_workers(...)` and BEFORE `_dispatch_to_idle_workers()`,
   `if self._failfast_triggered is not None: self._abort_pool(); break`.
   Fail-fast can only fire inside the main `while self.workers` loop (results
   arrive only via its select()), so one check-point suffices; the `break` stops
   all further dispatch. The post-loop tail (`_report_pool_usage`,
   `_emit_measurement_report`, `_emit_worker_trace`,
   `_synthesize_unevaluated_reports`) runs normally over the partially-drained
   state — the un-run queued + killed in-flight tests become honest
   `unevaluated`.
6. **`_abort_pool()`** (`supervisor.py`): `for pid, state in
   list(self.workers.items()): os.kill(pid, SIGKILL) [OSError-guarded];
   self._retire_worker(pid, state)`. The up-front SIGKILL is load-bearing: an
   in-flight worker is BUSY inside a test and never reads its dispatch pipe, so
   closing it (what `_retire_worker` does first) won't stop it and `_reap_child`
   would wait the whole reap timeout before SIGKILLing — and hold the lease
   charged meanwhile. Kill first → `_retire_worker`'s `_reap_child` reaps at
   once, then releases the lease (relay stdin.close) via the normal crash-funnel.
7. **Distinct exit code** (`__init__.py` `pytest_runtestloop`): after `results =
   supervisor.run(...)`, print the plain per-nodeid lines + aggregate summary as
   normal (honest record of what ran), set `session.testsfailed`, then `if
   supervisor.failfast_triggered is not None: pytest.exit("aira aitest:
   fail-fast — <nodeid> (an @aira_failfast test) failed; pool aborted",
   returncode=_AIRA_FAILFAST_EXIT_CODE)`. pytest 9.0.3's `pytest.exit(...,
   returncode=N)` is the documented mechanism: it raises `Exit`, `wrap_session`
   sets `session.exitstatus = N`, and the process returns N.
   `pytest_sessionfinish`/`pytest_terminal_summary` still run (they're in the
   `finally`), so the summary still prints.

**Exit code value:** `_AIRA_FAILFAST_EXIT_CODE = 42` (PROPOSED — deploy to
confirm they can key on this exact value). Distinct from pytest's 0–5, from a
normal test-failure `1`, from shell 126/127, and from signal codes 128+N.

## Tests (TDD; a test AT EACH HOP — the porous-chain lesson)

- Reader: marked item → True; unmarked → False.
- Register: `aira_failfast` produces no "unknown marker" warning.
- collect(): marked nodeids land in `self._failfast`.
- Detect: unit — marked+`failed` sets `_failfast_triggered`; marked+`error` sets
  it; marked+`passed` does NOT; unmarked+`failed` does NOT.
- `_abort_pool`: unit — SIGKILLs + retires every live worker; `self.workers`
  empty after; monkeypatch `os.kill`/`_retire_worker` to record.
- run() integration: a marked failing test in a pool with queued work → the
  queue does NOT all run (some `unevaluated`), `_failfast_triggered` set.
- Exit code (THE seam test, pytester end-to-end): marked failing test →
  `result.ret == 42`; UNMARKED failing test → `result.ret == 1` (unchanged);
  marked PASSING test → normal exit (no abort).
- Byte-identical: zero marks → the existing 283 aitest tests still pass.

## Accepted gaps / non-goals

- Presence-only (no `reason=`/args). If a future need arises, add then.
- One fixed exit code (not configurable) — a flag that means two things is
  over-design; deploy keys on the one documented constant.
- No per-marked-test exit-code differentiation.

## Design revision (advisor review, pre-build)

Two gaps that CHANGE the code, plus sharpening:

- **Gap 1 (latency bug) — `_replace_worker` spawns between detection and the
  run()-level check.** `_service_ready_workers` drains many workers per pass;
  the recycle branch (:2898) and crash branch (:3070) call `_replace_worker`
  BEFORE run() regains control to see the flag. On the LAST worker that's the
  BLOCKING empty-pool claim (`_bootstrap_from_empty_pool`, :2721-2725) — on a
  saturated daemon the "fail-fast" would stall indefinitely admitting a worker
  it's about to kill. **Fix:** `_replace_worker` early-returns at the very top
  when `self.failfast_triggered is not None`. Test: marked test fails on a
  recycling/last worker → no spawn/claim attempted.
- **Gap 2 (correctness) — the crash path bypasses `:2894`.** A marked test whose
  worker OOMs/segfaults never emits a result line → `_handle_worker_exit` →
  requeue-once → second crash drops it to `unevaluated` (:3068), never `failed`.
  A smoke leg exists to catch exactly this. **Decision: trigger there too** —
  "the leg did not pass" is the contract. Set `failfast_triggered` in the
  give-up branch (:3068, BEFORE its `_replace_worker()` at :3070, so Gap-1's
  guard then blocks the spawn). Test: unit — marked nodeid, `requeue_once`
  exhausted → `_handle_worker_exit` sets the flag.
- **Attribute:** PUBLIC `self.failfast_triggered` (read by `pytest_runtestloop`
  from outside the class); the marked set stays private `self._failfast`.
  `__init__` seeds both (`= None`, `= set()`).
- **First-write-wins:** every trigger guards `if self.failfast_triggered is None
  and ...` — two marked failures in one pass must not overwrite the nodeid in
  the exit message.
- **Deterministic seam test:** `--aitest-workers 1`, the marked failing test
  FIRST in file order, plain tests after → `ret == 42` AND the later nodeids
  print `unevaluated` (N=1 → the marked test runs first, aborts before the
  rest; N>1 would race the "some unevaluated" assertion).
- **Explain the unevaluated flood:** on abort, `pytest_runtestloop` prints one
  plain line before the aggregate — `aitest: fail-fast abort by <nodeid>; N
  tests not run` — so the synthesized-unevaluated count is explained where the
  honest word lives, not only in the `pytest.exit` banner.

Additional accepted gaps (build-record): SIGKILL discards `--cov` data from the
killed in-flight workers (acceptable for an abort); a FALLBACK (no-daemon)
worker's test-forked grandchildren survive its SIGKILL (no daemon scope to
`cgroup.kill`) — same class as the existing crash path, not new.

**Reviewer brief (Fable):** this feature IS a cancellation path — Fable 5's
named recurring blind spot. Attack specifically: abort while a worker has
`in_flight` + staged `pending_events`; abort on a recycling LAST worker (Gap 1);
marked test crashing twice (Gap 2); abort in the same pass as `_maybe_grow_pool`
just spawned; `_retire_worker` on a SIGKILLed worker with a live grant (peak
read before relay close still holds). Standing: what's removable; does the
crash-path decision match the ticket.

## Build + review record

Built TDD (Opus), reviewed by Fable in a detached worktree. **Fable BLOCK →
all addressed; full aitest suite 328 passed, exit 0; three porous-hop mutations
re-confirmed to red.**

- **P1 (BLOCK) — the distinct exit code was hiding WHY it fired.**
  `pytest.exit(returncode=42)` makes pytest's terminal reporter SKIP the whole
  summary (the tripping test's traceback, the short-summary, the AIRA-161
  unevaluated explainer) because 42 is outside its 0–5 range — Fable measured it
  (marked run → 0 traceback hits vs unmarked → 3; my "sessionfinish still runs"
  comment was a confident wrong claim about `terminal.py`). **Fix:** stash the
  tripping nodeid in `pytest_runtestloop`; set `session.exitstatus =
  _AIRA_FAILFAST_EXIT_CODE` in a new `pytest_sessionfinish`. `wrap_session` hands
  the reporter the ordinary exitstatus=1 (full summary prints) then returns the
  mutated `session.exitstatus` — exit 42 WITH the traceback. Regression: the seam
  test now asserts the tripping test's assertion message reaches the terminal.
- **Porous hops pinned (mutations that survived the shipped suite):** m4 the
  run() `_abort_pool()` CALL (a 60s in-flight test must be reaped + lease released
  on a sibling's failure), m6 crash-path trigger ordering before `_replace_worker`
  (the no-op stub hid it — record the flag at call time), m7 exit-code
  distinctness (seam tests import the constant, so 42→1 stayed green — pin the
  literal). Plus end-to-end Gap-1 (recycling last worker: no respawn) and Gap-2
  (crash-twice trips / crash-once-then-pass does not) real-fork regressions.
  Re-confirmed red: m4, m7, and the P1 traceback fix.
- **Latency (P2):** a give-up trip via `_dispatch_to_idle_workers`' BrokenPipe
  branch kept dispatching to the other idle workers in the same pass. Guard now
  checks the flag INSIDE the dispatch loop, and run() re-checks at the top of the
  loop so any late trip aborts before the next blocking select.
- **P3s:** killed in-flight tests get a self-explaining unevaluated reason;
  the plain line says "N unevaluated" not "N not run"; the exit-code comment
  marks 42 PROPOSED (deploy to confirm), not "confirmed".

**Documented gap (accepted):** a marked leg that never runs — refused admission
(exceeds ceiling) or a terminal daemon failure draining the queue — is marked
`unevaluated` WITHOUT tripping, so it exits 1 not 42. That is an infra failure,
not a leg failure, and a gate still sees non-zero. Kept the two-site detection
(result-line + crash give-up) over Fable's single-site route to keep the change
minimal and not expand the trigger to admission-refusal.

## Provenance

Source design: AIRA-260 Input 3 (fail-fast leg marker), reattributed to
deploy's selfcheck fold (PR #169). Targets release **v0.18**. Two-loop: Opus
builds (TDD), Fable reviews in a detached worktree.
