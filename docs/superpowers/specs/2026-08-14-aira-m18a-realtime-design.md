# M18a — `--realtime` buffering tactic (+ the `buffering` record)

- **Milestone:** Phase 5, third cut (after M16 rusage, M17 live-tee). **Split from M18** on Sol
  plan-review P2: `--realtime` (simple env injection) ships here; **`--pty` (the correctness-
  critical pty-capture cut) is deferred to M18b** — its design + Sol's fixes are preserved in §7.
- **Design authority:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §14 line 150,
  §11 (the Run `buffering(none|realtime|pty)` field).
- **Depends on:** M12 capture, M17 live-tee.
- **Review:** Sol plan-review r1 → REVISE (P2 split-pty + P1 env_digest + P1 realtime-honesty),
  r2 → REVISE (P0 Buffering-set-early + mergeEvidence, P1 wording, P1 preserve-user-preload); this
  is **v3** (§8 tracks resolutions).

## 0. Context — the gap

M17 streams whatever bytes the child flushes, but a glibc-stdio tool **block-buffers** stdout
when it is a pipe (not a TTY), so a live `--follow`/tee sees nothing until exit. §14 line 150
defines opt-in tactics that change only *how promptly* output appears, never the captured
content. `--realtime` **replicates `stdbuf` via the child's env** (no argv splice), keeping
stdout/stderr **separate**; it is a **no-op** where it cannot apply. AIRA records the tactic in
a new `buffering` field. (`--pty` — universal line-buffering via a real TTY, which *merges*
out+err — is the harder cut and lands in M18b, §7.)

## 1. Scope

**In:**
- **`RunRecord.Buffering`** (`none|realtime|pty`) + `Request.Realtime bool`. The `pty` value is
  reserved for M18b; M18a only ever records `none` or `realtime`.
- **`--realtime`**: when a `libstdbuf.so` is located, inject into the child env
  `LD_PRELOAD=<libstdbuf.so>` (prepended if the user already set one), `_STDBUF_O=L`,
  `_STDBUF_E=L`, `PYTHONUNBUFFERED=1`; keep out/err **separate**. Record `buffering=realtime`
  when injected, else `none` (honest no-op). **This does not prove the child honoured it** — see
  I3.
- Face flag `--realtime` on `aira run` (CLI + MCP).
- Live-tee (M17) unchanged: `--realtime` tees out/err separately.

**Out (deferred):** `--pty` (**M18b**, §7); telemetry/gate wiring (M19); detach + daemon +
`run-input`; proving the child actually changed buffering (unprovable — I3).

## 2. Invariants (each → a discriminating test)

- **I1 — realtime does not alter the captured record.** Same child: `RUN-n.out`/`RUN-n.err`
  exist (streams stay separate), `CaptureComplete`, digest over the exact captured bytes. The
  tactic changes flush *timing*, not the file contents/semantics.
- **I2 — `env_digest` is the BASE (user) environment digest, tactic-orthogonal.** `EnvDigest` is
  taken over the user's env (`entries`) **before** the stdbuf vars are appended to `cmd.Env`, so a
  plain run and a `--realtime` run with the same user env have **identical `env_digest`**; the
  child *actually* runs with the injected vars. `buffering` is the separate recorded dimension.
  This is a **base-environment digest plus a tactic marker** — it does *not* reconstruct the exact
  child environment (the injected `libstdbuf.so` path is host-dependent and the injected overrides
  are not themselves recorded); it identifies the user's declared env + that realtime was applied.
- **I3 — honest buffering = injection applied, not effect proven.** `buffering=realtime` means
  the stdbuf injection was **eligible and applied** (a readable `libstdbuf.so` was located and the
  vars set); it is *not* a claim the child changed buffering (static/musl/non-glibc/`setuid`/
  secure-exec binaries ignore `LD_PRELOAD`; AIRA cannot and does not prove effect). No locatable
  `libstdbuf.so` ⇒ `buffering=none` and the child env is left **exactly as the user gave it**
  (any user `LD_PRELOAD` unchanged) — never a fake `realtime`, never a clobbered preload.
- **I4 — no gate-lane leak.** The gate command-lane path constructs `runner.Request` without
  `Realtime`, so gate runs record `buffering=none` and their `env_digest` equals the plain
  digest (proof-binding is unaffected).

## 3. Design

### 3.1 `--realtime` env injection

In `Launch`, `env` is computed from `effectiveEnvironment`/`explicitEnvironment`
(`runner_linux.go:114-116`) and `EnvDigest(entries)` records the user env. **After** the digest,
if `req.Realtime`, call `stdbufInjection(env)` → `env'` and assign `cmd.Env = env'`. So the
digest reflects user intent; the child runs with the tactic (I2).

