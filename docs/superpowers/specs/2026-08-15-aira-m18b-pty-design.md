# AIRA M18b — `--pty` buffering tactic (real-TTY capture)

Status: PLAN (v1, for Sol adversarial plan-review)
Date: 2026-08-15
Milestone: #28 — Phase 5 · M18b — `--pty` buffering tactic (pty capture)
Depends on: M12 capture, M17 live-tee, M18a `buffering` field (all landed, master `d08a102`)
Design authority: 2026-08-07-aira-design.md §14 line 150, §11 (`buffering(none|realtime|pty)`).
Prior review: the M18a plan §7 preserves Sol's r1+r2 pty constraints — this plan implements them.

## 0. The gap M18a left

`--realtime` (M18a) replicates `stdbuf` via the child env — a best-effort no-op where
`LD_PRELOAD` is ignored (static/musl/setuid). `--pty` is the **universal** line-buffering
tactic: give the child a real controlling TTY so `isatty(1)` is true and glibc line-buffers
by default. A TTY has **one** stream (out+err interleave on the same device), so `--pty`
**implies merged capture**. This is correctness-critical (fd lifetime, EIO=EOF, controlling-TTY
setup, stdin no-deadlock, termios output processing) and gets its own two-loop.

## 1. Scope

**In:**
- `Request.PTY bool` + face flag `--pty` on `aira run` (CLI + MCP).
- Pure-Go pty allocation via `golang.org/x/sys/unix` (posix_openpt/grantpt/unlockpt/ptsname
  equivalents) — **no cgo**.
- `--pty` **implies `Merge=true`**: exactly one `RUN-n.log`, no `out`/`err` files. Requesting
  `--pty` with an explicit non-merge is accepted (pty forces merge) and the record shows
  `merge_streams=true`; the two are not contradictory because a TTY has a single stream.
- `RunRecord.Buffering="pty"`, recorded **only after** verified TTY setup (I3/§7 Ctty).
- Byte-faithful capture: the pts is put in **output-raw** mode (clear `OPOST`, so no `ONLCR`
  `\n`→`\r\n`) — the captured/digested stream is the child's raw bytes (§7 termios decision).
- Composes with the M12 containment: still `clone3`'d into the scope (`UseCgroupFD`),
  `run-kill`-able; `Setsid`+`Setctty` compose with `UseCgroupFD` in one `SysProcAttr`.
- M17 live-tee unchanged: the single merged stream tees to the caller (text face).

**Out (deferred):**
- TTY **stdin** (interactive input) — D1; stdin keeps the existing model (`/dev/null` default
  or `--stdin`), never the pts (the §7 P0 deadlock).
- Window size / `SIGWINCH`, terminal resize — D2.
- Telemetry/gate wiring (M19); detach/daemon/run-input.
- Proving the child actually line-buffered — a TTY makes glibc line-buffer by default but the
  child may still `setvbuf`; `buffering=pty` means **a verified controlling TTY was provided**,
  not an effect claim (I3, mirrors M18a honesty).

## 2. Invariants (each → a discriminating test)

- **I1 — pty capture is byte-faithful to the defined stream.** The pts is output-raw
  (`OPOST` cleared) at allocation, so a child writing `line\n` yields `line\n` in `RUN-n.log`
  (not `line\r\n`). The digest is over exactly the drained master bytes. A test writes a known
  payload incl. bare `\n` and asserts the captured bytes + digest are `\r`-free.
- **I2 — the child is genuinely on a controlling TTY.** A probe child asserts `isatty(1) &&
  isatty(2)`, `tcgetpgrp(1)==getpgrp()` (it owns the TTY foreground group), and `/dev/tty`
  opens. Containment (cgroup membership) alone proves nothing about TTY correctness.
- **I3 — honest, fail-CLOSED setup.** `buffering=pty` is recorded **only after** allocation +
  `Setsid`+`Setctty` succeed and `Start` returns. Any failure (openpt/grant/unlock/ptsname,
  a wrong `Ctty` index, `Start` error) → the run **fails** `E_RUN_PTY_UNAVAILABLE`; there is
  **no silent downgrade** to pipes that still records `pty` or `none`.
- **I4 — no stdin deadlock.** `cmd.Stdin` is **never** the pts (default `/dev/null`, or the
  existing `--stdin`). A child that reads stdin to EOF (e.g. `cat`) exits promptly; the master
  is only ever read, never written. Regression test: a no-stdin `--pty` run of a stdin-reading
  child terminates within the deadline.
- **I5 — EIO=EOF drain is complete and leak-free.** Ordering: `Start` → **close the parent's
  copy of the pts (slave)** → drain the master. The drain processes `n>0` **even when**
  `readErr == EIO`, maps **only** `EIO` → clean EOF/`CaptureComplete`, keeps the master open
  until drain completion, and treats **every other** read error as `partial`
  (`CaptureIncomplete`). No slave fd lingers after `Start` (else the master never EIOs → hang).
