# Whole-project simplification review — everything outside subprocess/slice management

- **Status:** review for the owner's decision. Nothing in this document has been
  implemented; every proposal is a proposal.
- **Date:** 2026-09-04
- **Reviewer:** Fable (fresh read; no prior involvement in any of these builds)
- **Base commit:** `9a65d47` (`master`), worktree `review-whole-project`
- **Prompt:** the owner, after the sibling review of the confine/admission
  subsystem (`docs/superpowers/reviews/2026-09-03-subprocess-slice-management-review.md`,
  PR #7 — still open at the time of writing) found real simplifications by reading
  one subsystem fresh: apply the same question to the rest of the project. *Is the
  accumulated complexity across the whole codebase earning its keep, or has an
  evening (several evenings) of successive, individually-justified additions
  produced more machinery than the problem needs?*
- **Scope read in full:** `internal/core/{core,routing,store_guard,response_contract}.go`,
  `cmd/aira/{main (front door, parsers, buildRequest, render),dispatcher,
  write_relay_store}.go`, `internal/store/{store (options, open, schema, migrations,
  write protocol, reconcile),mutate,scan_read,check,gate_eval,gate_index,gate_audit,
  relation_ready (ready fold),insights (registry),lifecycle (head),search,watch,
  schema_ownership}.go`, `internal/gate/{gate,canary,verdict}.go`,
  `internal/daemon/{storeops,watchdog (emit)}.go`, `internal/app/project.go`, the
  top-level design spec, the M10 gates spec (threat model), the M21/D5/D7/D7b
  specs (single-writer wording), the rant and ratchet specs (rationale lines), the
  agentic-development-loop doc, tonight's tickets AIRA-52..70 (root checkout), and
  the live machine-wide `state.db` (read-only) plus this repository's common-dir
  journal. Six sub-agents mapped the remaining files package by package
  (`internal/store` in full, `internal/core`+`internal/app`+`cmd/aira` in full,
  `internal/daemon` non-runner files, `internal/gate`+`domain`+`gitremote`+
  `gitcontext`+`install`, `internal/pylib`, and a chronology of the 36 non-runner
  specs and 70 tickets); every sub-agent claim graded HIGH below was spot-checked
  against source by me (§7.1 lists which).
- **Not done:** no tests were run; no code was changed; nothing was restarted.
  The only commands run against the shared daemon were read-only queries
  (`aira list/count/find ls/req ls/rant ls/test-report ls/spend ls/quota ls/
  commands count/lease ls/gate ls/insights ls|show`) and a read-only SQLite open
  of `state.db`.

---

## 0. Verdict

The half of AIRA that PR #7 did not cover is not one over-engineered design either.
It is **a complete Phase 1–4 product, built to specification through the two-loop,
sitting under a face layer that re-declares every verb by hand — and, on this
machine, almost none of it has ever been used.**

The numbers, from the live machine-wide `state.db` (five projects, 590 commits,
four weeks): tickets 85, relations 43, rants 17, leases claimed 7 times ever,
requirements 7 (imported once). **Findings 0. Gates 0. Test reports 0. Compute
events 0. Command events 0. Quota snapshots 0. Area hints 0. Supervisor leases 0.**
On this repository, `aira insights show` reports nine of its ten gauges
`unevaluated` (only `wip` computes). The code that serves those zero-row tables is
roughly 11,000 of the store's 17,700 production lines, six of its forty-one tables
by concern, and the entire `internal/gate` package.

That is the direct explanation for tonight's bug cluster. AIRA-53 (`gate add`
wrote nothing for three weeks), AIRA-54 (an empty gate set read `pass`), AIRA-55
(canaries were Go-only), AIRA-57 (every verb dumped HTML-escaped JSON) and the new
finding in §5 B1 (the generated skill tells agents to launch aitest in a form the
launcher never wires up) are all the same shape: **the documented surface and the
executed surface are different artifacts, kept in sync by hand, and nothing walked
the path.** The two-loop reviewed each addition against its own spec and tests; it
could not review the path nobody ran. The confine/admission side has all the
*observed* bugs because it is the only side with users.

Underneath that, two structural facts on the store side produce the latent-bug
class the same way PR #7's "ledger truth in daemon memory" produced the admission
class:

1. **Truth is kept in two or three places, and the second copy is never read.**
   The `tickets`/`relations`/`requirements` SQLite projections are not the read
   path — every `get`, `list`, `ready` and `grep` rescans the git files — so the
   projection exists only to be reported stale (`W_STALE_INDEX` was live on this
   repo during the review). Five of the six gate tables have no reader at all.
   The gate ledger is HMAC-"authenticated" with a key that sits beside it under
   the same UID. The watchdog writes 245 events into a project's `seq` space that
   never reach the journal, so the journal's "gap-detectable" property is false by
   construction. The outbox has a `resolution` column nothing ever sets, so a
   single write conflict bricks a ticket path permanently. The detached supervisor
   opens the daemon-owned DB read-write because two specs each deferred folding
   it to the other.
2. **The verb surface is declared in seven places** (three inside `core.go`, the
   CLI's `parseArgs` allow-list and its inline boolean/list flag lists, the
   660-line `buildRequest`, `routing.go`'s two classifier lists, the MCP face's
   zero-default mirror), although `ArgSpec` already carries everything a generic
   parser needs. Six verbs live in the dispatch table only as stubs that return
   `E_*_UNAVAILABLE`; eleven CLI verbs are not in it at all; `aira help` omits
   eight of them.

If the owner acts on three things (§6): generate the CLI from `ArgSpec` and
delete the hand mirrors; make the store's second copies either the read path or
gone (and fix the four truth-splitting bugs that come with that); and shrink the
gate subsystem to the core that has actually been proven — the verdict fold,
proof binding, subject digest, and the mutation canary — deleting the
write-only projection, the HMAC layers, nine dead definition fields, and the
ratchet machinery until test reports exist to ratchet over.

Two things are new and should not wait for any of that: B1 (the aitest activation
contract in the installed skill is wrong) and B2 (a manual or ratchet gate's
stored `pass` survives every non-Go change, so on a non-Go repository one
attestation reads `pass` forever).

---

## 1. Method and confidence grading

I read the faces and the store's write path first and formed the map before
reading any spec, so §2 describes what runs. Then I read the specs and tickets
for every mechanism I intended to propose cutting, so that each proposal in §4
names the decision that created the mechanism and how the incident it addressed
stays covered. Live data (§2.6) was gathered last and turned out to be the most
important input.

Grades, as in PR #7:

- **HIGH** — traced line by line by me, or traced by a sub-agent and re-traced by
  me at the cited lines; I would bet on it without a repro.
- **MEDIUM** — mechanism traced, consequence not observed; or one step rests on
  a sub-agent's trace I did not fully repeat.
- **LOW** — pattern-matched or cosmetic; flagged so it is not lost, not asserted.

File references are `path:line` against `9a65d47`.

---

## 2. The map — what exists today

### 2.1 What the design asked for, and what grew

`CLAUDE.md` and the design spec §5.1 name six layers:

```
internal/store/   internal/domain/   internal/query/   internal/interp/   internal/runner/   cmd/aira/
```

What exists at `9a65d47` (production lines / test lines / `func Test` count):

