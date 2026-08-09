# AIRA M5 — typed findings + query (Phase 2)

- **Status:** plan phase (rev 2 — folds orthogonal plan review: Gemini + Sol). Design spec for the plan gate.
- **Date:** 2026-08-09. **Branch:** `codex-aira-m5` off master `1c58ace` (Phase 1 complete).
- **Depends on:** the Finding contract in `2026-08-07-aira-design.md` §3 (line 95), §10, §16; the
  Phase-1 write protocol / layering (tickets, relations) it generalises.
- **Deferred (NOT M5):** `aira review` + review-depth (Phase 3, §16); FTS/`aira grep` (M6); `aira import`
  (M7); `resolves`/`resolved-by` edges *with findings as relation endpoints* (Phase-3-adjacent); durable
  **attestations** (a `waived` finding records a rationale + actor now; the attestation object is later).

## 1. Scope
A typed, queryable **Finding** with `find add|ls|show|set`. **Review findings** are git-durable files in
`.aira/findings/` (authoritative content, like tickets); **reconciliation findings** are DB-resident
(the existing internal writers, now typed and given a read path). Both are one Finding *concept* with a
`subtype`, but they are DIFFERENT shapes (§2) sharing one table + one query path. This milestone also
**generalises the mutation write protocol** to be file-type-dispatched (§4), because it currently
assumes tickets.

## 2. Data model — two subtypes, per-subtype validated constructors (Pike Rule 5)
The existing reconciliation writers (`recordFinding`/`recordRebuildFinding`/`recordScanFinding`) have
only a stable code + subject/details (often a path/root, NOT a ticket, verdict, source, or severity).
So a single "ticket-required, all-enums-required" Finding is WRONG. Model two shapes with a shared
identity envelope:

```
Finding{ Subtype, Key, ...subtype-specific fields }        // one concept, two shapes
```
- **Subtype** — closed enum `FindingSubtype{Review, Reconciliation}`.
- **ReviewFinding** (git-durable) — REQUIRES: `TicketID` (`ValidateID`), `Category` (open kebab token
  `^[a-z0-9]+(-[a-z0-9]+)*$`, incl `flaky-test`), `Severity` (reuse `Severity{P0,P1,P2}`), `Verdict`
  (`Verdict{Confirmed,Refuted,Plausible}`), `Source` (open lowercase token, same shape as Category),
  `Message` (non-empty; **mutable** content). OPTIONAL: `RequirementID` (validated if present),
  `File`+`Line` (**`Line` requires `File`; line is a positive int**). `Disposition`
  (`Disposition{Open,Fixed,Waived}`, default `Open`); **`Waived` REQUIRES non-empty `WaiverReason` +
  `WaiverActor`, and the constructor MUST REJECT waiver fields set when disposition is `Open`/`Fixed`**
  (waiver fields exist iff `Waived`).
- **ReconciliationFinding** (DB-resident) — REQUIRES: a stable `Code` (existing `E_*`) + `Subject` +
  `Message`/details. `TicketID` OPTIONAL (many have only a path/worktree). `Verdict`/`Source`/`Category`
  absent; `Severity` defaulted or absent; `Disposition` fixed to `Open` (reconciliation records are
  system diagnostics, not waivable review debt). Preserve the existing `code/subject/details`.

Two constructors — `NewReviewFinding(...) (Finding, error)` and `NewReconciliationFinding(...)` — are the
ONLY construction paths; each rejects every illegal combination for its shape. Typed enums live in
`domain`. There is one authoritative validation path per subtype; the DB read-back re-validates
(mirroring `leaseFromRow`).

## 3. Review-finding identity key (dedup by construction — content NOT included)
Findings are numerous and re-reported; no id allocator. A review finding's identity is the tuple
**(ticketID, source, category, canonical-file, line, requirementID)** — **one finding per identity
tuple is intended** (a second observation at the same locus/category/source UPDATES the mutable
content: message, severity, verdict, disposition). Message/severity/verdict/disposition are NOT part of
identity, so they are correctable in place.
- **Canonicalise** every identity component: `canonical-file` = repo-relative, forward-slash,
  `path.Clean`ed, no `..`/absolute/backslash/drive (reuse the area-glob containment rule); `line` = the
  decimal of a positive int (empty if no file); `requirementID`/`ticketID` normalised via `ValidateID`;
  `source`/`category` lowercased tokens. Absent optional components are the empty string.
