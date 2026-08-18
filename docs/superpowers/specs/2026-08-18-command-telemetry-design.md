
# AIRA Live Command-Telemetry Capture (`aira time`) — v3 Design

**Status:** v3 (Fable GATE-PASS; two gates x three iterations folded). Build contract. Base = Angle A (Candidate 1), with Candidate 3's outcome-enum/`Validate` and per-statistic percentile floors, and Candidate 2/3's subcommand key grafted on. Every file/function claim below was checked against the working tree.

---

## 1. Why

A slowdown investigation ("`go test` got slow this week — since which commit?") has **no evidence** today. AIRA records tickets, findings, compute, and runs, but never *how often ordinary commands run, how long they take, or how often they fail*. Retrospective log ingestion is rejected (the logs may not exist). The owner chose **option (i): a thin aira-owned wrapper agents run their commands through**, capturing by *being the launcher* (like `whale-run`), never by scraping.

The wrapper must be **lighter than `internal/runner`** (no cgroup admission, no `backend.Probe`, no capture blobs, no `RUN-n` ledger, no detach/kill handle) and must record to a **DB-only, high-volume-safe** store exactly as `compute_events`/`rants` do — a git file per command run would swamp the journal (§11 explicitly excludes per-run telemetry from the journal) and merge-conflict by construction.

The graveyard-maker is adoption: a wrapper agents don't use = an empty table. The design rides the **existing `whale-run` prefix reflex** and keeps the ordinary interactive path **byte-for-byte transparent on the normal path** so switching costs nothing and AIRA **never nags**.

---

## 2. The wrapper verb + faces

### Capture verb (carved)

```
aira time [--prefix P... | --no-prefix] [--cwd DIR] [--env K=V...] [--timeout D]
          [--ticket T] [--phase P] [--label L] -- <argv...>
```

- Parses exactly like `run`: options before a **standalone `--`**, target argv after it (reuse the `parseRunArgs` delimiter machinery).
- **Applies the configured `run.prefix`** unless `--no-prefix`; `--prefix` overrides. The exact plumbing seam is `Core.WithCommandPrefix(project.Config.Run.Prefix)`, then the handler calls `runner.EffectiveArgv(selectedPrefix, target)` (§6/§12). One wrapper can therefore give the whale RAM cap **and** telemetry only when that prefix is configured.
- Stdio is face-dependent and non-negotiable. When `FaceOutput.Live` is true (ordinary interactive CLI), set `cmd.Stdin/Stdout/Stderr = os.Stdin/os.Stdout/os.Stderr`: real TTY passthrough, no tee or capture. When `Live` is false (`--json` and MCP), set child stdin **and** stdout/stderr all to a single `/dev/null` opened `O_RDWR` as an `*os.File` (or leave them `nil`, which `os/exec` binds to `os.DevNull` by fd) — **never `io.Discard`**: a non-`*os.File` writer makes `os/exec` create a pipe and a copier goroutine, so a descendant the child leaves behind that inherits the write end blocks `cmd.Wait` past child exit and inflates `wall_ms` (the same hang class the repo already fixed for `gitValue` with `WaitDelay`). Additionally set `cmd.WaitDelay` (a small bound) as belt-and-suspenders so a leaked descendant can never wedge the wrapper. Never attach the JSON/MCP protocol streams to the child. In either case, forward SIGINT/SIGTERM to the direct child and map its outcome to §4/§8-I1.
- Silently records **one** `command_events` row. On the ordinary human face, `time` renders no response `Data` and normally emits no bytes at all; `--json` and MCP retain the structured response. A telemetry-write failure is a diagnostic on stderr (and a structured warning where available) that **never** changes the target outcome (§8-I3).

`aira time --json` and MCP `aira_time` return the **recorded `CommandEvent` view as structured `Data`** — the same object `commandTimingData.Command` carries (§4): `status`, `exit_code`, `signal`, `wall_ms`, `key`, `key_source`, `program`, and the git-provenance fields — **never streamed child bytes**. `commandTimingData` and the read faces project the identical `CommandEvent` shape, so there is one view, not two. The generated MCP tool description must say exactly: **"output not captured; returns the recorded command event only"**. MCP stdin is the JSON-RPC transport (`runMCPWithDispatcher` passes it to the dispatcher), so the child receives the `/dev/null` `*os.File` above, not that reader; its stdout/stderr go to the same `/dev/null`. Live stdio over MCP is deferred (§10).

### Read faces (routed, pure-store)

```
aira commands ls [q] [--by program|key|status|branch|ticket]     # MCP: aira_commands
aira commands count [q] --by <field>                             # size-before-fetch
aira insights show command-latency                               # existing aira_insights
```

