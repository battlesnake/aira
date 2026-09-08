---
{"schema":1,"id":"AIRA-117","project":"aira","title":"All three TestSliceCeilingRealCgroup* tests fail under -race on a real-cgroup host (helper dies before acknowledging)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["aira-106","cgroup","race","test"],"hold":false,"relations":[]}
---
Found while verifying AIRA-35 under `-race` (which AIRA-20 has just re-enabled
in CI). **Not caused by AIRA-35** -- reproduced on pristine `origin/master`
`7ecaf8a` in a detached worktree with no AIRA-35 changes present.

## Reproduction

```
git worktree add --detach <tmp> origin/master
cd <tmp>
aira confine -- go test -race ./internal/daemon/ \
  -run TestSliceCeilingRealCgroupSignalTracksRealAccounting
```

Result, reproducible (twice on the AIRA-35 branch and twice on pristine
master):

```
--- FAIL: TestSliceCeilingRealCgroupSignalTracksRealAccounting (2.28s)
    sliceceiling_real_cgroup_linux_test.go:405: helper did not acknowledge anon growth: <nil>
```

It is not one test: running `-run TestSliceCeilingRealCgroup` on pristine
`origin/master` fails **all three** real-cgroup ceiling tests, which is
consistent with a single shared cause in the fixture rather than three
independent bugs:

```
--- FAIL: TestSliceCeilingRealCgroupSignalTracksRealAccounting (2.66s)
--- FAIL: TestSliceCeilingRealCgroupNeverShrinksBelowRealUsage (2.52s)
--- FAIL: TestSliceCeilingRealCgroupUsageBoundHarnessDetectsAViolation (2.49s)
```

WITHOUT `-race`, on the same pristine master and the same host, the same test
passes (2/2 attempts, `ok aira/internal/daemon 2.518s`). So this is
race-build-specific, not a general flake.

## What the failure means

`ceilingCgroupFixture.grow` (`:173-181`) writes an instruction to the helper's
stdin and then `f.replies.Scan()`. The reported error is `<nil>`, so `Scan`
returned false on **EOF, not on a read error**: the helper subprocess had
already exited before acknowledging.

## Hypothesis (NOT yet confirmed -- it is the obvious candidate, not a finding)

The helper is the test binary re-invoked
(`exec.Command(os.Args[0], "-test.run=^TestSliceCeilingAllocHelper$")`,
`:135`), so under `-race` the helper is race-instrumented TOO. The race
detector's shadow memory multiplies an allocation's real footprint several
times over. The helper touches `ceilingFixtureTouch = 600 MiB` (`:30`) inside a
fixture scope capped at `ceilingFixtureCap = 2 GiB` (`:29`, written to
`memory.max` at `:120`). 600 MiB of instrumented anonymous memory plus the
helper's own runtime may exceed 2 GiB, in which case the kernel OOM-kills the
helper and the parent sees exactly this EOF.

If that is the cause, the fix is a race-aware fixture cap (or a smaller touch
under `-race`), not a change to the mechanism under test. Confirm first by
checking the helper scope's `memory.events` / `dmesg` for an OOM kill at the
moment of failure, rather than assuming.

## Why it is P2, not P1

CI's `race` job runs on a host with no delegated memory controller, so
`cgrouptest.SkipOrFailRealCgroup` skips this test there (the `race` job's own
comment in `.github/workflows/ci.yml` says as much). So CI is not red today.
The cost is that AIRA-106's real-cgroup signal has **no `-race` coverage at
all**, and all three tests hard-fail for any developer running `-race` locally
on a real-cgroup host -- which is the configuration this project's own
CLAUDE.md pushes people toward. It also means `aira confine -- go test -race
./...` cannot currently be used as a clean pre-merge gate on this machine, so
the next agent to try it will burn time re-deriving this.

relates AIRA-106, AIRA-20.

## Resolution (AIRA-117)

**The hypothesis was CONFIRMED before anything was changed.** Reproduced on this
branch's `origin/master` base, and the fixture cgroup was interrogated at the
moment of the failure rather than after teardown:

```
memory.max=2147483648        <- the flat 2 GiB ceilingFixtureCap
memory.peak=2147483648       <- pinned exactly at it
memory.events: max 21  oom 1  oom_kill 1
```

and, independently, the kernel log for the same fixture cgroup:

```
oom-kill:constraint=CONSTRAINT_MEMCG, oom_memcg=.../.aira-test-TestSliceCeilingRealCgroupSignalTracksRealAccounting-.../fixture,
  task=daemon.test, ...
Memory cgroup out of memory: Killed process ... (daemon.test) total-vm:6528904kB, anon-rss:2090240kB
```

with one such record per failing test (SignalTracksRealAccounting,
NeverShrinksBelowRealUsage, UsageBoundHarnessDetectsAViolation) and none for the
two tests that never call `grow`. So the `<nil>` EOF was the parent watching the
kernel OOM-kill a race-instrumented helper against the fixture's own cap.

### The measurement the fix is sized from

Re-run with the cap temporarily raised to 6 GiB so the helper could not die, the
cgroup's own `memory.stat anon` was read after each touch:

| touch | app bytes | cgroup anon after | delta |
|---|---|---|---|
| initial | 600 MiB | 1220 MiB | 1220 MiB |
| anon | 600 MiB | 2421 MiB | 1201 MiB |
| anon | 600 MiB | 3625 MiB | 1204 MiB |

**2.03x**, i.e. near enough exactly 2 — Go's `racemalloc` imitates a write across
the whole block, so the ThreadSanitizer shadow is resident, not merely reserved.
Three anon touches is the worst case any test here drives, so the -race fixture
needed 3.55 GiB against a 2 GiB cap: 78% over. The same measurement showed the
ORDINARY build was not comfortable either — 1.77 GiB against the same 2 GiB cap,
13% of margin.

### The fix, and why this shape

The mechanism under test is untouched: not one line of `internal/daemon/sliceceiling.go`
changed, and the constants the *signal* is judged against (the 96 MiB tolerance,
the 16 MiB test quantum, the 2 GiB external-pressure step) are unchanged. Only
the fixture's sizing moved.

1. **`ceilingFixtureRaceScale`** — one constant, in two small build-tagged files
   (`linux && race` = 2, `linux && !race` = 1). The app-level `ceilingFixtureTouch`
   is *divided* by it, so a -race build touches 300 MiB and the instrumentation
   doubles it back to the same **600 MiB of real cgroup memory** the ordinary
   build charges. Measured after: resident 1.81 GiB (ordinary) vs 1.83 GiB
   (-race) — 1.3% apart. That was chosen over simply raising the cap under -race
   because it keeps the quantity every assertion in the file is *about* — and the
   RAM this shared box actually pays — invariant across build modes, rather than
   making -race silently cost 4.8 GiB.
2. **The cap is now derived, not picked.** `ceilingFixtureCap = 2 x
   (ceilingFixtureAnonTouches x ceilingFixtureResident + ceilingFixtureOverhead)`
   = 4112 MiB, a full 2x margin over a worst case the new test *checks*. Page
   cache is deliberately excluded from the model: it is reclaimable, so the
   kernel evicts it under the cap instead of killing the job; only the anon floor
   can force an OOM. The cap's value is otherwise inert to what the tests assert
   (the two tests a fixture-sized `memory.max` could clamp already model the
   maximum away; the negative control writes its own 32 MiB cap over it).
3. **`grow` now names the cause.** On a failed `Scan` it reads the fixture's
   `memory.events` and, when `oom_kill > 0`, reports the OOM kill with
   `memory.max`/`memory.peak` and points at the two constants — instead of the
   bare `<nil>` that cost this ticket its investigation.

### Regression test

`TestSliceCeilingRealCgroupFixtureSurvivesWorstCaseGrowth` drives ONE fixture
through the worst-case sequence any test here performs (initial touch + two anon
grows, each followed by page-cache growth competing for the same cap) and pins
the sizing model in **both** directions — the footprint must reach the touches'
nominal total (so it cannot pass by allocating nothing, and it is the tripwire if
a future toolchain stops charging 2x) *and* stay under the modelled worst case
the cap is derived from, with `oom_kill` unchanged and the helper alive.

Proved non-porous by re-creating the exact pre-fix state and re-running it:

- `ceilingFixtureRaceScale=1` + `ceilingFixtureCap=2<<30` (i.e. pristine master)
  under -race → **FAIL**, with the new diagnostic: *"the kernel OOM-KILLED it
  against the fixture's own cap (memory.max=2147483648 memory.peak=2147487744
  oom_kill=1) ... see ceilingFixtureCap and ceilingFixtureRaceScale"*.
