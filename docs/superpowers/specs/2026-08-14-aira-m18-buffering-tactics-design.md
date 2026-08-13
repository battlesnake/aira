# M18 — `--realtime` / `--pty` buffering tactics (+ the `buffering` record)

- **Milestone:** Phase 5, third cut (after M16 rusage, M17 live-tee).
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14 line 150
  (`--realtime`/`--pty`), §11 (the Run `buffering(none|realtime|pty)` field).
- **Depends on:** M12 capture, M17 live-tee (both tactics feed the same drain → tee → file
  path), `golang.org/x/sys/unix` (already a direct dep → pure-Go pty, no cgo).

## 0. Context — the gap

M17 streams whatever bytes the child flushes, but a glibc-stdio tool block-buffers stdout
when its output is a pipe (not a TTY), so a live `--follow`/tee sees nothing until it exits.
§14 line 150 defines two opt-in tactics that change *how promptly* output appears —
**never** what the capture file ends up containing:
- **`--realtime`** — replicate `stdbuf` via the child's **env** (no argv splice), keeping
  stdout/stderr **separate**; a no-op where it cannot apply.
- **`--pty`** — give the child a **pty** so libc line-buffers universally; this **merges**
  out+err and the child emits TTY escapes, so `--pty` **implies `merge_streams=true`**.

AIRA records which tactic actually ran (`buffering`), honestly.

## 1. Scope

**In:**
- `RunRecord.Buffering` + `Request` fields; the record states the tactic that *actually* took
  effect (see honesty rules, §2).
- **`--realtime`**: inject `LD_PRELOAD=<libstdbuf.so>`, `_STDBUF_O=L`, `_STDBUF_E=L`,
  `PYTHONUNBUFFERED=1` into the child env, **only when `libstdbuf.so` is located**; keeps
  out/err separate. Records `buffering=realtime` when applied, else `none` (honest no-op).
- **`--pty`**: pure-Go pty (`/dev/ptmx` → unlockpt → `TIOCGPTN` → `/dev/pts/N`); child
  stdout+stderr (and stdin, §3.2) = the pts; `SysProcAttr` gains `Setsid`+`Setctty` alongside
  the existing `UseCgroupFD`; the drain reads the **master**. Implies `Merge` (single `RUN-n.log`).
  Records `buffering=pty`, `merge_streams=true`.
- Face flags `--realtime` / `--pty` on `aira run` (CLI + MCP), mutually exclusive; `--pty`
  with/without `--merge` is accepted (pty forces merge); `--realtime`+`--merge` is allowed
  (merge is orthogonal to realtime).
- Live-tee (M17) unchanged: `--realtime` tees out/err separately; `--pty` tees the merged
  stream to `LiveStdout`.

**Out (deferred):** telemetry/gate wiring (M19); detach + daemon-supervisor + `run-input`;
window-size negotiation beyond a fixed default; non-Linux pty; normalising/stripping TTY
escapes from the capture (captured verbatim — binary-safe).

## 2. Invariants (each → a discriminating test)

- **I1 — capture completeness is tactic-independent.** For the SAME child, the capture file
  is byte-complete under `none`, `--realtime`, and `--pty` (content differs — a TTY child may
  emit escapes / merge order — but the run is `CaptureComplete`, never falsely `partial`).
- **I2 — pty EIO is clean EOF.** Reading the pty master after the slave closes returns
  `EIO` on Linux; the drain treats `EIO` as end-of-output (State `complete`, digest over the
  bytes actually read), **not** a capture error/`partial`. (A naive `drain` marks it partial.)
- **I3 — honest buffering.** `buffering` records what *took effect*: `--realtime` with no
  locatable `libstdbuf.so` records `none` (never a fake `realtime`); `--pty` records `pty` +
  `merge_streams=true`. `--pty` and `--realtime` together are refused (`E_RUN_ARGUMENT_INVALID`).
- **I4 — `--pty` forces merge, faithfully.** A `--pty` run produces exactly one `RUN-n.log`
  output ref (no `out`/`err`), and the record cannot claim `pty` + separate streams.
