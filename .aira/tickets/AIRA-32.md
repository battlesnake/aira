---
{"schema":1,"id":"AIRA-32","project":"aira","title":"aitest Slice 3 — worker-admit generalisation polish + field-tuned watermarks","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","pytest"],"hold":false,"relations":[]}
---
Spec: docs/superpowers/specs/2026-09-01-aitest-design.md (§6 open questions, §5 Slice 3).

From Slice 1/2 field data: pick the memory.high-crossing proactive-recycle
watermark fraction, review worker-suite signature definition for peak-history
keying, and review the worker-admit wire shape for genuine language-agnostic
generality (only client today is aitest itself). Blocked by Slice 2.

## Precondition restated (2026-09-04, backlog-remediation Phase 0, plan section 2) — text only

**The stated blocker is satisfied and therefore misleading**, and there are two
further preconditions this ticket never named. It reads "Blocked by Slice 2";
Slice 2 is AIRA-31, which is **done**, so a reader coming here directly would
conclude it is ready to build.

**The real, current preconditions are three, all outstanding:**

1. **AIRA-33's precondition, shared:** AIRA-91 Part A built + fastest-ee
   re-verified + the `FASTEST_NO_AITEST=1` pin removed. This ticket's field-tuning
   wants Slice 1/2 field data from a suite actually running on aitest.
2. **AIRA-91 Part B decided** (the oomd-vs-admission policy call, backlog-
   remediation plan section 5 item 5 — explicit owner sign-off required, not a
   review-and-proceed decision).
3. **AIRA-35 landed.** This ticket's scope includes picking "the
   `memory.high`-crossing proactive-recycle watermark fraction" — and AIRA-35,
   contingent on the Part B decision, may remove worker-scope `memory.high`
   altogether (`internal/daemon/worker_admit.go:285`). Tuning a watermark on a
   mechanism that may be deleted is wasted work, and the plan carries AIRA-35 as
   gated on exactly that decision.

Preconditions 2 and 3 are this ticket's own, beyond the AIRA-33 chain: an earlier
draft of the remediation plan attributed the block to the AIRA-91/AIRA-33 chain
alone, which was incomplete.
