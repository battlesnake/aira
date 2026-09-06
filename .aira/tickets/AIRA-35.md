---
{"schema":1,"id":"AIRA-35","project":"aira","title":"aitest worker memory.high/memory.max split converges too slowly under kernel throttle (WSL2)","status":"in-review","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["admission","aitest","cgroup","oom"],"hold":false,"relations":[]}
---
Found during AIRA-30 (aitest Slice 1) Task 17's real end-to-end verification,
not by design/review inspection: a worker deliberately blowing its memory
budget did NOT converge to a kernel OOM-kill in practical time on this
host (WSL2). Real-world reproduction: a 32MiB-capped worker scope
(memory.max), deliberately over-allocating, spent minutes stuck in the
kernel's `mem_cgroup_handle_over_high` reclaim-throttle path (confirmed via
`/proc/<pid>/wchan`) rather than promptly hitting memory.max and being
killed. The process was observed to enter an unkillable D-state (SIGKILL
cannot wake a process blocked in that specific kernel path) during the
investigation.

Root cause (current understanding): `internal/daemon/worker_admit.go`'s
`memoryHigh := req.estimatedBytes * 4/5` leaves an 80%/100% gap between the
soft throttle (memory.high, triggers kernel reclaim pressure) and the hard
cap (memory.max, triggers the oom.group kill). On this host/kernel, that
gap gives the reclaim-throttle path enough room to oscillate near (but
under) the hard cap for a very long time instead of converging quickly.

