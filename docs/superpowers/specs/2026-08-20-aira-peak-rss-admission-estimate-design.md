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
  captured peak, the sample count, and an OOM-censor signal — an order-independent query, so
  it is immune to the ledger's randomised projection order.
- Auditable provenance: each run record states the effective reserve used and why
  (`estimate` / `fallback:*` / `disabled:*`), on new columns distinct from the runner's own
  admission wait-state.
- Honest degradation everywhere: no history, capture unavailable, censored (OOM) history,
  malformed data, read timeout/error → fall back to the configured fixed headroom; never
  block, never fabricate, never estimate from biased data.

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
- **Recency-windowed estimates** (age out old spikes / let a fixed OOM re-qualify a
  signature). v1 uses the conservative **all-history max** and treats **any** OOM in a
  signature's history as a permanent censor (safer: never under-reserves, never re-authorises
  a command that has hit the cap). A monotonic `started_at`-ordered window is a clean
  follow-up once a sortable timestamp column exists (§3 notes why `rowid` order is unusable).
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

**Order-independent aggregate.** `ledger.project` upserts from a replayed Go **map**
(`ledger.go:510`), whose iteration order is randomised, and `rebuild` re-inserts the *entire*
history in random order — so `rowid` tracks nothing useful and `ORDER BY rowid ... LIMIT N` is
**not** a recent window. v1 therefore uses an **order-independent aggregate** — `MAX(peak)` and
`COUNT` — which needs no ordering at all. A single conditional-aggregate query over the
signature index returns everything the estimator needs:

```sql
SELECT
  SUM(CASE WHEN status='exited'                          THEN 1 ELSE 0 END) AS exited_n,
  SUM(CASE WHEN status='exited' AND peak_rss IS NOT NULL THEN 1 ELSE 0 END) AS peak_n,
  MAX(CASE WHEN status='exited'                          THEN peak_rss END) AS peak_max,
  SUM(CASE WHEN status='oom-killed'                      THEN 1 ELSE 0 END) AS oom_n
FROM runs WHERE resource_signature = ?;
```

Filter rationale (verified against the code):

- **Only clean terminal exits** contribute peaks: `status='exited'` (`types.go:17`). Killed,
  cancelled, and lost runs are incomplete → their peak is a partial/unknown fraction →
  excluded. **OOM is a distinct terminal status** `'oom-killed'` (`types.go:21`), so
  `status='exited'` **already excludes it** — no separate column exists or is needed
  (`peak_rss IS NOT NULL` completes the filter).
- **`oom_n > 0` censors the signature.** Any OOM-killed run means the command has hit the cap;
  its peak is censored (a lower bound, not true demand), so estimating from the surviving
  successes would be *survivorship bias* — reassuring precisely when capacity is insufficient.
  The estimator returns `fallback:censored-oom` and does **not** estimate (§6). v1 censors on
  **any** historical OOM (conservative; recency-windowed re-qualification is deferred, §2).
- **`exited_n` vs `peak_n`** lets the estimator distinguish *capture-unavailable* (runs exist
  but every peak is nil → `+memory` not delegated) from *no-history* (no runs at all).

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
(`command.go:210-268` — `timeout`/`nice -n`/`ionice -c`/`stdbuf`/`sudo`/`env`/`whale-run`/
`nohup`/`KEY=VALUE`) into a helper `stripCommandWrappers(argv) []string` in `internal/core`.
`normaliseCommandKey` calls it then derives its coarse key (unchanged behaviour — reuses the
existing goldens); `resourceSignature` calls it then joins the **entire** stripped argv:

```go
func resourceSignature(argv []string) string // NUL-join of stripCommandWrappers(argv)
```

**Basis = the target argv (`req.Argv`) only.** The workload — not the launch wrapper —
determines memory: `go test ./...` uses the same RAM whether or not it is wrapped in
`timeout 600`. Keying on the wrapper-stripped target is both more stable (a changed prefix
never fragments history) and, critically, **`req.Argv` is exactly what the record stores as
`RunRecord.Argv`** (`runner_linux.go:350`), so if the signature ever needed re-derivation it
would be symmetric — but it does not, because core stores the computed signature directly.
The residual `stripCommandWrappers` is defensive (a target that itself leads with `env`/`sudo`).

