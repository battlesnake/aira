# AIRA M5 — typed findings + query (Phase 2)

- **Status:** plan phase; design spec for plan review + plan gate.
- **Date:** 2026-08-09. **Branch:** `codex-aira-m5` off master `1c58ace` (Phase 1 complete).
- **Depends on:** the Finding contract in `2026-08-07-aira-design.md` §3 (line 95), §10, §16; the
  Phase-1 write protocol / layering (tickets, relations) it mirrors.
- **Deferred to later milestones (NOT M5):** `aira review` + review-depth (Phase 3, §16); FTS/`aira grep`
  (M6); `aira import` (M7); `resolves`/`resolved-by` edges *involving findings* as relation endpoints
  (needs finding ids as relation endpoints — Phase-3-adjacent); full **attestations** (a `waived`
  finding records a rationale + actor now; the durable attestation object is Phase 3/4).

## 1. Scope
M5 makes review output survive by construction: a **typed, queryable Finding** entity with `find add`,
`find ls`, `find show`, `find set`. Review findings are **git-durable** files in `.aira/findings/`
(authoritative content, like tickets); reconciliation findings are **DB-resident** (the existing
internal writers, now typed); **both share one Finding schema** and one rebuildable `findings` index.

## 2. Data model (Pike Rule 5 — the crux; make illegal states unrepresentable)
`internal/domain` gains a `Finding` with a validated constructor. Fields (spec §3 line 95):
- **Subtype** — closed enum `FindingSubtype{Review, Reconciliation}`.
- **Key/ID** — a stable, content-derived key (see §3); the filename of a review finding and the DB key.
- **TicketID** — required, `ValidateID`-checked (the finding is about a ticket).
- **Category** — OPEN vocabulary, kebab token `^[a-z0-9]+(-[a-z0-9]+)*$` (incl. `flaky-test`). Validate
  SHAPE, not membership.
- **Severity** — reuse the closed `Severity{P0,P1,P2}` enum.
- **Verdict** — closed enum `Verdict{Confirmed, Refuted, Plausible}`.
- **Source** — OPEN registered vocabulary, lowercase token `^[a-z0-9]+(-[a-z0-9]+)*$`
  (`codex|fable|gemini|deepseek|semgrep|human|aira|…`), extensible without a spec edit. Validate shape.
- **Disposition** — closed enum `Disposition{Open, Fixed, Waived}`; `Waived` REQUIRES a non-empty
  `WaiverReason` + `WaiverActor` (accepted-debt rationale; the durable attestation is deferred, but a
  waiver without a recorded reason must be unconstructible).
- **Optional:** `RequirementID` (validated if present), `File` + `Line` (`file:line` locus), `Message`
  (the finding text; required and non-empty for a review finding).
Typed enums are validated in `domain`; a constructor `NewReviewFinding(...) (Finding, error)` /
`NewReconciliationFinding(...)` rejects every illegal combination (empty ticket, bad enum, waived
without reason, malformed source/category). Reconciliation findings carry a stable `Code` (the
existing `E_*` codes) and no git file. There is ONE authoritative validation path.

## 3. Finding key (dedup by construction)
No id allocator (findings are numerous; re-reporting must not duplicate). The key is content-derived
and idempotent:
- **Review finding key** = `digest(subtype "review" | ticketID | source | category | file | line |
  requirementID | normalize(message))` rendered as a stable, filesystem-safe id
  `f-<ticketID>-<source>-<category>-<hash8>` (readable + unique; `hash8` = first 8 hex of the digest of
  the locus+message). Re-adding the same logical finding (same key) UPDATES the file in place
  (idempotent); a different source or category or locus is a distinct finding (so `confirmed`-vs-
  `refuted` per source and per-category recurrence are honest). Decide the exact normalize() (trim +
  collapse whitespace; the message is part of identity so two genuinely different messages are two
  findings) and pin it in tests.
- **Reconciliation finding key** = the existing `finding_key` scheme (`reconcile:…`, `rebuild:…`,
  `scan:…`), unchanged.

## 4. Storage + write protocol
- **Review findings are git-durable files** `.aira/findings/<key>.md`: frontmatter carries the typed
  fields (subtype, ticket, category, severity, verdict, source, disposition, waiver, requirement, file,
  line), body carries the message. Written through the EXISTING crash-safe write protocol
  (`preparePathMutationEvent`/`materialiseIntent`, SQLite→file→journal, atomic rename) exactly as
  tickets/relations — reuse it, do not invent a second write path. Finding writes ARE journaled
  (§11: "finding/link/relation writes" are significant mutations); emit one event per add/set.
