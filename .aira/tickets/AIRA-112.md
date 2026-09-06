---
{"schema":1,"id":"AIRA-112","project":"aira","title":"Flake: TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration reports unverified scope integrity for a sub-millisecond job","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["confine","flake","testing"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-112","to":"AIRA-126"}]}
---
Reproduced on CLEAN origin/master (8f16769) with no local changes, while merging AIRA-106.

## Evidence

    cd <clean worktree at origin/master>
    aira confine -- go test ./internal/runner/ \
      -run TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration -count=20
    --- FAIL: TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration (0.31s)
        runner_test.go:849: unverified despite positive running observation:
          ... Status:exited ScopeIntegrity:unverified ...
    exit 1

It is intermittent: -count=5 on the AIRA-106 branch passed; -count=20 on clean master failed. It also failed once inside a full `go test ./...` run, which is how it was noticed.

## Why it is almost certainly the known sampling gap, not a new defect

The job under test is `/bin/sh -c printf ok` -- it starts and exits in well under the scope-membership sampler's window, so no positive in-scope observation is ever taken and ScopeIntegrity stays `unverified`. That is the sub-2ms escape recorded as an accepted coverage gap when the sampler was kept (AIRA-70's resolution: keep-sampler + in-scope dwell de-flake + document the gap). The de-flake evidently does not cover this particular test's job, which is about as short-lived as a job can be.

AIRA-34 (subtree-aware leader-migration check in monitorScopeMembership, merged cdf48d8) landed in this exact code path shortly before; whether it changed the timing here is worth a look, but the failure mode matches the pre-existing gap rather than a regression.

## Why it matters

`unverified` is the HONEST answer for a job the sampler could not observe -- the test asserting otherwise is what is wrong, not the production code. But as written it fails a full-suite run intermittently, so every merge gate on this repo can go red for a reason unrelated to the change under test. That trains people to re-run rather than read, which is exactly how a real failure gets waved through.

## Suggested direction (not decided)

Either give the test's job enough dwell for the sampler to take one observation, or assert the three-state honestly (`verified` OR `unverified`-with-a-reason) rather than requiring `verified`. Do not weaken the sampler to make the test pass.

## Handling in AIRA-106

Recorded, not worked around: AIRA-106's own full-suite run is reported as green EXCEPT this test, with the clean-master reproduction above as the evidence that it is not AIRA-106's. AIRA-106's own tests (internal/daemon, internal/install, cmd/aira, and the five real-cgroup slice-ceiling tests under AIRA_REAL_CGROUP=1) are green.

## Resolution

**This was NOT the accepted sub-2ms sampling coverage gap.** The ticket's stated
hypothesis — that a job too short-lived for the sampler never yields a positive
in-scope observation, so `unverified` is the honest answer — was checked first and
does not fit the observed failure. The failure message is `unverified DESPITE
positive running observation`: the leader *was* positively verified in
`cgroup.procs` at launch (the `running` ledger event was appended,
`scopeVerified == true`), and the verdict was then *downgraded* from `contained`
to `unverified` afterwards. A genuinely unobserved job takes the other branch of
`assertHonestExitScope` and passes. So the test was right and something in the
production classifier was wrong.

### Root cause: an ESRCH read of `/proc/<pid>/stat` was misread as an evidence gap

Reproduced on this branch with `-count=200` under `AIRA_REAL_CGROUP=1`, then
instrumented at every `summary.Gap = true` site in `monitorScopeMembership` and at
`classifyLaunchScopeIntegrity`'s inputs. The captured evidence, for both failing
`/bin/sh -c printf ok` launches in that run:

    monitor={LeaderMigrated:false HadDescendants:false Gap:true Escape:<nil>}
    teardown={Observed:true Empty:true DescendantKilled:false Gap:false}
    AIRA_DEBUG_PLIVE readerr err=&fs.PathError{Op:"read", Path:"/proc/327679/stat", Err:0x3} errno=no such process

`Err:0x3` is `ESRCH`. `processLive` (`internal/runner/runner_linux.go`) mapped a
failed `/proc/<pid>/stat` read to `processDead` only via
`errors.Is(err, os.ErrNotExist)`. Go maps `ENOENT` and `ENOTDIR` to
`os.ErrNotExist` — never `ESRCH`. But the kernel has *two* ways of saying the pid
is gone: `ENOENT` when the `/proc` entry is already unlinked at `open()`, and
`ESRCH` from the `read()` when the task is reaped between that open and the read.
The second is the ordinary case for a leader that exits between two membership
samples. `processLive` therefore returned `processUnknown`, the sampler's
ever-member loop recorded `summary.Gap = true` for a process it had positive
kernel proof was gone, and `classifyLaunchScopeIntegrity`'s
`Teardown.Observed && Monitor.Gap` branch downgraded a genuinely contained run to
`unverified`. Measured at roughly 0.5% of `printf ok` launches on this kernel
(2 of ~200 in one run, 3 of ~200 in another).

This was a false *negative* on containment attestation — AIRA discarding a
definitive kernel answer and reporting `unverified` when it could honestly report
`contained`. It also affected every other `processLive` consumer, including the
detached-run reconcile path's `processLive(record.SupervisorPID)`, where an ESRCH
read left a provably-exited supervisor `processUnknown`.

