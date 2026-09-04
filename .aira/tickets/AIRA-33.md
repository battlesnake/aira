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

## Precondition restated (2026-09-04, backlog-remediation Phase 0, plan section 2) — text only

**The stated blocker is satisfied and therefore misleading.** This ticket reads
"Blocked by Slice 2 (needs AIRA's own suite migrated and clean)". Slice 2 is
AIRA-31, which is **done**. A reader coming to this ticket directly — rather than
through the backlog-remediation plan — would conclude it is ready to build. It is
not.

**The real, current precondition** is the one the plan records in section 1:

> AIRA-91 Part A built + fastest-ee re-verified + the `FASTEST_NO_AITEST=1` pin
> removed.

AIRA-91's root cause is closed (`systemd-oomd` `cgroup.kill`s the whole confine
scope under sustained `memory.high` reclaim pressure — a real exit 137, never
exit 0). Its **Part A** — the confine trailer's kill-attribution fix, unified with
AIRA-70 — is Phase 1 Fix 5 of that plan. Until Part A ships and fastest-ee runs
clean on aitest without the `FASTEST_NO_AITEST=1` pin, AIRA's own dogfood
evidence for "the suite has run clean on aitest" does not exist, and deleting the
xdist stack would remove the fallback before its replacement is trusted.

The pre-deletion grep-sweep for a non-pytest caller of the CPU park/active-set
machinery (spec section 6 flags it as unconfirmed, not assumed) is still required
and still unrun.

Knock-on, unchanged: AIRA-17, AIRA-26, AIRA-65 and AIRA-77 close automatically
with this deletion and need no separate action; AIRA-25 closes via whichever of
AIRA-29 or this ticket lands first. AIRA-20's `-race` CI restoration additionally
depends on this ticket landing (or on quarantining
`TestRealPytestRAMForkDoesNotPinHelperStdin`, `internal/pylib/pytest_integration_test.go:606`).
