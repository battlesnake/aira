# AIRA — design spec (top-level scope + architecture)

- **Status:** designed (this document); not yet built.
- **Date:** 2026-08-07 (v3 — adds the test-report and subprocess-runner subsystems, a refined four-class durability model, and research-driven additions; v2 folded a four-lens Claude red-team + a Gemini pass).
- **Scope:** the whole-product scope, architecture, data model, and phasing. Each subsystem gets its own spec → plan → build cycle after this. This document is the map, not the construction drawing — where it says "resolved in the Phase-N spec," that is deliberate.
- **Companion:** [Phase 0 process-docs plan](#phase-0--bootstrap-process-docs-the-non-tool-deliverable).

---

## 1. What AIRA is, in one paragraph

**AIRA is "JIRA for AI agents": a machine-local (later optionally remote) work-item tracker, coordination layer, findings store, traceability assistant, metrics surface, and execution/verification substrate for AI coding agents working across one or more repos.** It is a single static Go binary, `aira`, exposed identically as a CLI, an MCP server, a Skill, an optional daemon, and a human TUI — all thin adapters over one core. It exists to kill three *measured* pains — colliding ID allocation, review findings that evaporate, and "too many cooks" churn between agents in different worktrees — and, as it grows, to absorb the surrounding execution toil (running commands, capturing output, tracking tests) that agents pay by hand today.

## 2. The problem (evidence, not assertion)

Observed in `~/claude/an earlier project` — a ~194-live-worktree, multi-agent monorepo — and confirmed by a sweep of its docs and 4,000+ commit history:

1. **ID allocation is out-of-repo, bespoke machinery.** `make id` drives an `flock`'d counter in the git *common-dir* (`next = max(counter, file_max)+1`, a self-healing cache over the authoritative markdown), a fail-closed duplicate-ID gate, and a "receipt" log that catches IDs *burned in a rebase* (an earlier ticket/384 were committed then dropped in the rebase meant to land them; an earlier ticket collided across clones). It works, but it's Python/bash bolted to one repo's two markdown tables.
2. **Findings evaporate.** Per-session Codex/Fable review findings die in chat unless a human converts them to a committed doc, a backlog row, or a test — 92 hand-written `docs/reviews/`+`docs/audits/` files exist only by manual transcription. The two-loop review found *seven real bugs in code that passed its own seventeen tests*, durable only because someone wrote them down. Nothing is queryable, so the same defect classes resurface loop after loop.
3. **Coordination is manual prose.** No "who owns what" database — worktree isolation plus owner sentences ("a peer session owns `runner.py`"), contended files hand-queued "for a non-parallel session," and `base-red owned by other sessions` triaged from memory at merge time.

The research also surfaced sharper, less-obvious pains AIRA is well-placed to prevent: half-building behind an un-landed prerequisite (the most expensive failure — throwaway work), gates that read green because the check *silently never ran* (semgrep skipped a 1.77 MB module on every commit forever; a blank pipeline leg reported green), stored-numeral quality baselines that merge-conflict and need constant re-baselining, and heavy subprocesses that OOM-kill the desktop or get killed by an agent stopping the *shared* RAM slice.

## 3. Goals and non-goals

**AIRA is:** the authority for work-item IDs, status, relations, leases, findings, requirements, gates, compute, timing, test outcomes, and subprocess runs — across worktrees and (later) machines; a tool for **agents first**, humans occasionally; and a **recorder and orchestrator**, not a judge (it tracks state, allocates IDs, holds leases, stores typed findings, owns the requirement↔code↔test graph, runs and records subprocesses, *emits* review/verification prompts, and *records* verdicts — it never calls a model).

**AIRA is not:** an agent/session runner or process supervisor in the *observability* sense — that is `agentmux`'s domain, and AIRA depends on it for **nothing** (§18); a model gateway (it emits prompts; the agent runs them); or a CI system (it records verdicts and runs its own cheap deterministic checks; anything heavy or judged is emitted as a command/prompt and its result recorded). It complements git and PRs; the durable content record lives in git.

**Two senses of "block," used precisely throughout:** *integrity* checks (ID uniqueness, graph resolvability) are **fail-closed and refuse an invalid operation** (principle §4.5). *Lifecycle gates* are **advisory and never block a valid-but-out-of-policy transition** in Phases 1–3 (§9). One refuses the impossible; the other declines to police the merely-unwise.

## 4. Design principles

From `kichad` (a Go CLI+MCP+Skill tool that has been a large productivity multiplier for agents), adapted:

1. **Primitives, not judgement.** Build a feature only if its result can be *checked*, not merely *judged*. Where only a judgement is possible, supply the primitive and *emit a prompt*; record the returned verdict. **Composition is a feature; output size is a cost.**
2. **One core, thin faces.** All behaviour lives behind a single interpreter, `core.Do(ctx, req) → resp`. Every surface is a thin adapter; the three that expose the command language — **CLI, MCP, Skill** — compile to the *same verbs* and cannot diverge. (TUI/daemon are adapters too — §4.8.)
3. **Generate every doc surface from the dispatch tables.** MCP schemas, `--help`, the agent guide, and the transition/gate *guidance text* (§9) are rendered from the command/field/status/relation/gate tables the parser reads — never transcribed.
4. **Verdict-first, three verdicts never two.** Every gate/check is `pass | fail | unevaluated`; a thing AIRA cannot stand behind reads **unevaluated**, counted apart from failures, never silently passed. Every metric that can't be computed reads unevaluated, never a fake zero.
5. **Refuse rather than guess.** An ambiguous reference (a selector naming >1 ticket) is rejected — the fail-closed *integrity* sense of §3.
6. **Silence is signal; errors are stable codes.** A clean check prints nothing; every rejection carries a stable machine code (`match on the code, not the wording`).
7. **Small tool surface + a full escape hatch** (~10 MCP tools + `aira_exec`).
8. **Layer strictly downward; the in-binary adapter layer holds no domain logic.** `store ← domain ← query ← interp ← adapters(cli/mcp/tui/daemon)`. Out-of-core *ingesters* (compute/quota/test-report capture shims, §12/§13) are separate small programs, not this layer; they may hold provider-normalisation logic.
9. **Self-verifying, recoverable edits.** Transitions are structurally validated before write; the reconciler is the safety net; nothing is written to git that would leave an unresolvable graph.

## 5. Architecture

### 5.1 One core, thin faces; strict downward layering

```
internal/store/    git files (content truth) + SQLite (coordination + telemetry + index) + reconciler
internal/domain/   tickets, relations, leases+area-hints, findings, requirements, gates,
                   compute-events, quota-snapshots, test-reports, runs, projects, milestones, event log
internal/query/    selectors, cursors, distributions, FTS search
internal/interp/   the command language: parse req → dispatch → shape output
internal/runner/   subprocess launch/capture/kill (§14) — in-binary (must be the parent/supervisor)
cmd/aira/          thin adapters:  cli · mcp · tui · daemon · install
```

`core.Do` is the single dispatch. **Embedded store:** leaning SQLite (WAL, FTS5) via a pure-Go driver (`modernc.org/sqlite`) to preserve the single-static-binary/no-cgo property; the driver gates ID-allocation atomicity and lease concurrency, so it is decided *first* in the Phase-1 spec.

### 5.2 Daemon-optional runtime

**Correctness never requires a running service** — critical for fresh worktrees and short-lived sub-agents. Daemonless (default): each call opens SQLite (WAL), reconciles the touched ticket(s) on demand, evaluates lease liveness lazily from `last_heartbeat`. Optional `aira daemon`: load-once stateful session, live `aira watch`, continuous reconciler, heartbeat reaper — **and the supervisor for detached subprocess runs** that aren't launched under a systemd scope (§14). Degrades cleanly.

### 5.3 Storage: git is the content truth; four durability classes

Git is the **sole authority for ticket content** — one file per ticket (`.aira/tickets/AIRA-42.md`, frontmatter + body), so two agents on *different* tickets never touch the same file; findings and requirements in `.aira/findings/`, `.aira/requirements/`. Human indices (`BACKLOG.md`, `ROADMAP.md`) are **rendered on demand** (`aira backlog`/`roadmap`), never committed — committed generated files merge-conflict across worktrees by construction (an earlier project pays exactly this in 37 re-home-onto-master + registry-automerge-drift commits).

Everything else is DB-resident, in **four explicit durability classes** (a refinement earned by the runner design):

| Class | What | Durability |
|---|---|---|
| **Content** | tickets, findings, requirements | **git** — sole truth, survives machine loss |
| **Audit / receipts** | the mutation event log (§11): allocations, transitions, claims, finding/link writes | **git-journaled** append-only (`.aira/journal/<worktree-id>/<date>.ndjson`, partitioned so it merges cleanly + stays bounded) — must survive, it's what catches git mistakes |
| **Operational telemetry** | compute-events, quota-snapshots, **test-reports**, **run metadata** | **DB-only, retention-capped** — high-volume, best-effort; losing it on a rare DB rebuild is tolerable, metrics rebuild forward |
| **Ephemeral coordination** | leases, heartbeats, worktree bindings, ID-counter cache | **DB-only, self-heals** — dead leases lapse, worktrees re-register, the counter re-derives from a git scan (§7) |

Plus **run output blobs** (stdout/stderr/stdin, §14): machine-local files (`~/.local/state/aira/runs/…`), gitignored, compressed and evicted under a disk cap — never git, never SQLite blobs.

**The reconciler** is small and earned — an earlier project's receipt/orphan check, generalised: every allocated ID resolves to a git ticket file and vice-versa (a burned-in-rebase ID → a reconciliation record); a lease on a vanished worktree → a record; a duplicate ID definition → a record; a stale index → a rebuild (silent). It can also carry a **submodule-pointer-drift** record (retiring an earlier project's manual `git -C <sub> log origin/master..HEAD` check). Reconciliation records are DB-resident coordination artifacts (stable-coded), distinct from git-durable review findings; both share the Finding schema.

