---
{"schema":1,"id":"AIRA-78","project":"aira","title":"Ratchet gate selects evidence by git HEAD, not by the subject digest — a dirty tree mints a pass from another tree's reports","status":"done","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["dogfood","gate","honesty"],"hold":false,"relations":[]}
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

## Resolution (2026-09-05)

Owner sign-off received: **delete, not fix.** Re-verified read-only against the
live `~/.local/state/aira/state.db` immediately before starting — `gates`,
`gate_results`, `gate_proofs`, `gate_attestations`, `gate_baselines`,
`gate_baseline_active`, `test_reports`, `test_report_results` were all still
zero rows.

**PR [#43](https://github.com/battlesnake/aira/pull/43), squash-merged as
`d0526d3`** (branch `aira78-delete-ratchet-gate` off `origin/master`).

**Deleted:** `internal/gate/gate.go`'s `KindRatchet`/`CheckerRatchet`/`Ratchet`
payload/`ComparisonKey`/`ratchetShardPattern` and every ratchet validation
branch; `internal/store/gate_ratchet.go` and `gate_ratchet_test.go` entirely
(`evaluateRatchet` and the whole `GateBaseline` mechanism —
`PinGateBaseline`/`ShowGateBaseline`/`ResolveGateBaseline`/
`deriveRatchetBaseline`/`baselineFromAuditRecord`, which existed only to serve
ratchet gates, not shared with checkable/manual); the `gate_baselines`/
`gate_baseline_active` DB tables (dropped via an idempotent `DROP TABLE IF
EXISTS` in `initDB` — no migration needed, zero rows ever existed), their audit
record kinds (`"baseline"`/`"baseline-pointer"`), reconcile-projection logic,
CLI subverbs (`baseline-pin`/`baseline-show`), and `Store`-interface methods;
the `"ratchet-status"` insight gauge. Also touched: `internal/store/
gate_command.go`, `gate_write.go`, `gate_audit.go`, `gate_index.go`,
`schema_ownership.go`, `store.go`; `internal/core/core.go`, `store_guard.go`,
`recording_store_test.go`, `dispatch_metadata_test.go`, `routing_test.go`,
`gate_honesty_test.go`, `insights_test.go`, `skill_test.go`;
`internal/gitcontext/env.go`; `cmd/aira/main.go`, `cmd/aira/skill_test.go`.

**Kept, deliberately:** `internal/gate/canary.go` and its
`CanarySyntheticRatchet` canary mode are byte-for-byte unchanged, per explicit
scope. `runCanary`'s synthetic-ratchet branch in `gate_eval.go` depends on
comparator primitives (`RatchetSnapshot`, `RatchetComparison`,
`compareNoNewFailures`, `sortedSet`) that used to live in `gate_ratchet.go` —
these were **relocated**, not deleted, into `gate_eval.go`, with a comment
explaining why. A store-level integration test
(`TestSyntheticRatchetCanaryLaneStillWiredThroughRunCanary`) exercises
`canaryFor` → `runCanary` → `compareNoNewFailures` end-to-end on a checkable
gate (the vehicle changed since ratchet gates no longer exist; `runCanary`'s
synthetic-ratchet branch never reads the referring gate's kind or payload).
`checkable`/`manual` gate kinds are untouched.

**`internal/codes/codes.go` divergence handling:** `U_GATE_BASELINE_MISSING`,
`U_GATE_INCOMPARABLE`, `E_GATE_BASELINE_INVALID` deleted from the catalogue —
each was exclusively produced by now-deleted code (verified via
`internal/codes/produced_test.go`'s produced-vs-catalogued `go/ast` scan, green
with **zero** new divergence-table entries). `E_GATE_RATCHET_REGRESSED` was
**kept**: it remains genuinely produced by the surviving (relocated)
`compareNoNewFailures`, reachable via the untouched synthetic-ratchet canary
lane (`ValidateCanary` requires that mode to always introduce a new failure, so
the comparator always returns that code for it).

**Docs:** removal-note banners added to the dedicated M13b ratchet design spec
and the M15b gauges spec, plus a one-line pointer at the `Gate` definition in
the main authoritative design doc (`2026-08-07-aira-design.md`). Other dated
historical milestone docs left as the historical record, consistent with how
this repo handled the AIRA-73 outbox deletion.

**Verification:** `go build ./...` exit 0; `go vet ./...` exit 0; `go test
./...` (all 15 packages) exit 0 — run independently three times (post-change,
post-rebase onto latest `origin/master`, and again via the pre-push hook's
`make` with `-count=1`).

**Process notes:** the actual code deletion was delegated to Codex
(`mcp__codex-terra__codex`) per this project's standing rule, with a detailed
brief covering the ratchet-exclusive `GateBaseline` mechanism (which doesn't
contain the literal word "ratchet" in most identifiers) and the
canary-relocation subtlety up front; Codex's diff was self-reviewed file by
file before any test/build run. The branch was rebased onto `origin/master`
(5 commits, including a same-night AIRA-89 no-op-branch cleanup that
deliberately left the `gate_index.go` baseline block for this ticket to
resolve) immediately before opening the PR, and the remote branch tip was
re-verified (local HEAD / upstream / `git ls-remote` / `gh pr view` all
agreeing) immediately before merging, per the AIRA-91 incident lesson.

**Sol (codex-sol) adversarial review:** first pass **BLOCK** — 2×P1 + 1×P2:
(1) the dropped `gate_baselines`/`gate_baseline_active` tables would persist in
any already-initialized local DB with no migration; (2) the relocation deleted
`TestSyntheticRatchetCanaryUsesSameComparatorInMemory` without replacing it,
leaving `runCanary`'s actual wiring for the synthetic-ratchet lane untested
(only the pure comparator function was still tested — a regression in the
wiring itself would have passed); (3) the M15b gauges spec still presented
`ratchet-status` as live. All three fixed (idempotent `DROP TABLE IF EXISTS`;
the relocated `runCanary` integration test above; the M15b banner) and
confirmed on a second pass — **PASS WITH NITS**, two minor P2s recorded as
consciously deferred rather than fixed: a dedicated migration-seed-then-verify
regression test for a table that will never carry data in practice, and a note
observing (not objecting) that the new integration test's expected
predicate/code are the only values a valid synthetic-ratchet canary can
produce.
