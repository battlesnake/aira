# aira v0.8 — FEE coordination substrate (shape-2 allocator-migration) — implementation plan

> **For agentic workers:** implement task-by-task, TDD. Steps use `- [ ]`. Argues from the owner's
> greenlit shape-(2) decision + the reconciled cross-session contract (below); do not re-derive or
> reintroduce cut/deferred scope.

**Ticket:** AIRA-237
**Base:** branch `aira-v08-fee` off `origin/master` `915a99f` (v0.7, proto 12), in a worktree.
**Goal:** Let an external project (fastest.ee first; Stoner/Flavour next) adopt aira as a cross-session
COORDINATION layer over its existing, externally-authored backlog — WITHOUT renaming its tickets —
by (a) importing its pre-allocated ids as coordination tickets, (b) becoming the forward ALLOCATOR
(`make id` → a thin wrapper over `aira id`), and (c) namespacing each project's prefixes so several
projects can keep their own intuitive `BL`/`NF`/… on one machine-wide store.

## Owner decision (2026-09-13) — shape (2), FEE- namespacing, GREENLIT

- **Shape (2) allocator-migration NOW; shape (3) full-migration LATER (~beta).** aira BECOMES the
  allocator (import seeds the per-prefix cursor; new ids mint from aira; `make id` → thin wrapper
  around `aira id`, keeping the many sub-agent briefs that call `make id` working). fastest.ee KEEPS
  authoring `docs/backlog.md` + `REQUIREMENTS.md`, and `check_traceability` runs UNCHANGED. **Build
  ZERO shape-(3) scaffolding** (no aira-owning-items, no aira-generated backlog, no gate re-sourcing).
- **FEE- prefixes are justified (NOT hypothetical):** the owner will run Stoner + Flavour on aira too,
  so bare `BL`/`NF`/`VR` would collide machine-wide across projects (`prefix_ownership.prefix` is a
  global PK). Namespacing keeps every project's intuitive prefixes. Build it as a GENERAL per-project
  prefix feature (Stoner/Flavour get the same shape), NOT a fastest.ee-specific hack.
- **Prepend happens IN AIRA, not the extractor** (confirmed with speed): the extractor emits BARE ids
  (`BL-123`) + bare link endpoints; aira applies the project's configured prefix on import + at the
  CLI input boundary → stores `FEE-BL-123` internally; repo-facing surfaces (files, `list`, what you
  type) stay `BL-123` under the `in_repo=false` (aira-only) config. `FEE` lives only in aira config.

## Global constraints (apply to every task)

- **Two-loop is MANDATORY** (this touches ID ALLOCATION): plan-review → build → build-review. No
  merge-then-test-async for this ticket.
- **Keep it simple / aira is primitives.** Reuse existing machinery: the id-preserving upsert +
  `id_counters` HWM seed in `ImportRequirements` (`internal/store/import_requirements.go`), the
  `allocations`/`id_counters`/`prefix_ownership` tables, `_largest_fitting`-style helpers. No new
  subsystem beyond the verb + the namespacing config.
- **aira owns ONLY coordination fields on re-import.** The external backlog is authoritative for
  title/status/body/labels; aira owns the LEASE, the WORKTREE-BINDING, and IN-AIRA relations. A
  re-import MUST NOT clobber those.
- **Honesty:** stable error codes (never a silent skip or fabricated success); a row that can't be
  imported surfaces as a named error; `unevaluated`/unknown never faked.
- **No back-compat obligation** (AIRA has no external users of its own).
- **Velocity:** TDD per task; mutation-sensitive assertions inline; DEFER `-race`/full-gate RUNS to
  the final verification task. The adversarial build-review is NOT deferred.

## Grounding (verified against source; re-verify at build time)

- Allocator mints from `id_counters(project_id, prefix, next_number)`: read `store.go:3353`, bump
  `next+1` `:3364`. Import seeds it via a high-water-mark upsert (`import_requirements.go:420-422`
  `ON CONFLICT … DO UPDATE SET next_number = MAX(current, imported)`; a second HWM upsert on the
  reconcile/receipt path `store.go:3142-3144`).
- IDs split on the LAST `-`: `splitTicketID` `store.go:3458` (`strconv.Atoi(id[idx+1:])`) → so
  `FEE-BL-123` = (prefix `FEE-BL`, number 123) parses free, BUT a numeric SUFFIX (`BL-1132c`) makes
  `Atoi` return 0 — see Task 3.
- **`domain.ValidateID`'s `idPattern` (`ticket.go:147`, `^[A-Z]{2,}-[1-9][0-9]*$`) is the DECISIVE id
  gate** — a single hyphen, digits-only number. It rejects both `FEE-BL-123` (two hyphens) and
  `BL-1132c` (trailing letter), and it gates ~33 non-test sites (create `core.go:2683`, lease
  `lease.go:207`, relations `relation_ready.go:118+`, selector `query.go:83`, the requirement-import
  path `import_requirements.go:158`, `Ticket.Validate` `ticket.go:197`). This is the load-bearing
  validator the earlier plan-review flagged as missed.