- **I6 — one merged stream, no gate-lane leak.** `--pty` writes exactly one `RUN-n.log`;
  `merge_streams=true`; `Buffering=pty`. The gate command-lane constructs `runner.Request`
  without `PTY`, so gate runs stay `buffering=none`, pipe-captured, digest unchanged.
- **I7 — `env_digest` is tactic-orthogonal.** As M18a: the digest is over the user's base env;
  `pty` is the separate recorded dimension. `--pty` injects no env (unlike `--realtime`).

## 3. Design

### 3.1 Allocation (pure-Go, no cgo)

A new `internal/runner/pty_linux.go` (build-tagged `linux`) exposes `allocatePTY() (master,
slave *os.File, err error)`:
1. `ptmx := unix.Open("/dev/ptmx", O_RDWR|O_NOCTTY|O_CLOEXEC, 0)` → master.
2. `unix.IoctlSetPointerInt(ptmx, TIOCSPTLCK, 0)` (unlockpt) and grant (Linux devpts needs no
   grantpt helper binary; `unix.IoctlGetInt(ptmx, TIOCGPTN)` gives the pts number `N`).
3. Open `/dev/pts/N` with `O_RDWR|O_NOCTTY` → slave (pts).
4. Put the **pts** in output-raw: `t, _ := unix.IoctlGetTermios(pts, TCGETS)`; clear `OPOST`
   (and leave input flags alone — we never write the master); `IoctlSetTermios(pts, TCSETS, t)`.
   (We clear only `OPOST` — the minimal change that removes `ONLCR` while keeping the TTY
   otherwise standard so `isatty`/line-buffering hold; §7 termios decision = byte-faithful.)
5. Wrap both fds in `*os.File` (so Go manages lifetime); master is `O_CLOEXEC` (never inherited
   by the child), the pts is intentionally inherited as the child's stdout/stderr.

Every step fails closed → `E_RUN_PTY_UNAVAILABLE` (with the failing syscall in the payload).

### 3.2 Launch wiring

Add a seam parallel to `setupPipes`: when `req.PTY`, `setupCapture` allocates the pty instead
of pipes and returns the master as the single "log" reader and the pts as the single writer:
- `cmd.Stdout = pts; cmd.Stderr = pts` (same fd → interleaved single stream).
- `cmd.Stdin` unchanged (the existing `setupStdin`: `/dev/null` default or `--stdin`).
- `cmd.SysProcAttr` gains `Setsid = true` (new session → the child can acquire a controlling
  TTY) and `Setctty = true` with **`Ctty = <child-fd index of the pts>`**. The `Ctty` is the
  index into the child's fd list `[stdin, stdout, stderr]`; with stdin=`/dev/null` and
  stdout=pts, **`Ctty = 1`**. (If a future TTY-stdin opt-in makes stdin the pts, `Ctty=0` — out
  of scope here.) `UseCgroupFD`/`CgroupFD` stay set; `Setsid`+`Setctty`+`UseCgroupFD` compose.
- The pty path forces `Merge=true` in the record and opens a single `RUN-n.log`.

**Ordering (I5):** exactly as the pipe path plus the slave close — after `Start`:
`for w := range writers { w.Close() }` already closes the parent's pts copy (the pts is the
"writer"); the master ("reader") is drained by the M17 tee/capture loop; the master is closed
only after drain completion. The child holds its own dup of the pts (fd 1/2); when it exits,
the last slave reference is the kernel's and the master read returns `EIO`.

### 3.3 EIO→EOF in the drain

The M17/M12 drain reads the reader (here the master) into the capture file + live tee. For the
pty reader it must:
- treat a read returning `n>0, err==EIO` as **data then EOF**: write the `n` bytes, then stop
  cleanly and mark `CaptureComplete`.
- treat `n==0, err==EIO` as clean EOF/`CaptureComplete`.
- treat **any other** error as `CaptureIncomplete`/`partial` (never coerce to complete).
- keep the master open across the loop; close after.
A `ptyReader` wrapper (or a per-stream "eofErrno" the drain honours) localises this so the pipe
path is unchanged (pipes EOF normally; only the pty path maps `EIO`).

### 3.4 Faces + record

- `--pty` (bool) on CLI `aira run` and MCP `aira_run`. `Request.PTY`.
- `RunRecord.Buffering="pty"` set **before the first ledger event** (M18a's field-loss trap:
  carried in `mergeEvidence` — the existing `if candidate.Buffering != "" { base.Buffering = … }`
  already preserves it; a `pty` value must be set on the candidate at every child-ran path).
- `--pty` implies `Merge=true`: the record's `merge_streams`/`OutputRefs` show a single `log`.
- text face tees the single merged stream (M17); `--json`/MCP suppress the tee.

## 4. Fail-closed matrix (E_RUN_PTY_UNAVAILABLE)

