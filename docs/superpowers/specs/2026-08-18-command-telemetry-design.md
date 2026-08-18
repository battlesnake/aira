
# AIRA Live Command-Telemetry Capture (`aira time`) — v1 Design

**Status:** build contract. Base = Angle A (Candidate 1), with Candidate 3's outcome-enum/`Validate` and per-statistic percentile floors, and Candidate 2/3's subcommand key grafted on. Every judge-flagged flaw is resolved below and every file/line reference was read against `master`.

---

## 1. Why

A slowdown investigation ("`go test` got slow this week — since which commit?") has **no evidence** today. AIRA records tickets, findings, compute, and runs, but never *how often ordinary commands run, how long they take, or how often they fail*. Retrospective log ingestion is rejected (the logs may not exist). The owner chose **option (i): a thin aira-owned wrapper agents run their commands through**, capturing by *being the launcher* (like `whale-run`), never by scraping.

The wrapper must be **lighter than `internal/runner`** (no cgroup admission, no `backend.Probe`, no capture blobs, no `RUN-n` ledger, no detach/kill handle) and must record to a **DB-only, high-volume-safe** store exactly as `compute_events`/`rants` do — a git file per command run would swamp the journal (§11 explicitly excludes per-run telemetry from the journal) and merge-conflict by construction.

The graveyard-maker is adoption: a wrapper agents don't use = an empty table. The design rides the **existing `whale-run` prefix reflex** and keeps the wrapper **byte-for-byte transparent** so switching costs nothing and AIRA **never nags**.

---

## 2. The wrapper verb + faces

### Capture verb (carved)

```
aira time [--prefix P... | --no-prefix] [--cwd DIR] [--env K=V...] [--timeout D]
          [--ticket T] [--phase P] [--label L] -- <argv...>
```

- Parses exactly like `run`: options before a **standalone `--`**, target argv after it (reuse the `parseRunArgs` delimiter machinery).
- **Applies the configured `run.prefix`** (via `runner.EffectiveArgv(prefix, target)`) unless `--no-prefix`; `--prefix` overrides. So one wrapper can give the whale RAM cap **and** telemetry (see §6 for the honest caveat that the cap depends on the prefix being configured).
- **Inherits the caller's real stdio fds** (`cmd.Stdin/Stdout/Stderr = os.Stdin/os.Stdout/os.Stderr`) — true TTY passthrough, no tee, no capture. **Forwards the child's exact exit status** as its own process exit code (§4/§8-I1). Forwards SIGINT/SIGTERM to the child.
- Silently records **one** `command_events` row. Prints nothing extra. A telemetry-write failure is a stderr diagnostic that **never** changes the child's exit (§8-I3).

MCP tool `aira_time` returns the **outcome object only** (`status`, `exit_code`, `signal`, `wall_ms`, `key`, `key_source`) — honestly *not* streamed bytes (MCP has no live terminal); live stdio over MCP is deferred (§10).

### Read faces (routed, pure-store)

```
aira commands ls [q] [--by program|key|status|branch|ticket]     # MCP: aira_commands
aira commands count [q] --by <field>                             # size-before-fetch
aira insights show command-latency                               # existing aira_insights
```

`q` filters: `program:`, `key:`, `status:`, `branch:` (head_ref), `commit:` (head_hash), `ticket:`, `phase:`.

cwd is captured client-side (`--cwd` or the process cwd). Git provenance is stamped by the **existing** `stampGitContext` path (`cmd/aira/dispatcher.go:421`): the `time` descriptor sets `GitContext:true`, so `RequiresGitContext` returns true and the resolver runs against the daemon `WorktreeScope` — the identical path `rant` uses.

**Verb name is `time`, never `exec`.** `exec "<cmds>"` (CLI) and `aira_exec` (MCP) are **spec-reserved** for the `internal/interp` command language (design spec lines 42, 55, 179). This is Candidate 2's fatal flaw; rejected.

---

## 3. Data model

New DB-only table, cloned field-for-field from the `compute_events` discipline (`internal/store/compute.go`, `store.go:806-823`). **Never** git-materialised, **never** journaled, retention-capped — the §5.3 *operational telemetry* durability class.

