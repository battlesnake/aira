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

## v3 changelog (2026-09-13) — v2 re-review FIX-THEN-BUILD → edits folded in

v2 (`30e4200`) was re-reviewed (Fable plan-gate; ID-allocation two-loop) → **FIX-THEN-BUILD**: the
structure held (compound-everywhere, path A, read-merge, `idLess` untouched, FEESPEND all confirmed
against source), but 11 code-grounded edits were required before build (no third re-review). Folded
in below. The load-bearing ones:

1. **Composition must happen INSIDE the store, not at `app/project.go:243`** (v2's named site is
   TEST-ONLY — `OpenWithDiagnostics` is reached only from the in-process test dispatcher). The LIVE
   path is `daemon.ScopeFromProject → WorktreeScope (proto struct) → store.NewScope → registerDB`,
   all bare-prefix. Composing at 243 would register bare `BL` in production while the in-process
   Step-6 test went green, then `aira id BL` → `E_ID_INVALID: unowned prefix "FEE-BL"`. Fix: add
   `IDPrefix` to `store.Options`/`ScopeOptions` **and `daemon.WorktreeScope` (a proto struct change —
   owner heads-up; free per no-compat, needs a coordinated same-proto daemon restart)**; compose ONCE
   in `store.Open`/`NewScope` for BOTH `Prefixes` and `RequirementPrefixes`.
2. **HWM seed: off-by-one + no interface + durability gap.** The `id_alloc` counter holds the
   LAST-minted number (box reads `BL 1217`; `BL-1217` already in receipts; master max `BL-1193`), so
   `next_number=counter` re-mints. Fix: an explicit `aira import … --allocated-max BL=1217` flag
   (value = last-allocated), applied `MAX(current, N+1)`; a state.db-loss re-mint window is an
   ACCEPTED documented gap (backstop: fastest.ee's dup-id gate; re-apply the flag = one idempotent
   command).
3. **Path-A site list was incomplete** — add the two reconcile-recovery INSERTs (`store.go:3046`,
   `:3108`), the register-INSERT model (`import_requirements.go:407`), `check.go:263`. `suffix TEXT
   NOT NULL` with NO default (an omitted column fails loudly). A **fresh-clone reconcile RED** (every
   fresh fastest.ee clone hits recovery: receipts are in git, state.db is not). Delete
   `prefixOf`/`numberOf` → one parser (`splitTicketID` returns `(prefix,number,suffix)`).
4. **`allocations` recreation needs a mechanism** — `CREATE TABLE IF NOT EXISTS` is a no-op on the
   machine-wide live state.db. Use the `recreateProjectOwnedTable` shape (PRAGMA check → `_v2` →
   INSERT SELECT → DROP → RENAME). Deleted the "fresh CREATE is fine" line.
5. **Importer must accept cross-worktree mints** — do NOT copy `ImportRequirements`' path-equality
   refusal (`import_requirements.go:245`); a `state='allocated'` ticket row minted in another worktree
   is "pre-allocated, materialise here" (rewrite `allocations.path` on materialise).
