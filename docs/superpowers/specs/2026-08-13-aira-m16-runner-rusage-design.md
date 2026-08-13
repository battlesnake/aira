# M16 — cgroup resource accounting at exit (peak_rss · CPU · oom-killed)

Status: PLAN (awaiting Sol plan-review → gate → build). Branch `codex-aira-m16` off master `0f4ca96`.
Milestone: Phase 5 (full subprocess runner), first cut. Spec §14 ("Resource accounting from the cgroup,
at exit") + §101 (Run record `cpu_user/cpu_sys/peak_rss?/status(...|oom-killed|...)`).
Class: **runner + cgroup correctness → the two-loop is mandatory; real-cgroup verification is load-bearing**
(the Codex sandbox has a read-only cgroup mount → real-cgroup tests SKIP there — the M12 lesson: my
real-cgroup gate caught 3 safety bugs the sandbox missed).

## 0. Context — the gap

M12 runner-lite launches, captures to files, and scope-kills via cgroup-v2, but **reads no cgroup
resource stats** and has **no `oom-killed` status** (statuses today: `starting|running|exited|killed|
lost`, types.go:11-19; `RunRecord` has no `peak_rss`/`cpu_*` fields). The spec §14 requires
peak-RSS/CPU/OOM read **from the scope's cgroup, at child-exit** (the cgroup is removed when it empties),
and §101 defines `oom-killed = cgroup memory.events oom_kill fired`, `peak_rss? = unevaluated` for a bare
run with no cgroup ("never a fake number"). This milestone closes that gap. It provides the DATA for the
§17 estimate-vs-actual gauge and data-driven `MemoryMax` tuning; those consumers are OUT of scope.

## 1. Scope

**IN:**
1. **Read cgroup-v2 stats at child-exit** — after `cmd.Wait` returns and the wait evidence is recorded,
   BEFORE the scope is removed (runner_linux.go ~283-430; scope teardown ~429): `memory.peak` (peak RSS
   bytes), `cpu.stat` (`user_usec`, `system_usec`), `memory.events` (`oom_kill` count) from the run's
   scope cgroup path.
2. **New `RunRecord` fields** (all `*int64`, nil = **unevaluated**, mirroring the M14 disjoint-bucket
   nullable discipline): `PeakRSS` (bytes), `CPUUser` (µs), `CPUSys` (µs). Persisted in the ledger JSON +
   surfaced through the faces; nil serialises as absent/unevaluated, never `0`.
3. **New status `oom-killed`** (types.go): the kernel OOM-killed the leader/scope. `Terminal()` includes
   it. Classification (mutually exclusive in practice — a userspace `run-kill` SIGKILL does NOT fire the
   cgroup `oom_kill` counter, only a kernel OOM does): **`memory.events.oom_kill > 0` → `oom-killed`**;
   else an explicit `run-kill` (`KillIntent.Present`) → `killed`; else `exited`. `oom-killed` is a
   terminal status set on the terminal record, integrated with the existing terminal-complete CAS +
   `#17` scope-integrity classification (must not regress those).
4. **HONEST unevaluated (the load-bearing honesty property):** a bare run with no cgroup (empty
   `run.prefix`, no scope), OR `memory.peak` absent (kernel < 5.19), OR any stat read failure ⇒ the
   corresponding field is **nil (unevaluated)** — NEVER a fabricated `0` or number. This resolves the
   §21 pre-5.19 `memory.peak` fallback question the honest way: **no fallback — absent ⇒ unevaluated.**
   A stat read failure never fails the run and never corrupts the exit record; it only leaves the field
   unevaluated (+ optionally a stable `U_RUN_*` note in `ErrorCodes` distinguishing "no cgroup" from
   "read failed").
5. **Faces:** `aira run`/`run-log`/`aira_run`/`aira_run_output` surface `peak_rss`/`cpu_user`/`cpu_sys`
   (nil ⇒ absent) and the `oom-killed` status. Descriptor-generated help/schema pick up the fields.
6. Real-cgroup tests (load-bearing) + Opus real-cgroup verification.

**OUT (written-down deferrals / later Phase-5 milestones):**
- The §17 **estimate-vs-actual gauge** and **`MemoryMax` tuning** — this milestone supplies the data.
- **Live tee I/O + streaming `run-log --follow`**, **`--realtime`/`--pty`**, **telemetry/gate
  auto-wiring** (`run --report/--ticket/--phase`), **`--detach` + the daemon supervisor**, **`run-input`**
  — subsequent Phase-5 milestones.
- **`getrusage(RUSAGE_CHILDREN)`** — explicitly rejected by §14 (misses grandchildren); cgroup only.

## 2. Design

### 2.1 Read placement + path
- The scope's cgroup path already exists in the runner (used for kill; cgroup_linux.go). Read the three
  files from that path AFTER `cmd.Wait` and the `wait-observed` append, BEFORE scope teardown — the
  cgroup and its cumulative stats are still readable then. If the run has no cgroup (bare/empty prefix)
  the fields stay nil.
- A single `readCgroupUsage(scopePath) (usage, error)` helper parses all three; a per-file parse/read
  failure yields a nil field for that metric (independent), not a whole-usage failure.

### 2.2 Parsing (cgroup-v2)
- `memory.peak`: a single integer (bytes). File absent (pre-5.19) ⇒ `PeakRSS = nil`.
- `cpu.stat`: whitespace `key value` lines; take `user_usec` → `CPUUser`, `system_usec` → `CPUSys`.
  Missing keys ⇒ nil for that metric.
- `memory.events`: `key value` lines; `oom_kill` → the OOM classification (2.1/§1.3).

### 2.3 Honesty invariants
- nil ⇒ unevaluated, never a fabricated number (§1.4). Verified by a bare-run test + a
  memory.peak-absent test.
- The read never fails the run nor mutates exit/signal/scope-integrity — it only adds the usage fields +
  (for a genuine OOM) the `oom-killed` status. A read error ⇒ unevaluated field + a stable note.
- `oom-killed` classification is exact and does not mis-tag a `run-kill` or a normal exit (§1.3).

## 3. §1b — pre-empted resolutions (anticipating Sol)

- **R1 (never a fake number).** Absent/failed stat ⇒ nil, never `0`. `*int64` end-to-end (ledger JSON,
  DB projection if any, faces).
- **R2 (read before teardown).** The stats are read while the cgroup still exists; a run whose scope was
  already reconciled/removed (crash path) ⇒ usage unevaluated (honest), not a crash.
- **R3 (oom classification exactness).** `oom_kill>0 ⇒ oom-killed` (kernel OOM); `run-kill ⇒ killed`;
  else `exited`. Mutually exclusive because a userspace SIGKILL does not increment `oom_kill`. Tested in
  all three directions on real cgroups.
- **R4 (no regression to #17 / terminal-CAS).** The usage read + `oom-killed` status slot into the
  existing terminal-complete CAS + scope-integrity classification without changing their verdicts; the
  `Terminal()` set gains `oom-killed`. A dedicated test asserts the #17 states are unchanged for the
  non-OOM paths.
- **R5 (unit vs real-cgroup split).** Parsing is unit-tested from fixture cgroup files (fast, sandbox-
  safe). The end-to-end read (a real mem-hog OOM'd under a real `MemoryMax`, real CPU burn) is
  real-cgroup only — SKIP-guarded in the sandbox, run under `whale-run` by Opus (load-bearing).

## 4. Tests

**Unit (parsing, fixture-driven, sandbox-safe):**
1. `memory.peak` parse → `PeakRSS`; absent file ⇒ nil.
2. `cpu.stat` parse → `CPUUser`/`CPUSys`; missing key ⇒ nil.
3. `memory.events` `oom_kill>0` ⇒ oom classification; `oom_kill:0`/absent ⇒ not OOM.
4. Classification precedence: (oom_kill>0)→oom-killed; (run-kill, no oom)→killed; (clean)→exited.
5. A read error on one file leaves ONLY that metric nil (independent), the others populated.

**Real-cgroup (SKIP in sandbox; Opus runs under whale-run — load-bearing):**
6. A memory-allocating command under a scope ⇒ `PeakRSS` populated and > 0.
7. A mem-hog under a low scope `MemoryMax` ⇒ kernel OOM ⇒ status `oom-killed`, `memory.events.oom_kill`
   detected (the discriminating OOM test).
8. A CPU-burning command ⇒ `CPUUser`/`CPUSys` populated > 0.
9. **Bare run (empty prefix, no cgroup) ⇒ `PeakRSS`/`CPUUser`/`CPUSys` nil (unevaluated, NOT 0)** — the
   load-bearing honesty test.
10. #17 non-regression: `contained`/`unverified`/`exited`/`killed` classifications unchanged when usage
    reading is added.

## 5. Files

- `internal/runner/types.go`: `PeakRSS`/`CPUUser`/`CPUSys *int64` on `RunRecord`; `StatusOOMKilled`;
  `Terminal()`.
- `internal/runner/cgroup_linux.go` (or a new `usage_linux.go`): `readCgroupUsage` + the three parsers.
- `internal/runner/runner_linux.go`: read usage at child-exit before teardown; OOM classification into
  the terminal record; thread usage into the ledger record.
- `internal/runner/ledger.go`: persist the new fields (JSON; the DB projection carries them if present).
- `internal/core/core.go` + `cmd/aira/…`: faces surface the fields + `oom-killed`.
- tests: fixture-unit + real-cgroup (SKIP-guarded).

## 6. Risks / expected yield

1. **Fake number (highest honesty risk)** — absent/failed stat must be nil, never 0. R1 + tests 1/9.
2. **OOM mis-classification** — a `run-kill` or clean exit wrongly tagged `oom-killed`, or a real OOM
   missed. R3 + test 7 (real OOM) + test 4.
3. **#17 / terminal-CAS regression** — the highest structural risk; the usage read must not perturb the
   scope-integrity/terminal verdicts. R4 + test 10 + the full real-cgroup runner suite green.
4. **Sandbox blindness** — parsing is unit-tested; the real read is real-cgroup only ⇒ Opus real-cgroup
   verification is load-bearing (M12 lesson).

## 7. Deferrals (filed)

- estimate-vs-actual gauge + MemoryMax tuning (data consumers).
- live-tee I/O, `--realtime`/`--pty`, telemetry/gate auto-wiring, `--detach`+daemon, `run-input` (later
  Phase-5 milestones).