```sql
CREATE TABLE IF NOT EXISTS command_event_counter (
    project_id TEXT PRIMARY KEY, next_number INTEGER NOT NULL, next_seq INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS command_events (
    project_id  TEXT NOT NULL,
    id          TEXT NOT NULL,               -- 'CMD-<n>'
    at          TEXT NOT NULL,               -- advisory wall stamp (RFC3339Nano)
    at_seq      INTEGER NOT NULL,            -- per-project monotonic ordering authority

    key         TEXT NOT NULL,               -- aggregation key (see §5)
    key_source  TEXT NOT NULL,               -- 'label' | 'program-subcommand' | 'program'
    program     TEXT NOT NULL DEFAULT '',    -- basename(argv[0]) of the TARGET, post-prefix
    argv_preview TEXT NOT NULL DEFAULT '',   -- bounded first-N-token snapshot (human drill only)
    argv_digest TEXT NOT NULL DEFAULT '',    -- sha256 of full target argv (honest identity)
    prefix_preview TEXT NOT NULL DEFAULT '', -- bounded snapshot of the applied launch prefix

    status      TEXT NOT NULL,               -- CommandOutcome (see §5/§8-I2)
    exit_code   INTEGER,                     -- NULL unless status='exited'
    signal      TEXT NOT NULL DEFAULT '',    -- non-empty only for signalled|timeout
    wall_ms     INTEGER,                     -- monotonic; NULL only for launch-failed/unknown

    ticket_id   TEXT NOT NULL DEFAULT '',
    phase       TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT '',
    session     TEXT NOT NULL DEFAULT '',
    cwd         TEXT NOT NULL DEFAULT '',

    -- Flattened, status-preserving git provenance (value + full 4-state status
    -- per field): the volatile provenance a 'slow since branch/commit X' query needs.
    head_hash          TEXT NOT NULL DEFAULT '', head_hash_status   TEXT NOT NULL DEFAULT 'unevaluated',
    head_ref           TEXT NOT NULL DEFAULT '', head_ref_status    TEXT NOT NULL DEFAULT 'unevaluated',
    worktree_id        TEXT NOT NULL DEFAULT '', worktree_id_status TEXT NOT NULL DEFAULT 'unevaluated',

    PRIMARY KEY(project_id, id),
    CHECK(status IN ('exited','signalled','timeout','launch-failed','unknown')),
    CHECK(key_source IN ('label','program-subcommand','program')),
    -- Illegal-states-unrepresentable at the DB boundary (mirrors §8-I2):
    CHECK((status='exited'       AND exit_code IS NOT NULL AND signal='' AND wall_ms IS NOT NULL)
       OR (status='signalled'    AND exit_code IS NULL     AND signal<>'' AND wall_ms IS NOT NULL)
       OR (status='timeout'      AND exit_code IS NULL     AND signal<>'' AND wall_ms IS NOT NULL)
       OR (status='launch-failed'AND exit_code IS NULL     AND signal='' AND wall_ms IS NULL)
       OR (status='unknown'      AND exit_code IS NULL     AND signal='')),
    CHECK(head_hash_status   IN ('value','none','unevaluated','mismatch')),
    CHECK(head_ref_status    IN ('value','none','unevaluated','mismatch')),
    CHECK(worktree_id_status IN ('value','none','unevaluated','mismatch')),
    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);
```

**Flattened git columns, not a sibling table** (fixes Candidate 2's JOIN + 6× row amplification on a high-volume aggregate-by-branch hot path) **and keeping the value+`_status` pair** (fixes Candidate 3's `NULL=unevaluated` regression that cannot represent `mismatch`/`none`). The three volatile fields are stored; stable repo scope is implied by `project_id`.

**Retention:** `evictCommandEvents` is an exact clone of `evictComputeEvents` (`compute.go`) — ring by `at_seq` (`... at_seq NOT IN (SELECT ... ORDER BY at_seq DESC LIMIT ?)`) plus optional age cutoff — run inside the insert's `withImmediate` txn. Default `maxCommandEvents = 50000` (higher than compute's 20000 given per-command volume); `maxCommandAgeDays = 0` (off). No cgroup ⇒ **no `cpu_user`/`cpu_sys`/`peak_rss` columns exist** (they are unevaluated by construction, §8-I5; do not add fake-zero columns).

`cpu`/`peak_rss` are deliberately absent: `aira run` remains the cgroup-resource path.

---

## 4. Dispatch — carved capture, routed reads

