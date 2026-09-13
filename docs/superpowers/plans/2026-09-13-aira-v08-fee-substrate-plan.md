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

## v2 changelog (2026-09-13) — plan-review BLOCK → re-plan

v1 (committed `675f332`) was **BLOCKED** by the mandatory plan-review gate (ultracode two-loop; the
class where green tests hide P0s). v2 folds in the confirmed findings + a local measurement of
fastest.ee's real backlog. The two heaviest changes are SIMPLIFICATIONS:

1. **BLOCKER fixed — killed the `in_repo=false` file-stripping design.** v1 stored key `FEE-BL-123`
   but wrote the ticket FILE as `BL-123.md`. AIRA rebuilds its index FROM the ticket files and
   enforces `filename == frontmatter-id == store-key` in three places — a divergent file is dropped
   from the index. v2: the FULL `FEE-BL-123` lives in filename + frontmatter + key in every
   `.aira/tickets` artifact; `FEE-` is stripped/prepended ONLY at the CLI display + core-ingress
   boundary. **Deleted the `id_prefix_in_repo` boolean** (it encoded two designs, not on/off of one).
   Safe: `.aira/tickets/*.md` are aira's OWN files, not fastest.ee's `backlog.md`/`REQUIREMENTS.md`
   (MEASURED: nothing in fastest.ee dereferences `.aira/tickets` paths), so nothing repo-authored
   ever shows `FEE-`.
2. **Task 3 split-suffix model MEASURED + PINNED to path (A).** v1 left the model as an open Step-0
   question and asserted the collision "as certain, unmeasured." MEASURED (below): the collision is
   real — 4 groups. Path (A) (widen the `allocations` key with a `suffix` column) is pinned over the
   ticket-only alternative (B), because every `(prefix,number)`-keyed site (retire/materialise/
   receipt-replay) would otherwise silently match the plain twin — (B) needs a bail-branch at each,
   the same enumeration as (A) but as a two-class model (the "flag that means two things" smell).
3. Confirmed highs threaded in: the materialise UPDATE keys on `numberOf` (a SEPARATE parser Task 3
   never named), not `splitTicketID`; the prefix-composition chokepoint must reach `prefix_ownership`
   + `AllocateID`, not just input/display; `find add --ticket` bypasses selector resolution.
4. Policy pins: retire-on-disappear → HONESTY-ONLY (report, don't auto-retire — deletes the
   origin-tracking machinery); dangling-link endpoint → named per-row error; deleted-backlog-link →
   documented add-only gap; leave `idLess`/`splitID` UNCHANGED. Grounding notes corrected.

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
  (`BL-123`) + bare link endpoints; aira applies the project's configured prefix. **The full
  `FEE-BL-123` is what aira stores, keys, and names its own files by** — `FEE-` is stripped only when
  aira PRINTS an id to a human (`list`/`show`) and prepended only when a human TYPES one (selector /
  `--ticket` / `--prefix` / link endpoints). fastest.ee's own repo files (`backlog.md`,
  `REQUIREMENTS.md`, source) are never touched and never see `FEE-`. Presence of `id_prefix` in aira
  config is the whole switch (no separate boolean).

## Global constraints (apply to every task)

- **Two-loop is MANDATORY** (this touches ID ALLOCATION): plan-review → build → build-review. No
  merge-then-test-async for this ticket.
- **Keep it simple / aira is primitives.** Reuse existing machinery: the id-preserving upsert +
  `id_counters` HWM seed in `ImportRequirements` (`internal/store/import_requirements.go`), the
  `allocations`/`id_counters`/`prefix_ownership` tables. No new subsystem beyond the verb + the
  namespacing config + the `suffix` column.
- **aira owns ONLY coordination fields on re-import.** The external backlog is authoritative for
  title/status/body/labels; aira owns the LEASE, the WORKTREE-BINDING, and IN-AIRA relations. A
  re-import MUST NOT clobber those.
- **Honesty:** stable error codes (never a silent skip or fabricated success); a row that can't be
  imported surfaces as a named error; `unevaluated`/unknown never faked.
- **No back-compat obligation** (AIRA has no external users of its own) — a fresh `CREATE TABLE` for
  the `allocations` schema change is fine; no migration dance.
- **Velocity:** TDD per task; mutation-sensitive assertions inline; DEFER `-race`/full-gate RUNS to
  the final verification task. The adversarial build-review is NOT deferred.

