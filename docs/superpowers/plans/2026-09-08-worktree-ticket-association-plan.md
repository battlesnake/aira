# AIRA-176 — associate worktrees + agent/session identity with tickets, and classify staleness honestly

Status: **plan — §4's open questions resolved (§5, 2026-09-09); not yet built.**
This document proposed a design; it was deliberately not a committed
implementation spec the way tonight's AIRA-149/151/153 plans were, because the
owner asked for a plan first, not a build. §5 now records the gate decisions on
§4's four open questions, including the two places where those decisions
**override §2** — read §5 before §2.1 and §2.3.

## 0. Problem, stated precisely

Owner-reported pain: a recent session spent real effort manually auditing a
population of git worktrees to sort them into four buckets — merged (safe to
delete), no unique changes (safe to delete), superseded by other work
(probably safe, wants a second look), and genuinely holding lost work that
needed recovering before deletion. That triage was done by hand, per
worktree, with no tool support.

The design spec already names this as a known, unaddressed pain point
(`docs/superpowers/specs/2026-08-07-aira-design.md:16`, observing ~194 live
worktrees) and states worktree bindings are meant to be "ephemeral and
self-healing" (`:77`) — but **nothing in the codebase or docs says when a
worktree is finished, or classifies its git state**. Verified by direct
search: there is no merge-base / `IsAncestor` / `--merged` logic anywhere in
this repository. The only existing "clean vs dirty" check is `purgeDirty`
(`internal/daemon/eject.go:262-278`), which is scoped to `.aira/` only and
exists to gate a destructive `eject`, not to report worktree state generally.

## 1. What already exists to build on (verified from source, not assumed)

- **`WorktreeID`** (`sha256(git --git-dir)`, `internal/app/project.go:154-181`)
  is already a first-class identity, embedded in the primary key of tickets,
  requirements, findings, leases, area_hints, and the `worktrees` registry
  table itself. What is *missing* is any row that says "ticket X is being
  worked in worktree Y on branch Z" — every existing worktree_id column
  answers "which checkout produced this row", never "who is working where".
- **Leases** (`internal/domain/ticket.go:350-473`, `internal/store/lease.go`)
  are a proven liveness primitive: boot-id + monotonic-clock TTL (15 min
  default), CAS-guarded claim/heartbeat/release/steal, a `worktree` field
  already present on `HeldLease`. This is the right precedent for "is
  someone actively working here right now" — but its TTL makes it the wrong
  lifetime for "which worktree was this ticket ever worked in", which needs
  to survive long after the lease has lapsed.
- **`touch`/area hints** (`internal/store/area.go`) is the closest existing
  precedent for a ticket↔worktree association write: DB-only (no git file,
  no journal), keyed `(project_id, ticket_id, worktree_id, glob)`, gated on
  holding a live lease with a matching token *and* worktree
  (`area.go:407-416`), DELETE-then-INSERT replace. It has two read paths with
  different staleness semantics that matter as precedent: `liveAreaClaims`
  (joined against currently-live leases only) versus `TicketAreaGlobs`
  (all-time, ungated, used for review-tier path selection). A worktree
  binding needs the same split: "who is *currently* here" versus "who was
  *ever* here" are different questions with different answers.
- **Confine owner resolution** (`resolveConfineOwner`,
  `cmd/aira/main.go:1607-1640`) already has a fallback chain — `--owner` →
  `$AIRA_CONFINE_OWNER` → the worktree's own `WorktreeID` → an unforgeable
  `@cwd-<basename>` inference → `"unknown"` — and an attested/inferred
  distinction (`ConfineOwnerIsAttested`) that gates a kill-guard today. This
  is the existing, validated notion of "who" and is the right identity to
  reuse rather than inventing a third one.
- **Session identity is real but thin**: `AIRA_SESSION` is read in exactly
  one place (`cmd/aira/dispatcher.go:707`, rant-caller stamping only),
  free-text, unvalidated, and *not joined to `AIRA_CONFINE_OWNER`* despite
  the Skill guide itself mandating `AIRA_CONFINE_OWNER=<stable-session-id>`
  (`internal/core/skill.go:318`). Two names for what is meant to be one
  concept. Worth a one-line note to a future ticket; not this one's problem
  to fix.