- **Key** = a readable, filesystem-safe id `f-<ticketID>-<source>-<category>-<hash>` where `hash` is a
  collision-resistant digest (≥16 hex, or full digest) of the canonicalised tuple — NOT truncated to a
  length that risks collision. The key is the filename (`.aira/findings/<key>.md`) and the DB row key.
  Pin the exact canonicalisation + key derivation in tests.

## 4. Storage, write protocol (generalised), and index
- **Review findings are git-durable files** `.aira/findings/<key>.md` (frontmatter = the typed fields;
  body = `Message`). **Reconciliation findings have no git file** (DB-authoritative).
- **Generalise the write protocol (required — it is ticket-specific today).** `preparePathMutationEvent`
  is generic, but `materialiseIntent`, the crash replay, and `reconcile` unconditionally parse the intent
  payload as a `Ticket` and touch allocations/tickets/relations. Add an intent **kind** (`ticket-file`
  vs `finding-file`) and DISPATCH: `materialiseIntent`/replay route a `finding-file` intent to a
  finding materialiser (parse finding frontmatter, upsert the finding index row) — never through the
  ticket/allocation path. Keep ONE crash-safe protocol (SQLite→file→journal→atomic-rename), now
  type-aware. Finding add/set emit one journaled event each (§11: "finding … writes" are significant).
- **`find set` rewrites the git file.** Disposition/waiver are review-finding CONTENT → `find set` MUST
  rewrite `.aira/findings/<key>.md` frontmatter through the same journaled write protocol, THEN re-index
  — never a DB-only disposition change (no git/DB drift).
- **The `findings` table = shared index (+ authoritative store for reconciliation rows).** Extend it via
  idempotent additive ALTER migrations (the `ensureAreaHintsGeneration` precedent; each column its own
  `ensure…` helper, idempotent via `PRAGMA table_info`): add `subtype TEXT NOT NULL DEFAULT
  'reconciliation'` (correctly types every legacy row), `worktree_id`, `ticket_id, category, severity,
  verdict, disposition, source, file, line, requirement_id, waiver_reason, waiver_actor, canonical_file,
  message`. Legacy reconciliation rows keep `code/subject/details`; map them into the shared columns.
  **The index key includes `worktree_id`** for review rows (Rebuild scans ALL worktrees; the same review
  key in divergent worktrees must not collide/overwrite — mirror the per-worktree `tickets`/`relations`
  index). Reconciliation rows retain their existing `(project_id, finding_key)` identity + worktree.
- **Rebuild + divergence (apply the M3 F1 lesson).** `Rebuild` scans each worktree's `.aira/findings/*.md`
  and reconstructs that worktree's REVIEW rows (DELETE this project+worktree's `subtype='review'` rows,
  re-insert from files); reconciliation rows are DB-authoritative and preserved. **`check` computes
  review file↔index divergence from the CANONICAL file scan and reports it BEFORE / independent of the
  heal** — because `check` triggers `Rebuild`, a divergence computed after the rebuild is already healed
  and silently lost (the exact M3 F1 trap). A malformed finding file is fail-closed/`unevaluated` (like a
  malformed ticket), never silently dropped. Serialise finding mutations against the rebuild's review-row
  deletion (one `BEGIN IMMEDIATE`).

## 5. Verbs (core.Do dispatch + thin CLI, mirroring `link`'s sub-verb split)
- `aira find add <ticket-id> --category C --severity S --verdict V --source SRC --message "…"
  [--file path:line] [--requirement REQ-ID]` → create/update the review finding (idempotent per identity
  §3); returns the finding + event. `subtype` is always `review` here.
- `aira find ls [<query>] [--by FIELD] [--fields …]` → query findings; default `subtype:review`,
  `subtype:reconciliation` or `subtype:any` widens; 50-row cap + distribution-on-overflow + `--fields`
  via the generic core wrapper.
