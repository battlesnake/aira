# M16 — cgroup resource accounting at exit (peak_rss · CPU · oom-killed)

Status: PLAN v4 (incorporates Sol plan-review r1–r3). r3: the Kill→lost terminal path snapshots too;
lock held through snapshot→CAS→remove; OOM is an auxiliary positive-only refinement (memory.events
unreadable → base status, never a fake oom-killed); runRecordCode oom-killed precedence specified.
Awaiting Sol re-review → gate → build.
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
4. **New status `oom-killed` (`StatusOOMKilled`)** — `Terminal()` includes it. **The base status is the
   observed wait/kill result (`exited`/`killed`/`lost`); `oom-killed` is an AUXILIARY POSITIVE-ONLY
   refinement** applied only when `memory.events.oom_kill > 0` is POSITIVELY read (Sol r3 P1-honesty).
   A userspace `run-kill`/timeout SIGKILL does not increment the cgroup `oom_kill` counter, so a real
   kernel OOM is distinguishable — **proven by the real-cgroup OOM test (the discriminator)**. If
   `memory.events` is **unreadable**, the base status stands (it honestly reports the exit signal) and
   `oom-killed` is NOT fabricated — the absence of `oom-killed` means "OOM not positively observed",
   never a claim of "definitely not OOM". Precedence: `oom_kill>0` ⇒ `oom-killed`; explicit
   `run-kill`/timeout (`KillIntent.Present`) ⇒ `killed`; else the existing `exited`/`lost`. Tested with a
   `memory.events` read failure (base status retained, no fake `oom-killed`, no crash).
5. **Response-contract integration (Sol P1-1, r3 P1).** Add stable code `E_RUN_OOM_KILLED`;
   `core.runRecordCode` (core.go:378-390) maps `oom-killed → E_RUN_OOM_KILLED`; register it in
   check.go's code table (:36-42) so an OOM run does NOT return `OK`. **Precedence:** `runRecordCode`
   today returns the first `ErrorCodes` entry before inspecting status — place `oom-killed` at the SAME
   precedence position as the existing terminal statuses (`killed`/`lost`), so an `oom-killed` record
   maps to `E_RUN_OOM_KILLED` consistently; a test covers an `oom-killed` record BOTH with and without a
   co-occurring scope/capture `ErrorCode` and asserts the documented precedence.
6. **Usage-read failures never fail the run or the gate (Sol P1-2).** A failed/absent stat leaves the
   field **nil** and emits **NO** `ErrorCodes` entry (any `ErrorCodes` entry makes `CleanSuccess` false,
   types.go:100-102, failing `runRecordCode` + rejecting command gates, gate_command.go:226-235). The nil
   field IS the honest signal; no separate diagnostic in this milestone.
7. **Terminal-CAS carries usage (Sol P0-3).** `mergeEvidence` (runner_linux.go:436-468) merges the
   non-nil usage pointers; every terminal winner — normal, `killed`, `oom-killed`, timeout, and the
   already-terminal CAS return (:503-509) — carries its snapshot.
8. **Faces.** `aira run`/`aira_run` return the full record ⇒ they get the fields AND the top-level
   `E_RUN_OOM_KILLED` code. `run-log`/`aira_run_output` return `OutputChunk` (Core.Do unwraps it and
   returns `OK`, core.go:341-363), whose schema has only `RunStatus`/`ErrorCodes` (types.go:141-152;
   `ReadOutput` :1219-1222) — **add nullable `peak_rss`/`cpu_user`/`cpu_sys` to the chunk** (Sol P1-3) so
   run-log surfaces them, and the chunk's `RunStatus` carries `oom-killed`. **The top-level
   `E_RUN_OOM_KILLED` response-code guarantee is scoped to `run`/`run-kill` (the record-returning verbs);
   `run-log` is a read op whose top-level code reflects the READ (OK), with the run's status+metrics in the
   chunk (Sol P1) — documented, not silently OK.**
9. Real-cgroup tests (load-bearing) + Opus real-cgroup verification.

**OUT (written-down deferrals / later Phase-5 milestones):**
- §17 estimate-vs-actual gauge + `MemoryMax` tuning (data consumers).
- Live tee I/O + streaming `run-log --follow`, `--realtime`/`--pty`, telemetry/gate auto-wiring.
- **`--detach` + the daemon supervisor — BLOCKED on the §14 shim/daemon decision (resolved: the daemon
  is the detached-run supervisor); this foreground `cmd.Wait` runner does not touch it** (Sol P2).
- `run-input`. `getrusage(RUSAGE_CHILDREN)` (rejected by §14 — misses grandchildren).

## 2. Design

### 2.1 Read placement — snapshot into the CANDIDATE, BEFORE the terminal CAS (Sol P0)
"Just before `scope.Remove()`" is too late: the normal path commits the terminal record via
`appendTerminalLocked` at runner_linux.go:~410-423, unlocks, and only then removes the scope (:429-430) —
and the CAS returns an already-terminal record unchanged (:503-509), so a post-commit read cannot land in
the record. Therefore: **read usage into the candidate `record`/`latest` BEFORE the terminal CAS, merge it
in, commit, THEN remove** — all under the per-run lock the path already holds. The cgroup files stay valid
after `cgroup.kill` (processes gone) and before `rmdir` (Sol-confirmed), so the kill path
(`killWithIntent` :1067-1117, holding the lock) snapshots before its removal too, threaded through
`attempt.Current` and the timeout path.