- **The AIRA-72 orphan-scope reaper** (`internal/runner/confine_manage.go`,
  `confine_manage_linux.go`) is the precedent for the *safety discipline* a
  "recommend deletion" feature needs: a conjunction of independently-provable
  positive facts (never inferred from absence), a final kernel-arbitrated
  re-check immediately before any destructive action, "no signal → no
  action" rather than escalation, and — the sharpest lesson — the sibling
  stale-lease mechanism is explicitly documented as *"a TTL policy, not a
  death proof"* (`internal/daemon/paths.go:66-99`). Whatever this feature
  reports must be phrased the same way: name what was actually established,
  never assert a verdict the evidence doesn't support.
- **MCP + Skill are fully generated from one dispatch-table entry**
  (`internal/core/core.go`'s `dispatchTable()` + `applyDispatchMetadata()`):
  a verb's `Name`/`Usage`/`Args`/`Run`/`MCPTool` plus a
  `summary`/`safety`/`example` metadata entry is enough to get a CLI
  subcommand skeleton, a fully-specified MCP tool (JSON Schema is a direct
  projection of `ArgSpec`, nothing hand-written), and an entry in the
  generated `SKILL.md`/agent guide — automatically, with structural tests
  (`TestSkillSafetyGolden`, `TestIncludedDescriptorsHaveMCPTool`,
  `TestDispatchMetadataMatchesInstrumentedHandlerReads`,
  `TestCanonicalDispatchNamesAndAliases`, plus a CLI/MCP-parity round-trip
  test) enforcing it stays that way. The **one** hand-maintained exception is
  CLI argument parsing itself (`cmd/aira/main.go`'s `parseArgs` allow-map and
  `buildRequest` switch) — every new verb needs one hand-written arm there.
  Contextual *prose* ("when/why to use this") is never generated; it is a
  literal `out.WriteString` block added to `renderMarkdownBody`
  (`internal/core/skill.go:313+`), tested by the established "guide teaches
  X" pattern (a `{text, why}` table asserted present in both generated
  documents, `skill_test.go:634` on).

## 2. Design

### 2.1 A new, durable, DB-only binding table

```
worktree_bindings(
  project_id      TEXT,
  worktree_id     TEXT,      -- existing identity, sha256(git --git-dir)
  ticket_id       TEXT,
  branch          TEXT,
  base_ref        TEXT,      -- e.g. "origin/master" at registration time
  base_commit     TEXT,      -- the actual sha branched from
  owner           TEXT,      -- reuses AIRA_CONFINE_OWNER's identity, not a new namespace
  registered_at   TEXT,
  last_seen_at    TEXT,       -- refreshed by heartbeat/claim/any aira verb run from this worktree
  released_at     TEXT NULL,  -- set when explicitly closed out; NULL = still the live binding
  PRIMARY KEY (project_id, worktree_id, registered_at)
)
```

Deliberately **DB-only**, matching `area_hints`/`leases`, not a git file:
worktree paths and bindings are local-machine state, not durable project
history, and they change too often (branch switches, rebases, worktree
moves) to belong in `.aira/tickets/*.md`. This also matches the AIRA-175
lesson from tonight — a *cached, potentially-stale* record is a liability;
what makes this design safe is that the binding is only ever a **hint** for
which ticket/branch to inspect, never the source of truth for whether a
worktree is stale. The git state itself is always re-derived live (§2.2).

`(project_id, worktree_id, registered_at)` as the key (not a plain
`(project_id, worktree_id)` current-row) deliberately keeps history: a
worktree that gets repurposed for a second ticket leaves both rows on disk,
so a later audit can say "this worktree previously worked AIRA-140, now
working AIRA-176" rather than silently losing the first fact. The *current*
binding is simply the row with the latest `registered_at` and a NULL
`released_at`.

### 2.2 Classification is computed live, never stored

Given tonight's own AIRA-175 finding (a derived index that quietly
disagreed with the git files it was supposed to reflect), this design
deliberately stores **no classification verdict**. `aira worktree audit`
computes a small set of independently-named facts against the *current* git
and lease state every time it runs, exactly like `aira check`'s dimensions:

