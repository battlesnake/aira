---
{"schema":1,"id":"AIRA-20","project":"aira","title":"Harden wall-clock-tight tests to re-enable a -race CI job","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["ci","flaky","testing"],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-33","to":"AIRA-20"}]}
---
The first GitHub Actions CI run (2026-08-30) showed build+test green but the `-race` job failed — NOT on any data race (0 `WARNING: DATA RACE`), but on wall-clock latency assertions that don't survive -race's slowdown on a shared CI runner:
- internal/daemon/watch_test.go:105 TestWatchReturnsConcurrentEventWithinPollInterval — `event latency=124ms poll=30ms` (asserts an event arrives within the 30ms poll interval; under -race it took 124ms).
- TestWatchShutdownTerminalDrainAndOverflowBoundaries/concurrent_event — same watch poll-interval class.
- internal/daemon/admission_linux_test.go:871 TestAdmissionT11KillAndReconcileDoNotTakeAdmissionLock — `Kill/Reconcile blocked on the admission lock` (a latency/"not blocked within" assertion inflated by -race).

These pass `make race` on a fast box + `make test` (no -race) everywhere; they only fail full-suite `-race` on a slow runner. The repo gate is `make ci` (no -race), so CI now runs build+vet+gofmt+test only (the `race` job was removed — see .github/workflows/ci.yml). Fix per the AIRA no-timing-flakes discipline: replace the fixed wall-clock thresholds with condition-based waiting / generous env-scalable deadlines (or gate the wall-clock assertion behind a non-race fast-path), so a `-race` CI job can be re-added and pass reliably on shared runners. Then restore the race job in ci.yml.

ANOTHER INSTANCE (2026-09-01, during the AIRA-27 confined gate): `internal/runner/governor_slot_test.go:453 TestGovernorSlotReconnectsWithSameUUID` — "relay did not reconnect" under the full parallel `-race` runner+cmd/aira suite. Passed 6/6 in isolation (both `-race` and plain), so it is the same wall-clock-tight reconnect-deadline class, not a data race and not caused by the AIRA-27 change (which only touches confine oom_score_adj). Add this test to the hardening list.

ANOTHER INSTANCE (2026-09-04, during the AIRA-68 build gate, WITHOUT -race): two of six full-suite `go test ./... -count=1` runs failed, each on a *different* wall-clock-tight `internal/runner` test, never the same one twice:
- `internal/runner/governor_slot_test.go:383 TestGovernorSlotReconnectsWithSameUUID` — failed at **2.01s** against a hard `time.After(2 * time.Second)` deadline (the test has four such deadlines, none synchronised to real work). Already listed above; this is a second sighting, and notably it now reproduces **without** -race, purely under full-suite CPU contention on a loaded shared box.
- `internal/runner/runner_test.go:833 TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration` — NEW to this list. Real-cgroup timeout/exit arbitration.

Both pass 10/10 in isolation. Not caused by AIRA-68, whose entire `internal/runner` diff is 45 added lines of struct fields + doc comments on `ConfineSliceReserve` (no function, control flow or behaviour; neither test reads that type). Three baseline passes at `d878d9a` did not reproduce either, so "reproduced on an unmodified baseline" is `unevaluated` — they are too infrequent for three passes to settle. Add `TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration` to the hardening list, and note that the wall-clock deadlines are now tight enough to flake *without* -race, which raises this above a -race-only concern.

ANOTHER TWO INSTANCES (2026-09-04, found by the independent AIRA-68 build-review verify agent, WITHOUT -race): the verify agent's own 3 consecutive `go test ./internal/runner/ -count=1` runs failed on **2 of 3**, on `TestM20LauncherDefersACKAndBoundsReadiness` and `TestGovernorSlotReconnectDoesNotBlockFailOpenOutput` — **neither of which the builder's own 6 runs had named**. So the package now has at least **4 distinct wall-clock-tight tests** flaking without `-race`, not 2, and each of the two build/verify agents independently hit a different pair by chance. This is stronger evidence the whole package's deadline discipline needs the hardening pass, not just the individual named tests — a fix that only hardens the 2-4 tests currently on this list risks leaving others of the same class undiscovered.

A FIFTH DISTINCT TEST (2026-09-04, found by the AIRA-80/81/60/86 captured-subject build): `TestM20DetachedRunKillWaitsForPreScopeSupervisorTerminal` failed in the **pre-push hook's `make test`**, and it failed in a *new sub-shape* — not a missed deadline but a teardown race surfacing as a `t.TempDir()` cleanup error:

    testing.go:1369: TempDir RemoveAll cleanup: unlinkat
    /tmp/TestM20DetachedRunKillWaitsForPreScopeSupervisorTerminal.../001/aira/runs: directory not empty

i.e. a descendant was still writing under the run directory after the test returned. `TestM20LauncherDefersACKAndBoundsReadiness` also failed once in the same session's first whole-suite run. Both packages passed on retry, `./internal/runner/` passed 3/3 in isolation immediately afterwards, and that change touches **zero** files under `internal/runner` (`git diff --name-only origin/master...HEAD -- internal/runner` is empty), so neither failure is attributable to it.

Two things this adds to the case above:

1. The count of distinct flaky tests in this package is now **at least 5**, and every agent that runs the suite a few times finds a different one. The per-test hardening list is not converging; the package needs the deadline/teardown discipline pass as a whole.
2. **This now blocks merges, not just CI.** The `make test` pre-push hook fails on it, so an unrelated PR cannot be pushed without either retrying until the flake misses or bypassing the hook. That is a direct cost on every other milestone, which raises the priority argument above "re-enable a -race job".

Not fixed here — recorded as evidence.

