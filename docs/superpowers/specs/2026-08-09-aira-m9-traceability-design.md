# AIRA M9 — requirements + covers/verifies traceability graph

Status: plan (author: Opus, owner-delegated), revised after Sol plan-review
BLOCK (all P0/P1/P2 absorbed). First milestone of Phase 3. Builds on master
`52ec856`. Correctness-critical (entity-kind-aware allocation touches crash
recovery), so the full two-loop adversarial process applies.

## 1. Scope

M9 makes the **requirement** a first-class git-durable entity and turns the
`covers:`/`verifies:` annotation convention into an enforced — but **advisory** —
traceability graph check. It is Phase 3's data-model foundation.

It builds as **three separately-gated milestones** (Sol-recommended split — the
allocator work is correctness-critical and independent, and must land and be
gated *before* anything depends on it):

- **M9a — entity-kind-aware ID allocation infrastructure** *(gated + merged
  first)*: make allocation kind-aware end to end — durable `kind` in the receipt
  and authenticated event, a kind-tagged prefix registry with a **transactional**
  schema migration/backfill, kind↔path validation, and prefix-kind consistency
  enforced on allocate, rebuild, and import (disagreement ⇒ `E_JOURNAL_CORRUPT`).
  Correctness-critical → full two-loop + DB-loss-rebuild + crash-window
  counterexamples. Built by Opus directly (crash-recovery core).
- **M9b — requirement entity + CRUD + `AR-1..7` import migration** *(after 9a)*:
  the Requirement domain type, store CRUD, and the atomic `req import` seed.
- **M9c — covers/verifies traceability graph check** *(after 9b)*: edge discovery
  + the check dimension + the §3 verdict contract. Sol judged the check folding +
  verdict contract "otherwise sound."

§§3–5 below are the whole design; the **M9a slice is §4.1 plus its tests
(§7.1/1b/5)**. This document is the shared design; each milestone is planned,
gated, and merged on its own.

## 2. Non-goals / explicit deferrals

- **No gates/ratchet/proven-to-fire/attestations** (M10), **no review/
  review-depth** (M11), **no runner** (M12).
- **No blocking enforcement.** Traceability compliance is advisory in Phases 1–3;
  whether a dangling edge ever *blocks a merge* is out of scope.
- **No new annotation syntax.** The scanner reads the existing `covers:`/
  `verifies:` comment convention verbatim.
- **The migration is NOT deferred** (Sol P1): the live tree already has
  `covers: AR-5/6/7`, so without seeded `AR-*` requirement nodes the check would
  be useless (empty→`unevaluated`) or every real annotation would dangle. 9a
  migrates `AR-1..7` with their existing IDs.

## 3. The two failure senses and the verdict contract (Sol P0/P1)

AIRA has two senses of "block" (spec §4.5). M9 traceability is **advisory in
enforcement** (surfaced at `aira check`, never refuses an operation) but
**fail-closed in honesty** (never a vacuous pass). The traceability check
dimension resolves to exactly one verdict per the crisp contract:

| Situation | Code | Dimension | Effect on overall `check` |
|---|---|---|---|
| A `covers:`/`verifies:` annotation targets a requirement ID that does not exist | `E_TRACE_DANGLING` | **fail** | `check` **fails** (a broken reference), but no write is refused |
| An **established** requirement whose status expects an edge lacks `covers` / `verifies` | `W_TRACE_UNCOVERED` / `W_TRACE_UNVERIFIED` | **warning** | overall **stays pass**; a warning is never a fail and never `unevaluated` |
| The scan cannot run/complete, the worktree snapshot is uncertain, **or the requirement registry is empty** | `U_TRACE_UNSCANNED` / `U_TRACE_EMPTY` | **unevaluated** | dimension `unevaluated`; **never a vacuous pass** |
| A requirement **node** file is unreadable / ID-mismatched (its own integrity, not an edge) | `E_REQUIREMENT_INVALID` (node-integrity, a **`fail` finding** — a broken entity, exactly like a malformed ticket) | overall **fail** (any `Findings` entry forces fail, as today — intended) | edges pointing at that node resolve to **`unevaluated`**, **not** `E_TRACE_DANGLING` (Sol P0 honesty) |

Key honesty rules:
- A **dangling edge** (annotation → truly-absent requirement) is a `fail` — the
  traceability analogue of a relation to a non-existent ticket — but unlike M3
  integrity it refuses **no** operation (the annotation lives in code, not an
  AIRA write).