| Fact | How established |
| --- | --- |
| `has_uncommitted_changes` | `git status --porcelain` non-empty |
| `unique_commit_count` | `git rev-list --count <base>..<branch>` |
| `merged_into_master` | `git merge-base --is-ancestor <branch> <upstream>` |
| `pushed_to_any_remote` | branch (or its tip commit) reachable from any `refs/remotes/*` |
| `live_lease` | a currently-live lease exists for the bound ticket (reuses `IsLive`) |
| `bound_ticket_status` | the associated ticket's own `status` field, if a binding exists |
| `binding_confidence` | `explicit` (a `worktree_bindings` row exists) or `inferred` |

`binding_confidence: inferred` is the fallback path for a worktree that
predates this feature, was never registered, or whose binding row is gone —
inferred from the branch name (this project's own branches are already
consistently `aira<N>-<slug>`) and/or commit-message `AIRA-<N>:` prefixes on
the branch's unique commits (also this project's own established
convention, observed all night). Inferred bindings are reported and used the
same way as explicit ones, but always visibly labelled — never silently
promoted to the same confidence as an explicit registration.

**A recommendation, not a verdict**, is composed from these facts in the
report only — nothing is written back to the DB:

- all of {no uncommitted changes, zero unique commits} → **"nothing to
  lose — safe to remove"**
- {merged_into_master, no live lease} → **"merged — safe to remove"**
- {unique commits present, pushed_to_any_remote, no live lease, bound ticket
  is done/superseded} → **"looks superseded — verify the ticket's actual PR
  before removing"**
- {uncommitted changes OR (unique commits AND NOT pushed_to_any_remote)} →
  **"holds work nothing else has a copy of — recover before removing"**
- `live_lease: true` → **"active — leave it"**, overriding all of the above

Every recommendation line names the facts it rests on, in the same sentence
— the AIRA-72 lesson applied here: *"merged into master, no live lease"* is
what was proven; *"safe to remove"* is what follows from it, stated as a
conclusion the reader can re-derive, not an opaque verdict.

### 2.3 New verbs

- **`aira worktree register <ticket-selector>`** — `SafetyMutate`. Run from
  inside the worktree being registered. Captures `branch` (`git branch
  --show-current`), `base_ref`/`base_commit` (merge-base against the
  configured upstream, or an explicit `--base` override), and `owner` from
  the same `resolveConfineOwner` chain `aira confine` already uses (no new
  identity concept). Writes a new `worktree_bindings` row. Idempotent per
  ticket+worktree: re-running with the same ticket updates `last_seen_at`
  rather than duplicating a row; a *different* ticket appends a new row and
  implicitly closes the prior one (`released_at` stamped).

  This is the natural machine-checkable form of the "Start here" ritual
  `CLAUDE.md` and `docs/dev/agentic-development-loop.md` already mandate in
  prose (dedicated worktree, feature branch, record starting commit) — it
  does not invent a new obligation, it makes an existing one queryable.

- **`aira worktree audit [selector]`** — `SafetyReadOnly`. Enumerates
  worktrees via the existing `discoverWorktrees` (`git worktree list
  --porcelain`, the one mechanism already used for this), joins each to its
  current/most-recent binding (explicit or inferred), computes the fact
  table from §2.2, and prints the recommendation. No selector audits every
  discovered worktree; a selector scopes to one ticket or one path. Never
  deletes, never writes.

- **Deliberately NOT in scope for a first version: any `--delete` or
  `gc` mode.** AIRA should never autonomously decide a worktree is
  disposable — a "merged, no changes" worktree can still hold something a
  human cares about that git can't see (scratch notes, an untracked
  reproduction script). The audit's job stops at an honest recommendation
  with its own reasoning attached; removing a worktree stays a `git
  worktree remove` a human or agent runs after reading the report. A softer
  middle ground worth keeping open for later: `audit --emit-commands`
  prints the exact `git worktree remove`/`git push <rescue-branch>` commands
  for each row without ever executing them — deferred, not designed here.

### 2.4 MCP / Skill integration

Both verbs are ordinary dispatch-table entries — nothing about them needs
special-casing in the generation pipeline:

