# M17 — foreground live-tee I/O (process ↔ caller-live ↔ file)

- **Milestone:** Phase 5, second cut (after M16 rusage-at-exit).
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14 (I/O model),
  §20/§21 (phasing).
- **Depends on:** M12 runner-lite (launch + file-direct capture + `run-log`), unchanged
  capture/digest semantics.
- **Review:** Sol plan-review r1 → REVISE (2×P0 lifecycle+race, 4×P1); this is **v2** with the
  resolutions incorporated (§8 tracks each).

## 0. Context — the gap

Today a foreground `aira run -- <argv>` is **silent**: `drain()` copies each child pipe to
its capture file only (`internal/runner/runner_linux.go:707`); the caller sees nothing until
the run terminalises and it issues a separate `run-log`. §14 requires the opposite: *"Each
stream is tee'd (process ↔ caller-live ↔ file): **stdout**/**stderr** stream live … and are
captured separately"*. This cut delivers the live half for **foreground** runs, daemonless.

## 1. Scope

**In:**
- `runner.Request` gains optional per-stream **live sinks** (`LiveStdout`, `LiveStderr
  io.Writer`; nil ⇒ no tee). A `--merge` run tees the single merged stream to `LiveStdout`;
  `LiveStderr` is never used for a merged run.
- `drain()` tees captured bytes to the stream's live sink **decoupled** from the capture
  write, so a slow / non-draining live consumer can **never** stall the capture drain or, via
  pipe backpressure, the child. The **capture file stays the authoritative, complete,
  digest-covered record**; the live sink is best-effort and **accountable** (§2 I3).
- Honest **elision markers**: when the bounded live queue overflows, live bytes are dropped and
  a coalesced `[aira: N bytes elided from live view — see run-log]` marker is written to the
  **live sink only** (never the capture file). File bytes + SHA-256 are byte-identical to today.
- CLI face wires the caller's own writers as live sinks for a **human/text foreground run**;
  live-tee is suppressed under `--json` (raw child bytes would corrupt the JSON `RunRecord` on
  stdout) and always nil for the **MCP face**.
- Binary-safe (raw bytes; no normalisation on the live path).

**Out (deferred):** `--realtime`/`--pty` (M18); telemetry/gate wiring `run
--report/--ticket/--phase/--tool` (M19); `--detach` + daemon-as-supervisor (owner-decided) +
`run-input` (later). `run-log --follow` keeps its current poll-until-terminal behaviour;
genuine cross-process live tail rides on the supervisor. No new `run` flag — live-tee is the
default of a foreground human run; `--detach` (deferred) is the opt-out.

## 2. Invariants (each → a discriminating test that fails the naive `MultiWriter(file,sink)`)

- **I1 — capture completeness is independent of the live consumer.** With a live sink that
  blocks (does not drain), the child still runs to normal exit and the capture file is
  byte-complete *while the sink is still blocked* — proven by the child touching a `DONE`
  sentinel after its last write and the test observing `DONE` within a short timeout. (Against
  `MultiWriter` the child deadlocks on pipe backpressure, `DONE` never appears → test fails.)
- **I2 — capture bytes + digest unchanged vs. no-tee.** Same argv with `LiveStdout==nil` and
  with any live sink produce byte-identical capture files and identical `OutputRef.Digest`. The
  elision marker never enters the capture path.
