# AIRA M18b — `--pty` buffering tactic (real-TTY capture)

Status: PLAN (v3 — Sol r2 fixes: close-master-to-unblock the bounded drain, TIOCGPTPEER-only fail-closed (no racy path fallback), cgroup.kill+populated=0 not "reap")
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

- **I1 — the runner performs no output translation of its own.** The pts is initialised
  output-raw (`OPOST` cleared) at allocation, so a child writing `line\n` yields `line\n` in
  `RUN-n.log` (not `line\r\n`) *for the runner's part*; the captured bytes are whatever the pts
  line discipline produces (which a child may alter via its own termios calls — we do **not**
  claim captured bytes equal the child's pre-line-discipline writes). The digest is over exactly
  the drained master bytes. A test writes a known payload incl. bare `\n` (child does not touch
  termios) and asserts the captured bytes + digest are `\r`-free.
- **I2 — a successful `Start` establishes the controlling TTY by construction.** With
  `Setsid=true` + `Setctty=true` (`Ctty=1`), a `Start` that returns nil means the kernel put the
  child in a new session and made the pts its controlling terminal — so `isatty(1)&&isatty(2)`
  and foreground-group ownership hold **by construction** (a `Setctty` failure makes `Start`
  fail). The runner does **not** re-verify at runtime; the real-cgroup integration test asserts
  `isatty(1&2)`, `tcgetpgrp(1)==getpgrp()`, `/dev/tty` opens — as a *test*, not a runtime claim.
  Containment (cgroup membership) alone proves nothing about TTY correctness.
- **I3 — honest, fail-CLOSED setup.** `buffering=pty` is recorded **only after `Start`
  succeeds** (§3.4). A parent-side pty setup failure (alloc/termios) → the run **fails**
  `E_RUN_PTY_UNAVAILABLE`; a `Start`-phase failure keeps its existing launch/scope code (§4);
  there is **no silent downgrade** to pipes that still records `pty` or `none`.
- **I4 — no fd-0 stdin deadlock; `/dev/tty` reads are bounded, not prevented.** `cmd.Stdin` is
  **never** the pts (default `/dev/null`, or the existing `--stdin`), so a child reading stdin
  to EOF (e.g. `cat`) exits promptly (regression test). A child that **opens `/dev/tty`
  directly** and reads will block (AIRA never writes the master) — TTY input is D1/unsupported in
  v1; such a read is **bounded by the run timeout + `run-kill`** (the child is scoped and
  killable), never an unkillable hang. We do not claim to prevent a `/dev/tty` block, only to
  bound it.
- **I5 — EIO=EOF drain is complete and leak-free.** Ordering: `Start` → **close the parent's
  copy of the pts (slave)** → `cgroup.kill`+`populated=0` → drain the master. The drain processes
  `n>0` **even when** `errors.Is(readErr, unix.EIO)`, maps **only** `EIO` → clean
  EOF/`CaptureComplete`, and treats **every other** read error as `partial` (`CaptureIncomplete`).
  On the normal path the master closes after `EIO`; on the bounded-join cutoff the master is
  **closed to unblock** the drain and the capture is `partial`. The `O_CLOEXEC` slave means no
  slave fd lingers in the child beyond fd 1/2 (else the master never EIOs → the bounded cutoff
  then applies rather than an unbounded hang).
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
2. `unix.IoctlSetPointerInt(ptmx, TIOCSPTLCK, 0)` (unlockpt).
3. **Obtain the slave via `TIOCGPTPEER` ONLY (Sol r1 P2 + r2 P1)** — `slaveFD, err :=
   unix.IoctlRetInt(ptmx, TIOCGPTPEER)` with `O_RDWR|O_NOCTTY|O_CLOEXEC` on the returned fd.
   `TIOCGPTPEER` (Linux 4.13+, universal on any realistic target) returns the peer of *this*
   master atomically and race-free. If it is unavailable (`ENOTTY`/pre-4.13) we **fail closed**
   `E_RUN_PTY_UNAVAILABLE` — we do **not** fall back to opening `/dev/pts/<N>` by path, because
   with multiple devpts mounts a reused index could attach the *wrong* master's slave (Sol r2);
   we never risk the wrong terminal.
4. **The slave is `O_CLOEXEC`** so the parent's original slave fd is *not* inherited by the
   child (only Go's dup'd fd 1/2 are); this is load-bearing for the EIO drain (§3.3, Sol r1 P0).
5. Put the **pts** in output-raw: `t, _ := unix.IoctlGetTermios(pts, TCGETS)`; clear `OPOST`
   (leave input flags — we never write the master); `IoctlSetTermios(pts, TCSETS, t)`. Clearing
   only `OPOST` removes the runner-induced `ONLCR` (`\n`→`\r\n`) while keeping `isatty`/line-
   buffering; the runner performs **no translation of its own** (I1 — it does not claim the
   child cannot re-enable `ONLCR` via its own termios calls).
6. Wrap both fds in `*os.File`; the master is `O_CLOEXEC` (never inherited), the pts is passed as
   the child's stdout/stderr (Go dups it to fd 1/2; the parent's original pts *os.File is closed
   right after `Start`).