**Every child-ran TERMINAL path (Sol P1, r3 P1).** Snapshot into the candidate before the CAS at each
path where a real child ran: the normal terminal path; `killWithIntent` (run-kill + timeout);
**reconcile** (:1344-1365, kills+terminalizes+removes an orphan — it already holds the scope handle+lock:
opens :1312, kills :1344, CAS :1360, removes :1365, unlocks :1368); and the **`Kill` → `lost`** path
(:1135-1142, when `killWithIntent` cannot prove completion — the scope may still exist): attempt the
snapshot; if the scope is unavailable, nil (honest). **Pre-launch / setup-failure removals**
(:165/:582/:613 — the child never ran) ⇒ usage stays **nil** (a just-created cgroup's setup-peak is NOT
the child's usage). A crash path where the scope was already removed ⇒ nil, not a crash. **Lock
discipline (Sol r3 P1):** on the normal path the per-run lock currently unlocks at :428 BEFORE
`scope.Remove()` (:429-430); the build MUST hold the lock through snapshot → CAS → removal (move the
unlock after removal), matching reconcile's ordering. Every terminal writer carries its snapshot BEFORE
the CAS (the early-terminal return :503-509 cannot merge a later candidate).

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
- **R10 (snapshot before the CAS at every child-ran removal — Sol r2 P0/P1).** Read usage into the
  candidate BEFORE `appendTerminalLocked`, at the normal-exit, `killWithIntent` (run-kill+timeout), and
  reconcile (:1344-1365) paths; every terminal writer carries its snapshot before the CAS (the early
  return :503-509 can't merge later). Pre-launch/setup-failure removals (:165/:582/:613, child never ran)
  ⇒ nil. Test 11 asserts metrics are RETAINED across each terminal race, not merely a terminal count.
- **R11 (run-log code scope — Sol r2 P1).** The `E_RUN_OOM_KILLED` top-level code is scoped to
  `run`/`run-kill`; `run-log`'s top-level code reflects the read (OK) with status+metrics in the chunk.

## 4. Tests

**Unit (fixture-driven cgroup files, sandbox-safe):**
1. `memory.peak` parse → `PeakRSS`; **absent file ⇒ nil** (the unevaluated case — Sol P0-1, not "bare run").
2. `cpu.stat` parse → `CPUUser`/`CPUSys`; missing key ⇒ that metric nil.
3. `memory.events` `oom_kill>0` ⇒ OOM; `0`/absent ⇒ not OOM.
4. Classification precedence: `oom_kill>0`→oom-killed; `run-kill`/timeout→killed; clean→exited.
5. One file's read error leaves ONLY that metric nil (others populated).
6. Response contract: an `oom-killed` record → `runRecordCode` = `E_RUN_OOM_KILLED` (not `OK`), BOTH with
   and without a co-occurring scope/capture `ErrorCode` (asserts the documented precedence, Sol r3 P1).
6b. **`memory.events` read failure (honesty, Sol r3 P1):** a signal-KILL exit whose `memory.events` is
   unreadable → the base wait status is retained, `oom-killed` is NOT fabricated, and it does not crash —
   the absence of `oom-killed` is "not positively observed", never "definitely not OOM".

**Real-cgroup (SKIP in sandbox; Opus under whale-run — load-bearing):**
7. **OOM discriminator (Sol P1-4).** The test itself creates a dedicated WRITABLE parent cgroup under
   `whale.slice`, enables the memory controller on it (`+memory` in the grandparent's
   `cgroup.subtree_control`), sets a LOW `memory.max`, and points the run's `RunConfig.cgroup_parent` at
   it (config only SELECTS the parent — it does not set `memory.max`, Sol P1); the run's child scope
   inherits the hierarchical limit and a mem-hog OOMs ⇒ status `oom-killed`, `memory.events.oom_kill>0`.
   The test must NOT constrain the test process itself (only the run's scope).
8. A CPU-burning command ⇒ **`CPUUser > 0`** (NOT `CPUSys>0` — system time can legitimately be 0, Sol P1-4).
9. A memory-allocating command ⇒ `PeakRSS` populated > 0.
10. A **run-killed** run ⇒ usage still snapshotted (read before `killWithIntent` removes the scope) AND
    status `killed`, NOT `oom-killed` (Sol P0-2 discriminator). Allow nil metrics on an unsupported
    kernel / read failure (assert the STATUS + the no-mis-tag, tolerate nil usage).
11. #17 non-regression + metrics-across-races: `contained`/`unverified`/`exited`/`killed` classifications
    + terminal-CAS unchanged when usage reading is added, AND metrics are RETAINED across each terminal
    race (normal, run-kill, timeout, reconcile, and the `Kill`→`lost` path) — asserting the actual
    metric values survive, not merely a terminal count (Sol r3 P1).

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
- **Memory-controller enablement for `peak_rss` (surfaced by real-cgroup verification).** `memory.peak`
  exists on a run cgroup ONLY if its parent delegates the `+memory` controller in `cgroup.subtree_control`.
  The default ambient parent (e.g. a `whale-run`/`agentmux` scope that holds the caller process) often
  does NOT — cgroup-v2's no-internal-process rule forbids enabling `+memory` on a cgroup that holds
  processes — so in the common case `peak_rss` is **honestly nil** (CPU accounting via `cpu.stat` is a
  core stat and works regardless; OOM classification works wherever `+memory` is present). Making
  `peak_rss` populate by default would require AIRA to interpose an OWNED intermediate cgroup (no direct
  processes) with `+memory` enabled between the ambient parent and each run scope — a runner-setup /
  cgroup-controller-management change, out of this "read the stats" milestone. Filed as a follow-up. The
  real-cgroup tests enable `+memory` on a purpose-built parent (under the scope's parent, since the
  scope itself holds the test process) to exercise the reading.
