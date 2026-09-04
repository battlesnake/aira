---
{"schema":1,"id":"AIRA-34","project":"aira","title":"confine scope-integrity reports `migrated` for legitimately-nested sub-scopes (leaf-only check)","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["accepted-gap","aitest","confine","deferred","telemetry"],"hold":false,"relations":[]}
---
Found during the aitest bootstrap-sequence spike (2026-09-01, ~/tmp/aitest-bootstrap-spike.sh): when a confined process relocates itself into a CHILD cgroup of its own scope (e.g. aitest's supervisor moving into `outer/.aira-supervisor` before enabling subtree_control + forking per-worker sub-scopes), confine's scope-integrity facet reports `scope-integrity=migrated` instead of `contained`. The process is still WITHIN the scope subtree (genuinely contained) — the check (#20/#70 ScopeContained attestation) verifies membership of the exact scope LEAF, so any move into a descendant cgroup reads as a migration/escape.

IMPACT: telemetry-only. Per #70, ScopeContained has 0 production consumers — nothing functional breaks; the only cost is that every aitest outer scope (and any future legitimately-nested confine job) reports `migrated` forever, which is misleading noise in the trailer.

DECISION (Opus + aitest, 2026-09-01): **DOCUMENT, do NOT build.** aitest documents `migrated` as its expected outer-scope status. A subtree-aware integrity check (a pid in ANY descendant cgroup of the scope = still contained) would be MORE correct, but per the architectural-simplicity rule (HARD, owner) telemetry-only signals never justify new machinery — "keep the primitive + document the gap" beats new code, exactly the #70 lesson. relates: aitest (nested per-worker sub-scopes make this the norm), #20 (descendant-escape attestation), #70 (ScopeContained telemetry-only, sampling gap accepted).

BUILD ONLY IF: a real production consumer of scope-integrity emerges AND nested sub-scopes become common enough that the `migrated`-noise is actually harmful. Until then: deferred/accepted-gap.

## Re-scoped (2026-09-04, backlog-remediation Phase 0, plan section 2) — one factual correction, decision unchanged

**The ticket's IMPACT claim is stale and is corrected here: "ScopeContained has 0
production consumers — nothing functional breaks" is no longer true.**

There is exactly one production consumer, on one path:
`admissibleScopeIntegrity` (`internal/store/gate_command.go:223`) admits a
command gate's evidence only when scope-integrity is `contained` or `unverified`.
A run reporting `migrated` — which per this ticket is what EVERY legitimately
nested confine job reports, aitest's outer scope included — therefore has its
evidence rejected as inadmissible (`gate_command.go:231`). That is a functional
consequence, not telemetry noise.

It is nonetheless **latent, not live**: a fresh read-only count of
`~/.local/state/aira/state.db` (2026-09-04, recorded in the backlog-remediation
plan section 5 item 2) shows `gates`, `gate_results`, `gate_proofs`,
`gate_attestations`, `gate_baselines`, `gate_baseline_active`, `test_reports` and
`test_report_results` all EMPTY. No gate has ever run in production here, so no
evidence has ever been rejected by this path.

**The DECISION is unchanged: document, do not build.** The architectural-
simplicity rule still applies, and a subtree-aware integrity check is still new
machinery for a consequence nothing currently exercises. What changes is the
BUILD-ONLY-IF trigger, which is now concrete rather than hypothetical: build it
when a gate is actually configured in a project that also runs nested confine
scopes — at that moment the gate's evidence starts being rejected, and this stops
being latent.

(The plan's row for this ticket says to "correct the ticket's stale line
references". There are no line references in the body to correct — it cites
tickets, not files:lines. The stale claim above is the correction that was
actually needed.)
