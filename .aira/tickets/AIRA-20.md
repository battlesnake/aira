---
{"schema":1,"id":"AIRA-20","project":"aira","title":"Harden wall-clock-tight tests to re-enable a -race CI job","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["ci","flaky","testing"],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-33","to":"AIRA-20"}]}
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

## Resolution (2026-09-06) — DONE, merged

Re-checked the "Hardening pass landed" precondition against current source before
building, per the brief's own instruction, rather than trusting it as written:

- `governor_slot_test.go` — **confirmed gone.** AIRA-33 landed (`f1f699a`, and
  further ticket-only commits since) and deleted the file whole along with
  `TestRealPytestRAMForkDoesNotPinHelperStdin`, the one test the earlier pass
  could not honestly harden (its cause was production code — a hardcoded
  `process.wait(timeout=1.0)` in the xdist governor — inside AIRA-33's own
  deletion scope). Item 1 of the brief was moot, exactly as its own note
  anticipated.
- `internal/daemon/cpuslots_gate_test.go` and `internal/daemon/sliceceiling_test.go`
  — **confirmed still present, still un-hardened** (`git log` showed neither file
  touched by AIRA-20's original pass, PR #40). Both survive AIRA-33.
- Grepped the whole of `internal/runner`, `internal/daemon`, `internal/pylib` for
  any OTHER `_test.go` file using `time.After` without importing `testdeadline`,
  in case something landed since the brief was written. Found two:
  `internal/runner/admission_exclusive_linux_test.go` (a single 1500ms
  **negative**-wait assertion — correctly left alone) and
  `internal/daemon/confine_kill_log_test.go` (a 1ms **poll ticker** inside an
  unbounded loop, not an assertion boundary — nothing to scale). Neither needed
  a change; no third file was missed.

### What was built

- `cpuslots_gate_test.go`: 4 liveness-backstop `time.After(5*time.Second)` sites
  (`TestCPUGateSpeculativeRequestNeverBlocksOnTheGate`,
  `TestCPUGateSpeculativeRequestNeverBlocksOnTheOuterScopeLock`,
  `TestNonSpeculativeRequestStillWaitsForTheOuterScopeLock`,
  `TestCPUSlotsCachedReadDoesNotBlockBehindAnInProgressScan`) now route through
  `testdeadline.After`. The file's one 150ms **negative**-wait assertion (`...
  must WAIT for the lock, not report ...`) is untouched, per `testdeadline`'s own
  contract: contention delays the subject and the timer alike, so scaling a
  negative wait only slows the suite.
- `sliceceiling_test.go`: 2 liveness-backstop `time.After(5*time.Second)` sites
  (`TestSliceCeilingThrottleReachesCapacityOnly`,
  `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`, both "granted after the
  ceiling recovered") now route through `testdeadline.After`. Its two 150ms
  negative-wait assertions, and one unrelated 50ms `time.Sleep` that just drives
  two goroutines racing under `-race` (not a wait boundary), are untouched.
- `.github/workflows/ci.yml`: added a third job, `race`, running `make race`
  (`go test ./... -race -count=1 -timeout 40m`), alongside the existing `build`
  and `test` jobs.

### Verification (the brief's explicit "don't just remove a skip comment" bar)

Local, before pushing: `go build ./...`, `go vet ./...`, `make fmt-check` clean;
full `go test ./... -count=1` → **exit 0**, every package `ok`; targeted
`go test ./internal/daemon/... -race` and `./internal/runner/... -race` → both
green.

One local-only finding surfaced and is recorded, not fixed, because it is
outside this ticket's file scope and does not reproduce in the environment that
matters: `internal/daemon/sliceceiling_real_cgroup_linux_test.go`'s
`TestSliceCeilingRealCgroupSignalTracksRealAccounting` (added by AIRA-103,
after AIRA-20's original pass) fails under `-race` **on this box specifically**,
because this box has real cgroup-v2 memory delegation and the race-instrumented
helper subprocess's extra footprint pushes its two `touch(600MiB)` anon holds
past headroom under the fixture's 2GiB cap. Reproduced identically on
unmodified `origin/master` — pre-existing, not introduced here.

Rather than trust that read of "won't reproduce on the real runner," it was
checked directly: pushed the branch, opened PR #50, and let the real
`ubuntu-latest` GitHub-hosted runner run the new `race` job for real. **It
passed in 5m41s**, every package `ok` including `internal/daemon` (74.9s) —
confirming the generic runner has no delegated memory controller to hand that
test (matching the `test` job's already-documented behaviour for the whole
`SkipOrFailRealCgroup` family) and the local failure is a delegation artifact of
this box, not something CI will ever see. `test` (1m23s) and `build + vet +
gofmt` (25s) also passed. This is the actual authority the brief asked for, not
an inference from local reasoning alone.

Independent review: self-review, then one independent code-reading pass via
Codex-Sol (`gpt-5.6-sol`, read-only sandbox) against the full diff and the
`testdeadline` package's own liveness/latency/negative-wait contract — verdict
**APPROVE**, no findings, confirmed every changed site's classification and the
CI yaml addition.

### Design questions from the brief, resolved

- **Item 1 (`governor_slot_test.go`)**: moot — AIRA-33 deleted the file before
  this ticket was picked up. Re-scoped to `internal/daemon/cpuslots_gate_test.go`
  and `sliceceiling_test.go` exactly as the brief's own fallback instructed, plus
  a repo-wide grep to confirm no third file needed the same treatment.
- **Item 2 (`cpuslots_gate_test.go` / `sliceceiling_test.go`)**: done as specified;
  positive multi-second waits scaled, ~150ms negative waits left alone.
- **Item 3 (re-enable `-race` CI)**: done, and verified against a real CI run
  rather than by removing a skip comment on faith — see above.

### PR and merge

- PR: https://github.com/battlesnake/aira/pull/50
- Merge commit: `85965a04cbc829f94dcfe2ad1cfb0dc0da72e208` (merged into
  `master`, standard merge commit, branch `aira20-wall-clock-race-ci`)
- Build commit on the branch: `2851e6b2a2204da1d8387f99c14d9250830c0f93`
- Post-merge CI on `master` at the merge commit re-verified green (all three
  jobs) before this resolution was written.
- Exact exit codes: `go build ./...` → 0; `go vet ./...` → 0; `make fmt-check`
  → 0 (no output); `go test ./... -count=1` → 0 (full suite, all packages
  `ok`); `go test ./internal/daemon/... -race` → 0; `go test
  ./internal/runner/... -race` → 0; GitHub Actions `race`/`test`/`build + vet +
  gofmt` jobs on PR #50 and on the post-merge `master` run → all `success`.