6. Canonicaliser seam named (`s.canonicalID` on the store; `parseSelector` is receiver-less);
   segment-count prepend rule; the silent `id:`/`ticket:` term matchers covered; config check
   `id_prefix ∉ prefixes`. `displayID` projection incl. `aira id` output. Worktree-audit is a 4th
   composition point. Strict-mode links validate endpoints BEFORE any write; disappear "previously
   imported" set = an `events` row `verb='ticket.import'` (the "no durable marker" v2 rationale was
   FALSE against source). `severity` added to the re-import overlay set.

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
- **Namespacing lives in the STORE, carried through the daemon protocol.** Composition
  (`<id_prefix>-<PREFIX>`) must happen where the live path actually registers/keys prefixes — inside
  `store.Open`/`NewScope` — so `id_prefix` is added to `store.Options` and to the serialised
  `daemon.WorktreeScope` (a proto struct change; free per no-compat, but the box daemon must be
  restarted at cutover to carry the new field). Composing only at the app layer misses the live
  daemon path entirely (the v2 blocker).

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
- **No back-compat obligation** (AIRA has no external users of its own) — BUT the state.db is a LIVE
  machine-wide cache shared by every project/session on the box, so `CREATE TABLE IF NOT EXISTS`
  cannot widen an existing PK. The `allocations` schema change uses the `recreateProjectOwnedTable`
  migration shape (Task 3), not a "fresh CREATE."
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
- **HWM seed source at cutover = `id_alloc.py`'s git-common-dir counter, which holds the
  LAST-ALLOCATED number** (verified on the box: counter reads `BL 1217`; `.git/fastest-id-receipts`
  already records `BL-1217` minted; master's backlog max is `BL-1193`). So the counter EXCEEDS
  `max(id in master's backlog.md)` (ids minted on unmerged branches aren't in master), and it is the
  LAST value, not the next. aira's `id_counters` is `next_number`, so the seed must be applied as
  `next_number = MAX(current, counter+1)` — seeding `next_number = counter` would re-mint `BL-1217`.
  Seeding from `max(imported rows)` alone would re-mint a live-on-a-branch id. The value enters aira
  via an explicit `--allocated-max` flag on the import verb (Task 4); `id_alloc` guards its counter
  with an `flock -x` (`id_alloc.py:75-88`, `LOCK_EX`) — NOT a lockfile — which is the quiesce held
  across seed→flip (Task 5).
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
- **The LIVE prefix-registration path is NOT the app layer.** `app/project.go:238-244`
  (`OpenWithDiagnostics`) is reached only from `cmd/aira/dispatcher.go:923` inside
  `inProcessDispatcher` (built only in `dispatcher_inprocess_test.go`; its own comment: no production
  fallback). The live path is `dispatcher.go:980 scopeForCWD → daemon.ScopeFromProject`
  (`scope.go:32`, bare `Prefixes`) → serialised `WorktreeScope` (`protocol.go:209-210`, no id_prefix
  field) → `server.go:1295 NewScope` → `store.go:661-668` → `registerDB` (`store.go:2034-2050`,
  `INSERT INTO prefix_ownership … ON CONFLICT DO NOTHING`) + `id_counters` seed. Same for daemon init
  (`server.go:1149`), eject (`eject.go:230`), and the two relay read-only stores (`dispatcher.go:544`,
  `supervisor_relay_store.go:73`). `readConfig` uses `DisallowUnknownFields` (`project.go:547`), so a
  new `id_prefix` field MUST be added to `ProjectConfig` (`project.go:88-96`); production `aira init`
  (`project.go:425-475`) takes only `--project/--prefixes`.
- **Seven `INSERT INTO allocations` sites** — path A threads suffix through ALL: `store.go:2105`
  (`AllocateID`), `:2192`, `:3046` + `:3108` (the two reconcile-RECOVERY inserts on `ErrNoRows` when a
  scanned ticket/requirement file has no allocation row — reached by EVERY fresh clone, since receipts
  live in the git common dir but state.db does not), `:3202` (receipt-replay), plus the model
  `import_requirements.go:407` (register) and `requirement.go:247`. The recovery INSERT under a
  `suffix NOT NULL` column with an omitted value PK-clashes `(FEE-BL,10,'')` against the plain twin →
  Rebuild aborts. `scanTickets` reads via sorted `os.ReadDir` (`store.go:4105`) so `FEE-BL-10.md`
  recovers before `FEE-BL-10a.md`.
- **Schema migration shape:** allocations DDL is `CREATE TABLE IF NOT EXISTS` (`store.go:804-810`) run
  unconditionally at `store.go:1104`; there is no schema-version; the `kind` precedent is a
  `tableHasColumn`-guarded ADD COLUMN (`store.go:1476`), which cannot widen a PK. Use
  `recreateProjectOwnedTable` (`schema_ownership.go:106-167`): PRAGMA `table_info` check → `_v2` with
  the 4-column PK → `INSERT … SELECT *, ''` → DROP → RENAME, in one `BEGIN IMMEDIATE`.
- **A durable import-origin marker EXISTS** (v2's "no marker" claim was FALSE): the importer writes a
  journaled `events` row per imported row; a target with an `events` row `verb='ticket.import'` is
  "previously imported." Use it for the disappear=REPORT `absent_from_batch` set.
- **Canonicaliser seam:** `ParseSelector`/`parseSelector` are receiver-less package functions
  (`query.go:47/58`) with no access to `id_prefix` — the prepend CANNOT live there. It needs a Store
  method (`s.canonicalID`, from `Options.IDPrefix`). Two term matchers do RAW string equality and
  return a SILENT empty result for a bare id under namespacing: the `id:` term (`query.go:394`) and
  the finding `ticket:` term (`finding.go:379`).
- **Worktree-audit is a 4th composition point** (goes silent under namespacing): `core/worktree.go:136`
  passes `ws.TicketPrefixes()` = composed prefixes; `facts.go:243/256` anchor the branch/subject regex
  `^(?i:FEE-BL)…`, so fastest.ee's `bl449-…` branches / `BL-1178:` subjects never match, and a
  non-match is SILENT.
- **Display strip:** `renderHuman` is a generic JSON renderer; the `id` verb returns `AllocateID` raw
  (`core.go:739-742`) — the one output the `make id` wrapper consumes, which must show `BL-101`, not
  `FEE-BL-101`. Needs a `displayID` field-projection over an enumerated field set.

---

## Task 1 — configurable per-project prefix (the namespacing foundation; compound stored everywhere)

**Files:** `ProjectConfig` + `.aira/config` loader (`internal/app/project.go`);
`domain.ValidateID`/`idPattern` (`ticket.go:147`); `validPrefix` (`store.go:3464`); **`IDPrefix`
added to `store.Options`/`ScopeOptions` and `daemon.WorktreeScope` (`internal/daemon/protocol.go:209`
— PROTO STRUCT CHANGE)**, composed inside `store.Open`/`NewScope` (`store.go:442-459`/`661-677`);
`s.canonicalID` on the Store + a `displayID` projection in `internal/core`; `core/worktree.go` +
`facts.go` (audit inference); Tests in `internal/domain`, `internal/store`, `internal/app`,
`internal/daemon`, `internal/core`, `cmd/aira`.

**Interfaces:** the project's `.aira/config` gains one field `id_prefix` (e.g. `"FEE"`; absent =
today's behaviour, prefixes used verbatim). When set: aira composes `<id_prefix>-<PREFIX>` (e.g.
`FEE-BL`) as the REGISTERED/owned/keyed prefix, and every stored id, frontmatter id, and
`.aira/tickets` FILE name is the full `FEE-BL-123`. The `<id_prefix>-` segment is prepended to
human-typed ids on input and stripped from ids on human-facing output. **No `id_prefix_in_repo`
boolean** — presence of `id_prefix` is the whole switch. Config validation: **`id_prefix` is plain
`[A-Z]{2,}` and MUST NOT appear in `prefixes`** (else the segment-count prepend rule is ambiguous).

**FOUR composition points (NAME each; do not write "boundary"):**
- **(a) Registration — INSIDE the store, ONE place.** Add `IDPrefix` to `store.Options`,
  `store.ScopeOptions`, and `daemon.WorktreeScope`; `store.Open`/`NewScope` compose
  `s.prefixes[idPrefix+"-"+p]` for BOTH `Prefixes` and `RequirementPrefixes`, and keep `s.idPrefix`.
  So `prefix_ownership` (`store.go:2050`) + `id_counters` own `FEE-BL`/`FEE-NF`. Every call site
  (`project.go:243/359/399`, `scope.go:32`, `server.go:1149/1295`, `eject.go:230`, `dispatcher.go:544`,
  `supervisor_relay_store.go:73`) gains only `IDPrefix: <config|scope>.IDPrefix`. Do NOT compose at
  `app/project.go:243` alone — that path is test-only (`inProcessDispatcher`).
- **(b) Input prepend — `s.canonicalID(raw)` on the Store** (from `s.idPrefix`), called at the top of
  every Store entry taking a ticket id: `Get`, `List` + the `id:` term matcher (`query.go:394`),
  `Ready`, `Relations`, `Claim`/`Release`/`Heartbeat`, `Link`/`Unlink`, `AddFinding` + the finding
  `ticket:` term (`finding.go:379`), `RegisterWorktreeBinding`, and `AllocateID`'s `--prefix`. RULE:
  prepend iff `<id_prefix>-<seg1>` is a REGISTERED key in `s.prefixes`; else pass through
  (idempotent on an already-compound id; a copied `FEE-BL-123` is not double-prepended). NOT
  `HasPrefix`. Telemetry `--ticket` on `compute`/`test-report`/`run` (`core.go:1163/1254/1649`) is an
  OPAQUE external tag (`compute.go:60`) — left un-normalized (documented decision). The IMPORT path
  stays strict: a JSONL row already carrying `<id_prefix>-` is `E_IMPORT_INVALID`.
- **(c) Display strip — a `displayID` projection in core** over an enumerated field set (`id`,
  `ticket.id`, `relations[].from/to`, `blocked_by`, `ticket_id`, `ticket`, `from`/`to`; NEVER `path`),
  applied to `id`/`list`/`show`/`create`/`claim`/`link`/`ready`/`find ls`. **`aira id BL` MUST print
  `{"id":"BL-101"}`** (the `make id` wrapper contract; it returns `AllocateID` raw today,
  `core.go:739-742`).
- **(d) Worktree-audit inference** (`core/worktree.go:136` → `facts.go:243/256`): pass BARE prefixes +
  `IDPrefix` in `worktree.Inputs`; match branch/subject candidates on the BARE prefix, compose the
  candidate `<id_prefix>-<PREFIX>-<n>`. Else audit inference goes SILENT under namespacing. (Note
  `core.go:734/770` `--prefix` are `eject`/`rant` daemon project-less lookups — document "type the
  compound"; only `core.go:740` is `aira id`.)

- [ ] **Step 0 — PIN (verify, don't assume):** confirm `idPattern` (`ticket.go:147`), `validPrefix`
  (`store.go:3464`), the config-parse check (`app/project.go:592-606`). Confirm the LIVE registration
  path is `ScopeFromProject → WorktreeScope → NewScope → registerDB` (NOT `app/project.go:243`, which
  is test-only), and enumerate all `Prefixes:`-passing call sites (grounding list). Enumerate every
  Store entry taking a ticket id (for `s.canonicalID`) + the two silent term matchers (`query.go:394`
  `id:`, `finding.go:379` `ticket:`). Confirm `AllocateID`'s `--prefix` (`core.go:740`) vs the
  `eject`/`rant` project-less `--prefix` (`core.go:734/770`).
- [ ] **Step 1 — RED:** `domain.ValidateID("FEE-BL-123")` currently fails; after the change it
  passes, while genuinely-invalid ids (`fee-bl-1`, `FEE--1`, `FEE-BL-0`, `A-1`, `AB-CD-EF-1`) still
  reject. `validPrefix("FEE-BL")` accepts the ONE compound `<PREFIX>-` shape but rejects arbitrary
  hyphens/lowercase. Config round-trips `id_prefix`.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** relax `idPattern` to a single optional compound segment:
  `^[A-Z]{2,}(-[A-Z]{2,})?-[1-9][0-9]*$` (Task 3 extends the number part for the trailing letter).
  Relax ONLY `validPrefix` (`store.go:3464`) to admit ONE `<PREFIX>-` separator. **Keep the
  config-parse charset check (`app/project.go:592-606`) STRICT on bare `config.Project.Prefixes`**
  (the compound is only ever formed by composition, never authored). Add `IDPrefix` to `ProjectConfig`
  (`project.go:88-96`), `store.Options`/`ScopeOptions`, and `daemon.WorktreeScope` (proto); compose
  inside `store.Open`/`NewScope` for BOTH `Prefixes` and `RequirementPrefixes`; thread `IDPrefix:` at
  every call site. Validate `id_prefix` as `[A-Z]{2,}` and reject `id_prefix ∈ prefixes`. Implement
  `s.canonicalID` (b) + `displayID` (c) + the audit inference (d). Add `aira init --id-prefix`.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — RED/GREEN — ergonomics + coordination-op coverage:** with `id_prefix=FEE`:
  `aira claim BL-123` resolves stored `FEE-BL-123`; `aira show`/`list`/`aira id BL` print bare
  (`aira id BL` → `{"id":"BL-101"}`); the ticket FILE is `FEE-BL-123.md` frontmatter `FEE-BL-123`.
  **Assert a compound id can be CLAIMED, LINKED (both endpoints bare-typed), ready-queued,
  exact-selected, `list id:BL-1`'d, `find add --ticket BL-123`'d (keys `FEE-BL-123`, not bare),
  `find ls ticket:BL-1`'d, and that `aira show FEE-BL-1 == aira show BL-1`** (idempotent canonicaliser).
  A checkout on branch `bl-449-x` infers binding `FEE-BL-449` (displayed `BL-449`) AND shows a
  cross-worktree lease on it as `LiveLease true` (audit inference, point d).
- [ ] **Step 6 — RED/GREEN — machine-wide ownership (through the DAEMON path):** exercised via
  `ScopeFromProject → NewScope` (NOT in-process only): `prefix_ownership` stores `FEE-BL` (not `BL`);
  **two projects that both declare bare `BL` under DIFFERENT `id_prefix` register `FEE-BL` and `STO-BL`
  and do NOT raise `E_PREFIX_OWNERSHIP_CONFLICT`** (the collision the feature prevents). `aira id BL`
  under `id_prefix=FEE` mints against `FEE-BL` ownership (not `E_ID_INVALID: unowned prefix`).
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
  VALIDATE prefix-owned (`E_IMPORT_INVALID: unowned prefix`, matching the requirement importer). Seed
  `id_counters` HWM per prefix (see Task 4 for the cutover source + the `--allocated-max` flag).
- **CROSS-WORKTREE PRE-ALLOCATED ROWS (do NOT copy `import_requirements.go:245`'s path-equality
  refusal).** Under shape (2) `aira id` runs in a feature worktree (records a `state='allocated'`,
  kind=ticket allocation row keyed `(prefix,number,suffix)` with THAT worktree's path,
  `store.go:2104-2106`) and the import runs elsewhere. A `state='allocated'` ticket row from ANY
  worktree of the project is "pre-allocated, materialise HERE": rewrite `allocations.path` to the
  importing worktree in the `markTicketMaterialised` transaction (`store.go:2384` does not today).
  Refuse ONLY a MATERIALISED/recovered row whose path is a DIFFERENT existing file (the genuine
  colliding-number-different-path case). Between mint and import the row honestly reports
  `E_ID_UNRESOLVED` (`check.go:275-307`) — clears at import.
- FIELD-SCOPED IDEMPOTENT UPSERT keyed by id, via READ-MERGE (never render-from-row): first import →
  create id-preserving + set backlog fields; re-import → read the existing ticket, overlay ONLY
  title/status/severity/body/labels (backlog is truth — `severity` sourced per-row from `Sev`), re-render
  — leaving the LEASE, worktree-binding, and the existing `Relations` slice untouched. **Digest
  POST-merge** (read → merge → render → digest), not from a pre-read render-from-row, so
  unchanged/repaired detect correctly.
- STATUS force-set: bypass `ValidateTransition` (backlog is truth) via the file-write path
  (`UpdateTicketContent` enforces the transition, so this is a DELIBERATE new bypass vs the
  requirement importer, which has no transition graph). Test a graph-forbidden transition lands.
- A row that DISAPPEARS between imports: **report it under `absent_from_batch`; do NOT auto-retire.**
  "Previously imported" is DEFINABLE — a target with a journaled `events` row `verb='ticket.import'`
  (the importer writes one per row anyway), so a natively-created aira ticket (no such event) is never
  in the set. (The v2 "no durable marker exists" rationale was WRONG against source; the honest
  primitive is still to surface, not act — a backlog marking a row done/retired flows through the
  normal status refresh.)
- LINKS: strict-mode validates ALL endpoints in a pass-0 PROBE (endpoint valid iff in the batch id-set
  OR `exactRecord` finds it) and aborts with ZERO writes on any failure — mirroring `ImportFindings`
  strict (`import.go:89-118`), which is a pre-write probe (v2's "validate in pass 2 after files are
  written" could not be zero-write). Non-strict: an unresolvable endpoint is a NAMED per-row error
  (`E_RELATION_TARGET_MISSING`/`E_IMPORT_INVALID`), counted — never a silent skip. Apply: pass-1
  create/refresh all rows; pass-2 GROUP links by `CanonicalRelationOwner` (LOWER-id endpoint) and do
  ONE read-merge-render PER OWNER file — NOT one `Link()` per link (`Link` full-scans per call,
  `relation_ready.go:150`, and returns `E_RELATION_EXISTS`); re-applying an existing link is a NO-OP;
  an inverse-kind row (`blocked-by`) is a named row error. Deleted-backlog-link policy: links are
  ADD-ONLY across re-imports (a removed backlog link is NOT pruned — indistinguishable from an
  aira-added link without a per-relation origin marker); DOCUMENTED coverage gap, not silent.
- Summary: created/refreshed/reported-disappeared/errored counts + per-row errors; strict mode fails
  on any row error (mirror `ImportFindings`' strict flag).

- [ ] **Step 1 — RED:** import a 2-row JSONL (BL-1, NF-1) → both tickets exist with preserved
  (prefixed) ids; `id_counters` seeded.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** implement `ImportTickets`/`…Bytes` + wiring, modelled on
  `ImportRequirements`, with the read-merge upsert + post-merge digest.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — RED/GREEN — idempotent upsert + coordination preservation:** re-import with a
  changed title/status → refreshed; **an aira-ADDED relation (via `aira link`, present in NO JSONL
  row) + `Hold`/`Assignee`/`Milestone` placed BEFORE re-import all SURVIVE** — these frontmatter-
  resident fields are the mutation-sensitive third that reds a render-from-row impl (a lease +
  worktree-binding are DB rows and survive ANY file rewrite, so they are NOT the load-bearing
  assertion — assert them too, but the relation/Hold assertion is what proves read-merge); an
  unchanged re-import detects unchanged (digest post-merge); a dropped row → REPORTED under
  `absent_from_batch`, not retired; a natively-created ticket (no `ticket.import` event) is untouched.
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

- [ ] **Step 0 — PIN (design, verified):**
  - **ONE parser:** make `splitTicketID` return `(prefix, number, suffix)` and **DELETE
    `prefixOf`/`numberOf`** (their one non-test caller is `store.go:2384` — route it through
    `splitTicketID`), so there is a single suffix-aware parse and no second parser to forget.
  - **Column:** `suffix TEXT NOT NULL` with **NO DEFAULT** — an omitted `suffix` on an INSERT then
    fails LOUDLY (a `DEFAULT ''` would silently write the plain-twin key). `id_counters` stays
    `(project_id, prefix)→next_number` on the NUMERIC part (suffix-agnostic; the allocator never mints
    a suffix).
  - **RECREATION MECHANISM** (not a fresh CREATE — state.db is live machine-wide): on open, if
    `PRAGMA table_info(allocations)` lacks `suffix`, recreate via the `recreateProjectOwnedTable`
    shape (`schema_ownership.go:106-167`) in one `BEGIN IMMEDIATE` — `allocations_v2` with the
    4-column PK → `INSERT … SELECT *, ''` → DROP → RENAME.
  - **ALL `(prefix,number)` sites threaded** (grounding list — miss one and it's a silent wrong-row,
    not an error): the SELECTs `store.go:3029/3092/3199`; the retire `:3008`; the receipt-replay INSERT
    `:3202`; **the two reconcile-RECOVERY INSERTs `:3046` + `:3108`** (hit by every fresh clone); the
    other INSERTs `:2105/:2192`; `import_requirements.go:318` (`findRequirementAllocation`) + `:407`
    (the register-INSERT MODEL for the new `import_tickets.go`); `outbox_retire.go:295`;
    `requirement.go:247/277`; and **`check.go:263`** (the SELECT) + `check.go:283` (`fmt.Sprintf("%s-%d")`
    id-reconstruction — use the allocation's stored `path`, or include the suffix). The git-tree scan
    HWM (`store.go:4228-4239`) and the maxima callers (`store.go:2777/2823/2858/2867/2873`,
    `query.go:530`, `requirement.go:93`) are NUMBER-only by design — the `splitTicketID` signature
    change compile-forces them; say so, don't leave them unmentioned.
  - **Sort tiebreak:** add `suffix` as the final tiebreak in the record sorts (`query.go:528-537`,
    `requirement.go:92-99`) so `BL-10a`/`BL-10b` order is deterministic, not input-dependent.
- [ ] **Step 1 — RED (three legs):**
  1. `ValidateID("BL-10a")` accepts; `splitTicketID("BL-10a")` = (`BL`, 10, `a`).
  2. **Import** `BL-10` + `BL-10a` + `BL-10b` → THREE distinct tickets AND three distinct
     `allocations` rows, all `state='materialised'`, `aira check` GREEN (assert on
     `allocations.state`, NOT just the tickets table); `aira id BL` then mints `BL-<numeric-max+1>`.
  3. **Fresh-clone RECOVERY** (the path every fastest.ee clone takes — receipts in git, state.db not):
     write `FEE-BL-10.md` + `FEE-BL-10a.md` directly into `.aira/tickets`, fresh DB, NO receipts,
     `Rebuild` (`store.go:2725`) → two distinct rows (suffix `''` and `a`), `state='recovered'`, check
     green. (Import-then-check short-circuits at 3029 and never enters the recovery branch — this leg
     is what makes 3046/3108 mutation-sensitive.)
  4. **Migration:** open a DB seeded with the OLD `allocations` DDL + one row → the row survives with
     `suffix=''` and a suffixed INSERT then succeeds.
- [ ] **Step 2:** run; FAIL.
- [ ] **Step 3 — GREEN:** relax `idPattern` number part to an optional trailing single `[a-z]`
  (final: `^[A-Z]{2,}(-[A-Z]{2,})?-[1-9][0-9]*[a-z]?$`); make `splitTicketID` 3-tuple + delete
  `prefixOf`/`numberOf`; recreate `allocations` (migration shape); thread the suffix through the full
  Step-0 site list.
- [ ] **Step 4:** run; PASS.
- [ ] **Step 5 — commit:** `feat(aira): AIRA-237 — split-suffix ids (BL-10a): suffix in the allocations PK, suffix-aware parsers (splitTicketID+numberOf+check.go), numeric-only HWM`.

## Task 4 — allocator seed → mint-forward E2E + the dup-id gate (the load-bearing round-trip)

**Files:** an end-to-end test crossing import + allocate (single- AND cross-worktree); the
`--allocated-max` flag wiring on the import verb.

> This is what shape (2) rests on: import seeds the cursor → `aira id` mints forward → author a row
> with that id → NO stomp. Confirmed by source-read, NOT yet run end-to-end — it MUST be an explicit
> test. (The `check_no_duplicate_ids` gate is fastest.ee's, not aira's — it cannot run in an aira Go
> test; it is the CUTOVER backstop, Task 5.)

- [ ] **Step 1 — RED/GREEN — round-trip + cross-worktree:** import ids BL-1..BL-100 → `aira id BL`
  mints `BL-101` (next free, no stomp) → creating a ticket with that minted id succeeds → a second
  `aira id BL` gives `BL-102`; none of BL-1..100 altered. Include the FEE-prefixed variant
  (`id_prefix=FEE`, type `BL-101` → stored `FEE-BL-101`) and a split-suffix floor (import `BL-10a`
  alongside a high plain `BL-100` → `aira id BL` mints `BL-101`, above the numeric max).
  **CROSS-WORKTREE (the normal shape-2 flow): `aira id BL` in worktree A (records a `state='allocated'`
  row with A's path) → `aira import` the same id in worktree B → the row materialises (path rewritten
  to B), `aira check` green** — the flow single-worktree Step-1 misses and the `ImportRequirements`
  path-refusal would break (Task 2).
- [ ] **Step 2 — RED/GREEN — HWM seed via `--allocated-max` (interface + fencepost + gap):** the import
  verb takes a repeatable `--allocated-max BL=1217` flag (bare prefix, composed with `id_prefix`;
  value = LAST-ALLOCATED per the `id_alloc` counter), applied AFTER the row loop via the existing HWM
  upsert `next_number = MAX(current, N+1)`. Assert **`aira id BL` mints EXACTLY `1218` (N+1), then
  `1219`** — an off-by-one (`next_number = N`) reds this; reject `--allocated-max` below the imported
  numeric max is unnecessary (the `MAX` covers it). DURABILITY = ACCEPTED documented gap: the seed
  lives in `id_counters` (DB); Rebuild recomputes maxima from receipts (`store.go:2768-2779`), so a
  state.db LOSS drops the counter headroom and could re-mint a pre-cutover unmerged-branch id (window
  = per prefix until the first post-cutover mint; backstop = fastest.ee `check_no_duplicate_ids`;
  re-apply `--allocated-max` = one idempotent command, recorded in the cutover note).
- [ ] **Step 3 — commit:** `test(aira): AIRA-237 — E2E seed→mint-forward (cross-worktree, --allocated-max, N+1 fencepost, no stomp)`.

## Task 5 — verification + the cutover protocol (doc, not executed here)

**Files:** verification runs; a short cutover note appended to AIRA-237 / this plan.

- [ ] **Step 1:** `aira confine -- make test` — exact exit code (check `PIPESTATUS`, not the wrapper);
  green.
- [ ] **Step 2:** `aira confine -- make race` — 0 data races.
- [ ] **Step 3:** mutation spot-checks: break the `--allocated-max` HWM seed (drop the upsert) → the
  E2E mint-`1218` assertion reds; render-from-row instead of read-merge → the aira-added-relation/`Hold`
  survives-reimport test reds; drop the `suffix` from the PK → the BL-10/BL-10a distinct-rows +
  `aira check`-green test reds; leave the reconcile-recovery INSERT (`store.go:3046`) un-suffixed →
  the FRESH-CLONE recovery test reds (the site the by-line list is most likely to miss).
- [ ] **Step 4 — leave `idLess`/`splitID` UNCHANGED + prove canonical-owner stability:** do NOT touch
  `domain.idLess`/`splitID` (`ticket.go:596-625`) — re-ordering flips `CanonicalRelationOwner` and
  invalidates existing frontmatter (`ticket.go:227`). Add a test asserting compound + suffixed ids
  produce a STABLE canonical owner so no one "improves" the ordering.
- [ ] **Step 5 — document the CUTOVER (fastest.ee-side, executed WITH speed+subpipe when the verb
  lands, NOT here):** `make id` becomes a thin wrapper over `aira id <PREFIX>` in the FEE-namespaced
  project. **Atomic hand-off — install → quiesce → seed → flip → unfreeze**, NO window where both
  allocators are live:
  - **(0) Install** the new aira binary + restart `aira-daemon.service` — the proto struct change
    (`WorktreeScope.IDPrefix`) AND the on-open `allocations` recreation both need the NEW daemon.
    Follow the shared-daemon-restart care: `aira confine --list` for in-flight scopes + heads-up
    active sessions first (job-safe, mid-admission legs transient-blip).
  - **(1) Quiesce** allocation: hold `flock -x` on the `id_alloc` counter file
    (`id_alloc.py:75-88` takes `LOCK_EX` — it has NO explicit quiesce mode; the flock IS the quiesce),
    and read each prefix's LAST-ALLOCATED counter value under that lock.
  - **(2) Seed + import:** `aira import --tickets <rows.jsonl> --allocated-max BL=<last> --allocated-max NF=<last> …`
    (per prefix), so `id_counters` = `MAX(current, last+1)`. Record the `--allocated-max` values in
    the cutover note (re-application after a state.db loss is one idempotent command — the durability
    gap backstop).
  - **(3) Flip** the `make id` wrapper to `aira id`; **(4) Unfreeze** (release the flock).
  - `FEESPEND` has 0 live rows, so composing it to `FEE-FEESPEND` orphans nothing.
- [ ] **Step 6 — move the dup-id gate here (it is fastest.ee's, not aira's):** after the flip, run
  fastest.ee's `check_no_duplicate_ids` (the fail-closed backlog/REQUIREMENTS gate) through a
  seed→mint→author-row cycle as the CUTOVER backstop — it cannot run in an aira Go test. It should
  never fire if the seed + forward-mint is correct; if the round-trip ever slips it catches the dup.
- [ ] **Step 7:** record exact exit codes; nothing claimed green from truncated output.

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

- Compound `FEE-BL-123` in filename + frontmatter + key everywhere in `.aira/tickets`; `id_prefix_in_repo`
  GONE; no store-identity divergence (`store.go:4165`/`query.go:288`/`check.go:317`)?
- **Composition INSIDE the store** (`IDPrefix` on `store.Options`+`ScopeOptions`+`daemon.WorktreeScope`
  proto; composed in `Open`/`NewScope` for BOTH `Prefixes` AND `RequirementPrefixes`), NOT at
  `app/project.go:243` (test-only)? All four points covered: registration→`prefix_ownership`,
  `s.canonicalID` at every id-taking Store entry + the `id:`/`ticket:` matchers, `displayID` incl.
  `aira id`, worktree-audit? Step-6 through the DAEMON path (`ScopeFromProject→NewScope`)?
  `id_prefix ∉ prefixes` config check; telemetry `--ticket` left opaque?
- idPattern relaxed to `^[A-Z]{2,}(-[A-Z]{2,})?-[1-9][0-9]*[a-z]?$`; `validPrefix` relaxed;
  config-parse charset kept STRICT on bare prefixes; `id_prefix` validated `[A-Z]{2,}`?
- Split-suffix path (A): `allocations` recreated via `recreateProjectOwnedTable` (NOT a fresh CREATE);
  `suffix TEXT NOT NULL` NO default; ONE parser (`splitTicketID` 3-tuple, `prefixOf`/`numberOf`
  DELETED); every `(prefix,number)` site threaded incl. the recovery INSERTs `store.go:3046/3108`,
  `import_requirements.go:407`, `check.go:263/283`; FRESH-CLONE recovery RED + migration RED present;
  `id_counters` number-only; suffix sort-tiebreak; `idLess`/`splitID` UNCHANGED + stability test?
- Import: read-merge (not render-from-row) + POST-merge digest; overlay incl. `severity`;
  CROSS-WORKTREE `state='allocated'` mint accepted + path rewritten (NOT `import_requirements.go:245`
  refusal); frontmatter fields (Relations/`Hold`) survive re-import; strict validates endpoints BEFORE
  any write; one read-merge-render PER OWNER (not per `Link()`); dangling = named row error;
  disappear = REPORT (set = `events verb='ticket.import'`); deleted-link add-only gap?
- E2E round-trip present + mutation-guarded; `--allocated-max` flag = LAST-allocated, mint == N+1;
  cross-worktree mint→import E2E; DB-loss re-mint = documented accepted gap?
- Status force-set documented as a new bypass; kind/severity contract (P0 kept, P4→P3, default P2;
  per-file kind); unowned prefix → `E_IMPORT_INVALID`?
- Shape-(3) scaffolding OUT; cutover documented (install+restart → flock-quiesce → seed via
  `--allocated-max` → flip → unfreeze; dup-gate at cutover; FEESPEND 0-rows), not executed here?
