# M16 — cgroup resource accounting at exit (peak_rss · CPU · oom-killed)

Status: PLAN v2 (incorporates Sol plan-review r1: 3×P0 + 4×P1). Awaiting Sol re-review → gate → build.
Branch `codex-aira-m16` off master `0f4ca96`. Phase 5 (full subprocess runner), first cut. Spec §14
("Resource accounting from the cgroup, at exit") + §101 (`cpu_user/cpu_sys/peak_rss?/status(...|oom-killed)`).
Class: **runner + cgroup correctness → the two-loop is mandatory; real-cgroup verification is load-bearing**
(the Codex sandbox has a read-only cgroup mount → real-cgroup tests SKIP there — the M12 lesson).

## 0. Context — the gap

M12 runner-lite launches, captures to files, and scope-kills via cgroup-v2, but **reads no cgroup
resource stats** and has **no `oom-killed` status** (statuses today `starting|running|exited|killed|lost`,
types.go:11-19; `RunRecord` has no `peak_rss`/`cpu_*`). **`Launch` ALWAYS creates an AIRA cgroup**
(runner_linux.go:121-163; an empty `run.prefix` only affects argv, :79/:169) — so `CgroupScope` is always
populated and there is **no "bare run with no cgroup"** case (Sol P0-1). The unevaluated case is a stat
that cannot be read: `memory.peak` **absent** (kernel < 5.19) or a read failure. This milestone reads
peak-RSS/CPU/OOM from the scope cgroup at child-exit and records them honestly; it supplies the DATA for
the §17 estimate-vs-actual gauge + `MemoryMax` tuning (both OUT of scope).

## 1. Scope

**IN:**
1. **`readCgroupUsage(scope)`** — reads `memory.peak` (peak RSS bytes), `cpu.stat` (`user_usec`,
   `system_usec`), `memory.events` (`oom_kill` count) from the run's scope cgroup. Cumulative stats
   INCLUDE descendants (the scope total — the honest resource figure). A per-file absent/parse/read
   failure yields a nil field for THAT metric only (independent), never a whole-usage failure and never a
   fabricated value.
2. **Snapshot BEFORE every scope removal (Sol P0-2).** The scope is removed at three points: normal
   terminalization (runner_linux.go:~423-430, after the terminal CAS) AND `killWithIntent`
   (:1105-1117, immediately after the cgroup kill — invoked by `run-kill` and by timeout, :248-264). The
   usage read must happen **before each** removal, under the per-run lock that already guards that
   removal, so killed/timed-out runs also carry usage. A single helper snapshots into the record wherever
   a removal is about to occur.