`q` filters: `program:`, `key-source:`, `key:`, `status:`, `branch:` (matches `head_ref` where `head_ref_status='value'`, comparing the caller's value against the ref's short name — `refs/heads/X` stripped to `X` — so `branch:main` matches the stored `refs/heads/main`; an exact `refs/…` value also matches verbatim), `commit:` (`head_hash` exact, `head_hash_status='value'`), `ticket:`, `phase:`. Rows whose git status is `none`/`unevaluated`/`mismatch` never match a `branch:`/`commit:` value filter (they surface only in the honest `(none)`/`(unevaluated)`/`(mismatch)` `--by branch` buckets). Any key drill supplies both `key-source:` and `key:` (§7).

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
    program     TEXT NOT NULL DEFAULT '',    -- basename(normalised target program); launch prefix excluded
    argv_preview TEXT NOT NULL DEFAULT '',   -- token+length-capped target snapshot (human drill only)
    argv_digest TEXT NOT NULL DEFAULT '',    -- sha256 over the NUL-delimited join of the target argv tokens (identity, not verbatim storage; NUL-delimited so ["ab","c"] and ["a","bc"] cannot collide)
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

    -- Flattened, status-preserving git provenance (value + 4-state status).
    -- Field.Reason is deliberately not stored on these high-volume rows.
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
    CHECK((head_hash_status IN ('value','mismatch') AND length(head_hash)>0)
       OR (head_hash_status IN ('none','unevaluated') AND head_hash='')),
    CHECK((head_ref_status IN ('value','mismatch') AND length(head_ref)>0)
       OR (head_ref_status IN ('none','unevaluated') AND head_ref='')),
    CHECK((worktree_id_status IN ('value','mismatch') AND length(worktree_id)>0)
       OR (worktree_id_status IN ('none','unevaluated') AND worktree_id='')),
    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);
