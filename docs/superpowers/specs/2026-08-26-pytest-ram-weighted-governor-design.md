# RAM-weighted pytest governor: per-test memory reservation against the shared aira.slice ledger

Status: PLAN v1 (for the Sol + DeepSeek + Fable plan-gate). Owner-initiated (2026-08-26): the #49 CPU
governor caps concurrent test COUNT (≈nproc uniform flock slots) but treats every test as equal
weight; memory-heavy tests (multi-GB OHW-corpus items) fill all slots and blow the 64 GiB `aira.slice`
cap even though the count is bounded — a real `-n16` OOM across two sessions' overlapping suites. Owner
asked for a per-test/suite/group RAM-estimate **annotation**, a **no-op shim** when the AIRA plugin is
absent, and a **sane nonzero default**. Owner-chosen fork (brainstormed): **reuse #67's daemon
admission** (one honest ledger shared by confine jobs and pytest workers) over a governor-local pool.

## 1. Motivation & root cause (verified)

`internal/pylib/aira_xdist_governor/__init__.py` holds one kernel `flock` on a `slot-N` file
(`AIRA_CPU_SLOTS_DIR`, an immutable pool of `NumCPU-reserve` slots created by
`internal/daemon/cpuslots.go`) across each test's setup/call/teardown. This bounds concurrent test
COUNT to ≈nproc but is RAM-blind: N uniform slots × heterogeneous per-test memory can exceed the slice
cap. The `-n16` failure was **inter-suite** (two sessions' suites filled the shared slot pool
together), so only a **shared** RAM ledger — not a suite-local one — bounds it. #67 already maintains
exactly such a ledger: the daemon's `aira.slice` admission holds Σ granted ≤ cap − headroom.

## 2. Design — per-test RAM reservation against the shared #67 ledger

Compose two independent, both-fail-open gates per test (in `pytest_runtest_protocol`, around the
existing CPU-slot acquire):
1. the existing **CPU-slot flock** (bounds count ≈nproc) — unchanged;
2. a new **RAM reservation** of the test's estimate, held for the test's duration, granted by the
   daemon's #67 admission against `aira.slice` (Σ granted ≤ cap − headroom).

A memory-heavy test therefore blocks in admission until the slice budget has room, while light tests
(small estimate) admit instantly and parallelise freely — so aggregate concurrent test RAM across ALL
sessions' suites stays under the slice cap. The reservation is released the instant the test finishes.

### 2a. The suite/test double-counting problem (KEY GATE QUESTION)

The pytest invocation itself runs under `aira confine -- pytest -n N` (heavy-command policy), i.e. as
ONE confine job with its own slice reservation + scope. The tests' memory lives INSIDE that scope. If
each test ALSO reserves against the slice on top of a suite-sized reservation, the slice ledger
double-counts (suite reservation + test reservations both charge, but the tests' RAM is within the
suite's already-charged scope). Proposed resolution (**Model 3**, for gate scrutiny):

- The pytest confine job **delegates its RAM accounting to the per-test reservations**: it launches
  with a MINIMAL framework-overhead reservation and **no per-scope `memory.max` sub-cap** (a confine
  launch mode: `--memory-reserve=<overhead>` + opt-out of the #67 estimate sub-cap, e.g.
  `AIRA_CONFINE_DELEGATE_RAM=1` / a `--delegate-ram` flag), so the slice ledger is charged by the
  governor's per-test reservations, not a big suite reservation. Containment (oom.group) + slice-
  boundedness remain; the slice cap + watchdog are the backstop.
- Alternatives the gate should weigh: (A) the governor reserves against a SUITE-LOCAL budget = the
  suite's own confine `memory.max` (bounds intra-suite only — does NOT fix the inter-suite `-n16`
  case, so rejected as the primary); (B) the governor's reservations are the suite's reservation —
  the suite reserves 0 up front and the tests reserve incrementally (≈Model 3). This interaction is
  the single most load-bearing correctness question and is called out explicitly.

## 3. Estimate model (annotation > default; history deferred)

Per-test estimate precedence:
1. **`aira_mem` pytest marker** — appliable at test / class / module scope (pytest marker inheritance
   gives "suite or group" for free: `@pytest.mark.aira_mem("4GB")`, or `pytestmark =
   pytest.mark.aira_mem("512MB")` at module top). Value: a human size (`"4GB"`,`"512MiB"`, or an int
   of bytes), parsed by the existing `[KMG]` parser lineage. The marker maps to #67's **pinned
   reserve** (the daemon uses it verbatim, never estimate-overrides it, still ≤ ceiling).
2. **configured nonzero default** (`AIRA_TEST_MEM_DEFAULT`, e.g. 512 MiB) when no marker — **nonzero
   is load-bearing**: a zero default would let unlimited unannotated heavies through (the exact
   `-n16` failure). Passed as the reservation when unpinned.
- **History self-calibration is DEFERRED to v2** (§10): reliable per-test peak-RSS capture inside a
  shared long-lived xdist worker (whose RSS accumulates across tests) is genuinely hard; the marker is
  the seed until then. #67's per-signature history + p90 prior still apply automatically IF a test
  signature ever accrues samples, but v1 ships no active per-test peak reporting.

## 4. The reserve helper (thin CLI reusing #67 admission — no protocol duplication)

A new project-less verb `aira confine-reserve --bytes N [--signature S] [--pinned] [--slice …]`
(dispatch-table, like `confine-list`): it resolves `aira.slice`, opens the daemon admit socket, and
issues the #67 admit (reserve=N, pinned iff a marker was given, signature=S). On GRANT it prints one
line to stdout (`granted reserve=<n> basis=<basis>`) and **holds the connection open, blocking on
stdin, until stdin closes or it is signalled**, then releases (connection close = #67 lease release,
exactly as confine holds its lease). This reuses the exact Go admit client — Python speaks NO wire
protocol. Non-grant outcomes (daemon-unavailable, `E_ADMIT_TOO_LARGE`, `E_ADMIT_SATURATED` after the
bounded wait) exit nonzero with the code on stdout → the plugin **fails open** (runs the test
ungoverned for RAM, logs once), consistent with the CPU governor.

