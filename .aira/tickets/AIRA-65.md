---
{"schema":1,"id":"AIRA-65","project":"aira","title":"TestRealPytestRAMForkDoesNotPinHelperStdin is load-flaky: governor gives the reserve helper a 1.0s budget before SIGTERM","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["governor","pylib","test-flake"],"hold":false,"relations":[]}
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