### 5.4 Remote-ready seam (design now, build local)

`core.Do(req) → resp` speaks **serializable JSON**, so the same protocol runs in-process, over a Unix socket, or over TCP/HTTP+auth — byte-identical interface, no command semantics assuming a local store. **`Store` is an interface** (local SQLite / remote RPC, drop-in). Reconciliation is a plain in-process function in Phase 1; a client/server reconcile protocol (git client-side, DB server-side) is an **open question (§21)**, not a v1 commitment.

## 6. Data model

Closed, small enums everywhere (an earlier project's audit found real bugs from overloaded string fields). Durability class per entity is as in §5.3.

- **Project** — slug, repo path(s), owned ID prefixes, **gate policy** (incl. the path→{suite, review-tier} map — §16), milestones, config (`.aira/config`).
- **Ticket** — `PREFIX-N`; title; body; **status** (`draft → planned → in-progress → in-review → done`, + `blocked`, `retired`, `superseded`); kind; severity (`P0|P1|P2`); assignee/agent/session/worktree-id; milestone; labels; timestamps (created/started/ended, event-log-authoritative — §11); estimated compute (plan) + actuals from linked compute-events.
- **Relation** — bidirectional, typed, stored once, presented both ways: `blocks`/`blocked-by`, `parent`/`child`, `relates`, `duplicates`/`duplicated-by`, `supersedes`/`superseded-by`, and **`resolves`/`resolved-by`** (a "centralise X" finding → the guard/ticket that closes it; an open finding whose guard never landed stays visible as debt — §10). The **ready-queue (§9) keys on `blocked-by`**.
- **Lease** — ticket-id, holder, acquired-at, **last-heartbeat** (expiry computed `last_heartbeat + TTL`). Plus **area hints** (declared globs; overlap → warning, never a block, §8).
- **Finding** — id; **subtype** (`review` = git-durable | `reconciliation` = DB-resident); ticket-id (+ optional requirement-id, `file:line`); **category** (kebab slug — incl. `flaky-test` derived from the test archive, §13); severity; **verdict** (`confirmed|refuted|plausible`); **source** (`codex|fable|gemini|deepseek|semgrep|human|aira`); **disposition** (`open|fixed|accepted|waived`) + rationale.
- **Requirement** — own prefix/registry; text; status (`built|partial|designed|planned|boundary|retired|superseded`); typed **`covers`** (→code) and **`verifies`** (→test) edges.
- **Gate** — name; kind (`checkable` with a cheap evaluator | `manual` attested | **`ratchet`** = no-regression-vs-a-live-baseline, §9); the lifecycle step it guards; **`proven-to-fire`** flag (§9). An un-attested manual gate reads `unevaluated`; once attested, `pass`.
- **ComputeEvent** — `{ticket, phase, provider, model, fresh_input, cache_read, cache_write?, output, reasoning?, cost?, at, agent/session, caller_reported_at?}` (§12). *Operational telemetry.*
- **QuotaSnapshot** — optional provider-supplied `{provider, account, used, limit, window, resets_at, at}` (§12). *Operational telemetry.*
- **TestReport** (+ per-test results) — `{id, ticket?, phase?, commit, branch, worktree-id, agent/session, at, results:[{name, outcome(pass|fail|skip|error), duration, message?}]}` (§13). *Operational telemetry.*
- **Run** — `{id, ticket?, phase?, label, argv, prefix, cwd, env-digest, merge_streams, buffering(none|realtime|pty), status(running|exited|killed|lost), exit, signal, started/ended, cpu_user, cpu_sys, peak_rss, output-refs{in,out,err|log}}` (§14). *Metadata: operational telemetry (DB); blobs: local+capped.*
- **Event** — the append-only *mutation* log: `{seq, at, actor, verb, target, payload-digest, caller_reported_at?}`. `seq` (monotonic) is the **ordering authority**; `at` (AIRA-stamped) is advisory (§11).

## 7. ID allocation

Report-time allocation. Three layers: **(1) the machine-wide DB counter is the atomic cross-worktree authority** (`new = counter+1` in one exclusive txn — it *must* be the DB, since a sibling worktree's just-committed `AIRA-50` isn't visible in this checkout; the counter is a cache that self-heals from a one-shot git scan on rebuild, `counter = max(counter, git_max)`, a repair pass not a per-allocation read); **(2) a fail-closed duplicate-definition check** (one shared parser between allocator and checker); **(3) a worktree-keyed receipt** catching an ID allocated-then-lost-in-a-rebase. `aira id <prefix>` is the primitive (retiring `make id`); an allocated ID must resolve to a live ticket or an explicit `retired`/`superseded` one — never nothing.

## 8. Coordination — the "too many cooks" fix

**Hard ticket lease** (`aira claim` → the *caller* periodically `aira heartbeat` → lazy liveness `now < last_heartbeat + TTL`; `--steal` a dead lease, logged). **Daemonless, a short-lived process runs nothing between calls — heartbeat *emission* is the caller's job; only the daemon reaps.** **Advisory area hints** (`aira touch <globs>` → overlap with another live claim's hints returns a **stable-coded warning, never a block** — the mechanical form of "a peer session owns `runner.py`", replacing hand-written sequencing notes). The stable code lets a harness enforce locally; hard *area-leases* remain a future hardening option (a hard file-lock across autonomous agents tends to deadlock/strand worse than the churn — advisory is the deliberate default; **the advisory-vs-hard choice is an open decision, §21**). **Worktree identity** = the stable git gitdir id, not the mutable path. `aira list --by assignee`/`--by status` makes who-is-on-what and base-red a query, not memory.

