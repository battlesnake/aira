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

## Sharpened by AIRA-151, unanswered (2026-09-07)

AIRA-151 shipped and removed the last accidental mitigation of this defect
(plan §3.7 / G2). `ResolveConfineReserve` still hands the daemon exactly
`DefaultConfineMemoryReserve` for every unpinned request regardless of how large
the slice is, so on any slice whose ceiling is below 4 GiB that value is over the
ceiling. What changed is only the consequence:

- **before AIRA-151:** a signature with **no** OOM record was refused terminally
  (`E_ADMIT_TOO_LARGE`), while a signature **with** one was clamped onto the
  ceiling and then usually wedged until the wait expired — two different
  outcomes for one defect, and the OOM record made the outcome worse;
- **after AIRA-151:** both are refused terminally, immediately, with `required`
  and `cap_minus_headroom`. The defect is unchanged; its consequence is now
  uniform, certain and immediate rather than sometimes-wedged.

The design question is unchanged and unanswered: **what should a small slice do
with a client that has no idea how big it is.** The candidate answers —
condition the default on the ceiling, make the daemon publish the admissible
size, refuse at install time — all remain open and all remain sizing decisions
needing their own two-loop.

One consequence AIRA-151 accepted and recorded rather than fixed, which belongs
to this ticket: on a slice whose ceiling is below the 4 GiB default, AIRA-128's
cold-start self-heal ("re-run the identical command" after `terminated-by=oom`)
no longer works for that command without a pinned reserve. AIRA-151's generated
agent-guide clause says so; the underlying fault is this unconditioned default.
