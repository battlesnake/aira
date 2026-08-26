# RAM-weighted pytest governor: per-test memory reservation against the shared aira.slice ledger

Status: PLAN v2 (BUILDER-READY). v1 re-gate: **Fable GATE-PASS-WITH-NITS — Model 3 verified CORRECT
against the #67 ledger** (the daemon charges `max(outstanding, current)`, never the sum, so a
reservation and its realized RSS never double-count, and actual slice usage self-corrects admission
when estimates are wrong). Sol + DeepSeek GATE-FAILed on a double-count / "leases-aren't-enforcement"
reading that the code refutes; their valid residue (fail-open policy, default-must-pin) is folded as
Fable's nits. v2 folds Fable's 3 mandatory P1s + 5 P2s: **(a)** v1 reserves are always PINNED (an
unpinned request with no per-signature history applies the machine-wide p90 prior over ALL confine
peaks — admit.go:198 — ballooning a light test); **(b)** `confine-reserve` is DAEMON-ONLY (no flock
fallback — else a daemon-down test holds the machine-wide flock for its whole runtime); **(c)** the
governor env is coupled to `--delegate-ram` by construction (stripped on non-delegate launches);
plus the marker grammar, fork-stdin close, fail-open policy, dispatch template, and test additions.
Owner-initiated (2026-08-26); owner-chosen fork: reuse #67's shared ledger.

## 1. Motivation & root cause (verified)

`internal/pylib/aira_xdist_governor/__init__.py` holds one kernel `flock` on a `slot-N` file
(`AIRA_CPU_SLOTS_DIR`, an immutable ~nproc pool from `internal/daemon/cpuslots.go`) per test →
bounds concurrent test COUNT, RAM-blind. The real `-n16` failure was inter-suite (two sessions'
suites filled the shared slot pool; 15 multi-GB corpus tests exceeded the 64 GiB slice → OOM). Only a
SHARED RAM ledger bounds that. #67 is exactly that ledger: the daemon's `aira.slice` admission holds
Σ granted ≤ cap − headroom and **charges `max(outstanding, current)`** (`checkedAvailable`,
admit.go:494-507), so it accounts a job by the greater of its reservation and its live RSS.

## 2. Design — per-test RAM reservation against the shared #67 ledger

Per test, in `pytest_runtest_protocol`, compose two independent, both-fail-open gates:
1. the existing **CPU-slot flock** (bounds count ≈nproc) — unchanged, acquired FIRST;
2. a new **RAM reservation** of the test's estimate, held for the test's duration, granted by the
   daemon's #67 admission against `aira.slice`.

A heavy test blocks in admission until the slice budget has room; light tests admit instantly and
parallelise. Because RAM-acquire follows slot-acquire, concurrent held reservations ≤ ~nproc
(≪ `admitMaxWaiters=256`, admit.go:24). A slot-holding test waiting on RAM holds NO reservation, so
all `outstanding` belongs to running jobs that complete → no deadlock. Crash-safe: the daemon peer-
read goroutine (admit.go:303) + idempotent `releaseAdmitWaiter` (:523) free the lease on ANY
connection close, including `kill -9` / oom.group of the whole suite.

### 2a. Suite/test accounting — Model 3, verified correct

The pytest invocation runs as ONE confine job (`aira confine --delegate-ram -- pytest -n N`). Two
code facts make per-test reservations sound WITHOUT double-counting:
- **`max(outstanding, current)` charging** (admit.go:494): the suite's realized RSS (`current`) and
  the per-test reservations (`outstanding`) never sum — the ledger takes the greater. So the tests'
  reservations and their actual memory are one charge, not two.
- **Auto-sub-cap opt-out**: an admitted confine grant is auto-enforced as the suite scope's
  `memory.max` (confine_linux.go:450-460). A per-test-on-top-of-a-suite-reservation design would
  OOM-kill the whole suite at the (small) suite reserve regardless of test grants. **`--delegate-ram`
  suppresses ONLY the `confine_linux.go:458` auto-sub-cap write**; `oom.group` (:446) and the finite-
  cap refuse-precondition (:354) are upstream and REMAIN — containment intact (an over-running suite
  is oom.group-killed within the slice, never a random victim). The suite's own reservation is a
  minimal framework overhead via the existing `--memory-reserve` flag (cmd/aira/main.go:580, already
  auto-pinned, confine_linux.go:366).
- **Inter-suite bound holds**: both suites' per-test reservations land in the same per-slice
  `queue.outstanding`, so Σ across sessions ≤ cap − headroom (the `-n16` fix). Confirmed necessary +
  sufficient; no cleaner model found.

## 3. Estimate model (annotation > pinned default; both PINNED in v1)

