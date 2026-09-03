# Confine supervisor CPU — the scope-membership sampler, not output relay

**Status:** plan v2 (post Sol plan-review: REVISE → all points addressed, see §9)
**Ticket:** TBD (owner to file after merge)
**Branch:** `aira-confine-cpu-investigation` off master `994abee`
**Author:** Fable (investigation + build), Sol plan-review

## 1. Problem

`top` on the shared box showed two `aira confine --delegate-ram ... -- make
merge-gate` supervisors at 65% and 25% CPU while the machine was otherwise quiet.
The supervisor is the top-level `aira confine` Go process; the pytest workers
underneath each showed 2-4%. The first hypothesis (output-relay overhead from
verbose pytest output, fixable with a 64 KiB / 10 ms buffered relay) was rejected
by the owner because relay cost must track the producing side's write rate, and
there was no matching signature on the producing side. This plan records what
was measured instead of assumed.

## 2. Evidence (all `/proc` sampling; `strace`/`perf` unavailable, `ptrace_scope=1`)

Sampler: `~/tmp/aira-confine-cpu/procsample.sh` (utime/stime from
`/proc/PID/stat`, `syscr/syscw/rchar` from `/proc/PID/io`, per-thread ticks).
Workloads: `~/tmp/aira-confine-cpu/experiment.sh` against the installed `aira`.

| case | scope tree | child output | supervisor CPU (user/sys) | reads/s | writes/s |
|---|---|---|---|---|---|
| live `make merge-gate` (PID 3575024, 2h49m) | 25 procs in nested `.aira-supervisor` + 2 tracked | quiet (`-q`) | 25.4% (6.4/19.0) | 13,000 | 86 |
| A: `sleep 9` | 1 | none | 12.8% (3.8/9.0) | 5,083 | 188 |
| B: `while :; do echo line; done` (stdout) | 2 | ~1M lines/s, direct fd | 14.8% (3.5/11.2) | 8,037 | 109 |
| C: same loop to stderr | 1 | ~200k lines/s via relay pipe | 97.2% (32.5/64.8) | 383,000 | 214,000 |
| D: 30 × `sleep 9 &` | 31 | none | 112.5% (26.0/86.5) | 96,000 | 50 |

Reading:

- CPU is 3:1 sys:user and scales with **scope tree size** (A→D: 13% → 112%),
  not with output volume (A≈B). Reads outnumber writes ~150:1 on the live job,
  so the supervisor is *polling*, not relaying.
- Child **stdout is never relayed**: `cmd.Stdout = stdout` is the supervisor's
  own `*os.File` (fd 1 of the child is the log file). Child **stderr is
  relayed** because `diagnostics` is wrapped in `confineLockedWriter` (not an
  `*os.File`), so `os/exec` inserts a pipe + `io.Copy` goroutine. That relay
  costs one read+write per child write (case C) but only matters at hundreds of
  thousands of stderr writes per second; the live job did 86 writes/s.
- Transient-fd sampling of the live supervisor showed `/proc/<pid>/stat`,
  `/proc/<pid>/cgroup`, `/proc/self/mountinfo` and `boot_id` cycling through
  its fd table — the signature of `monitorScopeMembership`.

## 3. Root cause

Two long-lived loops in `internal/runner/runner_linux.go`, both running for the
whole job lifetime in every `aira confine` and `aira run` supervisor:

1. `monitorScopeMembership` (line 1511): `time.NewTicker(2 * time.Millisecond)`.
   Each tick reads `cgroup.procs`, then `/proc/<pid>/stat` per direct member,
   then for every ever-seen member a liveness probe (`processLive` re-reads
   `boot_id` + `stat` every call), and for every alive ever-member no longer a
   direct member `observeProcessCgroup` (two more `stat` reads, `/proc/<pid>/
   cgroup`, and a fresh `unifiedMount()` scan of `/proc/self/mountinfo`, 45 KB
   here). ≈ 26 reads/tick on the live job × 500 Hz = the measured 13,000
   reads/s; ≈ 190 reads/tick on a 31-proc tree.
   The delegate-ram/aitest layout makes it worse: aitest moves the whole tree
   into a nested `.aira-supervisor` sub-cgroup, so the outer `cgroup.procs` is
   empty and the tracked members (`make`, `bash`) take the expensive
   not-a-direct-member path every tick.
2. `scopeMembershipEvents` (line 1744): a non-blocking inotify fd busy-polled
   with `read` → `EAGAIN` → `time.Sleep(1ms)`: ~2,000 syscalls/s and 1,000
   goroutine wakeups/s per job, doing nothing.