`stdbufInjection`:
- `path := locateLibstdbuf()` — probe, in order: `/usr/lib/*/coreutils/libstdbuf.so`,
  `/usr/libexec/coreutils/libstdbuf.so`, `/usr/lib/coreutils/libstdbuf.so`, then the directory
  of `stdbuf` resolved on `PATH` (`<dir>/../lib*/coreutils/libstdbuf.so`). Each candidate is
  accepted **only if it stats as an existing regular, readable file** (never a bare constructed
  path). Overridable by a test seam (`locateLibstdbufFn`). Returns "" if none valid.
- If `path == ""` → return `env` **unchanged** (any user `LD_PRELOAD` preserved) and signal
  `applied=false` → `buffering=none`.
- Else set (replacing any existing same-key): `_STDBUF_O=L`, `_STDBUF_E=L`, `PYTHONUNBUFFERED=1`,
  and `LD_PRELOAD=<path>` **prepended** to any user `LD_PRELOAD` (`<path>:<existing>`). Return
  `applied=true` → `buffering=realtime`.

### 3.2 The `buffering` field + faces

- `Request.Realtime bool`; `RunRecord.Buffering string` (persisted in the ledger). The record's
  `Buffering` is set from `applied` (`realtime` iff injected, else `none`).
- **Set `Buffering` BEFORE the first `starting` ledger event, and copy it in `mergeEvidence`
  (Sol r2 P0).** Compute `applied` at env setup and assign `record.Buffering` (an explicit
  `none` for plain/gate runs, never `""`) before any event serialises the record — otherwise a
  terminal/reconcile path that rebuilds via `mergeEvidence` (which does not currently copy this
  field) drops it. Add `Buffering` to the fields `mergeEvidence` carries forward so it survives
  every terminal CAS and reconcile.
- CLI (`cmd/aira/main.go`) + MCP (`aira_run`): a `--realtime` bool flag, help/schema from the
  dispatch table. (`--pty` is added in M18b; if both are wired later, M18b refuses the combo.)

### 3.3 What does NOT change

Capture/cap/eviction, `OutputRef`/digest semantics, `ReadOutput`/`run-log`, kill/scope/rusage,
the M17 live-tee, `setupPipes` (still separate out/err). Only the child `env` changes, additively,
and one new recorded field.

## 4. Tests

- **T1 (I3 honesty + seam)** — `locateLibstdbufFn` returns a real path → `--realtime` run records
  `buffering=realtime` and the child env (observed via a child that prints `$LD_PRELOAD`/`$_STDBUF_O`
  into its capture) contains the injected vars; seam returns "" → `buffering=none`, capture shows
  **no** `LD_PRELOAD`. *Fails an impl that records `realtime` regardless of location.*
- **T2 (I2 env_digest)** — same argv+env: plain run A vs `--realtime` run B → identical
  `env_digest`; B's child env contains the injected vars (capture) but A's does not. *Fails an
  impl that folds the stdbuf vars into the digest.*
- **T3 (I1 separate + complete)** — `--realtime` run of a child writing to both stdout and stderr
  → `RUN-n.out` and `RUN-n.err` present, both `CaptureComplete`, digests over captured bytes.
- **T4 (I3 prepend + no-op preserve)** — **unit-test `stdbufInjection` directly** (input env →
  output env; avoids launching a child with a bogus preload): with a user `LD_PRELOAD=/x.so` and a
  located libstdbuf → output `LD_PRELOAD` is `<libstdbuf>:/x.so` (prepended, not clobbered); with
  the seam returning "" → the env is returned unchanged (user `LD_PRELOAD=/x.so` preserved, no
  `_STDBUF_*` added). *Fails an impl that clobbers or drops the user preload on the no-op path.*
- **T5 (I4 gate)** — a command-gate run records `buffering=none` and its `env_digest` equals the
  same command's plain digest (proof-binding unaffected).
- Real-process e2e (Opus): `aira run --realtime -- <glibc tool that block-buffers on a pipe>`
  shows output arrive incrementally under a live tee where a plain run withholds it until exit
  (best-effort — environment-dependent; the load-bearing assertions are T1–T5, capture-identical).

## 5. Files

- `internal/runner/types.go` — `Request.Realtime`, `RunRecord.Buffering`.
- `internal/runner/env.go` — `stdbufInjection(env) (env', applied)`, `locateLibstdbuf()` +
  `locateLibstdbufFn` seam.
- `internal/runner/runner_linux.go` — Launch: post-digest injection; set `record.Buffering` before
  the `starting` event; add `Buffering` to `mergeEvidence`'s carried-forward fields.
- `internal/runner/ledger.go` — persist `Buffering`.
- `internal/core/core.go` — `run` handler: `Realtime` arg → `Request`.
- `cmd/aira/main.go` — `--realtime` flag for `run`.
- Tests as §4.