Per-test estimate precedence, both arms **pinned** in v1 (P1-a — an unpinned reserve with no
`pytest:<nodeid>` history hits `resolveAdmitReserve`'s p90 prior over ALL confine peaks, admit.go:198,
ballooning a light test to a whole-command footprint):
1. **`aira_mem` pytest marker** — test / class / module scope (marker inheritance gives suite/group:
   `@pytest.mark.aira_mem("4G")`, or `pytestmark = pytest.mark.aira_mem("512M")` at module top). Value
   grammar is **exactly `runner.ParseMemorySize`** (`[0-9]+[KMGkmg]?`, memory_size.go:15) — `"4G"`,
   `"512M"`, or a bare byte int; **NOT** `"4GB"`/`"512MiB"`. Parsed IN PYTHON (a ~5-line grammar
   mirror) so an unparseable marker falls to the default + one log (§7) rather than being an
   indistinguishable helper error. Sent as a **pinned** reserve.
2. **configured nonzero pinned default** (`AIRA_TEST_MEM_DEFAULT`, e.g. `512M`) when no marker —
   nonzero is load-bearing (a 0 default admits unlimited heavies = the `-n16` failure). Sent pinned.
- **History self-calibration is DEFERRED to v2** (§10). In v1 both arms pin, so #67's estimate/p90
  never applies to tests (correct — avoids the balloon). v2 adds per-test peak reporting → an unpinned
  history-estimated default.

## 4. The reserve helper — `aira confine-reserve` (DAEMON-ONLY, mirrors `confine`)

A new **CLI-only** verb `aira confine-reserve --bytes N --pinned --signature pytest:<nodeid>
[--slice …] [--max-wait …]` that mirrors `confine` itself (NOT `confine-list`): `core.Do` returns
`E_CONFINE_UNAVAILABLE` with `Include:false` for MCP (core.go:1667-1684 precedent), and it resolves
the admit socket directly (main.go:681). It issues the #67 admit **DAEMON-ONLY** — it must NOT fall
through to `admitWithFlock` (admission_linux.go:126-138): a per-test flock fallback would hold the
machine-wide EXCLUSIVE flock for the test's whole runtime, collapsing the run to ONE governed test
while others stall `max_wait` (P1-b). Expose a no-fallback path (`admitThroughDaemon` / a
`NoFlockFallback` admit mode); a dial failure ⇒ **instant nonzero exit** (fail open, §7). On GRANT it
prints one line to stdout (`granted reserve=<n> basis=pinned:client`) and **holds the connection
open, blocking on stdin, until stdin closes / it is signalled**, then releases (connection close =
#67 lease release). Reuses the exact Go admit client (proto-5 framing + grant validation + lease
arbitration all stay in Go); Python speaks no wire protocol.

## 5. Plugin changes (`aira_xdist_governor`)

In `pytest_runtest_protocol`, after the CPU slot: resolve the estimate (marker → default, parsed in
Python), spawn `aira confine-reserve --bytes <est> --pinned --signature pytest:<nodeid>` (path via
`AIRA_CONFINE_RESERVE_CMD`), wait (bounded) for the `granted` line; on grant run the test then close
the helper's stdin in `finally` (releasing the reservation); on any failure/timeout run ungoverned
(`_log_once`). **Fork safety (P2-b):** register the helper's stdin write-fd in the held-descriptor set
and close it in the existing `after_in_child` at-fork hook (__init__.py:25-35) — else a
`multiprocessing` child inherits it and stdin-close won't release until the child exits. All new
behaviour gates on `AIRA_TEST_MEM_GOVERNOR`.

## 6. The `aira_mem` marker + no-op shim (owner's explicit ask)

The plugin **registers** the marker (`pytest_configure` `config.addinivalue_line`, no unknown-marker
warning) and reads its value. When the AIRA plugin is NOT loaded, the loader/conftest shim provides a
**no-op `aira_mem`** so test files annotate unconditionally with zero hard dependency; the marker is
then inert metadata. Ship the shim beside the plugin.

## 7. Honesty & fail-open policy (advisory; contained by oom.group + slice cap)

- Ungoverned-after-failure is still in-slice contained (finite-cap precondition + oom.group), so
  desktop-safe; the risk is a slice-level OOM taking the victim scope (the whole delegate-ram suite).
  Acceptable for advisory v1, with two refinements (P2-c):
  - **SATURATED** ⇒ a generous bounded WAIT before falling open (mirror the CPU governor's 300 s;
    the daemon caps at 30 min) — fail-open at peak saturation is the worst moment.
  - **TOO_LARGE** (a pinned marker > ceiling) can never succeed by waiting ⇒ **clamp to the ceiling +
    re-admit** (serialises the monster) rather than run ungoverned; at minimum a loud log.
  - daemon-down / helper-missing / other ⇒ instant ungoverned + one log (like the CPU governor).