- Prefix charset validators: `validPrefix` (`store.go:3464`, A-Z only, len≥2 → `E_ID_INVALID` at
  store init `:443/450`); a SECOND check at config-parse (`internal/app/project.go:592-606`) →
  `E_CONFIG_INVALID` on a hyphenated prefix (peer-spike-observed; pin exactly at build).
- Prefix ownership is machine-wide-unique (`prefix_ownership.prefix` PK, `store.go:790-791`;
  `E_PREFIX_OWNERSHIP_CONFLICT`) — the reason FEE- namespacing is needed.
- Requirement-import unowned-prefix error code is `E_IMPORT_INVALID` (`import_requirements.go:78`).
- `resource_budget.go` already recommends the live `AIRA_AITEST_WORKER_OVERHEAD_BYTES` (v0.7 fix).

---

## Task 1 — configurable per-project prefix + repo-visibility switch (the namespacing foundation)

**Files:** `.aira/config` schema + loader; the prefix validators (`store.go:3464` + `app/project.go:592-606`); `domain.ValidateID`/`idPattern` (`ticket.go:147`); the selector-input + id-display boundary (`internal/core`/`cmd/aira`); Tests in `internal/domain`, `internal/store`, `internal/app`, `cmd/aira`.

**Interfaces:** the project's `.aira/config` gains `id_prefix = "FEE"` and `id_prefix_in_repo = <bool>` (default `true` = today's behaviour; `false` = aira-only). When `in_repo=false`: aira auto-prepends `<id_prefix>-` to a bare selector on input + strips it on repo-facing display/file names; internally always stores/keys the full `FEE-BL-123`.

