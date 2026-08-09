# AIRA M9 — requirements + covers/verifies traceability graph

Status: plan (author: Opus, owner-delegated). First milestone of Phase 3
(traceability + gates + runner-lite). Builds on Phase 2 (merged to master
`52ec856`).

## 1. Scope

M9 makes the **requirement** a first-class git-durable entity and turns the
existing `covers:`/`verifies:` annotation convention into an enforced — but
**advisory** — traceability graph check. It is the data-model foundation the
rest of Phase 3 (gates, review-depth) builds on, so the data model is weighted
heaviest.

Today (Phase 0 convention): a manual `REQUIREMENTS.md` table holds `AR-N`
requirement IDs and statuses; code carries `// covers: AR-5, AR-6` and tests
carry `// verifies: AR-5`. The enforcing graph check was deliberately deferred
to Phase 3 (an empty graph must not pass vacuously — `REQUIREMENTS.md:11`).

M9 delivers:

1. **A Requirement entity** — git-durable, one file per requirement in
   `.aira/requirements/<ID>.md` (consistent with tickets and review findings;
   one file per entity is merge-safe by construction). Fields: `id` (own prefix,
   e.g. `AR`), `text`, `status` ∈ {`built`, `partial`, `designed`, `planned`,
   `boundary`, `retired`, `superseded`}. The requirement file is the **node**;
   `covers`/`verifies` **edges are not stored on it** — they live in the code as
   annotations and are discovered by scanning, exactly as the convention already
   works. IDs are allocated through the existing `aira id <prefix>` allocator
   (no hand-picked IDs).

2. **The traceability graph check** — a pass that scans tracked source for
   `covers: <ID>[, <ID>…]` and tracked tests for `verifies: <ID>[, <ID>…]`,
   resolves each edge against the requirement registry, and reports:
   - a `covers`/`verifies` edge whose target requirement does not exist
     (**dangling edge**);
   - a requirement with no `covers` edge (**uncovered**) or no `verifies` edge
     (**unverified**);
   - a `built`/`partial` requirement whose edge set contradicts its status
     (e.g. `built` with zero `covers`).

   The check is **fail-closed in the honesty sense and advisory in enforcement**
   (spec §4.5/§9/§15, lines 30/163/207): a graph it cannot establish (scan
   error, unreadable file) reads `unevaluated`, and an **empty graph is never a
   vacuous pass**; but a dangling/uncovered edge is a **warning / `unevaluated`
   surfaced at `aira check`**, not an integrity refusal — traceability edges may
   dangle transiently mid-refactor. This is distinct from the ticket/relation
   graph, which is fail-closed *integrity* (M3).

3. **The `aira req` face** — `req add|ls|show|set` (grouped like `find`), plus
   the traceability report folded into `aira check` (and queryable). Generated
   through `core.Do` and surfaced on all three faces via the M8 dispatch
   descriptors (a new grouped verb → one MCP tool + Skill actions, automatically).

## 2. Non-goals / explicit deferrals

- **No gates, no ratchet, no proven-to-fire, no attestations** — that is M10.
- **No `review`/review-depth** — that is M11. **No runner** — M12.
- **No blocking enforcement.** Traceability compliance is advisory in Phases 1–3;
  whether a dangling edge ever *blocks a merge* follows the later
  advisory→hardening path and is out of scope.
- **No migration of the seed `REQUIREMENTS.md` table into `.aira/requirements/`
  as a silent data change.** M9 provides `aira req add` (allocating via `aira id`)
  and an `aira req import` path is **deferred**; the seed remains the human
  registry until requirements are deliberately created. (`REQUIREMENTS.md` is a
  rendered/seed doc, not the DB authority — mirrors `BACKLOG.md`.)
- **No new annotation syntax.** The scanner reads the existing
  `covers:`/`verifies:` comment convention verbatim; it does not invent tags.
- **The one-file-per vs single-registry question (spec §21) is decided here as
  one-file-per** for merge-safety/consistency; this is a deliberate, reviewed
  choice, not left open.

## 3. Invariants

1. **One authority.** Requirement *content* is the git file under
   `.aira/requirements/`; the DB is a rebuildable index (reconciler rebuilds it),
   exactly as for tickets/findings. No authoritative content in the DB.
2. **IDs via the allocator.** Requirement IDs come from `aira id <prefix>`; M9
   never hand-picks an ID. The requirement prefix is registered like a ticket
   prefix.