Every step fails closed → `E_RUN_PTY_UNAVAILABLE` (with the failing syscall in the payload).
Allocation + termios run in the **parent, before `Start`**, so these failures are cleanly
attributable to pty-unavailability (§4).

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

**Ordering (I5, revised for Sol r1 P0):** the master reaches `EIO` only once **every** slave
reference is closed — including any held by a **descendant** that inherited fd 1/2 (or reopened
`/dev/tty`). So the terminal sequence is:
1. `Start` succeeds → record `Buffering="pty"` (§3.4).
2. after the leader is waited, the parent closes its original pts `*os.File` (the `O_CLOEXEC`
   slave means it was never inherited beyond Go's dup'd child fds).
3. **`cgroup.kill` the scope, then wait for `cgroup.events` `populated=0`** before joining the
   master drain (Sol r2 P2). Only the direct child is `wait(2)`-reaped; grandchildren are not
   waitable (AIRA is not a subreaper) but `cgroup.kill` + `populated=0` guarantees every scope
   member is **dead**, which is what matters — a dead descendant's slave fd is closed by the
   kernel, so the last slave reference goes away and the master `EIO`s. (We say "killed", not
   "reaped", for grandchildren.)
4. the M17 decoupled drain reads the master into the capture file + tee. The join is **bounded /
   cancellable**: on the deadline we **atomically mark the capture `CaptureIncomplete`/`partial`,
   then CLOSE the master to unblock the goroutine blocked in `master.Read`** (a ctx expiry alone
   does not interrupt a blocking `Read` — Sol r2 P1), then join the drain; the read error the
   close induces is the *expected* incomplete-termination signal, not a new failure. Never an
   unbounded hang, never a false `complete`.
5. on the normal path the master is closed after the drain reaches `EIO`.

### 3.3 EIO→EOF in the drain

The M17/M12 drain reads the reader (here the master) into the capture file + live tee. For the
pty reader it must:
- always **commit the `n` bytes first**, then inspect the error.
- treat a read returning `n>0` with an `EIO` error (matched via **`errors.Is(err, unix.EIO)`** —
  Go wraps the errno in `*PathError`/`*os.SyscallError`, so a literal `err == unix.EIO` is
  unsafe, Sol r1 P1) as **data then clean EOF** → `CaptureComplete` after the bytes are written.
- treat `n==0` + `errors.Is(err, unix.EIO)` as clean EOF/`CaptureComplete`.
- treat **any other** error (incl. a non-EIO errno, or the bounded-join cutoff) as
  `CaptureIncomplete`/`partial` (never coerce to complete).