| failure | behaviour |
|---|---|
| `/dev/ptmx` open fails (no devpts, sandbox) | `E_RUN_PTY_UNAVAILABLE`, run not launched, no ledger junk |
| unlockpt / TIOCGPTN / pts open fails | `E_RUN_PTY_UNAVAILABLE` |
| termios get/set fails | `E_RUN_PTY_UNAVAILABLE` (we do not silently capture cooked bytes) |
| `Start` fails (bad Ctty / clone3 / EPERM) | existing launch-fail path, but a Ctty/Setctty error is `E_RUN_PTY_UNAVAILABLE`, never a pipe downgrade |
| never a silent downgrade to pipes recording `pty` or `none` | — |

`E_RUN_PTY_UNAVAILABLE` is registered in `internal/store/check.go` (exit-code map). A pty
failure fails **this run**; it never falls back to pipes (that would be a dishonest tactic).

## 5. Tests (TDD; discriminating)

Unit (seam-level, host-independent where possible):
- **allocatePTY**: returns a master+pts; pts `OPOST` cleared (assert termios); master `O_CLOEXEC`.
- **ptyReader / drain**: over a real `os.Pipe`-backed fake that returns `n>0,EIO` → bytes then
  `complete`; `n==0,EIO` → `complete`; `n>0, EAGAIN/other` → `partial` (so "all errors = EOF"
  fails); a large tail-marker payload + digest; assert no lingering slave fd.
- **Ctty derivation**: stdin=/dev/null ⇒ Ctty=1; (documented) pts-stdin ⇒ 0.
- **merge forcing**: `Request{PTY:true, Merge:false}` → record merge_streams=true, single log.
- **fail-closed**: an injected allocate/termios/Setctty failure → `E_RUN_PTY_UNAVAILABLE`, no
  ledger `starting` junk, no pipe downgrade.
- **buffering carry**: `mergeEvidence` preserves `pty` across a terminal merge.

Real-cgroup integration (`AIRA_REAL_CGROUP=1`, fail-not-skip like M16):
- **I2 TTY correctness**: a child (`sh -c 'test -t 1 && test -t 2 && tty'`) under `--pty` →
  captured `RUN-n.log` shows a `/dev/pts/*` path and exit 0; a plain (pipe) run of the same →
  `not a tty` / exit 1. This is the discriminator a pipe impl cannot pass.
- **I1 byte-faithful**: a child emitting `printf 'a\nb\n'` under `--pty` → captured bytes are
  `a\nb\n` (no `\r`); digest matches the raw form.
- **I4 no-stdin deadlock**: `--pty` run of `cat` with `/dev/null` stdin exits within the deadline.
- **I5 completion**: `CaptureComplete` on normal exit; a killed pty run is `partial`/killed.
- **containment**: the pty child is in the scope and `run-kill`-able (owner guard from #27 still
  applies).

**Real-binary e2e** (`~/tmp/aira-m18b-e2e.sh`, committed): `aira run --pty -- sh -c 'test -t 1;
echo tty=$?'` shows tty=0 (on a TTY); a plain run shows tty=1; `--pty` produces one `RUN-n.log`
retrievable via `run-log` with no `--stream err`; `--pty` of a stdin-reader terminates; a
sandbox with no devpts → `E_RUN_PTY_UNAVAILABLE` (fail-closed, honest).

## 6. Non-goals / deferrals (written down)

- **D1 TTY stdin** — interactive input via the pts (needs the master written + a no-deadlock
  input model). Out of v1; stdin stays `/dev/null`/`--stdin`.
- **D2 window size / SIGWINCH / resize** — no `TIOCSWINSZ`; the pty gets the default size.
- **D3 effect proof** — `buffering=pty` = a verified controlling TTY was provided, not a claim
  the child line-buffered (it may `setvbuf` to full). Mirrors M18a I3 honesty.
- **D4 non-Linux** — Linux-only (devpts), consistent with the runner.

## 7. Invariants for the build review to attack (both directions)

1. A `--pty` child is on a **verified** controlling TTY (isatty(1&2) + owns the fg group), or the
   run **fails** `E_RUN_PTY_UNAVAILABLE` — never a pipe run mislabeled `pty`.
2. `cmd.Stdin` is never the pts; a stdin-reading child never deadlocks.
3. The master drain maps **only** `EIO` → complete; every other error → partial; `n>0` on `EIO`
   is not dropped; no slave fd lingers (else hang).
4. The captured stream is byte-faithful to the defined (output-raw) stream — no `ONLCR` `\r`.
5. `--pty` ⇒ exactly one `RUN-n.log`, `merge_streams=true`, `Buffering=pty` (set pre-first-event,
   carried in `mergeEvidence`); gate lane unaffected.
6. `Setsid`+`Setctty(Ctty=1)`+`UseCgroupFD` compose; the child stays scoped + `run-kill`-able.
7. A pty failure never downgrades to pipes; it fails this run honestly.
