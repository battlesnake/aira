---
{"schema":1,"id":"AIRA-259","project":"aira","title":"aitest: emit a per-test dispatch/timing/RSS event stream (Chrome trace format) for an interactive Gantt of a confined run","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["aitest","telemetry"],"hold":true,"relations":[]}
---

HELD pending owner build-prioritisation. Requested by deploy (first consumer) to read
gate-scaling / DoD-curve runs as a picture. Design scoped with deploy 2026-09-16; this
body is the durable capture. NOT for the imminent 16/32/64-vCPU scaling run (that uses
tables + the coarse timeline); the Gantt's value is across the whole gate-scaling
program.

## Problem

Reading a confined/aitest run's inefficiency — worker occupancy, serial poles,
admission (RAM-gated queue) waits, RSS heat — lands far better as an interactive
timeline than a table. The compelling before/after: a monolithic corpus scan is ONE
fat bar hogging a lane while others idle; after parametrisation (deploy's BL-1251) it
is a CLOUD of short bars filling the lanes. The Gantt makes the parametrisation payoff
and the residual serial poles visible at a glance.

## Core deliverable (aira-owned): a per-test dispatch event stream

Aira owns the DATA + concurrency semantics; the picture is a commodity view (see
Renderer). Today aitest emits NONE of this — verified in supervisor.py: the only
durable artifact is `pool-report.json` (opt-in `AIRA_AITEST_MEASURE_DIR`, AIRA-230
reserve-adequacy gauge) which carries per-WORKER peak RSS + OOM + granted scope-path +
aggregate budget, with NO timestamps, NO per-test records, NO worker lane. The
worker→supervisor wire protocol (`<nodeid> <outcome>` + `\x01<json>` event lines,
worker.py) is pytest-outcome/junit-fidelity only and is ephemeral IPC. So this is NEW
instrumentation, not a renderer over existing data.

Emit, per test attempt, a Chrome Trace Event JSON record (Perfetto-loadable; also the
input to the thin HTML renderer below):
- **nodeid** (test id) and a **suite/file tag** (deploy's ask — lets the renderer
  colour "is this a corpus-scan pole" vs ordinary tests; the payoff view depends on it).
- **worker lane** (worker pid/slot) — one horizontal lane per worker.
- **admitted-at, started-at, ended-at** (monotonic + wall clock). The queued /
  admission-wait prefix maps to systemd-plot's light "activating" phase (a `B`/duration
  split or two spans), the running span to dark "active" — so RAM-gated admission
  latency shows up visually as the light lead-in, which IS the contention insight.
- **declared RSS** (`reservation_need[nodeid]`).
- **peak RSS** — LOCAL (real-cgroup) enrichment only, from the per-worker
  `_pool_peak_records` reads at retirement.

The single-threaded dispatch loop already has every field at its event point: the
grant in `_start_worker` = admitted; `state["dispatch_write"].write(nodeid)` +
`state["in_flight"]=nodeid` = started; the drained result line = ended;
`reservation_need` = declared RSS; `_pool_peak_records` = peak RSS. The NEW work is
timestamping those points and buffering a record — the values are already computed.

## Hard constraints (locked with deploy)

1. **Non-blocking.** The dispatch loop is single-threaded and latency-critical (a
   wedge there stops the whole pool). Buffer events in memory and flush ONCE at end —
   exactly like `_pool_peak_records` → `pool-report`. Never fsync per-event on the loop.
2. **Opt-in + fail-open.** Gated by an env (extend `AIRA_AITEST_MEASURE_DIR` or a
   sibling); a normal run pays nothing; a telemetry write that fails NEVER reds a suite.
3. **Honest RSS.** On the GCP ci-shim advisory path (no cgroup) peak RSS is
   `unevaluated`, NEVER a fabricated 0 (a 0 peak makes a pool look infinitely
   over-provisioned). The renderer draws unevaluated RSS as absent/greyed. admit / start
   / end / lane / declared-RSS + Tier 1 work on BOTH lanes; peak-RSS heat is
   local-cgroup only.
4. **Retry-once double-bar.** A crash-retried nodeid (Task 15, `self.attempts`) emits
   BOTH attempts as separate bars — a FEATURE: the Gantt should reveal a crash-retry,
   not hide it. Include an attempt index.

## Follow-ons (separate tasks; not this ticket's core)

- **Thin self-contained HTML renderer** over the same stream: one file, embedded JSONL
  + vis-timeline/d3, colour by suite/status, hover tooltips, no server (double-click;
  good as a CI artifact). The owner's constraint: many short elements, no room for
  per-bar labels → interactive colour + hover, not a static labelled SVG. Perfetto
  already renders the trace-format stream for free (lanes/colour/hover/zoom) — the thin
  HTML covers the offline case.
- **Gate-side Tier 1 is DEPLOY's, not aira's:** the coarse phase/leg timeline
  (`phase-timing.tsv`, leg pytest summaries) is gate-harness data. deploy emits it into
  the SAME trace format so phases + legs + per-test worker-lanes compose into one
  Perfetto view. Aira stays OUT of parsing gate-side TSV/log formats.

## Why simple (challenge pass)

The primitive worth owning is the event stream, not a plotting subsystem — emitting a
standard trace format hands the interactive Gantt to Perfetto for free and keeps aira's
"primitives, not judgement / stay simple" rule intact. Building a bespoke plotter
inside a coordination tool would be the over-design to avoid.

## Process

New instrumentation touching the aitest dispatch loop → correctness-adjacent, wants the
brainstorm → two-loop (Opus builds, Fable reviews) care. Owner greenlight required
before build (deploy is surfacing prioritisation). First consumer: deploy, who will
feed back what the analysis actually needs to see.