3. **Edges are discovered, not stored.** The graph edges come from scanning the
   `covers:`/`verifies:` annotations in tracked files; the requirement node file
   does not duplicate them (no second registry to drift).
4. **Fail-closed honesty.** The graph check reads `unevaluated` when it cannot
   establish its result (scan/read error) and **never reports a vacuous pass on
   an empty or unscannable graph**. A resolved-clean graph reports `pass`; a
   graph with dangling/uncovered/unverified/status-contradicting edges reports
   the specific advisory findings and an overall non-pass (`fail` for a dangling
   edge — a broken reference — vs advisory warning/`unevaluated` for
   uncovered/unverified, per §4.5 below).
5. **Advisory, not integrity.** No requirement or annotation state refuses a
   ticket/finding/relation operation. The traceability findings surface at
   `aira check`; they do not block edits (distinct from the fail-closed
   ticket/relation integrity graph).
6. **Reconciler-safe.** A requirement write leaves a rebuildable index; the
   reconciler is the safety net. A requirement file with invalid frontmatter is a
   refused *write* (integrity of the entity itself) but a *surfaced* check finding
   if it appears out-of-band — never a silent skip.
7. **Determinism + stable codes.** The scan is deterministic (sorted file
   iteration); every check finding carries a stable code (`E_*`/`W_*`/`U_*`) and
   the report distinguishes `pass`/`fail`/`unevaluated` exactly as `aira check`
   already does.

## 4. Design

### 4.1 Requirement entity (`internal/domain`)

`Requirement{ID, Text, Status}` with a validated constructor
(`NewRequirement`) mirroring `NewReviewFinding`: closed `Status` enum, non-empty
text, ID shape validated against the registered prefix. The git file is
`.aira/requirements/<ID>.md` with YAML frontmatter (`id`, `status`) + the text
as the body — parsed by a `ParseRequirement` mirroring `ParseTicket`. (The
Phase-1 `ParseTicket` EOF-trailing-content hardening, task #8, is a *shared*
parser concern; if the requirement parser reuses the ticket front-matter reader
it inherits any fix — noted, not conflated into M9.)

### 4.2 Store (`internal/store`)

- `AddRequirement`, `GetRequirement`, `ListRequirements`, `SetRequirement`
  (status transition), each with an event/receipt like tickets/findings,
  project+worktree-scoped in the index.
- `TraceabilityGraph(ctx)` — scans tracked files (via the existing git-scan
  path used by `check`) for `covers:`/`verifies:` annotations, resolves against
  the requirement set, and returns a structured result: per-requirement
  `{covered bool, verified bool}` and a list of dangling edges
  `{file, line, kind, target}`. Uses the same scan infrastructure as the ticket
  reconciler so it shares freshness/error semantics; a scan error →
  `unevaluated`.

### 4.3 Check integration (`internal/store/check.go`)

Fold a **traceability dimension** into `CheckReport` (a new dimension key like
the existing lease/area-overlap dimensions). It emits:
- `E_TRACE_DANGLING` (**fail**) — a `covers:`/`verifies:` annotation targets a
  requirement that does not exist (a broken reference is a real defect, but still
  advisory — it does not refuse any operation; it fails the *check*, matching how
  duplicate-ID is a check `fail`).
- `W_TRACE_UNCOVERED` / `W_TRACE_UNVERIFIED` (**warning**) — a requirement with
  no `covers`/`verifies` edge (advisory; a mid-refactor or not-yet-built
  requirement).
- `U_TRACE_UNSCANNED` (**unevaluated**) — the scan could not run/complete.

An **empty requirement set** yields the traceability dimension `unevaluated`
(nothing to establish), **never a vacuous pass** — the exact Phase-0 concern.

### 4.4 The `aira req` verb (`internal/core`)

Grouped verb `req` with operations `add|ls|show|set`, MCP tool `aira_req`,
discriminator `subverb` — modelled on `find` so it inherits the M8 descriptor
generation, the arg-accessor drift test, and the Skill/MCP faces for free. `set`
performs a status transition (validated closed enum). The traceability report is
reachable via `aira check` (folded dimension) and, optionally, a read-only
`req ls --trace`/projection that surfaces `covered`/`verified` per requirement.

### 4.5 The two failure senses (explicit)

