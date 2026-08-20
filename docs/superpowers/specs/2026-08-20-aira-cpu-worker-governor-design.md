# Embedded sidecar modules + dynamic cross-session CPU-worker governor, v2

**Status:** plan — Sol r1→r3 → APPROVE → Fable code-grounded gate → **GATE-PASS** (1 P0 + 3
P1 + 6 P2, folded in §1a) → this is **v5**. **Milestone:** Phase 5 · CPU-worker governor
(task #49). Fable live-verified the pytest hookwrapper (pytest 9.0.3, pre-yield wait
excluded from phase durations), `unix.Renameat2`+`RENAME_NOREPLACE` in the vendored
`x/sys v0.47.0`, and the flock/Serve-hook/NumCPU claims.

## 1a. v5 amendments (Fable plan-gate — AUTHORITATIVE; supersede any conflicting detail below)

- **A1 (P0) — fail-open at EVERY layer, + fsync before publish.** (a) The `conftest` snippet
  is **import-time** code — document it as **`try/except Exception: pass`** around the
  sys.path-insert + register, a no-op on any error (a raising conftest fails the whole
  suite). (b) When `ExtractPyLib` or `renameat2` errors (`EINVAL`/`ENOSYS` on an exotic FS),
  the wrapper **skips setting the env vars and proceeds** — never fails `run`/`time`. (c)
  When slot-dir creation fails, the daemon **logs and degrades** — never aborts the mandatory
  `Serve` (`server.go:108`). (d) **`fsync` the extracted files + their dirs (and the slot
  files + dir) BEFORE the `renameat2` publish** (precedent `syncDir`, `runner_linux.go:372`)
  — `XDG_DATA_HOME` is persistent ext4; a crash after the rename could otherwise durably
  publish `.ready` over zero-length files.
- **A2 (P1-1) — the `run` env append must be at the COMMON construction point, not
  `runner_linux.go:392`.** The `--detach` branch returns at `runner_linux.go:297` before 392,
  so 392 misses every detached run. Inject at `runner_linux.go:248-269` (after `EnvDigest` at
  :258, following the `stdbufInjection` precedent at :269 — so the two vars stay **out of the
  digest** and reach foreground **and** detached). Add a **detached-run env test**. Also
  confirm `--env`-replace mode (`req.ExplicitEnv`, :250-251) still injects them (stdbuf
  precedent: yes, regardless).
- **A3 (P1-2) — `//go:embed all:<pylibdir>`** (a plain directory embed drops `__init__.py`
  and any `_`/`.`-prefixed file). Keep the embedded dir **pristine**: gitignore
  `__pycache__/`/`*.pyc` so `all:` doesn't churn the content hash per box; the "Python can
  import it" test catches a broken tree.
- **A4 (P1-3) — the concurrency test uses K independent single-process `pytest` invocations,
  not xdist.** xdist is **not installed** on the canonical `make test` box (pytest 9.0.3 is);
  K concurrent single-process runs contend for the N flock slots — this both avoids the xdist
  dependency **and** exercises the real cross-process flock claim. Gate real-python tests on
  **pytest availability only**, and mirror #33: **`AIRA_REAL_PYTEST=1` hard-fails** if pytest
  is absent (like `SkipOrFailRealCgroup`) so coverage can't silently vanish.