Because core computes the signature from `req.Argv` before the run and stores it, two runs of
the same target collapse to one signature regardless of prefix, config `run.prefix`, or
worktree — exactly the equivalence class an estimate should share.

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
       ExitedCount int   // status='exited'
       PeakCount   int   // status='exited' AND peak_rss NOT NULL
       PeakMax     int64 // MAX over those; 0 when PeakCount==0
       OOMCount    int   // status='oom-killed'
   }
   type PeakRSSHistorian interface {
       PeakRSSHistory(ctx context.Context, signature string) (PeakRSSStats, bool, error)
   }
   ```

   The `bool` reports whether history was readable at all (false + nil err = DB absent →
   no-history; false + err = read failure). Backed by the §3 query over a stat-guarded,
   read-only, deadline-bounded open.

3. **Estimator (core, pure).** Maps stats → an effective reserve + a basis string, with
   checked integer arithmetic (§6). No I/O, fully table-testable.

4. **Admission wiring (core → runner).** Core's run handler builds `runner.Request` at
   `core/core.go:1452-1458`; it computes the signature, reads stats, calls the estimator, and
   sets three new `Request` fields **before the `request.Detach` branch at `:1477`**, so both
   the foreground (`c.runner.Launch`, `:1532`) and detached
   (`detacher.LaunchDetached`, `:1506` → shim → `admit` at `detach_linux.go:204`) lanes inherit
   the estimate. The runner consumes the override at the top of `admit` and stamps the record.

## 6. Estimate policy

Bias toward **not under-reserving** (an over-reserve wastes headroom; an under-reserve risks
an OOM-kill — the dangerous direction), while staying simple:

- **Censor first.** `OOMCount > 0` → `reserve = 0`, basis `fallback:censored-oom`. Never
  estimate a signature that has hit the cap.
- **Minimum samples.** Require `PeakCount >= minSamples` (default 3). Otherwise fall back with
  a *diagnostic* basis: `fallback:capture-unavailable` when `ExitedCount >= minSamples` but
  `PeakCount` is short (runs exist, peaks nil → `+memory` not delegated); else
  `fallback:insufficient-samples:n=<PeakCount>` (some captures, not yet enough); else
  `fallback:no-history` (no runs at all).
- **Conservative statistic, integer arithmetic.** `estimate = PeakMax + PeakMax*safetyPct/100`
  with `safetyPct` default 15 (≈ 1.15×). `PeakMax` (not a percentile) sizes to the worst
  observed clean run. The multiply is **overflow-guarded**: if `PeakMax > (MaxInt64-PeakMax)/…`
  would overflow, clamp to `maxEstimateReserve`. No floats.
- **Cap, don't emit an unadmittable reserve.** Clamp to `maxEstimateReserve = 1<<50`, a core
  const duplicating `daemon.admitMaxReserve` (`daemon/admit.go:25`) with a cross-reference
  comment (core cannot import daemon; the daemon still independently validates
  `reserve ∈ [0, admitMaxReserve]`, so the two agreeing is belt-and-braces, not load-bearing).
  A raw estimate above the cap → `reserve = maxEstimateReserve`, basis `estimate:capped`
  (reserving the maximum admittable is more honest than falling back to a *smaller* headroom,
  which would under-reserve a genuinely huge command). At real peak-RSS magnitudes (≤ 2^38-ish)
  the cap is never hit; it exists to bound the overflow-guarded arithmetic. Values within range
  → basis `estimate:max=<PeakMax>,n=<PeakCount>,f=115`.
- **Opt-in.** New config `run.memory_estimate` (bool, default **false**). Off → today's fixed
  reserve, feature fully inert (no signature computed, no read, no override, no basis). On →
  the above. In estimate mode `run.memory_headroom` is the **unknown-command fallback**, not a
  floor — the operator opts in to history-based sizing, so a well-characterised light command
  may reserve *less* than the blanket headroom (documented in the config docs).

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

Core sets `MemoryReserveOverride` **only** on a real estimate (`estimate:` / `estimate:capped`);
on any fallback it leaves the override nil and sets only the basis (the runner then reserves the
static headroom while the record still shows the estimate was attempted).

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

- `resource_signature TEXT` (indexed) — §4; enables the §3 query for *future* runs.
- `admission_reserve INTEGER` — the **effective reserve actually used** (override or static);
  previously unrecorded.
- `admission_reserve_basis TEXT` — the estimate basis.

The runner's own admission wait-state stays in `admission_reason`
(`immediate`/`waited`/`timeout`/`unevaluated`) — **uncontaminated**. The basis is stamped only
after a **real admission decision**, distinguishing bypass from estimate:

- `estimate:max=<bytes>,n=<count>,f=115` / `estimate:capped` — history-based reserve used.
- `fallback:censored-oom` / `fallback:capture-unavailable` /
  `fallback:insufficient-samples:n=N` / `fallback:no-history` / `fallback:read-timeout` /
  `fallback:read-error` — opted in, used the fixed headroom (reason distinguished).
- `disabled:no-admit` (`req.NoAdmit`) / `disabled:config` (no slice/reserve) — admission did
  not run; `admission_reserve` is NULL.
- `NULL` basis with a non-NULL `admission_reserve` — estimate off, static reserve used.

Provenance is best-effort: a stamp failure never fails a launch. This is the interim substitute
for the deferred §17 gauge.

## 9. Honesty and failure modes

Every failure degrades to the configured fixed reserve and continues; nothing fabricates or
blocks:

- Censored (OOM), too few captures, `+memory` not delegated (all peaks nil), DB absent,
  malformed, read timeout/`SQLITE_BUSY`/error → fixed headroom, with the distinguishing basis
  above.
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
  caps rests on the slice `OOMPolicy=kill`, as it already does; `PeakMax × safetyPct` and the
  OOM-censor bias the value toward over-reserve to stay inside that envelope. Stated, not hidden.
- **Lanes deliberately excluded.** The gate-command lane (`store/gate_command.go:77`, launches
  with `NoAdmit` unset) admits with the **static** reserve — core computes no signature for it,
  so it records `admission_reserve` = static, basis NULL. Behaviour unchanged and honest; called
  out so the exclusion is intentional, not an oversight.

## 10. Testing

- **Signature (pure):** `go test ./...` vs `go test ./internal/store` → distinct signatures;
  a strippable prefix (`timeout 600 go test ./...` via `req.Prefix`) does **not** change the
  signature (basis is `req.Argv`); `normaliseCommandKey` goldens unchanged after the
  `stripCommandWrappers` extraction.
- **Estimator (pure, table-driven):** censor precedence (`OOMCount>0` beats everything);
  `capture-unavailable` vs `insufficient-samples:n` vs `no-history` selection; `max×115%`
  rounding edge; overflow guard clamps instead of wrapping; `estimate:capped` above the cap;
  `< minSamples` → fallback; exact basis strings.
- **Aggregate reader (runner, real `runs.db` fixture):** excludes nil-peak, killed, cancelled,
  lost, and oom-killed rows from `PeakMax`/`PeakCount`; counts oom-killed into `OOMCount`;
  returns `(_, false, nil)` when `runs.db` is absent; a held write-lock trips the deadline →
  error (→ core `fallback:read-timeout`) rather than blocking; **order-independence** proven by
  inserting samples in a shuffled order and asserting `PeakMax` is unchanged.
- **Signature round-trip (real persisted record):** launch a run (foreground *and* detached),
  read the record back, assert `resource_signature` equals `resourceSignature(req.Argv)` and a
  second same-target run's aggregate sees the first — i.e. store == query, tested through the
  ledger, not a hand-built struct.
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
- **Permanent OOM censor.** A single historical OOM keeps a signature in fallback forever until
  recency-windowing lands (§2 deferral). This is the safe direction (never re-authorise a
  cap-hitting command); documented.
- **Under-estimate → OOM.** The dangerous direction. Mitigated by `PeakMax × safetyPct`, the
  clean-exit-only sample filter, OOM-censoring, and the unchanged `OOMPolicy=kill` backstop.
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
