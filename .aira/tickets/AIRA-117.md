---
{"schema":1,"id":"AIRA-117","project":"aira","title":"All three TestSliceCeilingRealCgroup* tests fail under -race on a real-cgroup host (helper dies before acknowledging)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["aira-106","cgroup","test","race"],"hold":false,"relations":[]}
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
