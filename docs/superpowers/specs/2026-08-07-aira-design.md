# AIRA — design spec (top-level scope + architecture)

- **Status:** Built. This is the original design map (2026-08-07); Phases 0–5 (§20) are all implemented and merged, plus a long tail of later hardening milestones (confinement, install, watchdog, daemon, project lifecycle, memory accounting). The per-subsystem specs in this directory and the live `.aira/` tracker hold the as-built detail; where a later spec supersedes a decision here, the later spec wins.
- **Date:** 2026-08-07 (v4 — folds a three-lineage adversarial review: Gemini + Codex/GPT-5.6-Sol + Fable. The mechanism fixes below are load-bearing; the architecture, honesty discipline, and phasing survived the review intact).
- **Scope:** whole-product scope, architecture, data model, phasing. Each subsystem gets its own spec → plan → build cycle. This is the map; "resolved in the Phase-N spec" is deliberate.
- **Companion:** [Phase 0 process-docs plan](#phase-0--bootstrap-process-docs-the-non-tool-deliverable).

---

## 1. What AIRA is, in one paragraph

**AIRA is "JIRA for AI agents": a machine-local (later optionally remote) work-item tracker, coordination layer, findings store, traceability assistant, metrics surface, and execution/verification substrate for AI coding agents working across one or more repos.** It is a single static Go binary, `aira`, exposed identically as a CLI, an MCP server, a Skill, an optional daemon, and a human TUI — thin adapters over one core. It kills three *measured* pains — colliding ID allocation, review findings that evaporate, and "too many cooks" churn across worktrees — and absorbs the surrounding execution toil (running commands, capturing output, tracking tests) agents pay by hand today.

## 2. The problem (evidence, not assertion)

Observed in `an earlier project` (~194 live worktrees) and confirmed by a docs + 4,000-commit-history sweep:

1. **ID allocation is out-of-repo, bespoke machinery** — `make id`: an `flock`'d common-dir counter (self-healing cache over the markdown), a fail-closed duplicate gate, and a **common-dir receipt log** that catches IDs *burned in a rebase* (IDs committed then dropped; another collided across clones). Python/bash bolted to one repo's two tables.
2. **Findings evaporate** — 92 hand-written `docs/reviews/`+`docs/audits/` files exist only by manual transcription; the two-loop review found *seven real bugs in code that passed its own seventeen tests*, durable only because someone wrote them down; the same defect classes resurface loop after loop, unqueryable.
3. **Coordination is manual prose** — no "who owns what" DB; owner sentences, files hand-queued "for a non-parallel session," `base-red owned by other sessions` triaged from memory at merge.

Sharper, less-obvious pains AIRA can *prevent*: half-building behind an un-landed prerequisite (throwaway work — the most expensive failure), gates that read green because the check *silently never ran* (semgrep skipped a 1.77 MB module forever; a blank pipeline leg reported green), stored-numeral baselines that merge-conflict and need constant re-baselining, and heavy subprocesses that OOM-kill the desktop or get killed by an agent stopping the *shared* RAM slice.

## 3. Goals and non-goals

**AIRA is:** the authority for IDs, status, relations, leases, findings, requirements, gates, compute, timing, test outcomes, and runs — across worktrees and (later) machines; agents-first, humans occasionally; a **recorder and orchestrator**, not a judge (it tracks state, allocates IDs, holds leases, stores typed findings, owns the covers/verifies graph, runs and records subprocesses, *emits* review/verification prompts, and *records* verdicts — it never calls a model).

**AIRA is not:** an agent/session *observability* runner — that is `agentmux`'s domain, and AIRA depends on it for **nothing** (§18); a model gateway; or a CI system (it records verdicts + runs its own cheap deterministic checks; heavy/judged work is emitted as a prompt and its result recorded). It complements git and PRs; the durable content record lives in git.

**Two senses of "block":** *integrity* checks (ID uniqueness, a relation resolving to a real ticket) are **fail-closed and refuse the invalid operation** (§4.5). *Lifecycle gates and traceability edges* are **advisory** in Phases 1–3 — a valid-but-out-of-policy transition, or a covers/verifies edge dangling mid-refactor, warns and reads `unevaluated`, never blocks (§9, §15). One refuses the impossible; the other declines to police the merely-unwise.

## 4. Design principles

From `kichad`, adapted:

1. **Primitives, not judgement.** Build only what can be *checked*; supply the primitive and *emit a prompt* for what can only be judged; record the verdict. Composition is a feature; output size is a cost.
2. **One core, thin faces.** All behaviour behind `core.Do(ctx, req) → resp`; CLI/MCP/Skill compile to the same verbs and cannot diverge.
3. **Generate every doc surface from the dispatch tables** — MCP schemas, help, agent guide, and the transition/gate guidance text (§9).
4. **Verdict-first, three verdicts never two** — `pass | fail | unevaluated`; what AIRA can't stand behind reads `unevaluated`, counted apart from failures. Every uncomputable metric reads `unevaluated`, never a fake zero.
5. **Refuse rather than guess** — an ambiguous selector is rejected (fail-closed integrity).
6. **Silence is signal; errors are stable codes.**
7. **Small tool surface + a full escape hatch** (~11 MCP tools + `aira_exec`).
8. **Layer strictly downward; the in-binary adapter layer holds no domain logic.** Out-of-core *ingesters* (compute/quota/test-report capture) are separate small programs, not this layer.
9. **Self-verifying, recoverable edits.** No write leaves an **unresolvable ticket/relation graph** — a relation to a non-existent ticket is refused (integrity). *Traceability* covers/verifies edges are advisory and may dangle transiently (§15). The reconciler is the safety net.

## 5. Architecture

### 5.1 One core, thin faces; strict downward layering

```
internal/store/    git files (content) + common-dir audit log + SQLite (telemetry+coordination+index) + reconciler
internal/domain/   tickets, relations, leases+hints, findings, requirements, gates, attestations,
                   compute/quota/test-report/run records, projects, milestones, event log
internal/query/    selectors, cursors, distributions, FTS
internal/interp/   the command language
internal/runner/   subprocess launch/capture/kill + the per-run supervisor shim (§14)
cmd/aira/          thin adapters:  cli · mcp · tui · daemon · install
```

**Embedded store:** leaning SQLite (WAL, FTS5) via `modernc.org/sqlite` (no cgo); the driver gates ID-allocation atomicity and lease concurrency, decided *first* in the Phase-1 spec.

### 5.2 Daemon-optional runtime

**Core correctness never requires a running service** — fresh worktrees and sub-agents work daemonless (open SQLite/WAL, reconcile on demand, evaluate lease liveness lazily). The optional `aira daemon` adds a load-once session, `aira watch`, a continuous reconciler, and a heartbeat reaper. **The one feature that needs a supervisor process is a *detached* subprocess run** (§14) — served by a tiny per-run supervisor *shim* (works daemonless) or the daemon; foreground runs never need it. "Daemon-optional" is a statement about *core* correctness, not about every optional feature.

### 5.3 Storage: three homes, four durability classes, one reconciler

Git is the **sole authority for ticket content** — one file per ticket (`.aira/tickets/AIRA-42.md`), so agents on *different* tickets never touch the same file; findings/requirements in `.aira/findings/`, `.aira/requirements/`. Human indices (`BACKLOG.md`, `ROADMAP.md`) are **rendered on demand**, never committed (committed generated files merge-conflict by construction). The DB holds **no authoritative content** — only a rebuildable index + coordination + telemetry — so there is no content split-brain.

Everything else falls in **four durability classes**, in three physical homes:

| Class | What | Home / durability |
|---|---|---|
| **Content** | tickets, findings, requirements, milestones, project config | **committed git** — survives machine loss |
| **Audit / receipts** | the mutation event log (§11), allocation receipts, **gate attestations** (manual attests, proven-to-fire evidence, waiver acceptances) | **git *common-dir*** (`$(git rev-parse --git-common-dir)/aira/…`), append-only — machine-shared across worktrees, **outside the commit graph** (so a rebase that drops a ticket commit cannot eat the receipt), and it never dirties a worktree. Machine-local (like the reflog); the *content* is the cross-machine record |
| **Operational telemetry** | compute-events, quota-snapshots, test-reports, run metadata | **DB-only, retention-capped** — high-volume, best-effort; loss on a rare rebuild is tolerable, metrics rebuild forward. **Exception:** a test-report/derived-baseline pinned as an active **ratchet baseline** is retention-exempt and promoted to the audit class (§9/§13) — a gate must never rest on evictable data |
| **Ephemeral coordination** | leases, heartbeats, worktree bindings, ID-counter cache | **DB-only, self-heals** — dead leases lapse, worktrees re-register, the counter rebuilds from a full multi-worktree scan (§7) |

Run **output blobs** (§14): machine-local gitignored files, capped + zstd-compressed + evicted — never git, never SQLite blobs.

**Write protocol + crash recovery (a Phase-1-spec deliverable, §21).** A single mutation touches SQLite, the common-dir journal, and a git file — with no shared transaction. The rule: **SQLite is the transactional authority for the mutation and the `seq`**; the git file and journal are derived and reconciled; a crash between writes is repaired by the reconciler (which store is authoritative and what it replays is specified there). **The reconciler** (an earlier project's receipt/orphan check, generalised): every allocated ID resolves to a git ticket file and vice-versa (a burned ID → a reconciliation record); a lease on a vanished worktree, a duplicate ID definition, a submodule-pointer drift → records; a stale index → a silent rebuild. Reconciliation records are DB-resident coordination artifacts (stable-coded), distinct from git-durable review findings; both share the Finding schema.

### 5.4 Remote-ready seam (design now, build local)

`core.Do(req) → resp` speaks serializable JSON (same protocol in-process / Unix socket / TCP+auth, byte-identical interface, no local-store assumptions); `Store` is an interface (local SQLite / remote RPC). `seq` is **per-project** in Phase 1, keyed as `(project_id, seq)`; cross-project ordering is deliberately not promised. Across machines, project streams may interleave. Reconciliation is in-process in Phase 1; a client/server reconcile protocol is an open question (§21), not a v1 commitment.

## 6. Data model

Closed, small enums (overloaded string fields caused real bugs in an earlier project). Durability class per §5.3.

- **Project** — slug, repo path(s), owned ID prefixes, **gate policy** (incl. the path→{suite, review-tier} map, §16), milestones, config. *Content (git).*
- **Ticket** — `PREFIX-N`; title; body; **status** (`draft → planned → in-progress → in-review → done`, + `retired`, `superseded`); kind; severity (`P0|P1|P2`); assignee/agent/session/worktree-id; milestone; labels; timestamps (event-log-authoritative). **Blockedness is *derived* from unmet `blocked-by` edges (plus an explicit hold flag), not a hand-set `blocked` status** — one source of truth, no status-vs-edge disagreement.
- **Relation** — bidirectional, typed, stored once, presented both ways: `blocks`/`blocked-by`, `parent`/`child`, `relates`, `duplicates`/`duplicated-by`, `supersedes`/`superseded-by`, **`resolves`/`resolved-by`** (a "centralise X" finding → the guard/ticket that closes it; unmitigated → visible debt). Ready-queue keys on `blocked-by` (§9). *Canonical storage side = the lower id, inverse derived (§21).*
- **Lease** — ticket-id, **holder token** (unforgeable — so B cannot release A's lease), acquired-at, last-heartbeat. Liveness = `now < last_heartbeat + TTL` on a **monotonic/DB clock** (not wall-clock, which jumps). Claim/heartbeat/steal are **atomic DB CAS** ops. Plus **area hints** (globs; overlap → warning, never a block, §8).
- **Finding** — id; subtype (`review` git-durable | `reconciliation` DB-resident); ticket-id (+ optional requirement-id, `file:line`); category (kebab, incl. `flaky-test`); severity; verdict (`confirmed|refuted|plausible`); **source** — an **open registered vocabulary** (`codex|fable|gemini|deepseek|semgrep|human|aira|…`, extensible without a spec edit); disposition (`open|fixed|waived`). *An accepted-debt gap is a `waived` finding whose waiver carries an acceptance **attestation** — "accepted" is not a separate disposition.*
- **Requirement** — own prefix; text; status (`built|partial|designed|planned|boundary|retired|superseded`); typed `covers`(→code)/`verifies`(→test) edges.
- **Gate** — name; kind (`checkable` | `manual` | `ratchet`); the step it guards; **`proven-to-fire`** = a *dated, evidence-linked* attestation (§9), not a bare boolean. Un-attested manual → `unevaluated`; attested → `pass`.
- **Attestation** — `{gate|waiver|proven-to-fire, subject, evaluator, lane, config-digest, commit, at, evidence-ref}`. *Audit class (durable).*
- **ComputeEvent / QuotaSnapshot** — §12. *Operational telemetry.*
- **TestReport** (+ per-test results) — `{id, ticket?, phase?, commit, branch, worktree-id, agent/session, at, run-ref?, suite-id, runner/config/env-digest, shard, retry-index, parser-complete(bool), coverage?, results:[{name, outcome(pass|fail|skip|error), duration, message?}]}`. The identity fields (suite/config/shard/retry/parser-complete) are what make flakiness and ratchet comparisons *valid* — same-test-same-commit differing under different config is concurrency-sensitivity, not flakiness; a passed *retry* is not a first-pass. *Operational telemetry (a pinned ratchet baseline is promoted to audit, §5.3).*
- **Run** — `{id, ticket?, phase?, label, argv, prefix, cwd, env-digest, tool?, merge_streams, buffering(none|realtime|pty), status(running|exited|killed|oom-killed|lost), exit, signal, started/ended, cpu_user, cpu_sys, peak_rss?, output-refs{out,err|log[, in]}}`. `killed` = via `aira run-kill`; `oom-killed` = cgroup `memory.events oom_kill` fired; `lost` = supervisor/AIRA died without an exit record. *Metadata: telemetry (DB); blobs: local+capped.*
- **Event** — the mutation log: `{project-id, seq, at, actor, verb, target, payload-digest}`. `seq` (per-project, monotonic, **append-only and gap-detectable** — not "tamper-resistant": daemonless the caller writes it) is the **ordering authority** within that project's journal; `at` (wall clock) is advisory. The event key is `(project_id, seq)`, and on DB loss each project's `seq` resumes above the max found by scanning its common-dir journal (§7 discipline).

## 7. ID allocation

Report-time; three layers. **(1) The machine-wide DB counter is the atomic cross-worktree authority** — `new = counter+1` in one exclusive txn *that also writes the receipt* (counter++ and receipt are one transaction; the ticket file is written after, and a crash between them leaves a receipt-without-file the reconciler resolves — offer to create, or retire). It must be the DB, not a git scan: a sibling worktree's just-*committed* `AIRA-50` isn't visible in this checkout, and a just-*written-uncommitted* one isn't visible to any object/ref scan at all. **Counter rebuild (on DB loss) enumerates every worktree from the common-dir and scans each working tree's `.aira/tickets/` + the common-dir journals + all refs** — *not* a single `max(counter, git_max)`, which would miss an uncommitted sibling-worktree ID and re-mint it (the cross-clone duplicate class). **(2) A fail-closed duplicate-definition check** (one shared parser). **(3) Receipts live in the common-dir** (audit class, §5.3) — out of the commit graph, so the rebase that drops a ticket commit can't drop the receipt that detects the loss. `aira id <prefix>` is the primitive; an allocated ID resolves to a live ticket or an explicit `retired`/`superseded` one — never nothing. *(Cross-clone/remote allocation stays an open question, §21 — as an earlier project's own design concedes.)*

## 8. Coordination — the "too many cooks" fix

**Hard ticket lease** — `aira claim` (records an unforgeable holder token) → the *caller* periodically `aira heartbeat` → lazy monotonic-clock liveness; `--steal` a dead lease is an **atomic CAS** (heartbeat-vs-steal can't both win), logged. Daemonless, heartbeat *emission* is the caller's job; only the daemon reaps. **Advisory area hints** — `aira touch <globs>`; overlap with another live claim → a **stable-coded warning, never a block** (the mechanical form of "a peer session owns `runner.py`"). The stable code lets a project's harness treat overlap as blocking *locally*; **AIRA itself is advisory-only — hard area-leases are a decided non-goal** (a hard file-lock across autonomous agents tends to deadlock and strand stale locks worse than the churn). **Worktree identity** = the stable git gitdir id. `aira list --by assignee`/`--by status` makes who-is-on-what and base-red a query.

## 9. Lifecycle and gates (V-model, advisory-first) — prevention over recording

One phase vocabulary, shared with compute attribution (§12): **`plan · plan-review · plan-fix · implement · work-review · work-fix`**, then merge; the two review *gates* sit at the plan gate and the build gate. **Attribution labels + advisory gates, not a mandate** — an agent may skip phases; trivial changes take an explicit lighter path (Phase-0 docs), so a one-line fix is never a six-phase ritual.

- **The `blocked-by` ready-queue is the highest-value early feature — it *prevents* rework rather than recording it.** `aira ready <id>` excludes items with unmet prerequisites, surfacing a dependency *before* effort is spent. **Phase 1 leads with this** (§20).
- Each gate is **checkable | manual | ratchet**; `aira ready <id>` folds them (*"not ready — 2 failed, 1 unevaluated, 3 passed"*).
- **A green gate must have actually *fired*, and the proof must not rot.** A checkable gate is `pass` only with a **dated, evidence-linked `proven-to-fire` attestation** (it *fired on a known-bad input in the lane that runs it*, linked to the run/report that fired) that **decays to `unevaluated` after a max-age** — because a lane proven-to-fire in January can, in June, still *be capable* of firing while an added `--exclude` skips the artifact under test (proven-to-fire is *lane capability, not input coverage*). Prefer a **continuous canary** (a known-bad sentinel run in-lane every time, its failure inverted) over one-time attestation. A check that could not run — a scanner that skipped the file, a leg that collected zero tests — reads `unevaluated`, never green. This is the direct fix for the "green guard that silently never ran" class.
- **Ratchet gates compute over a *durable, pinned* baseline, never evictable telemetry.** A `ratchet` gate ("no new failures / no coverage drop vs baseline") derives from the **pinned, provenanced, run-linked derived baseline** (a small failing-test-set + counts, promoted to the audit class, §5.3) — *not* the retention-capped archive, whose eviction would silently drop the gate's floor. A missing baseline reads `unevaluated` (never a vacuous pass); a known-flaky test (red-at-baseline / green-now / red-again, per §13) is not counted as a new failure. A coverage ratchet requires the coverage field in the TestReport schema (§6) — present, or the example is cut.
- **`tests-green` from a run's exit code requires exit==0 *and* a parsed §13 report with test-count > 0** (ideally not below baseline) — a bare exit 0 (`go test ./...` with zero test files) attests nothing; the exit code alone would forge exactly the gate this section exists to prevent.
- **Advisory first:** structural validation still refuses an illegal transition (integrity); policy gates only warn; hardening to blocking is per-project with a justified-exception allowlist. **Accepted coverage-gap debt** = a `waived` Finding + an acceptance **attestation** (who, why — audit-durable), one query, not PR prose.
- **Refusals and warnings teach** — a refused/warned transition returns actionable, agent-readable guidance generated from the transition/gate table (§4.3).

## 10. Findings and the learning loop

Typed findings make review output survive by construction. **Cross-ticket aggregation** (`aira find ls --by category`) answers "which mistake classes recur" — the trigger to derive a lint. **Reviewer verdict ratios** (`confirmed`-vs-`refuted` per source) are honest aggregates of *recorded* labels with provenance, surfacing reviewer-disjointness and kill-rate (the real quality signal). Disposition is on the record (`waived` = visible debt + rationale). A **`resolves` edge** keeps an unmitigated "centralise X" finding visible instead of relying on memory. **`aira import`** (§16) seeds the store from existing artifacts so the corpus starts populated.

## 11. Timestamps and the event log

**Every significant state-changing call is journaled** (append-only, common-dir, §5.3), stamped at ingress. Honest framing: **daemonless, AIRA runs in the caller's own process**, so the timestamp reads the caller's clock — there is no distinct clock authority until the daemon/remote phase; the value is one capture point + a monotonic `seq`. **`seq` (per-project, monotonic, gap-detectable) is the ordering authority within that project's journal; the event key is `(project_id, seq)`; `at` (wall clock) is advisory.** **The journal records *significant* mutations** (allocations, status transitions, claims/releases/steals/lapses, finding/link/relation writes, **attestations**) — the daemon records `lease.lapse` authoritatively in DB first and reconciliation materialises it in the journal — **not** high-frequency heartbeats (ephemeral DB refresh) nor per-run/per-`spend` telemetry detail (DB-only), which would swamp it; read verbs are timestamped cheaply but not journaled. On DB loss, each project's `seq` and the counter resume above the max found by scanning its journals.

## 12. Compute telemetry (phase- and model-attributed)

Per-ticket LLM compute, by the §9 phase vocabulary. *Operational telemetry (DB-only, retention-capped).* Phase-4 subsystem; the map fixes the data shape + one hard constraint. Superset schema, disjoint buckets (`fresh_input · cache_read · cache_write · output · reasoning`); provider-absent → `unevaluated`, never zero. **Hard constraint:** "cached" is a *subset* of input on OpenAI/Gemini/Codex, *separate* on Anthropic — the ingester normalises to disjoint buckets and, on a mismatch vs the reported total, **records the datum anyway and raises a reconciliation finding** (a conservation *warning*, never a fail-closed drop of drifting vendor telemetry). Capture is out-of-core (Claude Code → `Stop`/`SubagentStop` `usage` or OTEL, never the transcript; Codex → `codex exec --json`; direct API → the response `usage`; **Antigravity uncovered** — no dependable per-task export, manual reports accepted). Makes review-loop economics and estimate-vs-actual real (§17). **`aira quota`** records opt-in provider-supplied `QuotaSnapshot`s (a burn-rate gauge); AIRA records what an ingester hands it, never scrapes.

## 13. Test reports

A standard-format archive so test outcomes are durable, trended, checkable. *Operational telemetry (a pinned ratchet baseline → audit class, §5.3).* Ingestion is out-of-core: `aira test-report add --format <junit|go-json|tap|pytest-json|…> < report`, normalising common CI formats to one schema — or **auto-ingested from a run** (`aira run --report junit`, §14). The schema carries **suite/config/shard/retry/parser-complete/coverage/run-provenance** (§6), without which flakiness and ratchet comparisons are invalid.

Everything it enables is a **checkable primitive, not a judgement:** **flaky tests** (same test, same commit, *same config*, both pass and fail across reports → a `flaky-test` finding + a §17 gauge); **long-lived master breakage** ("red on `master` since Y" — a duration query); **ratchets** (the §9 `ratchet` gate, over the *pinned durable* baseline).

## 14. Subprocess runner

Agents run heavy commands via a `whale-run` prefix (a systemd slice with a hard RAM cap). Three dated pains motivate AIRA owning the *run*: (a) a **53 GB run killed the whole desktop, 2026-05-29** → the cap; (b) the **safe and catastrophic kill commands differ by one token, adjacent in the docs** (`stop whale-run-<name>.scope` vs `stop whale.slice`), now a HARD rule agents must remember; (c) agents **re-run a 4,260-test suite** to re-inspect mis-`grep`'d output and hand-roll capture (`codex -o out.md`; *"record the exit code, never infer green from a truncated log"*). AIRA is the **outer** wrapper (lifecycle/capture/handle/kill); the slice stays the **inner** wrapper (memory containment); the launch prefix is a configurable string (keeps agentmux independent, §18).

- **Launch.** `aira run [--ticket <id>] [--phase <p>] [--label <l>] [--tool <t>] [--report <fmt>] [--follow|--detach] [--merge] [--realtime|--pty] [--stdin <file|->|--no-stdin] -- <argv>` → a run handle, via a configurable **`run.prefix`** (`.aira/config`, default `agentmux whale run --`, empty = bare). Records launch as a §11 event; on exit appends exit/signal/wall/cpu/peak-RSS. Pure recorder.
- **AIRA owns the kill scope — it does not trust the prefix to name it.** After launch it reads the child's cgroup from `/proc/<pid>/cgroup` (and, for bare runs where available, launches the child in a cgroup/scope AIRA creates, e.g. `systemd-run --user --scope`). So the prefix stays genuinely opaque, and kill/rusage don't depend on a naming convention.
- **Scoped, safe kill — the headline safety win.** `aira run-kill <handle>` **cgroup-kills** the run's own scope (TERM → grace → KILL) — reliable against `setsid`/double-forking descendants — making the catastrophic `stop whale.slice` **unreachable through AIRA**. A *bare* run with no cgroup available falls back to `kill -- -<pgid>` **after verifying leader identity** (pid + `/proc/<pid>` start-time, to survive PID reuse), and the spec states plainly that a setsid-escaping child then leaks — so the default (cgroup) path is the supported one.
- **I/O model — in/out/err preserved *and* live.** Each stream is tee'd (process ↔ caller-live ↔ file): **stdout**/**stderr** stream live (and `run-log --follow`) and are captured **separately** to `RUN-n.out`/`RUN-n.err`; **opt-in `--merge`** = a real `dup2(stderr→stdout)` at exec (faithful kernel ordering) into one `RUN-n.log`, for temporal consistency (separate pipes can't guarantee cross-stream order). **stdin** is streamed into the process while open; **`store-stdin` is off by default** (secrecy posture), so `in` is not in the default output-refs — captured only with `--stdin` capture explicitly on. The runner **always drains pipes to the capture files** regardless of a live reader (no backpressure stall); on **disk-full it fails the run with a stable code**, never silently loses; binary-safe.
- **Prompt output — `--realtime` / `--pty`.** `--realtime` **replicates `stdbuf` via the child's env** (`LD_PRELOAD=libstdbuf.so` + `_STDBUF_O`/`_STDBUF_E`, + `PYTHONUNBUFFERED`) rather than splicing a binary into the argv — so it doesn't perturb the argv and **keeps out/err separate**; it works for glibc-stdio tools and is a **no-op** elsewhere (Go already unbuffered; Rust/Python/static/setuid — or when `libstdbuf.so` is absent). `--pty` allocates a pty (universal line-buffering) but **merges** out+err and injects TTY escapes (so `--pty` implies `merge_streams=true` — the Run row can't claim pty+separate). AIRA records which tactic ran; **the file capture is complete *up to a configurable cap* regardless** (on overflow: head+tail with the middle elided and marked — not "always complete"; the flags affect only how promptly a live `--follow` sees output).
- **Re-filter without re-running.** `aira run-log <handle> [--stream out|err|merged] [--tail N] [--head N] [--grep RE] [--since <line/byte>] [--full] [--follow]` re-queries the *same stored bytes* — a wrong first filter costs one cheap re-read, never a re-run. §19's overflow discipline applies. Replaces `-o out.md`.
- **Storage + eviction.** Metadata → DB (telemetry); blobs → machine-local gitignored files, per-run byte caps, **zstd when warm, evicted oldest-first under a max-disk cap** (+ optional TTL). **Eviction/compression never touches a blob with a live reader or a still-running run.** After eviction, metadata + any extracted report survive. `store-env` off by default; **secret redaction is best-effort** (a token split across `write()` chunks can slip — so captured logs are *not* certified sanitised); raw logs never journaled to git.
- **Detach needs a real supervisor — a systemd scope is not one.** A scope is a cgroup wrapper; it does **not** hold stdio, `waitpid`, or record the main pid's exit. So a naive daemonless `--detach` (parent exits holding the tee pipes) would SIGPIPE/hang the child and lose its exit code — every detached run would end `lost`, breaking the `tests-green`-from-exit-code wiring. **Detached runs therefore: (i) `dup2` the child's stdout/stderr directly onto the capture files at exec (no live tee; `--follow` = tail the file); and (ii) are owned by a tiny per-run supervisor *shim* (`aira` re-execing itself), placed *outside* the kill scope, that holds the fds, `waitpid`s, reads the cgroup stats *at exit* (the cgroup is removed when it empties), and writes the exit record** — this works daemonless; the daemon is the alternative. **The shim-vs-daemon choice is decided first in the Phase-5 spec (§21); everything else in the runner hangs off it.**
- **Resource accounting from the cgroup, at exit.** peak-RSS/CPU from the scope's cgroup (`memory.peak` — kernel ≥ 5.19; `cpu.stat`; `memory.events oom_kill` to mark `oom-killed`), read by the supervisor *at child-exit*. `getrusage(RUSAGE_CHILDREN)` misses grandchildren and is not used; a bare run with no cgroup reads `peak_rss` = `unevaluated` (never a fake number). This feeds estimate-vs-actual (§17) and data-driven `MemoryMax` tuning.
- **Wiring.** A run `--phase work-review --tool codex` auto-emits a ComputeEvent capturing the run's resource usage (wall/cpu/peak-RSS; `unevaluated` where the cgroup could not be read); authoritative token usage is recorded when the caller supplies it (`--usage <file> --provider <p>`, parsed by the §12 normaliser), absent → token buckets `unevaluated` (auto-extraction of usage from the tool's own stdout is a later cut). A `--report`ing run creates a §13 report from the captured output and records a **`tests-green` run-observation** (exit==0 **and** parsed test-count>0, §9 line "tests-green from a run's exit code") — this is an honest *observation of one passing run*, **not** a gate verdict: a green **checkable gate** still requires the §9 *proven-to-fire* attestation from the gate's own lane + canary (a single passing run has not fired the lane on a known-bad input), so M19 feeds gates the report/ratchet-baseline input and never writes a gate-audit verdict itself. *[Reconciled with §9 line 118 during M19 planning — the earlier "attests tests-green" wording is superseded by "records a tests-green observation"; the gate verdict stays with the command-gate.]*
- **Deferred to post-shim (not v1): `aira run-input` (live stdin push).** It requires a per-run control plane (a socket/FIFO on the shim) and no dated pain motivates it — so it lands once the shim exists, not in the first runner. (Foreground runs still take `--stdin` at launch.)
- **Execution fidelity.** Faithful cwd + env (the earlier project's tests were cwd-sensitive); argv-parse care (a single leading `--`); interactive approval prompts stay out of scope.

**Phasing (§20):** a **runner-lite** — launch + file-direct capture + scoped cgroup-kill + `run-log`, *no detach / no run-input / no telemetry wiring* — depends on nothing beyond Phase-1 tickets/leases and **banks the headline safety win early** (Phase 3), instead of leaving the live footgun until the full runner (Phase 5). This grows AIRA into an execution/verification substrate — a conscious scope step the recorder framing survives, and one all three reviewers endorsed keeping (with sharper mechanism, not cutting).

## 15. Discovery, indexing, compliance

Over requirement / backlog / implementation / verification: **discovery** = graph queries for what's *missing*; **indexing** = FTS + the covers/verifies graph (`aira grep`); **compliance** = graph checks both directions, run at **`aira check`/CI, not per-edit**, so a mid-refactor dangling covers/verifies edge reads `unevaluated` and is *surfaced*, not a per-keystroke refusal — whether it *blocks a merge* follows the advisory-first→hardening path. (This is the *traceability* graph; the *ticket/relation* graph is fail-closed integrity, §4.9.) "Does this test *faithfully* verify?" is judged → marked manual, prompt emitted, never faked.

## 16. Review emission, routing, and import

`aira review <id>` assembles a context-loaded prompt/command bundle for the agent to run — Codex-first, Fable-final, Gemini for user-facing/visual, Deepseek later; the convention is hardcoded to start (a per-project routing DSL is deferred, §21). AIRA emits a **review-depth recommendation** from ticket area/kind/severity (the analogue of `review_tier.py` Tier 0–3, MAX-over-paths, fail-closed default-up), tuned on recorded review-loop economics (§12); the path→review-tier map lives in the project gate policy (§6). The agent reports the verdict via `aira find add … --source codex`. **`aira import`** seeds the stores from existing markdown backlog/requirements and `docs/reviews/` docs (→ findings), so AIRA answers "which classes recur" over history on day one.

## 17. Insights, metrics, reporting (honest, drillable)

The useful JIRA subset, recast for agents — the *drill-down* is the point. **Kept:** WIP/concurrency + area-hint-overlap; per-stage dwell; collision/churn; recurring-mistake trend; reviewer verdict ratios; review-loop economics; traceability decay (the gauge that goes bad *silently*); estimate-vs-actual compute; flaky-rate, master-red-duration, ratchet status; run/build wall-clock, slowest-command, peak-RSS per phase; quota-burn-rate. **Dropped as ceremony:** story-point velocity, burndown theatre, leaderboards, priority pies. **Discipline:** every gauge is a **live query, never a stored numeral**; carries universe + as-of; uncomputable reads `unevaluated`; shows direction vs a baseline; is itself a drillable saved query — an earlier project's own "write the query, not the numeral" law applied to the process layer.

## 18. Boundary with agentmux (fully independent)

`agentmux` = process/session observability. AIRA = work-item + run level. **AIRA depends on agentmux for nothing** — it runs its own lease liveness, and the runner's launch prefix is a configurable string (default happens to be `agentmux whale run`; empty = bare; AIRA owns the kill scope regardless, §14). agentmux is a design-pattern reference and a source of learnings only.

## 19. Command / tool surface (small + escape hatch)

CLI: `init · id <prefix> · new/create · ls/list [<q>] [--by F] · count [q] --by F · show/get · set · mv <status> · claim/release/heartbeat · touch · link · find add|ls · req … · ready <id> · review <id> · import … · run … · run-kill · run-log · test-report add|ls · ratchet … · spend · quota … · grep · backlog · roadmap · stats · check · watch · exec "<cmds>" · mcp · tui · install · daemon`. (`run-input` lands post-shim, §14.) **`aira init`** is a first-class new-project scaffolder. **Token-efficient output:** MCP-only 50-row cap → distribution-over-one-field on overflow (never a silent truncation; the same discipline governs `run-log`); `--fields` opt-in; `count --by` size-before-fetch. **MCP tools — ~10 core + `aira_exec`, plus 3 runner tools that arrive with the runner:** core = `aira_create · aira_list · aira_get · aira_transition · aira_claim · aira_link · aira_finding · aira_ready · aira_review · aira_reconcile`; runner (Phase 3 lite / Phase 5) = `aira_run · aira_run_output · aira_run_kill`. Descriptions generated from the tables (§4.3).

**As built (reconciled 2026-08-29).** `create`/`list`/`show` are the canonical spellings and `new`/`ls`/`get` are kept as aliases. The human indices `backlog`/`roadmap`/`stats` were **not** shipped as their own verbs — that role folded into `list`, `ready`, and `insights` (a live-query gauge surface, never a stored numeral, §17). `ratchet` became a **kind** of `gate` rather than a top-level verb. Later phases added a large surface this early list predates: `confine` (+ `--list`/`--kill`/`confine-reserve`), `install`, `eject`, `rant`, `time`, `git`, `run-input`, `gate`, `insights`, `reconcile`, and the `req`/`find` CRUD trees. `aira` with no verb prints the current authoritative dispatch list.

## 20. Phasing

- **Phase 0 — Bootstrap process docs** (below).
- **Phase 1 — Coordination MVP, led by prevention:** git-file store + rebuildable index + reconciler + common-dir event log/receipts, the 3-layer ID allocator (with multi-worktree rebuild + crash-safe order), tickets/status/relations, atomic leases + area hints, **the `blocked-by` ready-queue + `aira ready`** (lead with it), a minimal finding/reconciliation record, CLI + query, `aira check`, on-demand `aira backlog`, a first `aira stats`. *(Close the §21 Phase-1 items — incl. the write-ordering/crash-recovery protocol — before coding.)*
- **Phase 2 — Findings + surfaces + import:** typed findings + query, **`aira import`**, FTS/`aira grep`, MCP server + Skill.
- **Phase 3 — Traceability + gates + runner-lite:** requirements, `covers`/`verifies`, the fail-closed traceability graph check, checkable/manual/**ratchet** gates + **dated proven-to-fire** + canaries, `ready`, `review` + review-depth, discovery/compliance — **and runner-lite** (launch + file-capture + scoped cgroup-kill + `run-log`) to bank the safety win early.
- **Phase 4 — Insights + telemetry + test reports:** honest gauges, compute-event ingestion + ingesters + quota, the test-report archive + flaky/ratchet gauges, milestones/roadmap, TUI, daemon niceties, remote-transport hardening.
- **Phase 5 — Full subprocess runner:** detach (decide **shim-vs-daemon first**), tee'd live I/O, `--merge`/`--realtime`/`--pty`, cgroup rusage-at-exit, telemetry/gate auto-wiring, and (post-shim) `run-input`.

## 21. Open questions (resolved in per-subsystem specs)

**Phase-1-spec deliverables (close before coding):** the **DB↔git-file↔journal write-ordering + crash-recovery protocol** (which store is authoritative; what the reconciler replays on a crash between writes) — as foundational as ID atomicity; ID-allocation atomicity (`BEGIN IMMEDIATE`/busy-timeout on the driver under concurrent processes) + the multi-worktree counter-rebuild scan; lease atomicity/holder-identity/monotonic-clock; the selector/anchor grammar; the git-file frontmatter + `.aira/config` schema + relation storage side (lower id, derive inverse); git commit semantics (recommend: AIRA writes the working tree, the agent commits; the receipt observes commit state); project/worktree discovery + prefix-ownership uniqueness; **DB placement** (one machine-wide DB — implied by the machine-wide counter — vs per-project); lease TTL / heartbeat defaults; the stable-code catalog + `aira check` exit codes; reconcile trigger/scope.

**Later / genuinely undecided:** requirements as one-file-per vs a single registry; the client/server reconcile protocol (only with a remote consumer); a per-project review-routing DSL (default: no); cost derivation (price table vs harness `cost_usd`); deferring `$register` saved-sets. **Runner (§14/Phase-5):** the **detach shim-vs-daemon** decision (decide first); blob-retention defaults + compression thresholds; cgroup-stats read path across cgroup-v2 layouts + the pre-5.19 `memory.peak` fallback; how far to normalise test-report formats. Compute/telemetry retention caps.

---

## Phase 0 — bootstrap process docs (the non-tool deliverable)

Before any AIRA feature work, lay down the V-model process **in this repo**, so agents follow it from commit #1 — adapted from `an earlier project` for **Go**:

- **`CLAUDE.md`** — the loop, worktrees-never-root, git-stash-named-refs, `whale-run` for heavy commands (and *target-your-own-scope, never the shared slice*), review-as-durable-artifact, pointers below.
- **`docs/dev/agentic-development-loop.md`** — the lifecycle in the §9 phase vocabulary (`plan → plan-review (Codex+Gemini) → plan gate (Fable) → plan-fix → implement (TDD) → work-review (Codex, then Fable two-loop) → work-fix → build gate → PR → Merge`), a per-step Who/Gate table, and an explicit **lighter path for trivial changes**.
- **`docs/review-and-merge-policy.md`** — Codex-first + Fable-final, owner-delegated; coverage gate; Gemini for user-facing text. *(This spec was itself reviewed this way: Gemini + Codex/GPT-5.6-Sol + Fable.)*
- **`docs/adversarial-verification.md`** — the two-loop red-team; "passing your own tests is not evidence of correctness."
- **`REQUIREMENTS.md` (seed) + `docs/backlog.md` (seed)** — stable-ID registries; once Phase 1 lands, AIRA dogfoods itself.
- **Traceability *convention* only** — `covers:` in Go doc comments, `verifies:` in tests. **The enforcing fail-closed graph check is built in Phase 3, not Phase 0** (it would pass vacuously with no graph). Records the `make id` → `aira id` migration intent.

AIRA dogfooding AIRA — tracking its own development, running its own tests through its own runner, ratcheting its own suite against a durable baseline — is the best living proof the interface is good.
