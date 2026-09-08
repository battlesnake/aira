---
{"schema":1,"id":"AIRA-168","project":"aira","title":"No real-cgroup coverage of the degenerate too-small-slice refusal","status":"done","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","tests"],"hold":false,"relations":[]}
---

AIRA-153 deferral **G9**: an accepted coverage gap, written down rather than
left to a reviewer to notice.

AIRA-153 makes `runner.SliceFittedReserve` return 0 below
`runner.MinPinnedScopeCap` (1 MiB), so a slice whose admission ceiling is at or
below 1205862 bytes still refuses terminally with `required` and
`cap_minus_headroom` populated, exactly as before the change. That is invariant
I7, and it is what stops a sub-megabyte `memory.max` being handed to a job that
would instant-OOM on placement.

It is driven deterministically through the REAL `admitConnection` wire path with
a stubbed memory reader
(`TestASliceTooSmallForAnyViableReserveStillRefusesTerminally`), but NOT against
a real cgroup: reproducing it needs a slice with a sub-1.2 MiB ceiling, which
cannot hold the Go test harness itself, let alone a workload.

Same class as AIRA-149's F7 coverage gap. Accepted. Recorded so "the suite is
green" is not read as covering it.

## Closed (2026-09-08)

Accepted coverage gap, same class as AIRA-149's F7: the degenerate refusal is
already driven deterministically through the real wire path with a stubbed
memory reader; a real sub-1.2 MiB cgroup cannot hold the Go test harness
itself, so no further coverage is being chased.