- `ceilingFixtureRaceScale=1` + the new cap under -race → **FAIL** on the model
  bound: *"the fixture's non-reclaimable footprint 3816494784 exceeds the
  modelled worst case 2155872256"*. So the upper bound is live independently of
  the OOM path.

The measured footprint is also `t.Logf`'d on every run, so the number the
constants are derived from is visible under `-v` on whatever kernel and build
mode is running rather than living only in this ticket.

### Verification that the fix addresses the DIAGNOSED cause

The before-evidence was per-fixture OOM kills. After the fix, the same
`-race -run TestSliceCeilingRealCgroup` run leaves the machine-wide count of
fixture OOM kills incremented by exactly **one**, and that one is
`aira-test-TestSliceCeilingRealCgroupHarnessDetectsALimitWrite` — the negative
control, whose entire purpose is to provoke a kill and which asserts it happened.
Zero kills from the three tests that used to die, and each of them asserts
`oom_kill` unchanged in its own body.

### Accepted cost

The higher cap lets the fixture's page cache grow further before the kernel
reclaims it, so `memory.current` peaks around 3.2 GiB (ordinary) / 2.6 GiB
(-race) rather than being clipped at 2 GiB. That memory is reclaimable and the
whole cgroup is torn down at test end; the NON-reclaimable footprint, which is
what the machine really pays, is unchanged from before at ~1.8 GiB.

