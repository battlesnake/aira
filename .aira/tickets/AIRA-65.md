---
{"schema":1,"id":"AIRA-65","project":"aira","title":"TestRealPytestRAMForkDoesNotPinHelperStdin is load-flaky: governor gives the reserve helper a 1.0s budget before SIGTERM","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["governor","pylib","test-flake"],"hold":false,"relations":[]}
---
## Symptom

`TestRealPytestRAMForkDoesNotPinHelperStdin` (`internal/pylib/pytest_integration_test.go:606`) failed once during a full `go test ./...` on master `9a65d47`, on a box running 15+ concurrent agent sessions (load 50-80). It did **not** reproduce in 60 subsequent executions across two commits, including the same whole-suite condition.

## Attribution (mechanical, not inferred)

Not caused by AIRA-58/59 (`9a65d47`):
- `git diff --name-only 4fd0de2 9a65d47 -- internal/pylib` returns **zero files** — the test and the Python plugin are byte-identical across that merge.
- The test overrides `AIRA_CONFINE_RESERVE_CMD` with a fake Python helper, so the real `aira confine-reserve` never runs and the new `validateConfineReserveRequest` ceiling / `MinPinnedScopeCap` refusal are on an unreachable path.
- Even if reached, neither could fire: the plugin sends `--max-wait 300s` against a 24h ceiling, and `--bytes 8M` against a 1MiB floor.

## Mechanism (measured)

The binding constraint is not the ordering assertion but `_stop_reservation` (`internal/pylib/aira_xdist_governor/__init__.py:311-334`): after closing the helper's stdin it does `process.wait(timeout=1.0)` and then `process.terminate()`.

So the helper has a hard **1.0-second scheduling budget** to wake from a blocking `sys.stdin.buffer.read()` and complete one `open()` + `write()` of its `released` marker. Under `aira confine`'s nice/ionice/cpu-weight deprioritisation on a heavily loaded box, a single low-priority Python process stalling past 1.0s is entirely plausible — it is then SIGTERM'd *before* writing the marker, and the test fails at `timed out waiting for .../released`.

Measured margins on the healthy path: `released` is written 3-6ms after the fork (ordering margin ~997ms), and `child-done` lands 834-940ms against a 2000ms deadline. So the ordering assertion is nearly unreachable; the 1.0s terminate budget is the real edge.

## Suggested direction

Either raise `_stop_reservation`'s wait, or have the helper write `released` before it can be exposed to SIGTERM. Note this is a **production** timeout, not only a test one: the same 1.0s budget applies to the real reserve helper, so a heavily loaded box may be terminating reservation helpers before they release cleanly. Worth checking whether that has any live consequence beyond this test.

Found during AIRA-58/AIRA-59 merge verification.

## Note (2026-09-05): a cross-reference in AIRA-103 points at PR #65, not at this ticket

Status-transition note only; this ticket's scope, status and content are otherwise
unchanged, and it is not reopened or rescoped.

AIRA-103's filed text says "AIRA-65 watchdog: this ticket's system-memory signal
must reuse the watchdog's existing `readMemAvailable`/`parseMemAvailable`". That
reference is to **PR #65 (the watchdog MemAvailable fix)**, not to this ticket,
which is a `TestRealPytestRAMForkDoesNotPinHelperStdin` load-flake in the pytest
RAM governor and has nothing to do with the watchdog. Recorded here so the
mis-reference does not later read as an unaddressed dependency.

The shared-primitive reuse AIRA-103 actually performs is recorded against the real
watchdog ticket, **AIRA-16**, and in
`docs/superpowers/specs/2026-08-23-aira-memory-watchdog-design.md` §10.

## Resolution (2026-09-05, backlog-completion triage)

Verified against current master (3251bed). The ticket has two halves; neither warrants building.

(1) The "production consequence" half was never live, and the ticket's own "worth checking" question is now answered from source. The real helper `runConfineReserveCommand` (cmd/aira/main.go:1275-1299) traps SIGINT/SIGTERM via signal.NotifyContext and treats signalCtx.Done() identically to stdin-EOF: both fall through to `defer reservation.Close()`, which is `WorkerAdmitLease.Close()` = `conn.Close()` (internal/runner/worker_admit_client_linux.go:46-50). The daemon holds the pinned reserve on that connection: `admitConnection` (internal/daemon/admit.go:1178-1194) runs a `conn.Read(one[:])` peer-watcher that cancels on ANY return and `defer release()` -> `releaseAdmitWaiter`. Even the governor's SIGKILL fallback (`process.kill()`, __init__.py:334) discharges via socket teardown, pinned by TestAdmitLedgerReleasesWhenAClientDiesWithoutClosingCleanly (admit_release_e2e_test.go:189+, reclaim on non-EOF read error). Both mechanisms predate the ticket: SIGTERM handling since 9117db6 (2026-08-26, the commit that added the RAM governor), peer watcher since b660b0c (2026-08-17); ticket filed cd5abd4 (2026-09-04). There is no `released` marker in production; it is a test-only artifact of the fake Python helper. The 1.0s budget therefore decides only HOW the helper exits, never WHETHER the daemon discharges the reserve.

(2) The test-flake half is still present and unchanged: `_stop_reservation`'s hardcoded `process.wait(timeout=1.0)` is at internal/pylib/aira_xdist_governor/__init__.py:328, and TestRealPytestRAMForkDoesNotPinHelperStdin (pytest_integration_test.go:633) still names AIRA-65 as owning that edge. But its failure shape is a false-FAIL only (marker never written -> test fails), never a false-pass, so it does not touch CLAUDE.md's honesty bar ("never a fake pass"). Three reviewed, landed artifacts already record the decision NOT to harden it: AIRA-20's merged PR (0824937/6694a97: "production code inside AIRA-33's deletion scope, so hardening it would be work thrown away"); .github/workflows/ci.yml:47-66, which gates -race restoration on AIRA-33 landing or an explicit quarantine, not on AIRA-65; and both docs/superpowers/plans/2026-09-04-simplification-programme-plan.md ("AIRA-65: closes -- superseded ... Both deleted") and docs/superpowers/reviews/2026-09-04-fable-backlog-simplification-sweep.md:169 list it as close-superseded-by-AIRA-33. Under the owner's HARD architectural-simplicity rule (document the gap over new machinery), building into a file the project has already agreed to delete is the textbook case of cost exceeding benefit.

Why not close-superseded: AIRA-33 has NOT landed (status planned; aira_xdist_governor/__init__.py, runner/governor_slot.go and daemon/governor.go are all still in tree). Its real precondition was AIRA-91 Part A + fastest-ee re-verified + the FASTEST_NO_AITEST=1 pin removed. Part A is DONE and deployed (2026-09-05, PR #35/#36 per AIRA-91:687-717), but I could not establish from this repo whether the fastest-ee pin has been removed (it lives in another repo; AIRA-32/33 still cite it as pending). Supersession is decided but not executed; claiming it now would be premature.

Why not build-small: the fix is trivially small (scale the wait, or have the fake helper write its marker from a SIGTERM handler), but its only consumer (-race CI restoration) is explicitly gated on AIRA-33, and the code is slated for deletion.

Why not already-done: the 1.0s is still there and the test is still load-flaky by its own comment.

Residual, stated honestly: if AIRA-33 is ABANDONED rather than landed, this should be reopened as build-small (scale `_stop_reservation`'s wait or quarantine the one test). The flake shape this ticket owns was seen once in 60+ runs; AIRA-20's 6th sighting was the ordering shape, which it fixed. Tiny doc follow-up on closure, not a build: ci.yml:62-66 and AIRA-33's knock-on list cite AIRA-65 by number; they should point at AIRA-33 alone once this closes. The AIRA-103 mis-citation (meant PR #65) is already recorded in the ticket body and needs nothing.


*Disposition: Closed — not needed, reached via a source-verified triage pass (Fable model) as part of the backlog-completion push, independently spot-checked by the coordinating session before closing.*
