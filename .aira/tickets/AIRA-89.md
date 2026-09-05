---
{"schema":1,"id":"AIRA-89","project":"aira","title":"Dead and unreachable symbols across store, runner, daemon and the CLI faces","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["dead-code","simplification"],"hold":false,"relations":[]}
---
PR #12 finding **B14** / plan candidate **78**, filed by the simplification programme's
Phase 0 (plan §4.2, §4.3). Suggested severity **P3**; the schema has no P3, so filed P2 at the
bottom of the band.

PR #12 traced a list of symbols with no production caller: `Store.pathLock`, `sortReports`,
`recordScanFinding`, `hasGateContent`, `FlakyCellStateSummary`, `ListComputeSpendByPhase`,
`GateAudit.Verify`, `GateAuditRecords`, `copyAsOf`, `PathTier`, `Store.gitDir`,
`cappedWhaleOnDisk`, `parseInstalledMemoryHigh`, `inspectExistingPath`, residual whale
coexistence checks, `scrubEnv(_, true)`, and three no-op branches.

## Three symbols are explicitly NOT part of the sweep

The plan carves these out, and the reasons are not stylistic:

- **`hasGateContent`** is named in **AIRA-56**'s analysis as *the unsound predicate*. Delete it
  as part of AIRA-56's fix so the reasoning survives with the deletion, not as a stray dead
  symbol.
- **`FlakyCellStateSummary`** belongs to the cell-level flaky classifier (plan candidate 62),
  and goes with that decision rather than on its own.
- **`GateAudit.Verify` / `GateAuditRecords`** must be re-checked against candidate 42's
  durable-head carve-out and AIRA-56's reader first — they are the plausible home of the
  ledger truncation check, which is load-bearing (plan §5.5).

## Status

The remainder is being removed by the programme's Phase 0 mechanical sweep, each symbol
re-verified as unreferenced against master before deletion rather than inherited from the
review's grep. This ticket records the finding and its three carve-outs so the exceptions are
not lost when the sweep lands.

## Partially discharged — simplification programme Phase 0 (branch `aira-phase0-mechanical`)

**Removed** (each re-verified as having no caller before deletion):
`Store.pathLock`, `Store.gitDir`, `sortReports`, `Store.recordScanFinding`,
`ListComputeSpendByPhase`, `copyAsOf`, `PathTier`, `cappedWhaleOnDisk`,
`inspectExistingPath`, and `scrubEnv`'s unreachable `rewrite` branch.

**Still open — the plan's three named carve-outs**, unchanged: `hasGateContent`
(delete as part of AIRA-56's fix), `FlakyCellStateSummary` (goes with the flaky
classifier, plan candidate 62), `GateAudit.Verify` / `GateAuditRecords`
(re-check against the durable-head carve-out and AIRA-56's reader first).

**Still open — three more the sweep declined**, with reasons:

- `parseInstalledMemoryHigh` is **not dead**: `internal/install/install_test.go:394`'s
  fake systemd parses the written unit file with it. Production-dead, yes, but it
  has a real caller and a symmetric live sibling (`parseInstalledMemoryMax`).
  This is PR #12's grep being wrong, not a deletion the sweep skipped.
- The **residual whale coexistence checks** are live behaviour, not dead code:
  they refuse an install that would let two independent caps sum past physical
  RAM unless `--allow-overcommit` is given (`install.go:511-513`, `:1624-1638`,
  `:1770`). Removing a refusal on the shared machine's memory caps is not
  mechanical; it belongs with plan candidate 21 in Phase 5a.
- The **"three no-op branches"** cannot be identified from the plan's summary
  alone, and PR #12's own text is not in the repository. Needs the reviewer's
  list before anyone can act on it.

## Carve-outs discharged — backlog remediation Phase 0 (plan §2)

All **three named carve-outs** above are now resolved, each re-verified as having
zero callers immediately before deletion (a fresh repo-wide grep including
non-Go files, not the sweep's snapshot):

- **`hasGateContent`** (`gate_index.go:54`) — deleted. AIRA-56's fix does not
  resurrect it: the plan resolves AIRA-56 by surfacing the existing
  `U_GATE_SET_EMPTY` primitive as an advisory finding, not by adopting a
  filesystem "does this project use gates" predicate. The reasoning that
  condemned it survives in
  `docs/plans/2026-09-03-aira53-54-gate-honesty-plan.md:324,472` and in AIRA-56's
  own body, so the deletion loses nothing.
- **`FlakyCellStateSummary`** (`testreport.go:90`) — deleted. It was a one-line
  alias forwarding to `FlakyCellSummary`, which stays and is the live method; no
  classifier decision is prejudiced by removing a synonym.
- **`GateAudit.Verify` / `GateAuditRecords`** (`gate_audit.go:369,449`) —
  deleted, and the load-bearing check the carve-out was protecting is confirmed
  NOT among them. The ledger truncation/chain/head verification lives entirely in
  `GateAudit.Read` → `readWithKey` (frame digest, HMAC tag, seq/prev-digest chain,
  nonce reuse, durable-head match), and `Read` has eight production callers
  (`gate_eval.go:288,609`, `gate_index.go:192`, `gate_ratchet.go:409`,
  `insights.go:537`, …). `Verify` was a three-line wrapper that called `Read` and
  discarded its records; `GateAuditRecords` sorted a slice by `Seq` that `Read`
  already returns in `Seq` order by construction (it rejects any record whose
  `Seq` is not the expected next one). Deleting them removes no check.

### Remaining scope (unchanged, deliberately still open)

The three items the sweep declined stay as recorded above: `parseInstalledMemoryHigh`
is not dead, the whale coexistence checks are live refusals, and the "three no-op
branches" still cannot be identified without PR #12's own text, which is not in
this repository. Only that last one is genuinely unactionable-pending-information;
it is why this ticket is not closed here.

## The "three no-op branches" identified and resolved (2026-09-05)

PR #12's own text turned out to be retrievable after all: it's committed to this
repo's history at `b3c10af`/`a6c1b64` (`docs/superpowers/reviews/2026-09-04-whole-project-simplification-review.md`),
just not present on the current tree — it was deleted by a later commit, not
lost. `git show a6c1b64:<path>` recovers it in full. The three branches it
named (`check.go:175-178`, `store.go:1503-1507`, `gate_index.go:231-239` at
that revision) were traced forward to their current locations, each
independently re-verified as still present and still a genuine no-op before
touching anything:

1. **`internal/store/check.go`** (`findingIndexDivergence` loop) — `dimension
   := "finding-integrity"` followed by `if finding.Kind == "unevaluated" {
   dimension = "finding-integrity" }`: both branches produce the identical
   string, so the conditional was pure no-op. Collapsed to the one-line form.
   Behaviour-preserving by construction (both arms were already equal).
2. **`internal/store/store.go`** (`RegisterBootstrap`) — a `SELECT 1 FROM
   ejections ...` pre-check whose result (`one`) was never used in either
   branch (success: empty body; `ErrNoRows`: falls through; any other error:
   propagated). Traced whether removing it changes observable behaviour: the
   real tombstone-clearing DELETE happens unconditionally inside
   `registerDB(ctx, true)`'s own transaction regardless of whether a row
   existed (DELETE of 0 or 1 rows is equally correct), and the existing
   `TrimRegistryProject` rollback on `registerDB`'s own failure already
   covers the case this pre-check's early-exit was trying to protect (a real
   DB error surfacing before vs. after the registry-file write makes no
   difference — both paths already roll the write back). Deleted the entire
   dead block.
3. **`internal/store/gate_index.go`** (`_ = baseline` discard in the
   `"baseline"` audit-record branch) — traced this one further before
   touching it, and stopped: `GateBaseline`, `PinGateBaseline`,
   `ShowGateBaseline`, `ResolveGateBaseline`, `baselineFromAuditRecord`,
   `deriveRatchetBaseline`, and the `gate_baselines`/`gate_baseline_active`
   tables this exact code path writes to are **entirely ratchet-exclusive**
   machinery (`deriveRatchetBaseline` references `def.Ratchet.Comparator`
   directly) — not a general gate-baseline concept shared by other kinds.
   This is inside AIRA-78's active scope (delete the ratchet gate kind,
   owner decision 2026-09-05), which a plain grep for the substring
   "ratchet" had missed since this file doesn't contain that word. Relayed
   to AIRA-78's build agent directly rather than fixing it here, to avoid
   two agents editing the same lines concurrently. Not resolved by this
   ticket's own commit — closes when AIRA-78 lands.

Verified: `go build ./...`, `go vet ./...` both clean; full `go test
./internal/store/...` — `ok aira/internal/store 164.627s` (exit 0).

**Ticket stays open** pending AIRA-78 landing item 3. `parseInstalledMemoryHigh`
and the whale coexistence checks remain correctly-declined non-deletions per
the earlier section.