## 5. Plugin changes (`aira_xdist_governor`)

In `pytest_runtest_protocol`, after acquiring the CPU slot: resolve the test's estimate (marker →
default), spawn `aira confine-reserve --bytes <est> [--pinned] --signature pytest:<nodeid>` (path via
`AIRA_CONFINE_RESERVE_CMD`/`aira` on PATH), wait (bounded) for the `granted` line; on grant run the
test then close the helper's stdin in `finally` (releasing the reservation); on any failure/timeout
run ungoverned (log once). All new behaviour is gated on an env the loader sets (e.g.
`AIRA_TEST_MEM_GOVERNOR=1`); absent → today's CPU-only behaviour, unchanged. The signature is
`pytest:<nodeid>` (namespaced so test history never collides with confine command signatures).

## 6. The `aira_mem` marker + no-op shim (owner's explicit ask)

The plugin **registers** the `aira_mem` marker (via `pytest_configure` `config.addinivalue_line`,
so pytest emits no unknown-marker warning) and reads its value. When the AIRA plugin is NOT loaded
(no daemon / opt-out), the loader/conftest shim provides a **no-op `aira_mem`** so test files annotate
unconditionally with zero hard dependency on AIRA — the marker is then inert metadata pytest ignores.
Ship the shim beside the plugin so a repo can vendor it.

## 7. Honesty & fail-open (advisory, never breaks a test)

- No daemon / helper missing / helper error / admit timeout / too-large ⇒ the test runs **ungoverned**
  (logged once, like the CPU governor's `_log_once`); a wrong estimate only mis-sizes admission, it
  never fails or skips a test. RAM governance is advisory, exactly as CPU governance is.
- The helper reports the resolved reserve + #67 basis honestly; an unparseable marker ⇒ fall to the
  default + log once (never a fabricated 0 or a hard error).
- No new daemon RAM accountant: the ONE ledger is #67's slice admission (owner's shared-ledger choice).

## 8. Code map (seams, verified on master 56bd635)

- `internal/pylib/aira_xdist_governor/__init__.py`: add the RAM-reservation acquire/hold/release
  around the CPU-slot flock; `aira_mem` marker registration + value parse; the no-op shim; all gated
  on `AIRA_TEST_MEM_GOVERNOR`. `internal/pylib/{env.go,extract.go}`: wire the new env
  (`AIRA_TEST_MEM_DEFAULT`, `AIRA_CONFINE_RESERVE_CMD`, `AIRA_TEST_MEM_GOVERNOR`).
- `cmd/aira/main.go` + `internal/core/core.go`: the `confine-reserve` verb (project-less, SafetyExecute;
  reuses `runner.admit`/the #67 client — `admission_linux.go:339` signature/pinned frame; holds the
  lease like confine). Reuse the `[KMG]` size parser (#57).
- `internal/runner/confine*.go`: the `--delegate-ram` / minimal-overhead launch mode for the pytest
  suite (§2a) — opt out of the per-scope estimate sub-cap while keeping oom.group + slice-boundedness.
- Skill/guide: document `aira_mem`, the nonzero default, and that heavy suites run
  `aira confine --delegate-ram -- pytest …`.

## 9. Tests (TDD; pure where possible; the Go/CLI parts under `aira confine`)

- **Estimate precedence:** marker (pinned) beats default; module/class/test marker scope resolves
  (inheritance); unparseable marker → default + one log.
- **RAM-bounded concurrency (the `-n16` fix):** with a tiny fake slice budget + heavy per-test
  estimates, concurrent admitted tests never exceed the budget (heavy tests serialise); light tests
  don't block. (Go-level via the #67 admit seam + a plugin-level integration test with a fake helper.)
- **Fail-open:** helper missing / daemon down / admit timeout / too-large ⇒ the test still runs
  (ungoverned), one log line, non-zero helper exit tolerated.
- **No-op shim:** `aira_mem` importable + inert when the plugin isn't loaded; marker registered (no
  unknown-marker warning) when it is.
- **confine-reserve verb:** grant → holds until stdin close → releases exactly once (#67 lease
  reuse); rejection surfaces the code + exits nonzero; project-less.
- **Double-count avoidance (§2a):** the delegate-ram suite launch reserves only overhead + no sub-cap;
  per-test reservations charge the slice; Σ across two simulated suites ≤ cap.
- Regression: CPU-only governance unchanged when `AIRA_TEST_MEM_GOVERNOR` unset; #67 confine admission
  unchanged. `go build/vet ./... && go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/
  ./internal/pylib/ -race`; `make test` under `aira confine`; the Python plugin's own pytest suite.

## 10. Residual risk & deferrals (stated)

- **History self-calibration deferred to v2** — needs reliable per-test peak-RSS capture in a shared
  xdist worker (a sampled high-water-mark minus baseline); until then unannotated tests use the
  configured default (never self-tighten). The marker is the seed.
- The per-test daemon round-trip (via the helper) reintroduces a daemon dependency in the test hot
  path; it is fully fail-open (daemon down ⇒ ungoverned), and the fork cost (~ms) is negligible beside
  a memory-heavy test.
- `owner`/estimate are cooperative and advisory; a deliberately-wrong annotation only mis-sizes its own
  admission. The slice cap + watchdog remain the hard backstop.
- The §2a suite/test-reservation interaction is the load-bearing design point the gate must confirm.