- A **malformed requirement node** is *not* a dangling edge. The node itself is a
  concrete broken entity → an `E_REQUIREMENT_INVALID` **`fail`** finding (forcing
  overall `fail`, exactly as a malformed ticket does; this "any `Findings` entry ⇒
  overall fail" behaviour is intended, Sol P1). Separately, any annotation that
  points at that unreadable node resolves to **`unevaluated`** (we cannot
  establish the node) rather than `E_TRACE_DANGLING`. So: a *missing* node makes
  its edges **dangling/fail**; an *unreadable* node is a node **fail** whose edges
  are **unevaluated** — the two are never conflated.
- **Verdict folding (Sol-endorsed):** one overall precedence `fail > unevaluated >
  pass`, with **independent per-dimension values preserved** (traceability is not
  split into a separate report to avoid masking). **Warnings never flip the
  overall verdict to fail, and are never reported as unevaluated.** Mixed states
  coexist; a mixed-precedence test asserts that an unrelated integrity `fail` and
  a traceability `unevaluated`/`warning` remain simultaneously visible in their
  own dimensions.

### 3.1 Status → edge expectation (Sol P1)

Only some statuses expect edges; the rest are **exempt** (no warning):

| status | expects `covers` | expects `verifies` |
|---|---|---|
| `built` | yes | yes |
| `partial` | yes | no (soft) |
| `designed`, `planned` | no | no |
| `boundary` (deliberately out-of-scope) | exempt | exempt |
| `retired`, `superseded` | exempt | exempt |

`W_TRACE_UNCOVERED`/`W_TRACE_UNVERIFIED` fire only for a status that expects the
missing edge. A `built` requirement with zero `covers` warns; a `planned` one
does not. This table is golden-tested.

## 4. Design — 9a foundation

### 4.1 Entity-kind-aware allocation (Sol P0; correctness-critical)

Today `AllocateID(ctx, prefix)` and the `allocations` table
(`project_id, prefix, number, worktree_id, state, path, seq, …`) assume the
allocation **materialises to a ticket file**: `check` (`check.go:119-149`) reads
each allocation and, if `state='allocated'`, demands a materialised **ticket**
file and parses `path` as a ticket (`E_ID_UNRESOLVED: allocation file contains …`).

M9 adds an **entity kind** to allocation. Crucially, `kind` must be durable
**everywhere the allocation is reconstructed from**, not just the DB column — the
DB is rebuildable from the git-common-dir receipts/journal, so a `kind` that lives
only in the DB would be lost on rebuild and a requirement allocation would come
back as the default ticket (Sol P0 recovery).

- **DB:** add `kind TEXT NOT NULL DEFAULT 'ticket'` to `allocations` (default
  preserves every existing row).
- **Durable receipt + event:** add `Kind` to `AllocationReceipt` (currently
  `{ProjectID, WorktreeID, ID, Path, Seq, At, State}`) and to the allocation
  event, **set at allocate time**. On read, a **missing `Kind` defaults to
  `ticket`** (back-compat for every pre-M9 receipt/event). `ensureReceiptAllocation`
  and `ensureAllocationEvent` (and the `store.go:~1340` reconstruction) carry and
  restore `kind`; DB rebuild from receipts reconstructs the correct kind. **`kind`
  ↔ `path` is validated** at materialise (a `requirement` allocation must
  materialise under `.aira/requirements/`, a `ticket` under `.aira/tickets/`).
- **`AllocateID`** becomes kind-aware (derives kind from the prefix registry, see
  below; existing callers unchanged). The materialisation update carries the kind.
- **`check`'s allocation verification** branches on kind: a `kind='requirement'`
  allocation must materialise to `.aira/requirements/<ID>.md` parsed as a
  **requirement**; the "no materialised ticket file" / `E_ID_UNRESOLVED` messages
  become kind-correct.

**Namespace / prefix registry (Sol P0 namespace).** Allocation identity stays
`(project, prefix, number)`; kind is a **property of the prefix**, because
**prefixes are disjoint by kind** — a prefix belongs to exactly one kind. The
current ticket-oriented registry (`s.prefixes map[string]bool`, the
`prefix_ownership` table, and the registry entry's `Prefixes []string`) becomes
**kind-tagged**: each registered prefix records its kind, a prefix may not be
re-registered under a different kind, and `AllocateID` looks up the kind from the
prefix. The migration **registers `AR` as a `requirement` prefix** and seeds its
high-water so `req add` yields `AR-8` (a kind column alone does not register the
prefix — Sol).

**Kind consistency + authority (Sol P0).** The **prefix registry is the
authority** for a prefix's kind. `kind` is redundantly present on the receipt,
event, and DB row *for recovery*, and is **included in the event's authenticated
payload/digest** so a lost/tampered kind is detectable. On any read/rebuild, the
kind from prefix-registry, receipt, event, and path are **cross-validated**; a
disagreement is **`E_JOURNAL_CORRUPT`** — the code never silently picks a winner.
Legacy missing-kind ⇒ `ticket` is applied **only for prefixes still registered as
`ticket`**; a missing-kind receipt for a prefix registered `requirement` is a
conflict (`E_JOURNAL_CORRUPT`), not a silent downgrade.

**Transactional schema migration (Sol P0; M5 F1 class).** Kind-tagging needs an
**explicit transactional schema migration + backfill** for **both** the
`prefix_ownership` table **and the `allocations` table** — `CREATE TABLE IF NOT
EXISTS` does **not** add a column to an existing table, so a bare `DEFAULT
'ticket'` never reaches pre-M9 rows in *either* table, and an existing DB with
allocated tickets would otherwise fail or lose kind semantics on the
`allocations` side. The migration, in one transaction, adds the `kind` column to
both tables, backfills existing rows to `ticket`, and is crash-atomic (the exact
M5 F1 lesson). The durable registry breadcrumb encodes `Prefixes []string`; add a
**legacy decoder** (old = bare strings ⇒ `ticket`; new = kind-tagged) plus a
migration test, or DB-loss recovery loses prefix kind.

**Recovery invariant enforced everywhere (Sol P1).** Prefix-kind consistency is
enforced not only in `AllocateID` but during **DB rebuild** and **imported-
allocation registration** too, because allocation identity stays
`(project, prefix, number)`; a manually-introduced or recovered mismatched
allocation must be caught (`E_JOURNAL_CORRUPT`), not pass through.

This is the `make id`→`aira id` generalisation the repo flagged; it is
correctness-critical (allocation/recovery/materialisation/prefix-ownership) →
adversarial verification with durable crash-window counterexamples incl. a
**DB-loss-then-rebuild** test that asserts a requirement allocation returns as a
requirement, not a ticket (mirrors the M5 F1 migration work).

### 4.2 Requirement entity (`internal/domain`)

`Requirement{ID, Text, Status}` + validated `NewRequirement` (mirrors
`NewReviewFinding`): closed `Status` enum {built, partial, designed, planned,
boundary, retired, superseded}, non-empty text, ID shape validated against the
registered `AR` prefix. Git file `.aira/requirements/<ID>.md` with **JSON
frontmatter inside `---`** (Sol P2 — the repo format, *not* YAML), reusing the
existing ticket/finding frontmatter reader (`internal/store/query.go:152` style)
and a `ParseRequirement` mirroring `ParseTicket`. **Filename↔`id` identity is
enforced** (the file `AR-3.md` must contain `id: "AR-3"`).

### 4.3 Store + migration (`internal/store`)

- `AddRequirement`/`GetRequirement`/`ListRequirements`/`SetRequirement`, each
  with an event/receipt, project+worktree-scoped index, reconciler rebuild from
  files (mirrors findings).
- **Migration:** `aira req import <REQUIREMENTS.md>` (a deterministic one-time
  seed, mirroring `aira import` for findings) parses the seed table and, for each
  `AR-N`, materialises `.aira/requirements/AR-N.md` **preserving the ID** and
  registers a `kind='requirement'` allocation at number N so the high-water mark
  advances (next `aira req add` → `AR-8`). **It uses the same atomic
  file/outbox/allocation/receipt/journal protocol as ticket creation** (Sol P1),
  so a crash mid-import can never leave (i) a materialised requirement file with
  no durable allocation evidence, or (ii) an allocation whose receipt/journal
  lost its `kind`. Idempotent (re-import of an unchanged row is a no-op; a changed
  row is surfaced, never silently overwritten). Import-specific crash-window tests
  cover the die-points; allocator-unit tests do not. This is the only way existing
  IDs enter — `req add` alone cannot recreate `AR-1..7`.

### 4.4 The `aira req` verb (`internal/core`)

Grouped verb `req` with operations `add|ls|show|set|import`, MCP tool
`aira_req`, discriminator `subverb` — modelled on `find` so it inherits the M8
descriptor-generated MCP + Skill faces, the arg-accessor drift test, and Skill↔
CLI parity with zero bespoke wiring. `add` allocates via §4.1; `set` is a
validated status transition.

## 5. Design — 9b traceability check

### 5.1 Edge discovery (code-only, coherent snapshot; Sol P1)

Edges are **discovered from code annotations only**, never stored on the
requirement node (no second registry to drift). The scanner:
- reuses the **same tracked-file git snapshot** the ticket reconciler already
  uses, so it shares freshness/scope/exclusion semantics and is a single coherent
  view; on any snapshot uncertainty (scan error, concurrent mutation detected,
  file unreadable) it yields `U_TRACE_UNSCANNED`/`unevaluated` rather than a
  partial result;
- matches `covers:`/`verifies:` only in a comment position, parses one-or-many
  comma-separated IDs with whitespace tolerance, and must **not** match the token
  inside a string literal — a tested fixture set (single, multi-ID, whitespace,
  string-literal-non-match, non-annotation lines) pins this deterministically.

### 5.2 Resolution + check integration (`internal/store/check.go`)

A new **traceability dimension** in `CheckReport` emits the §3 codes. It resolves
each discovered edge against the established requirement set, applies the §3.1
status table, and folds into the existing verdict precedence. An **empty
requirement registry** → `U_TRACE_EMPTY`/`unevaluated` (the exact Phase-0
"empty graph must not pass vacuously" concern), asserted load-bearing.

## 6. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Allocator generalisation corrupts the ticket allocation/recovery path | `kind` defaults to `ticket`; existing rows/callers unchanged; crash-window counterexamples for a `requirement` allocation that dies pre/post materialisation, reproduced end-to-end (two-loop). |
| Empty/unscannable graph passes vacuously | Empty registry → `U_TRACE_EMPTY`; scan/read/snapshot uncertainty → `U_TRACE_UNSCANNED`; both asserted not-pass. |
| Malformed requirement node reported as a dangling edge | Node integrity (`E_REQUIREMENT_INVALID`) is distinct from edge dangling; an annotation to an unreadable node → `unevaluated`, not `fail` (tested). |
| Warning wrongly flips overall check to fail / labelled unevaluated | Verdict precedence test with a mixed fail+warning+unevaluated run asserts warnings are orthogonal. |
| Migration can't preserve `AR-1..7` / breaks the live annotations | `req import` seeds nodes with preserved IDs + advances the allocator high-water; without it the graph is empty/unevaluated (never a false pass); idempotent, never silent-overwrite. |
| Frontmatter format drift (YAML vs the repo's JSON-in-`---`) | Reuse the existing frontmatter reader; filename↔id identity enforced. |
| Edges drift from a stored copy | Edges discovered from code only; never stored on the node. |
| Traceability wrongly refuses an operation | Advisory-only; a test asserts a dangling annotation does not refuse `req`/ticket writes — only `check` surfaces it. |
| Hand-picked requirement IDs | `req add` allocates via the kind-aware allocator; a spy asserts no hand-picked path. |

## 7. Tests (TDD; every confirmed counterexample becomes a regression test)

**9a**
1. Kind-aware allocation: a `requirement` allocation materialises to
   `.aira/requirements/<ID>.md` and `check` accepts it; a ticket allocation is
   unaffected; **crash-window** counterexamples (die between allocate and
   materialise, both directions) recover correctly — reproduced end-to-end.
1b. **Durable-kind recovery (Sol P0):** allocate a requirement ID, **drop the DB
   and rebuild from receipts/journal**, and assert the allocation returns as
   `requirement` (not the default ticket); a pre-M9 receipt with no `kind` field
   for a *ticket*-registered prefix rebuilds as `ticket` (back-compat); `kind`↔
   `path` mismatch is rejected.
1c. **Transactional schema migration (Sol P0; M5 F1 class):** an existing pre-M9
   DB — with **both** a populated `prefix_ownership` (no kind column, bare-string
   registry breadcrumbs) **and a populated `allocations` table of allocated
   tickets** (no kind column) — upgrades atomically: the `kind` column is added +
   existing rows backfilled to `ticket` in **both** tables in one transaction, the
   legacy `[]string` breadcrumb decodes to `ticket`, and a crash mid-migration
   leaves a recoverable DB (reproduce both migration crash windows, for both
   tables).
1d. **Kind-consistency / `E_JOURNAL_CORRUPT`:** a receipt/event/DB/path kind that
   disagrees with the prefix registry is rejected as `E_JOURNAL_CORRUPT` (no
   silent winner) — asserted at `AllocateID`, at DB rebuild, and at imported-
   allocation registration; a missing-kind receipt for a `requirement`-registered
   prefix is a conflict, not a silent ticket downgrade.
2. Requirement entity round-trip: `NewRequirement` validates the status enum +
   non-empty text + ID shape; `ParseRequirement`∘render == identity with
   **JSON-in-`---`** frontmatter; malformed frontmatter and filename↔id mismatch
   are refused writes.
3. Store CRUD + index + reconcile rebuild; status transition validates the enum.
4. Migration: `req import REQUIREMENTS.md` seeds `AR-1..7` with preserved IDs,
   advances the allocator (next `req add` → `AR-8`), is idempotent, never
   silently overwrites a changed row, and — **import-specific crash-window tests
   (Sol P1)** — a crash at each die-point leaves no materialised requirement
   without durable allocation evidence and no allocation whose receipt/journal
   lost `kind`.
5. Allocator namespace/path: ticket and requirement prefixes are **disjoint by
   kind** (a prefix cannot be re-registered under a different kind); allocation
   `path`/`kind` are correct; a spy asserts `req add` uses the allocator (no
   hand-picked ID).

**9b**
6. Edge discovery: fixture set (single, multi-ID, whitespace, string-literal
   non-match, non-annotation lines) → exactly the expected edges, deterministic.
7. Dangling: annotation → absent requirement ⇒ `E_TRACE_DANGLING`, `check`
   verdict **fail**, reproduced end-to-end via `Run`.
8. Uncovered/unverified honour §3.1: `built` w/o covers ⇒ `W_TRACE_UNCOVERED`
   (warning, overall pass); `planned`/`boundary`/`retired` w/o edges ⇒ **no**
   warning.
9. Honesty: empty registry ⇒ `U_TRACE_EMPTY`/`unevaluated` (proven not-pass);
   injected scan failure ⇒ `U_TRACE_UNSCANNED`/`unevaluated`.
10. Malformed-node resolution: an annotation to a requirement whose file is
    unreadable/ID-mismatched ⇒ `unevaluated` + `E_REQUIREMENT_INVALID`, **not**
    `E_TRACE_DANGLING`.
11. Mixed precedence (Sol): a run holding an **unrelated integrity fail** (e.g. a
    duplicate-id in a different dimension), a `built`-uncovered traceability
    warning, and a traceability `unevaluated` ⇒ overall **fail** by precedence,
    but **all three dimension values remain independently visible** (the
    traceability warning/unevaluated is neither masked by nor reclassified as the
    unrelated fail).
12. Advisory, not integrity: a dangling annotation does not refuse a `req`/ticket
    write; only `check` surfaces it.

**Both**
13. Face generation: `aira_req` MCP tool + Skill actions generated from the
    descriptor (M8 golden/drift/parity extend to `req` with zero bespoke wiring).
14. Build constraint: static/no-cgo; scan needs no daemon/network.

## 8. Expected yield

Requirements become a real, allocator-issued, git-durable entity with a
kind-aware allocator that no longer assumes tickets; the `AR-1..7` seed is
migrated so the existing annotations resolve; and the `covers:`/`verifies:`
convention becomes an honest advisory graph check: broken references fail
`check`, status-appropriate gaps warn, unreadable nodes and empty/unscannable
graphs read `unevaluated` — never a vacuous green. This is the foundation for
M10 gates and M11 review-depth, and lets AIRA answer "what is untraced?" over its
own codebase.

## 9. Out-of-scope confirmation

M9 adds no gates, attestations, ratchets, review emission, runner, or blocking
enforcement. Its only new store behaviour is the kind-aware allocator, the
requirement entity + migration, and the read-only traceability scan folded into
`check`. Those extensions remain M10–M12.