- **I3 — live accountability (not zero-loss).** Live bytes written to a sink are an **in-order
  subsequence** of the child's bytes, and `Σ(live bytes) + Σ(marker elided counts) == total
  captured bytes`. This holds deterministically under any scheduling. **Zero elision is only
  asserted under a lock-step sink** that accepts each chunk before the next is produced (a
  bounded non-blocking queue may legitimately elide during a producer burst even when the sink
  is ultimately fast — so "fast sink ⇒ no marker" is *not* an invariant).
- **I4 — no post-return live writes on the normal path.** When all drains reach EOF within the
  capture grace, `Launch` joins every live-writer goroutine before returning; therefore no child
  byte lands on the caller's sink after the run summary. On the **forced-close path** (§3.3,
  grace expired — already `CaptureForcedClosed`) at most **one in-flight chunk** may still reach
  the caller after return; this is bounded, confined to that abnormal condition, and documented.
- **I5 — race-free accounting.** All elision accounting is producer-local; the writer only reads
  fields off received values. `go test -race` on the runner package is clean.
- **I6 — merge fidelity.** A `--merge` run tees the single kernel-ordered merged stream to
  `LiveStdout`; a stderr sink receives nothing.

## 3. Design

### 3.1 Decoupling — bounded channel + one writer goroutine per stream

`drain(name, rd, dst, live)` keeps its **capture** path exactly as today: read a chunk →
`writeAll(dst, chunk)` (authoritative, blocking-until-written-or-fail) → hash → count. **New:**
after the successful capture write, hand the live path a value on a **bounded channel**
`chan liveChunk` (`liveChunk{ data []byte; droppedBefore int64 }`, capacity a small constant
`liveQueueDepth`, default 64, overridable by tests via an unexported var). A dedicated
**live-writer goroutine** per stream ranges the channel and writes to `live`.

- **Copy before send** — `buf` is reused each iteration, so the value carries
  `append([]byte(nil), buf[:n]...)`, never `buf[:n]` (aliasing bug otherwise).
- **Non-blocking send + producer-local accounting** — the drain holds a local `pendingDropped
  int64`. Send is `select { case ch <- liveChunk{cp, pendingDropped}: pendingDropped = 0;
  default: pendingDropped += int64(n) }`. The capture loop **never** blocks on the live path,
  and only the producer touches `pendingDropped` (no shared counter → I5).
- **Trailing drop** — after the read loop ends, if `pendingDropped > 0` the drain sends a final
  `liveChunk{nil, pendingDropped}` (best-effort, non-blocking is fine since the writer is
  draining) then `close(ch)`.
- **Writer** — for each received `liveChunk`: if `droppedBefore > 0`, `writeAll(live, marker(
  droppedBefore))`; if `len(data) > 0`, `writeAll(live, data)`. **`writeAll`** (handles legal
  short writes with nil error — P1). On a live write **error** (e.g. `EPIPE`) or after the gate
  is disabled (§3.3), set a local `off` flag and stop writing, but **keep draining the channel**
  so the producer's non-blocking send path never wedges. Marker text is the fixed greppable
  string above.
- **Ordering** — capture-write precedes live-send for the same chunk, the channel is FIFO, and a
  single writer serialises the sink; so the live view is a monotonic in-order subsequence with
  each gap's marker emitted immediately before the next surviving chunk (I3).

### 3.2 Lifecycle / join

`Launch` already spawns the drains (`:241`) and barriers on `collectCapture` (`:316`).

- **Normal path** (`collectCapture` returns `forced == false`, i.e. every drain EOF'd and closed
  its live channel): `Launch` **joins all live-writer goroutines** (a `sync.WaitGroup`) before
  returning. Guarantees I4 (no post-return writes). The join can block only if the caller's own
  sink is wedged — this is correct foreground backpressure (identical to `foo | tee f` when the
  reader stops) and is releasable / interruptible by `ctx` cancellation (timeout, Ctrl-C), which
  is teardown.
- **Forced-close path** (`forced == true`, grace expired because a descendant holds a pipe open
  so a drain cannot EOF — pre-existing M12 leak): `Launch` does **not** block on the live writers.
  Before returning it **disables the live gate** (an atomic flag on the sink wrapper) so all
  *subsequent* live writes become no-ops; at most the one already-in-flight chunk may still land
  (I4 forced-close clause). This confines the residual to the already-abnormal
  `CaptureForcedClosed` condition and matches the existing leaked-drain semantics.

### 3.3 Live-sink gate (bounds the forced-close residual)

Each live sink is wrapped in a tiny runner-owned `liveGate{ w io.Writer; off atomic.Bool }`.
The writer goroutine calls `gate.write(p)` = `if gate.off.Load() { return }` then `writeAll(w,
p)`. `Launch`'s forced-close path calls `gate.disable()` (sets `off`) on each gate. This never
blocks (no mutex around the in-flight write — a mutex would re-introduce the wedged-sink hang);
the bound is "≤1 in-flight chunk after disable", which is the honest, achievable guarantee for
an arbitrary `io.Writer`.

### 3.4 Request / face wiring (immutable at construction — P1)

- `runner.Request`: add `LiveStdout io.Writer`, `LiveStderr io.Writer`.
- `internal/core`: add an **immutable** face-output value set once at construction, e.g.
  `FaceOutput{ Stdout, Stderr io.Writer; Live bool }`, via a new constructor
  `NewWithRunnerFace(s, runner, stdin, FaceOutput)` (existing constructors default it to the
  zero value — `Live=false`, nil writers → no tee, so MCP/tests are unchanged). The `"run"`
  handler sets `request.LiveStdout/LiveStderr` **only when `FaceOutput.Live`** (merged run →
  `LiveStdout = FaceOutput.Stdout`, `LiveStderr` nil; separate → both). **No per-request Core
  mutation.**
- CLI face (`cmd/aira/main.go`): the CLI already parses `--json` before constructing the Core, so
  it constructs with `FaceOutput{Stdout: <run stdout>, Stderr: <stderr>, Live: verb=="run" &&
  !json}`. `--json` ⇒ `Live=false` (both streams suppressed; stdout carries only the JSON
  record). Text mode: child stdout/stderr stream live, then the summary renders; if the child's
  final byte was not `\n`, emit a `\n` separator before the summary (the writer tracks the last
  byte, or the face unconditionally ensures a leading newline for the summary block).
- MCP face: constructs with `Live=false`; `run` never streams; bytes only via `aira_run_output`.

### 3.5 What does NOT change

Capture files, `OutputRef` paths/bytes/digest/state, `ReadOutput`/`run-log`, `collectCapture`
grace/partial semantics, kill/scope/rusage/ledger. The live path is additive and side-effect-free
on the durable record.

## 4. Tests

Runner-level (`internal/runner`, no hardware dependence — sink fakes: `lockStepSink`,
`blockingSink` (releasable), `shortWriteSink`, `errorSink`, `bytes.Buffer`):
- **T1 (I1 discriminator)** — `blockingSink` as `LiveStdout`; child writes ≫ `liveQueueDepth`×chunk
  then touches a `DONE` file and exits 0. Run `Launch` in a goroutine; assert `DONE` appears within
  a short timeout **and** the capture file at the known path is byte-complete *while the sink is
  still blocked*; then release the sink, join, assert `exited`/0 and digest == hash(expected).
  *Fails against `MultiWriter`.*
- **T2 (I2)** — argv run A `LiveStdout=nil`, run B `bytes.Buffer`; capture files byte-equal +
  digests equal.
- **T3 (I3 accountability)** — ~1 MiB mixed binary through (a) a `lockStepSink` → assert **zero**
  markers and exact in-order bytes; (b) a bursty sink → assert accountability
  (received + Σelided == total) and in-order subsequence. Binary-safe.
- **T4 (elision + marker placement)** — forced overflow via a gated slow sink; assert ≥1 marker with
  a non-zero count, drop/accept/drop sequence places each marker immediately before the right
  surviving chunk, and the capture file is complete (marker only on the live path).
- **T5 (I6 merge)** — `--merge`: merged sink gets the merged stream; a stderr sink gets nothing.
- **T6 (short write + error)** — `shortWriteSink` delivers all bytes (writeAll); `errorSink`
  (returns EPIPE mid-run) does not stall the child and the capture stays complete.
- **T7 (I5)** — `go test -race ./internal/runner/...` clean (drop/accept interleavings).
- **T8 (I4 forced-close)** — descendant holds the pipe open past grace + a blocked sink: assert
  `Launch` returns (does not hang on the live writer), `CaptureForcedClosed` set, and the gate is
  disabled (no unbounded post-return live writes beyond one chunk).

Face-level (`internal/core` / `cmd/aira`):
- **T9** — CLI `run` text mode tees child stdout to face stdout, then the summary (with separator
  when needed); `--json` emits a clean parseable `RunRecord`, **no** child bytes on stdout.
- **T10** — MCP `aira_run` never streams; bytes only via `aira_run_output`.

Real-process e2e (Opus, `whale-run`): `aira run -- sh -c 'printf out; printf err 1>&2'` shows both
live; `--json` clean; a real slow-consumer harness confirms I1 on the built binary.

## 5. Files

- `internal/runner/types.go` — `Request.LiveStdout/LiveStderr`.
- `internal/runner/runner_linux.go` — `drain` live path; `liveChunk`, `liveGate`, marker helper,
  writer goroutine; `Launch` wiring (pass sinks, `WaitGroup` join on normal path, gate-disable on
  forced path); `liveQueueDepth` var.
- `internal/runner/livetee_test.go` (new) — T1–T8 + sink fakes.
- `internal/core/core.go` — `FaceOutput` + `NewWithRunnerFace`; `"run"` handler sink wiring.
- `cmd/aira/main.go` — construct Core with `FaceOutput{…, Live: run && !json}`; text separator.
- `internal/core/*_test.go`, `cmd/aira/*_test.go` — T9/T10; update existing text-mode `run` face
  tests that now also observe child output.
- Non-linux build: `drain`/`Request`/`liveGate` are OS-agnostic (shared file); confirm
  `cgroup_stub.go` path still compiles.

## 6. Risks / accepted degradations

- **Forced-close in-flight chunk (I4 clause).** ≤1 chunk may reach the caller after return on the
  abnormal `CaptureForcedClosed` path. Bounded, documented; a deadline-bounded refinement is a
  candidate M18 item, not needed now.
- **Foreground backpressure on return.** On the normal path, `aira run` return blocks iff the
  caller wedges its *own* stdout — correct `tee` semantics, ctx-cancellable. An autonomous agent
  that stops reading has abandoned the run; the capture file is still complete.
- **Default-on behaviour change.** Text-mode `run` now prints child output before the summary;
  existing text-mode `run` face tests are updated. `--json` consumers unaffected. Yield: the
  headline live-observability feature with zero change to the durable record.

## 7. Deferrals (filed)

`--realtime`/`--pty` (M18); telemetry/gate wiring (M19); detach + daemon-supervisor + `run-input`
(later); genuine cross-process `run-log --follow` (rides the supervisor); deadline-bounded
live-writer abandon (I4 hardening, candidate M18).

## 8. Sol plan-review r1 resolutions

- **P0 lifecycle** → §3.2/§3.3: normal path joins unconditionally (I4, zero post-return writes);
  forced-close disables a non-blocking gate (≤1 in-flight chunk). No lingering-goroutine "accepted
  degradation"; T1 reworked to a `DONE`-sentinel discriminator (no permanently-wedged-then-return
  contradiction); T8 covers forced-close+blocked-sink.
- **P0 elision race** → §3.1: producer-local `pendingDropped`, channel-carried `droppedBefore`,
  trailing sentinel; I5 + T7 (`-race`).
- **P1 forced/canceled capture path** → §3.2 forced-close branch + T8.
- **P1 writeAll on live path** → §3.1 writer + T6.
- **P1 "fast sink" not lossless** → I3 accountability + T3 (lock-step vs bursty).
- **P1 core wiring** → §3.4 immutable `FaceOutput`/`NewWithRunnerFace`, no per-request mutation.
- **P2 tests/faces** → T4 marker placement, T5 merge single-path, T9 `--json` suppresses both +
  text separator, T10 MCP.
