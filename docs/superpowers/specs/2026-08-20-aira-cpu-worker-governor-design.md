# Embedded sidecar modules + dynamic cross-session CPU-worker governor, v1

**Status:** plan (pre-review). **Milestone:** Phase 5 · CPU-worker governor (task #49).
**Branch:** `codex-aira-cpu-governor`. **Depends on:** M21 (daemon owns machine-local
runtime state), #29/D4 (the RAM-admission precedent + advisory-fail-open stance).

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
cap it finishes its current test, `gc.collect()`s, and **sleeps** (stops pulling) until a
slot frees; under the cap it **wakes**. Total *active* workers across all jobs ≤ `N`.

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
`ExtractPyLib()` extracts it **idempotently and atomically** to a **versioned** dir
`<XDG_DATA_HOME>/aira/pylib/<version>/` (`<version>` = the aira build version or a content
hash): if the dir already exists with a matching version-stamp, skip; else extract to a
temp dir and `rename` into place (atomic; concurrent extractors race harmlessly on the
rename). The wrappers call it once and set **`AIRA_PY_LIB=<that dir>`** in the child env.
Stable (not runtime) storage because the source content is stable and reused across runs.
AIRA never imports or executes it.

### 3.2 The flock slot dir (the shared primitive, `AIRA_CPU_SLOTS_DIR`)
The daemon, on `Serve` startup, creates a **machine-wide** slot dir
`<RuntimeDir>/cpuslots/` containing `N = max(1, runtime.NumCPU() − reserve)` empty files
`slot-0 … slot-{N-1}` (idempotent: create missing files, never delete extras). `reserve`
comes from **`AIRA_DAEMON_CPU_RESERVE`** (default **1** — leave a core for the OS/daemon).
The dir is created before `Ready` and is machine-wide (one per user/daemon, shared by every
session and project — the resource being governed is the machine's cores). The wrappers set
**`AIRA_CPU_SLOTS_DIR=<that dir>`** in the child env.

The **coordination is entirely kernel `flock`** and needs no daemon in the hot path: a
worker acquires a slot with `flock(LOCK_EX|LOCK_NB)` scanning `slot-0..N-1` for the first
free one (holding that fd), runs one test, then closes the fd (release). The daemon's only
role is *creating and sizing* the dir; if the daemon is down but the dir exists (from its
last run), workers keep coordinating. If the dir is **absent** (no daemon ever ran) the
module fails open (no gating).

### 3.3 The pytest xdist governor (the v1 consumer)
A pytest plugin (`aira_xdist_governor`) shipped in the embedded package. **Activation is
opt-in**: a project adds a two-line snippet to `conftest.py` that, *only when
`AIRA_PY_LIB` is set*, puts it on `sys.path` and registers it (documented; when unset the
snippet is a no-op, so committing it is safe for non-AIRA runs). When active it:
1. Reads `AIRA_CPU_SLOTS_DIR`; if unset/absent → **inactive** (no gating).
2. Gates each test **before `pytest_runtest_call`** (outside the timed body, so wait time
   never inflates durations or trips per-test timeouts): try to `flock` a free slot; if
   none free, `gc.collect()` **once** then sleep `poll_interval` (default ~0.75 s, config
   `AIRA_CPU_POLL_INTERVAL`) and retry, **re-scanning the dir each poll** so it wakes as
   soon as another job releases a slot. Hold the slot across the test; release after.
3. **Bounded fail-open:** if no slot is acquired within a max wait
   (`AIRA_CPU_MAX_WAIT`, default generous, e.g. 5 min), run the test anyway (advisory —
   never stall a suite; a genuinely stuck peer must not deadlock this one).
4. Works for any `-n` incl. `-n0`/`-n1` (single worker just rarely contends) and every
   `--dist` mode (the gate is per-test-execution, distribution-agnostic).

Total workers *running a test* across all jobs ≤ `N`; two suites contend equally for freed
slots and settle near `N/2` each; when one ends its slots free and the other's sleeping
workers wake on their next poll. The `gc.collect()` on sleep is a deliberate second win —
a sleeping worker also **releases RAM**, easing the memory contention #29 governs.

### 3.4 Env vars (all set by `aira run`/`aira time`)
`AIRA_PY_LIB` (module dir, §3.1) · `AIRA_CPU_SLOTS_DIR` (slot dir, §3.2) ·
`AIRA_CPU_POLL_INTERVAL`, `AIRA_CPU_MAX_WAIT` (module tunables, defaults baked in).
Daemon-level: `AIRA_DAEMON_CPU_RESERVE`.

## 4. Scope

**In:** §3.1 `go:embed` + idempotent atomic versioned extraction + `AIRA_PY_LIB`; §3.2
daemon slot-dir creation (`N = NumCPU − reserve`, `AIRA_DAEMON_CPU_RESERVE`) +
`AIRA_CPU_SLOTS_DIR`; wrappers append the env vars to `cmd.Env` (both `run` and `time`);
§3.3 the Python `aira_xdist_governor` plugin (pure-flock, gated before `runtest_call`,
`gc`-on-sleep, dynamic re-scan, bounded fail-open) + the opt-in `conftest` snippet doc.

**Out (stated):** the static `AIRA_CPU_BUDGET` (owner cut it); non-Python language modules
(Rust/Go — later, same slot dir); strict per-job fairness *quotas* (v1 is contention-fair,
which already caps oversubscription — the actual goal); any daemon involvement in the
coordination hot path (pure flock); a new RAM mechanism (#29 owns RAM; `gc`-on-sleep is the
only tie-in); non-Linux (`flock` + the Linux runner are the target, matching AIRA).

## 5. Testing

- **Semaphore cap (Go, no python):** many processes each `flock`-acquiring from an `N`-file
  dir + holding briefly → a shared observed-concurrency counter never exceeds `N`; two
  "jobs" contend fairly (neither starves).
- **Crash auto-release (Go):** a child killed (`SIGKILL`) while holding a slot → the slot is
  immediately re-acquirable by another process (kernel `flock` release; no reaper).
- **Daemon slot dir:** `Serve` creates `N = NumCPU − reserve` files idempotently (missing
  ones added, extras untouched), honours `AIRA_DAEMON_CPU_RESERVE` (incl. reserve ≥ NumCPU →
  floor 1), before `Ready`; survives a restart.
- **Extraction:** `ExtractPyLib` is idempotent (skip on version match), atomic
  (temp+rename), concurrent-safe (two extractors → one valid dir), re-extracts on version
  change; `AIRA_PY_LIB` names a dir Python can import.
- **Wrapper env:** `run` and `time` both export `AIRA_PY_LIB` + `AIRA_CPU_SLOTS_DIR` to the
  child (assert on the child's environment); a store-free `run` still works (env-only, no
  store touch).
- **pytest module (real python, gated behind a `pytest`-available marker):** a conftest
  activating the module + an `N=2` slot dir + a suite instrumented to record max concurrent
  in-`runtest_call` tests → never exceeds 2; wait time is **not** counted in test duration;
  `gc.collect` fires only when sleeping; **fail-open** when `AIRA_CPU_SLOTS_DIR` unset and
  when max-wait exceeded (test still runs); inactive when the conftest snippet isn't present.
- **Opt-out / off:** `AIRA_PY_LIB` unset → the conftest snippet is a no-op; module never
  imported → zero effect.

## 6. Risks

- **R1 — Python surface in a Go project.** *Mitigation:* embedded source only; AIRA never
  runs it; the consumer is opt-in and self-contained; tests that need a real interpreter are
  marked/skipped when `pytest` is absent so `make test` stays hermetic.
- **R2 — xdist hook correctness / duration inflation.** *Mitigation:* gate strictly before
  `pytest_runtest_call`; a real-python test asserts durations exclude wait time and that it
  works across `--dist` modes + `-n0`.
- **R3 — a suite stalls on the governor.** *Mitigation:* bounded fail-open (`AIRA_CPU_MAX_
  WAIT` → run anyway); no daemon in the hot path; kernel-released flocks — no deadlock class
  exists.
- **R4 — extraction races / stale versions.** *Mitigation:* atomic temp+rename, version-
  stamped dir, idempotent skip; old versions are inert (never auto-deleted mid-run).
- **R5 — advisory drift (a non-participating job oversubscribes).** *Mitigation:* stated
  honestly — only jobs whose runner imports a sidecar module participate; v1 covers pytest;
  other languages join as their module lands. Never overclaimed as a hard machine cap.

## 7. Sol build-review checklist

1. Coordination is pure `flock` — daemon absent from the hot path; a crashed worker / killed
   suite / dead daemon cannot wedge a slot (kernel release); no reaper invented.
2. Fail-open is total: unset env, absent dir, max-wait, missing conftest → tests run
   normally; the governor never stalls or fails a suite.
3. Daemon creates `N = max(1, NumCPU − reserve)` slot files idempotently (missing added,
   extras kept), before `Ready`, honouring `AIRA_DAEMON_CPU_RESERVE`; machine-wide single
   dir under `RuntimeDir`.
4. `go:embed` extraction is idempotent + atomic (temp+rename) + version-stamped +
   concurrent-safe; `AIRA_PY_LIB` importable; AIRA never executes the module; one static
   binary / no cgo preserved.
5. Both `run` and `time` export `AIRA_PY_LIB` + `AIRA_CPU_SLOTS_DIR`; no store touch added to
   store-free `run`.
6. The pytest gate sits before `pytest_runtest_call` (no duration inflation / timeout trip);
   `gc.collect` only on sleep; dir re-scanned each poll (dynamic wake); active only when
   opted-in and the dir exists.
7. Honesty: advisory throughput coordination (no OOM-kill analog); dynamic-only (no static
   budget); contention-fair (quotas deferred); Python is a sidecar AIRA never runs.