Important: the underlying containment invariant is NOT broken. `dmesg`
confirmed the correct worker scope's `oom.group` eventually fired
("Tasks in .../.aira-worker-2 are going to be killed due to
memory.oom.group set"), several minutes after the test started. This is a
convergence-SPEED problem, not a correctness/containment failure -- but a
worker stuck throttling for minutes (with a real risk of an unkillable
D-state) is a real production concern for exactly the goals aitest exists
for (fast, reliable containment; not a machine-disrupting hang), and
directly undermines "cooperative multitasking... avoid dead memory held
for long durations" from the aitest design spec.

Immediate mitigation shipped alongside this ticket (AIRA-30, Task 17):
`internal/pylib/pytest_aitest_e2e_test.go`'s
`TestRealPytestAitestEndToEndRealDaemonAndCgroup` is gated opt-in-only
(`AIRA_AITEST_SLOW_E2E=1`, skipped by default) with a hard 4-minute
`exec.CommandContext` timeout, so it can no longer hang a normal
`go test ./...` run or risk leaving an unkillable process on this shared
machine. That is a safety backstop for the TEST, not a fix for the
underlying throttle-convergence issue.

Candidate real fixes (needs its own plan-review, not a unilateral call --
this is exactly the kind of correctness-critical, cross-cutting tuning
change CLAUDE.md's two-loop rule is for):
- Narrow the memory.high/memory.max gap for aitest worker scopes
  specifically (e.g. 95% instead of 80%), reducing the throttle window.
- Add an explicit escalation path (e.g. daemon-side watchdog that
  force-kills a worker stuck over memory.high for too long, rather than
  relying solely on the kernel's own throttle-then-OOM progression).
- Investigate whether this is WSL2-specific (this dev host) or a general
  cgroup v2 characteristic that would also affect real Linux deployment
  targets -- changes the urgency/approach.

relates AIRA-30.

## Resolution

### What was measured, before deciding

The ticket offered three candidate fixes and asked for a plan-review rather
than a unilateral call. A probe (`~/tmp/aira35-probe/main.go`, committed as
tests -- see below) was built and run under `aira confine` on this host before
any code was touched: it creates real cgroup-v2 scopes, launches a Python
allocator into each via `clone3(CLONE_INTO_CGROUP)`, and times launch to that
scope's own `memory.events` `oom_group_kill`. Host: WSL2 6.18.33.2, 80 GiB RAM,
20 GiB swap active, inside `aira.slice` (`MemoryMax=64G`, `MemorySwapMax=8G`).

**Finding A -- the ticket understated the defect. Production worker scopes did
not contain a runaway AT ALL.** No production path wrote `memory.swap.max` (a
repo-wide grep returned nine hits, every one in a `_test.go`), and cgroup-v2's
`memory.max` bounds memory, not memory+swap. A 512 MiB allocation inside a
32 MiB `memory.max` was **never OOM-killed**: it was reclaimed into swap and
exited **0**, with ~520 MiB written to the swap device -- reproduced at 32/256
MiB caps and for a slow (8 MiB/100 ms) leaker as well as a fast one. The design
spec's own §7 invariant held only because the e2e HARNESS wrote
`memory.swap.max=0` on an ancestor: the test was proving the harness.

**Finding B -- `memory.high` was the stall, and narrowing it only moves it.**
Time to `oom_group_kill` with swap capped:

| `memory.max` | `high`=80% | `high`=95% | no `memory.high` |
|---|---|---|---|
| 32 MiB | **not converged in 420 s** (5475 `high` events, **0** `max` events) | 0.73/1.37/1.71 s | **0.029/0.031/0.033 s** |
| 64 MiB | -- | 0.82/2.86 s | **0.061 s** |
| 128 MiB | -- | 4.52/5.18 s | **0.080/0.101 s** |
| 256 MiB | not converged in 45 s | 11.50/13.28 s | **0.141/0.182 s** |
| **512 MiB (the shipped cap)** | -- | **16.42/18.42 s** | **0.361/0.481 s** |

The 95% delay tracks the ABSOLUTE width of the throttle window, so it grows
with the cap; the fraction is not the variable. 512 MiB is the shipped default
(`internal/pylib/aitest/__init__.py`'s `_resolve_estimated_bytes`; per-suite
history sizing is still deferred).

**Finding C -- not WSL2-specific.** Both are generic cgroup-v2/mm behaviour;
WSL2 only contributes swap-on-by-default.

### What was built

- `runner.CreateWorkerScope` no longer writes `memory.high`, writes
  `memory.swap.max=0` (AFTER `memory.max` succeeds, so an undelegated
  controller's ENOENT cannot be misread as "no swap support"), and returns a
  swap disposition. Three honest values: `enforced` (written and verified),
  `not-applicable` (no `memory.swap.max` AND `/proc/swaps` proved absent inside
  a demonstrably-mounted `/proc`), `unavailable` (anything else -- the grant
  proceeds, refusing would stall every aitest run on such a host). Any other
  error fails closed and removes the scope.
- `memory_high` removed from the worker-admit wire end to end (daemon response,
  runner client + lease, CLI grant fields, rendered line, `supervisor.py`'s
  `_OUTCOME_GRANT_FIELDS`). `swap_cap` replaces it; `supervisor.py`'s
  `_note_swap_cap_state` says once, on the run's own output, when it is
  `unavailable`. **`ProtocolVersion` 7 -> 8.**
- `worker.py`'s recycle watermark reads `memory.max` at 64%
  (`AIRA_AITEST_WORKER_MEMORY_WATERMARK_PCT`), reproducing the old
  80%-of-`memory.high` trigger point to within a page.
- `supervisor.py._describe_worker_death` reads the worker scope's
  `memory.events` BEFORE the scope is removed and, on `oom_group_kill > 0`,
  names the cap and `AIRA_AITEST_ESTIMATED_BYTES`. Required by the change: a
  worker over 512 MiB used to swap and PASS, and now dies and reports
  unevaluated.
- Spec `2026-09-01-aitest-design.md` amended (§3.3, §3.4, §3.7, §5, §6, §7),
  including that history-based sizing is still deferred and a flat 512 MiB
  ships. `.aira/tickets/AIRA-32.md` amended for the env-var rename.
- Plan: `docs/superpowers/specs/2026-09-06-aira35-worker-oom-convergence-plan.md`.

### Result

Convergence **31.7 ms** at 32 MiB and **238 ms** at the 512 MiB production cap.
The opt-in-gated (`AIRA_AITEST_SLOW_E2E=1`) 4-minute e2e is **un-gated** and
runs in **1.4 s**, with its ancestor swap cap deleted so it exercises
production. New un-gated `TestWorkerScopeOOMGroupKillConvergesPromptly` (a
{32 MiB, 512 MiB} table, 5 s bound capped at 10 s after `testdeadline` scaling)
plus `TestUncappedSwapLetsAWorkerEscapeItsMemoryMax` as the platform-fact
negative control.

### Mutation testing (the porousness check)

- Drop the `memory.swap.max` write -> convergence test, scope unit test AND the
  e2e all fail. The e2e failing is the proof that deleting the harness's
  ancestor write de-porosed it.
- Restore `memory.high` at 80% -> convergence test and the `memory.high`-unset
  assertion fail. It also reproduced the ticket's hazard live: PID in state
  `DN`, wchan `__mem_cgroup_handle_over_high`, 640 s, 7107 `high` events, **0**
  `max` events -- and exposed a real hang path in the newly un-gated test
  (600 s, Go's package timeout, not the 60 s context), now bounded by
  `WaitDelay` to a re-measured 76 s.
- Hardcode `swap_cap=enforced` in the daemon -> found GREEN by the build
  reviewer. Fixed (see below) and re-run: now killed in both packages.

### Exit codes (final, post-rebase onto origin/master)

| command | exit |
|---|---|
| `aira confine -- go build ./...` | **0** |
| `aira confine -- go vet ./...` | **0** |
| `aira confine -- make fmt-check` | **0** |
| `aira confine -- go test ./...` (full suite) | **0** |
| `aira confine -- python3 -m pytest internal/pylib/aitest/ -q` | **0** (167 passed) |
| `aira confine -- go test -race ./internal/runner/ ./internal/pylib/...` | **0** |
| `aira confine -- go test -race ./internal/daemon/` | **1** -- three `TestSliceCeilingRealCgroup*` failures, PRE-EXISTING on pristine `origin/master` (filed as AIRA-117, not caused here) |

### How each open question in the ticket was resolved

1. *Narrow the `memory.high`/`memory.max` gap (e.g. 95%)?* **Rejected on
   measurement.** Not a fix at the shipped 512 MiB cap (16-18 s), and the delay
   scales with the cap, so it degrades as caps grow.
2. *Add a daemon-side escalation/kill path?* **Rejected.** None exists, the
   architectural-simplicity rule disfavours adding one, and it would be strictly
   slower and strictly more machinery than the ~30 ms the kernel already
   achieves once the throttle is gone.
3. *Is this WSL2-specific?* **No** -- generic cgroup-v2/mm behaviour (Finding C).
4. *`memory.swap.max` on worker scopes?* **Yes**, and it turned out to be the
   larger of the two defects (Finding A). Spec-consistent: §3.4 already treats a
   worker OOM as normal and requeue-once.
5. *Interaction with AIRA-32's watermark tuning?* AIRA-32 is closed; its
   resolution anticipated this fork. The surviving key is `memory.max`, recorded
   in an amendment to its own ticket. **AIRA-32's work was not done here** -- the
   fraction is deliberately unchanged.

### Reviews

**Fable plan gate, 2 rounds, both PASS-WITH-CHANGES.** Round 1 (11 findings)
caught: the plan falsely claimed peak-RSS-history sizing when the code pins a
flat 512 MiB; the missing `worker_admit_cli_granted_linux_test.go`; a silent
degradation on the swap-unavailable path ("ENOENT is proof" contradicted by the
plan's own named gap); a negative control that could false-FAIL; and "loses
nothing" ignoring that post-grant slack shrinks. Round 2 (7 more) caught the
decisive one: a 32 MiB-only convergence test **passes against the 95% variant
the plan rejected**, so the test had to include the 512 MiB row. All 18 folded
into plan v3 before any code was written.

**Adversarial build review, APPROVE-WITH-FIXES, 7 findings, all handled:**

- **P1 unbumped `ProtocolVersion`.** The dangerous direction is undetectable
  below the version layer BY DESIGN -- the new supervisor deliberately tolerates
  a stale `memory_high` and deliberately stays silent on an absent `swap_cap` --
  so an old daemon with a new supervisor would have run whole suites with the
  livelock and no swap cap, silently. **Fixed: 7 -> 8.**
- **P1 tautological `swap_cap` tests, proved by the reviewer's own mutation:**
  hardcoding `enforced` in the daemon left the suite GREEN, because every fake
  also returned `enforced`. **Fixed:** both fakes now report `not-applicable`;
  re-ran the mutant, now killed in both packages.
- **P2 `-race` made the convergence bound vacuous** (`testdeadline.Wait` x4 ->
  20 s, above the 16-18 s of the variant it must reject). Live, since AIRA-20
  just re-enabled `-race` in CI. **Fixed:** scaling capped at 10 s.
- **P2 the negative control violated its own docstring** by `t.Fatal`-ing when
  no swap was used, which an exhausted shared swap budget can cause.
  **Fixed:** checks `SwapFree` and skips with the reason.
- **P2** renderer accepted an uncatalogued `swap_cap` -- **now refused**, as it
  already refuses an uncatalogued state or class.
- **P2** "reproduces the trigger point exactly" was an overclaim
  (`writeScopeMemoryCap` page-floors, ~2 KiB in ~344 MB) -- **corrected** in code
  and spec.
- **P2** `_describe_worker_death` could throw `UnicodeDecodeError` past its
  `OSError` guard -- **fixed**, contradicted its own fallback contract.

### Tickets filed

- **AIRA-110** -- `aira confine`'s own scopes have the same latent property:
  `memory.max` does not bound swap, and `aira.slice` allows 8 GiB. Deliberately
  NOT fixed here: a confined job is arbitrary user work, so killing one that
  would have completed by swapping is a design call of its own.
- **AIRA-117** -- three `TestSliceCeilingRealCgroup*` tests fail under `-race`
  on a real-cgroup host; reproduced on pristine `origin/master`, so not caused
  by this work.

### Not deployed

Per the brief, the rebuilt binary was not deployed and `aira-daemon.service` was
not restarted. **The `ProtocolVersion` 7 -> 8 bump makes that an ATOMIC
reinstall + restart**: until both move together, worker-admit answers
`E_DAEMON_PROTOCOL` (loud and terminal, by design) rather than silently losing
containment.