A **dangling** `covers:`/`verifies:` (annotation → missing requirement) is a
`fail` of `aira check` — it is a concrete broken reference, the traceability
analogue of a relation to a non-existent ticket, *except* it does not refuse the
write that introduced it (the annotation lives in code, not in an AIRA
operation). An **uncovered/unverified** requirement is a `warning` +
`unevaluated`-leaning advisory: it may be legitimately mid-flight. The scan
failing is `unevaluated`. This tri-state is the honesty contract applied to
traceability.

## 5. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Empty/absent graph passes vacuously (the Phase-0 concern) | Empty requirement set → traceability dimension `unevaluated`, never `pass`; asserted by test. |
| Scan can't establish edges but reports pass | Scan/read error → `U_TRACE_UNSCANNED` / `unevaluated`; asserted by an injected scan-failure test. |
| Traceability wrongly blocks an operation | It is advisory-only: check-surfaced findings, never an integrity refusal; a test asserts a dangling annotation does not refuse `req`/ticket writes. |
| Edges drift between a stored copy and the code | Edges are *only* discovered from code annotations; never stored on the requirement — no second registry. |
| Annotation parsing misreads (e.g. `covers:` in a string literal, multiple IDs, trailing content) | Deterministic, tested annotation parser with a fixture set incl. multi-ID, whitespace, and non-annotation lines; a comment-only match rule. |
| Requirement file with malformed frontmatter silently ignored | Parse error is a refused write and a surfaced check finding, never a silent skip (mirrors ticket parsing). |
| Hand-picked requirement IDs | `aira req add` allocates via `aira id <prefix>`; a test asserts the path goes through the allocator. |

## 6. Tests (TDD; every confirmed counterexample becomes a regression test)

1. **Requirement entity**: `NewRequirement` validates status enum, non-empty
   text, ID shape; round-trips through `.aira/requirements/<ID>.md`
   (`ParseRequirement`∘render == identity); malformed frontmatter is refused.
2. **Store CRUD + index**: add/ls/show/set with events/receipts, project+worktree
   scoped; reconciler rebuilds the requirement index from files; a status
   transition validates the closed enum.
3. **Scan / edge discovery**: fixture files with `covers:`/`verifies:` (single,
   multi-ID, whitespace variants, a `covers:` inside a string that must NOT
   match) produce exactly the expected edge set, deterministically.
4. **Graph resolution — dangling**: an annotation targeting a missing requirement
   → `E_TRACE_DANGLING`, check verdict `fail`; reproduced end-to-end via `Run`.
5. **Graph resolution — uncovered/unverified**: a requirement with no covers/no
   verifies → `W_TRACE_UNCOVERED`/`W_TRACE_UNVERIFIED` warnings, not a `fail`.
6. **Fail-closed honesty**: empty requirement set → traceability dimension
   `unevaluated`, NOT `pass` (proven load-bearing by asserting it is not pass);
   an injected scan failure → `U_TRACE_UNSCANNED`/`unevaluated`.
7. **Advisory, not integrity**: a dangling annotation present does not refuse a
   `req add`/`req set`/ticket write; only `check` surfaces it.
8. **ID allocation**: `req add` obtains its ID from the allocator; a spy asserts
   no hand-picked ID path.
9. **Face generation**: `aira_req` MCP tool + Skill actions are generated from the
   descriptor (the M8 golden/drift/parity tests extend to `req` with zero bespoke
   wiring); `req`'s per-operation arg reads match its declared metadata.
10. **Build constraint**: compiles into the static/no-cgo binary; the scan needs
    no daemon/network.

## 7. Expected yield

Requirements become a real, ID-allocated, git-durable entity, and the
`covers:`/`verifies:` convention becomes a checkable — but honestly advisory —
traceability graph: broken references fail `check`, uncovered/unverified
requirements warn, and an unscannable or empty graph reads `unevaluated` instead
of a vacuous green. This is the data-model foundation for M10 gates (which attach
to requirements/steps) and M11 review-depth, and it lets AIRA answer "what is
untraced?" over its own codebase — dogfooding the traceability assistant.

## 8. Out-of-scope confirmation

M9 adds no gates, attestations, ratchets, review emission, runner, or blocking
enforcement, and no new store behaviour beyond the requirement entity + the
read-only traceability scan folded into `check`. Those remain M10–M12.
