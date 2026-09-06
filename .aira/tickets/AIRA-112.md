---
{"schema":1,"id":"AIRA-112","project":"aira","title":"Flake: TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration reports unverified scope integrity for a sub-millisecond job","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["confine","flake","testing"],"hold":false,"relations":[]}
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