## 9. Lifecycle and gates (V-model, advisory-first) — prevention over recording

The two-gate loop is modelled as lifecycle + gates, sharing **one phase vocabulary** with compute attribution (§12): **`plan · plan-review · plan-fix · implement · work-review · work-fix`**, then merge; the two review *gates* sit at the **plan gate** and the **build gate**. **These are attribution labels + advisory gates, not a mandate** — an agent may skip phases, and trivial changes take an explicit lighter path (Phase-0 docs, mirroring an earlier project), so a one-line fix is never forced through a six-phase ritual.

- **The `blocked-by` ready-queue is the highest-value early feature — it *prevents* rework rather than recording it.** "Never half-build behind an un-landed prerequisite" is the most expensive failure (throwaway work), caught today only by human vigilance or a *late* two-loop review. `aira ready <id>` excludes items with unmet `blocked-by` edges, so a prerequisite is surfaced *before* effort is spent, not after. **Phase 1 leads with this** (§20).
- Each gate is **checkable | manual | ratchet**. `aira ready <id>` folds them: e.g. *"not ready — 2 failed, 1 unevaluated, 3 passed."*
- **Verdict provenance — a green gate must have *actually fired*.** A checkable gate is not `pass` unless it is attested to have *fired on a known-bad input in the lane that runs it* (its `proven-to-fire` flag); a check that could not run — a scanner that skipped the file, a leg that collected zero tests — reads **`unevaluated`**, never green. This is the direct fix for an earlier project's "green guard that silently never ran" class (semgrep skipping a 1.77 MB module forever; a blank pipeline leg reporting pass).
- **Ratchet gates compute their baseline live, never a committed numeral.** A `ratchet` gate ("no new test failures / no coverage drop / no new dyntyping vs baseline") derives its baseline **live over recorded data** (the test-report archive §13, findings, compute), so there is no committed baseline file to merge-conflict and no re-baseline-commit ceremony (an earlier project pays 28 re-baseline commits + committed `baselines/*.json` for exactly this). This is §17's "every gauge is a live query, never a stored numeral" applied to quality gates.
- **Advisory first:** structural validation still refuses an illegal transition (integrity, §3); policy gates only warn. Hardening to blocking is per-project, with a justified-exception allowlist. **Accepted coverage-gap debt** is a *waived* Finding + a gate attestation with provenance (who accepted, why) — one query, not PR prose.
- **Refusals and warnings teach** — a refused/warned transition returns actionable, agent-readable guidance generated from the transition/gate table (§4.3): current state, exactly what advances it, and the process step re-explained.