- `worktree-register` → `MCPTool: "aira_worktree_register"`, safety
  `Mutate`, one hand-written CLI arm.
- `worktree-audit` → `MCPTool: "aira_worktree_audit"`, safety `ReadOnly`,
  one hand-written CLI arm.
- Routing (`internal/core/routing.go`'s `Classify`): both touch the local
  git checkout directly (mirroring `discoverWorktrees`'s existing shell-outs
  and confine's own owner-resolution), so both classify as client-local, not
  daemon-proxied — consistent with every other git-reading verb in this
  codebase.
- New `renderMarkdownBody` prose block (`internal/core/skill.go`), placed
  beside the existing "Start here"-adjacent guidance: *register your
  worktree against its ticket when you start (mirrors CLAUDE.md's own
  ritual, now machine-checkable); before removing ANY worktree, run `aira
  worktree audit` rather than eyeballing `git status`/`git log` by hand —
  same "keep the primitive, teach the idiom" instinct this project already
  applied to AIRA-142's wait-loop idiom.*
- New "guide teaches X" tests (`skill_test.go`) pinning that prose, plus the
  new verbs will need entries in the existing structural golden lists
  (`TestSkillSafetyGolden`, `TestCanonicalDispatchNamesAndAliases`) — routine,
  not a design question.

### 2.5 Recovery guidance (the "genuinely lost work" case)

Not a new mechanism — a documented idiom, matching how this project already
handles stashing: for uncommitted changes found by the audit, the
recommendation names the existing named-ref-stash convention
(`CLAUDE.md`'s "Git stash safety" section) or, more simply, "commit to a
rescue branch and push it" before removing the worktree. For unpushed
commits, "push the branch somewhere before removing the worktree" is the
whole recovery story — `aira worktree audit` existing to make sure that
step is never skipped by mistake, not to automate it.

## 3. Explicit non-goals (write these down so a later reader meets a decision, not a gap)

- No automatic worktree deletion, ever, in this design.
- No new session/agent-identity concept — reuses `AIRA_CONFINE_OWNER`'s
  existing, validated, attested/inferred mechanism. The separate
  `AIRA_SESSION` inconsistency noted in §1 is real but is its own,
  much smaller, ticket.
- No attempt to make `worktree_bindings` durable across a full DB rebuild —
  it is local-machine, session-adjacent state, the same category as leases,
  and inherits the same "self-healing, not archival" posture the design
  spec already states for worktree registration generally.
- No daemon-side periodic scanning in v1. `aira worktree audit` is
  pull/on-demand, matching `aira check`/`aira reconcile`'s own "runs only
  when a command runs one" behaviour (verified: staleness alone does not
  today trigger anything automatically anywhere in this codebase) — a
  background version could be a v2 if the on-demand form proves useful
  first.

## 4. Open questions for whoever gates this

1. Is `worktree_bindings` the right shape, or should the *existing*
   `worktrees` table just grow columns? This plan's answer (a new table)
   trades a slightly bigger schema for keeping history and not widening the
   core registry row's responsibility — but it is a real either/or.
2. Should `aira claim <ticket>` *also* opportunistically write/refresh a
   binding as a side effect (since leases already carry a `worktree` field),
   in addition to the standalone `register` verb? This plan leans yes as a
   low-cost robustness addition, but does not treat it as load-bearing —
   `register` must work standalone regardless, since `claim` is not
   verified to be universally used today.
3. Exactly which upstream ref counts as "master" for `merged_into_master`
   and `pushed_to_any_remote` — this repo's own convention (confirmed
   tonight) is a single `origin/master`, but the check should read that from
   git config rather than hard-code it, and that needs a concrete design
   pass, not just a plan-level assumption.
4. Full two-loop, or the lighter path? This adds a persistent schema
   migration touching multi-session shared state, which argues for real
   rigor — but it is an observability/audit feature, not a correctness-
   critical coordination primitive (kill arbitration, lease CAS, ID
   allocation) in the sense `CLAUDE.md` reserves the mandatory full loop
   for. This plan's own recommendation: build with an internal plan,
   Fable-reviewed, not the full plan-review+gate loop — but this is a real
   judgement call for whoever picks it up, not settled here.

## 5. §4 resolved — gate decisions (2026-09-09)

Re-verified against master `c797a88`. §1's claims all hold; only line numbers
moved (`resolveConfineOwner` is now `cmd/aira/main.go:1895-1928`). Two things
§1 missed, both load-bearing below: the `internal/gitcontext` package, and the
fact that one worktree here routinely serves several tickets at once.

### 5.1 Q1 — a new table, keyed per (worktree, ticket), with no history dimension

**Decision: a new `worktree_bindings` table, not columns on `worktrees`; and the
key is `(project_id, worktree_id, ticket_id)`, which changes §2.1.**

Why a new table rather than widening `worktrees`:

- `worktrees` is an auto-upserted *discovery* row, not an assertion. `registerDB`
  (`internal/store/store.go:1936`) and `RegisterWorktree` (`:1985`) both
  `INSERT … ON CONFLICT DO UPDATE` it on essentially every command run in a
  checkout, and `markWorktreeActive` (`:3090`) flips `active` from the scanner.
  Nothing on that row was ever declared by an agent. Merging an asserted
  association into a row a side effect rewrites constantly conflates "AIRA has
  seen this checkout" with "someone declared what it is for", and leaves no way
  to tell "never registered" from "registered, then cleared" without inventing a
  sentinel column.
- Widening costs one `ensureColumnAdded` migration per column (the
  `ensureAreaHintsGeneration` pattern, `store.go:1226-1231`) on a table with 80
  rows here, almost none of which would ever carry a binding.
- A new table costs essentially **zero structural-test churn** — verified, and
  this is the part that would have been guessed wrong. The schema guard is
  structural, not a golden list: `projectIDTables`
  (`internal/store/schema_ownership_test.go:161`) enumerates every table
  carrying a `project_id` column and asserts it cascades to `projects`, and
  `Eject` sweeps by the identical rule via `projectTablesOnConn`
  (`internal/store/lifecycle.go:374`). A table declared with
  `project_id TEXT NOT NULL … FOREIGN KEY(project_id) REFERENCES
  projects(project_id) ON DELETE CASCADE` is therefore covered by the guard and
  swept by eject with no list to update anywhere.

Why the key changes from §2.1's `(project_id, worktree_id, registered_at)`:

- **§2.1 assumed one ticket per worktree, and that is false for this
  repository.** 17 of the last 300 commits on master carry multi-ticket
  prefixes — `AIRA-188/189/190:`, `AIRA-181/183/184/185:`, `AIRA-171/172:` — so
  one worktree serving several *live* tickets simultaneously is routine here,
  not an edge case. Under §2.1's stated rule ("a different ticket appends a new
  row and implicitly closes the prior one"), registering AIRA-189 would stamp
  `released_at` on a still-live AIRA-188 binding and the audit would then report
  a finished association for work in progress. `(project_id, worktree_id,
  ticket_id)` — the exact shape `area_hints` already uses (`store.go:898`) —
  represents the real relation and makes that mis-close unrepresentable.
- **The dropped history dimension costs nothing git does not already hold, live
  and drift-free.** A prior binding is either evidenced by commits on the branch
  — whose `AIRA-<N>:` prefixes §2.2's `inferred` path already parses — or it
  produced no commits, in which case there was nothing to lose. Keeping a
  second, drift-capable copy of a fact git owns is precisely the AIRA-175
  liability §2.2 invokes against itself, and an unbounded append-only table with
  no retention policy, added for an audit feature whose whole purpose is to
  recommend deletions, is the per-feature machinery `architectural-simplicity`
  forbids. Where the audit wants prior tickets it reads them off the branch,
  alongside every other live fact.

Resulting columns: `project_id`, `worktree_id`, `ticket_id`, `branch`,
`base_ref`, `base_commit`, `owner`, `owner_attested`, `registered_at`, plus the
projects FK. `last_seen_at` is dropped (see §5.2). `released_at` is dropped
too: a released binding is DELETEd, so a row means "declared" and its absence
means "not declared", with no third tombstone state to keep consistent.

One trap to name before a builder walks into it: **`ticket_id` carries no
foreign key to `tickets`, and must not be given one.** `tickets` is keyed
`(project_id, worktree_id, id)` (`store.go:830`) — one row per worktree holding
the file — so a ticket ID has no single parent row to reference. `area_hints`
has the same `ticket_id TEXT NOT NULL` with a projects-only FK; follow it
exactly.

### 5.2 Q2 — no; `claim` writes no binding, `register` is the only writer

**Decision: no opportunistic write from `claim`.** §4.2's "leans yes" is
overturned.

- **Routing forbids the cheap version.** `claim` is `RouteDaemon` — it falls
  through `Classify`'s default (`internal/core/routing.go:35-62`) — so its
  handler (`internal/core/core.go:1292`) runs inside the daemon, which cannot
  reach the caller's checkout. `branch` and `base_commit` are client-side git
  facts, and `owner` comes from `resolveConfineOwner` (`cmd/aira/main.go:1895`),
  a cwd-dependent CLI-face function that calls `app.Discover(ctx, ".")`. Making
  `claim` write a real binding therefore means widening `claim`'s argument
  surface — and its generated MCP schema — with client-resolved git fields, and
  making a lease operation depend on git succeeding. Lease CAS is one of the
  correctness-critical primitives `CLAUDE.md` names by hand; it is not the place
  to add a side-effecting write for a convenience.
- **It would corrupt the evidence grade the whole design rests on.** A binding
  written as a side effect of `claim` is indistinguishable from one an agent
  deliberately asserted, collapsing the `explicit`/`inferred` split that §2.2 —
  correctly, following `ConfineOwnerIsAttested`
  (`internal/runner/confine.go:754`) — treats as load-bearing.
- **The robustness it was reaching for is already in the DB and needs no write.**
  A held lease row already carries `worktree_id` and `actor`
  (`store.go:861-875`), so "who is working ticket X, from where, right now" is
  answerable today, and §2.2 already reads it as the `live_lease` fact. What the
  lease cannot do is *survive*: the table's own CHECK requires
  `worktree_id IS NULL` in state `free`, so `release` **erases** the
  association. That is the precise gap `worktree_bindings` fills — a gap in
  durability, not in capture — and `claim` writing a binding would not have
  closed it any better than `register` does.

Three consequences, recorded so they are not re-litigated:

- **`last_seen_at` is dropped from §2.1.** §2.1 would have it "refreshed by
  heartbeat/claim/any aira verb run from this worktree", which turns every
  `SafetyReadOnly` command into a writer — a thing the safety classification
  exists to forbid. With `register` the only writer, `last_seen_at` would always
  equal `registered_at`. Recency comes instead from the branch's own last commit
  date and from the live lease, both live facts.
- **`register` does not require a live lease**, unlike `touch`
  (`internal/store/area.go:396-416`, which demands a live lease with matching
  token *and* worktree). `touch`'s gate exists because area hints feed
  *arbitration* between concurrent agents, where a false hint misroutes someone
  else's work. A binding is a label for an audit, not an arbitration input, and
  gating it would make the start-of-work ritual fail before the claim and make
  it impossible to label a worktree after the fact — which is most of the ~80
  worktrees this feature exists for. The honesty counterpart is that the row
  records the owner *and whether it was attested*, so a binding declared by an
  unattested caller is visibly weaker evidence rather than silently equal to a
  claimed one.
- The "claim is already habitual, register might be forgotten" argument is
  cashed in where it belongs — §2.4's Skill prose teaches `claim` and `register`
  as one ritual — not in a hidden side effect.

### 5.3 Q3 — a fail-closed resolution chain ending in `unevaluated`; `@{upstream}` is banned

Two measurements on this repository force the decision:

- **`git symbolic-ref refs/remotes/origin/HEAD` → `fatal: ref
  refs/remotes/origin/HEAD is not a symbolic ref`.** The canonical git-native
  answer is *not set* on the very repository this feature is being built for, so
  a design that simply "reads it from git config" would be `unevaluated`
  everywhere it matters most.
- **`@{upstream}` is actively wrong for this purpose.** `branch.<name>.merge` is
  configured for 86 of 129 local branches, and for a feature branch it points at
  *itself*: `branch.aira29-dynamic-reserve.merge = refs/heads/aira29-dynamic-reserve`.
  Comparing a branch to its own pushed copy yields zero unique commits and
  `is-ancestor` true for every pushed branch — a blanket false "merged — safe to
  remove" across the entire population, which is the exact false-pass class this
  project treats as the worst failure mode. (`branch.aira72-verify.merge` is
  `refs/heads/master` instead, so the semantics are not even uniform across the
  repo.)

**Resolution chain for the integration ref — first positively-established source
wins:**

1. `--base <ref>` passed on the invocation.
2. the `base_ref` recorded on that worktree's own binding row at register time.
3. `.aira/config` → `git.integration_ref`, a new field on the existing
   `GitConfig` (`internal/app/project.go:33-39`; the config file already carries
   a `"git": {}` section). Free to add — `GitConfig.UnmarshalJSON` uses
   `DisallowUnknownFields`, and `aira-not-live-no-compat` means no migration is
   owed.
4. `git symbolic-ref refs/remotes/origin/HEAD`, when it is set.
5. otherwise **`unevaluated`, with a reason that names the remedy** — set
   `git.integration_ref`, or run `git remote set-head origin -a`.

Never `master`, never `main`, never `init.defaultBranch` (a repo-*creation*
default, not an integration target), never `@{upstream}`.

The consequence is deliberately **asymmetric, in the safe direction**.
`merged_into_master: unevaluated` suppresses the "merged — safe to remove"
recommendation entirely: a check that cannot establish its result reports
`unevaluated`, never a fake pass. It does **not** suppress the recovery
recommendations, because `pushed_to_any_remote` needs no integration ref at all
— it is containment of the branch tip in any `refs/remotes/*`, evaluable
whenever the refs are readable. So "holds work nothing else has a copy of —
recover before removing", the one bucket where being wrong actually loses work,
stays evaluable even when the integration ref does not resolve.

**Shape:** report each fact as the `Field{value | none | unevaluated, reason}`
tri-state `internal/gitcontext` already defines
(`internal/gitcontext/resolver.go:23-38`) — the package §1 missed. It is the
existing vocabulary for exactly this, and reusing it gives the audit its honesty
machinery with no new concept.

**One caution inherited from that same package:** `gitcontext` deliberately
refuses to trust a `.git/config` it cannot fully see, reporting
`unevaluated("config-include")` whenever an `include`/`includeIf` is present
(`resolver.go:330-334`). The audit must not re-derive the integration ref by
parsing config files itself for that reason; steps 3-4 above read it through
`git`, which resolves includes. That is consistent anyway — `merge-base
--is-ancestor`, `rev-list --count` and `status --porcelain` are all
subprocess-only, so the audit is a subprocess-git feature end to end. Every such
call must use `runGit`'s environment (`store.go:3885`:
`gitcontext.ScrubbedEnvironment()` plus pinned `LC_ALL=C`/`LANG=C`); an
inherited `GIT_DIR` overrides `-C` and would silently audit a different
repository (AIRA-93, `internal/gitcontext/env.go`).

### 5.4 Q4 — full two-loop, settled by the owner

The owner has directed the full two-loop for this build, so §4.4's own
recommendation ("build with an internal plan, Fable-reviewed, not the full
plan-review+gate loop") is **superseded**. Recorded here so a later reader meets
the decision rather than the superseded advice.

### 5.5 Scope notes carried into the build (observations, not decisions)

- **Branch and HEAD are free.** The single `git worktree list --porcelain` call
  `discoverWorktrees` (`store.go:3797`) already makes emits `branch
  refs/heads/<name>` and `HEAD <sha>` per worktree, and currently discards both.
  Across ~80 worktrees here, per-worktree subprocess work should be reserved for
  the facts that genuinely need it.
- **The `inferred` path covers 65 of the 76 worktree branches here** by name
  (`aira<N>-…`). The 11 that do not match are not failures to fix: two are
  `master`, several are harness worktrees (`worktree-wf_…` under
  `.claude/worktrees/`) that are not ticket work at all, and the rest are
  genuine but differently named (`investigate-aira91-92-…`,
  `review-whole-project`). A worktree with no ticket must report none, never a
  forced guess.