### Gate

Run in the foreground under `aira confine`, exact exit codes:

- `go build ./...` — exit **0**
- `go vet ./...` — exit **0**
- `AIRA_REAL_CGROUP=1 go test ./... -count=1` — exit **0**
- `AIRA_REAL_CGROUP=1 go test -race ./internal/daemon/ -run TestSliceCeilingRealCgroup -count=2` — exit **0** (`ok aira/internal/daemon 15.070s`)
- bonus, the ticket's own "cannot be used as a pre-merge gate" complaint:
  `AIRA_REAL_CGROUP=1 go test -race ./internal/daemon/ -count=1` — exit **0**
  (`ok aira/internal/daemon 122.652s`)

## Review (Fable build-review gate) — MERGED

PR #90 merged as `5588a8d` (2026-09-07). Everything below is the reviewer's own
reproduction on this host (kernel 6.18.33, go1.25.0), not the builder's transcript.

- **Root cause independently re-confirmed BEFORE trusting the fix.** Pristine
  `origin/master` (`4463bce`) in a detached worktree,
  `AIRA_REAL_CGROUP=1 aira confine -- go test -race ./internal/daemon/ -run
  TestSliceCeilingRealCgroupSignalTracksRealAccounting$` → exit 1, the bare
  `helper did not acknowledge anon growth: <nil>`; and the kernel log, timestamped
  after a marker taken immediately before the run:
  `oom-kill:constraint=CONSTRAINT_MEMCG, oom_memcg=.../.aira-test-TestSliceCeilingRealCgroupSignalTracksRealAccounting-*/fixture,
  task=daemon.test` / `Killed process (daemon.test) total-vm:6602704kB, anon-rss:2090112kB`.
  Anon pinned at the flat 2 GiB cap; the hypothesis is an established finding.
- **Fix addresses that cause.** On the branch, `-race -run TestSliceCeilingRealCgroup
  -count=3` → exit 0, 18/18 PASS. The fixture OOM kills recorded after a fresh marker
  were exactly three, all `HarnessDetectsALimitWrite` (the negative control, one per
  count); zero from the three tests that used to die.
- **The measurement the constants rest on holds in BOTH modes on this host:** the new
  test logged resident = 1.79 GiB under `-race` (300 MiB touched x3 → 2.04x) and
  1.77 GiB in the ordinary build (600 MiB x3), against the 2.01 GiB modelled worst
  case and 4.02 GiB cap. `memory.peak` is present on this kernel, so the diagnostic
  prints a real figure.
- **Non-porosity re-run by the reviewer, both directions.** Pre-fix constants
  restored (`raceScale=1`, `cap=2<<30`) under `-race` → FAIL with the new diagnostic
  (`OOM-KILLED ... memory.max=2147483648 memory.peak=2147487744 oom_kill=1`).
  `raceScale=1` with the NEW cap under `-race` → FAIL on the model bound alone
  (resident 3.82 GiB > 2.01 GiB; `memory.current` clamped at the cap, no kill), so the
  upper bound is live independently of the OOM path. Edits reverted, tree clean.
- **Mechanism untouched:** `internal/daemon/sliceceiling.go` has no diff; only fixture
  sizing and the `grow` diagnostic moved.
- Gates, foreground under `aira confine`, exact exit codes: `go build ./...` 0;
  `go vet ./...` 0; `go vet -race ./internal/daemon/` 0;
  `AIRA_REAL_CGROUP=1 go test ./... -count=1` 0; the `-race -count=3` run above 0.
  CI: build+vet+gofmt, test and race all green on the PR head.

Accepted, non-blocking findings (documented, not fixed here):

1. The "footprint invariant across build modes" claim is true of ANON only. The
   helper's `file` instruction writes `size` = `ceilingFixtureTouch` bytes, so under
   `-race` the page-cache growth in `SignalTracksRealAccounting` and
   `NeverShrinksBelowRealUsage` is 300 MiB, not 600 MiB. Still ~3x the 96 MiB tolerance
   and ~19x the 16 MiB test quantum, so the page-cache half of the signal is exercised
   at a real, non-trivial size; noted as a precision gap in the Resolution's wording.
2. `SignalTracksRealAccounting` (line ~539) asserts the anon rise against
   `ceilingFixtureTouch - tolerance`, i.e. 204 MiB under `-race` for a rise that is
   really ~600 MiB. It should compare against `ceilingFixtureResident` so the floor is
   the same 504 MiB in both modes. Not a false-pass risk today: the new test's lower
   bound (`resident >= 3 x 600 MiB - 96 MiB`) is the tripwire that catches a helper
   charging less than the constants claim. Cheap follow-up tightening.
3. `ceilingFixtureRaceScale = 2` is a measured property of this toolchain/kernel pair,
   and the tripwire for it is a hard FAIL (not a skip) under `AIRA_REAL_CGROUP=1` on a
   host where the factor differs. That is the intended honesty behaviour and the
   message names both constants; recorded so the next reader on a different Go race
   runtime knows where to look.