- **I5 — realtime keeps streams separate + does not corrupt user env.** `--realtime` produces
  `RUN-n.out`/`RUN-n.err`; the injected vars are AIRA-applied — see §3.1 for the `env_digest`
  decision (digest reflects the *user* env; the tactic is recorded separately, so two runs
  differing only in `--realtime` share an `env_digest`).
- **I6 — scope containment holds under pty.** The pty child is still `clone3`'d into the
  cgroup scope (`UseCgroupFD`) and is `run-kill`-able; `Setsid`/`Setctty` compose with it.

## 3. Design

### 3.1 `--realtime` (env injection)

In `Launch`, after `env` is computed (`effectiveEnvironment`/`explicitEnvironment`,
`runner_linux.go:114-116`) and **after** `EnvDigest(entries)` is taken, append the stdbuf
vars to the `env` slice actually handed to `cmd.Env` — so **`env_digest` reflects the user's
intended environment, not the AIRA tactic** (I5; a gate lane never uses `--realtime`, so no
proof-binding concern). Locate `libstdbuf.so` by probing known coreutils paths
(`/usr/lib*/coreutils/libstdbuf.so`, `/usr/lib/*/coreutils/libstdbuf.so`) and, as a fallback,
the dir of the `stdbuf` binary on `PATH`; if none found, apply nothing and record `none`
(I3). `_STDBUF_O=L`/`_STDBUF_E=L` = line-buffered; `PYTHONUNBUFFERED=1` covers CPython. If the
user already set `LD_PRELOAD`, prepend (`libstdbuf.so:<existing>`), don't clobber.

### 3.2 `--pty` (pure-Go pty, no cgo)

- Allocate: `ptmx, _ := os.OpenFile("/dev/ptmx", O_RDWR|O_NOCTTY, 0)`; `unix.IoctlSetPointerInt(
  ptmx.Fd(), TIOCSPTLCK, 0)` (unlockpt); `n, _ := unix.IoctlGetInt(ptmx.Fd(), TIOCGPTN)`; open
  `/dev/pts/N` as `pts` (O_RDWR|O_NOCTTY). Set a fixed default winsize (e.g. 80x24) via
  `TIOCSWINSZ` on the pts. On any allocation error → `E_RUN_PTY_UNAVAILABLE`, fail-closed
  (never silently downgrade to pipes — that would mis-record `buffering`).
- Wire: `cmd.Stdout = cmd.Stderr = pts`; `cmd.Stdin = pts` unless `--stdin`/`--no-stdin` was
  given (§3.2 stdin note); `SysProcAttr.Setsid = true`, `Setctty = true`, `Ctty = <child fd of
  pts>` (Go maps the pts to the child's fd set; use the conventional `Ctty` index for the pts
  among Std{in,out,err}). Keep `UseCgroupFD`/`CgroupFD` (I6).
- Capture source: the **master** `ptmx` replaces the pipe reader for a single `log` stream.
  After `cmd.Start()`, **close the parent's `pts`** (the child holds it) so the master sees
  EOF/EIO when the child tree exits.
- `drain` reads `ptmx` → `RUN-n.log`; **`EIO` ⇒ treat as EOF, State `complete`** (I2). This is
  the one `drain` change: a `pty` flag (or reading through a small wrapper) so `syscall.EIO`
  after any bytes is end-of-stream, not `firstErr`. All other read errors stay partial.
- Merge/tee: exactly the `--merge` topology — one `log` capture + `LiveStdout` tee (M17).

  **stdin note:** for a `--pty` run with no explicit `--stdin`, wiring `cmd.Stdin = pts` gives
  the child a TTY stdin (what interactive tools expect) but leaves the master with no writer;
  AIRA does not feed it (live stdin push = deferred `run-input`). `--no-stdin` → `cmd.Stdin =
  /dev/null`. An explicit `--stdin file/-` overrides to the configured source (child stdin is
  then not a TTY; still records `buffering=pty` for out/err). Documented, Sol to sanity-check.

### 3.3 The `buffering` field + faces

- `Request.Buffering` (an enum `none|realtime|pty`, or two bools `Realtime`/`Pty` normalised in
  core) and `RunRecord.Buffering string`. `--pty` sets `Merge=true` in the core handler.
- Refuse `--realtime`+`--pty` at the face/handler with `E_RUN_ARGUMENT_INVALID`.
- CLI (`cmd/aira/main.go`) + MCP (`aira_run`) expose `--realtime`/`--pty` bool flags; help/schema
  generated from the dispatch table as usual.