- [ ] **Step 0 — PIN (verify, don't assume):** enumerate the EXACT id/prefix validator set. Confirm `idPattern` (`ticket.go:147`), `validPrefix` (`store.go:3464`), and the config-parse check (`app/project.go:592-606`). Grep every `ValidateID`/`validPrefix` call site. Confirm whether selector INPUT funnels through one resolution chokepoint or many (drives the prepend/strip cost).
- [ ] **Step 1 — RED:** `domain.ValidateID("FEE-BL-123")` currently fails; after the change it passes, while genuinely-invalid ids (`fee-bl-1`, `FEE--1`, `FEE-BL-0`, `FEE-BL-`) still reject. `validPrefix("FEE-BL")` accepts the ONE compound `<PREFIX>-` shape but rejects arbitrary hyphens/lowercase. Config round-trips `id_prefix` + `id_prefix_in_repo`.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** relax `idPattern` to a single optional compound segment: `^[A-Z]{2,}(-[A-Z]{2,})?-[1-9][0-9]*$` (NOT arbitrary hyphens; Task 3 extends the number part for suffixes). Relax `validPrefix` + the config-parse check to admit the `<PREFIX>-` separator. Add the config fields. Consider collapsing the config-parse charset check to call the shared `validPrefix` so the rule lives in ONE place.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — RED/GREEN — ergonomics + coordination-op coverage:** with `id_prefix=FEE, id_prefix_in_repo=false`: `aira claim BL-123` resolves the stored `FEE-BL-123`; `aira show`/`list` + the ticket FILE name present `BL-123` (stripped); the store key is `FEE-BL-123`. **Assert a compound id can be CLAIMED, LINKED, ready-queued, and exact-selected** (not merely split/created — the ~33 ValidateID sites). With `in_repo=true`, everything shows `FEE-BL-123`.
- [ ] **Step 6 — commit:** `feat(aira): AIRA-237 — configurable per-project prefix + repo-visibility switch (idPattern/validPrefix/config-parse relaxed to one compound segment)`.

## Task 2 — id-accepting ticket-import verb (`aira import --tickets <file.jsonl>`)

**Files:** new `internal/store/import_tickets.go` (model on `import_requirements.go`); a core verb + CLI face; Test `internal/store/import_tickets_test.go`.

**Input:** JSONL, one object per line (ALL ids BARE; aira applies the project prefix):
`{"id":"BL-123","title":"…","status":"planned","kind":"chore","severity":"P2","body":"…","labels":["area:x"],"links":[{"kind":"blocks","to":"NF-45"}]}`

**Contract (CLOSED with subpipe):**
- `status`: aira-canonical (draft/planned/in-progress/in-review/done/retired/superseded); the extractor maps its vocabulary; a malformed/unknown status is a row error.
- `severity`: per-row from the backlog `Sev` column — **P0 KEPT**, P1/P2/P3 direct, **P4 → P3 clamp**; default **P2** for Sev-less rows.
- `kind`: per-FILE default — `REQUIREMENTS.md` rows → `requirement-work`, `docs/backlog.md` (BL) rows → `chore`. (aira enums: severity {P0,P1,P2,P3}; kind {feature,bug,chore,spike,requirement-work}.)

**Semantics:**
- PRESERVE `id` verbatim (never mint on import); apply the project prefix. VALIDATE prefix-owned (`E_IMPORT_INVALID: unowned prefix`, matching the requirement importer) + no number collision with a DIFFERENT path. Seed `id_counters` HWM per prefix.
- FIELD-SCOPED IDEMPOTENT UPSERT keyed by id: first import → create id-preserving + set backlog fields; re-import → REFRESH title/status/body/labels (backlog is truth) but NEVER touch aira-owned coordination state (a live lease, worktree-binding, in-aira relations).
- STATUS force-set: bypass `ValidateTransition` (backlog is truth), documented as a deliberate bypass (requirements have no transition graph, so this is NEW vs the requirement importer — test a graph-forbidden transition lands via the file-write path).
- A row that DISAPPEARS between imports (previously imported, absent now): retire (not hard-delete — a live lease may exist) + report, scoped to a durable IMPORT-ORIGIN set so a natively-created aira ticket absent from the file is NOT swept.
- LINKS: two-pass (create/refresh all rows, then apply links via `CanonicalRelationOwner` as `Link()` does — relations live in the ticket-file frontmatter, canonical-owned by either endpoint; a render-from-row would WIPE them, so Pass 1 preserves the existing `Relations` slice verbatim). Re-applying an existing link is a NO-OP, never `E_RELATION_EXISTS`.
- Summary: created/refreshed/retired/errored counts + per-row errors; strict mode fails on any row error (mirror `ImportFindings`' strict flag).

- [ ] **Step 1 — RED:** import a 2-row JSONL (BL-1, NF-1) → both tickets exist with preserved (prefixed) ids; `id_counters` seeded.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** implement `ImportTickets`/`…Bytes` + wiring, modelled on `ImportRequirements`.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — RED/GREEN — idempotent upsert:** re-import with a changed title/status → refreshed; a lease + worktree-binding + an aira-added relation placed BEFORE re-import SURVIVE; a dropped row → retired (not deleted), reported; a natively-created ticket absent from the file is NOT retired.
- [ ] **Step 6 — RED/GREEN — validation + contract:** unowned prefix → `E_IMPORT_INVALID`; a colliding number → error (not silent overwrite); malformed status → row error; kind/severity defaults + per-row severity (P0 kept, P4→P3) applied.
- [ ] **Step 7 — commit:** `feat(aira): AIRA-237 — id-accepting ticket import (preserve ids, seed allocator, field-scoped idempotent upsert, two-pass links)`.

## Task 3 — split-suffix ids (`BL-1132c`) — the (prefix, number) model

**Files:** `splitTicketID` (`store.go:3458`), `idPattern` (`ticket.go:147`), the `allocations`/`id_counters` schema + allocate path, the import verb; Tests.

> fastest.ee has ~7 ids of the form `<PREFIX>-<N><letter>` (BL-1132c, IN-4b; `check_traceability`'s `_PREFIX_NUM_RE` allows a trailing `[a-z]`). The extractor emits them AS-IS (fail-closed, emitted==defined — they must round-trip, never be dropped/transformed). aira owns the model.

**The problem:** `idPattern`'s number is digits-only (rejects `1132c`); `splitTicketID`'s `Atoi("1132c")`=0 (wrong HWM seed); and the `allocations` PK is `(project_id, prefix, NUMBER-as-int)` with a numeric `id_counters`, so `BL-1132` and `BL-1132c` both reduce to number 1132 → they COLLIDE on the key, or a suffix can't be an int.

- [ ] **Step 0 — DECIDE + PIN (design):** choose the model (lean: carry an optional suffix — `allocations.number` stays int + a nullable `suffix` column, OR a parsed `(number, suffix)`; `id_counters` seeds on the NUMERIC prefix so `aira id BL` still mints `max-numeric+1` and never collides with a suffixed id). Confirm every consumer of `(prefix, number)` (allocate, collision-check, receipt/journal, `splitTicketID`).
- [ ] **Step 1 — RED:** `ValidateID("BL-1132c")` accepts; `splitTicketID`/the number parse yields (prefix `BL`, number 1132, suffix `c`); import of `BL-1132` + `BL-1132c` creates BOTH as distinct tickets (no collision); `aira id BL` after importing `BL-1132`(+c) mints `BL-1133`.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** relax `idPattern` number part to an optional trailing `[a-z]` (`…[1-9][0-9]*[a-z]?$`); parse the numeric prefix for the counter; store the suffix so `BL-1132` and `BL-1132c` are distinct rows. Keep it minimal (only the compound-prefix + trailing-letter shapes, not arbitrary ids).
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aira): AIRA-237 — split-suffix ids (BL-1132c): distinct rows, numeric-prefix HWM seed`.

## Task 4 — allocator seed → mint-forward E2E + the dup-id gate (the load-bearing round-trip)

**Files:** an end-to-end test crossing import + allocate; verify `check_no_duplicate_ids` interplay.

> This is what shape (2) rests on: import seeds the cursor → `aira id` mints forward → author a row with that id → NO stomp. Confirmed by source-read, NOT yet run end-to-end — it MUST be an explicit test.

- [ ] **Step 1 — RED/GREEN — round-trip:** import ids BL-1..BL-100 → `aira id BL` mints `BL-101` (next free, no stomp) → creating a ticket with that minted id succeeds + doesn't collide → a second `aira id BL` gives `BL-102`; none of BL-1..100 altered. Include the FEE-prefixed variant (`id_prefix=FEE`, type `BL-101` repo-facing / `FEE-BL-101` stored) and a split-suffix floor (import BL-1132c → `aira id BL` still mints above the numeric max).
- [ ] **Step 2 — RED/GREEN — dup-id gate belt-and-suspenders (speed):** `check_no_duplicate_ids` (the fail-closed backlog/REQUIREMENTS gate) STAYS unchanged; assert it remains GREEN through a full seed → `aira id` mint → author-row cycle (it should never fire if the HWM seed + forward-mint is correct; if the round-trip ever slips it catches the dup instead of a silent one).
- [ ] **Step 3 — commit:** `test(aira): AIRA-237 — E2E seed→mint-forward round-trip (no stomp) + dup-id-gate green through the cycle`.

## Task 5 — verification + the cutover protocol (doc, not executed here)

**Files:** verification runs; a short cutover note appended to AIRA-237 / this plan.

- [ ] **Step 1:** `aira confine -- make test` — exact exit code; green.
- [ ] **Step 2:** `aira confine -- make race` — 0 data races.
- [ ] **Step 3:** mutation spot-checks: break the `id_counters` HWM seed (import stops seeding) → the E2E round-trip reds (mints a colliding id); break the coordination-field preservation → the lease/binding-survives-reimport test reds; break the split-suffix distinctness → the BL-1132 vs BL-1132c test reds.
- [ ] **Step 4 — document the CUTOVER (fastest.ee-side coordination, executed WITH speed+subpipe when the verb lands, NOT here):** `make id` becomes a thin wrapper over `aira id <TYPE>` in the FEE-namespaced project. **CUTOVER ATOMICITY (speed):** today `make id` serialises allocation via a fail-closed lockfile. Flip-window race: a worktree calling OLD `make id` AFTER the import snapshots the max but BEFORE the wrapper goes live → old-make-id and aira both mint the same next number → dup (caught by the gate, but avoidable). Clean cutover = **quiesce allocation → import/seed at the true max → flip the make-id wrapper → unfreeze**, with NO window where both allocators are live. One atomic hand-off.
- [ ] **Step 5:** record exact exit codes; nothing claimed green from truncated output.

## OUT OF SCOPE (do NOT build)

- Shape-(3) full migration: aira-owning-items, aira-generated backlog, `check_traceability` re-sourcing against aira — the owner targets these ~beta, additively over shape (2), with NO verb rework.
- The fastest.ee-side extractor + the `make id` wrapper (subpipe's lane; built to the closed JSONL contract).
- CPU-time-weighted aitest scheduling, config-TOML, single-test mode, usage-history file, under-size warning — those are the OTHER v0.8 track (deferred v0.7 features), a separate effort.

## Self-review checklist (before the build-review gate)

- idPattern + validPrefix + the config-parse check ALL relaxed to the one compound `<PREFIX>-` segment (+ the trailing-letter for suffixes), enumerated not assumed? A compound id can be claimed/linked/ready-queued/selected (tested)?
- Import preserves ids + seeds `id_counters` HWM; E2E round-trip test present + mutation-guarded; dup-id gate stays green through the cycle?
- Re-import refreshes backlog fields but NEVER clobbers a live lease / worktree-binding / aira-added relation; relations preserved via read-merge + CanonicalRelationOwner (not render-from-row); links idempotent?
- Split-suffix ids round-trip as DISTINCT rows; `aira id` mints above the numeric max?
- Status force-set documented (new bypass vs requirements); disappearing row → retire+report scoped to import-origin, never sweeps native tickets?
- kind/severity contract (P0 kept, P4→P3, default P2; per-file kind); unowned prefix → E_IMPORT_INVALID?
- Shape-(3) scaffolding correctly OUT; make-id cutover documented (quiesce→seed→flip→unfreeze), not executed here?
