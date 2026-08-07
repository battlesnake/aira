# AIRA — design spec (top-level scope + architecture)

- **Status:** designed (this document); not yet built.
- **Date:** 2026-08-07 (v2 — folds a four-lens adversarial review of v1).
- **Scope:** the whole-product scope, architecture, data model, and phasing. Each subsystem gets its own spec → plan → build cycle after this. This document is the map, not the construction drawing — where it says "resolved in the Phase-N spec," that is deliberate, not a gap.
- **Companion:** [Phase 0 process-docs plan](#phase-0--bootstrap-process-docs-the-non-tool-deliverable).

---

## 1. What AIRA is, in one paragraph

**AIRA is "JIRA for AI agents": a machine-local (later optionally remote) work-item tracker, coordination layer, findings store, traceability assistant, and metrics surface for AI coding agents working across one or more repos.** It is a single static Go binary, `aira`, exposed identically as a CLI, an MCP server, a Skill, an optional daemon, and a human TUI — all thin adapters over one core. It exists to kill three concrete, *measured* pains: colliding ID allocation, review findings that evaporate at session end, and "too many cooks in the kitchen" churn between agents in different worktrees.

## 2. The problem (evidence, not assertion)

Observed in `~/claude/an earlier project` — a 189-live-worktree, multi-agent monorepo — not hypothesised:

1. **ID allocation is out-of-repo, bespoke machinery.** `make id PREFIX=BL` drives an `flock`'d counter in the git *common-dir* (`next = max(counter, file_max) + 1`, a self-healing *cache* over the authoritative markdown files), backed by a fail-closed duplicate-ID gate and a "receipt" log that catches IDs *burned in a rebase* (an earlier ticket/384 were committed then dropped in the rebase meant to land them). It works, but it's Python/bash bolted to one repo's markdown tables.
2. **Findings evaporate.** Per-session Codex/Fable review findings die in chat unless someone deliberately turns them into a committed `docs/reviews/` doc, a backlog row, or a regression test. The two-loop adversarial review — which found *seven real bugs in code that passed its own seventeen tests* — leaves no durable, queryable trace. We lose the ability to learn from mistakes.
3. **Coordination is manual prose.** No "who owns what" database. Worktree isolation plus owner sentences ("a peer session owns `runner.py`") and contended files hand-queued "for a non-parallel session." Two agents editing the same file under different tickets is discovered *after* the churn, not prevented.

AIRA absorbs (1), fixes (2), mechanises (3).

## 3. Goals and non-goals

**AIRA is:**
- The authority for work-item IDs, status, relations, leases, findings, requirements, gates, compute, and timing — across worktrees and (later) machines.
- A tool for **agents first**, humans occasionally: CLI + socket + MCP + Skill are primary; a TUI serves the rare human.
- A **recorder and orchestrator**, not a judge. It tracks state, allocates IDs, holds leases, stores typed findings, owns the requirement↔code↔test graph, *emits* review/verification prompts, and *records* verdicts. It never calls a model.

**AIRA is not:**
- An agent/session runner or process supervisor — that is `agentmux`'s domain, and AIRA depends on it for **nothing** (§16).
- A model gateway. It emits ready-to-run prompts; the driving agent executes them.
- A CI system. It records verdicts and runs its *own* cheap deterministic checks (reconciliation, duplicate-ID, orphaned-link); anything heavy or judged is emitted as a command/prompt and its result recorded.
- A replacement for git or PRs. The durable content record lives in git; AIRA complements it.

**Two senses of "block," used precisely throughout:** *integrity* checks (ID uniqueness, graph resolvability) are **fail-closed and refuse an invalid operation** (principle §4.5). *Lifecycle gates* are **advisory and never block a valid-but-out-of-policy transition** in Phases 1–3 (§9). These are not in tension: one refuses the impossible, the other declines to police the merely-unwise.

## 4. Design principles

Imported from `kichad` (a Go CLI+MCP+Skill tool that has been a large productivity multiplier for agents) and adapted:

1. **Primitives, not judgement.** Build a feature only if its result can be *checked* (a status, an ID, an overlap, a coverage %), not merely *judged*. Where only a judgement is possible, supply the primitive and *emit a prompt*; record the returned verdict. **Composition is a feature; output size is a cost.**
2. **One core, thin faces.** All behaviour lives behind a single command interpreter, `core.Do(ctx, req) → resp`. Every surface is a thin adapter over it; the three that expose the command language — **CLI, MCP, Skill** — compile to the *same verbs* and so cannot diverge. (TUI and daemon are adapters too; they add no domain logic — §4.8.)
3. **Generate every doc surface from the dispatch tables.** MCP schemas, `--help`, the agent guide, and the transition/gate *guidance text* (§9) are rendered from the command/field/status/relation/gate tables the parser reads — never transcribed. (kichad adopted this after two real doc-drift bugs.)
4. **Verdict-first, three verdicts never two.** Results lead with the verdict. Every gate/check is `pass | fail | unevaluated`; a thing AIRA cannot stand behind reads **unevaluated**, counted apart from failures, never silently passed. Every metric that can't be computed reads unevaluated, never a fake zero.
5. **Refuse rather than guess.** An ambiguous reference (a selector naming >1 ticket) is rejected, not resolved to a plausible wrong answer. This is the fail-closed *integrity* sense of §3.
6. **Silence is signal; errors are stable codes.** A clean check prints nothing. Every rejection carries a stable machine code an agent can branch on (`match on the code, not the wording`).
7. **Small tool surface + a full escape hatch.** ~10 MCP tools plus one `aira_exec` that accepts the entire command language.
8. **Layer strictly downward; the in-binary adapter layer holds no domain logic.** `store ← domain ← query ← interp ← adapters(cli/mcp/tui/daemon)`. (Out-of-core *ingesters* — the compute/quota capture shims of §12 — are separate small programs beside AIRA, not this adapter layer; they may hold provider-normalisation logic.)
9. **Self-verifying, recoverable edits.** Transitions are structurally validated before write; the reconciler is the safety net; nothing is written to git that would leave an unresolvable graph.

## 5. Architecture

### 5.1 One core, thin faces; strict downward layering

```
internal/store/    git files (content truth) + SQLite (coordination + index) + the reconciler
internal/domain/   tickets, relations, leases+area-hints, findings, requirements,
                   gates, compute-events, quota-snapshots, projects, milestones, the event log
                   — typed, with closed enums
internal/query/    selectors, cursors, distributions, FTS search
internal/interp/   the command language: parse req → dispatch to domain → shape output
cmd/aira/          thin adapters:  cli · mcp (server) · tui · daemon · install
```

`core.Do` is the single dispatch. An MCP tool returns `[]string` of interpreter commands (kichad's pattern) — the MCP layer owns zero domain logic. The Skill is a `SKILL.md` with `allowed-tools: Bash(aira *)`.

**Embedded store:** leaning SQLite (WAL, FTS5) via a pure-Go driver (`modernc.org/sqlite`) to preserve the single-static-binary/no-cgo property; the driver choice gates ID-allocation atomicity and lease concurrency, so it is decided *first* in the Phase-1 spec, not treated as packaging.

### 5.2 Daemon-optional runtime

**Correctness never requires a running service** — critical for fresh worktrees and short-lived sub-agents.

- **Daemonless (default):** each `aira` invocation opens the SQLite DB (WAL for multi-process concurrency), reconciles the touched ticket(s) on demand, and evaluates lease liveness *lazily* from `last_heartbeat`. A short-lived process; no service to keep alive.
- **Optional `aira daemon`:** adds a load-once stateful session (parse a project once, keep a warm cursor/result-set across calls), a live `aira watch`, a continuous reconciler, and a **heartbeat reaper**. If the daemon is down, every command still works daemonlessly. No hard dependency, degrades cleanly.

### 5.3 Storage: git is the content truth; SQLite is coordination + a rebuildable index

Not "two authoritative copies" — a clean split of authority that still delivers the consistency-checking the two-copy idea was for:

- **Git files** (`<repo>/.aira/`) are the **sole authority for ticket content**: one file per ticket (`.aira/tickets/AIRA-42.md`, YAML frontmatter + body), so two agents on *different* tickets never touch the same file. Findings and requirements live in `.aira/findings/` and `.aira/requirements/`. Content survives machine loss and is PR-reviewable. Human indices (`BACKLOG.md`, `ROADMAP.md`) are **rendered on demand** (`aira backlog`, `aira roadmap`) or git-ignored — never a committed generated file, because committed generated files merge-conflict across worktrees by construction.
- **SQLite** (machine-wide, `~/.local/state/aira/`) holds three things, none content-authoritative:
  1. **Ephemeral coordination state** — leases, heartbeats, worktree bindings, the **ID-counter cache** (§7). Losing it *self-heals*: dead leases lapse, worktrees re-register, the counter re-derives from a git scan (§7). Safe to be DB-only.
  2. **Durable history** — the **mutation event log** (§11), compute-events, quota-snapshots. This is *not* rebuildable from ticket content, so it must **not** be DB-only (or a DB loss would destroy all process history). Each is append-only and **also journaled to git**: `.aira/journal/<worktree-id>/<date>.ndjson`, append-only + partitioned by worktree and date so it merges cleanly and stays bounded. The SQLite copy is then a **rebuildable index** over those committed journals — a lost or corrupt DB loses nothing durable, only the un-journaled tail of the current process.
  3. A **rebuildable query/FTS index** over git content, so `aira list`/`grep` are fast. Staleness is a *rebuild*, not a finding.
- **The reconciler** is therefore small and earned — it is an earlier project's receipt/orphan check, generalised, and it is exactly the "catch git mistakes / lost worktrees" the two-copy idea promised:
  - every **allocated ID resolves to a git ticket file**, and every git ticket file has a **DB receipt** (both directions) — a violation (an ID burned in a rebase; a hand-added file the DB never issued) is a **reconciliation record** with a stable code;
  - a **lease on a worktree that no longer exists** is a reconciliation record;
  - a **duplicate ID definition** is a reconciliation record;
  - a stale index → rebuild (silent).

There is no git-wins/DB-wins *content* matrix (git is the only content authority), and DB-only state has no git counterpart to conflict with. **Reconciliation records are DB-resident coordination artifacts** (stable-coded, ephemeral), distinct from **review findings** (git-durable content); both share the Finding schema and the stable-code discipline (§6).

### 5.4 Remote-ready seam (design now, build local)

Two cheap, defensible seams — nothing more committed to v1:

- **`core.Do(req) → resp` speaks serializable JSON**, so the *same* protocol runs in-process (daemonless), over a Unix socket (local daemon), or over TCP/HTTP+auth (remote), with a byte-identical agent/human interface. **No command semantics may assume the store is local.**
- **`Store` is an interface.** Local = embedded SQLite; remote = an RPC client — a drop-in.

Reconciliation is a plain in-process function in Phase 1; the seam *permits* a later client/server split but does not bake one in. How reconciliation works when git (client-side) and the DB (server-side) are on different machines is an **open question (§19)**, not a v1 commitment. Auth/multi-tenant/network transport are a later phase behind the same seam.

## 6. Data model

Closed, small enums everywhere (an earlier project's audit found real bugs from overloaded string fields — "blocked_on means three things"). Enums are types, not strings.

- **Project** — slug, repo path(s), owned ID prefixes, gate policy, milestones, config (`.aira/config`).
- **Ticket** — `PREFIX-N`; title; body (git file); **status** (`draft → planned → in-progress → in-review → done`, plus `blocked`, `retired`, `superseded`); kind (`feature | bug | chore | spike | requirement-work`); severity (`P0 | P1 | P2`); assignee / agent-id / session-id / worktree-id; milestone; labels; timestamps (created/started/ended — DB/event-log-authoritative, §11); estimated compute (set at plan) with actuals derived from linked compute-events.
- **Relation** — bidirectional, typed, stored once and presented both ways: `blocks`/`blocked-by`, `parent`/`child`, `relates`, `duplicates`/`duplicated-by`, `supersedes`/`superseded-by`. The **ready-queue (§9) keys on `blocked-by`** — there is no separate "depends-on" edge. (Which endpoint file physically stores an edge, and inverse-derivation, is a Phase-1-spec decision — §19.)
- **Lease** — ticket-id, holder (agent/session/worktree-id), acquired-at, **last-heartbeat** (expiry is *computed* `last_heartbeat + TTL`, not stored). Hard on the ticket. Plus **area hints**: declared paths/globs; overlap with another live hint → warning, never a block (§8).
- **Finding** — id; **subtype** (`review` = git-durable content | `reconciliation` = DB-resident coordination artifact, §5.3); ticket-id (+ optional requirement-id, `file:line`); category (kebab slug); severity; **verdict** (`confirmed | refuted | plausible`); **source** (`codex | fable | gemini | deepseek | semgrep | human | aira`); **disposition** (`open | fixed | accepted | waived`) + rationale; created-at.
- **Requirement** — own prefix/registry; text; **status** (`built | partial | designed | planned | boundary | retired | superseded`); typed **`covers`** (→ code location) and **`verifies`** (→ test location) edges — the two arms of the V.
- **Gate** — name; kind (`checkable` with a cheap evaluator | `manual` attested); the lifecycle step it guards. Structurally *either* checkable *or* manual (asserted by a test). **An un-attested manual gate reads `unevaluated`** (counted apart from failures, never silently passed); once attested it reads `pass`.
- **ComputeEvent** — `{ticket, phase, provider, model, fresh_input, cache_read, cache_write?, output, reasoning?, cost?, at, agent/session, caller_reported_at?}` (§12).
- **QuotaSnapshot** — optional, provider-supplied account-level usage/limit state (`{provider, account, used, limit, window, resets_at, at}`), for providers that expose it (§12). Distinct from ComputeEvent; opt-in; best-effort.
- **Event** — the append-only *mutation* log: every state-changing call is one event `{seq, at, actor, verb, target, payload-digest, caller_reported_at?}`. `seq` (monotonic) is the **ordering authority**; `at` (AIRA-stamped wall clock) is advisory for display/metrics (§11).

## 7. ID allocation

Report-time allocation (an ID exists the instant an item is drafted, so it can be referenced in commits and cross-links while the branch is alive). Three independent layers:

1. **Rare — the atomic allocator.** The **machine-wide DB counter is the cross-worktree allocation authority**: `new = counter + 1` inside one exclusive write transaction (BEGIN IMMEDIATE / busy-timeout mechanics decided in the Phase-1 spec). It *must* be the DB and not a git scan, because a sibling worktree's just-committed `AIRA-50` is not visible in this worktree's checked-out `.aira/`. The counter is a **cache**, not the content record: on a lost/rebuilt DB it self-heals from a one-shot git scan (`counter = max(counter, git_max)`) — a *repair* pass, never a per-allocation read.
2. **Loud — a fail-closed duplicate-definition check.** A ticket ID is "defined" at exactly one canonical site (its ticket file). One parser is shared between allocator and checker so they cannot drift. Any residual collision → a reconciliation record + non-zero `aira check`.
3. **Receipt — an append-only record of every issued ID keyed by the allocating worktree**, so an ID *allocated then lost in a rebase* is caught (the an earlier ticket/384 failure). Keyed by worktree so a sibling's in-flight IDs never false-alarm this one.

`aira id <prefix>` is the allocation primitive on its own (the `make id` replacement); `aira new` composes it with ticket creation. **An allocated ID must resolve to a live ticket or an explicit `retired`/`superseded` one — never nothing.**

## 8. Coordination — the "too many cooks" fix

- **Hard ticket lease.** `aira claim <id>` → the *caller* periodically calls `aira heartbeat <id>` → liveness is computed lazily as `now < last_heartbeat + TTL`. A second claimant sees the holder + worktree + freshness and backs off; a demonstrably-dead lease can be stolen with `aira claim --steal` (logged as an event). **Daemonless, a short-lived `aira` process runs nothing between calls — heartbeat *emission* is the caller's responsibility; only the optional daemon runs a reaper.** (Concrete TTL / heartbeat-interval defaults are a Phase-1-spec decision — §19.)
- **Advisory area hints (soft).** `aira touch <paths/globs>` declares what a ticket's holder will edit. Overlap with another live claim's hints → a **warning with a stable code**, never a block — the mechanical form of "a peer session owns `runner.py`," letting agents self-serialise instead of discovering churn afterwards. The stable code lets a harness treat overlap as blocking *locally* if a project wants; hard *area-leases* remain a future hardening option (parallel to gate-hardening, §9). Advisory is the deliberate default: a hard file-lock across autonomous agents tends to deadlock and strand stale locks worse than the churn it prevents.
- **Everything queryable.** `aira list --by assignee`/`--by status` — who's on what, what's stale, what's blocked.
- **Worktree identity** is the stable git worktree gitdir id, not the mutable path (Phase-1-spec — §19).

## 9. Lifecycle and gates (V-model, advisory-first)

The two-gate loop is modelled as ticket lifecycle plus gates, and shares **one phase vocabulary** with compute attribution (§12): **`plan · plan-review · plan-fix · implement · work-review · work-fix`**, then merge. The two review *gates* sit at the **plan gate** (after `plan-review`/`plan-fix`) and the **build gate** (after `work-review`/`work-fix`). These phases are **attribution labels plus advisory gates, not a mandate** — an agent may skip phases, and trivial changes take an explicit lighter path (defined in the Phase-0 process docs, mirroring an earlier project's lighter path for small changes), so a one-line fix is never forced through a six-phase ritual.

- Each gate (`plan-approved`, `build-reviewed`, `tests-green`, `findings-triaged`, `coverage-defended`, …) is **checkable-or-manual**. `aira ready <id>` folds them into one answer, e.g. *"not ready — 2 failed, 1 unevaluated, 3 passed."*
- **Advisory first:** AIRA exposes gate state and warns on an out-of-policy transition, but does **not** block it (Phases 1–3). *Structural* validation still applies (an illegal transition to a non-existent status is refused — the integrity sense of §3); *policy* gates only warn. Specific gates can be hardened to blocking per project once the model is proven, with a justified-exception allowlist recorded on the ticket.
- **Refusals and warnings teach — process feedback at the point of friction.** When a transition is refused (structurally illegal) or warned (an advisory gate unmet), AIRA returns not only a stable code but **actionable, agent-readable guidance generated from the transition/gate table**: what state the ticket is in, exactly what advances it (which gates — checkable or manual — and the command that satisfies each), and a re-explanation of the relevant process step. An agent that never read the process docs is taught the process at the moment it needs it, and the guidance can never drift from the real rules because it is rendered from the same table the interpreter enforces (§4.3).
- **Dependency sequencing** is a first-class, checkable rule: `blocked-by` edges (§6) feed a **ready-queue** that excludes items with unmet prerequisites ("never half-build behind an un-landed dependency").

## 10. Findings and the learning loop

Typed findings make per-session review output survive by construction — the fix for pain (2). Beyond storage:

- **Cross-ticket aggregation.** `aira find ls --by category` over a window answers "which mistake classes recur," the trigger to derive a policy or a lint.
- **Reviewer/model verdict ratios.** `confirmed`-vs-`refuted` per `source` — stated honestly as an aggregate of *recorded* verdict labels (not independently verified), so an agent reads the raw ratio rather than a "crying wolf" verdict AIRA can't stand behind. Verdict provenance (who set it, whether a second check corroborated) is recorded so the gauge's universe is truthful.
- **Disposition is on the record.** `waived` findings accumulate as visible debt, with rationale; nothing is silently dropped.

## 11. Timestamps and the event log

**Every state-changing call is journaled** in the append-only event log, stamped at ingress with AIRA's clock. This gives authoritative timing for metrics (cycle time, per-stage dwell, throughput-over-time), a complete audit trail, and durations even when an agent forgets to report start/end (first-touch → last-touch on a ticket).

Honest framing of "authority":
- **Daemonless, AIRA runs *in the caller's own process on the caller's machine*, so the ingress timestamp reads the same system clock the agent would.** There is no clock authority distinct from the caller until the daemon/remote phase (§5.4); the value of stamping at ingress in Phase 1 is a single consistent capture point and a tamper-resistant `seq`, not a separate clock.
- **`seq` (monotonic DB order) is the ordering authority; `at` (wall clock, can jump on NTP) is advisory** — metrics order by `seq`, display by `at`.
- A caller-supplied timestamp is kept as a separate, lower-trust `caller_reported_at`, never conflated with `at`.
- **The durable log records mutations only** (allocations, transitions, claims/heartbeats, finding/link writes, `spend`) — read verbs (`ls`/`show`/`grep`/`count`) are called constantly and journaling each is hot-path write-amplification for no consumer. Every call is still cheaply timestamped at ingress; a full read-audit is an explicit opt-in for when a consumer exists.
- **The log (with compute-events and quota-snapshots) is append-only and *journaled to git*** (`.aira/journal/…`, §5.3), so it survives a lost or rebuilt DB; SQLite holds the queryable index over it, not the only copy.

## 12. Compute telemetry (phase- and model-attributed)

Per-ticket LLM compute, broken down by the §9 phase vocabulary (`plan · plan-review · plan-fix · implement · work-review · work-fix`). **This is a Phase-4 subsystem; the map here fixes the data shape and the one hard constraint, and defers capture internals to that subsystem's own spec.**

- **Superset schema, disjoint buckets.** `ComputeEvent` stores `fresh_input · cache_read · cache_write · output · reasoning`, populated per source; provider-absent fields read **unevaluated**, never zero.
- **The one hard constraint (a real normalisation rule).** "Cached" is a *subset* of input on OpenAI/Gemini/Codex (their prompt/input counts *include* the cached count) but *separate* on Anthropic (`input_tokens` excludes `cache_read_input_tokens`). The capture ingester normalises to disjoint buckets, and AIRA **checks they reconcile against the reported total; on a mismatch it records the datum anyway and raises a reconciliation finding** — never fail-closed-dropping external telemetry, whose vendor accounting can silently drift (a conservation *warning*, not a hard refusal — an earlier project's verification discipline without brittle coupling to a vendor's schema).
- **Capture lives out-of-core, in swappable ingesters** (AIRA is a recorder, never a collector): Claude Code → the `Stop`/`SubagentStop` hook's native `usage` object or OTEL `claude_code.api_request` (carries `cost_usd` + `query_source` for sub-agent→ticket attribution), never the transcript JSONL; Codex → `codex exec --json` `turn.completed.usage` (no cache-write figure — flagged, not zeroed); direct API → the response `usage`/`usageMetadata` at the call site. **Antigravity has no dependable per-task export today, so no capture ingester is built** — the source is marked *uncovered*, with manually-reported numbers still accepted. Each ingester posts `aira spend <ticket> --phase <p> --model <m> --in … --out … --cache-read … [--cache-write …] [--reasoning …]`.

Reference field mapping (the reason the ingesters can be thin):

| bucket | Anthropic | OpenAI (Chat / Responses) | Gemini | Codex |
|---|---|---|---|---|
| output | `output_tokens` | `completion_tokens` / `output_tokens` | `candidatesTokenCount` | `output_tokens` |
| cache_read | `cache_read_input_tokens` | `…_details.cached_tokens` | `cachedContentTokenCount` | `cached_input_tokens` |
| cache_write | `cache_creation_input_tokens` | `…_details.cache_write_tokens` (GPT-5.6+) | — | — |
| reasoning | (in output) | `…_details.reasoning_tokens` | `thoughtsTokenCount` (best-effort) | `reasoning_output_tokens` |
| model | `response.model` | `model` | `modelVersion` | `SessionMeta` |

This makes review-loop economics (review vs build tokens, findings-per-review-token) and estimate-vs-actual calibration real metrics.

**Subscription / quota usage (optional, provider-supplied).** Separately from per-call accounting, AIRA can record **account-level quota snapshots** for providers that expose them — the data agentmux surfaces (Claude from the statusline JSON, Codex from `chatgpt.com/backend-api/wham/usage`, Gemini from a self-kept tally). An opt-in `QuotaSnapshot` feed (`aira quota …`) powering a quota-burn-rate gauge (§15); best-effort, absent → unevaluated; AIRA records what an ingester hands it, never scrapes an account itself.

## 13. Discovery, indexing, compliance

Over the four planes — requirement / backlog / implementation / verification:

- **Discovery** = graph queries for what is *missing*: requirements with no ticket; code with no `covers:` (orphaned); built requirements with no verifying test; tickets claiming a module.
- **Indexing** = FTS over ticket/finding/requirement text + the covers/verifies graph → `aira grep <text>`.
- **Compliance** = graph checks both directions (generalised from `check_traceability.py`): every edge resolves; no dangling/orphaned/duplicate. These run at **`aira check` / CI, not per-edit**, so an edge left dangling mid-refactor (code moved before its test/requirement is updated) reads **unevaluated** and is *surfaced*, not a per-keystroke refusal; whether a dangling edge *blocks a merge* follows the same advisory-first → per-project-hardening path as the gates (§9). "Does this test *faithfully* verify?" is a **judged** property → AIRA marks it manual and *emits a prompt*, never faking a check. Every consistency issue is a finding with a stable code.

## 14. Review emission and multi-model routing (emit-only)

`aira review <id>` assembles a **context-loaded prompt/command bundle** (diff + requirement + test + relevant policy) for the driving agent to run — Codex-first (orthogonal, different-vendor lens), Fable-final (primary gate), Gemini for user-facing/visual, Deepseek later. The convention is **hardcoded** to start (it already lives verbatim in the user's CLAUDE.md); whether a per-project routing DSL is ever needed is deferred to §19. The agent runs the bundle (`codex exec …`, the Gemini curl, a Fable sub-agent) and reports the verdict via `aira find add … --source codex`. AIRA **records who-said-what**, so reviewer-disjointness becomes queryable and the same emitted invocation, run through a compute ingester (§12), returns both the verdict and its token cost.

## 15. Insights, metrics, reporting (honest, drillable)

The genuinely-useful subset of JIRA reporting, recast for AI-agent projects — the *drill-down* is the point, because an agent runs the investigation a human wouldn't for a dashboard blip.

**Kept** (mostly falling straight out of the event log + leases): live WIP/concurrency + area-hint-overlap rate (the too-many-cooks gauge); per-stage dwell (a growing `in-review` = the review loop is the bottleneck); collision/churn (files touched by >1 ticket, re-review count, reopen rate); recurring-mistake trend; reviewer verdict ratios; review-loop economics; traceability decay (% covered, % covered-and-verified, orphaned-code, built-without-test — the gauge that goes bad *silently*); estimate-vs-actual compute; quota-burn-rate.

**Dropped as ceremony:** story-point velocity, burndown-as-theatre, assignee leaderboards, priority pie-charts.

**Discipline:** every gauge is a **live query over the typed event/finding/compute data, never a stored numeral**; carries its **universe + as-of time**; uncomputable reads **unevaluated**; shows **direction vs a baseline window**; and **is itself a drillable saved query** ("why red?" returns the exact rows). A metric AIRA can only *judge* is refused; one it can *check* is a primitive.

`aira stats` in Phase 1 computes only what the event log + leases give (WIP/concurrency, per-stage dwell, area-hint-overlap rate); the rest land in Phase 4.

## 16. Boundary with agentmux (fully independent)

`agentmux` (the `~/tools` repo) is *process/session-level*: which agent is alive, busy/idle, quota, tmux topology, snapshot/restore. AIRA is *work-item level*. **AIRA depends on agentmux for nothing** — it runs its own lease liveness. agentmux is a **design-pattern reference and a source of learnings only** (the daemon+socket+thin-client shape; the per-provider sidecar-babysitting recipes; the quota surfaces AIRA optionally mirrors in §12). No coupling.

## 17. Command / tool surface (small + escape hatch)

CLI verbs (each compiles to the core interpreter): `init · id <prefix> · new/create · ls/list [<query>] [--by <field>] [--fields …] [order by …] · count [<query>] --by <field> · show/get · set · mv <status> · claim/release/heartbeat · touch <paths> · link <id> <rel> <id> · find add|ls · req … · ready <id> · review <id> · spend <id> … · quota … · grep <text> · backlog · roadmap · stats · check (reconcile) · watch · exec "<cmds>" · mcp · tui · install · daemon`.

**`aira init` is a first-class new-project scaffolder** (not just a registration verb): it creates the `.aira/` tree (`tickets/`, `findings/`, `requirements/`, `config`), registers the project + its owned ID prefixes in the machine-wide DB, writes a starter gate policy, seeds `REQUIREMENTS.md`/backlog stubs, and can lay down the V-model process-doc set generalised from Phase 0 (§Phase 0) so a fresh repo starts with the process discoverable.

**Token-efficient output** (kichad's rules): MCP-only 50-row cap that degrades to a **distribution over one field** on overflow (covers the whole set, states total + order + how to fetch the rest, never a silent truncation, never a summary); high-token fields opt-in via `--fields`; `count --by` for size-before-fetch; terse text default, JSON mode as the agent contract.

MCP tools (~10 + exec): `aira_create · aira_list · aira_get · aira_transition · aira_claim · aira_link · aira_finding · aira_ready · aira_review · aira_reconcile · aira_exec`. **Descriptions generated from the tables** (§4.3), so the MCP surface can never drift from behaviour. `aira install` lays down the binary, registers the Skill for detected agent CLIs, and (with `--mcp`) registers the MCP server.

## 18. Phasing (each phase = its own spec → plan → build)

- **Phase 0 — Bootstrap process docs** (the non-tool deliverable; see below).
- **Phase 1 — Coordination MVP:** git-file store + rebuildable index + reconciler (receipt/orphan check) + **mutation event log** (AIRA-stamped, `seq`-ordered), the 3-layer ID allocator, tickets/status/relations, leases + area hints, a **minimal finding/reconciliation record** (needed for reconciliation output — pulled forward from Phase 2), CLI + basic query, `aira check`, on-demand `aira backlog`, a first `aira stats`. AIRA begins dogfooding its own backlog. *(The Phase-1 spec must close, before coding: the selector/anchor grammar; ID-allocation atomicity mechanism; the git-file frontmatter + `.aira/config` schema and relation storage side; git commit semantics — does AIRA write the working tree or commit; project/worktree discovery + identity; lease TTL defaults; the stable-code catalog; reconcile trigger/scope — §19.)*
- **Phase 2 — Findings + surfaces:** the full typed findings store + query, **FTS index / `aira grep`**, MCP server + Skill (generated surface), token-efficient output/distributions.
- **Phase 3 — Traceability + gates:** requirements, `covers`/`verifies` edges, the **fail-closed traceability graph check** (built now that the graph it checks exists), checkable/manual gates, `ready`, `review` emission, discovery/compliance.
- **Phase 4 — Insights + telemetry + scale:** honest drillable gauges, compute-event ingestion + per-harness ingesters + quota snapshots, milestones/roadmap, TUI, daemon niceties, remote-transport hardening behind the §5.4 seam.

This brainstorm produces the top-level spec (this file) plus the Phase 0 plan; Phases 1–4 are dependency-sequenced and each gets its own design.

## 19. Open questions (resolved in per-subsystem specs)

**Phase-1-spec deliverables (must close before coding Phase 1):** the selector/anchor grammar (id literals, `field:value` predicates over the closed enums, and/or/not, `@N`/`next`/`prev` cursors, the `touch` glob syntax, and the exact "ambiguous" rejection rule); ID-allocation atomicity (`BEGIN IMMEDIATE`/busy-timeout, validated on the chosen driver under concurrent short-lived processes); the git-file frontmatter + `.aira/config` schema and the canonical storage side for a relation (recommend: store on the lower id, derive the inverse; define the reconciler rule when both files carry the edge); **git commit semantics** — does `aira` stage/commit ticket files or write the working tree only and leave commit to the agent (recommend the latter, with the receipt observing commit state) — this defines what "in git" means for the reconciler; project discovery (recommend git common-dir or upward `.aira/` walk → a project row) + prefix-ownership uniqueness; worktree identity (git gitdir id) + the dead-worktree liveness probe; lease TTL / heartbeat-interval defaults; the Phase-1 stable-code catalog + `aira check` exit-code convention; reconcile trigger + scope (recommend per-touched-ticket on writes, whole-project only on `aira check`).

**Later / genuinely undecided:** requirements as one-file-per vs a single registry (Phase 3); the client/server reconcile protocol (only when a remote consumer exists); whether a per-project review-routing DSL is needed *at all* (default: no — hardcode the convention); cost derivation (price table vs harness-reported `cost_usd`); deferring `$register` saved-sets until a concrete workflow needs them (cursors stay; the MCP overflow uses them).

---

## Phase 0 — bootstrap process docs (the non-tool deliverable)

Before any AIRA feature work, lay down the V-model development process **in this repo**, so agents discover and follow it strictly from commit #1. Adapted from `~/claude/an earlier project` for a **Go** project (dropping its Python/packaging specifics). Deliverables:

- **`CLAUDE.md`** — the agent guide: the loop, the worktrees-never-root rule, the git-stash-named-refs rule, `whale-run` for heavy commands, review-as-durable-artifact, and pointers to the docs below.
- **`docs/dev/agentic-development-loop.md`** — the canonical lifecycle in the §9 phase vocabulary: `plan → plan-review (Codex + Gemini) → plan gate (Fable) → plan-fix → implement (TDD) → work-review (Codex build review, then Fable two-loop adversarial) → work-fix → build gate → PR → Merge → Next`, with a per-step Who/Gate table. Two review gates: plan (before code) and build (before merge).
- **`docs/review-and-merge-policy.md`** — Codex-first (orthogonal, different-vendor) + Fable-final (primary gate), owner-delegated; the coverage gate; Gemini for user-facing text.
- **`docs/adversarial-verification.md`** — the two-loop red-team (red-team the work, then red-team the fixes), distinct-attack-angle families, "passing your own tests is not evidence of correctness."
- **`REQUIREMENTS.md` (seed) + `docs/backlog.md` (seed)** — the stable-ID registries; with the note that once AIRA (Phase 1) can track its own backlog, AIRA dogfoods itself.
- **Traceability *convention* only** — `covers:` in Go doc comments, `verifies:` in tests. **The enforcing fail-closed graph check is *not* built in Phase 0** (it would pass vacuously with no graph to check); it lands in **Phase 3**, when the covers/verifies edges exist. Phase 0 also records the `make id` → `aira id` migration intent.

The sequencing is deliberately self-referential: write the process docs and this spec as the bootstrap (this brainstorm), then run all subsequent AIRA feature work through the loop, tracked in AIRA's own backlog once Phase 1 lands. AIRA dogfooding AIRA is the best living proof the interface is good.