3. **New `RunRecord` fields** `PeakRSS`, `CPUUser`, `CPUSys` (all `*int64`, µs for CPU; nil =
   **unevaluated**, mirroring M14's nullable-bucket discipline). Persisted in the ledger JSON + DB
   projection; nil serialises absent, never `0`.
4. **New status `oom-killed` (`StatusOOMKilled`)** — `Terminal()` includes it. Classification:
   `memory.events.oom_kill > 0` (a KERNEL OOM) ⇒ `oom-killed`; an explicit `run-kill`/timeout
   (`KillIntent.Present`) ⇒ `killed`; else `exited`. A userspace SIGKILL does not increment the cgroup
   `oom_kill` counter, so the two are mutually exclusive — **this kernel behaviour is proven by the
   real-cgroup OOM test (Sol P1-4), the discriminator**.
5. **Response-contract integration (Sol P1-1).** Add stable code `E_RUN_OOM_KILLED`; `core.runRecordCode`
   (core.go:378-390) maps `oom-killed → E_RUN_OOM_KILLED`; register it in check.go's code table
   (:36-42) so an OOM run does NOT return `OK`.
6. **Usage-read failures never fail the run or the gate (Sol P1-2).** A failed/absent stat leaves the
   field **nil** and emits **NO** `ErrorCodes` entry (any `ErrorCodes` entry makes `CleanSuccess` false,
   types.go:100-102, failing `runRecordCode` + rejecting command gates, gate_command.go:226-235). The nil
   field IS the honest signal; no separate diagnostic in this milestone.
7. **Terminal-CAS carries usage (Sol P0-3).** `mergeEvidence` (runner_linux.go:436-468) merges the
   non-nil usage pointers; every terminal winner — normal, `killed`, `oom-killed`, timeout, and the
   already-terminal CAS return (:503-509) — carries its snapshot.
8. **Faces.** `aira run`/`aira_run` return the full record (get the fields). `run-log`/`aira_run_output`
   return `OutputChunk`, whose schema has only `RunStatus`/`ErrorCodes` (types.go:141-152; `ReadOutput`
   :1219-1222) — **add nullable `peak_rss`/`cpu_user`/`cpu_sys` to the chunk/envelope** (Sol P1-3) so
   run-log surfaces them. Plus the `oom-killed` status.
9. Real-cgroup tests (load-bearing) + Opus real-cgroup verification.

**OUT (written-down deferrals / later Phase-5 milestones):**
- §17 estimate-vs-actual gauge + `MemoryMax` tuning (data consumers).
- Live tee I/O + streaming `run-log --follow`, `--realtime`/`--pty`, telemetry/gate auto-wiring.
- **`--detach` + the daemon supervisor — BLOCKED on the §14 shim/daemon decision (resolved: the daemon
  is the detached-run supervisor); this foreground `cmd.Wait` runner does not touch it** (Sol P2).
- `run-input`. `getrusage(RUSAGE_CHILDREN)` (rejected by §14 — misses grandchildren).

## 2. Design

### 2.1 Read placement
The scope object (`scope`, with `.Members()/.Empty()/.Remove()`; `record.CgroupScope` = `scope.Reference()`,
runner_linux.go:163) is alive until removal. Snapshot usage into the record immediately before each
removal: (a) the normal path just before `scope.Remove()` (:~426); (b) inside/around `killWithIntent`
before it removes the scope (:1105-1117). Both are already under the per-run lock. A crash path where the
scope was already reconciled/removed ⇒ usage nil (honest), not a crash.

### 2.2 Parsing (cgroup-v2)
- `memory.peak`: single integer (bytes); absent (pre-5.19) ⇒ `PeakRSS` nil.
- `cpu.stat`: `key value` lines; `user_usec`→`CPUUser`, `system_usec`→`CPUSys`; missing key ⇒ that metric nil.
- `memory.events`: `key value` lines; `oom_kill` value drives §1.4.

### 2.3 OOM classification placement
In the status block (runner_linux.go:369-405): after the existing classification sets `exited`/`killed`/
`lost`, if `oom_kill > 0` and the terminal status is not an explicit `run-kill`/timeout `killed`, set
`StatusOOMKilled`. Must NOT perturb the #17 scope-integrity verdicts (classifyLaunchScopeIntegrity) nor
the terminal-complete CAS (Sol — highest structural risk).

## 3. §1b — resolutions (r1 incorporated)

- **R1 (never a fake number).** Absent/failed stat ⇒ nil, never `0`; `*int64` end-to-end.
- **R2 (read before EVERY removal — Sol P0-2).** Normal exit AND `killWithIntent` (run-kill + timeout)
  snapshot usage before removing the scope, under the run lock; already-removed ⇒ nil.
- **R3 (oom classification exactness — Sol P1-4).** `oom_kill>0 ⇒ oom-killed`; `run-kill`/timeout ⇒
  `killed`; else `exited`. Proven mutually-exclusive by the real-cgroup OOM test.
- **R4 (no #17 / terminal-CAS regression).** Usage read + `oom-killed` slot in without changing the
  scope-integrity / terminal verdicts; `mergeEvidence` merges usage; every terminal winner carries it.
- **R5 (CAS carries usage — Sol P0-3).** Non-nil usage merged in `mergeEvidence`; the already-terminal
  return + kill/timeout paths carry the snapshot (else metrics vanish).
- **R6 (response contract — Sol P1-1).** `E_RUN_OOM_KILLED` added + `runRecordCode` + check.go.
- **R7 (no gate-breaking notes — Sol P1-2).** No `ErrorCodes` entry for a usage-read miss; nil field only.
- **R8 (run-log surfaces metrics — Sol P1-3).** `OutputChunk` gains nullable metrics.
- **R9 (no fallback for absent memory.peak — Sol-confirmed sound).** Absent ⇒ unevaluated; consistent
  with M14.

## 4. Tests

**Unit (fixture-driven cgroup files, sandbox-safe):**
1. `memory.peak` parse → `PeakRSS`; **absent file ⇒ nil** (the unevaluated case — Sol P0-1, not "bare run").
2. `cpu.stat` parse → `CPUUser`/`CPUSys`; missing key ⇒ that metric nil.
3. `memory.events` `oom_kill>0` ⇒ OOM; `0`/absent ⇒ not OOM.
4. Classification precedence: `oom_kill>0`→oom-killed; `run-kill`/timeout→killed; clean→exited.
5. One file's read error leaves ONLY that metric nil (others populated).
6. Response contract: an `oom-killed` record → `runRecordCode` = `E_RUN_OOM_KILLED` (not `OK`).

**Real-cgroup (SKIP in sandbox; Opus under whale-run — load-bearing):**
7. A mem-hog under a **`cgroup_parent` (RunConfig) whose `memory.max` is low** (the existing config path,
   no new feature) ⇒ kernel OOM ⇒ status `oom-killed`, `memory.events.oom_kill>0` (the discriminator).
8. A CPU-burning command ⇒ **`CPUUser > 0`** (NOT `CPUSys>0` — system time can legitimately be 0, Sol P1-4).
9. A memory-allocating command ⇒ `PeakRSS` populated > 0.
10. A **run-killed** run ⇒ usage still snapshotted (read before `killWithIntent` removes the scope) AND
    status `killed`, NOT `oom-killed` (Sol P0-2 discriminator).
11. #17 non-regression: `contained`/`unverified`/`exited`/`killed` classifications + terminal-CAS
    unchanged when usage reading is added.

## 5. Files

- `internal/runner/types.go`: `PeakRSS`/`CPUUser`/`CPUSys *int64`; `StatusOOMKilled` + `Terminal()`;
  `OutputChunk` nullable metrics.
- `internal/runner/cgroup_linux.go` (or `usage_linux.go`): `readCgroupUsage` + parsers.
- `internal/runner/runner_linux.go`: snapshot before each removal (normal + `killWithIntent`); OOM
  classification; `mergeEvidence` usage merge; `ReadOutput` populates chunk metrics.
- `internal/runner/ledger.go`: persist the fields.
- `internal/core/core.go`: `runRecordCode` `oom-killed`→`E_RUN_OOM_KILLED`; faces surface fields.
- `internal/store/check.go`: register `E_RUN_OOM_KILLED`.
- tests: fixture-unit + real-cgroup (SKIP-guarded).

## 6. Risks / expected yield

1. **Fake number** — absent/failed ⇒ nil, never 0. R1 + tests 1/5.
2. **Usage lost on kill/timeout** (Sol P0-2) — snapshot before every removal + CAS carry. R2/R5 + test 10.
3. **OOM mis-classification / response-contract miss** — R3/R6 + tests 4/6/7.
4. **#17 / terminal-CAS regression (highest structural risk)** — R4 + test 11 + full real-cgroup runner
   suite green.
5. **Sandbox blindness** — parsing unit-tested; the real read is real-cgroup only ⇒ Opus real-cgroup
   verification is load-bearing (M12 lesson).

## 7. Deferrals (filed)

- estimate-vs-actual gauge + MemoryMax tuning; live-tee I/O, `--realtime`/`--pty`, telemetry/gate
  auto-wiring, `--detach`+daemon (blocked on the daemon decision), `run-input`.