**`time` is CARVED (`RouteClient`) — forced, not chosen.** Execution must run in the caller's own process so the child inherits the real terminal + cwd; the pure-store daemon rejects any `RouteClient` verb and cannot hold the caller's fds. Identical to `run`. Add `case canonical == "time": return canonical, RouteClient` to `core.Classify` (`routing.go:35`).

**The record write is a client-direct write** inside the carved handler (`c.store.AddCommandEvent`), the **exact posture `run --tool` uses today** for `ComputeEvent` (`run_wiring.go:390` `wireRunCompute`). `time` is **not** `StoreFreeCarved` (it always writes), so `dispatchClient` takes the writable-store branch (`dispatcher.go` → `app.OpenWithDiagnostics`). Routing the write to the daemon single-writer is **D7b's write-relay (task #36)** — a shared, already-accepted deferral, not reinvented (§10). This is a real second writer vs the daemon's store, bounded exactly as `run --tool` is (SQLite WAL + `busy_timeout`); stated plainly, not hidden.

**`commands` (read) is ROUTED (`RouteDaemon`, the default)** — pure-store, like `spend`/`quota`. The daemon serves `ListCommandEvents`/`CommandDistribution`/`CommandLatencyByKey` with single-reader consistency; the `command-latency` gauge rides the existing routed `insights` verb.

### The one new core seam: exact child-exit passthrough

Today `core.Do` collapses a nonzero child into `E_RUN_FAILED` (`core.go:543`, exit `1`) — it cannot pass through `exit 7`. `time` needs the child's exact code. Introduce a dedicated carrier the handler returns and `Do` recognises **before** the generic returns (`core.go:~465`):

```go
type commandTimingData struct {
    Command     any    `json:"command"`      // the recorded CommandEvent view
    ProcessExit int    `json:"-"`            // exact code to propagate
}
// in Do, after presentRunData:
if t, ok := data.(commandTimingData); ok {
    return Response{OK: true, Code: "OK", Data: t.Command, Exit: t.ProcessExit}
}
```

`Response.Exit` already exists and the CLI already honours it first (`main.go:1203 if response.Exit != 0 { return response.Exit }`). Do **not** reuse the `runRecord`/`handlerData` paths (they derive exit from a *code*, which is the wrong vocabulary).

**Exit-vocabulary reconciliation (I1):** once the child launches, the child owns the process exit code by shell convention — `N` (exited), `128+signum` (signalled), `124` (timeout, GNU `timeout` convention), `127` (cannot-exec/launch-failed), `3` (genuinely unknown terminal state). This **intentionally overlaps** AIRA's own 1–4 code space — that is exactly what a transparent wrapper (`env`/`nice`/`timeout`/`whale-run`) does, and the `--json`/structured face is the disambiguator (`status` + `exit_code` + `signal`). AIRA's own error vocabulary is used **only for pre-launch precondition failures** (bad args, missing `--`, prefix invalid), where no child ran and nothing is recorded.

---

## 5. The command-key / aggregation honesty decision

**Default aggregation key = a subcommand-aware program key, with `key_source` stamped on every row and cell.** Full argv is *always* retained (`program`, bounded `argv_preview`, `sha256 argv_digest`) so a drill-down shows exactly what ran, but the **key** drives aggregation.