### Fix

One semantic correction plus its documentation, in `internal/runner/runner_linux.go`:

- New `procAbsenceProof(err)` helper: a `/proc/<pid>` read failure is proof of
  absence for `ENOENT` **or** `ESRCH`, and for nothing else. `processLive` uses it
  in place of the bare `errors.Is(err, os.ErrNotExist)`.
- The helper's doc comment records why (both errnos, which syscall produces which,
  and why Go's `ErrNotExist` misses one), and that only the *absence* side is
  widened: every caller acting on a positive claim (`witnessedEscape`, the
  leader-migration probe, `initialMigrated`) requires `processAlive`, so a wider
  DEAD side cannot fabricate an escape, a migration, or a containment claim.

The sampler itself is untouched — no widened dwell, no retried sample window, no
new machinery, no weakened attestation. Per the ticket's own instruction, the
sampler was not weakened to make the test pass; a definitive kernel answer that
was being thrown away is now used.

### Tests (TDD, all three verified failing against the old behaviour first)

In `internal/runner/scope_monitor_linux_test.go`:

- `TestAIRA112ReapedLeaderIsNotAResidualGap` — drives `monitorScopeMembership`
  through the exact reproduced sequence: a seeded first sample sees a live member,
  the first `Members()` poll flips the mocked stat reads to `ESRCH`, and the
  summary must carry no `Gap`, no `LeaderMigrated`, no `Escape`. Fails on the old
  code with `Gap:true` — the precise production symptom.
- `TestAIRA112UnreadableLeaderStatIsStillAGap` — the opposite direction, so the
  fix cannot be widened into "any read error means dead": `EACCES` (hidepid)
  carries no proof of absence and must still record a gap.
- `TestAIRA112ProcessLiveMapsAbsenceErrnosToDead` — pins `processLive`'s errno
  contract per case: `ENOENT`→dead, `ESRCH`→dead, `EACCES`→unknown, `EIO`→unknown.

Soak: `TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration` with
`-count=400` under `AIRA_REAL_CGROUP=1` (1200 real-cgroup launches), where the
pre-fix code failed within 200-300.

### Second, independent defect found in the same test — filed as AIRA-126

The `-count=400` soak surfaced a different failure in the same test's third
scenario (`/bin/sleep 0.04` against a 50ms timeout, which deliberately straddles
its own deadline):

    U_RUN_RECONCILE_REQUIRED: kill intent won before terminal evidence