- An unparseable marker ⇒ default + one log (never a fabricated 0 or a hard error). The helper reports
  the resolved reserve + basis honestly. The ONE ledger is #67's slice admission (no second RAM
  accountant).

## 8. Code map (seams verified on master 56bd635)

- `internal/pylib/aira_xdist_governor/__init__.py`: RAM acquire/hold/release around the CPU flock;
  `aira_mem` registration + Python-side `[KMG]` parse; the no-op shim; fork-stdin close; gated on
  `AIRA_TEST_MEM_GOVERNOR`.
- `internal/pylib/env.go`: **add `AIRA_TEST_MEM_GOVERNOR`, `AIRA_TEST_MEM_DEFAULT`,
  `AIRA_CONFINE_RESERVE_CMD` to `governorEnvironmentKeys` (:18-23)** so `AppendChildEnvironment`
  (confine_linux.go:508) STRIPS inherited values on every launch and sets them **iff `--delegate-ram`**
  (P1-c — a governed plugin under a non-delegate launch would recreate the double-count + sub-cap
  failure; coupling by construction makes the mismatch impossible). `extract.go` as needed.
- `cmd/aira/main.go` + `internal/core/core.go`: the `confine-reserve` CLI-only verb (mirrors
  `confine`: `E_CONFINE_UNAVAILABLE` from `core.Do`, `Include:false`; admit socket via main.go:681);
  the `--delegate-ram` flag on the confine launch (suppress ONLY the confine_linux.go:458 sub-cap
  write). Reuse `runner.ParseMemorySize` (memory_size.go:15).
- `internal/runner/admission_linux.go`: a daemon-only admit path (no `admitWithFlock` fallback) for
  `confine-reserve`; the pinned frame (:336-341) already carries `pinned:client`.
- `internal/runner/confine_linux.go`: `--delegate-ram` gates the :458 sub-cap write; :446 oom.group +
  :354 finite-cap refuse untouched.
- Skill/guide: document `aira_mem` (`"4G"`/`"512M"`), the nonzero default, and that heavy suites run
  `aira confine --delegate-ram -- pytest …`.

## 9. Tests (TDD; pure where possible; Go/CLI under `aira confine`)

- **Estimate precedence:** marker (pinned) beats default; module/class/test scope resolves; unparseable
  marker → default + one log; helper round-trip proves `basis="pinned:client"`.
- **RAM-bounded concurrency (`-n16` fix):** tiny fake slice budget + heavy estimates ⇒ concurrent
  admitted tests never exceed budget (heavy serialise, light don't block); two simulated suites'
  reservations share `queue.outstanding` ≤ cap. (Go via the #67 admit seam + plugin-level with a fake
  helper.)
- **Daemon-only / fail-open:** daemon-down ⇒ `confine-reserve` exits nonzero INSTANTLY, the flock
  fallback is NEVER engaged; the test still runs (ungoverned) + one log.
- **Lease lifecycle:** grant → holds until stdin close → releases exactly once; `kill -9` of the
  helper ⇒ lease released exactly once (connection-close path).
- **delegate-ram:** suppresses ONLY the :458 auto-sub-cap while oom.group (:446) AND the finite-cap
  refuse (:354) both still hold; a non-delegate launch STRIPS the governor env (mode mismatch
  impossible).
- **No-op shim:** `aira_mem` importable + inert when the plugin isn't loaded; registered (no warning)
  when it is. **Fork:** a forking test's child does not pin the reservation past its own exit.
- Regression: CPU-only governance unchanged when `AIRA_TEST_MEM_GOVERNOR` unset; #67 confine admission
  unchanged. `go build/vet ./... && go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/
  ./internal/pylib/ -race`; `make test` under `aira confine`; the plugin's own pytest suite.

## 10. Residual risk & deferrals (stated)

- **History self-calibration deferred to v2** — needs reliable per-test peak-RSS capture in a shared
  xdist worker; v1 pins annotation-or-default (never self-tightens). The marker is the seed.
- `--delegate-ram` opts the suite OUT of #67's per-scope sub-cap, so a suite's containment is
  `oom.group` + the slice cap (+ watchdog), NOT a per-suite `memory.max`; an over-running delegate-ram
  suite is killed as one oom.group unit within the slice (contained, never a random victim), but is
  coarser than a per-suite cap. This is the deliberate cost of delegating RAM to per-test reservations.
  The per-test governor keeps Σ concurrent estimates under budget so the suite normally never reaches
  the slice cap.
- The per-test daemon round-trip is fully fail-open (daemon down ⇒ ungoverned); fork cost (~ms) is
  negligible beside a memory-heavy test. Estimates are cooperative/advisory; the slice cap + watchdog
  are the hard backstop.
