---
{"schema":1,"id":"AIRA-38","project":"aira","title":"aitest Slice 1 follow-ups round 2: error-classification gaps, coverage gaps, spec drift (4-lens Fable review)","status":"done","kind":"chore","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","hardening","test-coverage"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-39","to":"AIRA-38"},{"kind":"relates","from":"AIRA-40","to":"AIRA-38"},{"kind":"relates","from":"AIRA-44","to":"AIRA-38"}]}
---
Findings from a second, four-lens critical-only Fable review (daemon/cgroup,
Python state machine, test-coverage gaps, spec/code consistency) not fixed
immediately. The two P0s from this round (BrokenPipeError on dispatch to a
worker dead since its last drain; denial at last-worker retirement ending
the run early) and one P1 (aggregate guard omitting supervisor RSS) WERE
fixed immediately with regression tests -- this ticket is the remainder.
Overlaps with AIRA-37 (round 1 follow-ups) are noted; do these together.

**P1 — error-classification chain lets a config/argument problem escalate
to permanent loss of confinement.** Three compounding gaps: (a)
`internal/runner/worker_admit_client_linux.go:78-82` unmarshals
`response.Data` without checking `response.OK`/`response.Error` first
(contrast `admitThroughDaemon`) -- an `errorFrame` carries nil `Data`, so
every daemon-side validation error surfaces as "malformed worker-admit
response"; (b) `runWorkerAdmitCommand` accepts any positive `--max-wait`
but `validateWorkerAdmitArgs` rejects `max_wait_ms > 30min` as a protocol
error instead of clamping like slice-level `admit` does; (c) the Python
classifier (`supervisor.py`'s denied/timeout substring match) treats
anything matching neither substring as `WorkerAdmitUnavailable` ->
`_disable_daemon`. Chained: an oversized `--max-wait` (or any Go-side
message reword) silently converts a `denied` into `WorkerAdmitUnavailable`,
stripping containment for the rest of the run with the real cause masked --
the exact P0 class the previous round fixed, resurrectable with all
default-tier tests green (finding #9's point: no test crosses the
Go<->Python classification boundary with the real binary). Fix: check
`response.Error`/`OK` before unmarshalling; clamp `max_wait_ms`
server-or-client-side; add a default-tier contract test running the real
`aira worker-admit` binary against deny/timeout daemon fixtures and
feeding its actual stderr through the Python classifier.

**P1 — a permanent `reject:exceeds-ceiling` denial hangs the startup wait
loop forever behind a factually wrong "budget contended" warning.** The
daemon correctly short-circuits requests that could never fit as a stable
fact, but the Python client's substring classifier collapses it into the
same `WorkerAdmitDenied` as a transient contention denial, so the new
indefinite-retry loop retries a never-satisfiable request forever, printing
"budget contended" when the real problem is a mis-sized
`AIRA_AITEST_ESTIMATED_BYTES`. Propagate the daemon's actual denial reason
through to the Python side and treat `exceeds-ceiling` as a distinct,
terminal (non-retriable) condition client-side.

**P1 — a malformed/truncated `granted` line crashes the run with an
unclassified `KeyError`.** `acquire_worker` parses any `granted ...` prefix
without validating required keys (`scope`, `worker_id`, caps);
`spawn_worker`'s subsequent `grant["scope"]` raises `KeyError`, caught by
none of the surrounding except clauses, propagating out of `run()` and
losing all results -- contradicts the docstring's own claim that malformed
responses become `WorkerAdmitUnavailable`. Validate required keys after
parsing; raise `WorkerAdmitUnavailable` (releasing the admit process first)
on a bad line.

**P1 — `drainIntoScope` (bootstrap) treats a just-exited pid as fatal.**
`internal/runner/aitest_bootstrap_linux.go`'s drain loop errors on any
`moveIntoScope` failure; a transient child exiting between the
`cgroup.procs` read and write fails with ESRCH (a zombie fails the same way
non-transiently), and the error propagates to `_disable_daemon` -- a
sub-millisecond pid race silently strips admission and per-worker caps for
the entire suite. Tolerate ESRCH/ENOENT (a gone pid is by definition
drained) and continue the loop.

**P1 — daemon-reachable `unevaluated` (outer-scope read failure) also
routes to permanent unconfined fallback**, same root cause as the two items
above (the Python classifier has no distinct bucket for it) -- a
potentially transient read failure from a daemon that is plainly still
there strips containment for the rest of the run. Decide the intended
classification explicitly and pin it with a test.

**P1 — significant untested surface area, several structural (would stay
green if the implementation regressed):**
- `_should_recycle`'s memory-watermark and elapsed-time branches (the
  spec's genuinely novel mechanism) have zero tests -- only the max-tests
  branch is exercised.
- The daemon poll loop's "grant after contention clears" behavior (spec
  §3.3's central "re-evaluated each tick" claim) is untested -- a cached
  first-evaluation result would still pass every existing test.
- `TestRealPytestAitestEndToEndFallback` never asserts the pytest exit
  code -- deleting the line that produces a nonzero exit on
  failed+unevaluated tests would stay green.
- The granted (confined, non-fallback) path has zero DEFAULT-TIER e2e
  coverage -- gating the whole real-daemon/real-cgroup e2e behind
  `AIRA_AITEST_SLOW_E2E=1` for AIRA-35's sake also gated the pass/fail
  wiring and the CLI<->supervisor contract, which don't carry the OOM
  hazard. Split a default-tier variant excluding just `test_oom.py`.
- `validateWorkerAdmitArgs` has no direct unit tests (overflow-safe
  parsing, bounds, missing/mistyped fields all revertible with zero
  failures).
- No mixed-healthy-and-crashing multi-worker test (spec's named race:
  requeue-at-front without double-recording).
- `run_one` can never actually return `"error"` (the outcome collector's
  rank table only has passed/skipped/failed) despite three places
  documenting the contract -- either wire it up or delete the dead branch.

**P2s (bundle; several overlap AIRA-37, do together):**
- No client-side deadline on the acquire chain in three places
  (`acquire_worker`'s `readline()`, the placement-ack read, the Go CLI's
  connection) -- a wedged relay/daemon hangs the whole run with zero
  diagnostic (overlaps AIRA-37's CLI-deadline item; extend to cover the
  Python-side reads too).
- Daemon restart voids the in-memory worker ledger (no #74-style
  reconstruction here) -- still-running workers contribute nothing to
  `committed` post-restart, and `nextSeq` restarting at 1 collides with
  existing `.aira-worker-N` dirs.
- `evaluateWorkerAdmit` ignores reclaimable file cache (unlike slice-level
  `admit`), causing persistent spurious denials on fixture-heavy suites.
- Ledger keying is per-`(job_id, outer_scope)`, not per-scope -- nothing
  stops two `job_id`s against the same outer scope from each getting
  Σcaps up to ceiling.
- `CreateWorkerScope` leaks the cgroup dir on cap-write failure; no
  minimum on `estimated_bytes` allows a sub-page cap (overlaps AIRA-37).
- Unguarded env-var parsing in the worker discards a completed test's
  outcome on a malformed `AIRA_AITEST_WORKER_*` value (overlaps AIRA-37);
  relatedly `--aitest-workers=abc` is silently coerced to 1 rather than
  refused.
- Result-line nodeid isn't validated against `state["in_flight"]` -- a
  corrupt/spaceless line misattributes an outcome and silently orphans
  the real one.
- `WaitedMS` still never populated (overlaps AIRA-37); `signature` is
  spec-promised on the wire but never sent -- mark the deferral in the
  spec explicitly.
- fd hygiene: worker admit-process stderr never closed in the child;
  placement-failure path leaks the raw `dispatch_write` fd.
- `test_recycle_with_two_concurrent_workers_does_not_hang_on_retirement`
  still proves its property via `elapsed < 4.0` wall-clock -- flaky on a
  loaded host, porous against any hang under ~4s. Replace with a
  structural assertion.
- Spec §2/§3.3/§4's admission-model prose ("live measured occupancy... not
  a once-computed static split") predates the aggregate-committed-caps
  guard added by build-review -- amend to state the actual shipped model.
- Plan sections (Task 4, 13/15) still narrate pre-fix behavior with no
  drift note, unlike Task 16 which already carries one -- add banners or
  inline corrections for consistency.

Not urgent enough to block further Slice 1 use (the P0-tier findings from
this same round were fixed immediately) -- tracked here so the remainder
isn't silently dropped. relates AIRA-30, AIRA-37.