Traced to source and confirmed structurally independent of this fix (none of
`processLive`'s call sites are on the kill/terminal-CAS path): when the timer and
the child's exit are simultaneously ready and the timer branch wins,
`killWithIntent` has already published a durable kill intent, `killScope` finds
`len(pids) == 0` and deliberately refuses to call an empty scope a completed kill,
and Launch's terminal CAS then returns a non-terminal record requiring reconcile
for a run that finished cleanly and on time. An isolated 800-iteration probe of
that scenario alone hit it at iteration 43 (~2%; ~0.25% per full-suite run).

Whether that should arbitrate to `exited` (honour the pending wait outcome) or to
`killed` (treat a provably-empty scope as a vacuously-completed kill, using the
already-computed but unused `killResult.Empty`) is a kill-arbitration design
decision, which CLAUDE.md puts in the two-loop class. It is **not** worked around
in production code here. AIRA-126 carries the full trace.

The test's third scenario now accepts that as an explicitly-evidenced third
outcome — `LaunchError.Code == "U_RUN_RECONCILE_REQUIRED"`, a record carrying an
unproven kill intent (`Present && !Completed`), both `E_RUN_TIMEOUT` and
`U_RUN_RECONCILE_REQUIRED` error codes, a non-terminal status, and zero terminal
records. Any other error, or a record missing any part of that signature, still
fails. The comment points at AIRA-126 and says to tighten it back when that lands.

### Residual coverage gaps, written down and accepted

1. **The real sub-2ms gap still exists and is still honest.** A leader that exits
   before the post-`Start()` `scope.Members()` read is never positively observed,
   `scopeVerified` stays false, no `running` event is appended, and the run reads
   `unverified`. That was observed twice in the same 200-iteration run and the
   test passes on it, because `assertHonestExitScope`'s `ScopeUnverified` branch
   requires exactly that: no positive observation. Unchanged by this ticket; it
   remains the accepted sampling gap already documented on
   `classifyLaunchScopeIntegrity` and `scopeMembershipSampleInterval`.
2. **`observeProcessCgroup`'s own died-between-checks window is still a `Gap`.**
   If a process is alive at the `processLive` guard and reaped a few instructions
   later inside `observeProcessCgroup`, that observation returns `Readable:false`
   and the three call sites (the leader-migration probe, the descendant loop, and
   `attestScopeTeardown`'s residual check) record a gap. That is the same
   *class* as this defect but a different, much narrower window; it did not fire
   once in ~1200 instrumented real launches, and its verdict is
   conservative-and-honest (`unverified`, never a false positive), so it is left
   alone rather than changed without a reproduced failure. Recorded here so it is
   not silent.
3. `processStartTick` still returns 0 on any read error, which the sampler treats
   as an unknown identity for a *member* pid. That is correct: for a descendant we
   never managed to stat, we genuinely cannot attest anything. The leader is
   substituted with its known identity before that check, so it never trips.

### Verification (exact exit codes, every heavy command under `aira confine --`)

The first review BLOCKed this PR as **verification-incomplete**, not for any code
defect: the earlier full-suite run was cut short (SIGTERM at 13/16 packages) and
no exit code was ever recorded, and the executed revert check and the flake soak
were still queued behind it. Every command below was then re-run to completion, one
at a time (serialised per CLAUDE.md), with each exit code read from `$?` rather
than inferred from output.

`origin/master` moved twice during that work, the second time landing AIRA-107's
real code changes in `internal/codes`, `internal/core` and `internal/store`. The
branch was rebased onto each (no conflicts, `internal/runner` untouched by either)
and the whole gate was re-run at the final base, **`origin/master` at `9abc100`**,
so nothing below is a result carried over from an earlier tree.

- `aira confine -- go build ./...` — exit **0**
- `aira confine -- go vet ./...` — exit **0**
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit **0**.
  Log: `~/tmp/aira-AIRA-112-fulltest-rebased.log`, terminated by an explicit
  `TEST_EXIT=0` line. All **15** packages `go list ./...` reports accounted for:
  14 `ok` (`cmd/aira` 58.9s, `app`, `codes`, `core` 27.1s, `daemon` 75.3s,
  `domain`, `gate`, `gitcontext`, `gitremote`, `install`, `pylib` 33.8s, `runner`
  68.1s, `store` 136.3s, `testdeadline`) plus `internal/cgrouptest`
  `[no test files]`. Zero FAIL, zero panic. (`internal/query` and `internal/interp`
  do not exist in this tree; they are named in the layering plan, not yet built.)
  The same command at the previous base `0cda3dc` also exited **0** with all 15
  packages accounted for — `~/tmp/aira-AIRA-112-fulltest-final.log`.
- `gofmt -l internal/ cmd/` — no output, exit **0**.
- **Executed revert check** — the tests were proved non-porous by running them
  against the old behaviour, not by reading them. Reverting the single call site
  to `errors.Is(err, os.ErrNotExist)` and running
  `aira confine -- go test ./internal/runner/ -run 'TestAIRA112' -count=1` gives
  exit **1**:
  - `TestAIRA112ReapedLeaderIsNotAResidualGap` — **FAIL** (the production symptom).
  - `TestAIRA112ProcessLiveMapsAbsenceErrnosToDead` — **FAIL**, and only in the
    `esrch-at-read` subtest: `processLive(read /proc/4242/stat: no such process)
    = 0, want 2`. `enoent-at-open`, `eacces` and `eio` still pass, so the test
    fails for exactly the reason the fix exists and no other.
  - `TestAIRA112UnreadableLeaderStatIsStillAGap` — **PASS** on the old code, as
    intended: it is the over-widening mutation guard (EACCES must stay a gap), so
    passing both sides is its correct role, not porosity.

  Restoring the call site returned the working tree to the committed state
  (`git status --porcelain` empty, `git diff` empty), and the same command then
  gives exit **0** with all three tests PASS.
- Flake soak:
  `AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner/ -run TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration -count=150`
  — exit **0** (`ok aira/internal/runner 89.056s`), 450 real-cgroup launches. Run
  at base `0cda3dc`; the later rebase onto `9abc100` changed no file under
  `internal/runner`, so it is the same tree under soak.
- Earlier targeted soak, pre-rebase: the same command at `-count=400` — exit **0**
  (`ok aira/internal/runner 301.608s`), 1200 real-cgroup launches. The same
  command on the pre-fix tree failed inside 200-300 iterations.

### Merge record

Merged via PR #70 as merge commit `315d500fe60c191c9c19961182977844214796ff`
(2026-09-06). Independent build-review re-ran build/vet/gofmt/full suite from a
clean worktree at the PR head (`ec6c04f`, based on `9abc100`): exit 0/0/0/0, 14
packages `ok` + `cgrouptest` no test files, zero FAIL. The kernel ESRCH-at-read
claim was reproduced with a standalone probe (open `/proc/<pid>/stat`, reap the
child, `read()` → `ESRCH`, `errors.Is(err, os.ErrNotExist) == false`). The
executed revert check was repeated independently: with the one call site put
back to `errors.Is(err, os.ErrNotExist)`, `ReapedLeaderIsNotAResidualGap` and
`ProcessLiveMapsAbsenceErrnosToDead/esrch-at-read` go red (exit 1) and the
restored tree is clean and green (exit 0). Accepted non-blocking notes: the
AIRA-126 test accommodation is tight (seven-part evidence signature matching
only `runner_linux.go` lines 786-842) and the two other scenarios still pin
both terminal edges; the D5 spec's pseudo-code note "read error →
processUnknown" is now imprecise for ENOENT/ESRCH (a historical spec, not
updated here).