```

**Flattened git columns, not a sibling table** (fixes Candidate 2's JOIN + 6× row amplification on a high-volume aggregate-by-branch hot path) **and keeping the value+`_status` pair** (fixes Candidate 3's `NULL=unevaluated` regression that cannot represent `mismatch`/`none`). The value↔status CHECKs exactly mirror `rant_git_context`: `value|mismatch` requires a non-empty value; `none|unevaluated` requires an empty value. The three volatile fields are stored; stable repo scope is implied by `project_id`. The source resolver's `Field.Reason` is dropped while status survives, deliberately keeping high-volume rows lean; reason persistence is deferred (§10).

**Retention:** `evictCommandEvents` is an exact clone of `evictComputeEvents` (`compute.go`) — ring by `at_seq` (`... at_seq NOT IN (SELECT ... ORDER BY at_seq DESC LIMIT ?)`) plus optional age cutoff — run inside the insert's `withImmediate` txn. Default `maxCommandEvents = 50000` (higher than compute's 20000 given per-command volume); `maxCommandAgeDays = 0` (off). No cgroup ⇒ **no `cpu_user`/`cpu_sys`/`peak_rss` columns exist** (they are unevaluated by construction, §8-I5; do not add fake-zero columns).

`cpu`/`peak_rss` are deliberately absent: `aira run` remains the cgroup-resource path.

---

## 4. Dispatch — carved capture, routed reads

**`time` is CARVED (`RouteClient`) — forced, not chosen.** Execution must run in the caller's own process so an ordinary Live child can inherit the real terminal + cwd; the pure-store daemon rejects any `RouteClient` verb and cannot hold the caller's fds. Identical to `run`. Add `case canonical == "time": return canonical, RouteClient` to `core.Classify` (`routing.go:35`).

**The record write is a client-direct write** inside the carved handler (`c.store.AddCommandEvent`), the **exact posture `run --tool` uses today** for `ComputeEvent` (`wireRunCompute` calls `c.store.AddComputeEvent`). `time` is **not** `StoreFreeCarved` (it always writes), so `dispatchClient` takes the writable-store branch (`app.OpenWithDiagnostics`). This is a real second writer beside the daemon-owned store, bounded by the same SQLite WAL + 5000 ms `busy_timeout` as the accepted `run --tool` path. D7a made only store-free carved invocations avoid a writable store; it explicitly left writable carved verbs valid. Command telemetry **joins D7b's write-relay scope (task #36)**; v1 implementation is client-direct and does not block on D7b (§10).

**`commands` (read) is ROUTED (`RouteDaemon`, the default)** — pure-store, like `spend`/`quota`. The daemon serves `ListCommandEvents`/`CommandDistribution`/`CommandLatencyByKeyPair` with single-reader consistency; the `command-latency` gauge rides the existing routed `insights` verb.

### The one new core seam: exact child-exit passthrough

Today `core.Do` collapses a nonzero child into `E_RUN_FAILED` (`core.go:543`, exit `1`) — it cannot pass through `exit 7`. `time` needs the child's exact code. Introduce a dedicated carrier the handler returns and `Do` recognises **before** the generic returns (`core.go:~465`):

```go
type commandTimingData struct {
    Command     any    `json:"command"`      // the recorded CommandEvent view
    ProcessExit int    `json:"-"`            // passthrough or wrapper-synthesised code
}
// In Do, after handlerData/pending-detach extraction and presentRunData:
if t, ok := data.(commandTimingData); ok {
    return Response{
        OK: true, Code: "OK", Data: t.Command, Exit: t.ProcessExit,
        Warnings: warnings, AfterWrite: afterWrite,
    }
}
```

`Response.Exit` already exists and `render` honours it after rendering/finalisation. The carrier branch must not drop warnings already unwrapped from `handlerData` or the accumulated `AfterWrite` callback. Do **not** reuse the `runRecord` result path (it derives exit from AIRA's error-code vocabulary).

The human face needs a dedicated `time` suppression branch beside the existing `run-log` special case, before the generic `renderHuman` call. `renderHuman` currently `MarshalIndent`s every successful `response.Data`, so allowing `commandTimingData.Command` through it would dump an event after every command. `renderTime` emits no `Data` to stdout, emits only real warnings/errors to diagnostic stderr, invokes `AfterWrite` with the delivery result, and returns `Response.Exit`. `--json` and MCP continue through their structured renderers. **Do not add `time` to `needsSeparator`:** the child writes directly to inherited fd 1, bypassing `lineTrackingWriter`, so its byte tracker cannot observe the child and cannot decide whether a separator is needed.

**Exit-vocabulary reconciliation (I1):** a normally exited target passes through `N`; a signalled target maps to `128+signum`. Codes `124` (timeout), `127` (launch-failed), and `3` (genuinely unknown terminal state) are **wrapper-synthesised**, not literally child-owned. The launch-failed mapping deliberately conflates the shell's 126/127 cannot-invoke distinctions. At the ordinary human CLI, a child exit 1–4 and an AIRA error exit 1–4 are genuinely ambiguous; **only** `--json`/MCP resolves that overlap through `status`/`code` plus `exit_code`/`signal`. AIRA's own error vocabulary is used strictly for pre-launch precondition failures (bad args, missing `--`, invalid prefix/config), where nothing ran and no row is recorded.

---

## 5. The command-key / aggregation honesty decision

**Default aggregation identity = the pair (`key_source`, `key`), with both stamped on every row, bucket, gauge cell, and drill.** The event retains argv identity as a SHA-256 digest plus a bounded, token-and-length-capped preview; the preview is **not certified sanitised** and is not verbatim full argv. The pair drives aggregation.

Honesty tradeoff, all three modes recorded:
- **full argv** — most honest identity, but args vary (paths, `-run` filters) so every run is unique and frequency collapses → "is `go test` slow" is unanswerable;
- **bare `argv[0]`** — aggregates but a documented heuristic that collapses `go build`/`go test`/`go vet` into `go` (Candidate 1's weakness);
- **caller `--label`** — most aggregable and intentional, but an unverifiable claim needing agent effort.

**Resolution (deterministic, pure, golden-tested — no shell parsing):**
1. `--label L` present → `key=L`, `key_source=label`.
2. Else operate on the **target argv** (the configured/override prefix is already separate). Repeatedly consume leading `VAR=val` assignments and only these exact wrapper forms:
   - `timeout <duration>`: consume two tokens.
   - `nice -n <N>` and `ionice -c <N>`: consume three tokens.
   - `stdbuf` followed by one or more exact `-i <L>`, `-o <L>`, or `-e <L>` pairs: consume the wrapper and those pairs.
   - `sudo` with optional exact `-u <user>`: consume one or three tokens.
   - `env`: consume the wrapper, after which the loop consumes its leading assignments.
   - Argumentless `whale-run` and `nohup`: consume one token.

   Then `program = basename(first remaining token)`. If a recognised arg-taking wrapper is malformed or uses an unsupported option shape, stop rather than guessing, so that wrapper becomes the honest program. This table replaces flat token stripping: wrapper arguments can never become the program merely because their wrapper was removed.
3. If `program ∈` known-driver set `{go, cargo, git, make, npm, pnpm, yarn, node, python, python3, pytest, docker, kubectl}` → append the first following non-flag token → `key = "go test"`, `key_source=program-subcommand`.
4. Else `key = program`, `key_source=program`.

Required normaliser goldens include `timeout 30s go test ./x` → (`program="go"`, `key_source="program-subcommand"`, `key="go test"`), `nice -n 10 go build` → `"go build"`, and target `whale-run go test` → `"go test"`. Every list row and gauge cell carries `key_source`, so a human always sees asserted-label vs heuristic-program vs subcommand-derived. AIRA **never claims the key IS the command**; the digest/preview is only a bounded verification aid. Known residual: an unrecognised or genuinely invoked wrapper honestly keys on the wrapper name—we do not infer deeper intent (§10).

---

## 6. Adoption mechanism (the graveyard-maker)

Adoption is voluntary and is the honest ceiling of the whole feature; four concrete, honest levers:

1. **Interactive transparency.** Under the ordinary Live CLI, `aira time -- go test` attaches the target to the real stdio fds, exits with the mapping in I1, and renders no response payload. Switching from `whale-run go test` adds no normal-path AIRA output. (`--json`/MCP deliberately use /dev/null stdio and return the recorded command event.)
2. **Prefix subsumption through an explicit seam.** `runner.EffectiveArgv` does not discover configuration: it only validates and combines the slices supplied to it, while `runner.Runner.prefix` is private and normally consulted inside `Launch`. Therefore both dispatcher construction paths pass a defensive copy of `project.Config.Run.Prefix` through new `Core.WithCommandPrefix(prefix []string)`. The `time` handler chooses explicit `--prefix`, configured prefix, or empty for `--no-prefix`, and calls `runner.EffectiveArgv(selectedPrefix, target)` without calling `Launch`. If a project sets `run.prefix: ["whale-run"]`, `aira time -- go test` gives both the RAM cap and telemetry. `Run.Prefix` defaults empty (`PrepareInit` constructs no Run prefix), and `time` creates no cgroup: the RAM cap is subsumed **only after** the documented one-time prefix configuration. Without it, telemetry still works but AIRA makes no RAM-cap claim.
3. **Retrofit shell alias.** `whale-run(){ aira time -- "$@"; }` instruments every existing habit with zero per-command edits — paired with lever 2 so the cap is retained.
4. **Skill + MCP.** The dev-loop skill's command snippets switch `whale-run <cmd>` → `aira time -- <cmd>`; MCP agents get `aira_time`.

**AIRA never nags:** it records silently; a telemetry-write failure is a stderr diagnostic that never alters the child (I3); non-adoption yields no reminder, warning, or gate. Every aggregate's universe reads **"recorded `aira time` runs only"** so the coverage gap is visible, never implied away.

---

## 7. The reviewable surface + honesty

Two live-query surfaces (§17: never a stored numeral; each carries universe + as-of; uncomputable reads `unevaluated`).

**(1) `aira commands ls/count --by program|key|status|branch|ticket`** — exact frequency counts and per-bucket failure counts, size-before-fetch (like `count`). `--by key` groups and returns the pair (`key_source`, `key`), never `key` alone. A key drill filters by both, for example `key-source:program-subcommand key:go test`; a label and heuristic that happen to spell the same key remain distinct populations. `none`/`unevaluated`/`mismatch` git states become honest `(none)`/`(unevaluated)`/`(mismatch)` buckets, never silently merged.

**(2) `command-latency` insight gauge** — added to `insightRegistry` with `Kind: GaugeKindDuration` (currently reserved but unused), its `init()` case, and a `computeCommandLatency` reusing `gaugeUniverse`/`gaugeCellUnevaluated`/`GaugeCell.Fields`. Per **(`key_source`, `key`) cell**:
- **count** (frequency): exact count of recorded runs.
- **p50 / p95**: exact **nearest-rank** order statistics computed in Go over the key's retained `wall_ms` for **`status='exited'` rows only** (completed runs — a timeout's capped duration would corrupt the statistic; SQLite has no percentile, the retention cap bounds the read). **Per-statistic sample floor** (fixes Candidate 1's single coarse floor): p50 requires `n ≥ 5`, p95 requires `n ≥ 20`; below floor the field reads `unevaluated` with reason `"n=<k>, need ≥<floor>"` — so a row can show p50 evaluated and p95 unevaluated in the same cell (per-field `GaugeCell.Fields`, as `review-loop-economics` does). Never a fabricated percentile.
- **failure-rate** = `exited-nonzero / exited-total`, exact and carrying the raw counts. `signalled`, `timeout`, `launch-failed`, `unknown` are surfaced as **separate labelled counts, never folded** (a wrapper/environment failure is not a command failure).
- Each cell explicitly carries `key_source` and `key`; its **Drilldown = `{Verb:"commands ls", Query:"key-source:<s> key:<k>"}`**. The store query applies both predicates. This targets a real verb and cannot blend an asserted label with a heuristic key.
- Universe: distinct (`key_source`, `key`) pairs, `Scope:"recorded aira time runs only"`, `AsOf:{command_at_seq: MAX(at_seq)}`.

---

## 8. Invariants

- **I1 — exact target exit where one exists; explicit wrapper codes otherwise.** Process exit = target `N` (exited) / `128+signum` (signalled); the wrapper synthesises `124` (timeout), `127` (launch-failed, collapsing shell 126/127 distinctions), or `3` (unknown). AIRA's own 1–4 vocabulary is strictly pre-launch, when nothing ran and no event is recorded. Human CLI overlap between target/AIRA 1–4 is real and is resolved only by `--json`/MCP.
- **I2 — outcome↔exit↔signal↔wall pairing (illegal states unrepresentable).** `exited` ⇒ exit non-null, signal empty, wall non-null; `signalled`/`timeout` ⇒ signal non-empty, exit null, wall non-null; `launch-failed` ⇒ exit null, signal empty, wall null; `unknown` ⇒ exit null, signal empty. Enforced by domain `Validate` **and** the DB CHECK.
- **I3 — telemetry never harms the child.** A write failure adds an AIRA stderr diagnostic only; it never changes the child's exit or alters/suppresses the child's own bytes.
- **I4 — honest counts, not claims.** Aggregates are counts of *recorded* runs over the retained window; universe carries count + `"recorded aira time runs only"` + as-of `at_seq`. Never a prediction.
- **I5 — duration is monotonic and authoritative; cpu/peak_rss are unevaluated by construction.** `wall_ms` via `time.Since`; `at` is advisory (§11). No cgroup ⇒ no cpu/memory columns, never a fake 0 (use `aira run` for resource evidence).
- **I6 — failure-rate is narrow and honest.** `exited-nonzero / exited-total` only; other outcomes are separate counts; latency percentile is over `exited` rows only.
- **I7 — percentiles honest below floor.** Exact nearest-rank; `unevaluated` (reason-stamped) below p50 `n≥5` / p95 `n≥20`.
- **I8 — git provenance is 4-state.** value/none/unevaluated/mismatch per field, from the resolver + daemon-scope cross-check; never a fabricated hash.
- **I9 — DB-only.** Never journaled, never a git file, retention-capped by seq+age.
- **I10 — heuristic pair, bounded honest identity.** (`key_source`, `key`) drives aggregation and drilldown. Argv identity is retained via SHA-256 digest plus a bounded, length-capped, not-certified-sanitised preview—not verbatim argv.
- **I11 — no cgo.** The lite launcher uses Go `os/exec`, `os/signal`, and syscall facilities only; no cgo dependency or helper binary is introduced.

---

## 9. v1 implementation scope

1. `internal/domain/command.go` — `CommandEvent`, `CommandEventInput`, `CommandOutcome` enum `{exited, signalled, timeout, launch-failed, unknown}`, `Validate` enforcing I2 (modelled on `domain/compute.go`; reuse `nil = unevaluated` discipline).
2. `internal/store/command.go` — `AddCommandEvent` (`withImmediate` insert + `nextCommandNumbers` + `evictCommandEvents` + flattened git columns via a shared cross-check helper), `ListCommandEvents`, `CommandDistribution(query, by)`, `CommandLatencyByKeyPair(ctx)` (group by `key_source,key`; Go nearest-rank p50/p95 with per-statistic floor). Direct clones of the `compute.go`/`rant.go` shapes.
3. `internal/store/store.go` — `command_events` + `command_event_counter` DDL beside `compute_events`; `maxCommandEvents`/`maxCommandAgeDays` fields, defaults (50000/0), validation.
4. `Core.commandPrefix []string` + `WithCommandPrefix([]string)` (defensive copy), set from `project.Config.Run.Prefix` by both carved dispatcher paths and on both normal/output-cap Core construction branches. The carved `time` handler chooses configured/override/no prefix, passes it with target to `runner.EffectiveArgv`, and never calls runner `Launch`.
5. Carved `time` verb — thin exec helper (Live: real OS stdio; non-Live: a single `/dev/null` (O_RDWR) for stdin+stdout+stderr; monotonic timing; `--timeout` kills the direct child; classify outcome), then client-direct `AddCommandEvent`; returns `commandTimingData` for exit passthrough/synthesis. Human renderer suppresses the structured payload; JSON/MCP retain it.
6. Routed `commands` read verb (`ls`/`count`) + `command-latency` gauge in `insightRegistry`, with every key aggregation/query keyed by (`key_source`,`key`).
7. CLI `--` passthrough (`parseTimeArgs` clone of `parseRunArgs` with the smaller option set; `time` arm in `removeJSON`), MCP `aira_time`/`aira_commands`, skill entries from the dispatch tables; dev-loop skill doc updated to `aira time -- <cmd>` + the `run.prefix:["whale-run"]` one-time step. The `time` descriptor's generated MCP description includes "output not captured; returns the recorded command event only".
8. `ProjectConfig.Commands {MaxEvents, MaxAgeDays}` sibling of `Compute`, plumbed store opts → `daemon.WorktreeScope` → store defaults.

---

## 10. Deferrals

- **Daemon single-writer for the record write** = D7b write-relay (task #36). Command telemetry joins that relay scope; v1 remains client-direct, consistent with the existing accepted writable carved path used by `run --tool` and does not block on D7b.
- **cpu/peak_rss / cgroup rusage** for timed commands — that is `aira run`; `time` is cgroup-free ⇒ unevaluated by construction.
- **Output capture / blobs / detach / kill / follow** — `aira run`'s job; `time` is transparent, foreground-only, fire-and-forget.
- **MCP live stdio/output capture** — `aira_time` gives the child `/dev/null` (O_RDWR) stdio and returns the recorded command event only in v1; the JSON-RPC stdin is never exposed to the child.
- **Direction-vs-baseline** ("slower than last week") — v1 shows the honest retained-window percentile only.
- **Normaliser extensions** (`python -m X` → `X`, `agentmux whale run ...`, unsupported wrapper option shapes, and deeper unwrap) — v1 stops at the explicit per-wrapper table rather than guessing.
- **argv secret redaction / verbatim storage** — v1 stores argv identity as SHA-256 plus a bounded, token-and-length-capped preview. The preview is **not** certified sanitised and full argv is not retained; stronger redaction or opt-in verbatim storage is later work.
- **Git `Field.Reason` persistence** — value and four-state status survive in the flattened row; reason is omitted to keep high-volume rows lean.
- **Process-group control** — the child remains in the wrapper's process group. Ctrl-C can therefore deliver SIGINT once from the terminal and again from the wrapper's forwarder. Removing the double delivery requires a deliberate process-group topology and is deferred.
- **Descendant timeout cleanup** — `--timeout` kills only the direct child; descendants may linger. This is the known #20 residual and is not overclaimed as tree/cgroup cleanup.
- **Auto-`ComputeEvent` bridge** from `time --tool` — deferred to avoid coupling the lite path to the compute normaliser.

---

## 11. Discriminator tests

1. **Exit passthrough:** `aira time -- sh -c 'exit 7'` → process exit **7** AND recorded `status=exited, exit_code=7`. (Swallow/normalise-to-1 fails.)
2. **Outcome pairing `Validate`:** `{exited, exit_code:nil}`, `{signalled, exit_code:non-nil}`, `{exited, exit_code:0, signal:"KILL"}` all rejected by domain `Validate` and DB CHECK. (The failure dimension same-table would hide.)
3. **Failure-rate honesty:** a key with 3×exit0 + 1×exit-nonzero + 1×signalled + 1×timeout → failure-rate = **1/4** (exited-only denominator); signalled and timeout appear as separate counts. (Folding → 2/6 or 3/6 fails.)
4. **Per-statistic floor:** 6 exited samples → p50 evaluated, p95 `unevaluated("n=6, need ≥20")`; 3 samples → both unevaluated; ≥20 → both exact nearest-rank. (A p95 from <20 fabricated fails.)
5. **Key = target subcommand, prefix separate:** `aira time --prefix whale-run -- go test ./x` → `key="go test"`, `key_source=program-subcommand`, `program="go"`, `argv_digest` set; target double-wrap `aira time -- whale-run go test` → `key="go test"` (leading argumentless wrapper consumed). (Keying on configured prefix or `whale-run` fails.)
6. **Wrapper-argument goldens:** `timeout 30s go test ./x` → `key="go test"`; `nice -n 10 go build` → `key="go build"`; `whale-run go test` → `key="go test"`. Duration/priority arguments becoming the key fails.
7. **Pair aggregation:** one `--label "go test"` row and one heuristic `go test` row produce two (`key_source`,`key`) buckets/cells. Each drilldown includes and applies both filters; querying only the spelling cannot back a singular gauge cell.
8. **launch-failed ≠ failure:** `aira time -- /nonexistent` → `status=launch-failed`, `exit_code NULL`, `wall_ms NULL`, wrapper exit **127**, excluded from failure denominator, surfaced as its own count. A permission error uses the same 127 (documented shell-126/127 conflation).
9. **Telemetry write never alters target exit:** force `AddCommandEvent` to error after target exit 0 → process still exits 0 + a stderr diagnostic. A focused core response-construction test (factor the carrier branch into a tiny helper if necessary) supplies a warning and non-nil `AfterWrite` and proves both survive alongside `Exit`.
10. **Human suppression / structured retention:** interactive `aira time -- true` writes no event JSON and no separator; `aira time --json -- true` and `aira_time` contain the outcome object. The human warning path still reaches stderr and `AfterWrite` still runs.
11. **Face-safe stdio:** Live execution receives `os.Stdin/Stdout/Stderr`; non-Live JSON and MCP execution receives a single `/dev/null` (O_RDWR) for stdin/stdout/stderr. An MCP test feeds another JSON-RPC frame after `aira_time` and proves the child did not consume it.
12. **Configured prefix seam:** with `project.Config.Run.Prefix=["whale-run"]`, no explicit prefix produces `EffectiveArgv(["whale-run"], target)`; `--no-prefix` produces the target alone and `--prefix` wins. A test fake proves runner `Launch`/`Probe` is never called.
13. **Non-Live stdio never hangs on a leaked descendant:** `aira time --json -- sh -c 'sleep 30 & exit 0'` (a backgrounded descendant inherits the child's stdout/stderr) returns promptly on the child's own exit 0 with `status=exited, exit_code=0` and a bounded `wall_ms` — it does NOT block until the descendant dies. Proves the child got a `/dev/null` `*os.File` (no `os/exec` copier-goroutine pipe) and that `WaitDelay` bounds any residual. An `io.Discard` implementation hangs (or inflates `wall_ms`) and fails this test.
14. **Git 4-state and pairing preserved:** branch X → `head_ref_status=value 'refs/heads/X'`; detached HEAD → `none` with empty value; unreadable → `unevaluated` with empty value; disagreement → `mismatch` with non-empty value. Invalid value↔status combinations fail the DB CHECK. `Field.Reason` is absent by design.
15. **DB-only:** 1000 `aira time` calls grow no git-tracked file, add zero journal entries, reserve no `RUN-n`; rows land in `command_events`; the `maxCommandEvents+1`-th insert evicts oldest by `at_seq`.
16. **No cgroup dependency:** `aira time` succeeds where cgroup delegation is absent / `backend.Probe` would fail (it never calls Probe/Create).
17. **Routed read parity + resolvable paired drilldown:** `commands ls` classifies `RouteDaemon`; daemon and in-process return identical rows; the gauge drill `commands ls key-source:<s> key:<k>` targets a real verb and the exact pair.
18. **Timeout:** `aira time --timeout 1s -- sleep 30` → `status=timeout`, `signal=KILL`, `exit_code NULL`, `wall_ms≈1000`, wrapper exit **124**; excluded from failure-rate/latency and surfaced separately. A child process spawned by `sleep` is allowed to remain, documenting the direct-child-only residual.

---

## 12. Named integration points (verified against the working tree)

**Reuse (read, do not reinvent):**
- `internal/store/compute.go` — `AddComputeEvent`/`evictComputeEvents`/`nextComputeNumbers`/`scanComputeEvent`, `optionalInt64`/`nullInt64`: the exact shape to clone.
- `internal/store/store.go` — `compute_events`/`compute_event_counter` DDL (`:806-823`); defaults (`:471-478`); `Options`/`ScopeOptions` `MaxComputeEvents…` fields (`:68`, `:91`, `:142`); additive `ALTER` precedent (`:844`).
- `internal/store/rant.go` — `gitContextFields`/`contextFromFields`/`crossCheckRantContext` (`:563-602`): reuse the daemon-scope cross-check (rename to a shared `crossCheckGitContext`), then **flatten** value+status into the `command_events` columns rather than a sibling table.
- `internal/store/store.go` — `rant_git_context` uses `CHECK((status IN ('value','mismatch') AND length(value)>0) OR (status IN ('none','unevaluated') AND value=''))`; clone that pairing for each flattened git field, not just the status vocabulary.
- `internal/store/insights.go` — `insightRegistry` (`:101`) + `init()` (`:112`); `GaugeKindDuration` (`:22`, reserved/unused); `gaugeUniverse`/`gaugeCellUnevaluated`/`GaugeCell.Fields` scaffolding; `computeFlakyRate` (`:308`, `Denominator==0 → unevaluated`) and `computeQuotaBurn` (`:361`, per-field unevaluated) as honesty precedents.
- `internal/gitcontext/resolver.go` — `Field{Value,Status,Reason}` (`:34`), `StatusValue|None|Unevaluated|Mismatch` (`:26-31`).
- `internal/runner/types.go` — `EffectiveArgv(prefix, target)` validates and joins only the supplied slices. It does **not** retrieve configuration. `internal/runner/runner_linux.go` validates `Config.Prefix` into private `Runner.prefix` in `New` and normally passes it to `EffectiveArgv` only inside `Launch`. **Do not** call `Launch`/admit/`backend.Probe`/ledger; **do not** reuse `runner.Status` (its `oom-killed`/`lost` are unreachable cgroup-less).
- `internal/core/run_wiring.go` — `wireRunCompute` (`:358-406`, `c.store.AddComputeEvent` at `:390`): the precedent carved-write posture `time` copies.

**Create / modify:**
- `internal/domain/command.go` (new) — `CommandEvent`/`CommandEventInput`/`CommandOutcome`/`Validate` (model on `domain/compute.go:56-140`).
- `internal/store/command.go` (new) — store methods (§9.2).
- `internal/core/core.go` — add `commandPrefix []string` to `Core` and `WithCommandPrefix(prefix []string) *Core`, which stores a defensive copy. Register `time` (`GitContext:true`, `MCPTool:"aira_time"`); because `cmd/aira/mcp.go:makeToolBinding` uses descriptor `Usage` as the MCP tool description, make that Usage include **"output not captured; returns the recorded command event only"**. Register the routed `commands` read descriptor. Add `commandTimingData` handling in `Do` only after handler-data/pending-callback extraction and `presentRunData`, returning `Warnings:warnings` and `AfterWrite:afterWrite` as well as `Exit`. Add `AddCommandEvent`/`ListCommandEvents`/`CommandDistribution`/`CommandLatencyByKeyPair` to `Store`. The thin exec helper selects OS stdio solely from `c.face.Live`; it never calls the runner.
- `internal/core/routing.go` — `Classify`: `time → RouteClient` (`:35`, beside `run`); `commands` stays default `RouteDaemon`. `RequiresGitContext` already keys off `descriptor.GitContext` ⇒ `time` gets stamping free. **Do not** add `time` to `StoreFreeCarved` (it writes).
- `cmd/aira/main.go` — `parseTimeArgs` (clone the `parseRunArgs` delimiter discipline with the smaller option set); add `time` to `removeJSON`'s `--`-aware branch; `buildRequest` maps target/options. Add a dedicated `if verb == "time" && !jsonOutput { return renderTime(...) }` beside the `run-log` special case. `renderTime` suppresses `Data`, reports only actual warnings/errors to stderr, runs `AfterWrite`, and honours `Response.Exit`. **Leave the `needsSeparator` condition exactly scoped to observable face-stream verbs; do not add `time`**, because an fd-inheriting child bypasses `faceStdout`'s byte tracker.
- `cmd/aira/dispatcher.go` — in **both** `dispatchCarved` and `inProcessDispatcher`, extend `FaceOutput.Live` from `(run || git) && !jsonOutput` to `(run || git || time) && !jsonOutput` (canonicalise before comparing if needed). Chain `.WithCommandPrefix(project.Config.Run.Prefix)` on the Core in both dispatcher functions and in both their normal and output-cap construction branches. Add `MaxCommandEvents`/`MaxCommandAgeDays` to `scopeFromProject` beside compute retention.
- `cmd/aira/mcp_project.go` — `runMCPWithDispatcher` passes its `input` (the JSON-RPC reader) to `newDaemonDispatcher` with `jsonOutput=true` and `stdout=io.Discard`. This is why the Live=false `time` helper must use `/dev/null` rather than `c.stdin`; never let the child consume the protocol.
- `internal/app/project.go` — `ProjectConfig.Commands CommandsConfig{MaxEvents,MaxAgeDays}` sibling of `Compute` (`:81`, `:89`); plumb to store opts (`:230`), config view (`:626`), and validation (`:482`).
- `internal/daemon` — add `MaxCommandEvents`/`MaxCommandAgeDays` to `WorktreeScope` + `ScopeOptions` (same chain as `MaxComputeEvents`).

**Rejected:** `aira exec`/`aira_exec` (spec-reserved §19); runner exec-core extraction; pretending `EffectiveArgv` can discover private runner configuration; `runner.Status` reuse (imports unreachable cgroup states); sibling git-context table (JOIN + row amplification on the hot path); status-only git CHECKs; key-only aggregation; adding `time` to `needsSeparator`; gauge-only v1 (a slowdown investigation needs the free-form `commands ls` drill).