## Hardening pass landed (2026-09-05, this repo's last backlog-remediation item)

The package-wide deadline pass this ticket asks for is **done**, across
`internal/runner`, `internal/daemon` and `internal/pylib`. `-race` CI restoration
is **not** done and is not deferred by oversight: it stays blocked on AIRA-33 (which
deletes `TestRealPytestRAMForkDoesNotPinHelperStdin` and the xdist stack), which is
itself blocked on AIRA-91. Restoring `-race` before that would declare clean a suite
with a known load-flaky test still inside it.

What landed:

- `internal/testdeadline`, a test-only package that separates the three cases a
  wall-clock wait can be. A **liveness backstop** ("did this ever happen?") is not a
  property under test, so `Wait`/`After` floor it at `MinBackstop` and scale it; a
  **latency assertion** ("was this prompt?") scales through `Exceeded` without a
  floor; a **negative wait** ("did this correctly NOT happen?") is left alone,
  because contention delays the subject and the timer alike and it cannot produce a
  false failure. `AIRA_TEST_DEADLINE_SCALE` multiplies all of it, with a built-in ×4
  under `-race`.
- Every positive-branch `time.After`, polling deadline, test-side `context.WithTimeout`
  and socket deadline in the three packages routed through it. `governor_slot_test.go`
  was deliberately skipped: AIRA-33 deletes it.
- The named flaky tests fixed at their real cause rather than by widening alone,
  because several of their bounds were **vacuous** once widened:
  `TestAdmissionT11KillAndReconcileDoNotTakeAdmissionLock` (a 1s `admissionMaxWait`
  meant a regression that took the lock would return anyway — now an hour, so
  "took the lock" means "never returns"); `TestWatchReturnsConcurrentEventWithinPollInterval`
  and `TestWatchShutdownTerminalDrainAndOverflowBoundaries/concurrent_event` (a fixed
  sleep stood in for "the watch has scanned", now a real hook, so the ordering is
  deterministic instead of guessed); `TestWatchPeerCloseCancelsAndLongPollDoesNotMonopolizeDB`
  (`wait_ms` raised to the watch cap plus a still-in-flight guard, or both assertions
  went vacuous); `TestAdmitPeerCloseFreesNextWithoutWaitingForPoll` (it compared
  against `defaultAdmitPollInterval`, a constant this server never uses — it parks the
  poll at an hour — so the 250ms bound could only ever fire as a false fail);
  `TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration` (the run's own 1s
  timeout is now scaled, which is what failed at 2.01s);
  `TestM20DetachedRunKillWaitsForPreScopeSupervisorTerminal` (the injected
  terminalizer is now joined, which is the `TempDir RemoveAll ... directory not empty`
  shape).
- `TestRealPytestRAMForkDoesNotPinHelperStdin` is **not** fixed here. Its cause is
  AIRA-65's finding — a hardcoded `process.wait(timeout=1.0)` in
  `aira_xdist_governor/__init__.py` — which is production code inside AIRA-33's
  deletion scope, so hardening it would be work thrown away. Its test-side waits were
  scaled with everything else.

### Two more flaky tests, found by this branch's own verification

The ticket's central claim — that every agent who runs the suite a few times finds a
different one, so the per-test list never converges — held during the fix itself. Two
tests not on the list above failed during verification and are fixed here:

- **`TestRealPytestRAMForkDoesNotPinHelperStdin`** (a 6th sighting, and the first
  attributable to the ordering assertion rather than to AIRA-65's SIGTERM budget):
  release landed 431ms AFTER child-done. The forked child's fixed 1.0s sleep IS the
  discriminator's whole margin, so it now scales; a pinning implementation still
  fails. AIRA-65's `_stop_reservation` budget is untouched and still owns the other
  failure shape (no marker written at all).
- **`TestScopeMembershipEventsDeliversModifyAndReleasesFD`** (a 7th distinct test):
  `inotify watch not established: 9 open fds, baseline 9`. Not a deadline —
  `openFDCount` counted every entry in `/proc/self/fd`, a process-global number, so
  any leftover goroutine closing a socket cancelled the watcher's own +1. Now counts
  inotify descriptors only. Mutation-checked live.

Whole-suite evidence at the branch tip: 6 consecutive green `go test ./... -count=1`
runs after the last fix, plus `make ci` green, plus `-race` green on `internal/daemon`
and `internal/runner` (not wired into CI — see the block above). Every failure observed
during this work was reproduced, attributed and fixed rather than retried away.

### One observed flake left UNFIXED and unattributed, in a package outside this pass

GitHub CI on the AIRA-20 branch failed once on `cmd/aira`, a package this pass does
not touch (its diff against master is empty):

    --- FAIL: TestCLIRunRealCgroupOrClearSkip (0.00s)
        main_test.go:523: fork/exec /usr/bin/git: bad file descriptor

`main_test.go:523` is a plain `exec.Command("git", "init", dir).Run()` with no
ExtraFiles and no timing, so EBADF out of `forkAndExecInChild` points at the same
process-global descriptor race as `TestScopeMembershipEventsDeliversModifyAndReleasesFD`
above — a descriptor closing under another goroutine — rather than at anything in this
change. It passed on rerun and did not reproduce in four local runs. Recorded rather
than retried away, but NOT fixed: `cmd/aira` was outside this pass's scope and one
sighting is not enough to locate the closer.

Note also that `internal/daemon/worker_admit_real_cgroup_linux_test.go:44` still uses
the `len(os.ReadDir("/proc/self/fd"))` process-global measurement this pass replaced in
`internal/runner`. It is far more robust as written (a directional `growth > 5` against
a +30 defect signal, with GC disabled), so it was left alone — listed here so the class
has both of its known instances written down.
