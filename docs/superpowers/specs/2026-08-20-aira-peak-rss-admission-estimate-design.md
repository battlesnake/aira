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

- A per-`(project, command-signature)` peak-RSS **estimate** computed client-side before
  admission, replacing the fixed reserve when opted in and history exists.
- A **domain-free run-sample reader** on the runner exposing captured peak-RSS + argv from
  the per-project `runs.db` ledger.
- Reuse of the #46 command-signature normaliser so the estimate keys commands with the
  **same vocabulary** as command-telemetry.
- Auditable provenance: the run record states which reserve was used and why
  (`estimate` vs `fallback` vs `disabled`).
- Honest degradation everywhere: no history, capture disabled, malformed data, or read
  error → fall back to the configured fixed headroom; never block, never fabricate.

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
- Changing the fixed-headroom default behaviour. The estimate is **opt-in**; with it off,
  `aira run` behaves exactly as today.

## 3. Where `peak_rss` history lives, and why `runs.db`

There are three peak-RSS-adjacent sinks; only one carries **both** a captured peak and the
command that produced it:

| Sink | Store | Has `peak_rss`? | Has argv? | Keyed by |
|---|---|---|---|---|
| runner ledger `runs.db` | per-project common-dir file DB | yes (`runs.peak_rss`, `ledger.go:505`) | yes (`RunRecord.Argv`, `types.go:91`, in `record_json`) | run id |
| `compute_events` | `state.db` | yes | **no** | ticket/phase/model |
| `command_events` (#46) | `state.db` | **no** | signature only | `(key_source,key)` |

`runs.db` is therefore the source. It is a **per-project, common-dir** projection
(`common/aira/runs/runs.db`), so history accumulates across every worktree and session of
the project — the correct basis for a project-wide estimate. It is a **direct-writer file
ledger by design** (the D7b honest boundary explicitly retains "the runner run-log" as a
direct writer), so reading it client-side introduces no daemon-routing concern.

Sampling rules (honest inputs only):

- **Only clean terminal exits.** Include a run's peak only if it reached `status = exited`.
  Exclude non-terminal, killed, and **OOM-killed** runs: an OOM-killed run's `peak_rss` is
  the cap it hit, which *understates* true demand and would bias the estimate low.
- **Only real captures.** Include only rows with non-nil `peak_rss`. Where `+memory` is not
  delegated every row is nil → zero samples → fallback. The feature is inert exactly where
  capture does not happen.
- **Bounded, recent window.** Read at most the most recent `historyWindow` rows
  (`ORDER BY rowid DESC LIMIT historyWindow`, default 200), normalise argv in Go, and keep
  those whose signature matches the current command. This bounds the per-run read and lets
  stale spikes age out. No schema change; no stored signature column (signatures are derived
  at read time from `record_json.argv`).

## 4. Command signature — reuse the #46 normaliser (layering)

The current command signature is `normaliseCommandKey(argv, label)`
(`internal/core/command.go:208-286`): it strips wrapper prefixes (`timeout`, `nice -n`,
`ionice -c`, `stdbuf`, `sudo`, `env`, `whale-run`, `nohup`, `KEY=VALUE`) and derives a stable
key — `program subcommand` for known drivers (`go/cargo/git/make/npm/pytest/...`), else bare
`program`, or an explicit label. It returns a `domain.CommandKeySource`.

**Layering constraint (decisive).** The runner imports **neither `core` nor `domain`**
(verified: no such imports in `internal/runner/*.go`); it is a deliberately decoupled leaf
that speaks its own `RunRecord`/`Request`/`Config` types, and `core` translates runner
results into domain events. Therefore the estimate is computed in **`core`, above the
runner**, where `normaliseCommandKey` and `domain` are already in scope. The runner is **not**
coupled to `domain` for this feature.

Consequence: the normaliser stays exactly where it is (single source of truth, shared with
command-telemetry). Core computes the current command's signature from the same argv basis
the run uses — `EffectiveArgv(req.Prefix, req.Argv)` (`types.go:362-373`) — and matches it
against signatures computed from each historical `RunRecord.Argv`. Both sides normalise the
**same way**, so a command logged yesterday and launched today collapse to one key.

## 5. Component boundaries

Three units, each independently testable:

1. **Runner sample reader (domain-free).** A new optional interface implemented by
   `*runner.Runner`:

   ```go
   // internal/runner (domain-free; argv + captured peak only)
   type PeakRSSSample struct {
       Argv    []string
       PeakRSS int64 // bytes; only clean-exit, non-nil, non-OOM rows are returned
   }
   type PeakRSSSampler interface {
       PeakRSSSamples(ctx context.Context) ([]PeakRSSSample, error)
   }
   ```

   Backed by a bounded `SELECT argv-from-record_json, peak_rss FROM runs WHERE status='exited'
   AND peak_rss IS NOT NULL AND <not oom-killed> ORDER BY rowid DESC LIMIT historyWindow`
   over `runs.db` (a read-only projection open; the ledger already builds `runs.db` via
   `ledger.project`). Returns `([], nil)` when the DB or column is absent — never an error for
   "no data".

2. **Estimator (core, pure).** Given the samples, the current signature, and the fixed
   headroom, returns an **effective reserve** and a **provenance string**:

   ```go
   // internal/core (pure; no I/O)
   func estimateReserve(samples []runner.PeakRSSSample, sig string, headroom int64,
       policy estimatePolicy) (reserve int64, provenance string)
   ```

   Policy (built-in defaults; §6). Pure and table-testable with no cgroup or DB. On a
   fallback (`< minSamples`, empty, or read error) it returns `reserve = 0` and a `fallback:*`
   provenance; core then leaves the override **nil** (the runner uses the static reserve)
   while still carrying the provenance onto the record.

3. **Admission wiring (core → runner).** Core's run entrypoint (`core/core.go:1452` builds
   `runner.Request`, `:1532` calls `c.runner.Launch`) computes the signature, reads samples
   via the optional interface, calls `estimateReserve`, and populates two new `Request`
   fields — a numeric override and a short basis string (§7, §8). The runner consumes the
   override at the top of `admit` and stamps the effective reserve + basis onto the run record.

