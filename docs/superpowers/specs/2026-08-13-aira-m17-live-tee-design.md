# M17 — foreground live-tee I/O (process ↔ caller-live ↔ file)

- **Milestone:** Phase 5, second cut (after M16 rusage-at-exit).
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14 (I/O model),
  §20/§21 (phasing).
- **Depends on:** M12 runner-lite (launch + file-direct capture + `run-log`), unchanged
  capture/digest semantics.

## 0. Context — the gap

Today a foreground `aira run -- <argv>` is **silent**: `drain()` copies each child
pipe to its capture file only (`internal/runner/runner_linux.go:707`). The caller sees
nothing until the run terminalises and it issues a separate `run-log`. §14 requires the
opposite: *"Each stream is tee'd (process ↔ caller-live ↔ file): **stdout**/**stderr**
stream live … and are captured separately"*. This cut delivers the live half for
**foreground** runs, daemonless. It banks the headline observability win the runner-lite
phasing (§20, design line 159) set up.

## 1. Scope

**In:**
- `runner.Request` gains optional per-stream **live sinks** (`LiveStdout`, `LiveStderr
  io.Writer`; nil ⇒ no tee). A `--merge` run tees the single merged stream to
  `LiveStdout`.
- `drain()` tees captured bytes to the stream's live sink **decoupled** from the capture
  write, so a slow/non-draining live consumer can **never** stall the capture drain or,
  via pipe backpressure, the child. The **capture file stays the authoritative, complete,
  digest-covered record**; the live sink is best-effort.
- Honest **elision marker**: when the live sink cannot keep up, live bytes are dropped and
  a coalesced `[aira: N bytes elided from live view — see `run-log`]` marker is written to
  the **live sink only** (never the capture file). The file's bytes and SHA-256 digest are
  byte-identical to today.
- CLI face wires the caller's own writers as live sinks for a **human/text foreground run**;
  suppressed under `--json` (raw child bytes on stdout would corrupt the JSON record) and
  always nil for the **MCP face** (request/response, not a stream).
- Binary-safe (raw bytes; no normalisation on the live path).

**Out (explicit deferrals, unchanged from roadmap):**
- `--realtime` / `--pty` buffering tactics (§14 line 150) — **M18**. This cut tees whatever
  bytes the child flushes; buffering *promptness* is orthogonal.
- Telemetry / gate auto-wiring (`run --report/--ticket/--phase/--tool`, §14 line 155) — **M19**.
- `--detach` + the daemon-as-supervisor (owner-decided) and `run-input` (§14 line 156) —
  later Phase-5 cuts. `run-log --follow` keeps its current *poll-until-terminal* behaviour;
  genuine cross-process live tail arrives with the supervisor.
- No new `run` flag: live-tee is the default behaviour of a foreground human run, not an
  opt-in. `--detach` (deferred) will be the opt-out.

## 2. Invariants (must hold; each gets a discriminating test)

- **I1 — capture completeness is independent of the live consumer.** With a live sink that
  never drains (blocks forever / returns after a long block), the capture file for that
  stream is byte-complete and its recorded digest matches the child's actual output; the
  child runs to normal exit. (This is the anti-footgun; the `MultiWriter` naive design
  fails it.)
- **I2 — capture bytes and digest are unchanged vs. no-tee.** Same run with `LiveStdout ==
  nil` and with a fast live sink produce byte-identical capture files and identical
  `OutputRef.Digest`. The elision marker never enters the capture path.
- **I3 — faithful live passthrough under a keeping-up consumer.** A live sink that drains
  promptly receives every child byte, in order, with **no** elision marker. Binary-safe.
- **I4 — honest elision.** When drops occur, the live stream carries ≥1 coalesced marker
  accounting for the dropped byte count; the marker text is fixed and greppable; markers are
  never interleaved mid-UTF8-sequence in a way that claims to be child bytes (marker is
  clearly delimited).