Honesty tradeoff, all three modes recorded:
- **full argv** — most honest identity, but args vary (paths, `-run` filters) so every run is unique and frequency collapses → "is `go test` slow" is unanswerable;
- **bare `argv[0]`** — aggregates but a documented heuristic that collapses `go build`/`go test`/`go vet` into `go` (Candidate 1's weakness);
- **caller `--label`** — most aggregable and intentional, but an unverifiable claim needing agent effort.

**Resolution (deterministic, pure, golden-tested — no shell parsing):**
1. `--label L` present → `key=L`, `key_source=label`.
2. Else operate on the **target argv** (the prefix is already split off): strip leading wrapper tokens from a fixed allowlist `{env, nice, ionice, timeout, stdbuf, nohup, sudo, whale-run, agentmux}` and leading `VAR=val` assignments (handles an agent double-wrapping `aira time -- whale-run go test`). `program = basename(first remaining token)`.
3. If `program ∈` known-driver set `{go, cargo, git, make, npm, pnpm, yarn, node, python, python3, pytest, docker, kubectl}` → append the first following non-flag token → `key = "go test"`, `key_source=program-subcommand`.
4. Else `key = program`, `key_source=program`.

Every list row and gauge cell carries `key_source`, so a human always sees asserted-label vs heuristic-program vs subcommand-derived. AIRA **never claims the key IS the command**; the argv digest/preview is the verification escape hatch. Known residual: a genuine wrapper-as-command honestly keys on the wrapper name — we do **not** infer intent.

---

## 6. Adoption mechanism (the graveyard-maker)

Adoption is voluntary and is the honest ceiling of the whole feature; four concrete, honest levers:

1. **Byte-for-byte transparency.** `aira time -- go test` streams the same stdout/stderr/stdin, exits with the same code, adds no output. Switching from `whale-run go test` observes *nothing different*. Anything less and agents drop it, so I1/I3 are load-bearing for adoption, not just honesty.
2. **Prefix subsumption (with the honest caveat).** `aira time` applies the configured `run.prefix`. If a project sets `run.prefix: ["whale-run"]`, then `aira time -- go test` gives **both** the RAM cap **and** telemetry in one wrapper of equal length. **Honest correction (Judge 2):** `Config.Run.Prefix` defaults **empty** (`internal/app/project.go:65`, `init` at `:362` sets none) and `aira time` creates **no** cgroup of its own — so the cap is preserved **only when that prefix is configured**. Setting `run.prefix:["whale-run"]` is a documented **one-time** adoption step; without it, `aira time` still captures telemetry but the RAM cap is whatever the target argv itself provides.
3. **Retrofit shell alias.** `whale-run(){ aira time -- "$@"; }` instruments every existing habit with zero per-command edits — paired with lever 2 so the cap is retained.
4. **Skill + MCP.** The dev-loop skill's command snippets switch `whale-run <cmd>` → `aira time -- <cmd>`; MCP agents get `aira_time`.

**AIRA never nags:** it records silently; a telemetry-write failure is a stderr diagnostic that never alters the child (I3); non-adoption yields no reminder, warning, or gate. Every aggregate's universe reads **"recorded `aira time` runs only"** so the coverage gap is visible, never implied away.

---

## 7. The reviewable surface + honesty

Two live-query surfaces (§17: never a stored numeral; each carries universe + as-of; uncomputable reads `unevaluated`).

**(1) `aira commands ls/count --by program|key|status|branch|ticket`** — exact frequency counts and per-bucket failure counts, size-before-fetch (like `count`). This is the free-form drill a slowdown investigation needs (filter by `branch:X`, `key:go test`, `commit:<sha>`). `none`/`unevaluated`/`mismatch` git states become honest `(none)`/`(unevaluated)`/`(mismatch)` buckets, never silently merged.

**(2) `command-latency` insight gauge** — added to `insightRegistry` (`insights.go:101`) with `Kind: GaugeKindDuration` (the reserved-but-unused kind, `insights.go:22`), its `init()` case, and a `computeCommandLatency` reusing `gaugeUniverse`/`gaugeCellUnevaluated`/`GaugeCell.Fields`. Per **key**:
- **count** (frequency): exact count of recorded runs.
- **p50 / p95**: exact **nearest-rank** order statistics computed in Go over the key's retained `wall_ms` for **`status='exited'` rows only** (completed runs — a timeout's capped duration would corrupt the statistic; SQLite has no percentile, the retention cap bounds the read). **Per-statistic sample floor** (fixes Candidate 1's single coarse floor): p50 requires `n ≥ 5`, p95 requires `n ≥ 20`; below floor the field reads `unevaluated` with reason `"n=<k>, need ≥<floor>"` — so a row can show p50 evaluated and p95 unevaluated in the same cell (per-field `GaugeCell.Fields`, as `review-loop-economics` does). Never a fabricated percentile.
- **failure-rate** = `exited-nonzero / exited-total`, exact and carrying the raw counts. `signalled`, `timeout`, `launch-failed`, `unknown` are surfaced as **separate labelled counts, never folded** (a wrapper/environment failure is not a command failure).
- Each cell's **Drilldown = `{Verb:"commands ls", Query:"key:<k>"}`** — a **real** verb (fixes Candidate 3's dangling drilldown, whose gauge pointed at the capture verb).
- Universe: distinct keys, `Scope:"recorded aira time runs only"`, `AsOf:{command_at_seq: MAX(at_seq)}`.

---

## 8. Invariants