## Measured facts — fastest.ee backlog (2026-09-13; read-only; reproduction below)

Reproduction (run from `/home/mark/claude/fastest-ee`, read-only):
```
# suffixed ids (PREFIX-digits+letters), both backlog files:
grep -ohE '\b[A-Z]{2,}-[0-9]+[a-z]+\b' REQUIREMENTS.md docs/backlog.md | sort -u
# per suffixed id: own standalone table-row + plain-twin row?  (^| ID |)
#   base=${id%[a-z]}; grep -cE "^\| *$id \|" ... ; grep -cE "^\| *$base \|" ...
# collision groups among standalone-row ids (plain + suffixed) sharing (prefix,number):
grep -ohE '^\| *[A-Z]{2,}-[0-9]+[a-z]* \|' REQUIREMENTS.md docs/backlog.md \
  | grep -oE '[A-Z]{2,}-[0-9]+[a-z]*' | sed -E 's/([A-Z]+-[0-9]+)[a-z]*/\1/' \
  | sort | uniq -c | awk '$1>1'
```

- **Suffix convention = parent → split-children.** `BL-10` is a parent ("answered (gaps→BL-10a/b)")
  with real standalone-row children `BL-10a`, `BL-10b`; likewise `BL-37` → `BL-37a/b/c`. All observed
  suffixes are a SINGLE trailing `[a-z]`.
- **Exact import collision set (standalone table rows sharing `(prefix,number)`):**
  **`BL-37` (4: BL-37,a,b,c), `BL-10` (3: BL-10,a,b), `PW-1` (2), `IN-4` (2).**
  7 real suffixed tickets total: `BL-10a`, `BL-10b`, `BL-37a`, `BL-37b`, `BL-37c`, `IN-4b`, `PW-1b`.
  All coexist with a plain-number twin (`BL-10`, `BL-37`, `IN-4`, `PW-1` all have standalone rows).
  ⇒ `BL-10` and `BL-10a` both parse to number 10 → THEY COLLIDE on `allocations`
  `(project_id, prefix, number)`. **The suffix MUST enter the key** (Task 3, path A).
- The other 15 grep hits are NOT imported: `BL-1132c`, `BL-27a`, `BL-28d`, `BL-320w`, `BL-803c`,
  `BL-964a`, `FI-1a`, `FI-3a`, `FM-1b`, `FM-2d`, `FM-2f`, `OPS-5b`, `TEN-1a` are inline prose
  cross-references (0 standalone rows); `ICM-209xx`/`ICM-42xxx` are IMU part numbers. The import
  consumes STRUCTURED backlog rows (table first column), never a prose sweep.
- **The forward allocator never mints a suffix.** `make id` → `scripts/next_id.sh <PREFIX>` →
  `libs/engine/id_alloc.py <PREFIX>` → plain `<PREFIX>-N`. Suffixed children are hand-authored. ⇒
  suffix-handling is IMPORT + STORAGE + DISPLAY only; the allocator + `id_counters` HWM stay
  NUMBER-only.
- **HWM seed source at cutover = `id_alloc.py`'s git-common-dir flock counter**, floored at
  `max(worktree constant, origin/master constant, ledger max)`. This can EXCEED `max(id in master's
  backlog.md)` because ids allocated on unmerged branches are not yet in master. Seeding aira's
  `id_counters` from `max(imported rows)` alone would re-mint a live-on-a-branch id. Seed from the
  `id_alloc` counter (or `max(counter, imported)`) at the cutover instant (Task 4/5).
- **`.aira/tickets` deref: EMPTY** — nothing in fastest.ee references `.aira/tickets` paths, and
  `.aira/` holds only a `config` today. Compound filenames (`FEE-BL-123.md`) are safe.
- **`FEESPEND` is fastest.ee's only currently-declared aira prefix, with 0 live tickets**
  (`aira count --by status` → total 0). So `id_prefix=FEE` composing it to `FEE-FEESPEND` orphans
  NOTHING; no FEESPEND migration needed.

## Grounding (verified against source at `/home/mark/tmp/aira-fee`; re-verify at build time)

- **Rebuildable-index identity invariant (the v1 blocker):** a ticket's FILE base name must equal its
  frontmatter id must equal the store/index key — enforced at `store.go:4165` (`scanTickets`:
  `if filepath.Base(path) != ticket.ID+".md"` → `E_CONFIG_INVALID`, excluded from the index),
  `query.go:288` (`exactRecord`: `ticket.ID != id || filepath.Base(path) != id+".md"`), and
  `check.go:317/329` (`E_ID_UNRESOLVED`). ⇒ the stored key and the on-disk filename cannot diverge.
