---
{"schema":1,"id":"AIRA-153","project":"aira","title":"The unpinned 4 GiB confine reserve default is unconditioned by the slice ceiling","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F4**, and the actual reason AIRA-150 is a certainty on a
small slice and near-unreachable on the production one.

`runner.ResolveConfineReserve` leaves an unpinned request at
`DefaultConfineMemoryReserve` (4 GiB) regardless of how large the slice it is
about to be admitted into actually is. On the 64 GiB production `aira.slice` the
default is well under the ceiling and nothing is visible; on any small slice — a
CI slice, a shim-configured ceiling, a fixture — the blind default alone exceeds
the ceiling, so EVERY unpinned request resolves to the ceiling (with an OOM
record) or is refused terminally (without one).

Note the asymmetry that frames the question: the no-OOM path ALREADY refuses
such a request terminally with `E_ADMIT_TOO_LARGE`. So the design question is
not "should the default be clamped" but "what should a small slice do with a
client that has no idea how big it is".

Sizing decision on the machine-wide gate: full two-loop.
