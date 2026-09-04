---
{"schema":1,"id":"AIRA-78","project":"aira","title":"Ratchet gate selects evidence by git HEAD, not by the subject digest — a dirty tree mints a pass from another tree's reports","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["dogfood","gate","honesty"],"hold":false,"relations":[]}
---
Found during the AIRA-72 two-loop (Codex/Sol P0-3, confirmed by the Fable plan gate). Deliberately deferred out of AIRA-72's scope because it is an evidence-selection defect, not a digest-scope defect.

## Defect

`evaluateRatchet` (`internal/store/gate_ratchet.go`) binds its verdict to a subject digest taken over working-tree bytes, but selects the test reports it compares via `s.gitValue(ctx, "HEAD")`. On a dirty tree those disagree: the comparison consumes reports produced for the committed code while the verdict is stamped against the working tree. The gate then *mints* a fresh pass from evidence that does not describe the subject.

This is distinct from AIRA-72. AIRA-72 was a stale pass being *re-served*; this is a fake pass being *newly created*, so AIRA-72's fix does not close it. AIRA-72 strictly narrows the window — a dirty tree now produces a different subject digest, so a previously stored pass is no longer re-served — but a freshly computed ratchet verdict still consumes HEAD-selected evidence.

Under `CLAUDE.md`'s rule ("a check that cannot establish its result reports `unevaluated`, never a fake pass") this is a P0.

## Direction

Either require a clean tree for a ratchet verdict, or — better — have test reports carry a subject digest and require it to match the digest being bound. A cheap interim close suggested by the plan gate: after the baseline resolution in `evaluateRatchet`, run `git diff-index --quiet HEAD --` and report `U_GATE_INCOMPARABLE` on a non-zero exit.

Needs its own adversarial loop; the interim close should not be applied without one, since `U_GATE_INCOMPARABLE` on every dirty tree is a large behavioural change for anyone actually using ratchets.

## Note

AIRA-72's "closed on every checker" yield claim was corrected to exclude this case.

## Why this P0 is not being built now (2026-09-04, backlog-remediation Phase 0, plan section 2) — text only

Recorded on the ticket itself so a reader who never opens the backlog-remediation
plan is not left with an unexplained parked P0.

**The severity is correct and unchanged.** Under CLAUDE.md's "a check that cannot
establish its result reports `unevaluated`, never a fake pass", minting a fresh
pass from HEAD-selected evidence against a working-tree digest is a P0.

**It is LATENT, not live: the gate kind has no producer.** A fresh read-only count
of `~/.local/state/aira/state.db` (2026-09-04) shows the *entire* gate subsystem
empty — `gates`, `gate_results`, `gate_proofs`, `gate_attestations`,
`gate_baselines`, `gate_baseline_active`, `test_reports`, `test_report_results`,
all zero rows. Ratchet evidence is `test_reports`, and nothing writes them. No
fake pass has ever been minted here, and none can be until something starts
producing test reports.

**Recommended disposition, awaiting explicit owner sign-off — NOT actioned
anywhere in this plan:** **delete the ratchet gate kind.** Zero production rows,
and it is consistent with the owner's stated preference for deleting over adding
(plan section 0). That is the narrow form of the wider question the plan raises
in section 5 item 2, which is whether more of the gate subsystem should go the
same way.

**Fallback if the owner keeps it:** give test reports a subject digest (a schema
change — free, since AIRA has no users or data to migrate) and require it to match
the digest being bound, then apply the Phase 1 Fix 3 captured-subject pattern to
it. The plan gate's cheap interim (`git diff-index --quiet HEAD --` →
`U_GATE_INCOMPARABLE`) is explicitly NOT recommended as a standalone close: this
ticket's own body already warns it is a large behavioural change for anyone using
ratchets, and it needs its own adversarial loop.

**Either path is gated on the owner's answer**, exactly like AIRA-28/29 and
AIRA-91 Part B elsewhere in that plan — it is not a default an executor proceeds
on.