## 10. Findings and the learning loop

Typed findings make review output survive by construction — the fix for pain (2). **Cross-ticket aggregation** (`aira find ls --by category` over a window) answers "which mistake classes recur" — the trigger to derive a lint/policy, invisible today. **Reviewer verdict ratios** (`confirmed`-vs-`refuted` per `source`) are stated honestly as aggregates of *recorded* labels with provenance, not a verdict AIRA can't stand behind — surfacing reviewer-disjointness and kill-rate (the real quality signal an earlier project observes but cannot measure). **Disposition is on the record** — `waived` findings accumulate as visible debt with rationale. A **`resolves` edge** links a "centralise X" finding to the guard/ticket that closes it, so an unmitigated finding stays visible instead of depending on memory. Findings can be **seeded** from existing artifacts (§16, `aira import`) so the corpus starts populated.

## 11. Timestamps and the event log

**Every state-changing call is journaled** (append-only, stamped at ingress with AIRA's clock; git-journaled per §5.3), giving authoritative timing for metrics, an audit trail, and durations even when start/end aren't reported. Honest framing: **daemonless, AIRA runs in the caller's own process, so the ingress timestamp reads the caller's clock** — there is no clock authority distinct from the caller until the daemon/remote phase; the value is one consistent capture point + a tamper-resistant `seq`. **`seq` (monotonic) is the ordering authority; `at` (wall clock) is advisory.** A caller-supplied timestamp is a separate lower-trust `caller_reported_at`. **The git-journaled log records *significant* mutations** (allocations, status transitions, claims/releases/steals, finding/link/relation writes) — **not** high-frequency heartbeats (which only refresh ephemeral DB state, §5.3) nor per-run / per-`spend` telemetry detail (operational telemetry, DB-only), which would swamp the audit trail; read verbs are timestamped cheaply but not journaled at all (hot-path write-amplification for no consumer; full read-audit is opt-in).

## 12. Compute telemetry (phase- and model-attributed)

Per-ticket LLM compute, broken down by the §9 phase vocabulary. *Operational telemetry (DB-only, retention-capped — §5.3).* **Phase-4 subsystem;** the map fixes the data shape and one hard constraint, deferring capture internals to its own spec. Superset schema, disjoint buckets (`fresh_input · cache_read · cache_write · output · reasoning`); provider-absent fields read **unevaluated**, never zero. **The one hard constraint:** "cached" is a subset of input on OpenAI/Gemini/Codex but separate on Anthropic — the ingester normalises to disjoint buckets and, on a mismatch vs the reported total, **records the datum anyway and raises a reconciliation finding** (a conservation *warning*, never a fail-closed drop of drifting vendor telemetry). Capture is out-of-core, in swappable **ingesters** (Claude Code → `Stop`/`SubagentStop` hook `usage` or OTEL `claude_code.api_request`, never the transcript; Codex → `codex exec --json`; direct API → the response `usage`/`usageMetadata`; **Antigravity has no dependable per-task export — no ingester built, source marked uncovered**, manual reports still accepted). Field mapping (Anthropic `cache_read_input_tokens`/`cache_creation_input_tokens`; OpenAI `…_details.cached_tokens`/`cache_write_tokens`(GPT-5.6+)/`reasoning_tokens`; Gemini `cachedContentTokenCount`/`thoughtsTokenCount`; Codex `cached_input_tokens`/`reasoning_output_tokens`). Makes review-loop economics and estimate-vs-actual real (§17). **Subscription/quota** (`aira quota`, opt-in provider-supplied `QuotaSnapshot`) powers a quota-burn-rate gauge; AIRA records what an ingester hands it, never scrapes.

## 13. Test reports

A standard-format test-report archive so test outcomes become durable, trended, and checkable rather than a scrollback or an ephemeral `~/tmp` log. *Operational telemetry (DB-only, retention-capped).* **Ingestion is out-of-core**, like §12: `aira test-report add --format <junit|go-json|tap|pytest-json|…> < report`, normalising the common CI formats into one internal schema — or **auto-ingested from a run** (`aira run --report junit`, §14) so a test run's exit + parsed output becomes a report in one shot. Each report is tagged with commit/branch/worktree/agent/ticket/phase + an AIRA timestamp.

Everything it enables is a **checkable primitive, not a judgement:**
- **Flaky tests** — the same test at the same commit both passing and failing across reports → a flakiness signal + a `flaky-test` finding category + a §17 gauge. AIRA reports the fact; it doesn't decide why.
- **Long-lived master breakage** — "test X has been red on `master` since commit/date Y" is a duration query over the archive.
- **Ratchets** — "no new failures / no coverage drop vs a **live baseline** over the archive" is precisely an earlier project's zero-new-failures-vs-merge-base gate, as a **`ratchet` gate kind (§9)** — no committed baseline, folded into `aira ready` and the build gate.

## 14. Subprocess runner

Agents run heavy commands today via a `whale-run` prefix (a systemd slice with a hard RAM cap, so an OOM kills the job not the desktop). Three documented pains motivate AIRA owning the *run*, not just the work-item: (a) a **dated OOM incident** (a 53 GB run killed the whole desktop, 2026-05-29) drove the cap; (b) the **safe and catastrophic kill commands differ by one token and sit adjacent in the docs** (`stop whale-run-<name>.scope` vs `stop whale.slice` — the latter SIGTERMs *every* session's in-flight work), now a HARD rule agents must remember; (c) agents **re-run a 4,260-test suite** to re-inspect output they piped through the wrong `grep`/`tail`, and hand-roll capture (`codex -o out.md`; *"record the exit code, never infer green from a truncated log"*). There is already a universal prefix every heavy command flows through — the exact interposition point.

AIRA becomes the **outer** wrapper (lifecycle/capture/handle/kill); the slice stays the **inner** wrapper (memory containment). This keeps agentmux fully independent (§18): the launch prefix is a configurable string.

- **Launch.** `aira run [--ticket <id>] [--phase <p>] [--label <l>] [--follow|--detach] [--merge] [--realtime|--pty] [--stdin <file|->|--no-stdin] -- <argv>` → a run handle. Launched via a configurable **`run.prefix`** (`.aira/config`, default `agentmux whale run --`, empty = bare). Records launch as a §11 event; on exit appends exit/signal/wall/cpu/peak-RSS. Pure recorder (§3).
- **Scoped, safe kill — the headline safety win.** Because AIRA launched the process it knows the exact scope/pgid, so `aira run-kill <handle>` stops *only that* (`stop whale-run-<name>.scope`, or `kill -- -<pgid>` for a bare run) — the catastrophic `stop whale.slice` is **unreachable through AIRA**. This mechanises the HARD rule instead of documenting around a one-token footgun.
- **I/O model — in/out/err preserved *and* live.** Each stream is tee'd (process ↔ caller-live ↔ file): **stdin** streams into the process while open and is captured to `RUN-n.in`; agents can also push into a running process's stdin (`aira run-input <handle> [data|--eof]`). **stdout** and **stderr** stream live to the runner's own out/err (and `run-log --follow`) and are captured **separately** to `RUN-n.out`/`RUN-n.err`. **Opt-in `--merge`** = a real `dup2(stderr→stdout)` at exec (faithful kernel ordering) into one `RUN-n.log`, for when **temporal consistency** of interleaved out+err is needed (separate pipes can't guarantee cross-stream order — that's why merge exists). The runner **always drains pipes to the capture files** regardless of whether anyone reads the live passthrough, so a slow/absent monitor never backpressures the child; binary-safe.
- **Prompt output — `--realtime` / `--pty`.** Many programs *block*-buffer stdout to a pipe. `--realtime` **replicates `stdbuf` via the child's environment** (`LD_PRELOAD=libstdbuf.so` + `_STDBUF_O`/`_STDBUF_E`, line or unbuffered; plus `PYTHONUNBUFFERED=1`) rather than splicing the `stdbuf` binary into the argv — so it never perturbs the carefully-passed argv and **keeps out/err separate**; it works for glibc-stdio tools and is a no-op elsewhere (Go is already unbuffered; Rust/Python/static/setuid it can't reach). `--pty` allocates a pty so `isatty()` is true and nearly every runtime line-buffers — universal, but **merges** out+err and injects TTY escape codes. AIRA records which tactic ran; **the file capture is always complete regardless** — the flags affect only how promptly a live `--follow` sees output, never whether it's captured.
- **Re-filter without re-running.** `aira run-log <handle> [--stream in|out|err|merged] [--tail N] [--head N] [--grep RE] [--since <line/byte>] [--full] [--follow]` re-queries the *same stored bytes* — a wrong first filter costs one cheap re-read, never a re-run of a minutes-long suite. §19's head+tail+overflow discipline applies so a multi-MB log never silently truncates. Replaces the hand-rolled `-o out.md`.
- **Storage + security.** Metadata → DB (operational telemetry); blobs → machine-local gitignored files, per-run byte caps (head+tail retention on overflow), **zstd-compressed when warm, evicted oldest-first under a max-disk-usage cap** (+ optional TTL). After eviction the metadata + any *extracted* report/error-summary survive, so the run stays auditable (just not re-greppable). `store-stdin`/`store-env` off by default; secret-shaped tokens redacted; raw logs never journaled to git.
- **Streaming / detach.** `--detach` returns the handle without blocking the short-lived daemonless process. Because `agentmux whale run` launches a **systemd scope, the scope itself is the supervisor**, so a detached run survives a daemonless `aira` exiting and AIRA re-attaches by scope name; a **bare** (prefix-less) detached run needs the daemon (§5.2). Liveness via a pidfd/scope-active probe (mirroring the §8 lease model); dev servers get a background handle, not a blocked call.
- **Resource accounting from the cgroup, not the child.** `getrusage(RUSAGE_CHILDREN)` misses grandchildren under the whale scope, so peak-RSS/CPU are read from the scope's cgroup (`memory.peak`, `cpu.stat`). This feeds estimate-vs-actual compute (§17) and turns the guessed ⅔-RAM `MemoryMax` into a **data-driven** cap tuned from the recorded per-phase peak-RSS distribution.
- **Wiring.** A run tagged `--phase work-review --tool codex` auto-emits a ComputeEvent (parse the tool's `--json` usage; absent tokens → record wall/cpu/peak-RSS, buckets `unevaluated`); a test run `--report`s a §13 report and **auto-attests the `tests-green` gate from the stored exit code** — "record the exit code, never infer green from a truncated log" becomes a non-forgeable gate input.
- **Execution fidelity.** Faithful cwd + env (an earlier project tests are cwd-sensitive); argv-parse care (a single leading `--`, keep `-n auto tests/` intact); interactive approval prompts (Claude's ✋) stay out of scope.

This grows AIRA from tracker/coordinator into the agent's **execution + verification substrate** — a conscious scope step. The recorder model extends there cleanly (record the work, not just the work-items), and it composes with compute telemetry (§12), test reports (§13), and gates (§9).

## 15. Discovery, indexing, compliance

Over requirement / backlog / implementation / verification: **discovery** = graph queries for what's *missing* (uncovered requirements, orphaned code, built-without-test); **indexing** = FTS over ticket/finding/requirement text + the covers/verifies graph (`aira grep`); **compliance** = graph checks both directions (generalised from `check_traceability.py`), run at **`aira check`/CI, not per-edit**, so a mid-refactor dangling edge reads `unevaluated` and is surfaced, not a per-keystroke refusal — whether it *blocks a merge* follows the advisory-first→hardening path (§9). "Does this test *faithfully* verify?" is judged → AIRA marks it manual and emits a prompt, never faking a check.

## 16. Review emission, routing, and import

`aira review <id>` assembles a context-loaded prompt/command bundle (diff + requirement + test + policy) for the agent to run — Codex-first, Fable-final, Gemini for user-facing/visual, Deepseek later; the convention is **hardcoded** to start (a per-project routing DSL is deferred until a second project needs one, §21). AIRA can also **emit a review-depth recommendation** from ticket area/kind/severity (the analogue of an earlier project's `review_tier.py` Tier 0–3, MAX-over-paths, fail-closed default-up), folded into the bundle and tuned on recorded review-loop economics (§12) — the path→review-tier map lives in the project gate policy (§6), subsuming a bespoke external classifier. The agent runs the bundle and reports the verdict via `aira find add … --source codex`; AIRA records who-said-what. **`aira import`** seeds the stores from existing artifacts (a repo's markdown backlog/requirements, and hand-written `docs/reviews/` docs → findings), so AIRA starts populated and can answer "which classes recur" over history on day one.

## 17. Insights, metrics, reporting (honest, drillable)

The genuinely-useful JIRA subset, recast for agents — the *drill-down* is the point (an agent runs the investigation a human wouldn't for a dashboard blip). **Kept:** WIP/concurrency + area-hint-overlap rate (too-many-cooks); per-stage dwell; collision/churn (files touched by >1 ticket, re-review count, reopen rate); recurring-mistake trend; reviewer verdict ratios; review-loop economics; traceability decay (the gauge that goes bad *silently*); estimate-vs-actual compute; **flaky-rate, master-red-duration, ratchet status** (§13); **run/build wall-clock, slowest-command, peak-RSS per phase** (§14); quota-burn-rate. **Dropped as ceremony:** story-point velocity, burndown theatre, assignee leaderboards, priority pies. **Discipline:** every gauge is a **live query over the typed event/finding/telemetry data, never a stored numeral**; carries its universe + as-of time; uncomputable reads `unevaluated`; shows direction vs a baseline window; and **is itself a drillable saved query**. This applies an earlier project's own "write the query, not the numeral" / "a figure with no committed reproduction is retired" law to the process layer, dissolving the stored-numeral-rot class for the data AIRA owns.

## 18. Boundary with agentmux (fully independent)

`agentmux` is *process/session-level observability* (which agent is alive, busy/idle, quota, tmux topology). AIRA is *work-item + run level*. **AIRA depends on agentmux for nothing** — it runs its own lease liveness, and the subprocess launch prefix is a configurable string (default happens to be `agentmux whale run`, but empty = bare, or any other launcher). agentmux is a design-pattern reference and a source of learnings only (the daemon+socket+thin-client shape; per-provider sidecar recipes; the quota surfaces §12 optionally mirrors).

## 19. Command / tool surface (small + escape hatch)

CLI verbs: `init · id <prefix> · new/create · ls/list [<query>] [--by F] · count [q] --by F · show/get · set · mv <status> · claim/release/heartbeat · touch <paths> · link <id> <rel> <id> · find add|ls · req … · ready <id> · review <id> · import … · run … · run-kill · run-log · run-input · test-report add|ls · ratchet … · spend <id> … · quota … · grep <text> · backlog · roadmap · stats · check · watch · exec "<cmds>" · mcp · tui · install · daemon`. **`aira init` is a first-class new-project scaffolder** (the `.aira/` tree, project + prefixes + gate policy registration, seed registries, optionally the Phase-0 process docs). **Token-efficient output:** MCP-only 50-row cap → distribution-over-one-field on overflow (never silent truncation, never a summary — the same discipline governs `run-log` on multi-MB output); `--fields` opt-in; `count --by` size-before-fetch. MCP tools (~10 + `aira_exec`): `aira_create · aira_list · aira_get · aira_transition · aira_claim · aira_link · aira_finding · aira_ready · aira_review · aira_run · aira_run_output · aira_run_kill · aira_reconcile · aira_exec` — descriptions generated from the tables (§4.3).

## 20. Phasing (each phase = its own spec → plan → build)

- **Phase 0 — Bootstrap process docs** (non-tool deliverable; below).
- **Phase 1 — Coordination MVP, led by prevention:** git-file store + rebuildable index + reconciler + git-journaled event log, the 3-layer ID allocator, tickets/status/relations, leases + area hints, **and the `blocked-by` ready-queue + `aira ready`** (the highest-value prevention feature — lead with it), a minimal finding/reconciliation record, CLI + query, `aira check`, on-demand `aira backlog`, a first `aira stats`. AIRA starts dogfooding its own backlog. *(Phase-1 spec must close the §21 Phase-1 items before coding.)*
- **Phase 2 — Findings + surfaces + import:** full typed findings store + query, **`aira import`** (seed from existing markdown + review docs), FTS/`aira grep`, MCP server + Skill, token-efficient output.
- **Phase 3 — Traceability + gates:** requirements, `covers`/`verifies`, the fail-closed traceability graph check (now the graph exists), checkable/manual/**ratchet** gates + **verdict provenance**, `ready`, `review` emission + review-depth recommendation, discovery/compliance.
- **Phase 4 — Insights + telemetry + test reports:** honest drillable gauges, compute-event ingestion + per-harness ingesters + quota snapshots, **test-report archive + flaky/ratchet gauges**, milestones/roadmap, TUI, daemon niceties, remote-transport hardening behind the §5.4 seam.
- **Phase 5 — Subprocess runner:** the §14 substrate (scoped-kill, tee'd I/O, re-filter handle, cgroup rusage, telemetry/gate wiring). Last because it depends on the daemon niceties (detached supervision), telemetry (§12), test-reports (§13), and gates (§9) — and is the largest single subsystem.

## 21. Open questions (resolved in per-subsystem specs)

**Phase-1-spec deliverables (close before coding):** the selector/anchor grammar (id literals, `field:value` predicates, and/or/not, `@N`/`next` cursors, `touch` globs, the "ambiguous" rejection rule); ID-allocation atomicity (`BEGIN IMMEDIATE`/busy-timeout on the chosen driver under concurrent short-lived processes); the git-file frontmatter + `.aira/config` schema and the canonical storage side for a relation (recommend: lower id, derive the inverse); **git commit semantics** (recommend: AIRA writes the working tree, the agent commits; the receipt observes commit state) — defines what "in git" means for the reconciler; project/worktree discovery (git common-dir / upward `.aira/` walk) + prefix-ownership uniqueness; lease TTL / heartbeat-interval defaults; the stable-code catalog + `aira check` exit-code convention; reconcile trigger/scope (recommend: per-touched-ticket on writes, whole-project on `aira check`).

**Later / genuinely undecided:** **area hints — advisory-only (leaning) vs adding optional hard area-leases** (a standing user decision); requirements as one-file-per vs a single registry; the client/server reconcile protocol (only when a remote consumer exists); whether a per-project review-routing DSL is needed at all; cost derivation (price table vs harness-reported `cost_usd`); deferring `$register` saved-sets until a workflow needs them. **Runner (§14):** blob retention policy defaults + compression thresholds; cgroup-stats read path across cgroup v2 layouts; PTY interaction with `--merge`; how far to normalise test-report formats (§13). Compute/telemetry retention caps (§5.3 operational-telemetry class).

---

## Phase 0 — bootstrap process docs (the non-tool deliverable)

Before any AIRA feature work, lay down the V-model development process **in this repo**, so agents follow it from commit #1. Adapted from `~/claude/an earlier project` for **Go** (dropping its Python/packaging specifics):

- **`CLAUDE.md`** — the loop, worktrees-never-root, git-stash-named-refs, `whale-run` for heavy commands (and target-your-own-scope-never-the-slice), review-as-durable-artifact, pointers below.
- **`docs/dev/agentic-development-loop.md`** — the lifecycle in the §9 phase vocabulary: `plan → plan-review (Codex+Gemini) → plan gate (Fable) → plan-fix → implement (TDD) → work-review (Codex build review, then Fable two-loop) → work-fix → build gate → PR → Merge`, with a per-step Who/Gate table and an explicit **lighter path for trivial changes** (§9).
- **`docs/review-and-merge-policy.md`** — Codex-first (orthogonal, different-vendor) + Fable-final (primary gate), owner-delegated; coverage gate; Gemini for user-facing text.
- **`docs/adversarial-verification.md`** — the two-loop red-team; "passing your own tests is not evidence of correctness."
- **`REQUIREMENTS.md` (seed) + `docs/backlog.md` (seed)** — stable-ID registries; once AIRA (Phase 1) can track its own backlog, AIRA dogfoods itself.
- **Traceability *convention* only** — `covers:` in Go doc comments, `verifies:` in tests. **The enforcing fail-closed graph check is built in Phase 3, not Phase 0** (it would pass vacuously with no graph to check). Records the `make id` → `aira id` migration intent.

AIRA dogfooding AIRA — tracking its own development, running its own tests through its own runner, ratcheting its own suite — is the best living proof the interface is good.