- **I5 — no post-return writes to caller sinks.** In the common case (sink drains, or sink
  is a non-blocking buffer) all live-writer goroutines are joined before `Launch` returns,
  so no child byte lands on the caller's stdout after the run summary. The one documented
  exception is a permanently-wedged sink (caller stopped reading its own stdout) — then no
  correctness consumer exists to observe interleaving; this degradation is accepted and
  logged in §6.
- **I6 — merge fidelity.** A `--merge` run tees the single kernel-ordered merged stream to
  `LiveStdout`; `LiveStderr` is unused for merged runs.

## 3. Design

### 3.1 Decoupling — bounded channel + per-stream writer goroutine

`drain(name, rd, dst, out)` gains a live sink argument. Its read loop is unchanged for the
**capture** path: read a chunk → `writeAll(dst, chunk)` (authoritative, blocking until
written-or-fail) → hash → count. **New:** after the successful capture write, hand a *copy*
of the chunk to the live path via a **non-blocking send** on a bounded channel
(`chan []byte`, small fixed capacity, e.g. 64 chunks). A dedicated **live-writer goroutine**
per stream ranges the channel and writes to the sink.

- **Copy before send** — the read buffer (`buf`) is reused each iteration; the channel send
  MUST carry `append([]byte(nil), buf[:n]...)`, not `buf[:n]` (aliasing bug otherwise).
- **Non-blocking send** — `select { case ch <- cp: default: dropped += n }`. On the `default`
  branch the chunk is dropped and its length accumulated into a per-stream `dropped int64`.
  The capture loop never blocks on the live path.
- **Coalesced marker** — before the live writer writes the next *accepted* chunk after a drop
  gap, it emits the marker string with the accumulated dropped-byte count, then resets the
  counter. (Accounting lives with the producer; the marker is emitted by the writer so it
  interleaves correctly with real live bytes.) Marker write failure is ignored (best-effort).
- **Lifecycle** — when the capture read loop ends (child stream closed / EOF), `drain` closes
  the live channel. The live writer drains the residual buffered chunks (≤ capacity), emits a
  final marker if `dropped > 0`, then returns. `Launch` joins the live writers alongside the
  existing capture `collectCapture` barrier. Because sends are non-blocking and the channel
  bounded, the live writer's only unbounded wait is a single in-flight `sink.Write` to a
  wedged consumer — see I5 / §6.
- **Sink write errors** (e.g. `EPIPE` if the caller closed its read end) set a per-writer
  `disabled` flag; the writer stops touching the sink but keeps draining the channel so the
  capture side never blocks on a full channel. No capture impact.

### 3.2 Request / face wiring

- `runner.Request`: add `LiveStdout io.Writer` and `LiveStderr io.Writer`. `setupPipes` is
  unchanged; the sinks are consumed only by `drain`. For a merged run, `LiveStdout` is the
  merged sink and `LiveStderr` is ignored.
- `core.go` `"run"` handler: the core already holds face writers (`c.stdout`, the same writer
  the face renders to; add a companion `c.stderr` if not present). Populate `request.LiveStdout
  = c.stdout`, `request.LiveStderr = c.stderr` **only when the face is in human/streaming mode**.
  A new core field `c.liveTee bool` (set by the CLI face for a non-`--json` foreground run;
  false for MCP and for `--json`) gates this. For `--merge`, set `LiveStdout = c.stdout`,
  leave `LiveStderr` nil.
- CLI face (`cmd/aira/main.go`): when `verb == "run"` and output is **not** `--json`, enable
  live-tee (`c.liveTee = true`) with `c.stdout = <the run's live stdout>`, `c.stderr =
  <stderr>`. The run-summary record still renders **after** the child output. Under `--json`,
  live-tee stays off so stdout carries only the JSON `RunRecord`.
- MCP face: never sets live sinks (`ReadOutput`/`run-log` remains the way to fetch bytes).

### 3.3 What does NOT change

- Capture files, `OutputRef` paths/bytes/digest/state, `ReadOutput`, `run-log`,
  `collectCapture` grace/partial semantics, kill/scope/rusage paths, the ledger schema. The
  live path is additive and side-effect-free on the durable record.

## 4. Tests (each invariant → a discriminator that fails the naive design)