| Package | Prod | Test | Tests | In the layout diagram? |
|---|---|---|---|---|
| `internal/store` | 17,662 | 15,146 | 420 | yes |
| `internal/runner` | 11,588 | 14,208 | 387 | yes (PR #7's scope) |
| `cmd/aira` | 7,807 | 7,659 | 211 | yes |
| `internal/daemon` | 7,207 | 10,617 | 302 | no ("optional daemon" in §5.2) |
| `internal/core` | 4,534 | 5,398 | 138 | implied by "`core.Do`" |
| `internal/domain` | 2,132 | 1,012 | 44 | yes |
| `internal/install` | 1,771 | 1,682 | 39 | no |
| `internal/gitremote` | 1,166 | 1,137 | 38 | no |
| `internal/gate` | 1,021 | 415 | 14 | no |
| `internal/app` | 879 | 347 | 15 | no |
| `internal/gitcontext` | 579 | 301 | 8 | no |
| `internal/pylib` (Go) | 428 | 1,920 | 42 | no |
| `internal/pylib` (Python) | 2,353 | 3,608 | 112 | no |
| `internal/cgrouptest` | 207 | 0 | 0 | no |
| **Total** | **~59,300** | **~63,400** | **1,770** | |

`internal/query/` and `internal/interp/` were never created: the selector grammar
lives in `internal/store/query.go` and there is no command language (the
command-telemetry spec even reserves the verb `exec` for `internal/interp`,
`2026-08-18-command-telemetry-design.md` §2). Seven packages grew that the layout
does not name. The design spec itself was edited in place on 2026-08-29 to say
"where a later spec supersedes a decision here, the later spec wins" (line 3), so
the map is now self-amending and the git history of the spec is the only stable
record of what was originally decided.

### 2.2 Subsystem inventory

Every distinct mechanism in scope; LOC is production Go unless marked. "Live rows"
is the machine-wide `state.db` at review time across all five registered
projects, or the repository's common-dir for file ledgers.

| # | Mechanism | Where | LOC | Purpose | Motivating spec / pain | Live rows | Interacts with |
|---|---|---|---|---|---|---|---|
| S1 | **Write protocol** — outbox intent → receipt → path-locked atomic file write → mark materialised → journal → mark journaled; `reconcile` replays from the first incomplete stage | `store.go:1595-2131` | ~800 | ID allocation that survives crashes, rebases and sibling worktrees; git file is the content authority | Phase-1 §2/§5 (the `make id` receipt-log pain); design §7 | allocations 80, outbox 247 (all materialised), events 509 | S2, S3, S5 |
| S2 | **Journal + receipts** — per-project `seq` from `event_counters`; `events` table; common-dir `journal.jsonl`/`receipts.jsonl` (append-only, torn-tail repair, flock) | `store.go:1930-2015, 3075-3271` | ~350 | Ordering authority and cross-worktree receipt evidence outside the commit graph | design §11; Phase-1 §2 | journal 244 events (this repo) | S1, S11, S13 |
| S3 | **Rebuild + Check** — full multi-worktree scan, receipt/journal/ref reconciliation, counter re-raise, 14-dimension consistency report; `Check` runs `reconcile` and `Rebuild` | `store.go:2223-2759`, `check.go` | ~1,100 | DB loss recovery; integrity findings with stable codes | design §5.3/§7; Phase-1 §8 | — | everything |
| S4 | **Torn-read scanning** — double read + compare, `O_NOFOLLOW`, bounded retry, `inconclusive` outcome | `scan_read.go`; copies in `traceability.go:54`, `gate_command.go:286`, `gate_index.go:59` | ~120 + 3 copies | Working-tree authority without reporting a fake fail/pass on a torn file | #8-part2 (three real honesty violations) | — | every read path |
| S5 | **Selector/query grammar + ticket reads** — `List/Get/Count`, `records()` scanning `.aira/tickets` per call, distributions, 50-row cap | `query.go`, `relation_ready.go:1-480` | ~1,100 | The CLI's `list`/`show`/`count`/`link` | Phase-1 §6/§10 | tickets 85 (82 here) | S4, S6 |
| S6 | **Ready queue** — blocker graph from canonical relation files, index-divergence hints, gate fold | `relation_ready.go:440-606` | ~170 | "highest-value early feature" (design §9) | Phase-1 §10 | relations 43 | S5, S14 |
| S7 | **Leases + area hints** — CAS claim/heartbeat/steal, monotonic clock + `boot_id`, token file outside the DB, glob-intersection overlap warnings | `lease.go`, `area.go` | 1,287 | "too many cooks" without hard file locks | design §8; Phase-1 §7; D1 reaper | leases 7 rows (all `free`), area_hints 0, 14 lease events ever | S1, daemon reaper |
| S8 | **Findings** — git-file review findings + DB reconciliation findings, content-keyed identity, waivers, JSONL import | `finding.go`, `import.go` | 847 | "findings evaporate" (design §2 pain #2) | M5, M7 | **0 review findings** machine-wide | S3, S12 |
| S9 | **Requirements + traceability** — `.aira/requirements/*.md`, markdown-table import, `covers:`/`verifies:` Go-comment graph scan, traceability check dimension | `requirement.go`, `import_requirements.go`, `traceability.go` | 1,311 | Phase-3 traceability | M9 | 7 requirements (one import, `stoner`); 0 here | S3, S14 |
| S10 | **Test reports + flaky** — `go-json`/JUnit ingest, identity tuple, cell-level three-state flaky classifier, retention | `testreport.go`, `testreport_parse.go` | 838 | design §13 | M13 | **0** | S12, S15 |
| S11 | **Telemetry: compute, quota, command events** — provider-normalised token buckets with conservation check, quota snapshots, `aira time` command events with their own query grammar and p50/p95 | `compute.go`, `command.go`, `domain/compute.go`, `core/command.go` | 1,436 | design §12; command-telemetry spec ("a slowdown investigation has no evidence today") | **0 / 0 / 0** | S12, D7b relay |
| S12 | **Insights** — 10 registered gauges with universe/as-of/drilldown | `insights.go`, `admission_insight.go` | 933 | design §17 ("live query, never a stored numeral") | 1 of 10 computable here | S8–S11, S14 |
| S13 | **Rant** — `RANT-n`, tags, typed refs, append-only reviews (two triggers), physical redaction, caller-observed git context stored as an EAV table | `rant.go`, `domain/rant.go`, `gitcontext/` | 1,437 | friction capture (rant spec §1) | rants 17, reviews 0, git-context rows 102 | S2, FTS |
| S14 | **Gates** — definition files, four checkers, four canary modes, HMAC-chained audit ledger, six SQLite projection tables, verdict fold, `check`/`ready` fold, ratchet baselines, `gate add/set` materialisation (since 2026-09-03) | `internal/gate/`, `store/gate_*.go` | 4,069 | design §9 ("a green gate must have actually fired") | **0 gates** on any project; no ledger exists for this repo | S3, S6, S10, runner (command checker) |
| S15 | **Review tier** — path/kind/severity → tier, MAX-over-factors, `aira review` bundle | `review_tier.go` | 409 | design §16 | — | S7 (area globs) |
| S16 | **Lifecycle** — init/adopt, eject with live-state + durability guards, FK-cascade ownership, tombstones, registry trim | `lifecycle.go`, `schema_ownership.go`, `daemon/eject.go`, `app/project.go` (init) | ~1,300 | a throwaway project squatted the `AIRA` prefix (lifecycle spec) | ejections 2 | S1, daemon |
| S17 | **Schema migrations** — seven `ensure*` probes on every open + a runtime FK-rewriter of 25 tables + pre-M9/pre-M21 compat branches | `store.go:1065-1470`, `schema_ownership.go` | ~700 | upgrading older `state.db` shapes | M5/M6/M9a (08-09/10), rant (08-18), lifecycle (08-27) | — | every `Open` |
| S18 | **Core dispatch** — 43-entry table, `argAccessor`, `handlerData` unwrapping, verdict→exit, `DispatchDescriptors` projection, generated MCP/Skill/help | `core/core.go`, `core/skill.go`, `cmd/aira/mcp.go` | ~3,800 | "one core, thin faces; generate every doc surface" (design §4) | — | all faces |
| S19 | **CLI face** — `Run`, 12 hand flag parsers, `buildRequest`, 7 renderers, TTY default | `cmd/aira/main.go` | 2,420 | the CLI | Phase-1 §11; AIRA-57 | — | S18, daemon dispatcher |
| S20 | **Daemon dispatcher + relay** — route classify, auto-start race loop, protocol-version replacement, `ensure-scope`, read-only store + six relayed writes with response validation | `dispatcher.go`, `write_relay_store.go`, `daemon/storeops.go`, `core/routing.go` | ~1,400 | mandatory DB-owning daemon (M21 amendment); D7a/D7b | — | S18, daemon |
| S21 | **Daemon core (non-runner)** — framed protocol v5, 47 server-side ops, scope cache, discovery, reaper, journal flusher, watch, supervisor leases, eject | `daemon/{server,protocol,paths,discovery,watch,supervisor_lease,eject,service}.go` | ~2,700 | M21, D1–D3, D5, discovery, install-service, lifecycle | supervisor_leases 0 | everything routed |
| S22 | **TUI** — 4-layer reducer/executor/renderer, palette, inline edits, foreground and detached execute | `cmd/aira/tui*.go` | 3,100 | a human reading indented JSON (TUI v1 §1) | — | S20, watch |
| S23 | **`aira git`** — bounded clone/fetch/push/ls-remote with SSH→gh fallback, 9 codes | `gitremote/` | 1,166 | agents doing git network ops badly (gitauth §1) | — | core `GitOps` seam |
| S24 | **aitest** — fork+admission pytest worker pool; Go embed/extract; env injection | `pylib/` | 428 Go + 2,353 py | replace xdist (aitest spec) | — | runner confine, daemon `worker-admit` |
| S25 | **Install** — slice, anchor, daemon service, oomd/delegation drop-ins, root re-exec | `install/` | 1,771 | #55/#62 | live units | daemon |

Adjacent and already reviewed (PR #7): the runner, `daemon/{admit,watchdog,
governor,worker_admit,confine_*}.go`, `aira_xdist_governor`, `confine_peak_history`
(10,253 rows — the one telemetry table with real data).

### 2.3 Life of one `aira create` and one `aira time`

**`aira create "title"`** (daemon-routed, the common case):

1. `main.go:80-100` strips `--json`, decides `renderJSON` from the TTY, lower-cases
   the verb; `parseArgs` (`:500-618`) walks argv with a hand-maintained boolean
   list (`:537`), list-valued list (`:567`) and per-verb `allowed` map (`:573-605`).
2. `buildRequest` (`:1359-2017`) — the `create` case joins positionals into
   `title` and copies four options into `Args`.
3. `daemon.PathsFromEnv`, then `scopeForCWD` → `app.Discover` (three `git
   rev-parse` subprocesses, each retried 3× with a 10 s deadline,
   `project.go:139-179, 789-828`) → `daemon.ScopeFromProject`.
4. `newDaemonDispatcher` → `Dispatch` (`dispatcher.go:126-156`): `Classify`
   (`routing.go:35`) says daemon; `stampRantCaller`, `stampGitContext` (no-op
   here); frame → `exchangeWithReplacement` → `exchangeOrStartUsing`
   (`:452-569`, a 120-line start-if-absent race loop with a flock, a fork of
   `/proc/self/exe daemon` or `systemctl --user start`, and 20 ms polling).
5. Daemon `serveConnection` (`server.go:518-669`): version check, state-id pin,
   scope cache lookup (`storeForScope`, recomputes identity from canonical paths),
   `core.New(view).Do`.
6. `core.Do` (`core.go:511-585`) → the `create` closure (`:723-730`) →
   `store.CreateTicketWithEvent` → S1: `BEGIN IMMEDIATE` (counter, seq,
   allocation, outbox, event), receipt append, path lock, atomic file write, mark
   materialised (also upserts the `tickets` projection and replaces `relations`),
   journal append, mark journaled.
7. Response frame back; `render` (`main.go:2165`) → `renderHuman` or JSON.

Seven hops, three git subprocesses, one SQLite transaction plus two more for the
marks, three fsync'd file appends. The read side of step 6's projection is never
used by `show`/`list` (§2.5).

**`aira time -- go test ./...`** (client-routed):

1–3 as above, but `parseTimeArgs` (`main.go:1286`) instead of the generic parser.
4. `Classify` says client → `dispatchClient` (`dispatcher.go:264-343`): an
   `ensure-scope` store-op round trip to the daemon; **`app.Discover` again**
   (three more git subprocesses); `validateProjectSnapshot` re-derives the scope
   and compares; `app.BuildWithoutStore` constructs a full `runner.Runner`, a
   `gitremote.Client` and opens the gate audit handle; `store.OpenReadOnly` on
   `state.db` (`mode=ro&query_only`); wrap in `writeRelayStore`.
5. `core.Do` → `runCommandTime` (`core/command.go`): exec with signal forwarding,
   classify exit, `pylib.AppendChildEnvironment` (the xdist governor's
   coordinates are injected into every timed command), build a `CommandEventInput`.
6. `writeRelayStore.AddCommandEvent` → `add-command-event` store-op frame →
   daemon `runStoreOp` (`storeops.go:217-264`) → `view.AddCommandEvent` → S1-style
   transaction on `command_events` + `command_event_counter`.
7. `renderTime` prints nothing on success.

Six git subprocesses, two daemon round trips, a runner and a git client
constructed and discarded, to record one row into a table that has zero rows.

### 2.4 The faces: where a verb is declared

For the `gate` verb (the worst case) and `create` (a typical one), the places a
maintainer must edit to add or change an argument:

| Declaration | Where | Enforced how |
|---|---|---|
| `verbSpec.Args []ArgSpec` (name, kind, required, positional, enum, description) | `core.go:1868-1876` | `argAccessor.reads` records every read — but the `reads` map is never consumed (`core.go:112-127`) |
| `applyDispatchMetadata` map (summary, safety, destructive, example, per-operation `OperationSpec.Args`) | `core.go:1989-2113` | `panic("missing dispatch metadata")` at construction |
| `gateDefinitionOperationArgs()` / `mutationOperationArgs()` | `core.go:2415-2431` | none |
| `parseArgs.allowed["gate"]` (CLI option allow-list) | `main.go:604` | none |
| `parseArgs` boolean-flag list and list-valued-flag list | `main.go:537, 541, 567` | none |
| `buildRequest` `case "gate"` | `main.go:1943-2012` | none — accepts `--gate_id`/`--canary_id` from `allowed` then ignores them (positional only) |
| `routing.Classify` (`gate run|canary-run` → client) and `StoreFreeCarved` | `routing.go:53, 96-118` | daemon refuses client-only verbs (`server.go:626-629`) |
| MCP zero-default lists and per-verb special cases | `mcp.go:140-154, 191-194, 429-511` | none |
| TUI palette / inline / execute request builders | `tui_palette.go:89-150`, `tui_inline.go:55-89`, `tui_execute.go:258-289` | none |

`ArgSpec` already carries `Kind ∈ {string,bool,stringlist}`, `Positional`,
`Required` and `Enum` — the complete input to a generic parser. MCP and Skill are
generated from it; the CLI is not. Six verbs (`confine`, `confine-reserve`,
`confine-list`, `confine-kill`, `install`, `eject`) exist in the table as stubs
whose `Run` touches every argument (`_ = stringArg(...)`) and returns an
`E_*_UNAVAILABLE` error (`core.go:706-717, 1706-1767`) — registered so the
generators see them, executed elsewhere. Eleven CLI verbs (`daemon`, `mcp`,
`skill`, `tui`, `watch`, `governor-slot`, `aitest-bootstrap`, `worker-admit`,
`__supervise`, `__confine-setup`, `__slice-anchor`) are not in the table at all,
so `aira help` omits eight user-facing ones and `core.Do("watch")` returns
`E_UNKNOWN_VERB` while the daemon special-cases it (`server.go:634-645`).

Rendering is not one function either: `render`, `renderHelp`, `renderHuman`
(whose "human" mode for a non-verdict response is indented JSON), `renderConfine
ListResponse`, `renderRunLog`, `renderTime`, `printWatchEvent`, plus raw `Fprintf`
in five runner-side verbs, `toolResponse` in MCP (which still HTML-escapes,
contradicting the rule at `core.go:74-80` that AIRA-57 introduced), and
`MarshalIndent` in the TUI.

### 2.5 The store's truth map

This is the table that explains the latent-bug class. For each entity: where the
authoritative copy lives, what else is written, and who reads the second copy.

| Entity | Authority | Second copy | Who reads the second copy |
|---|---|---|---|
| Ticket content | `.aira/tickets/ID.md` (git) | `tickets` table (id, path, digest, status, hold, title, kind, severity) | `checkStaleIndex` and `indexedDigests` — to emit `W_STALE_INDEX` (`check.go:404`, `query.go:339`). `List`/`Get` scan the files (`query.go:241`). |
| Relations | ticket frontmatter (canonical lower-id owner) | `relations` table | `Ready`'s `canonicalFindingsForIndexedRelations` "uses the disposable index only as an uncertainty hint" (`relation_ready.go:625-662`); `Relations()` scans files (`:277`). |
| Requirements | `.aira/requirements/ID.md` | `requirements` table | `checkStaleIndex` only. |
| Review findings | `.aira/findings/key.md` | `findings` rows with `subtype='review'` | `findingIndexDivergence`; `ListFindings` scans files. |
| Reconciliation findings | `findings` rows | — | `Check` (authoritative in DB; correct) |
| Search | files + rants | `search_fts` | `Search` — after rebuilding it from scratch on every query (`search.go:43-48, 101-128`) |
| Events/ordering | `events` + `event_counters` | `journal.jsonl` | `Rebuild` on DB loss; `watch` reads `events` |
| Watchdog decisions | `events` (actor `aira-watchdog`) | *none* — `AppendWatchdogEvent` (`store/watch.go:11-19`) allocates a `seq` and inserts an event with no outbox row, so the journal flusher never sees it | 245 rows `journaled=0` live; the common-dir journal has 244 events and 245 gaps |
| Rant events | `events` + `journal.jsonl` | no outbox row (`rant.go` never touches `outbox`) | a crash between COMMIT and journal append is unrepairable — both replayers select from `outbox` (`store.go:2138, 2171`) |
| Gate definitions | `.aira/gates/*.json` | `gates` table | `rant.go:510` (ref existence check) |
| Gate results / proofs / attestations / baselines | HMAC-chained `common/aira/gates/audit.bin` + `HEAD` + `hmac.key` | `gate_results`, `gate_proofs`, `gate_attestations`, `gate_baselines`, `gate_baseline_active` | **nobody** (grep of all non-test code) |
| Outbox intents | `outbox` | — | `resolution` is compared `IS NULL` at 12 sites and **never written** |
| Detached-run telemetry | `state.db` via the daemon (D7b) | the `__supervise` process opens `state.db` read-write itself (`main.go:387` → `app.OpenWithDiagnostics` → `store.Open`) and calls `AddTestReport`/`AddComputeEvent` directly (`run_wiring.go:261, 272, 391`) | — |
| Counters | `event_counters`, `id_counters`, `test_report_counter`, `rant_counter`, `compute_event_counter`, `command_event_counter`, `quota_snapshot_counter` | — | seven counter tables; five with the identical `(project_id, next_number, next_seq)` shape and five copies of `next*Numbers` |

### 2.6 Size, churn, tests, data

- Production Go in scope: **~47,700** lines (all packages except `runner`);
  tests **~49,000**; Python 2,353 + 3,608. 1,383 `func Test` outside the runner.
- `core.go` and `main.go` have each been touched by 63 commits; `store.go` by 67;
  `check.go` by 51 (inflated by the `ExitCodes` catalog living there — 176 stable
  codes, edited by every milestone).
- The `*Store` type has **221 methods**; the `core.Store` interface exposes 51,
  plus fourteen optional capability interfaces discovered by type assertion at
  call sites (`core.go:182-216, 921, 1052, 1085, 1147, 1258; run_wiring.go:109`).
- The `Store` struct carries **18 test-only hook fields** (`store.go:161-213`)
  plus four package-level test seams; each is used by one or two test files.
- Live data, machine-wide (`~/.local/state/aira/state.db`, read-only, 2026-09-04):
  5 projects; 9 worktrees (8 active); tickets 85; relations 43; rants 17; leases 7
  (all free); requirements 7; **findings 0; gates 0 (all six tables); test_reports
  0; compute_events 0; command_events 0; quota_snapshots 0; area_hints 0;
  supervisor_leases 0**; outbox 247 (all materialised); confine_peak_history
  10,253; ejections 2. Events by verb: `watchdog.trip` 121, `watchdog.defer` 120,
  `ticket.update` 97, `ticket.create` 72, `relation.add` 44, `requirement.import`
  17, `rant.create` 17, `lease.claim` 7, `lease.release` 4, `lease.lapse` 3,
  `watchdog.outcome` 2, `relation.remove` 2, `watchdog.recovered` 1,
  `watchdog.intent` 1, `id.allocate` 1.
- `aira insights show` on this repository: exit 3, nine gauges `unevaluated`
  ("no findings", "no compute events", "no recorded command events", "no quota
  snapshots", "no ratchet gates configured", "no sufficiently-evidenced cells",
  "no estimate-mode runs recorded", `U_TRACE_UNSCANNED` from one malformed
  `covers:` annotation at `cmd/aira/confine_test.go:233`); `wip` computes.
- `registry.jsonl`: 96 breadcrumbs, never pruned; `common/aira/locks/`: 88 path
  lock files, never removed.

---

## 3. Where the complexity is doing real work

These are hard to reason about **and** earn it. I would not simplify them without
a specific reason, and none of §4 touches their invariants.

**3.1 The write protocol and its crash matrix (S1).** DB intent first, receipt
second, file third, journal last, with `reconcile` resuming at the first
incomplete stage (`store.go:2035-2131`) and the partial unique index
`unresolved_path_intent` preventing two intents on one path. This is the direct
mechanisation of the `make id` receipt log the design §2 names as pain #1, and
the Phase-1 spec's 32-process `BEGIN IMMEDIATE` evidence is real. Every piece
closes a hole: without the receipt-before-file order a rebase can eat an ID
without trace; without the path lock two worktrees can race one file. Keep all
of it — §4 P3 is about what sits *beside* it, not this.

**3.2 Lease CAS with a monotonic clock and boot id (S7).** `Claim`/`Heartbeat`/
`Steal` as guarded `UPDATE … WHERE generation=?`, liveness on `CLOCK_MONOTONIC`
plus `boot_id`, the holder token hashed in the DB and held in a file outside it.
Wall-clock jumps and PID reuse are exactly the failure modes that make naive
leases wrong; the shape is minimal for the guarantee. Used seven times in four
weeks, but correct, small, and the reaper (D1) depends on it. Keep.

**3.3 Torn-read scanning (S4).** `stableReadFile` — two reads, byte compare,
bounded retry, `O_NOFOLLOW`, "vanished" is inconclusive not an error — with
`U_INDEX_UNESTABLISHED` propagated as a real outcome. #8-part2 recorded three
actual honesty violations (a torn read became a permanent fake fail; readiness
false-passed across independent scans; a query reported a fake zero). The
primitive is ~50 lines and correct. What is not earning its keep is that the
same idea is implemented four times (§4 P3(g)).

**3.4 The gate verdict fold, proof binding and canary inversion (S14 core).**
`FoldVerdictWithCode` (`gate/verdict.go:57-83`) is 27 lines and fail-closed in
the right order: a canary that did not fire is an established *fail*, an
unproven pass is *unevaluated*, and nothing else mints `trusted`. `GateCheck`'s
re-verification of every binding field against the proof record
(`gate_eval.go:665-711`) is what makes a stale lane, a changed definition or a
different subject read `U_GATE_PROOF_STALE` rather than green. The mutation
canary (materialise the tracked tree, inject a failing test, run the identical
lane, invert) is the continuous-canary the design §9 asked for. AIRA-55's
`inject-file` made it language-agnostic. This ~600-line core is the part of the
gate subsystem I would keep verbatim; §4 P2 is about the ~3,400 lines around it.

**3.5 The daemon's outcome classification (S20).** `RequestNotSentError` /
`RequestOutcomeUnknownError` / `StoreOpOutcomeUnknownError`
(`protocol.go:266-306`), honoured all the way to the TUI's three-way
applied/rejected/outcome-unknown report and the relay's "operation is safe to
re-run" hint (`write_relay_store.go:262-268`). This is the difference between a
mutation that might have committed being reported as "rejected" and being
reported honestly. Keep.

**3.6 Protocol versioning with monotonic replacement (S21).** A newer client
replaces an older daemon; an older client is refused loudly. Correct direction.
(Its blast radius on a shared box is a hazard, §5 B10, but the mechanism is
right.)

**3.7 Generated MCP and Skill surfaces from `DispatchDescriptors` (S18).** The
MCP tool schemas and SKILL.md are produced from the table with a validator that
refuses an included action without a summary, safety class or example
(`core/skill.go:112-260`). This is the design's rule §4.3 working. The CLI is the
outlier, not the generator.

**3.8 `gitremote`'s bounded execution (S23).** Process groups, concurrent drain,
TERM→KILL escalation, `GIT_TERMINAL_PROMPT=0`, positively-classified auth
failure. An agent hanging on a credential prompt is a real, dated pain (gitauth
§1). 988 lines is a lot for four verbs, but each part closes a way to hang.

**3.9 aitest's crash-only, never-fold-unevaluated design (S24).** Workers only
ever `os._exit`; a dead worker's staged events are discarded, the test is
requeued once, then reported `unevaluated` and counted as failed for the exit
code. The sub-agent could not construct a path where an unrun test reads passed
or skipped, and neither could I. Keep the semantics; §5 B1 is about activation,
not the pool.

**3.10 Closed enums in `internal/domain`.** Status, kind, severity, relation kind,
verdict, disposition, outcome — all closed, all validated, with the transition
table in one function. The design §6 said "overloaded string fields caused real
bugs"; this is the fix, and it is thin.

---

## 4. Where it isn't — proposals

Each proposal: what to cut or merge; why it is not pulling its weight; the
decision that created it and how what that decision protected stays protected;
what else has to change. Ordered by leverage.

### P1 — Generate the CLI from `ArgSpec`; one declaration per verb

**What:** replace `parseArgs` (`main.go:500-618`), its `allowed` map, the inline
boolean and list-valued flag lists, `buildRequest` (`:1359-2017`), and the
per-verb parsers for `run`/`time`/`git`/`confine*` (`:656-882, 1187-1333`) with
one generic parser driven by the verb's `[]ArgSpec` (positional order =
declaration order; `Kind` decides `--flag` vs `--flag value` vs repeatable;
`Enum` validates; `Required` refuses). Merge `applyDispatchMetadata` into the
table entries (the panic that keeps them in sync is evidence they should be one
struct). Give the eleven out-of-table verbs real entries with a `Face` field
(`cliOnly`, `daemonOnly`, `internal`) instead of `Include`'s hand list
(`core.go:2108`) and the six `E_*_UNAVAILABLE` stubs. Generate `aira help` from
the same descriptors the Skill already uses. Derive the MCP zero-default and
coercion lists (`mcp.go:429-511`) from `Kind` instead of copying `buildRequest`.
Delete `parseInstallDescriptorArgs` (unreachable — `install` is intercepted at
`main.go:58`), the `init` and `confine` cases of `buildRequest` (unreachable —
built by hand at `:192-199` and diverted at `:114-124`), and `argAccessor.reads`.
Keep the handful of genuine irregularities as explicit per-verb hooks: `create`
joining positionals into `title`, `git`'s `--` separator, `rant <text>` vs `rant
ls`, `find add --file path:line`.

**Why it is not pulling its weight:** ~1,500 lines of `main.go` are a hand
transcription of `ArgSpec`, and the transcription has drifted in every direction
a transcription can: flags accepted then ignored (`--gate_id`, `--canary_id`),
verbs the generators cannot see (eight missing from `help`), an `install` entry
whose args lag the live parser (`--watchdog`, `--watchdog-interval`), a stub
`confine` case that omits `owner`/`admit_timeout`/`delegate_ram`, and two
byte-size grammars (`app.parseByteCount` rejects `1.5G` in config while
`runner.ParseMemorySize` accepts it on the flag). AIRA-57 was the visible symptom
of the same fact — the front door was never generated. The MCP face proves the
descriptors are sufficient: it *is* generated, with ~80 lines of special cases
that a `Kind`-driven decoder would absorb.

**Motivating decisions and coverage:** the CLI predates `ArgSpec` (Phase-1 CLI
`2609df1`, 2026-08-08; descriptors arrived with M8b on 08-09). M8b's Sol P0 —
"invokable/installable must be proven by an E2E test, not parity alone" — stays:
the real-entrypoint tests in `cmd/aira/main_test.go` and `skill_test.go` are the
regression suite for this change and should be kept green through it. No spec
argues for hand parsing; the design's rule is the opposite (§4.3).

**What else changes:** `routing.Classify`/`StoreFreeCarved` become descriptor
fields (`Route`, `StoreFree`) so the daemon's server-side refusal and the TUI's
palette filter read the same bit. The `Live` list (`run||git||time`,
`dispatcher.go:381, 653`) likewise. Roughly −1,500 lines in `main.go`, −200 in
`mcp.go`, −150 in `core.go`; the descriptor struct gains three fields.

### P2 — Gates: keep the proven core, delete what has never been exercised

**What, in decreasing certainty:**

(a) **Delete the SQLite projection.** `gate_results`, `gate_proofs`,
`gate_attestations`, `gate_baselines`, `gate_baseline_active` have no reader
outside tests; `gates` has one (`rant.go:510`, an existence check that can read
the file instead). Delete the five tables, `rebuildGateProjection`
(`gate_index.go:183-266`), their six entries in `projectOwnershipTables`, and the
call from `Rebuild` (`store.go:2635`). This also removes hazard B7 (a corrupt
gate ledger currently aborts ticket reconcile).

(b) **Replace the HMAC/nonce/HEAD layers with the same append-only,
hash-chained, frame-checksummed JSONL the project already uses for
`journal.jsonl`.** Keep `seq`, `prev_digest`, the per-frame SHA-256 and the
`fsync` discipline (tamper-*evidence* and torn-write detection); drop the HMAC
tag, the 32-byte key file with its 0600 check, the durable `HEAD` with its own
tag, and the O(n) nonce-uniqueness scan on every append (`gate_audit.go:65-110,
216-220, 329-353, 433-445`). Keep the challenge nonce on `review` records as an
opaque challenge id — that is the only place a nonce carries meaning.

(c) **Delete the nine definition fields nothing consumes:** `AppliesTo.
{LifecycleStep,Ticket,Milestone,Labels,Paths}`, `Enabled`, `Advisory`,
`FailureGuidance`, `Manual.{Role,EvidenceKinds,PromptID}`, plus
`ProofPolicy.RequireCurrentCanary` and `CanaryDeclaration.Cadence`, which are
validated and digested and never read by any evaluator (grep of `internal/store`
and `internal/core` non-test code: zero consumers; `W_GATE_DISABLED` is
catalogued and never emitted). Today every gate applies to every ticket
(`relation_ready.go:582-601`) and `enabled:false` evaluates anyway. Either the
selector is implemented or it is removed; a definition file describing behaviour
that does not exist is the AIRA-53 class.

(d) **Defer the ratchet kind until test reports exist.** `gate_ratchet.go`
(525), `gate baseline-pin|baseline-show`, the two baseline tables (already in
(a)), the `synthetic-ratchet` canary mode, the `ratchet-status` gauge. There
are zero test reports on this machine to ratchet over, and a ratchet's proof
today rests on a canary that "proves the comparator on declared data only"
(`gate_eval.go:477-488`), not the evidence pipeline. Per the owner's no-compat
rule this is a deletion now and a restore from git later, not a flag.

(e) **Collapse the manual-attestation ritual to one record.** `review` mints a
challenge; `attest fail` then `attest pass` by the same actor on the same nonce
yields proof-of-fire and a trusted pass (`gate_eval.go:313-357`). Nothing in
the sequence is evidence the negative case fires — it is the same person typing
twice. One `attest --verdict pass|fail --actor X` record bound to the current
subject digest, decaying by `MaxAgeSecs`, carries the same information in a
quarter of the code. (This also fixes B11: `gate add --checker
manual-attestation` currently produces a gate that cannot run because no
`attestation-challenge` canary can be materialised.)

(f) **Delete the `fixture` canary mode and `copyFixtureSeed`.** AIRA-55
established that the fixture route is "impractical for anything beyond a toy",
rejected relaxing its `.git` ban as unsafe, and showed that `inject-file` over
the tracked snapshot delivers what the fixture seed was wanted for. Keep
`mutation` (the three kinds) and the tracked-snapshot materialiser.

(g) **Fix the subject digest** — see §5 B2; this is a bug, listed here because
(a)–(f) are the moment to change the record schema.

**Why it is not pulling its weight:** 4,069 production lines (plus 2,918 test
lines) for a feature with zero definitions on any project; its creation verb
did not create for three weeks; its first real user (a Rust project, AIRA-55)
abandoned it for a hand-rolled `make canary`. The parts that were exercised
tonight — the fold, the mutation canary, the proof binding — are ~600 lines and
are sound (§3.4). The HMAC design's own spec concedes its adversary: "It does
not claim protection from a privileged user who can replace the local key"
(M10 §4.4). On a single-user machine every caller is that user; the key at
`common/aira/gates/hmac.key` is readable by every process that can write the
ledger, so the tag proves exactly what the hash chain already proves. Meanwhile
every `Append` re-reads and re-verifies the whole ledger (`gate_audit.go:182`),
every `GateCheck` does the same, and `ready` calls `GateCheck` on every
invocation.

**Motivating decisions and coverage:**
- M10 §1.1 "no result is inferred from a missing row" and §9.2 "an imported or
  DB-edited `attested=true` row cannot satisfy `gate check`": still true —
  verdicts never read the DB, and after (a) there is no DB row to edit.
- M10 §4.4 "ordinary callers cannot manufacture a record that AIRA did not
  issue": downgraded to what it actually delivers — a caller who edits the
  ledger by hand breaks the chain unless they recompute every later digest,
  which the plain chain also forces. What is lost is the claim of
  unforgeability against a key-holding user, which was never held.
- M10a decision 21 (honest empty set) and AIRA-54: unchanged (`U_GATE_SET_EMPTY`).
- M10b's admissibility predicate, env digest, output cap, parser-incomplete →
  unevaluated: unchanged (all in `gate_command.go`, kept).
- M13b (ratchet): the spec's R1 "the audit snapshot is the authoritative durable
  baseline" is exactly why (d) is a deferral not a redesign — when test reports
  exist, the baseline record format is the part to bring back.
- AIRA-56 (ready vs empty set) and AIRA-60 (`ValidateCanary` normalising check):
  unaffected; AIRA-60 gets easier with one fewer path predicate (there are five,
  `canary.go:189`, `command.go:101`, `gate.go:252`, `gate_eval.go:555`,
  `gate_command.go:372`).

**What else changes:** `gate` loses `baseline-pin`/`baseline-show`; the
descriptor shrinks by ~10 mutation/ratchet args; `insights` loses one gauge;
`schema_ownership.go`'s list shrinks (or vanishes under P3(c)). Approximately
−2,000 production lines and −1,200 test lines, leaving ~2,000 that are all on a
path someone has walked.

### P3 — Store: one truth per entity, one counter, no migrations, one writer

**(a) Make the projection the read path, or delete it.** `List`, `Get`,
`Relations`, `Ready`, `ListFindings`, `ListRequirements` all rescan the working
tree (`query.go:241`, `relation_ready.go:277, 446`); the `tickets`, `relations`
and `requirements` tables are read only to warn that they are stale. The
#8-part2 decision — "agents' uncommitted entities are live coordination state;
a committed-snapshot fix would drop them" — chose working-tree authority
deliberately, and the scanners are already torn-read-safe. So the consistent
move is to **delete the three projection tables**, the projection half of
`Rebuild` (`store.go:2485-2611`), `checkStaleIndex`, `relationIndexDivergence`,
`findingIndexDivergence` for review findings, the `W_STALE_INDEX` and
`E_*_INDEX_DIVERGENCE` codes, and the four `check` dimensions that exist to
describe a cache nobody reads (`stale-index`, `rebuild-integrity`,
`relation-integrity`'s index half, `finding-integrity`'s index half).
Reconciliation findings stay DB-authoritative. `search_fts` becomes what it
already is in practice — rebuilt per query — but from an in-memory table, with
the machine-wide `search-rebuild.lock` (which serialises every `grep` on the box
across all projects, `search.go:137`) gone. The alternative — serve reads from
the index and fall back to a scan on digest mismatch — is the design's original
intent and is also fine; what is not fine is paying for both and using neither.

**(b) One counter table.** `event_counters`, `id_counters`, and the five
identical `*_counter` tables (`store.go:912-919, 989-991, 1014-1016, 1045-1047`)
with five copies of `next*Numbers` become `counters(project_id, kind, next)` and
one function. The design §6 names one `seq` authority per project; the code has
seven.

**(c) Delete the migrations.** `ensureRantReviewTriggerCurrent`,
`ensureAreaHintsGeneration`, `ensureOutboxKind`, `ensureAllocationKind`,
`ensureFindingsSchema` (with its `findings_m5` copy/drop/rename and orphan
recovery), `ensureSearchFTS`, the `compute_events` ALTER loops, all of
`schema_ownership.go` (a runtime rewriter that string-splices `FOREIGN KEY` into
`sqlite_master` DDL for 25 tables), and the pre-M9/pre-M21 compat branches
(`normaliseKind`'s `""→ticket`, the two-part legacy `allocationEventDigest`,
`Options.GitDir==""` identity bypass) — ~700 lines that run `PRAGMA table_info`
probes on every `Open`. The owner's standing rule (2026-08-20, restated) is
"spend ZERO effort on data-migrations… just define the new shape";
`ensureProjectOwnershipFKs` was written a week after it. Replace with a single
`schema_version` row and a loud `E_SCHEMA_INVALID: delete state.db` — the DB is
rebuildable by design (§3.1), so the upgrade path is `rm` + `reconcile`.

**(d) Keep the project journal pure.** Route watchdog decisions to a daemon-level
log (PR #7 P6 proposed this for a different reason; the data now quantifies it:
52% of this project's `events` rows, all `journaled=0`, all consuming `seq`).
Give rant events an outbox row like leases do (`lease.go:317-321`), or stop
journaling them — either is consistent; the current state (journaled without a
replay path) is neither.

**(e) One writer.** Fold the detached supervisor's `AddTestReport`/
`AddComputeEvent` through the same store-op frames the client relay uses
(`daemon.NewAddTestReportStoreOp`, `NewJSONStoreOp`) — the supervisor already
speaks the daemon protocol for its lease. ~50 lines; closes the D5↔D7b gap
(§5 B5). If PR #7's P8 direction (`run` becomes `confine` + recording) is taken,
this path disappears with it, but that is a multi-milestone refactor and this
is an afternoon.

**(f) Resolve the outbox.** Implement the catalogued-but-unbuilt
`E_PATH_INTENT_UNRESOLVED` path: `reconcile` (or `reconcile --resolve`) marks a
conflicted intent `resolution='conflict'` after recording its finding, so the
partial unique index releases the path. See §5 B3.

**(g) One copy of each primitive.** `stableReadFile` and the three other
double-read snapshotters; four SHA-256 helpers; three git runners (`runGit`,
`gitValue`, bare `exec.Command` in `runCanary`); `syncDir`/`syncGateDir`; three
query mini-grammars (`query.go:93`, `compute.go:270`, `command.go:161`);
`Options` vs `ScopeOptions` (two 17-field structs copied field by field,
`store.go:53-107, 311-321`); `OpenReadOnly` duplicating ~70 lines of
`newScopeContext`. Mechanical.

**Why it is not pulling its weight:** `store.go` is 3,716 lines and 67 commits
because every milestone added a table, a counter, a migration and a projection
to it. The projections cost a transaction per mutation and a scan per `check`
and deliver a warning. The migrations cost seven probes per open for a database
with no users. The truth-splitting produced four real defects (B3, B4, B5, B7)
that no test caught because each half was tested against its own spec.

**Motivating decisions and coverage:** Phase-1 §2 ("SQLite for mutation
sequencing and coordination authority; a rebuildable index") — the sequencing
and coordination authority is untouched; the index is what (a) removes or
promotes. #8-part2 §0.1 (working-tree authority) — honoured, and simplified.
M5 F1 (crash-atomic migration) and M9a (two-table transactional migration) —
the lessons were about *how* to migrate; the owner has since said not to. D3
§2.1 ("any future event writer MUST keep seq-allocation and event-insertion in
one `BEGIN IMMEDIATE`") — (d) keeps that; it changes which log the watchdog
writes to. M21 §2 / D7b §1 (single writer as routing policy) — (e) makes the
policy true for the one remaining production writer.

**What else changes:** `Check` shrinks from 14 dimensions to ~9; `Rebuild` to
the receipt/journal/ref reconciliation that actually recovers a lost DB; the
`ExitCodes` catalog loses the never-produced index codes (§5 B9 lists them).
Net roughly −1,800 production lines in `internal/store`.

### P4 — Faces: one path per verb, and stop validating ourselves

**What:** (i) delete the dead TUI symbols (`runTUIWithScreen`, `executeLaunchOnUI`,
`submitPalette`, write-only `paletteConfirmForm`/`inlineStageConfirm`, the
no-op `viewEvents` fetch), `runMCP`, `NewWithRunnerInput`, `GenerateSkill`,
`ValidSafetyClass`, `minSamples`; (ii) collapse `dispatchClient`'s two
near-identical branches (`dispatcher.go:265-291` vs `:292-342`) and the
double Core construction when `outputCap>0` (`:382-385, 654-657`); (iii) one
git-discovery function (`app.Discover`, `DiscoverBootstrap`, `PrepareInit` are
three copies of the same three `rev-parse` calls, `project.go:139-214,
409-496`), called once per command — `dispatchClient` currently re-runs it and
`validateProjectSnapshot` re-derives the scope to compare, which is a TOCTOU
guard against the caller's own process; (iv) one state-dir resolver
(`app.stateDir` vs `daemon.PathsFromEnvironment`, which disagree when `HOME` is
unset) and one `WorktreeScope` builder (`ScopeFromProject`, `bootstrapScope`,
`runnerDaemonScope` — three); (v) drop the response-consistency validation in
`write_relay_store.go:119-254` (~135 lines checking that the daemon — the same
binary, over a 0600 socket in the user's own runtime dir — returned
self-consistent counts). Keep the size bounds (`boundedCheckReport`,
`maxRelayedTestResults`); those protect memory, not trust.

**Why:** these are the sub-agent's HIGH/MEDIUM oddities in `cmd/aira` and
`internal/app`, each traced; none is load-bearing. (v) is the pattern PR #7
found in the runner: adversarial-review hardening against a peer that is the
same process image.

**Motivating decisions and coverage:** M21 §5.5 (state identity pinned to the
daemon's own `$XDG_STATE_HOME`) — kept, with one resolver. D7b R4 ("a
possibly-lost row beats a silent duplicate") — the `OUTCOME_UNKNOWN` path is
untouched; only the DTO shape checks go. TUI v2's Sol P0 (palette must never
execute a client-routed verb) — kept.

### P5 — Telemetry and insights: dogfood or freeze, per subsystem

The owner turned dogfood mode on (2026-08-28). Four subsystems have had no
producer since. For each, one of two honest options:

| Subsystem | Live rows | Dogfood option (one change that produces data) | Freeze option |
|---|---|---|---|
| Command events (`aira time`, S11) | 0 | Record one `command_event` from `aira confine` — every heavy command already goes through it, and the loop doc (`agentic-development-loop.md:44-50`) still teaches `aira time` while `CLAUDE.md` mandates `aira confine`; the chronology found no document reconciling them | Delete `time`, `commands`, `command_events`, the `command-latency` gauge and the third query grammar (~940 lines) |
| Test reports (S10) | 0 | aitest already emits JUnit; pipe it to `test-report add --format junit` at the end of a `--delegate-ram` run | Keep the table; delete the flaky classifier and `test-report flaky` until reports exist |
| Findings (S8) | 0 | Make `aira find add` the recorded form of a review verdict in the build-review step of the loop; the `review` verb already emits the `report_instruction` | Keep (it is the design's pain #2; small) |
| Compute + quota (S11) | 0 / 0 | Needs a capture hook (M14 D2, never built) | Delete `quota` (493 lines of `compute.go` are shared; quota alone is ~150 + a table + a gauge); leave compute until a hook exists |
| Insights (S12) | 1/10 computable | follows the above | Register only gauges whose source table has a producer |

**Why:** a gauge that can only read `unevaluated` is honest but is also dead
weight that every `insights show` and every TUI insights refresh pays for (the
TUI issues N+1 dispatches for it). More importantly, the design's own rule
(§4.1, "build only what can be checked") has a corollary this project has been
violating for four weeks: a mechanism with no caller in the dogfood loop has
never been checked. The AIRA-53/54 pair is what that looks like when it
surfaces.

**Motivating decisions and coverage:** command-telemetry §1 ("a slowdown
investigation has no evidence today") is satisfied by the `confine` hook more
cheaply than by the `time` verb, since `confine` is what runs. M13's identity
tuple stays. M14's disjoint buckets stay for compute. The `quota` verb had no
motivating incident; design §12 lists it as "opt-in".

### P6 — Rant: one representation of git context

`GitContext` is stored three ways: an EAV table (`rant_git_context`, one row per
field with `value/status/reason`, 102 live rows for 17 rants), six inline columns
on `compute_events`, and six inline columns on `command_events`, each with the
same four-state CHECK constraints (`store.go:938-946, 1001-1011, 1026-1042`).
Pick the inline shape (it is the one two of three already use), and fold
`rant_context_refs` into a JSON column — the refs are validated at write time
and only ever read back whole. Keep redaction (a real secrecy feature, used
correctly with `secure_delete`), keep idempotency keys, keep the append-only
review trigger. Rant is *used*; this is a tidy, not a cut. ~150 lines.

### P7 — pylib: fix the contract, embed what ships, delete the governor

(i) B1 below is a bug fix, not a proposal, but it belongs on this list because
the fix is one line in `confine_linux.go:757` (inject the aitest coordinates
whenever the runtime dir is known, not only under `--delegate-ram`) *or* one
sentence in `skill.go:326`; the owner should choose which contract is wanted.
(ii) `go:embed all:aitest` embeds the 3,600-line Python test suite, `conftest.py`
and a 512 MiB-allocating fixture into the release binary and extracts them into
every user's `~/.local/share/aira/pylib/<hash>/` (AIRA-66 covers `__pycache__`;
the tests are the larger problem). Embed explicit files. (iii) The Go test
`pytest_aitest_supervisor_test.go` shells out to run the entire Python suite a
second time inside `go test`, inheriting the environment; the Python suite should
run once, in CI, as Python. (iv) PR #7 P1 (delete `aira_xdist_governor`,
`governor-slot`, `governor.go`, `confine-reserve`) is reaffirmed with a new
reason: with `--delegate-ram`, `env.go:149-169` injects both the governor's
per-test reservation coordinates and aitest's per-worker ones, so a project
whose `conftest.py` loads both plugins runs two admission systems stacked.

### P8 — Layering: move four things to where the diagram says they live

- `internal/store` imports `runner` in five production files because the
  command-gate `Execution` seam is typed with `runner.Request`/`RunRecord`/
  `OutputRequest`/`OutputChunk` (`store.go:129-132`). Define the seam with
  store-owned types (argv, cwd, env, timeout → exit, streams, admissibility
  facts) and adapt in `app`. Store also shells out to `git` in three places;
  worktree discovery (`discoverWorktrees`, `runGit`, `validGitRoot`) belongs in
  `app` or `gitcontext`.
- `internal/domain` imports `gitcontext`, which imports `gitremote` (a 1,166-line
  network client) for one function, `RedactURL` (`resolver.go:327`). Move
  `RedactURL` down.
- `internal/core/command.go` imports `pylib` to inject the governor environment
  into `aira time`'s child; environment shaping belongs in the runner.
- The 176-entry `ExitCodes` catalog, `ExitForCode` and `ErrorCode` live in
  `store/check.go` and are edited by every milestone (51 commits). A leaf
  `internal/codes` package, with a test that every produced code is catalogued
  and every catalogued code is produced, closes §5 B9.

### P9 — Seed `check` dimensions as `unevaluated`

`Check` pre-seeds all fourteen dimensions as `"pass"` (`check.go:130-135`) and
relies on every evaluator to overwrite. AIRA-54 fixed the one dimension where an
early return left the seed standing. Seed `unevaluated` and set `pass` only when
an evaluator returns cleanly; the shape of the bug disappears for all fourteen.
Five lines.

---

## 5. Further bugs and hazards

### B1 — The installed skill's aitest activation form never wires aitest to the daemon (HIGH)

`internal/core/skill.go:326` (rendered into SKILL.md and the agent guide):
*"Launch the whole invocation under a plain `aira confine -- pytest
--aitest-workers=auto ...` — no `--delegate-ram`, no `--memory-reserve`… aitest's
own supervisor forks each worker and admits it individually through the daemon
(`worker-admit`)."* But `internal/runner/confine_linux.go:757-778` injects
`AIRA_AITEST_BOOTSTRAP_CMD`/`WORKER_ADMIT_CMD`/`MAX_WORKERS_FALLBACK` only
`if request.DelegateRAM`, and unconditionally strips them otherwise (the comment
at `:769-777` describes this as a deliberate Fable build-review fix). `DelegateRAM`
is set solely from the `--delegate-ram` flag (`main.go:934`). So a launch that
follows the skill text reaches `supervisor.py:186-189` with the bootstrap command
unset → `_disable_daemon` → unconfined fallback, and with the fallback ceiling
env absent the pool is `min(N, 1)` = **one** unconfined fork worker
(`supervisor.py:172, 1197`) — slower than serial pytest, with a warning the agent
has been told to ignore. AIRA-44's live scenario shows real use was `aira confine
--delegate-ram -- make test`, i.e. the opposite of the skill. Traced both sides.
This is the AIRA-53 class in the one document agents are told to trust.

### B2 — A manual, ratchet or dimension gate's stored `pass` survives every non-Go change (HIGH)

`digestEvaluationRoot` (`gate_eval.go:63-79`) hashes `trackedTracePaths`
(`traceability.go:88-112`): tracked `*.go` outside `vendor/` plus
`.aira/requirements/*.md`. That digest is the `subject` for manual gates
(`gate_eval.go:155, 291`), for ratchet gates (`gate_ratchet.go:114-119`), and for
the `GateCheck` lookup of every non-command gate (`gate_eval.go:647-658`).
`GateCheck` serves `latest[gate_id + subject]` and never re-evaluates. Therefore:
on a non-Go repository the subject is a constant, so one `attest pass` reads
`pass` for every future commit until `MaxAgeSecs` (which is 0 = never for a
hand-authored gate, `gate_eval.go:706`); on a mixed repository a ratchet or
review gate passed at commit A keeps reading `pass` at commit B if no `.go` file
changed, even though B's test reports (which `evaluateRatchet` keys on `HEAD`)
may show a new failure. Command gates use `digestTrackedRoot` (all tracked files)
and are unaffected. AIRA-55's Rust project is precisely the population this hits.
Fix: one subject digest, over all tracked files, for every checker. Traced.

### B3 — A write conflict permanently bricks a ticket path (HIGH)

`outbox.resolution` is compared `IS NULL` at twelve sites and written nowhere
(grep of `internal/store` non-test code). When `reconcile` finds an intent whose
file no longer matches its precondition it records an `E_WRITE_CONFLICT`
finding and continues (`store.go:2120-2126`), leaving the row `materialised=0,
resolution=NULL`. The partial unique index `unresolved_path_intent`
(`store.go:793-795`) then refuses every later intent on that path with
`E_PATH_INTENT_BUSY` (`:1747-1750`), and `Eject` refuses with `E_EJECT_UNVERIFIED:
N unresolved materialisations remain` (`lifecycle.go:290-295`). The catalogued
`E_PATH_INTENT_UNRESOLVED` (`check.go:82`) is never produced — it is the
designed repair that was not built. Recovery today is restoring the file to the
exact precondition or intended bytes by hand. Not observed live (247/247 outbox
rows materialised), but the path is one concurrent edit away. P3(f).

### B4 — Watchdog and rant events break the journal's replay and gap-detection contracts (HIGH)

`AppendWatchdogEvent` (`store/watch.go:11-19`) allocates the project's next
`seq` and inserts an `events` row with no outbox row; the journal flusher walks
`outbox` (`store.go:2171`), so those events are never journaled. Live: 245 such
rows in the `aira` project, all `journaled=0`, against 244 journaled mutation
events — the common-dir journal has 245 permanent gaps, so the design §6 property
"`seq` … append-only and gap-detectable" is false and the design §11 rule ("the
journal records significant mutations, not high-frequency telemetry") is
violated by the majority of the rows. Rant events (`rant.go:106-113, 219-226,
270-277`) insert into `events` and call `journalEvent` inline but create no
outbox row, so a crash between COMMIT and the journal append is unrepairable by
either replayer. Leases show the correct pattern (`lease.go:317-321`: outbox
row with `path=''`, `materialised=1`). P3(d).

### B5 — The detached supervisor is a third production writer to `state.db` (HIGH)

`runSupervisor` (`main.go:346-418`) → `app.OpenWithDiagnostics` (`:387`) →
`store.Open` (read-write, registers the worktree, appends a registry
breadcrumb) → `core.NewWithRunner(s, …).WireAndSettleDetached` → `c.store.
AddTestReport` / `AddComputeEvent` (`run_wiring.go:261, 272, 391`) on every
`aira run --detach` with telemetry. The record knows: D5 §1.3 defers the fold to
"D7b (task #36)"; D7b §1 says "Folding it is D5's remaining work, not D7b";
neither happened. M21 §2's "no production escape hatch" holds for the client
and not for the supervisor. Zero `supervisor_leases` rows suggest detach is
unused today, which is the moment to fix it. P3(e).

### B6 — `aira grep` rebuilds the whole FTS index on every query under a machine-wide lock (HIGH)

`Search` (`search.go:35-60`) takes `search-rebuild.lock` in `Dir(dbPath)` — the
state dir shared by every project on the machine — then `reconcileSearchIndex`
(`:101-128`) rescans every ticket and finding file, `DELETE`s the slice and
re-`INSERT`s all rows plus every rant, and only then runs the `MATCH`. Every
`grep` on every project serialises behind every other and pays a full rebuild.
`rebuild.lock` (`store.go:2224`) has the same machine-wide scope. Traced. P3(a).

### B7 — Five gate tables are write-only, and their rebuild can block ticket reconcile (HIGH)

`gate_results`, `gate_proofs`, `gate_attestations`, `gate_baselines`,
`gate_baseline_active` have no non-test reader (grep repo-wide);
`rebuildGateProjection` (`gate_index.go:183-266`) is the only writer and is
called from `Rebuild` (`store.go:2635`). It tolerates only `errGateAuditEmpty`
(`gate_index.go:192-198`); a missing `hmac.key` beside an existing ledger, or
any chain/HMAC failure, returns `E_JOURNAL_CORRUPT` and the project's `reconcile`
fails until the gate ledger is repaired — a cost paid to populate tables nothing
reads. P2(a).

### B8 — `check` pre-seeds fourteen dimensions as `pass` (MEDIUM)

`check.go:130-135`. AIRA-54 fixed `gates`; the other thirteen depend on every
early-return path overwriting the seed. I found no second dimension that
currently reads a fake pass (each evaluator either sets its dimension or returns
an error), so this is a hazard shape, not a live defect. P9.

### B9 — The stable-code catalog and the produced codes disagree (MEDIUM)

Catalogued and never produced: `W_GATE_DISABLED`, `W_GATE_PROOF_EXPIRING`,
`E_TOKEN_WORKTREE` (Touch returns `ErrLeaseToken` for a worktree mismatch,
`area.go:414-416`), `E_DB_CORRUPT`, `E_RECONCILE_REQUIRED`,
`U_INSIGHT_UNEVALUATED`, `E_PATH_INTENT_UNRESOLVED`. Produced and not
catalogued (default exit 1): `U_RELATION_GRAPH_UNESTABLISHED` — returned as a
top-level error from `Relations`/`Get` (`relation_ready.go:311`) and exiting 1
while its sibling `U_INDEX_UNESTABLISHED` exits 3, so the same "could not
establish" outcome has two exit classes; also `E_GATE_EXISTS`, `E_RANT_REDACTED`,
`E_RANT_REDACTION_INCOMPLETE`. `ErrorCode` classifies by string prefix up to the
first `:` (`check.go:648-660`), so `errGateAuditEmpty` ("empty gate audit")
reads as `E_INTERNAL`. Sub-agent traced; the `U_RELATION_GRAPH_UNESTABLISHED`
exit I re-checked. P8.

### B10 — A newer client restarts the shared daemon; the runner's copied protocol constant is unpinned (MEDIUM)

`exchangeWithReplacement` → `replaceOlderDaemon` (`dispatcher.go:389-415,
594-617`) runs `systemctl --user restart aira-daemon.service` from any client
whose `ProtocolVersion` exceeds the daemon's. On this box that is a shared
service serving every session, and the project's hard rule is never to restart
it casually; a worktree-built binary with a bumped version running `aira list`
will do so. Separately, `internal/runner` cannot import `daemon`, so it carries
its own `runnerDaemonProtocolVersion = 5` and a hand-copied frame codec
(`admission_linux.go:79-99`, `governor_slot.go:76-120`); no test pins it to
`daemon.ProtocolVersion`, so the next bump strands every runner verb in
`E_DAEMON_PROTOCOL`, which the admission path routes into the flock fallback PR
#7 flagged as B2. The dispatcher side I traced; the runner copy is the
sub-agent's, spot-checked at `admission_linux.go:79-84`.

### B11 — `gate add --checker manual-attestation` produces a gate that cannot run (MEDIUM)

`buildCanaryDeclaration` (`gate_write.go:377-427`) emits only `Mode=mutation`,
and only when `mutation_kind` is given; a manual gate needs an
`attestation-challenge` canary, which `gate add` cannot write. `RunGate`,
`AttestGate` and `GateAction("review")` all call `canaryFor` first
(`gate_eval.go:163, 295, 405`) and fail `E_GATE_CANARY_INVALID: referenced canary
declaration is missing`. The write warns "no resolvable canary", so it is not
silent — but the documented verb cannot produce a working manual gate. Fixed by
P2(e) or by writing the challenge canary. Sub-agent traced; I re-read
`gate_write.go:416`.

### B12 — Routed verbs have one 30-second connection deadline (MEDIUM, pattern)

`serveConnection` sets a single `SetDeadline` (`server.go:527`); the generic
`core.Do` path neither clears nor refreshes it, so a routed verb slower than
~30 s (a large `import`, a `gate attest` that re-reads the whole ledger) commits
and then fails the response write → the client sees `RequestOutcomeUnknown`.
Store-ops got a daemon-owned deadline (`storeops.go:186-206`); routed verbs did
not. AIRA-18 was the same class on the governor connection. Not traced to a
repro.

### B13 — Unbounded growth: registry breadcrumbs, path locks, extraction dirs (LOW)

`registry.jsonl` gains a line on every uncached scope build and every detached
run (96 lines live, 40 for this repo); `common/aira/locks/` gains a
`path-<sha>.lock` per ticket path ever written (88 live) and never removes one;
`~/.local/share/aira/pylib/<hash>/` gains a full extraction per Python-tree
change and the spec says old ones are "never auto-deleted". None is dangerous;
all are the kind of thing that is noticed in a year.

### B14 — Dead and unreachable code (LOW, all traced by the sub-agents, spot-checked)

`parseInstallDescriptorArgs` (`main.go:620-654`); `buildRequest` cases `init`
and `confine`; `Store.pathLock`, `sortReports`, `recordScanFinding`,
`hasGateContent`, `FlakyCellStateSummary`, `ListComputeSpendByPhase`,
`GateAudit.Verify`, `GateAuditRecords`, `copyAsOf`, `PathTier`, the
`Store.gitDir` field (assigned, never read); `cappedWhaleOnDisk`,
`parseInstalledMemoryHigh`, `inspectExistingPath` in `install`; the whale
coexistence checks that remain live after whale was retired; the `runMCP`,
`runTUIWithScreen`, `executeLaunchOnUI`, `submitPalette` faces; `scrubEnv(_,
rewrite=true)` never called with `true`; three no-op branches
(`check.go:175-178`, `store.go:1503-1507`, `gate_index.go:231-239`).

### B15 — Two byte-size grammars (LOW)

`app.parseByteCount` (`project.go:749-775`, integer + one letter) governs
`run.memory_headroom` in `.aira/config`; `runner.ParseMemorySize` (decimal,
`i`/`B` suffixes) governs every CLI flag. `memory_headroom: "1.5G"` is refused
while `--memory-max 1.5G` is accepted. P1 makes it one parser.

### B16 — The git hooks run an unconfined `go vet`/`go build` against the shared root checkout on every commit and push from any worktree (MEDIUM, observed)

`core.hooksPath` is `/home/mark/claude/aira/.githooks`; `pre-commit` is
`exec make -C "$ROOT_DIR" fmt-check vet build` and `pre-push` runs the full
suite, both with `ROOT_DIR` resolved from the hook file's own location — the
root checkout — regardless of which worktree is committing. Committing this
Markdown file failed `fmt-check` on another session's unformatted
`internal/daemon/server.go` in the root tree, and the build it would have run
is exactly the heavy, unconfined Go work `CLAUDE.md` forbids outside
`aira confine`. The pre-push half was already known tonight; the pre-commit
half is the same flaw one step earlier. Fix: resolve `ROOT_DIR` from
`git rev-parse --show-toplevel` of the committing worktree and wrap the heavy
targets in `aira confine --`, or drop the hooks in favour of CI.

### Adjacent to PR #7 (noted, not re-derived)

- **Peak-RSS history is mostly singletons.** 5,103 of 10,253
  `confine_peak_history` rows belong to signatures with exactly one sample;
  only 1,302 of 6,405 signatures have two or more. The signature is the joined
  argv, so nearly every job is unique and the per-signature estimator (PR #7
  M12) falls through to the machine p90 prior almost always. Worth knowing
  before tuning anything that depends on per-signature history.
- **Stacked admission under `--delegate-ram`** (P7(iv)) strengthens PR #7 P1.
- **`internal/store/cgrouptest_linux.go`** (AIRA-69) is a runner-side test
  helper living in the store package; it is the one place the store's *tests*
  need a cgroup.

---

## 6. If the owner can only act on three things

1. **P1 — generate the CLI from `ArgSpec` and delete the hand mirrors.** It is
   mechanical, it is the largest single deletion available (~1,800 lines across
   `main.go`, `mcp.go`, `core.go`), and it removes the *class* behind AIRA-53,
   AIRA-57, B1's cousin in `install`, the ignored `--gate_id` flags and the eight
   verbs missing from `help`: a documented surface that is a different artifact
   from the executed one. After it, a verb is declared once and every face —
   help, CLI, MCP, Skill, TUI palette, daemon routing — reads the same struct.

2. **P3 — one truth per entity in the store.** Delete the projections nobody
   reads (or make them the read path — but pick one), collapse seven counters
   to one, remove the migrations the owner already said not to write, keep the
   project journal for mutations only, and give the supervisor the relay. This
   closes B3, B4, B5, B6 and B7 — every store-side defect in §5 is a place where
   a second copy of the truth drifted from the first — and it halves `store.go`.
   It is the structural fix for the same class PR #7 found in the ledger.

3. **P2 — shrink gates to the proven core, and fix B2 while the record schema
   is open.** The fold, the mutation canary, the subject digest and the proof
   binding are sound and small. The projection tables, the HMAC theatre, the
   nine dead definition fields, the self-attested manual ritual, the fixture
   mode and the ratchet-without-reports are ~2,000 lines that no project has
   exercised and that hid a creation verb which did not create for three weeks.
   B1 and B2 are the two findings I would fix before any of the above; each is
   a few lines.

P4–P9 are each under a day. P5 is the one that needs an owner decision rather
than code: for each telemetry subsystem, either wire a producer into the loop
that actually runs (`aira confine` recording a command event; aitest's JUnit
into `test-report add`; `find add` in the build-review step) or delete it until
one exists. The process corollary is worth writing into `CLAUDE.md`: **a
mechanism with no caller in the dogfood loop is `unevaluated`, not done**, and
the two-loop should ask "who runs this?" before "is this correct?".

---

## 7. Appendix

### 7.1 What I did and did not do

- Read in full, myself: the files listed at the top (~9,500 lines), the design
  spec, the M10 gate spec's threat-model and audit sections, the M21/D5/D7/D7b
  single-writer passages, the rant and ratchet spec rationale lines, the
  agentic-development-loop doc, tickets AIRA-52..70, the live `aira` read
  verbs, and the live `state.db` (read-only SQLite over `file:…?mode=ro`).
- Six sub-agents produced package maps and a spec/ticket chronology. Every
  claim from them that I graded HIGH I re-traced at the cited lines: the
  write-only gate tables (grep), `outbox.resolution` never written (grep),
  rant events without outbox rows (grep), `List`/`Get` scanning files
  (`query.go:196-241`), FTS rebuild per query (`search.go:35-128`), the
  supervisor's `store.Open` (`main.go:387`, `run_wiring.go:261/391`), the
  D5↔D7b mutual deferral (both specs), the skill/confine aitest mismatch
  (`skill.go:326`, `confine_linux.go:757-778`), the Go-only subject digest
  (`traceability.go:88-112`, `gate_eval.go:155/291/647`), the HMAC key
  placement (`gate_audit.go:48-62`), the nine unconsumed definition fields
  (grep), `replaceOlderDaemon` (`dispatcher.go:594-617`), and the
  `applyDispatchMetadata`/`parseArgs`/`buildRequest` triplication. MEDIUM
  items that rest on a sub-agent trace I did not fully repeat are marked as
  such in §5.
- Ran no tests, restarted nothing, wrote nothing to any store.

### 7.2 Size of the proposed deletions (approximate, production / test lines)

| Proposal | Production | Test |
|---|---|---|
| P1 CLI generation | −1,800 (net of a ~300-line generic parser) | −600 (hand parser tests; keep the real-entrypoint tests) |
| P2 gates | −2,000 | −1,200 |
| P3 store truth | −1,800 | −1,000 |
| P4 faces | −400 | −150 |
| P5 telemetry freeze (if chosen for command+quota) | −1,100 | −1,200 |
| P6 rant tidy | −150 | −50 |
| P7 pylib (governor deletion counted in PR #7) | −50 + embed change | −930 (`pytest_integration_test.go`) |
| P8 layering | ~0 (moves) | 0 |

### 7.3 Tickets this review would close, re-scope, or open

- **Closes on fix:** B1 (new — file it), B2 (new — file it, P1 severity), B3
  (new), B4 (new), B5 (D5/D7b residual — file it), B6 (new), B7 (with P2(a)).
- **Re-scopes:** AIRA-56 (ready vs empty gate set) — unchanged by P2 but easier
  after it; AIRA-60 (`ValidateCanary` normalising check) — five path predicates
  become two under P2(f); AIRA-66 (`go:embed all:`) — broaden to "embed only what
  ships" (P7(ii)); AIRA-33 (retire the xdist governor) — reaffirmed by P7(iv);
  AIRA-69 (cgrouptest placement) — the helper's home is the runner, not the store.
- **Superseded if P5 freezes command telemetry:** the command-telemetry spec's
  ten deferrals (§10) and `docs/dev/agentic-development-loop.md:44-50`, which
  should say `aira confine` either way.
- **Unaffected:** every runner/admission ticket (PR #7's list), AIRA-58/59
  (whose frontmatter, incidentally, still says `planned` after their bodies
  record deployment — the ticket lifecycle's own dogfood evidence).

### 7.4 Cross-references to PR #7

PR #7's verdict was that the runner side had "four admission mechanisms, two
launch paths, and three memory-pressure responders that were each reasonable
when added and were never reconciled". This side's equivalent sentence: **seven
places that declare a verb, three copies of the truth for most entities, and a
Phase 1–4 feature set that was reconciled with its specs but never with a
user.** The two reviews agree on the mechanism of the bugs — state kept in more
than one place, reviewed in isolation — and on the remedy — pick one place. PR
#7's P8 ("one launch path") and this review's P3(e) ("one writer") meet at the
detached supervisor; PR #7's P1 (delete the xdist stack) and this review's
P7(iv) meet at `env.go`; PR #7's P6 (daemon-level watchdog log) and this
review's B4/P3(d) meet at the project journal.
