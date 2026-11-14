---
{"schema":1,"id":"AIRA-337","project":"aira","title":"De-flake two internal/runner tests: confine scan reads cmdline before exec completes; AIRA-138 CPU reader fires on a zombie","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.42","labels":[],"hold":false,"relations":[]}
---


Filed from the 2026-11-14 rant triage (see docs/plans/2026-11-14-backlog-plan.md). Surface: daemon. Size: S. Correctness-critical (two-loop): no.

## Problem
Two make-ci flakes in internal/runner, both test-fixture races and not product defects. (1) TestConfineScanReadsTheSupervisorCommandAndSaysUnevaluatedWhenItCannot (confine_manage_linux_test.go:980-1005) scans straight after exec.Command.Start(). /proc/<pid>/cmdline can read empty between the exec-status pipe close and create_elf_tables, so Command is nil. Measured 2/20 failures (-count=20, exit 1) on master. The AIRA-135 review flagged it and it was never fixed (RANT-57). (2) overBudgetOnceLeaderIsDead (confine_deadline_linux_test.go:255-262) fires once processLive says dead, which includes a ZOMBIE, while the fixture's leafMembersLocked still lists the unreaped leader. killConfineScope then writes cgroup.kill and the test sees fired-kill-completed instead of fired-not-executed (RANT-58). This is the AIRA-148 zombie-window class; 0/20 in an isolated run, but it was observed under load.

## Evidence
RANT-57: go test -count=20 -run TestConfineScanReads... exit=1, 2 FAIL at :1005 Command:<nil>. RANT-58: -count=20 exit=0 (not reproduced), mechanism from processLivenessFromStat runner_linux.go:1910-1920 (Z => dead) vs leafMembersLocked :90-98 (start tick => member) and killConfineScope confine_linux.go:2602-2634.

## Minimum fix (from-scratch minimum)
(1) After live.Start(), poll /proc/<livePID>/cmdline (bounded by aira126Scale(5s), failing with a clear message on expiry) until it contains "held-job", then call ListConfines. (2) In overBudgetOnceLeaderIsDead, fire only when processLive(identity)==processDead AND len(scope.Members())==0 (the leader is reaped), mirroring AIRA-148's 'gate on the reap, not on the zombie'. This also hardens the four other tests sharing the reader (lines 369, 467, 500, 570). Test-only: no production file changes. Deliberately NOT included: retries, larger sleeps, or any change to processLive or ListConfines.

## Tests
Each fix is shown to work by a forced-window mutation, not just green repeats. For (1), a scratch build where the test reads with no settle poll while a readCmdline hook returns empty on the first call fails, and the fixed test passes. For (2), a scratch fixture that delays the reap (reader fires on zombie) yields fired-kill-completed before the fix and fired-not-executed after. Then run `aira confine -- go test -count=50 -run '<name>$' ./internal/runner` for each, exit 0, plus the five deadline tests together.

## Files
- internal/runner/confine_manage_linux_test.go
- internal/runner/confine_deadline_linux_test.go

## Related tickets
AIRA-135, AIRA-148
