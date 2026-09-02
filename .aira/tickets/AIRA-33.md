---
{"schema":1,"id":"AIRA-33","project":"aira","title":"aitest Slice 4 — retire aira_xdist_governor / governor-slot / daemon governor.go","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","cleanup","pytest"],"hold":false,"relations":[]}
---
Spec: docs/superpowers/specs/2026-09-01-aitest-design.md (§3.8, §5 Slice 4, §6).

Delete internal/pylib/aira_xdist_governor, aira governor-slot
(internal/runner/governor_slot.go), pytest call sites of aira confine-reserve,
and internal/daemon/governor.go's park/active-set scheduler + the three
2026-08-30 scheduler-slice specs, once AIRA's own dogfood suite has run clean
on aitest. Before deleting governor.go: grep-sweep for any non-pytest caller
of the CPU park/active-set machinery (spec §6 flags this as unconfirmed, not
assumed). Blocked by Slice 2 (needs AIRA's own suite migrated and clean).
