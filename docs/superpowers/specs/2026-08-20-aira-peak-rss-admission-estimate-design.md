# AIRA peak-RSS admission estimate (#29 part 2) — design

Status: proposed
Date: 2026-08-20
Owner: AIRA
Prerequisite milestones: #29 (RAM-aware admission), D4 (daemon fairness-queue),
M16 (runner cgroup rusage-at-exit), #46 (command-telemetry key normaliser).

## 1. Motivation

`aira run` today gates admission on a **single fixed reserve** — `run.memory_headroom`,
a blanket "minimum free bytes to require" applied identically to every command
(`internal/app/project.go:572-593` → `runner.Config.MemoryReserve` → `Runner.memoryReserve`,
`internal/runner/runner_linux.go:37-39`). A one-line `go vet` and a 6 GB `pytest`
suite reserve the same number. Set the number low and heavy suites oversubscribe RAM;
set it high and light commands are needlessly serialised. Agents were observed hand-tuning
`-n` to dodge this exact thrash (the motivation that produced the CPU-worker governor, task #49).

This milestone replaces the blanket reserve, **when the operator opts in**, with a
**per-command estimate** derived from that command's own peak-RSS history: "how much has
*this* command actually used before?" Heavy commands reserve more; light commands reserve
less; more jobs pack safely into the same slice. When there is no usable history the
estimate is `unevaluated` and admission falls back to the configured fixed headroom — the
feature is **inert by default and degrades honestly**.

## 2. Scope

**In scope.**

- A per-`(project, resource-signature)` peak-RSS **estimate** computed client-side before
  admission, replacing the fixed reserve when opted in and history exists.
- A **core-computed resource signature** stored on each run record (a new indexed column) so
  history is matched by an exact stored key — symmetric by construction, no read-time argv
  reparse, and finer than the coarse command-telemetry key (a light workload's history can
  never authorise a heavier one).
- A **domain-free aggregate reader** on the runner returning, for a signature, the max
  captured peak (OOM peaks included), the sample count, the total count, and an OOM count — an
  order-independent query, so it is immune to the ledger's randomised projection order.
- Auditable provenance: each run record states the effective reserve used and why
  (`estimate` / `fallback:*` / `disabled:*`), on new columns distinct from the runner's own
  admission wait-state.
- Honest degradation everywhere: no history, capture unavailable, degenerate/malformed data,
  read timeout/error → fall back to the configured fixed headroom; an OOM history instead
  *floors* the reserve at ≥ headroom (never re-authorises a cap-hitting command below it);
  never block, never fabricate, never estimate from biased data.

**Explicitly deferred (not this milestone).**

- **Automatic `peak_rss` capture under the default ambient parent** (the AIRA-owned
  `+memory` intermediate cgroup). `peak_rss` is captured today **only where the run scope's
  parent delegates the `+memory` controller** — e.g. when `run.cgroup_parent` points at a
  memory-delegating slice such as `whale.slice`, making `.aira-RUN-N` a direct child that
  exposes `memory.peak`. Under the default ambient leaf scope (a `whale-run`/`agentmux`
  scope that *holds* the caller process), cgroup-v2's no-internal-process rule forbids
  `+memory` there and `memory.peak` is **honestly nil**
  (`internal/runner/usage_linux.go:22-45`; documented at `usage_real_test.go:48-53`,
  admission design §7, M16 design §"+memory follow-up"). Interposing an owned intermediate
  is delicate cgroup-topology work (delegation permissions, re-parenting, coexistence with
  the #33 test isolation) and is filed as a **separate future milestone**. This design needs
  **no new cgroup code**: it consumes `peak_rss` wherever the operator's configured parent
  already delegates `+memory`, and is inert where it does not.
- The §17 estimate-vs-actual insight **gauge**. Useful for operator trust, but it reads
  `state.db` while our history lives in the per-project `runs.db`; wiring a `runs.db`-backed
  gauge is separable. Deferred with the provenance recorded on each run record (§8) as the
  interim audit surface.
- Per-run `memory.max` caps (rides the same deferred `+memory` enablement).
- **Recency-windowed estimates** (age out old spikes / let a since-fixed OOM re-qualify a
  signature). v1 uses the conservative **all-history max** and lets **any** OOM in a signature's
  history floor the reserve at ≥ the OOM cap permanently (safer: never under-reserves, never
  re-authorises a cap-hitting command below its cap). A monotonic `started_at`-ordered window is
  a clean follow-up once a sortable timestamp column exists (§3 notes why `rowid` order is
  unusable).
- Changing the fixed-headroom default behaviour. The estimate is **opt-in**; with it off,
  `aira run` behaves exactly as today.

## 3. History source and query — `runs.db`, keyed by a stored signature

There are three peak-RSS-adjacent sinks; only the runner ledger `runs.db` carries a captured
peak *and* the command that produced it (`compute_events` has the peak but no argv;
`command_events` has the signature but no peak). `runs.db` is a **per-project, common-dir**
projection (`common/aira/runs/runs.db`, `ledger.go:66`), so history accumulates across every
worktree and session of the project — the correct basis for a project-wide estimate. It is a
**direct-writer file ledger by design** (the D7b honest boundary explicitly retains "the
runner run-log" as a direct writer), so reading it client-side introduces no daemon-routing
concern.

**Query by a stored signature, not a read-time argv reparse.** Each run record gains a
nullable, indexed `resource_signature TEXT` column (§4), stamped by the runner from a
core-computed value. The estimate matches history on this exact stored key. This is
**symmetric by construction** (the value stored is the value queried) and eliminates the
signature-drift risk of deriving keys from `RunRecord.Argv` at read time — which would have
been wrong anyway, because the record stores the *target* argv only, with any launch prefix in
a separate `LaunchPrefix` field (`runner_linux.go:350`, `detach_linux.go:177`) and the
config-level `run.prefix` invisible to the record. History accrues **going forward**: rows
written before this milestone have a `NULL` signature and never match — honest, no back-fill.

**Current after every run — no reconcile needed.** The runner calls `ledger.project(ctx)` at
terminalization on **every** lane (`runner_linux.go:821,1596,1612,1833`;
`detach_linux.go:647,736`), so `runs.db` reflects a completed run before the next `aira run`
reads it — the "second run learns from the first" without an intervening reconcile.

**Order-independent aggregate.** `ledger.project` upserts from a replayed Go **map**
(`ledger.go:510`), whose iteration order is randomised, and `rebuild` re-inserts the *entire*
history in random order — so `rowid` tracks nothing useful and `ORDER BY rowid ... LIMIT N` is
**not** a recent window. v1 therefore uses an **order-independent aggregate** — `MAX(peak)` and
`COUNT` — which needs no ordering at all. A single conditional-aggregate query over the
signature index returns everything the estimator needs:

```sql
SELECT
  COUNT(*)                                                                   AS total_n,
  SUM(CASE WHEN status IN ('exited','oom-killed') AND peak_rss IS NOT NULL
        THEN 1 ELSE 0 END)                                                   AS sample_n,
  MAX(CASE WHEN status IN ('exited','oom-killed') THEN peak_rss END)         AS peak_max,
  SUM(CASE WHEN status='oom-killed'               THEN 1 ELSE 0 END)         AS oom_n
FROM runs WHERE resource_signature = ?;
```

Filter rationale (verified against the code):

- **Samples are complete-lifecycle peaks.** `status='exited'` (`types.go:17`) *and*
  `status='oom-killed'` (`types.go:21`) both ran to a real terminal peak; killed, cancelled,
  and lost runs are incomplete → partial/unknown peak → excluded. No `oom_killed` *column*
  exists (OOM is encoded as the status), so the status set plus `peak_rss IS NOT NULL` is the
  complete filter.
- **OOM peaks are INCLUDED, not censored.** An `oom-killed` run's `peak_rss` is the cap it hit —
  a *lower bound* on true demand, and the strongest evidence the command is a hog. Discarding it
  (v2's censor) was survivorship bias; falling back to a possibly-*smaller* headroom would
  *re-authorise* the very command that OOM'd. So the OOM peak is folded into `peak_max`, and its
  presence (`oom_n>0`) additionally floors the reserve at the headroom (§6). `oom-killed` rows
  under a non-delegating scope have a nil peak and simply drop out — honest, no data.
- **`total_n` / `sample_n` / `oom_n`** let the estimator classify fallbacks precisely (§6):
  `total_n==0` → no runs; `sample_n==0 && total_n>0` → runs exist but no usable peak
  (capture-unavailable); `0<sample_n<minSamples` → insufficient.

**Bounded, fail-fast read.** The reader `stat`s `runs.db` first — absent (no run has ever
projected) → no history, no error. Otherwise it opens a **read-only** handle
(`?_pragma=query_only(1)&_pragma=busy_timeout(0)`) and runs the aggregate under a short
context deadline (`sampleReadTimeout`, default 250 ms). A concurrent-writer `SQLITE_BUSY`,
deadline, or any error → `fallback:read-timeout` / `fallback:read-error`; the read **never**
blocks admission (which has its own `admissionMaxWait` and must not inherit a DB-lock wait).

## 4. Resource signature — finer than the telemetry key, shared strip only

The command-telemetry key (`normaliseCommandKey`, `internal/core/command.go:208-286`) collapses
`go test ./...` and `go test ./internal/store` both to `go test`. That coarseness is fine for a
latency *view* but dangerous for resource *admission*: a light workload's history would
authorise a heavier one. The **resource signature is therefore distinct and finer** — it
retains the full stripped argv, not just `program subcommand`.

Only the **wrapper-stripping** is shared. Extract the prefix-strip loop
(`command.go:210-268`, including the all-stripped reset at :266-268 — `timeout`/`nice -n`/
`ionice -c`/`stdbuf`/`sudo`/`env`/`whale-run`/`nohup`/`KEY=VALUE`) into a helper
`stripCommandWrappers(argv) []string` in `internal/core`. `normaliseCommandKey` calls it then
derives its coarse key (unchanged behaviour — the goldens at `command_test.go:50-73` never
exercise the all-stripped path, so a verbatim extraction preserves them); `resourceSignature`
calls it then joins the **entire** stripped argv:

```go
func resourceSignature(argv []string) string // NUL-join of stripCommandWrappers(argv)
```

**Basis = the effective argv** — `commandPrefix ++ req.Prefix ++ req.Argv`, wrapper-stripped.
The config `run.prefix` is available to core as `commandPrefix` (threaded via `WithCommandPrefix`,
`core.go:420`; `dispatcher.go:226,228,445,447`) and the explicit `--prefix` as `req.Prefix`.
Innocuous wrappers (`timeout`/`nice`/…) strip out, so `timeout 600 go test ./...` and
`go test ./...` share history; but a **memory-changing** launcher that survives stripping — e.g.
`valgrind` (not in the strip set) — stays in the signature, so it can never alias a plain run.
Core computes this once and **stores** the resulting signature on the record, so store == query
is symmetric by construction regardless of how the runner later composes the launch argv (no
read-time re-derivation from `RunRecord.Argv`).

**Deliberately excluded from the signature (documented residual aliasing).** Working directory
and environment are **not** keyed, for a concrete reason: `runs.db` is per-project common-dir
and AIRA's core workflow is short-lived per-worktree runs — keying on CWD would make a fresh
worktree ignore all accumulated project history and sit permanently in fallback, defeating the
feature. The argv itself already distinguishes test *scope* (`go test ./...` vs
`go test ./internal/store` are different signatures), so the residual risk is only the *same*
argv run from a genuinely different repo position, or a resource-changing env var
(`GOMEMLIMIT=…`) that stripping erases — both rare in this workflow, both bounded by the
`safetyPct` margin and the `OOMPolicy=kill` backstop, and both cleanly addressable later by a
CWD dimension or a resource-env allowlist digest (§12). Stated, not hidden.

## 5. Component boundaries

Four units, each independently testable:

1. **Signature (core, pure).** `stripCommandWrappers` + `resourceSignature` (§4). Shares the
   strip set with `normaliseCommandKey`; table-tested, including that a differing target
   (`./...` vs `./internal/store`) yields a *different* signature.

2. **Aggregate reader (runner, domain-free).** An optional interface implemented by
   `*runner.Runner`, asserted on `c.runner` via the existing seam pattern
   (`reportMaxBytesRunner`/`auxTelemetryRunner`, `run_wiring.go:107,151`):

   ```go
   // internal/runner — opaque signature string in, plain ints out; no domain types
   type PeakRSSStats struct {
       TotalCount  int   // COUNT(*) for the signature, any status
       SampleCount int   // status IN ('exited','oom-killed') AND peak_rss NOT NULL
       PeakMax     int64 // MAX peak_rss over those samples; 0 when SampleCount==0
       OOMCount    int   // status='oom-killed'
   }
   type PeakRSSHistorian interface {
       PeakRSSHistory(ctx context.Context, signature string) (PeakRSSStats, bool, error)
   }
   ```

   The `bool` reports whether history was readable at all (false + nil err = DB absent →
   no-history; false + err = read failure). Backed by the §3 query over a stat-guarded,
   read-only, deadline-bounded open.

3. **Estimator (core, pure).** Maps stats + the configured headroom → an effective reserve + a
   basis string, with checked integer arithmetic (§6). No I/O, fully table-testable.

4. **Admission wiring (core → runner).** Core's run handler builds `runner.Request` at
   `core/core.go:1452-1458`; **only when `c.memoryEstimate` is set** (a new Core option,
   `WithMemoryEstimate(project.Config.Run.MemoryEstimate)`, mirroring `WithCommandPrefix` and
   applied at every run-dispatching construction site — `dispatcher.go:226,228,445,447`) it
   computes the signature, reads stats, calls the estimator, and sets three new `Request` fields
   **before the `request.Detach` branch at `:1477`**, so both the foreground
   (`c.runner.Launch`, `:1532`) and detached (`detacher.LaunchDetached`, `:1506` → shim → `admit`
   at `detach_linux.go:204`) lanes inherit the estimate. With the flag off, none of this runs —
   the reader is never called. The runner consumes the override at the top of `admit` and stamps
   the record.

## 6. Estimate policy

Bias toward **not under-reserving** (an over-reserve wastes headroom; an under-reserve risks
an OOM-kill — the dangerous direction), while staying simple:

- **Minimum samples, precisely classified.** Require `SampleCount >= minSamples` (default 3).
  Otherwise fall back with a *diagnostic* basis that never lies about why: `fallback:no-history`
  only when `TotalCount==0`; `fallback:capture-unavailable` when `TotalCount>0 && SampleCount==0`
  (runs exist but no usable peak — `+memory` not delegated, or only killed/cancelled/lost runs);
  `fallback:insufficient-samples:n=<SampleCount>` when `0<SampleCount<minSamples`.
- **Conservative statistic, integer arithmetic.** `estimate = PeakMax + PeakMax*safetyPct/100`
  with `safetyPct` default 15 (≈ 1.15×). `PeakMax` (the worst observed complete-run peak,
  **including any OOM-killed run's cap** — the strongest evidence of demand, §3) sizes the
  reserve. The multiply is **overflow-guarded** (if `PeakMax > (MaxInt64-PeakMax*safetyPct)`
  arithmetic would overflow, clamp to `maxEstimateReserve`). No floats.
- **A defended `estimate:` basis always means the override was applied.** If `PeakMax <= 0`
  (a degenerate stored `peak_rss` of 0) or the computed `estimate <= 0`, do **not** emit an
  override — fall back with basis `fallback:malformed`. The runner's guard is
  `*override > 0` (§7), so an `estimate:` basis must correspond to a strictly-positive reserve
  that was actually enforced; this closes the "reserve 0 → static enforced but record says
  `estimate:max=0`" mis-report.
- **OOM floors the reserve at the headroom.** When `OOMCount > 0`, the command has hit the cap,
  so reserve at least the operator's headroom: `reserve = max(headroom, estimate)`, basis
  `estimate:oom:max=<PeakMax>,n=<SampleCount>,oom=<OOMCount>,f=115`. Including the OOM peak in
  `PeakMax` already pushes the estimate up; the explicit `max(headroom, ·)` guarantees a
  cap-hitting signature is never *re-authorised* below the headroom even in a pathological
  `cap < headroom` config. (A chronic OOMer whose inflated estimate exceeds what the slice can
  hold then simply waits and fails open — bounded by `OOMPolicy=kill`, §9 — the honest outcome
  for a job that provably does not fit.)
- **Cap, don't emit an unadmittable reserve.** Clamp to `maxEstimateReserve = 1<<50`, a core
  const duplicating `daemon.admitMaxReserve` (`daemon/admit.go:25`) with a cross-reference
  comment (core cannot import daemon; the daemon still independently validates
  `reserve ∈ [0, admitMaxReserve]`, so the two agreeing is belt-and-braces). A raw estimate
  above the cap → `reserve = maxEstimateReserve`, basis `estimate:capped`. At real peak-RSS
  magnitudes (≤ 2^38-ish) the cap is never hit; it exists to bound the overflow-guarded
  arithmetic. Non-OOM values within range → basis `estimate:max=<PeakMax>,n=<SampleCount>,f=115`.
- **Opt-in.** New config `run.memory_estimate` (bool, default **false**). Off → today's fixed
  reserve, feature fully inert (no signature computed, no read, no override, no basis). On →
  the above. In estimate mode `run.memory_headroom` is the **unknown-command fallback** and the
  OOM floor, but **not** a floor for well-characterised non-OOM commands — the operator opts in
  to history-based sizing, so a light command may reserve *less* than the blanket headroom
  (documented in the config docs).

`run.memory_estimate = true` is only valid when admission is configured (`run.slice` +
`run.memory_headroom` present); otherwise `E_CONFIG_INVALID`, mirroring the existing
both-or-neither check (`project.go:575-577`). `minSamples`, `safetyPct`, `sampleReadTimeout`,
`maxEstimateReserve` are built-in constants in v1 — deliberately not new config surface.

## 7. Admission integration (every reserve read)

`Request` gains three fields (all zero-valued when the estimate is off → existing behaviour
byte-identical; all round-trip through the detach control file, whose reader and writer are the
same `/proc/self/exe` binary, so `DisallowUnknownFields` is safe):

```go
ResourceSignature     string // core-computed; runner stamps it on the record
MemoryReserveOverride *int64 // nil → use the configured static reserve
MemoryReserveBasis    string // "estimate:..." | "fallback:..." | "" (off)
```

Core sets `MemoryReserveOverride` **only** on a real, strictly-positive estimate (`estimate:` /
`estimate:oom:` / `estimate:capped`); on any fallback (including `estimate<=0`) it leaves the
override nil and sets only the basis (the runner then reserves the static headroom while the
record still shows the estimate was attempted).

`Runner.admit` (`admission_linux.go:103-120`) computes the **effective reserve once, at the
top**, and threads it into **every** reserve read on the admit path — the plan enumerates all
of them so none is missed:

```
effectiveReserve := r.memoryReserve
if req.MemoryReserveOverride != nil && *req.MemoryReserveOverride > 0 {
    effectiveReserve = *req.MemoryReserveOverride
}
```

- `:107` **disabled gate stays on the *static* config** (`r.memorySlice=="" || r.memoryReserve==0`).
  The estimate refines an already-enabled admission; it never turns admission on.
- `:159` and `:173` flock pre-lock and under-lock comparisons `max-cur >= effectiveReserve`.
- `:319` the daemon frame (`admitThroughDaemon` builds `Args{"reserve": effectiveReserve}`) —
  **no daemon change**, the daemon honours any per-frame reserve in `[0, admitMaxReserve]`.
- `:450` `noteAdmission` — the 30 s waiting diagnostic prints `reserve=%d`; it must log
  `effectiveReserve`, not `r.memoryReserve`, or the log lies about what is being enforced.

`admitWithFlock` / `admitThroughDaemon` / `noteAdmission` take `effectiveReserve` as a
parameter. `req.NoAdmit` still bypasses first (`:104`).

## 8. Provenance and auditability

Three new nullable columns on `runs`, added via the established `ensureRunColumn` lazy-migration
pattern (`ledger.go:505,520-548`) — set at **both** record-construction sites
(`runner_linux.go:350` foreground and `detach_linux.go:175-181/:217` detached):

- `resource_signature TEXT` — §4; enables the §3 query for *future* runs.
- `admission_reserve INTEGER` — the **effective reserve actually used** (override or static);
  previously unrecorded.
- `admission_reserve_basis TEXT` — the estimate basis.

**Index + upsert.** `ensureRunColumn` only adds columns; nothing today creates an index. So
`project()` (`ledger.go:485-518`), **after** the `ensureRunColumn` loop, executes
`CREATE INDEX IF NOT EXISTS runs_resource_signature ON runs(resource_signature)` — idempotent,
so existing `runs.db` files gain the index on their next projection — and the upsert statement
(`ledger.go:512`) is extended to write the three new columns (the existing lazy-column pattern
already implies this).

The runner's own admission wait-state stays in `admission_reason`
(`immediate`/`waited`/`timeout`/`unevaluated`) — **uncontaminated**. The basis is stamped only
after a **real admission decision**, distinguishing bypass from estimate:

- `estimate:max=<bytes>,n=<count>,f=115` / `estimate:oom:max=<bytes>,n=<count>,oom=<k>,f=115` /
  `estimate:capped` — history-based reserve used (a strictly-positive override was enforced).
- `fallback:capture-unavailable` / `fallback:insufficient-samples:n=N` / `fallback:no-history` /
  `fallback:malformed` / `fallback:read-timeout` / `fallback:read-error` — opted in, used the
  fixed headroom (reason distinguished; no override applied).
- `disabled:no-admit` (`req.NoAdmit`) / `disabled:config` (no slice/reserve) — admission did
  not run; `admission_reserve` is NULL.
- `NULL` basis with a non-NULL `admission_reserve` — estimate off, static reserve used.

Provenance is best-effort: a stamp failure never fails a launch. This is the interim substitute
for the deferred §17 gauge.

## 9. Honesty and failure modes

Every failure degrades to the configured fixed reserve and continues; nothing fabricates or
blocks:

- Too few captures, `+memory` not delegated (all peaks nil), DB absent, degenerate/malformed
  peak, read timeout/`SQLITE_BUSY`/error → fixed headroom, with the distinguishing basis above.
  A history containing an OOM does **not** fall back — its peak floors the reserve at ≥ headroom
  (§6), the safe direction.
- The estimate sets only the reserve **threshold**; it never removes the real-free-memory gate.
  The flock and daemon loops still poll actual `memory.current`/`memory.max` and still bound
  their wait (`admissionMaxWait`), so the estimate cannot cause an infinite block. The sample
  read has its **own** short deadline and cannot import a DB-lock wait into admission.
- **Advisory boundary (inherited from #29, unchanged).** Under the **flock daemon-down
  fallback**, the reserve is a *threshold*, not a true reservation — concurrent launches can
  each observe the same free bytes before either child ramps (#29 §I6b, "cannot reserve future
  ramp-up"). The **daemon** path is the one that maintains Σ-outstanding-reserve accounting
  (D4). The estimate changes only the reserve *value*, not this domain split, and does **not**
  require the daemon (that would break #29's deliberate daemon-down fallback). Correctness under
  caps rests on the slice `OOMPolicy=kill`, as it already does; `PeakMax × safetyPct` (with the
  OOM peak folded in and the headroom floor) biases the value toward over-reserve to stay inside
  that envelope. Stated, not hidden.
- **Lanes deliberately excluded.** The gate-command lane (`store/gate_command.go:77`, launches
  with `NoAdmit` unset) admits with the **static** reserve — core computes no signature for it,
  so it records `admission_reserve` = static, basis NULL. Behaviour unchanged and honest; called
  out so the exclusion is intentional, not an oversight.

## 10. Testing

- **Signature (pure):** `go test ./...` vs `go test ./internal/store` → distinct signatures;
  a *strippable* prefix (`timeout 600 …`) does **not** change the signature; a *non-strippable
  memory-changing* prefix (`valgrind …` via `req.Prefix`/`commandPrefix`) **does**;
  `normaliseCommandKey` goldens unchanged after the `stripCommandWrappers` extraction.
- **Estimator (pure, table-driven):** OOM handling — the OOM peak enters `PeakMax` *and*
  `OOMCount>0` floors the reserve at the headroom (`estimate:oom:` basis) and never falls to a
  smaller reserve; `no-history` (`TotalCount==0`) vs `capture-unavailable`
  (`TotalCount>0,SampleCount==0`) vs `insufficient-samples:n` selection; `max×115%` rounding
  edge; overflow guard clamps instead of wrapping; `estimate:capped` above the cap;
  `PeakMax<=0`/`estimate<=0` → `fallback:malformed` with **no** override; `< minSamples` →
  fallback; exact basis strings.
- **Aggregate reader (runner, real `runs.db` fixture):** `PeakMax` includes both `exited` and
  `oom-killed` peaks and excludes nil-peak/killed/cancelled/lost; `OOMCount` counts oom-killed;
  `TotalCount`/`SampleCount` correct; returns `(_, false, nil)` when `runs.db` is absent; a held
  write-lock trips the deadline → error (→ core `fallback:read-timeout`) rather than blocking;
  **order-independence** proven by inserting samples shuffled and asserting `PeakMax` is unchanged.
- **Signature round-trip (real persisted record):** launch a run (foreground *and* detached),
  read the record back, assert `resource_signature` equals the core-computed value and a second
  same-command run's aggregate sees the first (relying on the terminalization `project()`, §3) —
  i.e. store == query, tested through the ledger, not a hand-built struct; assert the
  `runs_resource_signature` index exists after projection.
- **Admission wiring:** with an override set, the flock (`:159`/`:173`), daemon frame (`:319`),
  and `noteAdmission` (`:450`) all use `effectiveReserve`, and the record carries
  `admission_reserve` + basis; with override nil the admit path is **byte-identical** to today;
  `:107` disabled still keys off static config; `NoAdmit` still bypasses and records
  `disabled:no-admit`; the **detached** control file carries the three fields and the detached
  record stamps them.
- **Config:** `run.memory_estimate=true` without `run.slice`/`run.memory_headroom` →
  `E_CONFIG_INVALID`; with them → accepted; default false → the reader is **not** called (assert
  no signature/read work happens).
- **Real-HW (Opus, `AIRA_REAL_CGROUP=1`):** under a `+memory`-delegating parent a real run
  captures `peak_rss`; a second same-signature run is admitted against an estimate-derived
  reserve with `estimate:` provenance; under a non-delegating parent the peak is nil → the run
  falls back with `capture-unavailable`. Confirms config-capture works end-to-end and the
  feature is inert without it.

Every confirmed review counterexample becomes a discriminating regression test, proven red
against the pre-fix code.

## 11. Risks

- **Sparse history for volatile argv.** Full-argv signatures fragment for commands whose target
  varies each run (a changing `-run` regex embedded in the target, a tmp path). Those stay in
  fallback — honest and harmless; the stable heavy commands that matter (`make test`,
  `go test ./...`, `pytest tests/…`) accumulate. Accepted, not hidden.
- **Residual aliasing (CWD / resource env).** The signature omits working directory and
  environment (§4) so project-wide history is shared across worktrees. A *same-argv* run from a
  different repo position, or one carrying a memory-changing `GOMEMLIMIT`/`NODE_OPTIONS`, can
  therefore share history with a differently-sized run. Bounded by `safetyPct` + `OOMPolicy=kill`;
  a CWD dimension / resource-env allowlist digest is the clean follow-up (§12).
- **Chronic OOMer never fits.** A signature whose only high sample is an OOM reserves ≥ that cap
  (× safety), so it may exceed what the slice can grant and then wait → time out → fail open on
  every run. That is the honest outcome for a job that provably does not fit; the added latency
  is accepted and bounded by `admissionMaxWait`. Recency-windowing (§2) lets a since-fixed leak
  re-qualify later.
- **Under-estimate → OOM.** The dangerous direction. Mitigated by `PeakMax × safetyPct`, folding
  the OOM peak into `PeakMax`, the headroom floor for OOM signatures, and the unchanged
  `OOMPolicy=kill` backstop.
- **`runs.db` read on the hot path.** One stat-guarded, deadline-bounded, read-only aggregate
  per admitted estimate-mode run; measured in the real-HW pass.
- **Operator surprise (reserving less than headroom).** Mitigated by opt-in
  (`run.memory_estimate`, default off) and documenting headroom-as-fallback-not-floor.

## 12. Deferrals (restated)

1. AIRA-owned `+memory` intermediate cgroup for zero-config capture under the ambient parent.
2. §17 estimate-vs-actual insight gauge.
3. Per-run `memory.max` caps.
4. Recency-windowed estimates + OOM re-qualification (needs a sortable `started_at`/sequence
   column; `rowid` order is unusable, §3).
5. Percentile/EWMA estimators and per-command config overrides.

Each is a clean, independently reviewable follow-up; none is a hidden gap in this milestone.