- Allocator mints from `id_counters(project_id, prefix, next_number)` (NUMBER-only; no suffix): read
  `store.go:3353`, bump `next+1` `:3364`; ownership lookup `AllocateID` `store.go:2088`
  (`kind, owned := s.prefixes[prefix]`). Import seeds it via a high-water-mark upsert
  (`import_requirements.go:420-422` `ON CONFLICT … DO UPDATE SET next_number = MAX(current, imported)`;
  a second HWM upsert on the reconcile/receipt path `store.go:3142-3144`).
- **Two SEPARATE `(prefix,number)` parsers** — Task 3 must fix BOTH: `splitTicketID` (`store.go:3458`,
  `strconv.Atoi`) AND `numberOf`/`prefixOf` (`store.go:3451-3456`, `strconv.ParseInt`). Both split on
  the LAST `-` and both return number `0` on a numeric-suffix tail (`Atoi("1132c")=0`,
  `ParseInt("10a")=0`). The ticket-materialise UPDATE (`store.go:2384`,
  `UPDATE allocations SET state='materialised' WHERE prefix=? AND number=?`) keys on
  `prefixOf`/`numberOf` — NOT `splitTicketID` — so a suffixed id would materialise `number=0`, match
  no row, and leave the allocation stuck `state='allocated'` (later spuriously retirable +
  `aira check` fabricates `E_ID_UNRESOLVED`).
- **All `(prefix,number)`-keyed allocation sites** (each must be suffix-aware under path A):
  `store.go:2384` (materialise), `3008` (retire), `3029`/`3092`/`3199` (SELECT), `3202` (receipt INSERT),
  `4228-4239` (git-tree scan → HWM, number-only), `import_requirements.go:318` (`findRequirementAllocation`),
  `outbox_retire.go:295`, `requirement.go:277`, and `check.go:283`'s
  `fmt.Sprintf("%s-%d", prefix, number)` id-reconstruction (drops the suffix → stats the wrong file).
- `allocations` PK is `(project_id, prefix, number)` (`store.go:804-810`, `number INTEGER NOT NULL`);
  SQLite cannot `ALTER` a PRIMARY KEY, so path A recreates the table (no back-compat).
- **`domain.ValidateID`'s `idPattern` (`ticket.go:147`, `^[A-Z]{2,}-[1-9][0-9]*$`) is the DECISIVE id
  gate.** It rejects both `FEE-BL-123` (two hyphens) and `BL-10a` (trailing letter), and gates ~33
  non-test sites (create, lease `lease.go:207`, relations `relation_ready.go:118+`, selector
  `query.go:83`, requirement-import `import_requirements.go:158`, `Ticket.Validate` `ticket.go:197`).
  **`find add --ticket` (`core.go:1012`) format-validates the ticket id via `ValidateID` with NO
  resolution** (`finding.go:176`) and keys the finding on the raw string — so a bare `BL-123` typed
  under a namespaced project silently orphans against the stored `FEE-BL-123` (the ingress leak).
- Prefix charset validators: `validPrefix` (`store.go:3464`, A-Z only, len≥2 → `E_ID_INVALID` at
  store init `store.go:442/447`, feeding `prefix_ownership` `store.go:2050`); a SECOND check at
  config-parse (`internal/app/project.go:592-606`) → `E_CONFIG_INVALID`. `config.Project.Prefixes`
  flows verbatim to `StoreOptions.Prefixes` (`app/project.go:243`) → `s.prefixes` → `prefix_ownership`.
- Prefix ownership is machine-wide-unique (`prefix_ownership.prefix` PK, `store.go:790-791`;
  `E_PREFIX_OWNERSHIP_CONFLICT`) — the reason FEE- namespacing is needed.
- Requirement-import unowned-prefix error code is `E_IMPORT_INVALID` (`import_requirements.go:78`).
- **Relations live in ticket-file frontmatter, canonical-owned by the LOWER-id endpoint ONLY** (not
  "either"): `CanonicalRelationOwner` returns the lower id via `idLess` (`ticket.go:327-332`), and
  `Ticket.Validate` refuses a relation stored on any other side (`ticket.go:227-229`). `Link` writes
  the owner's file (`relation_ready.go:176-177`). A render-from-row re-import WOULD wipe an
  aira-added relation; a read-merge preserves it.
