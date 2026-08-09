# AIRA M9 — requirements + covers/verifies traceability graph

Status: plan (author: Opus, owner-delegated), revised after Sol plan-review
BLOCK (all P0/P1/P2 absorbed). First milestone of Phase 3. Builds on master
`52ec856`. Correctness-critical (entity-kind-aware allocation touches crash
recovery), so the full two-loop adversarial process applies.

## 1. Scope

M9 makes the **requirement** a first-class git-durable entity and turns the
`covers:`/`verifies:` annotation convention into an enforced — but **advisory** —
traceability graph check. It is Phase 3's data-model foundation.

It builds in **two phases** (each gated), because the allocator work is
correctness-critical and independent of the scan:

- **9a — foundation:** make the ID allocator entity-kind-aware, add the
  Requirement entity + store CRUD + index + reconcile, and **migrate the seed
  `AR-1..7` requirements** into `.aira/requirements/` with preserved IDs.
- **9b — traceability check:** discover `covers:`/`verifies:` edges from code and
  fold a traceability dimension into `aira check` with the honest tri-state
  verdict contract.

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
| A requirement **node** file is unreadable / ID-mismatched (its own integrity, not an edge) | `E_REQUIREMENT_INVALID` (node-integrity) + the node is treated as **unestablished** | node → **unevaluated**, not "missing" | that node's edges resolve to `unevaluated`, **not** `E_TRACE_DANGLING` (Sol P0 honesty) |

Key honesty rules:
- A **dangling edge** (annotation → truly-absent requirement) is a `fail` — the
  traceability analogue of a relation to a non-existent ticket — but unlike M3
  integrity it refuses **no** operation (the annotation lives in code, not an
  AIRA write).
- A **malformed requirement node** is *not* a dangling edge. An annotation that
  points at a requirement whose *file* is unreadable/ID-mismatched resolves to
  `unevaluated` (we cannot establish the node), and the node emits its own
  `E_REQUIREMENT_INVALID`. "Missing node" (`fail`) and "unreadable node"
  (`unevaluated`) are distinguished.
- **Warnings never flip the overall verdict to fail, and are never reported as
  unevaluated.** Mixed states coexist (a run can hold a fail edge, a warning, and
  an unevaluated node simultaneously); the overall verdict follows the existing
  `check` precedence (any fail → fail; else any unevaluated → surfaced as
  unevaluated dimension; warnings are orthogonal), asserted by a mixed-precedence
  test.

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

M9 adds an **entity kind** to allocation:
- Add a `kind TEXT NOT NULL DEFAULT 'ticket'` column to `allocations` (default
  preserves every existing row and back-compat).
- `AllocateID` becomes kind-aware (`AllocateID(ctx, kind, prefix)`, with a
  `kind='ticket'` shim for existing callers, or a dedicated
  `AllocateRequirementID`); the materialisation update and the crash-recovery
  reconstruction (`store.go:~1340`) carry the kind.
- `check`'s allocation verification branches on kind: a `kind='requirement'`
  allocation must materialise to `.aira/requirements/<ID>.md` parsed as a
  **requirement** (not a ticket); the "no materialised ticket file" /
  `E_ID_UNRESOLVED` messages become kind-correct.

This is the `make id`→`aira id` generalisation the repo flagged; it is
correctness-critical (allocation/recovery/materialisation) → adversarial
verification with durable crash-window counterexamples (mirrors the M5 F1
migration work).

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
  `AR-N`, writes `.aira/requirements/AR-N.md` **preserving the ID** and registers
  a `kind='requirement'` allocation at number N so the high-water mark advances
  (next `aira req add` → `AR-8`). Idempotent (re-import of an unchanged row is a
  no-op; a changed row is surfaced, never silently overwritten). This is the only
  way existing IDs enter — `req add` alone cannot recreate `AR-1..7`.

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
2. Requirement entity round-trip: `NewRequirement` validates the status enum +
   non-empty text + ID shape; `ParseRequirement`∘render == identity with
   **JSON-in-`---`** frontmatter; malformed frontmatter and filename↔id mismatch
   are refused writes.
3. Store CRUD + index + reconcile rebuild; status transition validates the enum.
4. Migration: `req import REQUIREMENTS.md` seeds `AR-1..7` with preserved IDs,
   advances the allocator (next `req add` → `AR-8`), is idempotent, and never
   silently overwrites a changed row.
5. Allocator namespace/path: requirement and ticket prefixes do not collide;
   allocation `path`/`kind` are correct; a spy asserts `req add` uses the
   allocator (no hand-picked ID).

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
11. Mixed precedence: a run holding a dangling `fail`, a `built`-uncovered
    warning, and an unreadable-node `unevaluated` ⇒ overall **fail**, warning not
    reclassified, unevaluated surfaced.
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