- **The `findings` table becomes the shared, rebuildable index** (and the authoritative store for
  reconciliation findings, which have no git file). Extend it via additive ALTER migrations (the
  `ensureAreaHintsGeneration` precedent — no `schema_version` table): add
  `subtype, ticket_id, category, severity, verdict, disposition, source, file, line, requirement_id,
  waiver_reason, waiver_actor, canonical_file, message`. The existing reconciliation writers
  (`recordFinding`/`recordRebuildFinding`/`recordScanFinding`) set `subtype='reconciliation'` and the
  fields they have; review findings set `subtype='review'` + `canonical_file`. Existing findings-table
  tests must still pass.
- **Rebuild** scans `.aira/findings/*.md` and rebuilds the review rows (DELETE the project's review
  rows, re-insert from files) — mirroring the relation-index rebuild; reconciliation rows are
  DB-authoritative and preserved. `check` reports a review-index/file divergence (mirroring
  `E_RELATION_INDEX_DIVERGENCE`). Canonical git files are authoritative; the table is a rebuildable hint.

## 5. Verbs (core.Do dispatch + thin CLI, mirroring `link`'s sub-verb split)
- `aira find add <ticket-id> --category C --severity S --verdict V --source SRC --message "…"
  [--file path:line] [--requirement REQ-ID]` → create/update the review finding; returns the finding +
  event. subtype is always `review` here (reconciliation findings are internal-only).
- `aira find ls [<query>] [--by FIELD] [--fields …]` → query findings; 50-row cap + distribution-on-
  overflow + `--fields` reuse the core wrapper. Default lists review findings; `subtype:reconciliation`
  or `subtype:any` widens.
- `aira find show <id>` → one finding (git body + fields).
- `aira find set <id> --disposition open|fixed|waived [--reason "…"] [--actor NAME]` → change
  disposition; `waived` REQUIRES `--reason` (E_ARGUMENT/E_WAIVER_REASON_REQUIRED otherwise); records the
  actor; emits an event. (This is the visible-debt lifecycle.)
Add stable codes as needed (e.g. `E_FINDING_INVALID`, `E_WAIVER_REASON_REQUIRED`, exit 2). A malformed
finding file surfaced by scan is `unevaluated`/fail-closed like a malformed ticket — reuse that
discipline; do NOT silently drop it.

## 6. Query fields + distributions
- `validFindingField`: `subtype, ticket, category, source, verdict, disposition, severity, requirement,
  text` (text = substring over message; NOT FTS, that's M6). ANDed terms, same grammar as tickets.
- `validFindingDistributionField` (`--by`): `subtype, category, source, verdict, disposition, severity,
  ticket`. `find ls --by category` (recurrence) and `--by source` (reviewer verdict ratios) are the
  headline queries and MUST size-before-fetch honestly (no silent truncation).

## 7. Invariants
- Git files are the sole authority for review-finding CONTENT; the DB is a rebuildable index +
  the authoritative store only for reconciliation findings. No content split-brain (mirror tickets).
- Illegal findings are unconstructible (typed enums + validated constructor + `waived⇒reason`).
- Dedup is by construction (content key), so `find add` is idempotent per logical finding.
- Stable codes, verdict/exit honesty, the 50-row cap + distribution, `--fields`, and no-cgo/static
  all carry over unchanged from Phase 1.
- Findings relate to tickets by `ticket_id`; the `resolves`/`resolved-by` EDGE integration is deferred.

## 8. Tests / verification plan (TDD)
- Domain: every illegal Finding is rejected by the constructor (bad enum, empty ticket, waived w/o
  reason, malformed source/category); each is a case.
- Key/dedup: `find add` twice with identical inputs → ONE file, ONE row (update, not duplicate);
  changing source/category/locus/message → a distinct finding. Pin the key derivation.
- Write protocol: a review finding creates one `.aira/findings/<key>.md` + one index row + one journal
  event; crash-safety reuses the ticket path (a targeted crash-hook test if cheap).
- Rebuild/divergence: rebuild reconstructs review rows from files (deletes stale); `check` reports
  file/index divergence; a malformed finding file is fail-closed (not silently dropped).
- Verbs through `core.Do`: add/ls/show/set parity CLI≡core; `find set --disposition waived` without
  `--reason` is rejected; disposition transitions emit events.
- Query: `--by category`/`--by source` distributions correct + honest overflow (>50); term filters
  (subtype/ticket/verdict/etc.); reconciliation findings become queryable (`subtype:reconciliation`).
- Regression: existing findings-table tests + all Phase-1 tests stay green; M3 lease concurrency
  `-count=20` reliable; no-cgo static build.

## 9. Expected yield
`find add|ls|show|set` + a typed, git-durable, queryable Finding store: review output stops
evaporating, `find ls --by category/source` answers "which mistake classes recur" and reviewer
verdict ratios, `waived` is visible debt with a rationale, and reconciliation findings gain a read
path — all over the same honest query/output contract. Deferrals (review-emission, FTS, import,
resolves-edges, attestations) are explicit.