- keep the master open across the loop; close after.
A `ptyReader` wrapper (or a per-stream `eofErrno` the drain honours) localises this so the pipe
path is unchanged (pipes EOF normally; only the pty path maps `EIO`).

### 3.4 Faces + record

- `--pty` (bool) on CLI `aira run` and MCP `aira_run`. `Request.PTY`.
- `RunRecord.Buffering="pty"` is set **only after `Start` succeeds** (Sol r1 P1) — unlike
  M18a's env tactic (known pre-launch), the controlling TTY is established by the kernel at
  `clone3`+`Setctty` during `Start`, so a successful `Start` is the earliest honest point. The
  pre-`Start` events (`scope-created`/`starting`) carry `Buffering=""`; the running-transition
  event sets `"pty"`, and `mergeEvidence` (`if candidate.Buffering != "" { base.Buffering = … }`)
  carries it through the terminal CAS. A failed `Start` never records `"pty"`.
- `--pty` implies `Merge=true`: the record's `merge_streams`/`OutputRefs` show a single `log`.
- text face tees the single merged stream (M17); `--json`/MCP suppress the tee.

## 4. Fail-closed matrix (E_RUN_PTY_UNAVAILABLE)

Only **parent-side pty setup** (allocation/termios, before `Start`) maps to
`E_RUN_PTY_UNAVAILABLE`. `Start`-phase failures are **not** collapsed into the pty code (Sol r1
P1): clone3/cgroup/exec/permission failures are not pty-unavailability and keep their existing
stable codes. Because `Ctty` is derived deterministically (`=1`, §3.2), a Ctty misconfiguration
does not occur in practice; if `Start` fails it is a genuine launch/scope failure, honestly
coded as such.

| failure | code |
|---|---|
| `/dev/ptmx` open fails (no devpts, sandbox) | `E_RUN_PTY_UNAVAILABLE`, run not launched, no ledger junk |
| unlockpt / `TIOCGPTPEER`(+validated fallback) fails | `E_RUN_PTY_UNAVAILABLE` |
| termios get/set fails | `E_RUN_PTY_UNAVAILABLE` (we do not silently capture cooked bytes) |
| `Start` fails (clone3 / cgroup / exec / EPERM) | **existing** `E_RUN_LAUNCH_FAILED` / `E_RUN_SCOPE_UNAVAILABLE` (unchanged classification) — not the pty code |
| any pty setup path | **never** a silent downgrade to pipes recording `pty` or `none` |

`E_RUN_PTY_UNAVAILABLE` is registered in `internal/store/check.go` (exit-code map). A pty
setup failure fails **this run**; it never falls back to pipes (that would be a dishonest tactic).

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

1. A `--pty` child is on a controlling TTY **by construction** (successful `Start` with
   `Setsid`+`Setctty`); a parent-side setup failure → `E_RUN_PTY_UNAVAILABLE`, a `Start` failure
   → its existing launch/scope code — never a pipe run mislabeled `pty`, never all-Start-errors
   collapsed into the pty code.
2. `cmd.Stdin` is never the pts (no fd-0 deadlock); a `/dev/tty`-reading child is bounded by the
   run timeout + `run-kill`, not prevented (TTY input is D1).
3. The master drain maps **only** `errors.Is(err, unix.EIO)` → complete (bytes committed first);
   every other error and the bounded-join cutoff → partial; the `O_CLOEXEC` slave + kill-and-reap
   of scope members before the join ensure no lingering slave fd hangs the drain unbounded.
4. The captured stream is byte-faithful to the defined (output-raw) stream — no `ONLCR` `\r`.
5. `--pty` ⇒ exactly one `RUN-n.log`, `merge_streams=true`, `Buffering=pty` (set pre-first-event,
   carried in `mergeEvidence`); gate lane unaffected.
6. `Setsid`+`Setctty(Ctty=1)`+`UseCgroupFD` compose; the child stays scoped + `run-kill`-able.
7. A pty failure never downgrades to pipes; it fails this run honestly.