## 6. Risks / expected yield

- **libstdbuf location varies by distro** → fail-open to `none` (honest) rather than guess wrong;
  the seam makes T1/T2 deterministic without depending on the host having coreutils' libstdbuf.
- Yield: prompt live output for glibc-stdio tools under M17's tee, with a truthful `buffering`
  record and zero change to the durable capture — plus the `buffering` field infra M18b builds on.

## 7. Deferred to M18b — `--pty` (design captured; own plan+review+build cycle)

`--pty` gives the child a real TTY (universal line-buffering) but **merges** out+err and the
child emits TTY escapes, so `--pty` **implies `merge_streams=true`**. Pure-Go via
`golang.org/x/sys/unix` (no cgo). Correctness-critical; its own two-loop. **Sol plan-review r1
constraints to honour in the M18b plan:**
- **Stdin (P0):** do **NOT** default `cmd.Stdin = pts` (a `cat`-like child blocks forever — AIRA
  never writes the master). Keep the existing stdin model (`/dev/null` default, or `--stdin
  file/-`); stdout/stderr = pts is sufficient for line-buffering (`isatty(1/2)` true). An optional
  TTY-stdin is a separate opt-in, not the default. Add a no-stdin non-blocking regression test.
- **Ctty (P0):** `SysProcAttr.Ctty` is the **child-side index into the fd list**, not a parent
  fd — derive it from the actual topology (stdin `/dev/null`, stdout=pts ⇒ `Ctty=1`; pts-stdin ⇒
  `0`). A bad `Setctty` must make `Start` **fail** (fail-closed, `E_RUN_PTY_UNAVAILABLE`), never a
  silent downgrade that still records `pty`. **Record `pty` only after verified setup**, and test
  the child is genuinely on a TTY (`isatty(1)&&isatty(2)`, `tcgetpgrp`, `/dev/tty`) — containment
  alone proves nothing about TTY correctness.
- **EIO=EOF capture (P1):** ordering is Start → **close the parent's pts** → drain the master
  (queued master data is read before Linux returns `EIO`; if the parent keeps the slave open the
  master never EIOs and the drain hangs). The drain must process `n>0` **even when** `readErr ==
  EIO`, map **only** `EIO` → EOF/`complete`, keep the master open until drain completion, and
  leave every other read error as `partial`. Unit-level master-drain test with a large/tail-marker
  payload + digest, a **non-EIO** error case (so "all errors are EOF" fails), and an assertion no
  extra slave fd lingers.
- **Merge/honesty:** exactly one `RUN-n.log`, no `out`/`err`; record `pty` + `merge_streams=true`;
  pty allocation failure is fail-closed (`E_RUN_PTY_UNAVAILABLE`), never a downgrade.
- **Containment:** the pty child is still `clone3`'d into the scope (`UseCgroupFD`) and
  `run-kill`-able; `Setsid`/`Setctty` compose with it.
- **Termios transformation (Sol r2):** a pty applies terminal output processing — notably `ONLCR`
  (`\n`→`\r\n`), plus other `c_oflag` cooking. The M18b plan MUST decide the captured stream: set
  the pts to raw/output-raw (`cfmakeraw`/clear `OPOST`) for byte-faithful capture, OR accept the
  transformed bytes as the defined stream — and the digest/capture tests must assert *that*
  defined stream, not assume pipe-identical bytes.

## 8. Sol plan-review resolutions

### r2
- **P0 Buffering set-early + mergeEvidence** → §3.2 + §5: `record.Buffering` set (explicit `none`
  for plain/gate, never `""`) before the `starting` event; `mergeEvidence` carries it forward so no
  terminal/reconcile merge drops it.
- **P1 I2 overreach** → I2 reworded to "base-environment digest plus a tactic marker"; does not
  reconstruct the exact host-dependent child env.
- **P1 preserve user preload + validate path** → I3/§3.1: no-op leaves the env (incl. any user
  `LD_PRELOAD`) unchanged; `locateLibstdbuf` accepts only an existing regular/readable file; T4 is
  now a direct `stdbufInjection` unit test (prepend + no-op-preserve), not a child launch.
- **M18b §7 termios** → added the `ONLCR`/`OPOST` transformation constraint for the M18b plan.

### r1
- **P2 split pty** → M18a = `--realtime` + `buffering` field; `--pty` deferred to M18b (§7) with
  the pty design + all r1 pty constraints (P0 stdin, P0 Ctty, P1 EIO) recorded for its own cycle.
- **P1 env_digest** → I2 + §3.1: user-env digest taken before injection; documented as
  tactic-orthogonal; T2 asserts digest equality + injected child env; T5 asserts the gate path.
- **P1 realtime honesty** → I3: `realtime` = injection *applied/eligible*, not effect proven;
  no-op → `none`; wording corrected throughout.