- Idempotency in `ImportRequirements` is digest-driven: `row.Digest` is computed from
  render-from-row bytes at parse time (`import_requirements.go:186`) BEFORE any existing file is read,
  then drives `unchanged`/`repaired`/outbox `intended_digest`. A ticket importer that PRESERVES
  in-file relations must digest POST-merge (read → merge → render → digest), or it mis-detects
  unchanged/repaired.

---

## Task 1 — configurable per-project prefix (the namespacing foundation; compound stored everywhere)

**Files:** `.aira/config` schema + loader (`internal/app/project.go`); `domain.ValidateID`/`idPattern`
(`ticket.go:147`); `validPrefix` (`store.go:3464`) + `StoreOptions.Prefixes` composition
(`app/project.go:243` → `store.go:442`); the display-strip in `list`/`show` and the input-prepend at
core ingress (`internal/core/core.go`, `cmd/aira`); Tests in `internal/domain`, `internal/store`,
`internal/app`, `internal/core`, `cmd/aira`.

**Interfaces:** the project's `.aira/config` gains one field `id_prefix` (e.g. `"FEE"`; absent =
today's behaviour, prefixes used verbatim). When set: aira composes `<id_prefix>-<PREFIX>` (e.g.
`FEE-BL`) as the REGISTERED/owned/keyed prefix, and every stored id, frontmatter id, and
`.aira/tickets` FILE name is the full `FEE-BL-123`. The `<id_prefix>-` segment is prepended to
human-typed ids on input and stripped from ids on human-facing output. **No `id_prefix_in_repo`
boolean** — presence of `id_prefix` is the whole switch.

**Three composition points (NAME each; do not write "boundary"):**
- **(a) Registration:** compose `<id_prefix>-<PREFIX>` when building `StoreOptions.Prefixes` from
  `config.Project.Prefixes` (`app/project.go:243`) so `s.prefixes` (`store.go:442`) and
  `prefix_ownership` (`store.go:2050`) hold `FEE-BL`; and prepend `<id_prefix>-` to the `--prefix`
  arg of `aira id`/allocate (`core.go:734/740/770` → `AllocateID`, `store.go:2088`).
- **(b) Input prepend:** for a namespaced project, prepend `<id_prefix>-` to a bare human-typed id
  before it hits `ValidateID`/resolution. Most verbs funnel `selector` → `c.store.Get` →
  `parseSelector` (one chokepoint); ALSO cover the link endpoints `from`/`to`
  (`core.go:1450/1454/1465/1469`, incl. `relationSelectorID`) and **`find add --ticket`**
  (`core.go:1012`) which validates+keys without resolving. Telemetry `--ticket` on
  `compute`/`test-report`/`run` (`core.go:1163/1254/1649`) is an OPAQUE external tag (`compute.go:60`)
  — leave it un-normalized (documented decision), it is not a store key.
- **(c) Display strip:** strip a leading `<id_prefix>-` when `list`/`show` PRINT an id to a human.