- **I1 — child owns the exit code.** Process exit = `N` (exited) / `128+signum` (signalled) / `124` (timeout) / `127` (launch-failed) / `3` (unknown). AIRA's own vocabulary is used only for pre-launch precondition errors. The overlap with 1–4 is intentional and disambiguated by the structured face.
- **I2 — outcome↔exit↔signal↔wall pairing (illegal states unrepresentable).** `exited` ⇒ exit non-null, signal empty, wall non-null; `signalled`/`timeout` ⇒ signal non-empty, exit null, wall non-null; `launch-failed` ⇒ exit null, signal empty, wall null; `unknown` ⇒ exit null, signal empty. Enforced by domain `Validate` **and** the DB CHECK.
- **I3 — telemetry never harms the child.** A write failure is a stderr diagnostic only; it never changes the child's exit or output.
- **I4 — honest counts, not claims.** Aggregates are counts of *recorded* runs over the retained window; universe carries count + `"recorded aira time runs only"` + as-of `at_seq`. Never a prediction.
- **I5 — duration is monotonic and authoritative; cpu/peak_rss are unevaluated by construction.** `wall_ms` via `time.Since`; `at` is advisory (§11). No cgroup ⇒ no cpu/memory columns, never a fake 0 (use `aira run` for resource evidence).
- **I6 — failure-rate is narrow and honest.** `exited-nonzero / exited-total` only; other outcomes are separate counts; latency percentile is over `exited` rows only.
- **I7 — percentiles honest below floor.** Exact nearest-rank; `unevaluated` (reason-stamped) below p50 `n≥5` / p95 `n≥20`.
- **I8 — git provenance is 4-state.** value/none/unevaluated/mismatch per field, from the resolver + daemon-scope cross-check; never a fabricated hash.
- **I9 — DB-only.** Never journaled, never a git file, retention-capped by seq+age.
- **I10 — heuristic key, honest record.** `key`+`key_source` drive aggregation; full argv (`program`+preview+`sha256` digest) is always retained and drillable.

---

## 9. v1 scope

1. `internal/domain/command.go` — `CommandEvent`, `CommandEventInput`, `CommandOutcome` enum `{exited, signalled, timeout, launch-failed, unknown}`, `Validate` enforcing I2 (modelled on `domain/compute.go`; reuse `nil = unevaluated` discipline).
2. `internal/store/command.go` — `AddCommandEvent` (`withImmediate` insert + `nextCommandNumbers` + `evictCommandEvents` + flattened git columns via a shared cross-check helper), `ListCommandEvents`, `CommandDistribution(query, by)`, `CommandLatencyByKey(ctx)` (Go nearest-rank p50/p95 with per-statistic floor). Direct clones of the `compute.go`/`rant.go` shapes.
3. `internal/store/store.go` — `command_events` + `command_event_counter` DDL beside `compute_events`; `maxCommandEvents`/`maxCommandAgeDays` fields, defaults (50000/0), validation.
4. Carved `time` verb — thin exec helper (apply configured prefix via `runner.EffectiveArgv`, inherit real std fds, monotonic timing, `--timeout` via context+kill, classify outcome), then client-direct `AddCommandEvent`; returns `commandTimingData` for exact exit passthrough.
5. Routed `commands` read verb (`ls`/`count`) + `command-latency` gauge in `insightRegistry`.
6. CLI `--` passthrough (`parseTimeArgs` clone of `parseRunArgs` with the smaller option set; `time` arm in `removeJSON`), MCP `aira_time`/`aira_commands`, skill entries from the dispatch tables; dev-loop skill doc updated to `aira time -- <cmd>` + the `run.prefix:["whale-run"]` one-time step.
7. `ProjectConfig.Commands {MaxEvents, MaxAgeDays}` sibling of `Compute`, plumbed store opts → `daemon.WorktreeScope` → store defaults.

---

## 10. Deferrals