- The record's `Buffering` is set from what actually applied (realtime→none downgrade honestly).

### 3.4 What does NOT change

Capture cap/eviction, `OutputRef`/digest *semantics* (digest still over the exact captured
bytes), `ReadOutput`/`run-log`, kill/scope/rusage/ledger, and the M17 live-tee machinery. Only
the capture *source* (pty master) and child *env*/`SysProcAttr` change, additively.

## 4. Tests

Runner-level (`internal/runner`, real cgroup + pty; use `isolatedScopeParent` where a scope may
linger; unit-level where possible):
- **T1 (I2, pty EIO=EOF)** — a child that writes N bytes then exits under `--pty`; assert the run
  is `CaptureComplete`, `RUN-n.log` has the bytes, digest matches, status `exited`/0. *Fails a
  drain that marks EIO partial.* Prefer a unit-level drain test over a real pty master fd (write
  bytes to a pts, close it, assert the master-drain completes) so it runs in CI.
- **T2 (I4 merge)** — `--pty`: exactly one `log` output ref, no `out`/`err`; `merge_streams`/
  `Merge` true; `Buffering=="pty"`.
- **T3 (I3 honesty)** — `--realtime` with `libstdbuf.so` located → `buffering=realtime` and the
  child env contains `LD_PRELOAD`/`_STDBUF_*`; with location forced-empty (test seam) →
  `buffering=none`, no `LD_PRELOAD`. `--realtime`+`--pty` → `E_RUN_ARGUMENT_INVALID`.
- **T4 (I5 env_digest)** — same argv+env, run A plain vs run B `--realtime`: identical
  `env_digest`; run B's *child* env (observed via a child that prints its env to the capture)
  contains the injected vars.
- **T5 (I1 completeness parity)** — a glibc-stdio child (`printf` in a loop) under none/realtime/
  pty: all `CaptureComplete`; the pty capture is non-empty and contains the expected payload.
- **T6 (I6 containment)** — a `--pty` run is placed in the scope (`ScopeContained`) and
  `run-kill` terminates it (real cgroup, isolated parent).
- **T7 (tee integration)** — `--pty` tees the merged stream to a live sink (M17 fake); `--realtime`
  tees out/err separately.
- Real-process e2e (Opus): `aira run --pty -- python3 -c 'print("live",flush=False)'`-style shows
  prompt output that a plain run buffers; `--realtime` on a glibc tool; `--pty` merge + run-kill.

## 5. Files

- `internal/runner/types.go` — `Request` realtime/pty (+`Buffering`), `RunRecord.Buffering`.
- `internal/runner/env.go` — `stdbufInjection(env) → env'` + `locateLibstdbuf()` (+ test seam).
- `internal/runner/pty_linux.go` (new) — `openPTY() (ptmx, pts *os.File, err)`, winsize; pure
  `x/sys/unix`.
- `internal/runner/runner_linux.go` — Launch: realtime env inject (post-digest); pty branch
  (allocate, wire `SysProcAttr`, master-drain, close pts); `drain` EIO=EOF for the pty source;
  `openOutputs`/`setupPipes` gain the pty topology (single `log` from the master).
- `internal/runner/ledger.go` — persist `Buffering`.
- `internal/core/core.go` — `run` handler: realtime/pty args, `--pty`⇒`Merge`, refuse both,
  set `Request` fields; response mapping.
- `cmd/aira/main.go` — `--realtime`/`--pty` bool flags for `run`.
- Tests as §4 (`internal/runner/*_test.go`, core/cmd).

## 6. Risks / deferrals

- **pty capture completeness (I2)** is the correctness-critical piece — EIO-as-EOF must not
  falsely partial nor swallow a genuine error; the unit-level master-drain test is the guard.
- **libstdbuf location** varies by distro; fail-open to `none` (honest) rather than guess wrong.
- **`Ctty` fd index** for `Setctty` is fiddly (must be the child-side fd of the pts among the
  Std fds); the containment + e2e tests catch a wrong controlling-tty setup.
- Deferrals: M19 wiring; detach/daemon/`run-input`; escape-stripping; winsize negotiation.