- **P2 folds:** `NumCPU` is the right basis on this box (whale.slice has only `CPUWeight`, no
  `CPUQuota`/`AllowedCPUs`; `NumCPU` reads `sched_getaffinity` so a cpuset is honoured); note
  a future `cpu.max` quota would **not** shrink N (accepted advisory drift). The
  reboot-clears-`XDG_RUNTIME_DIR` re-derivation only holds when it's **set** — `PathsFromEnv`
  falls back to a **persistent** `<stateHome>/aira/run` where N never re-derives (logged-N
  mitigates; state it). Open slot fds **`O_CLOEXEC`** + note the `multiprocessing`-fork edge
  (a forked child inherits the fd and can pin a slot past the item — advisory-bounded). A
  **failed loser-validation** (no `.ready` / incomplete slot set) → **log-once + fail-open**;
  a dir with **zero visible slot files** → treat as **inactive** (don't contend to max-wait).
  The daemon may **sweep its own-prefix crash-litter** (`.cpuslots-<pid>-*`, and stale
  `.tmp-<pid>-*` under pylib) at start (it holds the single-instance flock) — cheap, optional. **Branch:**
`codex-aira-cpu-governor`. **Depends on:** M21 (daemon owns machine-local runtime state),
#29/D4 (the RAM-admission precedent + advisory-fail-open stance).
**v2 folds:** the pytest gate is a `pytest_runtest_protocol` **hookwrapper** (wait precedes
every phase → not counted in setup/call/teardown durations; `try/finally` release) not an
in-`runtest_call` wait (Sol r1 P0); the slot dir is created **once and never resized while
locks may be held** — an N change needs a fresh dir at quiescence (Sol r1 P0); fail-open is
**total** (every error class + monotonic clock + unconditional `finally`) (Sol r1 P1); the
cap invariant is scoped to **participating workers, outside bounded fail-open** (Sol r1 P1);
the unsupported "contention-fair / settle near N/2" claim is dropped for **cap-only, no
fairness guarantee** + randomized probe/backoff as light anti-starvation (Sol r1 P1);
extraction is **content-hash**-stamped with an explicit loser path (Sol r1 P2); the
`gc.collect()` claim is softened to opportunistic Python-object reclaim (Sol r1 P2).

## 1. Goal and honest scope

Concurrent test suites oversubscribe the box: several sessions each launch `pytest -n
auto` (or `go test -p`, `make -j`), each grabs every core, and the machine thrashes until
agents retreat to `-n 0` (serial) by hand. AIRA should let those jobs **cooperatively cap
the total number of test workers actually running across the whole machine at ≈ `nproc`**,
flexing dynamically as jobs come and go — so one job uses all cores, two jobs share them,
and nobody has to hand-tune `-n`.

**The shape (owner-specified).** AIRA becomes a **distribution vehicle for per-language
sidecar modules** — code AIRA itself never runs — plus the **shared coordination dir** those
modules use:
- AIRA `go:embed`s the sidecar module source (Python first; Rust/Go/others later), extracts
  it idempotently, and exposes the path via an env var so a project can import it.
- AIRA (the daemon) creates and owns a **machine-wide flock slot dir**; the imported module
  does the actual worker coordination against it.
- v1 ships **one consumer: a Python/pytest xdist governor.** Later languages get their own
  module against the **same** slot dir.

**Dynamic only (owner cut the static budget).** There is no `AIRA_CPU_BUDGET`. xdist keeps
one process per core; the module gates whether each worker **runs the next test**: over the
cap it finishes its current test, `gc.collect()`s (opportunistically reclaiming unreachable
Python objects — not a guaranteed OS-RAM release), and **sleeps** (stops pulling) until a
slot frees; under the cap it **wakes**. **The scoped invariant (Sol r1 P1):** the number of
*participating* workers holding a slot ≤ `N`, **except** during bounded fail-open. It is
**not** a hard "total active workers" cap — non-participating jobs (a plain `pytest` with no
conftest snippet, a `go test` before its module ships) are uncounted, and a max-wait
fail-open deliberately permits transient `> N`. Honestly a *cap on cooperating workers*,
not a machine-wide guarantee.

**Honesty / low-risk invariants (load-bearing):**
- **Advisory, not safety.** CPU oversubscription only costs throughput — there is no
  `OOMPolicy=kill` analog — so the governor is advisory and may be wrong with no worse
  outcome than slightly-suboptimal parallelism.
- **Pure kernel flock, daemon-independent in the hot path.** A slot is an `flock(LOCK_EX|
  LOCK_NB)` on one of `N` files. The kernel releases it when the holder dies — so a crashed
  worker, a killed suite, or a dead daemon **cannot** wedge the semaphore. No token, no
  lease, no reaper.
- **Opt-in and fail-open, always.** Env var unset, conftest not importing the module, slot
  dir absent, or a max-wait exceeded → the module does **nothing** and tests run normally.
  The governor must never stall or fail a suite.

## 2. Current shape (verified)

- Daemon `Paths` (`internal/daemon/paths.go:19`) has `RuntimeDir` (per-user daemon dir under
  `XDG_RUNTIME_DIR`) — the natural home for the ephemeral slot dir; `PathsFromEnv` derives it.
- The wrappers set the child environment via `cmd.Env`: `internal/core/command.go:49`
  (`time`, base built at `command.go:149` from `os.Environ()`), `internal/runner/
  runner_linux.go:392` (`run`). A clean seam to append two env vars.
- No `go:embed` and no `runtime.NumCPU` in the tree yet — both are stdlib, introduced here.
- The `AIRA_DAEMON_*` env convention (`reapIntervalFromEnv` etc.) is the pattern for a
  daemon-level `AIRA_DAEMON_CPU_RESERVE`.

## 3. Design

### 3.1 Embedded module + exposure (`AIRA_PY_LIB`)
`go:embed` the Python sidecar source (a package dir) into the binary. A shared helper
`ExtractPyLib()` extracts it **idempotently and atomically** to a **content-hash**-stamped
dir `<XDG_DATA_HOME>/aira/pylib/<sha256-of-embedded-tree>/` — a content hash, **not** the
build version (Sol r1 P2), so a rebuilt binary whose module bytes changed but version
didn't still re-extracts, and a matching hash is proof the extracted tree is current. Flow:
if `<dir>/.ready` exists → skip (fast path). Else extract into a **private** temp dir
(`<...>/.tmp-<pid>-<rand>/`), write `.ready` last, and publish with **`renameat2(…,
RENAME_NOREPLACE)`** onto `<dir>`. **The loser path (Sol r1 P2 / r3):** `RENAME_NOREPLACE`
fails with **`EEXIST`** if `<dir>` already exists at all (not plain `rename`, which could
replace an empty one); the loser **validates** `<dir>/.ready` (the winner finished) and
**removes its own private temp**. A half-written tree is never published (`.ready` is written
before the rename, so a crashed extractor leaves only an unpublished, never-imported temp). The
wrappers call it once and set **`AIRA_PY_LIB=<that dir>`**. Stable (XDG_DATA, not runtime)
storage — content is stable and reused; old-hash dirs are inert (never auto-deleted mid-run).
AIRA never imports or executes it.

### 3.2 The flock slot dir (the shared primitive, `AIRA_CPU_SLOTS_DIR`)
The daemon, on `Serve` startup, **creates the slot dir `<RuntimeDir>/cpuslots/` once** with
`N = max(1, runtime.NumCPU() − reserve)` empty files `slot-0 … slot-{N-1}` (`reserve` from
**`AIRA_DAEMON_CPU_RESERVE`**, default **1**). The dir is machine-wide (one per user/daemon;
the governed resource is the machine's cores) and created before `Ready`. The wrappers set
**`AIRA_CPU_SLOTS_DIR=<that dir>`** in the child env.

**Atomic publication (Sol r2 P0b).** The N-file set must appear **all-or-nothing** — creating
the dir then adding files one-by-one would let a worker (or a mid-creation daemon crash)
observe a **partial/empty** population and mis-count the cap, possibly permanently. So the
daemon builds the complete set in a **private temp dir** (`<RuntimeDir>/.cpuslots-<pid>-<rand>/`
with all N files) and publishes it with **`renameat2(…, RENAME_NOREPLACE)`** onto
`<RuntimeDir>/cpuslots`. `RENAME_NOREPLACE` (not plain `rename(2)`, which would *replace* an
existing **empty** target — Sol r2/r3 P0b) fails with **`EEXIST`** whenever the target exists
at all. So an observer sees either **no dir** (→ fail-open, no gating) or the **complete
N-file dir**, never a partial one. On `EEXIST` (a prior start / racing daemon already
published), the loser **validates** the existing dir is a complete slot set and removes only
its own temp — it never merges or overwrites (which would re-introduce the resize hazard).
`RENAME_NOREPLACE` is Linux-native, matching AIRA's Linux target.

**Sizing is create-once, never a live resize (Sol r1 P0).** If the dir already exists the
daemon **leaves its file set untouched** — it does **not** add, remove, or replace slot
files to match a changed `NumCPU`/`reserve`, because a worker may hold an `flock` on an
existing file: removing/replacing files while locks are held would split the population into
**disjoint** lock sets and blow the cap. So `N` is fixed at first-creation for the life of
the dir. `XDG_RUNTIME_DIR` is cleared on reboot/logout, so `N` naturally re-derives then; a
mid-life resize is **explicitly unsupported** and requires deleting the dir **at quiescence**
(no workers holding slots). The daemon logs the effective `N` on startup so a stale size is
visible. (Idempotent "ensure exists" — not "reconcile to current size".)

The **coordination is entirely kernel `flock`** and needs no daemon in the hot path: a
worker acquires a slot with `flock(LOCK_EX|LOCK_NB)` scanning `slot-0..N-1` for the first
free one (holding that fd), runs one test, then closes the fd (release). The daemon's only
role is *creating and sizing* the dir; if the daemon is down but the dir exists (from its
last run), workers keep coordinating. If the dir is **absent** (no daemon ever ran) the
module fails open (no gating).

### 3.3 The pytest xdist governor (the v1 consumer)
A pytest plugin (`aira_xdist_governor`) shipped in the embedded package. **Activation is
opt-in**: a project adds a small snippet to `conftest.py` that, *only when `AIRA_PY_LIB` is
set*, puts it on `sys.path` and registers it (documented; unset → the snippet is a no-op,
so committing it is safe for non-AIRA runs). When active:

1. Reads `AIRA_CPU_SLOTS_DIR`; if unset/absent/unreadable → **inactive** (no gating).
2. **Gate via a `pytest_runtest_protocol` hookwrapper (Sol r1 P0), not an in-`call` wait.**
   `@pytest.hookimpl(hookwrapper=True)`: *before* `yield` (which precedes this item's setup,
   call, and teardown) acquire a slot; `yield` (run the item); release in an unconditional
   `finally`. Because the acquire-wait happens **before any phase runs**, it is **not**
   counted in the setup/call/teardown durations pytest reports, and it precedes a
   call-phase timeout. (`pytest-timeout`: the default *signal* method times only the call
   phase → unaffected; the *thread* method times the whole item → a long contention wait
   could count against it, bounded by `AIRA_CPU_MAX_WAIT` — a documented advisory edge, not
   a correctness bug.) A hookwrapper legitimately holds state around all other
   implementations, which a plain hook cannot.
3. **Acquire loop:** scan `slot-0..N-1` in a **randomized order** with `flock(LOCK_EX|
   LOCK_NB)`, holding the first free fd. If none free, `gc.collect()` **once** then sleep a
   randomized `poll_interval ± jitter` (base `AIRA_CPU_POLL_INTERVAL`, ~0.75 s) using a
   **monotonic** deadline, and retry, **re-listing the dir each poll** so a released slot is
   seen promptly. Randomized probe + jittered backoff are a light anti-starvation measure —
   **not** a fairness guarantee (see below).
4. **Bounded, total fail-open (Sol r1 P1):** if no slot is acquired within `AIRA_CPU_MAX_WAIT`
   (default generous, e.g. 5 min, monotonic), run the item anyway. Moreover **any** error —
   malformed `POLL_INTERVAL`/`MAX_WAIT`, permission error, dir vanished/mutated, `open`/
   `flock` failure, interrupted sleep (`EINTR`), or an unexpected exception anywhere in the
   plugin — is caught, logged once, and **disables gating for that item** (run ungoverned).
   The plugin must **never** raise into a test or stall a suite; release is always from an
   unconditional `finally`.
5. Works for any `-n` incl. `-n0`/`-n1` and every `--dist` mode (the gate is per-item,
   distribution-agnostic).

**What is and isn't guaranteed.** The plugin caps *participating* workers running an item at
`≤ N` (outside fail-open); it makes **no fairness guarantee (Sol r1 P1)** — a naive
first-free scan + poll can, under adversarial churn, favour workers with short tests or
transiently starve a suite. Randomized probe + jittered backoff reduce systematic bias;
strict fairness (a real queue / ticket order) is deferred. The `gc.collect()` on sleep may
opportunistically reclaim unreachable Python objects (it does **not** guarantee returning
RAM to the OS — Python arenas) — a minor, best-effort tie-in to the memory pressure #29
governs, not a second RAM mechanism.

### 3.4 Env vars (all set by `aira run`/`aira time`)
`AIRA_PY_LIB` (module dir, §3.1) · `AIRA_CPU_SLOTS_DIR` (slot dir, §3.2) ·
`AIRA_CPU_POLL_INTERVAL`, `AIRA_CPU_MAX_WAIT` (module tunables, defaults baked in).
Daemon-level: `AIRA_DAEMON_CPU_RESERVE`.

## 4. Scope

**In:** §3.1 `go:embed` + idempotent atomic **content-hash** extraction (+ loser path) +
`AIRA_PY_LIB`; §3.2 daemon **create-once** slot dir (`N = NumCPU − reserve`,
`AIRA_DAEMON_CPU_RESERVE`, no live resize) + `AIRA_CPU_SLOTS_DIR`; wrappers append the env
vars to `cmd.Env` (both `run` and `time`); §3.3 the Python `aira_xdist_governor` plugin
(pure-flock, `pytest_runtest_protocol` hookwrapper, randomized probe + jittered monotonic
backoff, `gc`-on-sleep, total fail-open) + the opt-in `conftest` snippet doc.

**Out (stated):** the static `AIRA_CPU_BUDGET` (owner cut it); non-Python language modules
(Rust/Go — later, same slot dir); **any fairness guarantee** (v1 caps concurrency but a
queue/ticket order is deferred — Sol r1 P1); **live resizing** of the slot dir (fixed at
first-creation; change needs a fresh dir at quiescence — Sol r1 P0); any daemon in the
coordination hot path (pure flock); a new RAM mechanism (#29 owns RAM; `gc`-on-sleep is a
best-effort tie-in only); non-Linux (`flock` + the Linux runner are the target).

## 5. Testing

- **Semaphore cap (Go, no python):** many processes each `flock`-acquiring from an `N`-file
  dir + holding briefly → a shared observed-concurrency counter never exceeds `N` (the cap,
  not fairness, is what's asserted).
- **Crash auto-release (Go):** a child killed (`SIGKILL`) while holding a slot → the slot is
  immediately re-acquirable by another process (kernel `flock` release; no reaper).
- **Daemon slot dir create-once + atomic publish (Sol r1/r2 P0):** `Serve` builds the
  `N = max(1, NumCPU − reserve)` files in a private temp dir and `rename`s it into place
  (honours `AIRA_DAEMON_CPU_RESERVE`, incl. reserve ≥ NumCPU → floor 1), before `Ready` —
  assert an observer only ever sees no dir or the **complete** N-file set (never partial),
  including under a simulated mid-creation abort; a **second start with a different
  NumCPU/reserve leaves the existing file set untouched** (rename loses → validate + drop
  temp; no add/remove/replace, no lock disturbed); logs the effective `N`.
- **Extraction (Sol r1 P2):** `ExtractPyLib` is idempotent (skip on `.ready` present), atomic
  (private temp + `rename`), concurrent-safe (two extractors → exactly one published dir, the
  loser validates `.ready` + removes its own temp), re-extracts on a **content-hash** change,
  never publishes a half-written tree; `AIRA_PY_LIB` names a dir Python can import.
- **Wrapper env:** `run` and `time` both export `AIRA_PY_LIB` + `AIRA_CPU_SLOTS_DIR` to the
  child (assert on the child's environment); a store-free `run` still works (env-only, no
  store touch).
- **pytest module (real python, gated behind a `pytest`-available marker):** a conftest
  activating the module + an `N=2` slot dir + a suite instrumented to record max concurrent
  *in-protocol* tests → never exceeds 2 (the **cap**; no `N/2`-fairness assertion — Sol r1
  P1); acquire-wait is **not** counted in the reported call/setup/teardown duration (assert
  a contended test's phase durations exclude wait); `gc.collect` fires only when sleeping.
- **Total fail-open (Sol r1 P1):** the item still runs (never errors, never hangs) for each
  of: `AIRA_CPU_SLOTS_DIR` unset; dir absent/removed mid-run; a malformed
  `POLL_INTERVAL`/`MAX_WAIT`; a permissions error; max-wait exceeded. Assert no exception
  propagates into the test and the suite completes.
- **Opt-out / off:** `AIRA_PY_LIB` unset → the conftest snippet is a no-op; module never
  imported → zero effect.

## 6. Risks

- **R1 — Python surface in a Go project.** *Mitigation:* embedded source only; AIRA never
  runs it; the consumer is opt-in and self-contained; tests that need a real interpreter are
  marked/skipped when `pytest` is absent so `make test` stays hermetic.
- **R2 — xdist hook correctness / duration inflation.** *Mitigation:* a
  `pytest_runtest_protocol` hookwrapper so the wait precedes all phases; a real-python test
  asserts phase durations exclude wait time + it works across `--dist` modes + `-n0`; the
  `pytest-timeout` thread-method edge is documented + bounded by max-wait.
- **R3 — a suite stalls on the governor.** *Mitigation:* total bounded fail-open (any error
  or max-wait → run ungoverned); no daemon in the hot path; kernel-released flocks — no
  deadlock class exists.
- **R4 — extraction races / stale modules.** *Mitigation:* private-temp + `rename`,
  content-hash stamp + `.ready`, idempotent skip, explicit loser path; old-hash dirs inert.
- **R5 — resize splits the lock population.** *Mitigation:* the slot dir is create-once, never
  resized while locks may be held; a change takes effect only on a fresh dir at quiescence
  (reboot clears `XDG_RUNTIME_DIR`); the daemon logs the effective `N`.
- **R6 — advisory drift + no fairness.** *Mitigation:* stated honestly — only jobs importing a
  sidecar module participate (v1 = pytest); fail-open permits transient `> N`; v1 caps
  concurrency but gives no fairness guarantee (randomized backoff only). Never overclaimed as
  a hard machine cap or a fair scheduler.

## 7. Sol build-review checklist

1. Coordination is pure `flock` — daemon absent from the hot path; a crashed worker / killed
   suite / dead daemon cannot wedge a slot (kernel release); no reaper invented.
2. Fail-open is **total**: unset env, absent/removed dir, malformed interval/max-wait,
   permission/`open`/`flock` error, `EINTR`, max-wait, missing conftest, any plugin
   exception → tests run normally; monotonic clock; release from an unconditional `finally`;
   the governor never stalls or fails a suite.
3. Daemon **create-once + atomically-published** slot dir: `N = max(1, NumCPU − reserve)`
   built in a private temp dir and published with **`renameat2(RENAME_NOREPLACE)`** (observers
   see no dir or the complete set, never partial — even on a mid-creation abort), before
   `Ready`, honouring `AIRA_DAEMON_CPU_RESERVE`; a later start with a different size **does
   not** add/remove/replace files (`EEXIST` → validate + drop temp; no disjoint lock
   populations); machine-wide single dir under `RuntimeDir`; effective `N` logged.
4. `go:embed` extraction: idempotent + atomic (private temp + **`renameat2(RENAME_NOREPLACE)`**,
   never plain `rename` over an empty target) + **content-hash**-stamped + concurrent-safe
   with an explicit `EEXIST` loser path (validate `.ready`, clean own temp); `AIRA_PY_LIB`
   importable; AIRA never executes the module; one static binary / no cgo.
5. Both `run` and `time` export `AIRA_PY_LIB` + `AIRA_CPU_SLOTS_DIR`; no store touch added to
   store-free `run`.
6. The pytest gate is a `pytest_runtest_protocol` **hookwrapper** (wait precedes all phases —
   no duration inflation; `pytest-timeout` signal-method unaffected, thread-method edge
   documented + max-wait-bounded); `gc.collect` only on sleep; dir re-listed each poll
   (dynamic wake); randomized probe + jittered backoff; active only when opted-in and the dir
   exists.
7. Honesty: advisory throughput coordination (no OOM-kill analog); dynamic-only (no static
   budget); **cap-only, no fairness guarantee** (queue deferred); the cap is on *participating*
   workers outside fail-open, not a hard machine total; Python is a sidecar AIRA never runs.