## 6. Estimate policy

Bias toward **not under-reserving** (an over-reserve merely wastes headroom; an under-reserve
risks an OOM-kill — the dangerous direction), while staying simple enough to reason about:

- **Minimum samples.** Require at least `minSamples` matching clean-exit captures (default 3).
  Fewer → `unevaluated` → fallback. Prevents one noisy run from setting policy.
- **Conservative statistic.** `estimate = ceil(safetyFactor × max(peak_rss over the window))`.
  `max` (not a percentile) answers "will it fit" against the worst recently observed run;
  `safetyFactor` (default 1.15) absorbs modest growth. The bounded recent window (§3) ages
  out stale spikes so a single historical anomaly does not inflate forever.
- **No floor, no local cap.** The estimate stands on its own; the existing daemon bound
  `reserve ∈ [0, admitMaxReserve]` (`internal/daemon/admit.go`) still applies, and the flock
  and daemon paths still gate on **real** free memory. A modest under-estimate is within the
  advisory envelope #29 already accepts (bounded by the slice `OOMPolicy=kill`); `max ×
  safetyFactor` biases toward over-estimate to keep well inside it.
- **Opt-in.** New config `run.memory_estimate` (bool, default **false**). Off → today's fixed
  reserve, feature fully inert (no sample read, no override). On → use the estimate when
  `>= minSamples` history exists; otherwise fall back to `run.memory_headroom`. In
  estimate mode the headroom is the **unknown-command fallback**, not a floor — the operator
  opts in to history-based sizing, so a well-characterised light command may reserve *less*
  than the blanket headroom. This is stated in the config docs.

`run.memory_estimate` may be set only when admission is already configured (`run.slice` +
`run.memory_headroom` present); `run.memory_estimate = true` without them is
`E_CONFIG_INVALID` (mirrors the existing both-or-neither check at `project.go:575-577`).
Built-in policy constants (`minSamples`, `safetyFactor`, `historyWindow`) are not new config
surface in v1 — deliberately minimal, exposed later only if needed.

## 7. Admission integration (both paths)

`Request` gains two fields (both populated by core; both zero-valued when the estimate is
off, keeping existing behaviour byte-identical):

```go
MemoryReserveOverride *int64 // nil → use the configured static reserve
MemoryReserveBasis    string // "estimate:..." | "fallback:no-history" | "fallback:read-error" | ""
```

Core sets `MemoryReserveOverride` **only** when it has a real estimate (`>= minSamples`); on
any fallback it leaves the override nil and sets only the basis. `Runner.admit`
(`internal/runner/admission_linux.go:103-120`) computes the **effective reserve once, at the
top**, and threads it into both admission paths:

```
effectiveReserve := r.memoryReserve
if req.MemoryReserveOverride != nil && *req.MemoryReserveOverride > 0 {
    effectiveReserve = *req.MemoryReserveOverride
}
```

- The **`disabled` gate is unchanged** and stays on the *static* config
  (`r.memorySlice == "" || r.memoryReserve == 0` → `disabled`). The estimate **refines** an
  already-enabled admission; it never turns admission on. If the operator has not configured
  admission, no override is ever computed.
- `admitThroughDaemon` builds its frame with `effectiveReserve` (not `r.memoryReserve`), so
  the daemon fairness-queue reserves the estimated amount. **No daemon change** — the daemon
  honours whatever reserve it receives (admission design §7 already anticipates this;
  D4 lists "peak-RSS estimation (#29 part 2)" as client-side future work).
- `admitWithFlock` compares `max - cur >= effectiveReserve` at both the pre-lock and
  under-lock rechecks (replacing the two `r.memoryReserve` reads at `admission_linux.go:159,
  173`).

`req.NoAdmit` still bypasses everything first (`admission_linux.go:104`).

## 8. Provenance and auditability

The runner's own admission wait-state stays in the existing `admission_reason` column
(`immediate`/`waited`/`timeout`/`unevaluated`) — **uncontaminated**. The estimate decision is
recorded independently on **two new nullable columns** added via the established
`ensureRunColumn` lazy-migration pattern (`ledger.go:505` already grows the schema this way):

- `admission_reserve INTEGER` — the **effective reserve in bytes** actually used (override or
  static). Lets an audit see what each run reserved; previously unrecorded.
- `admission_reserve_basis TEXT` — the estimate basis, from `Request.MemoryReserveBasis`:
  - `estimate:max=<bytes>,n=<count>,f=<safetyFactor>` — history-based reserve used.
  - `fallback:no-history` — opted in, `< minSamples`, used fixed headroom.
  - `fallback:read-error` — opted in, sample read failed, used fixed headroom.
  - `NULL`/empty — estimate off; unchanged behaviour.

Both are set by the runner from `Request` fields core populates, so the runner stays
domain-free and `admission_reason` keeps its existing meaning. This provenance is the interim
substitute for the deferred §17 gauge. It never blocks and is best-effort: a provenance write
must not fail a launch.

## 9. Honesty and failure modes

Every failure degrades to the configured fixed reserve and continues:

- Sample read error, malformed `record_json`, missing `runs.db`/column → `[]` samples →
  fallback (`estimate-fallback:read-error` on genuine error, `no-history` on empty).
- `+memory` not delegated (every peak nil) → zero samples → fallback → feature inert.
- Estimate `<= 0` (defensive) → fallback.
- The estimate **never** removes the real-free-memory gate; it only sets the reserve
  *threshold*. The flock/daemon loops still poll real `memory.current`/`memory.max` and still
  bound their wait (`admissionMaxWait`, timeout path unchanged) — the estimate cannot cause an
  infinite block.
- Advisory boundary is unchanged from #29: correctness under caps is enforced by the slice
  `OOMPolicy=kill`, not by the reserve arithmetic; the estimate biases toward over-reserve to
  stay inside that envelope.

## 10. Testing

- **Estimator (pure, table-driven):** min-samples gate; `max × safetyFactor` with a rounding
  edge; window truncation ages out an old spike; OOM/killed/non-terminal rows excluded;
  empty → fallback; sub-`minSamples` → fallback; provenance strings exact.
- **Signature parity:** the same command logged (via `RunRecord.Argv`) and launched (via
  `EffectiveArgv(Prefix, Argv)`) collapse to one key across the full wrapper-strip set; a
  different subcommand does not. Reuses/extends the #46 goldens.
- **Sample reader (runner, real SQLite `runs.db` fixture):** bounded `LIMIT`; excludes
  nil-peak, OOM, and non-exited rows; returns `[]` (not error) on absent DB/column.
- **Admission wiring:** with an override set, both the flock pre-lock and under-lock
  comparisons and the daemon frame use `effectiveReserve`, and the record carries
  `admission_reserve` (bytes) + `admission_reserve_basis`; with override nil the admission
  path is byte-identical to today; `disabled` still keys off static config; `NoAdmit` still
  bypasses.
- **Config:** `run.memory_estimate = true` without `run.slice`/`run.memory_headroom` →
  `E_CONFIG_INVALID`; with them → accepted; default false → no sample read (assert the reader
  is not called).
- **Real-HW (Opus, `AIRA_REAL_CGROUP=1`):** under a `+memory`-delegating parent, a real run
  captures `peak_rss`; a second run of the same signature is admitted against an
  estimate-derived reserve with `estimate:` provenance; under a non-delegating parent the
  peak is nil and the run falls back with `no-history`. Confirms end-to-end that "config
  capture" works and that the feature is inert without it.

Every confirmed review counterexample becomes a discriminating regression test, proven red
against the pre-fix code.

## 11. Risks

- **Signature drift.** If launch-time and record-time argv bases diverge, history never
  matches and the estimate is silently always-fallback. Mitigated by the explicit
  signature-parity test (§10) using the exact `EffectiveArgv`/`Argv` bases.
- **`runs.db` read on the hot path.** One bounded read per admitted run. Mitigated by the
  `LIMIT historyWindow` and a read-only open; measured in the real-HW pass.
- **Under-estimate → OOM.** The dangerous direction. Mitigated by `max × safetyFactor`, the
  clean-exit/no-OOM sample filter, and the unchanged `OOMPolicy=kill` backstop; stated as an
  accepted advisory boundary, not hidden.
- **Operator surprise (reserving less than headroom).** Mitigated by making the whole feature
  **opt-in** (`run.memory_estimate`, default off) and documenting that in estimate mode the
  headroom is the fallback, not a floor.

## 12. Deferrals (restated)

1. AIRA-owned `+memory` intermediate cgroup for zero-config capture under the ambient parent.
2. §17 estimate-vs-actual insight gauge.
3. Per-run `memory.max` caps.
4. Percentile/EWMA estimators, per-command config overrides, a stored signature column with an
   index (only if the bounded scan proves insufficient).

Each is a clean, independently reviewable follow-up; none is a hidden gap in this milestone.