Neither has anything to do with output. The 2 ms interval was a deliberate
choice (#20 / #70: "keep-sampler + document the sub-2ms escape as an accepted
coverage gap"); this plan keeps the sampler and its semantics and changes only
its period and the idle cost of the event watcher.

## 4. Change (minimal, architectural-simplicity rule)

1. **Sampler period 2 ms → 50 ms**, as a named constant
   `scopeMembershipSampleInterval`. The witness semantics are unchanged (sample
   `cgroup.procs`, track ever-members by PID identity, witness escapes, final
   sample on stop, event-triggered sample on `cgroup.events` change). What
   changes is the documented coverage gap: a descendant that forks and leaves
   (or exits) within one sampler period is not observed. That gap was "sub-2ms";
   it becomes "sub-50ms". Cost model after the change, per job: ~12 syscalls ×
   20 Hz for a single-process tree, ~300 × 20 Hz for a 31-process tree —
   roughly 0.1% and ~2% of a core instead of 13% and 112%.
   Rationale for 50 ms over 10-20 ms: 5-10 concurrent supervisors are normal on
   this box; at 20 ms a merge-gate-shaped job still costs ~1% each and a
   31-proc tree ~5%. 50 ms keeps the aggregate under a few percent. Reviewer may
   argue for 20 ms; the constant is the only knob.
2. **Event watcher: blocking read on a pollable `*os.File`** instead of the
   1 ms busy loop. The inotify fd is already `IN_NONBLOCK`, so `os.NewFile`
   registers it with the Go netpoller and `Read` parks with zero syscalls until
   an event. `stop` closes the file, which unblocks the pending `Read`
   (`os.ErrClosed`), so the goroutine and fd are released exactly as before.
   Channel semantics (buffer 1, non-blocking send) unchanged.
3. **Docs/comments**: update the accepted-gap wording in
   `classifyLaunchScopeIntegrity`, `monitorScopeMembership`, the
   `TestRealCgroupForkThenMigrateOutIsWitnessedNotContained` comment, and add a
   dated amendment to `2026-08-24-aira-descendant-escape-attestation-design.md`.

Not changed: everything about output. `confineLockedWriter`/stderr relay stays
(see deferrals). No buffering is introduced anywhere; `capture_complete` /
`capture_forced_closed` / `merge_streams` paths in `runner_linux.go` are not
touched.

## 5. Invariants preserved

- A witnessed escape is still reported; the final on-stop sample and the
  teardown attestation (`attestScopeTeardown`) are unchanged.
- `ScopeContained` keeps its leader-only, sampling-based meaning; the gap is
  wider and is written down (review policy: gaps are never silent).
- Leader-migration detection, `HadDescendants`, `Gap` semantics unchanged.
- The event watcher still fires a sample on `cgroup.events` modification and
  still stops cleanly (no goroutine/fd leak in the daemon-hosted `aira run`
  path).

## 6. Tests

TDD, red → green:

- **New** `TestScopeMembershipSamplerIsRateLimited` (runner package, linux):
  asserts the period is at or above a 20 ms contract floor (red at 2 ms), then
  drives `monitorScopeMembership` against a counting fake scope (leader = the
  test process) for 300 ms and asserts `Members()` calls ≤ (300 ms / interval)
  + 2; finally adds a live child to membership right before `stop` and asserts
  the on-stop sample ran (one more `Members()` read) and reported
  `HadDescendants`.
- **New** `TestScopeMembershipEventsDeliversModifyAndReleasesFD`: temp
  `cgroup.events` file, fake scope pointing at it; over three start/stop
  cycles: no event before modification; **≤ 20 read syscalls during a 200 ms
  idle window** (`/proc/self/io` `syscr`; the old busy-poll made ~200 — this is
  the red→green guard); a modification delivers; a 20-write burst coalesces
  without blocking; after `stop` the open-fd count returns to baseline.
- **Re-tuned** real-cgroup dwells so each descendant is still observed at the
  new period (five periods): `sleep 0.03/0.05` → `sleep 0.25` in
  `TestRealCgroupCleanMultiProcessRunIsUnverified`,
  `TestRealCgroupNestedDescendantIsNeverEscaped`,
  `TestRealCgroupForkThenMigrateOutIsWitnessedNotContained`, and the
  `runner_test.go` sibling-migration fixture. These tests are load-bearing for
  the honesty verdicts and must stay green, not be loosened.
- Existing `internal/runner` suite + `make ci` green with exact exit codes.

## 7. Measurement (before/after, same harness)

Re-run `experiment.sh` with the fixed binary; report A/B/C/D and, if a
delegate-ram merge-gate is running, a live sample. Expected: A ≈ 0.1-0.5%,
D ≈ 1-3%, C unchanged (out of scope), B ≈ A.

## 8. Deferrals (written down, not silent)

- **stderr relay cost** (case C): real, one read+write per child write via the
  `confineLockedWriter` pipe, but irrelevant at real stderr rates. A fix
  (hand the child the real fd when `request.Stderr` is an `*os.File`, or a
  small buffered relay) is a separate decision because it changes how the
  supervisor's own status lines interleave with child stderr.
- **Per-tick waste**: `processLive` re-reads `boot_id` on every call (3× per
  tick) and `observeProcessCgroup` re-scans `mountinfo` per observation. Both
  are immutable for the supervisor's lifetime and could be cached; at 20 Hz
  they cost ~0.3% on the live shape, so left alone for simplicity.
- **Observation, not in scope**: across tonight's finished job logs
  `scope-integrity=unverified` outnumbers `contained` 1695:118 — worth an
  owner look at what drives `Gap`/`HadDescendants` in ordinary jobs.

## 9. Plan-review outcome (Sol, GPT-5.6, verdict REVISE) and dispositions

- Root cause confirmed: "13,000 reads/s matches 26 × 500Hz; case D confirms
  per-member scaling; case C isolates the stderr relay as a separate cost".
- 50 ms endorsed over 20 ms (aggregate load from 5-10 supervisors) and over
  100 ms (doubles an already-widened window); final/event samples remain.
- Inotify shutdown must be robust to an early read failure with `stop` never
  closing → **done**: the closer goroutine selects on `stop` *or* reader
  completion and owns the exactly-once `Close`; close-induced read errors are
  normal shutdown.
- The event test as first written passed on the old code and so guarded nothing
  → **done**: an idle-window `syscr` assertion from `/proc/self/io` (old reader:
  ~200 reads per 200 ms idle; new: none), a 20-write burst that must coalesce
  without blocking, and three start/stop cycles. Proven red against the old
  reader in a detached worktree at the plan commit (§10).
- Call this a coverage *reduction* and change every "sub-2ms" claim → **done**
  in `classifyLaunchScopeIntegrity`, `monitorScopeMembership`, the test
  comment, and the 2026-08-24 attestation spec (§0, §1, §5 amendment).
- Dwell re-tune is honest but prefer ≥ 250 ms → **done**: `sleep 0.25` (five
  periods) in the six sampler-dependent tests (five real-cgroup tests in
  `internal/runner`, one gate-command test in `internal/store`); the comments
  state they prove detection of a dwelling escaper, never a shorter one.
- Preserve and test the final on-stop sample → **done**: the rate test adds a
  live child to membership immediately before `stop` and asserts
  `HadDescendants` and one further `Members()` read.
- Adaptive rate, open-fd `pread`, per-tick caching: keep deferred (agreed).
- Not covered by tests, accepted: cgroup deletion/rename under the watcher;
  sampler overrun (a tick longer than the period simply drops ticks — the
  behaviour is unchanged from 2 ms, where overrun was the *normal* state on a
  loaded tree).

## 10. Build evidence

Before = installed `aira` (master `994abee` build); after = this branch built
with `aira confine -- go build -o ~/tmp/aira-confine-cpu/aira-fixed ./cmd/aira`
(exit 0). Same harness, same 4 s sample windows, same loaded shared box.

| case | scope tree | supervisor CPU before → after | reads/s before → after |
|---|---|---|---|
| A: `sleep 9` (quiet) | 1 | 12.8% → **0.8%** | 5,083 → 175 |
| B2: `timeout 9 sh -c 'while :; do echo line; done'` (stdout, ~1M lines/s) | 2 | 14.8% → **1.0%** | 8,037 → 302 |
| C: same loop to stderr (relay pipe, out of scope) | 1 | 97.2% → 58.2% | 383,000 → 215,000 |
| D: 30 × `sleep 9 &` | 31 | 112.5% → **3.5%** | 96,000 → 3,864 |

Case C is the untouched stderr relay and stays relay-dominated, as the plan
predicted; the ~40-point drop there is the sampler's former share of that job.
A live `--delegate-ram make merge-gate` after-sample needs the fixed binary
installed machine-wide, so it is not in this table (before: 25.4%, 13,000
reads/s, PID 3575024).

TDD record: both new tests were run against the plan commit's code in a
detached throwaway worktree (with only the constant introduced at its old 2 ms
value so they compile): `TestScopeMembershipSamplerIsRateLimited` failed on the
floor (`2ms is below the 20ms contract floor`) and
`TestScopeMembershipEventsDeliversModifyAndReleasesFD` failed on idle cost
(`255 read syscalls over 200ms idle`), exit 1. On this branch both pass, exit 0.
The first full `internal/runner` run after the change exposed
`TestRealCgroupConfineWitnessesSiblingEscape` (a fifth sampler-tuned dwell at
`sleep 0.03` that the initial sweep missed — it read `contained` for a 30 ms
escaper, i.e. the widened gap made visible); re-tuned to 0.25 s like its
siblings. The first full `make ci` (exit 2) then exposed a sixth, outside the
runner package: `internal/store` `TestCommandGateAdmitsMultiProcessGreenCommand`
forks a `sleep 0.05` child to prove the gate admits an honest `unverified`
multi-process run, and at 50 ms the child went unobserved (`got "contained"`);
re-tuned to `sleep 0.25`, green 3/3. A repo-wide sweep found no further
sampler-tuned dwells (the remaining short sleeps are Python polling loops in
`pylib`). Final `make ci` on the rebased tree: see the PR. `TestM20LauncherDefersACKAndBoundsReadiness/handle_before_ack`
failed once with `ack=""` (the test reads the ack file between its create and
write) and passed 5/5 in isolation — a pre-existing race in a detach test
this change does not touch; reported, not fixed here.