- `aira find show <id>` → one finding.
- `aira find set <id> --disposition open|fixed|waived [--reason "…"] [--actor NAME]` → rewrite the git
  file's disposition/waiver via the write protocol; `waived` REQUIRES `--reason`
  (`E_WAIVER_REASON_REQUIRED`, exit 2) + records `--actor`; emits an event.
New stable codes: `E_FINDING_INVALID`, `E_WAIVER_REASON_REQUIRED` (exit 2).

## 6. Query — a DISTINCT finding path (do not shoehorn into the ticket matcher)
Selector parsing, `matchesTerms`, `countRows`, and exact `Get` are `TicketRecord`/`domain.Ticket`-specific,
and a finding id is NOT a ticket selector. Add a parallel finding query path:
- `FindingRecord` + `matchesFindingTerms` + `validFindingField`: `subtype, ticket, category, source,
  verdict, disposition, severity, requirement, text` (text = substring over message; NOT FTS → M6). ANDed
  terms, same surface grammar; `subtype:any` special-cases the subtype filter.
- `validFindingDistributionField` (`--by`): `subtype, category, source, verdict, disposition, severity,
  ticket`. **Reuse ONLY the generic core 50-row-cap + distribution + `--fields` WRAPPER** (core.go:270-292),
  not the ticket matcher. `find ls --by category` (recurrence) and `--by source` (reviewer verdict
  ratios) size-before-fetch honestly (no silent truncation).

## 7. Invariants
Git files are the sole authority for review-finding CONTENT; the DB is a rebuildable index (+ authoritative
only for reconciliation rows) — no content split-brain. Illegal findings unconstructible (per-subtype
constructors). Dedup by construction (identity key). One write protocol, type-dispatched. Stable codes,
verdict/exit honesty, 50-row cap + distribution, `--fields`, no-cgo/static all carry over. Findings relate
to tickets by `ticket_id`; the `resolves`-edge integration is deferred.

## 8. Tests / verification plan (TDD)
- Domain: every illegal review finding rejected (bad enum, empty ticket/message, waived-w/o-reason,
  waiver-set-when-not-waived, Line-w/o-File, malformed source/category); reconciliation constructor
  accepts ticket-less code/subject and rejects review-only fields.
- Key/dedup: `find add` twice identical → ONE file + ONE row (content update, not duplicate); changing
  message/severity/verdict → SAME finding (mutable); changing source/category/locus → DISTINCT finding;
  path-spelling variants canonicalise to the same key; pin the derivation; no hash collision on a
  directed adversarial set.
- Write protocol: a `finding-file` intent routes to the finding materialiser (NOT the ticket path);
  add/set create/rewrite `.aira/findings/<key>.md` + one index row + one journal event; a targeted
  crash-hook test if cheap; `find set` rewrites git frontmatter then re-indexes (no DB-only change).
- Rebuild/divergence: rebuild reconstructs per-worktree review rows (deletes stale); two worktrees with
  the same key do NOT collide (worktree_id); `check` reports file/index divergence computed BEFORE the
  heal (revert-check: a hand-divergent index is reported, not silently healed); malformed finding file
  fail-closed.
- Migration: `ALTER … DEFAULT 'reconciliation'` types legacy rows; each column migration idempotent;
  existing findings-table tests + reconciliation writers still pass; reconciliation findings become
  queryable (`subtype:reconciliation`).
- Verbs through `core.Do`: add/ls/show/set parity CLI≡core; distributions `--by category`/`--by source`
  correct + honest overflow (>50); term filters; `subtype:any`.
- Regression: all Phase-1 tests green; M3 lease concurrency `-count=20`; no-cgo static build.

## 9. Expected yield
Review output stops evaporating: a typed, git-durable, queryable Finding store with idempotent `find add`,
`find ls --by category/source` (recurrence + reviewer verdict ratios), `waived`=visible debt with a
rationale, reconciliation findings gaining a read path — over the same honest query/output contract, with
one type-dispatched crash-safe write protocol. Deferrals (review-emission, FTS, import, resolves-edges,
attestations) explicit.