Runner-level (`internal/runner`, no cgroup/hardware dependence — a fake `blockingWriter` and a
`countingWriter` as sinks):
- **T1 (I1)** — `blockingWriter` (its `Write` blocks on an unsignalled channel) as `LiveStdout`;
  child prints > channel-capacity×chunk bytes then exits 0. Assert: `Launch` returns, record is
  `exited`/0, capture file is byte-complete, digest matches a direct hash of the expected output.
  *Fails against `MultiWriter(file, sink)`* (would deadlock the child + capture).
- **T2 (I2)** — same argv, run A with `LiveStdout=nil`, run B with a fast `bytes.Buffer` sink;
  assert capture files byte-equal and digests equal; assert the buffer contains no marker.
- **T3 (I3)** — fast sink, ~1 MiB of mixed binary bytes; assert the sink received exactly the
  child bytes in order, no marker, binary-safe (compare to capture file bytes).
- **T4 (I4)** — a **gated** slow sink (releases after the child exits) forced to overflow; assert
  the sink stream contains the marker with a plausible non-zero elided count AND that the
  capture file is still complete (marker only on the live path).
- **T5 (I6)** — `--merge`: assert the single merged sink receives the merged stream and no bytes
  go to a stderr sink.
- **T6 (aliasing)** — many small distinct chunks through a fast sink; assert no chunk corruption
  (guards the copy-before-send rule).

Face-level (`internal/core` / `cmd/aira`):
- **T7** — CLI `run` in text mode tees child stdout to the face stdout then prints the summary;
  `--json` mode emits a clean parseable `RunRecord` with **no** child bytes on stdout.
- **T8** — MCP `aira_run` never streams; output retrievable only via `aira_run_output`.

Real-process e2e (Opus, under `whale-run`): `aira run -- sh -c 'printf out; printf err 1>&2'`
shows both live; `--json` stays clean; a genuine slow-consumer harness confirms I1 on the real
binary.

## 5. Files

- `internal/runner/types.go` — `Request.LiveStdout`, `Request.LiveStderr`.
- `internal/runner/runner_linux.go` — `drain` signature + live path; `Launch` wiring to pass
  sinks and join live writers; the live-writer goroutine + marker helper.
- `internal/runner/runner_test.go` (+ maybe a new `livetee_test.go`) — T1–T6 + sink fakes.
- `internal/core/core.go` — `c.stderr`/`c.liveTee` plumbing; `"run"` handler sink wiring.
- `cmd/aira/main.go` — enable live-tee for non-`--json` foreground `run`; keep `--json` clean.
- `internal/core/*_test.go`, `cmd/aira/*_test.go` — T7/T8; update any existing `run` face test
  that now also observes child output in text mode.
- The stub (`cgroup_stub.go`) build path is unaffected (drain/Request are OS-agnostic; confirm
  the non-linux build still compiles — the live path lives in the shared drain).

## 6. Risks / expected yield / accepted degradations

- **Wedged-sink lifetime (I5 exception).** If the caller permanently stops reading its own
  stdout, one live-writer goroutine may block on its final `sink.Write` past `Launch` return.
  No correctness consumer is reading, so no observable corruption; the capture file is complete.
  Accepted and documented; a deadline-bounded abandon is a possible M18 refinement, not needed
  now. **Sol: challenge whether this can wedge a real agent** (the agent reads the CLI's stdout;
  if it stops, it has already abandoned the run).
- **Default-on behaviour change.** Text-mode `run` now emits child output before the summary.
  Existing text-mode `run` face tests must be updated; `--json` consumers are unaffected. Yield:
  the headline live-observability feature with zero change to the durable record.
- **Marker honesty.** The elided-count marker is best-effort (its own write can fail); the
  guarantee that survives is I1/I2 (capture completeness + digest), which is what downstream
  gates/telemetry read.

## 7. Deferrals (filed)

- `--realtime`/`--pty` (M18); telemetry/gate wiring (M19); detach + daemon-supervisor +
  `run-input` (later). `run-log --follow` genuine cross-process live tail rides on the
  supervisor. Deadline-bounded live-writer abandon (I5 hardening) is a candidate M18 refinement.