- **Daemon single-writer for the record write** = D7b write-relay (task #36); v1 is client-direct, identical to `run --tool` today — shared deferral, not reinvented.
- **cpu/peak_rss / cgroup rusage** for timed commands — that is `aira run`; `time` is cgroup-free ⇒ unevaluated by construction.
- **Output capture / blobs / detach / kill / follow** — `aira run`'s job; `time` is transparent, foreground-only, fire-and-forget.
- **MCP live stdio** — `aira_time` returns outcome-only in v1 (no terminal to inherit); bounded MCP stdin is a thin follow-on.
- **Direction-vs-baseline** ("slower than last week") — v1 shows the honest retained-window percentile only.
- **Normaliser extensions** (`python -m X` → `X`, deeper wrapper unwrap) — documented deterministic heuristic; v1 ships the allowlist + known-driver set.
- **argv secret redaction** — `argv_preview` is length-capped, **not** certified sanitised (stated honestly; store-argv could be made opt-in later).
- **Auto-`ComputeEvent` bridge** from `time --tool` — deferred to avoid coupling the lite path to the compute normaliser.

---

## 11. Discriminator tests

1. **Exit passthrough:** `aira time -- sh -c 'exit 7'` → process exit **7** AND recorded `status=exited, exit_code=7`. (Swallow/normalise-to-1 fails.)
2. **Outcome pairing `Validate`:** `{exited, exit_code:nil}`, `{signalled, exit_code:non-nil}`, `{exited, exit_code:0, signal:"KILL"}` all rejected by domain `Validate` and DB CHECK. (The failure dimension same-table would hide.)
3. **Failure-rate honesty:** a key with 3×exit0 + 1×exit-nonzero + 1×signalled + 1×timeout → failure-rate = **1/4** (exited-only denominator); signalled and timeout appear as separate counts. (Folding → 2/6 or 3/6 fails.)
4. **Per-statistic floor:** 6 exited samples → p50 evaluated, p95 `unevaluated("n=6, need ≥20")`; 3 samples → both unevaluated; ≥20 → both exact nearest-rank. (A p95 from <20 fabricated fails.)
5. **Key = target subcommand, prefix stripped:** `aira time --prefix whale-run -- go test ./x` → `key="go test"`, `key_source=program-subcommand`, `program="go"`, `argv_digest` set; double-wrap `aira time -- whale-run go test` → `key="go test"` (leading `whale-run` stripped). (Keying on `whale-run` fails.)
6. **launch-failed ≠ failure:** `aira time -- /nonexistent` → `status=launch-failed`, `exit_code NULL`, `wall_ms NULL`, **process exit 127**, excluded from failure denominator, surfaced as its own count. (Recording as command-failure, or exit≠127, fails.)
7. **Telemetry write never alters child exit:** force `AddCommandEvent` to error after child exit 0 → process still exits 0 + a stderr diagnostic. (Returning the DB error as process exit fails.)
8. **Git 4-state preserved:** branch X → `head_ref_status=value 'refs/heads/X'`; detached HEAD → `status=none`; unreadable → `unevaluated`; daemon-scope worktree disagreement → `mismatch`. (A 2-state `NULL=unevaluated` that cannot represent `mismatch`/`none` fails — Candidate 3's regression.)
9. **DB-only:** 1000 `aira time` calls grow no git-tracked file, add zero journal entries, reserve no `RUN-n`; rows land in `command_events`; the `maxCommandEvents+1`-th insert evicts oldest by `at_seq`.
10. **No cgroup dependency:** `aira time` succeeds where cgroup delegation is absent / `backend.Probe` would fail (it never calls Probe/Create). (Proves it is the lite path.)
11. **Routed read parity + resolvable drilldown:** `commands ls` classifies `RouteDaemon`; daemon and in-process return identical rows; the `command-latency` gauge Drilldown `commands ls key:<k>` targets a **real** verb.
12. **Timeout:** `aira time --timeout 1s -- sleep 30` → `status=timeout`, `signal=KILL`, `exit_code NULL`, `wall_ms≈1000`, process exit **124**; excluded from failure-rate and from the latency percentile, surfaced as a timeout count.

---

## 12. Named integration points (verified against `master`)

**Reuse (read, do not reinvent):**
- `internal/store/compute.go` — `AddComputeEvent`/`evictComputeEvents`/`nextComputeNumbers`/`scanComputeEvent`, `optionalInt64`/`nullInt64`: the exact shape to clone.
- `internal/store/store.go` — `compute_events`/`compute_event_counter` DDL (`:806-823`); defaults (`:471-478`); `Options`/`ScopeOptions` `MaxComputeEvents…` fields (`:68`, `:91`, `:142`); additive `ALTER` precedent (`:844`).
- `internal/store/rant.go` — `gitContextFields`/`contextFromFields`/`crossCheckRantContext` (`:563-602`): reuse the daemon-scope cross-check (rename to a shared `crossCheckGitContext`), then **flatten** value+status into the `command_events` columns rather than a sibling table.
- `internal/store/insights.go` — `insightRegistry` (`:101`) + `init()` (`:112`); `GaugeKindDuration` (`:22`, reserved/unused); `gaugeUniverse`/`gaugeCellUnevaluated`/`GaugeCell.Fields` scaffolding; `computeFlakyRate` (`:308`, `Denominator==0 → unevaluated`) and `computeQuotaBurn` (`:361`, per-field unevaluated) as honesty precedents.
- `internal/gitcontext/resolver.go` — `Field{Value,Status,Reason}` (`:34`), `StatusValue|None|Unevaluated|Mismatch` (`:26-31`).
- `internal/runner/types.go` — `EffectiveArgv(prefix, target)` for prefix+target validation. **Do not** touch `Launch`/admit/`backend.Probe`/ledger; **do not** reuse `runner.Status` (its `oom-killed`/`lost` are unreachable cgroup-less — Candidate 2's smell).
- `internal/core/run_wiring.go` — `wireRunCompute` (`:358-406`, `c.store.AddComputeEvent` at `:390`): the precedent carved-write posture `time` copies.

**Create / modify:**
- `internal/domain/command.go` (new) — `CommandEvent`/`CommandEventInput`/`CommandOutcome`/`Validate` (model on `domain/compute.go:56-140`).
- `internal/store/command.go` (new) — store methods (§9.2).
- `internal/core/core.go` — register `time` descriptor (`GitContext:true`, `MCPTool:"aira_time"`) modelled on the `run` block (`:1380-1512`) and `commands` read descriptor modelled on `spend`/`quota` (`:964-1068`); add the `commandTimingData` exit-carrying branch in `Do` (`:~465`); add `AddCommandEvent`/`ListCommandEvents`/`CommandDistribution`/`CommandLatencyByKey` to the `Store` interface (`:112-186`). Thin exec helper (new small file or inline) inheriting real std fds.
- `internal/core/routing.go` — `Classify`: `time → RouteClient` (`:35`, beside `run`); `commands` stays default `RouteDaemon`. `RequiresGitContext` already keys off `descriptor.GitContext` ⇒ `time` gets stamping free. **Do not** add `time` to `StoreFreeCarved` (it writes).
- `cmd/aira/main.go` — `parseTimeArgs` (clone of `parseRunArgs` `:435`, smaller option set); `time` arm in `removeJSON`'s `--`-aware branch (`:299`, currently `EqualFold(argv[0],"run")`); `buildRequest` maps `time` positional target → `args["argv"]`, options → `prefix/cwd/env/timeout/ticket/phase/label/no_prefix`; add `time` to the human-separator branch (`:186`). The exit path (`:1203`) already returns `response.Exit`.
- `cmd/aira/dispatcher.go` — extend the **Live gate** in **both** `dispatchCarved` and `inProcessDispatcher` (`Live: (request.Verb=="run"||request.Verb=="git") …`) to include `"time"` so its stdio streams under CLI and is suppressed under JSON/MCP (fixes Judge 3's "no dispatcher change" error in Candidate 3). Add `MaxCommandEvents`/`MaxCommandAgeDays` to `scopeFromProject` (beside `MaxComputeEvents`).
- `internal/app/project.go` — `ProjectConfig.Commands CommandsConfig{MaxEvents,MaxAgeDays}` sibling of `Compute` (`:81`, `:89`); plumb to store opts (`:230`), config view (`:626`), and validation (`:482`).
- `internal/daemon` — add `MaxCommandEvents`/`MaxCommandAgeDays` to `WorktreeScope` + `ScopeOptions` (same chain as `MaxComputeEvents`).

**Rejected:** `aira exec`/`aira_exec` (spec-reserved §19); runner exec-core extraction (needless refactor risk in a correctness-critical package; the ~30-line inherit-stdio helper reusing exported `EffectiveArgv` is the honest minimal reading); `runner.Status` reuse (imports unreachable cgroup states); sibling git-context table (JOIN + row amplification on the hot path); 2-state `NULL=unevaluated` git columns (cannot represent `mismatch`/`none`); gauge-only v1 (a slowdown investigation needs the free-form `commands ls` drill).