---
{"schema":1,"id":"AIRA-35","project":"aira","title":"aitest worker memory.high/memory.max split converges too slowly under kernel throttle (WSL2)","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["admission","aitest","cgroup","oom"],"hold":false,"relations":[]}
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
