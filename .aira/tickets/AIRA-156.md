---
{"schema":1,"id":"AIRA-156","project":"aira","title":"Accepted coverage gap: no real-cgroup test drives the ceiling-equal-reserve wedge","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","testing"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F7**, filed as a RECORDED, ACCEPTED gap rather than as work.

Reproducing AIRA-150 end to end needs a slice whose ceiling equals the resolved
reserve with a nonzero residual charge — which is PRECISELY the knife-edge
AIRA-139 removed from the OOM self-heal fixture, and re-adding it would re-add
that flake (measured: pass when a poll happened to read `current=0`, 30s
`E_ADMIT_SATURATED` when it read 4096).

What covers it instead: `TestSaturatedRejectionReportsNoContentionWhenNothingWasEverQueuedOrHeld`
(`internal/daemon/admit_saturated_diagnosis_test.go`) drives the identical shape
deterministically through the real `admitConnection` wire path with a stubbed
slice reader, and `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` keeps the real
fixture clear of the edge.

The residual, accepted gap is therefore narrower than "uncovered": no REAL-CGROUP
test drives the wedge. Close this only if that changes; do not close it by
re-adding the flake.