- [ ] **Step 0 — PIN (verify, don't assume):** enumerate the EXACT id/prefix validator set +
  ingress sites. Confirm `idPattern` (`ticket.go:147`), `validPrefix` (`store.go:3464`), the
  config-parse check (`app/project.go:592-606`), and that `parseSelector` / `relationSelectorID` /
  `find add`'s `ValidateID` are the resolution points. Confirm `s.prefixes`/`prefix_ownership` are
  fed from `StoreOptions.Prefixes`.
- [ ] **Step 1 — RED:** `domain.ValidateID("FEE-BL-123")` currently fails; after the change it
  passes, while genuinely-invalid ids (`fee-bl-1`, `FEE--1`, `FEE-BL-0`, `A-1`, `AB-CD-EF-1`) still
  reject. `validPrefix("FEE-BL")` accepts the ONE compound `<PREFIX>-` shape but rejects arbitrary
  hyphens/lowercase. Config round-trips `id_prefix`.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** relax `idPattern` to a single optional compound segment:
  `^[A-Z]{2,}(-[A-Z]{2,})?-[1-9][0-9]*$` (Task 3 extends the number part for the trailing letter).
  Relax ONLY `validPrefix` (`store.go:3464`) to admit ONE `<PREFIX>-` separator. **Keep the
  config-parse charset check (`app/project.go:592-606`) STRICT on bare `config.Project.Prefixes`**
  (they are bare `BL`/`NF`; the compound is only ever formed when composing `StoreOptions.Prefixes`);
  add validation of the new `id_prefix` field as plain `[A-Z]{2,}`. Add the config field + the (a)/
  (b)/(c) composition.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — RED/GREEN — ergonomics + coordination-op coverage:** with `id_prefix=FEE`:
  `aira claim BL-123` resolves the stored `FEE-BL-123`; `aira show`/`list` print `BL-123`; the ticket
  FILE is `FEE-BL-123.md` with frontmatter id `FEE-BL-123` (compound everywhere on disk). **Assert a
  compound id can be CLAIMED, LINKED (both endpoints bare-typed), ready-queued, exact-selected, and
  `find add --ticket BL-123`'d** (the ingress leak) — the finding keys to `FEE-BL-123`, not bare.
- [ ] **Step 6 — RED/GREEN — machine-wide ownership:** `prefix_ownership` stores `FEE-BL` (not `BL`)
  for a namespaced project; **two projects that both declare bare `BL` under DIFFERENT `id_prefix`
  register `FEE-BL` and `STO-BL` and do NOT raise `E_PREFIX_OWNERSHIP_CONFLICT`** (the collision the
  feature exists to prevent).
- [ ] **Step 7 — commit:** `feat(aira): AIRA-237 — configurable per-project id_prefix (compound stored/keyed/named everywhere; strip+prepend only at the human boundary; idPattern/validPrefix relaxed to one compound segment)`.

## Task 2 — id-accepting ticket-import verb (`aira import --tickets <file.jsonl>`)

**Files:** new `internal/store/import_tickets.go` (model on `import_requirements.go`); a core verb +
CLI face; Test `internal/store/import_tickets_test.go`.

**Input:** JSONL, one object per line (ALL ids BARE; aira applies the project prefix):
`{"id":"BL-123","title":"…","status":"planned","kind":"chore","severity":"P2","body":"…","labels":["area:x"],"links":[{"kind":"blocks","to":"NF-45"}]}`

**Contract (CLOSED with subpipe):**
- `status`: aira-canonical (draft/planned/in-progress/in-review/done/retired/superseded); the extractor
  maps its vocabulary; a malformed/unknown status is a row error.
- `severity`: per-row from the backlog `Sev` column — **P0 KEPT**, P1/P2/P3 direct, **P4 → P3 clamp**;
  default **P2** for Sev-less rows.
- `kind`: per-FILE default — `REQUIREMENTS.md` rows → `requirement-work`, `docs/backlog.md` (BL) rows
  → `chore`. (aira enums: severity {P0,P1,P2,P3}; kind {feature,bug,chore,spike,requirement-work}.)

**Semantics:**
- PRESERVE `id` verbatim (never mint on import); apply the project prefix (Task 1 composition).
  VALIDATE prefix-owned (`E_IMPORT_INVALID: unowned prefix`, matching the requirement importer) + no
  number collision with a DIFFERENT path. Seed `id_counters` HWM per prefix (see Task 4 for the
  cutover source).
- FIELD-SCOPED IDEMPOTENT UPSERT keyed by id, via READ-MERGE (never render-from-row): first import →
  create id-preserving + set backlog fields; re-import → read the existing ticket, overlay ONLY
  title/status/body/labels (backlog is truth), re-render — leaving the LEASE, worktree-binding, and
  the existing `Relations` slice untouched. **Digest POST-merge** (read → merge → render → digest),
  not from a pre-read render-from-row, so unchanged/repaired detect correctly.
- STATUS force-set: bypass `ValidateTransition` (backlog is truth) via the file-write path
  (`UpdateTicketContent` enforces the transition, so this is a DELIBERATE new bypass vs the
  requirement importer, which has no transition graph). Test a graph-forbidden transition lands.
- A row that DISAPPEARS between imports (previously imported, absent now): **report it in the summary;
  do NOT auto-retire.** (Rationale: the requirement importer has no sweep; auto-retire needs a durable
  import-origin marker — a `label` is refreshed away, a new `allocations.kind` is immutable
  (`store.go:245`) — and the honest primitive is to surface, not to act. The backlog marking a row
  done/retired flows through the normal status refresh anyway.)
- LINKS: two-pass (create/refresh all rows, then apply links via `CanonicalRelationOwner` as `Link()`
  does — LOWER-id endpoint owns the frontmatter tuple). Re-applying an existing link is a NO-OP
  (never `E_RELATION_EXISTS`). A link whose endpoint is NOT resolvable (outside the batch AND absent
  from the store) is a NAMED per-row error (surfaced `E_RELATION_TARGET_MISSING`/`E_IMPORT_INVALID`),
  counted in the summary — never a silent skip; strict mode fails on it. Deleted-backlog-link policy:
  links are ADD-ONLY across re-imports (a link the backlog author removes is NOT pruned — aira cannot
  distinguish it from an aira-added link without a per-relation origin marker); DOCUMENTED coverage
  gap, not silent.
- Summary: created/refreshed/reported-disappeared/errored counts + per-row errors; strict mode fails
  on any row error (mirror `ImportFindings`' strict flag).

- [ ] **Step 1 — RED:** import a 2-row JSONL (BL-1, NF-1) → both tickets exist with preserved
  (prefixed) ids; `id_counters` seeded.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** implement `ImportTickets`/`…Bytes` + wiring, modelled on
  `ImportRequirements`, with the read-merge upsert + post-merge digest.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — RED/GREEN — idempotent upsert + coordination preservation:** re-import with a
  changed title/status → refreshed; **a lease + worktree-binding + an aira-ADDED relation (via
  `aira link`, present in NO JSONL row) placed BEFORE re-import all SURVIVE** (this reds a
  render-from-row impl — the load-bearing assertion); an unchanged re-import detects unchanged (digest
  post-merge); a dropped row → REPORTED, not retired; a natively-created ticket absent from the file
  is untouched.
- [ ] **Step 6 — RED/GREEN — validation + contract:** unowned prefix → `E_IMPORT_INVALID`; a colliding
  number+different path → error (not silent overwrite); malformed status → row error; a link to an
  unresolvable endpoint → named per-row error, counted; kind/severity defaults + per-row severity
  (P0 kept, P4→P3) applied.
- [ ] **Step 7 — commit:** `feat(aira): AIRA-237 — id-accepting ticket import (preserve ids, seed allocator, read-merge idempotent upsert, post-merge digest, two-pass links, disappear=report)`.

## Task 3 — split-suffix ids (`BL-10a`) — suffix in the allocations key (path A, MEASURED)

**Files:** `idPattern` (`ticket.go:147`); `splitTicketID` + `numberOf`/`prefixOf`
(`store.go:3451-3462`); the `allocations` table schema + PK (`store.go:804-810`) + the enumerated
`(prefix,number)` sites; `check.go:283`; the import verb; Tests.

> MEASURED (above): fastest.ee has 7 real suffixed standalone-row tickets across 4 collision groups
> (`BL-37`×4, `BL-10`×3, `PW-1`×2, `IN-4`×2), each a `<PREFIX>-<N><letter>` child sharing a number
> with its plain twin. The extractor emits them AS-IS (emitted==defined; they must round-trip).

**The problem + why path (A):** `idPattern`'s number is digits-only (rejects `10a`); `splitTicketID`
AND `numberOf` both `Atoi`/`ParseInt` the tail → `0` (wrong HWM + wrong materialise key); the
`allocations` PK is `(project_id, prefix, NUMBER-as-int)`, so `BL-10` and `BL-10a` collide. The
allocator NEVER mints a suffix (measured), so `id_counters` stays NUMBER-only; the suffix is needed
ONLY to make the `allocations` rows distinct. **Path (A): add a `suffix` column to the PK** — chosen
over a ticket-only model (suffixed ids get no allocation row) because the receipt-replay INSERT
(`store.go:3199-3205`) and the retire/materialise/SELECT sites are all keyed `(prefix,number)`: given
a suffixed id they'd match the plain twin's row (silent wrong-row corruption) unless the suffix is in
the key. (A) keeps the existing one-ticket↔one-allocation-row invariant and widens the key uniformly;
the alternative needs a bail-branch at every site (two entangled models).

- [ ] **Step 0 — PIN (design, verified):** `allocations` is RECREATED with PK
  `(project_id, prefix, number, suffix)`, `suffix TEXT NOT NULL DEFAULT ''` (SQLite can't `ALTER` a
  PK; no back-compat). `splitTicketID` returns `(prefix, number, suffix)`; `numberOf`/`prefixOf` route
  through it (or become suffix-aware). `id_counters` stays `(project_id, prefix)→next_number` on the
  NUMERIC part (suffix-agnostic). Thread `suffix` through EVERY enumerated `(prefix,number)` site
  (grounding list): `store.go:2384/3008/3029/3092/3199/3202`, `import_requirements.go:318`,
  `outbox_retire.go:295`, `requirement.go:277`, and `check.go:283` (use the allocation's stored
  `path`, or include the suffix in the reconstruction — do NOT `fmt.Sprintf("%s-%d")`). The git-tree
  scan HWM (`store.go:4228-4239`) uses the number only (via the new `splitTicketID`), ignoring suffix.
- [ ] **Step 1 — RED:** `ValidateID("BL-10a")` accepts; the parse yields (prefix `BL`, number 10,
  suffix `a`); **import of `BL-10` + `BL-10a` + `BL-10b` creates THREE distinct tickets AND three
  distinct `allocations` rows, all `state='materialised'`, and `aira check` is GREEN** (assert on
  `allocations.state`, not just the tickets table — the materialise-stuck bug is invisible to a
  tickets-only assertion); `aira id BL` after that import mints `BL-<numeric-max+1>`, never a suffix.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** relax `idPattern` number part to an optional trailing single `[a-z]`
  (final: `^[A-Z]{2,}(-[A-Z]{2,})?-[1-9][0-9]*[a-z]?$`); parse the numeric prefix for the counter;
  recreate `allocations` with the suffix PK; thread the suffix through the Step-0 site list.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aira): AIRA-237 — split-suffix ids (BL-10a): suffix in the allocations PK, suffix-aware parsers (splitTicketID+numberOf+check.go), numeric-only HWM`.

## Task 4 — allocator seed → mint-forward E2E + the dup-id gate (the load-bearing round-trip)

**Files:** an end-to-end test crossing import + allocate; verify `check_no_duplicate_ids` interplay.

> This is what shape (2) rests on: import seeds the cursor → `aira id` mints forward → author a row
> with that id → NO stomp. Confirmed by source-read, NOT yet run end-to-end — it MUST be an explicit
> test.

- [ ] **Step 1 — RED/GREEN — round-trip:** import ids BL-1..BL-100 → `aira id BL` mints `BL-101` (next
  free, no stomp) → creating a ticket with that minted id succeeds + doesn't collide → a second
  `aira id BL` gives `BL-102`; none of BL-1..100 altered. Include the FEE-prefixed variant
  (`id_prefix=FEE`, type `BL-101` → stored `FEE-BL-101`) and a split-suffix floor (import `BL-10a`
  alongside a high plain `BL-100` → `aira id BL` still mints `BL-101`, above the numeric max).
- [ ] **Step 2 — RED/GREEN — HWM seed source (cutover-correct):** the seed is taken from the AUTHORITATIVE
  next-number (the `id_alloc` counter value at cutover), NOT `max(imported rows)` — assert that
  seeding from a counter value ABOVE the imported max (simulating an unmerged-branch id) makes
  `aira id` mint above THAT, so a live-on-a-branch id is never re-minted. (Note: aira's dup-id gate
  cannot see an unmerged-branch id from the store alone — the counter is the source of truth.)
- [ ] **Step 3 — RED/GREEN — dup-id gate belt-and-suspenders (speed):** `check_no_duplicate_ids` (the
  fail-closed backlog/REQUIREMENTS gate) STAYS unchanged; assert it remains GREEN through a full
  seed → `aira id` mint → author-row cycle (it should never fire if the seed + forward-mint is
  correct; if the round-trip ever slips it catches the dup instead of a silent one).
- [ ] **Step 4 — commit:** `test(aira): AIRA-237 — E2E seed→mint-forward round-trip (no stomp, counter-sourced HWM) + dup-id-gate green through the cycle`.

## Task 5 — verification + the cutover protocol (doc, not executed here)

**Files:** verification runs; a short cutover note appended to AIRA-237 / this plan.

- [ ] **Step 1:** `aira confine -- make test` — exact exit code (check `PIPESTATUS`, not the wrapper);
  green.
- [ ] **Step 2:** `aira confine -- make race` — 0 data races.
- [ ] **Step 3:** mutation spot-checks: break the `id_counters` HWM seed (import stops seeding) → the
  E2E round-trip reds (mints a colliding id); break the read-merge coordination preservation (make it
  render-from-row) → the lease/binding/aira-added-relation-survives-reimport test reds; break the
  split-suffix distinctness (drop the suffix from the PK) → the BL-10 vs BL-10a distinct-rows +
  `aira check`-green test reds; leave `numberOf` number-only → the materialise-`state` assertion reds.
- [ ] **Step 4 — leave `idLess`/`splitID` UNCHANGED + prove canonical-owner stability:** do NOT touch
  `domain.idLess`/`splitID` (`ticket.go:596-625`) — re-ordering flips `CanonicalRelationOwner` and
  invalidates existing frontmatter (`ticket.go:227`). Add a test asserting compound + suffixed ids
  produce a STABLE canonical owner so no one "improves" the ordering.
- [ ] **Step 5 — document the CUTOVER (fastest.ee-side, executed WITH speed+subpipe when the verb
  lands, NOT here):** `make id` becomes a thin wrapper over `aira id <PREFIX>` in the FEE-namespaced
  project. **Atomic hand-off — quiesce → seed → flip → unfreeze**, NO window where both allocators are
  live: (1) quiesce `make id` (its fail-closed lockfile); (2) import the backlog rows AND seed
  `id_counters` from the `id_alloc` git-common-dir COUNTER value (the true next-number, ≥ backlog max
  — accounts for unmerged-branch ids); (3) flip the `make id` wrapper to `aira id`; (4) unfreeze.
  Note: `FEESPEND` has 0 live rows so composing it to `FEE-FEESPEND` orphans nothing.
- [ ] **Step 6:** record exact exit codes; nothing claimed green from truncated output.

## OUT OF SCOPE (do NOT build)

- Shape-(3) full migration: aira-owning-items, aira-generated backlog, `check_traceability`
  re-sourcing against aira — the owner targets these ~beta, additively over shape (2), with NO verb
  rework.
- The fastest.ee-side extractor + the `make id` wrapper (subpipe's lane; built to the closed JSONL
  contract).
- A per-relation origin marker to prune deleted backlog links (documented add-only gap, Task 2).
- CPU-time-weighted aitest scheduling, config-TOML, single-test mode, usage-history file, under-size
  warning — those are the OTHER v0.8 track (deferred v0.7 features), a separate effort.

## Self-review checklist (before the build-review gate)

- Compound `FEE-BL-123` stored in filename + frontmatter + key everywhere in `.aira/tickets`; strip
  only on human OUTPUT, prepend only on human INPUT; `id_prefix_in_repo` GONE; no store-identity
  divergence (`store.go:4165`/`query.go:288`/`check.go:317`)?
- The three composition points (registration→`prefix_ownership`, input at
  selector+`from`/`to`+`find add --ticket`, output strip) all covered + tested; telemetry `--ticket`
  left opaque by decision; `prefix_ownership` stores `FEE-BL` + two-projects-share-`BL` test present?
- idPattern relaxed to `^[A-Z]{2,}(-[A-Z]{2,})?-[1-9][0-9]*[a-z]?$`; `validPrefix` relaxed;
  config-parse charset kept STRICT on bare prefixes; `id_prefix` validated `[A-Z]{2,}`?
- Split-suffix path (A): `allocations` recreated with the suffix PK; BOTH `splitTicketID` AND
  `numberOf`/`prefixOf` suffix-aware; every enumerated `(prefix,number)` site threaded incl.
  `check.go:283`; RED asserts `allocations.state='materialised'` + `aira check` green; `id_counters`
  number-only; `idLess`/`splitID` UNCHANGED + stability test?
- Import: read-merge (not render-from-row) + POST-merge digest; lease/binding/aira-added-relation
  survive re-import; links idempotent + two-pass; dangling endpoint = named per-row error;
  disappear = REPORT not auto-retire; deleted-link add-only gap documented?
- E2E seed→mint round-trip present + mutation-guarded; HWM seeded from the `id_alloc` COUNTER (not
  `max(imported)`); dup-id gate stays green through the cycle?
- Status force-set documented as a new bypass; kind/severity contract (P0 kept, P4→P3, default P2;
  per-file kind); unowned prefix → `E_IMPORT_INVALID`?
- Shape-(3) scaffolding OUT; cutover documented (quiesce→seed-from-counter→flip→unfreeze), FEESPEND
  0-rows noted, not executed here?
