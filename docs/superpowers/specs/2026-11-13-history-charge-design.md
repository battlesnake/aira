# Discounted admission charge for pinned jobs, and a fast lane for quick jobs

Tickets: AIRA-323 (completion telemetry), AIRA-324 (flat charge discount for pinned jobs),
AIRA-325 (history refinement, gated), AIRA-326 (fast-lane queue order). The owner un-held
AIRA-323 on 2026-11-14 for release v0.43 (the ticket file still carries the old `hold` and
`v0.41` label until the coordinator's transition lands). 324, 325 and 326 stay held. Status:
plan, not built. Process class: **correctness-critical admission core** (it changes what the
ledger charges and the order the evaluator runs): full two-loop, Opus builds TDD, Fable
reviews in a throwaway detached worktree. AIRA-323 is a **client + daemon + wire + schema**
change **without** a protocol bump (section 5.1.6); either side can be installed first.

Revision 3. Reproduction of the section 2 measurements: `docs/dev/aira-history-charge-replay.py`
(read-only on `~/.local/state/aira/state.db`; run it under `aira confine`). The script is
rev 2's, unchanged by rev 3. Its `keys()` is **research tooling with no frozen contract**:
nothing in AIRA-323 computes a command key (section 5.1.4).

## Rev 3 changes (what changed since rev 2, and why)

Scope of the revision: only AIRA-323 (section 5.1) is redesigned. Sections 5.2-5.4 keep their
rev-2 design; their code citations are refreshed because `admit.go` moved (AIRA-328 merged
after rev 2), the key references now point at the reader tickets, and they gain sequence notes.

**AIRA-323 now stores four raw facts and nothing derived** (plan-review round 3 collapse). Each
finished confine job's row gains `wall_ms`, `wait_ms`, `started_at_ms` and `repo`, all
nullable. These are the facts that cannot be recovered later: once a job has ended, nothing
on the machine still knows how long it waited, how long it ran, when it started or which
repository it ran in. Everything else a reader needs (a command key at any level) is a pure
function of `signature` and `repo`, both stored in the same row, so it can be computed later,
exactly, for every row 323 writes.

What was **cut**, and why:

1. **The three key columns (`key_masked`, `key_first4`, `key_cmd`), the Go `CommandKeys`
   function, the Python/Go parity contract, the committed golden table
   (`docs/dev/aira-history-charge-keys.golden.json`, 39 cases), its mutation check
   (`docs/dev/aira-history-charge-key-mutations.py`, 40 mutations), the replay script's
   `--self-test`, and tests T6 (key golden table) and T7 (no key without established inputs).**
   Nothing in 323 reads a key, and every key is a function of two columns the same row stores.
   Freezing the function now meant guessing the level and the masking rules before any
   repo-keyed row existed, and three review rounds kept finding mutations of `keys()` and
   `unwrap()` that the table did not pin (reported by the reviews as 5, then 8, then 5 more);
   each fix produced new findings. The key function, its level, its index and its backfill move
   to the first reader ticket (section 10, "Hand-off"). Test numbers are reused: T6 is now the
   repo-identity seam at the launch site, T7 and T8 are the old T8 and T9, and T9 (version skew
   and the re-send) replaces the old T10 protocol-bump pin.
2. **The protocol bump.** The new fields are optional on the `confine-report` frame. A new
   client that meets an old daemon re-sends the report once without them, so the peak and OOM
   sample is kept; an old client never sends them. This follows AIRA-340's measured no-bump
   pattern for an optional admit field. With no bump, launches and queued jobs are untouched
   at the cutover, and only the four new facts are lost between client install and daemon
   restart (section 5.1.6). Rev 3's earlier text (proto 15 to 16, stale clients' launches and
   queued jobs refused at the cutover, an empty-queue restart step) is withdrawn.
3. **Client-side pre-validation of the new fields and the three shared `runner` predicates.**
   The handler is the only layer that checks the values (daemon-local rules). A value it
   refuses is handled by the same once-only re-send, so the sample is still kept. The client
   sends what it measured.
4. **The replay script's rev-3 changes.** Its repo-aware `keys(sig, repo)`, legacy placeholder,
   `history_sql()` and `--self-test` existed to serve the frozen key. The script is restored to
   rev 2's text (`git diff 4d935a0c -- docs/dev/aira-history-charge-replay.py` is empty). The
   reader ticket adds repo handling when it tunes its key on real rows.
5. **Backlog-plan correction (rev 3's change 8) dropped.** It said a stale client's launches
   are refused at the cutover; with no bump that is no longer true, and a stale client's
   report is accepted with the four columns NULL. The backlog plan's line about 323 (line 171)
   is not edited by this spec.
6. **The mutation counts recorded in rev 3's change log** (every count was about the key
   table) are removed with it; nothing they measured remains in 323.

What **stays** from rev 3, each re-checked against the code:

- `started_at_ms` is a client wall-clock reading taken at the return of the admission call,
  not `at - wall_ms` (section 5.1.1).
- `wait_ms` is measured by the client across the whole admission call, sent only for a
  daemon grant, and not equated with the grant's `waited_ms` (section 5.1.1).
- The repo identity's definition (section 5.1.3), now the value of the `repo` column only,
  with a test seam in `confineDeps` (T6).
- Validation has one owner, the handler; refuse, never repair; absent is NULL, never 0.
- The evidence limits added to section 2 (repo-split coverage unmeasured in both directions;
  6.2% of retained rows sit under the 20-row cap; failed runs are never recorded).
- Retention unchanged at 20 rows per exact signature, with the cross-repo eviction note.
- Sequence notes for AIRA-340, 341, 228, 225, 321, 331 and 236 (section 10).

Earlier rev-3 rounds (kept where the design still has the item): round 1 restored
`started_at_ms` as a client-captured field, required symlink resolution of the working
directory before the repo walk, and stopped T4 equating the client's wait with the daemon's;
round 2 named every new field in the allowlist (now a T1 mutation) and moved all validation
into the handler. The review of the collapsed text (round 4) found three porous checks, now
pinned: T9 runs its old-daemon cases with `OOM` true and false and asserts the stored `oom`
(a re-send that reset the bit passed every earlier assertion); T3 has a refused case one past
every bound of every field, so deleting only the `wait_ms` upper bound is red; and the rollout
smoke test (section 6) checks a fresh-nonce probe's own new row instead of the latest row in
the store, which another session's report could make a false pass or a false failure.
Round 5 found T4 porous by construction: each of its cases moved one clock in one phase, so a
launch site that started the wall timer, or read the wall clock, before `deps.admit` (folding
the queue wait into `wall_ms` and shifting `started_at_ms` by it) passed every case. T4 is now
one composite scenario in which the repo walk, the admission and the run each move both clocks
by a distinct amount, with all three values asserted together, and the moved-start mutations
listed; section 5.1.3 no longer claims T6 pins the timers.

## 1. The problem, in plain terms

Jobs queue (often 20-30 deep) while the box is nearly idle. A snapshot taken for this plan,
2026-11-13, 64 GB slice on an 80 GB machine:

| Measure | Value |
|---|---|
| Running jobs | 20 |
| Booked against the slice (sum of reserves) | 44.4 GB |
| Actual RSS of those jobs | 13 GB (their peaks so far sum to 17.6 GB) |
| Queued jobs | 26, asking for 48 GB |
| Admission ceiling | 45.7 GB (system-pressure throttle, down from 64 GB) |

Admission books each job's **reserve** until it ends, and the reserve is sized for the job's
worst case. Over the last two weeks a finished job peaked at a median of about a third of what
it reserved. Pinned reserves (`--memory-reserve`, about 60% of jobs) are used as is, for the
memory cap and for the booking (`resolveAdmitReserve`, `admit.go:1403`: a pinned request
returns `request.reserve`); history is never read for them. Auto-sized reserves use history only
for an exact-argv match with 3+ clean samples (about 15% of jobs: argv carries temp paths and
file lists, 34,731 distinct signatures in two weeks).

A second complaint (owner, 2026-11-13): jobs that are historically quick (under 5 minutes)
and small (under 4 GB) should rarely wait long. The queue is FIFO after manual pushes
(`admitOrderLocked`, `admit.go:402`) plus a bounded bypass past a blocked head (AIRA-307).
Nothing knows a job is quick, and the history table has no duration.

## 2. What the data says

Walk-forward replay (`--memory-reserve` pins only, 50,006 jobs finished since 2026-10-20).
"Charge" is what would be booked; the pin stays the cap. **Above** = share of jobs whose actual
peak (the cgroup's `memory.peak`) exceeded the charge. **Gain** = sum of pins over sum of
charges. The risk columns draw 20 jobs and report how often their total peak exceeded their
total charge, for 20 mixed jobs and for 20 jobs of one command and subcommand ("burst"), and
the 99th percentile of total peak over total charge. OOM-ed rows are in the history and veto a
key, as the history rule below requires.

| Charge rule | Above | Gain | Mixed P(over) / p99 | Burst P(over) / p99 |
|---|---|---|---|---|
| pin (today) | 0% | 1.00x | 0 / 0.61 | 0 / n.a. |
| flat 0.9 x pin | 2.9% | 1.11x | 0 / 0.68 | 0 / 0.78 |
| **flat 0.8 x pin** | 6.1% | **1.25x** | 0 / 0.78 | 0.1% / 0.87 |
| flat 0.7 x pin | 12.4% | 1.43x | 0.1% / 0.90 | 0.9% / 1.00 |
| flat 0.6 x pin | 21.3% | 1.67x | 1.8% / 1.03 | 11.8% / 1.16 |
| history: max x 1.25, else pin (72% of jobs covered) | 3.4% | 1.28x | 0.1% / 0.80 | 0.1% / 0.80 |
| min(0.8 x pin, history) | 8.7% | 1.52x | 0.4% / 0.93 | 0.2% / 0.92 |

Reading it:

- **Flat 0.8 x pin gets 1.25x with a burst p99 of 0.87; history gets 1.28x at 0.80.** The gain
  is the same to within noise. History buys a lower "above" rate (3.4% against 6.1%), not
  capacity. The combination reaches 1.5x but its p99 of 0.92-0.93 leaves little margin.
- 20 jobs at today's pins already peak at only 0.61 of their total pins (p99). Auto-sized jobs
  (0.51) are not touched: their cap is their charge, so a job above it is a real OOM.
- The 3x gap seen in section 1 cannot be closed by prediction: a prediction must cover the
  biggest run of a command, so a typical run uses a quarter to a half of it.
- Coverage for history is 72% of pinned jobs once the OOM veto (any OOM in the key's last 20
  rows) and the `aitest-parent` namespace (kept separate, as `AitestParentSignaturePrefix`
  requires) are honoured. Before that correction the table read 90% and 1.34x.

Limits of this evidence, stated so no one reads more into it:

- The table holds job-**end** rows only. Concurrency is simulated by independent draws, not
  duration-weighted; real clustering in time (a burst of builds starting together) is not
  captured.
- Rows are ordered by end time, so a job is predicted from peers that finished while it was
  still running. That is optimistic for history, worst for same-command bursts. It does not
  touch the flat rows.
- The store keeps 20 rows per exact signature; the replay keeps 20 per coarse key. The build's
  window query must be what is measured (AIRA-325).
- "Pinned" here is `cap:operator:--memory-reserve` rows only.
- **(rev 3) Every key above is argv-only.** No stored row has a repo identity, so every figure
  here is rev 2's argv-only grouping. Coverage with repo-scoped keys is **unmeasured, in both
  directions**, until AIRA-323 rows accrue. Splitting thins each key's history, which can lower
  coverage (fewer keys reach 3 clean samples), but the OOM veto means it can also raise it.
  Counterexample (by construction, not measured): repo A's `./runall.sh` OOMs once, then repo
  B's `./runall.sh` runs clean three times. The merged argv-only window holds the OOM and is
  vetoed; repo B's split window has 3 clean samples and is usable. The 72% figure is therefore
  neither an upper nor a lower bound for repo-scoped keys. The first reader ticket measures it
  (section 10).
- **(rev 3) Retention removes rows, mostly for frequent commands.** Re-measured 2026-11-14 on
  the live store, read-only, with the SQL in section 5.1.5 (run under `aira confine`, exit 0):
  232,671 retained confine rows over 158,986 signatures. 723 signatures sit at the 20-row cap
  and hold 14,460 rows, 6.2% of what is retained. How many rows those 723 signatures have
  lost to eviction cannot be counted from the table, because evicted rows are gone. Any
  timeline replay (section 6) therefore under-represents the most frequent commands.
- **(rev 3) Failed runs are not in the history.** Only an OOM or a clean success is reported
  (`shouldRecordConfinePeak`, AIRA-264, called at `confine_linux.go:1446`). A run that fails
  for any other reason leaves no row, so its wall and wait times are missing from section 6's
  timeline. Recording failed runs is out of scope: AIRA-264 excluded them because they would
  feed the estimator.

Section 6 closes the first two limits with AIRA-323's timing before anything is enforced.

## 3. Owner decisions this plan rests on (restated, not reopened)

- Dynamic overcommit of unused reservation is wanted, tending to no overcommit as real use
  nears the limit (2026-11-13).
- Historically quick (under 5 min) and small (under 4 GB) jobs go higher in the queue
  (2026-11-13).
- (rev 3) The repo identity goes into the command key, so two `./runall.sh` scripts in different
  repos stop sharing history (RANT-133), with no reset of existing history; the estimator's
  subject stays argv-only until AIRA-321 (2026-11-14, backlog plan B#2a). AIRA-323 honours it
  by recording the repo identity on every new row as a raw fact; the key is built from it by the
  first reader (section 10). Rows written before 323 have no repo and are never keyed argv-only
  under the repo-split key.
- Prior decisions kept: the admission ledger is a counter derived from the waiter set (the
  2026-09-10 rebuild); AIRA-103 keeps every throttle out of anything the kernel enforces; the
  AIRA-307/309 queue rules (pushed first, bounded bypass) stay; AIRA-264 records only OOM or
  clean-success runs.

## 4. The from-scratch minimum, and the gap

Only the problem "queued jobs wait while real memory is idle, and quick jobs wait too long".
Greenfield minimum: (1) record how long and how big each job really was; (2) book a pinned job
at a fixed fraction of its pin, never letting the booking exceed what real free memory could
back; (3) run quick jobs first.

Revision 1 was the plan with a per-command history predictor and a pressure valve. Gap
against the minimum, and where it went:

- **History predictor**: same capacity as the flat fraction (section 2), more machinery (key
  chain, window query, index, OOM veto, retention). Kept as a gated refinement (AIRA-325), only
  if the enforced flat discount shows the margin to go lower. The fast lane (AIRA-326) and the
  cold-start cap (AIRA-331) need a per-command class too. AIRA-323 records the raw facts that
  class is built from (the signature, already stored, and the new `repo`) from the first day;
  the first reader builds the key function (section 10).
- **Pressure valve `f(p)`**: dropped. `checkedAvailable` (`admit.go:2726`) already books the
  larger of real use and the ledger, so once real use rises the physical term dominates;
  revision 1 wrongly said live reads had been removed. The one thing the ledger cannot see is
  whether a job's full cap could be backed by real free memory, so the flat discount adds one
  rule for that (section 5.2), and nothing is tuned.
- **Monthly predictor re-selection**: dropped.
- **Per-job live usage in the ledger**: still not built; revisit only if the enforce data says
  the flat discount is not enough.
- **Reserved headroom for the fast lane**: not built; measure fast-class waits first.

AIRA-323's own minimum is "for each finished job, record the facts that cannot be recovered
later: when it started, how long it waited, how long it ran, and which repository it ran in".
Rev 3 is exactly that: the client sends four raw facts (a start time, two durations and a repo
identity) on the report it already sends, and the daemon stores them in four nullable columns.
No key, no index, no derived value and no protocol bump (rev 3 changes, cuts 1-4).

## 5. Design

### 5.1 Telemetry (AIRA-323)

Pure data collection: nothing in admission reads the new columns. The estimator
(`ResourcePeakHistory`, `ConfinePeakP90`), retention, `confine --budget` and `confine --dump`
behave exactly as today. Line numbers are master at `4d935a0c`.

#### 5.1.1 What is measured, and by whom

| Value | Computed by | How | When it is absent (NULL) |
|---|---|---|---|
| `wall_ms` | client | Monotonic milliseconds from the return of the admission call (`deps.admit`, `confine_linux.go:744`) to the construction of the `ConfinePeakReport` (`confine_linux.go:1456`). It covers the run, scope setup and the teardown reads before the report. | Never absent on a confine report from a new client. A sub-millisecond run reports 0, which is a real 0. |
| `wait_ms` | client | Monotonic milliseconds across the whole `deps.admit` call. | Sent **only** when the admission ended in a daemon grant. `deps.admit` is `admitConfine` (`confine_linux.go:1900`), which returns the package's `admissionResult` (`admission_linux.go:27-35`). The launch site is in the same package and tests `admission.release != nil`. `release` is set in exactly one place, the lease return at `admission_linux.go:689`, for every daemon grant: `immediate`, `waited`, and the daemon's own `unevaluated` grant. The client-local results carry no `release` and a `waitedMS` that is a Go zero value, not a measurement, so no wait is sent for them. Those results are `disabled` (`admission_linux.go:208`) and `unevaluated` with reason `no-daemon` (`:240`). They share the state name `unevaluated` with a daemon grant, and `release` is what tells them apart. `bypassed` (`:197`) needs `NoAdmit`, which `admitConfine` never sets. |
| `started_at_ms` | client | The **wall** clock, as Unix milliseconds (UTC), read at the same instant the `wall_ms` timer starts: the return of `deps.admit`. | Never absent on a confine report from a new client. |
| `repo` | client | Section 5.1.3, from the client's working directory at launch, computed once, before the admission call. | Absent when the identity cannot be established (section 5.1.3). |
| `at` (existing) | daemon | `time.Now()` when the report is handled (`confine_report.go:101`). | Never. |

Every column is also NULL on rows written before 323, on rows from an old client, on rows
from the `aira confine-report` pool face, and on a row stored by the re-send of section 5.1.6.

**The clock seam.** The launch site reads two clocks, and the build adds both to `confineDeps`
(`confine_linux.go:207`; it has no clock today): `wallNow func() time.Time` (production
`time.Now().Round(0)`, which drops the monotonic reading so only the wall clock is used) and
`monoNow func() time.Duration` (production `time.Since(processEpoch)`, where `processEpoch` is a
package variable set once from `time.Now()`, so the difference is monotonic). `wait_ms` and
`wall_ms` are differences of `monoNow`; `started_at_ms` is `wallNow().UnixMilli()`. Tests replace
the two independently, which is how T4 steps the wall clock without moving the monotonic one.

**Why `started_at_ms` is sent, not derived as `at - wall_ms`.** `at` is stamped when the report
is handled. Between the end of `wall_ms` (the report's construction) and that stamp lie the dial
(`ReportPeakSample`, `admission_linux.go:809`), the frame write and the daemon's scheduling, with
a 250 ms report deadline (`confine_linux.go:1447`) but no measured typical delay. And `wall_ms` is
monotonic while `at` is wall clock, so a clock step during the run (NTP, a WSL resume) moves `at`
and not `wall_ms`, shifting the derived start by the size of the step. A reader that has all
three can check them: `at - (started_at_ms + wall_ms)` is the delivery delay plus any clock step
during the run. The daemon does not judge it, because a step makes a disagreement a true record
of the clocks.

**Why `wait_ms` is client-measured and not the grant's `waited_ms`.** The daemon measures
`waited_ms` from **its** waiter's enqueue (`admit.go`, `waiter.waitedMS =
elapsedMilliseconds(waiter.enqueued, ...)`). A client that reconnects mid-queue
(`errAdmitReconnect`, `admission_linux.go:45`) is enqueued afresh, so the figure restarts. A
second admission timer (`admissionStarted`, `admission_linux.go:295`) is also local to one
exchange. Measuring around `deps.admit` gives the wait the user felt. The trailer keeps printing
the grant's figure, unchanged. The two measure different intervals and are **not** expected to be
equal: the client's interval contains the dial (up to the 500 ms `leaseKeeperDialTimeout`), the
frame exchange and, after a reconnect, the earlier queue time and the 500 ms reconnect gaps
(`leaseKeeperReconnectGap`, `lease_keeper_linux.go:24`; loop at `admission_linux.go:262-282`). An
immediate grant has a daemon `waited_ms` of 0 and a client `wait_ms` of the transport time. No
test equates them; T4 states its expectations on the client's own clock.

`at` caveat for readers: `at` is RFC3339Nano text with trailing zeros trimmed, so sorting the
text is not chronological below one second (`...03.98Z` sorts after `...03.984Z`). This is
pre-existing (retention's `ORDER BY at DESC` has it too). `started_at_ms` is an integer and
sorts correctly.

#### 5.1.2 The wire: `confine-report` frame

Four new **optional** fields in `Request.Args` of the `confine-report` frame, sent by
`ReportPeakSample` (`admission_linux.go:803`). The handler's rules:

| Field | JSON type | Rule |
|---|---|---|
| `wall_ms` | integer | `0 <= wall_ms <= 31,622,400,000` (366 days in ms) |
| `wait_ms` | integer | same bounds |
| `started_at_ms` | integer | `946,684,800,000 <= started_at_ms < 4,102,444,800,000` (2000-01-01T00:00:00Z inclusive to 2100-01-01T00:00:00Z exclusive). **Not** compared with `at` or with the daemon's clock (section 5.1.1: a clock step makes a disagreement true). |
| `repo` | string | exactly `none`, or exactly 16 lowercase hex digits (`^[0-9a-f]{16}$`) |

Client (`internal/runner`):

- `ConfinePeakReport` (`confine_budget.go:14`) gains `WallMS *int64`, `WaitMS *int64`,
  `StartedAtMS *int64` and `Repo string`. Nil or empty means not established.
- `ReportPeakSample` adds each field only when it is set. The `<= 0 -> omit` idiom used for
  `peak_rss` and `budget` (`admission_linux.go:823-831`) must **not** be copied, because 0 is a
  real duration. Nil is the only "absent".
- The client does not pre-check the values against the handler's rules. It sends what it
  measured. A value the handler refuses (a clock before 2000, a client bug) is caught by the
  once-only re-send of section 5.1.6, which keeps the peak and OOM sample and loses only the
  four facts of that run.
- The confine launch site fills the four fields per section 5.1.1. Shim mode still sends no
  report (`confine_shim_linux.go:515`, unchanged).
- `aira confine-report` (the aitest pool face, `cmd/aira/main.go:1405-1438`) is unchanged and
  sends none of the new fields.

Daemon (`internal/daemon/confine_report.go`):

- `confineReportFields` (lines 17-20) gains the four fields (six today, ten after). The count
  guard at line 23 keeps `len(args) <= len(confineReportFields)`, read from the map, so T9's
  old-daemon simulation reproduces it. The refusal text at line 24 names the four new optional
  fields beside the existing ones.
- Integers are parsed with `exactAdmitInt64` (`admit.go:3431`), as `peak_rss` and `budget` are.
  Its accepted forms are reused unchanged: an integer, an integral float64, a `json.Number`, or
  a decimal string.
- **The handler is the one layer that validates the new fields** (type, bounds, the `repo`
  pattern), with small daemon-local checks beside the existing ones. Every existing rule on this
  frame is enforced in the handler (`confine_report.go:31-93`), and only the handler answers
  `E_DAEMON_PROTOCOL`; an error from the writer comes back as `E_DAEMON_INTERNAL` ("record
  confine peak: ...", `confine_report.go:99-104`), the wrong code for a client's bad value.
- **Refuse, never repair.** A present field that fails its rule makes the handler refuse the
  whole report with `E_DAEMON_PROTOCOL`, and nothing is written, as for every existing field.
  The client's re-send (5.1.6) then stores the sample without the four facts. The handler never
  stores a partly accepted frame.
- **No kind gate.** The handler and the writer treat the four fields the same for every subject
  kind. Only the confine launch site sends them; a `pytest-worker` report that carried them
  would have them stored as given (T2 pins this, so a hidden kind rule cannot creep in).
- An absent field is stored as NULL, never 0.

The store (`RecordConfinePeak`, `confine_peak_history.go:99`):

- `ResourcePeakObservation` (`confine_peak_history.go:86`) gains `WallMS`, `WaitMS`,
  `StartedAtMS *int64` and `Repo string`.
- The writer stores the four values as given (nil or `""` -> NULL) and does not re-check them.
  Its doc comment states the precondition: the values already passed the handler's rules. Its
  only production caller is the handler (`confine_report.go:99`; no other non-test call of
  `RecordConfinePeak`). The writer's existing checks and repairs (a non-positive peak or budget
  becomes NULL, `confine_peak_history.go:107-117`) are pre-existing and are not a precedent for
  the new fields.

#### 5.1.3 Repo identity

The value of the `repo` column. Nothing in 323 reads it.

**Definition.** The first 16 lowercase hex digits of SHA-256 of the canonical (symlink-resolved,
absolute) path of the git **common** directory that contains the client's working directory.
This is the same derivation as the project ID (`app.gitIdentity`, `internal/app/project.go`:
`hashID(EvalSymlinks(common))`), truncated, so the stored value is a prefix of the ProjectID that
`aira init` would give that checkout. All linked worktrees of one repository share one common
directory, and so one identity. That matches the AIRA-321 decision ("not `--show-toplevel`,
which would re-fragment to one key per worktree"). A submodule has its own git directory and so
its own identity. Outside any git repository the value is the literal `none`, an established
fact.

**Privacy.** The stored value is a truncated hash, never a path. 64 bits is ample for one
machine's repositories. It is not secret: anyone who can read the state DB can confirm a guessed
path, but that DB already stores full argv, absolute paths included, in `signature`.

**Derivation, client-side, pure Go, no git subprocess.** A new function
`runner.RepoIdentity(cwd string) (id string, ok bool)` in `internal/runner`. It cannot live in
`internal/app`, which imports `runner`. `ok` is false exactly when the identity is
**unevaluated**; then `id` is `""`.

0. `cwd = filepath.EvalSymlinks(cwd)`. An error is unevaluated. This step is required: Go's
   `os.Getwd` returns `$PWD` when `$PWD` names the current directory, so it can return a logical
   path through a symlink (documented: "Getwd may return any one of them"). Confirmed 2026-11-14
   with a throwaway `~/tmp` probe: with `link -> repo/sub` and the shell in `link`, `os.Getwd`
   returned `.../link`, whose parent has no `.git`, so an unresolved walk misses the repository.
   T5's symlink case is the committed reproduction.
1. Walk from the resolved `cwd` up to `/`. At each directory, `os.Stat(dir + "/.git")`:
   - not found (`ENOENT`/`ENOTDIR`): go to the parent;
   - any other error: unevaluated;
   - a directory: `gitdir` is that path;
   - a regular file: read at most 4 KiB. It must be one line `gitdir: <path>` (a relative path
     resolves against `dir`). Anything else is unevaluated.
2. The common dir is `gitdir`, unless the file `gitdir/commondir` exists. Then it is that file's
   trimmed content, resolved against `gitdir` when relative. A read error other than not-found
   is unevaluated.
3. `filepath.EvalSymlinks(common)`. An error is unevaluated. This differs from
   `app.gitIdentity`, which falls back to the unresolved path; here a fallback would silently
   give one repository two identities.
4. Reaching `/` with no `.git` gives `("none", true)`.

`GIT_DIR`, `GIT_WORK_TREE`, `GIT_CEILING_DIRECTORIES` and bare repositories are ignored on
purpose: the identity is a property of the directory. Cost: one `stat` per ancestor plus at most
two small file reads, against the 2-5 ms and retry loop of a `git rev-parse` subprocess
(`gitValue`, `project.go:790`). Unevaluated sends no `repo` (NULL), never `none`, and never fails
or delays the launch.

**The launch-site seam.** `confineDeps` gains `repoIdentity func() (string, bool)`. Production:
`os.Getwd()`, then `RepoIdentity(cwd)`; a `Getwd` error is `("", false)`. The launch site calls
it **once, before `deps.admit`**, so neither timed interval includes it, and copies the result
into the report only when `ok` is true. T6 pins the single call, its place before admission
and the copy rule; T4 pins that neither timed interval includes it (its fake `repoIdentity`
moves both clocks, and a timer started before it is red).

Detached launches: the confine supervisor is started by `spawnDetachedShim`
(`confine_detach_linux.go:49-63`) with `cmd.Dir` and `cmd.Env` unset, so it inherits the
launcher's working directory and `$PWD` (Go's `os/exec` default); its own `os.Getwd`
(`confine_detach_linux.go:591`) therefore sees the launcher's directory. Verified by code reading,
not run.

AIRA-321 must reuse `runner.RepoIdentity`, so the stored `repo` and the future estimator subject
agree.

#### 5.1.4 No command key in 323

AIRA-323 computes and stores no key. A reader's key is a function of `signature` (the
NUL-joined effective argv the report carries, prefixed for a `--delegate-ram` parent,
`confine_linux.go:622-637`; not the 512-character display-truncated admit signature) and `repo`, both in the same row, so any key level can be
computed later, exactly, for every row 323 writes: as a stored column filled by one `UPDATE`,
or at read time. Rows without a `repo` (pre-323, old client, re-send, unevaluated) have no
repo-split key; the reader must not key them argv-only under the repo-split key. The
hand-off is in section 10.

#### 5.1.5 Schema and retention

`confine_peak_history` gains four nullable columns:

```
wall_ms INTEGER, wait_ms INTEGER, started_at_ms INTEGER, repo TEXT
```

- In the `CREATE TABLE` DDL (`store.go:875`) as plain nullable columns, **with no CHECK**: the
  rules have one owner, the handler. Following AIRA-180, a CHECK would sit in the DDL shape only
  (its `budget` ALTER carries none) and be unreachable behind the handler.
- Added to the `ensureConfinePeakHistorySubject` column list (`store.go:1313-1321`, the loop
  calling `ensureColumnAdded` at `:1318`) as plain `ADD COLUMN <name> <type>`, the same text
  as the DDL column, so both shapes are identical (T8).
- **No data migration.** Existing rows read NULL in every new column, which means unevaluated,
  never 0. `scanResourceBudgetRow` is unchanged.
- **No index.** A reader adds its own, next to its query and that query's latency test. A
  full-table `GROUP BY` already misses its 250 ms deadline on this box (AIRA-314), so a reader's
  query must be one indexed read.

**Retention is unchanged: 20 rows per exact `(kind, signature)`** (`confinePeakHistoryLimit`,
`confine_peak_history.go:13`, delete at lines 157-161). It is not per repo.

- **Cross-repo eviction.** Two repos running the same argv (`make test`, `./runall.sh`) share one
  signature, and so one 20-row window. Repo A's runs can evict repo B's rows for that argv. Every
  surviving row is still correctly attributed, so B's history is thinner but never contaminated.
  Accepted for 323: retention per `(kind, signature, repo)` would widen the window the estimator
  aggregates (`ResourcePeakHistory` reads every row of the signature), an admission behaviour
  change. AIRA-321, by putting the repo into the signature, makes retention per argv-and-repo
  as a side effect.

Exposure measurement, reproducible read-only (section 2's 6.2%):

```
select count(*) from confine_peak_history where kind='confine';
select count(*), sum(n) from (select signature, count(*) n from confine_peak_history
  where kind='confine' group by signature having n>=20);
```

#### 5.1.6 Version skew without a protocol bump

`daemon.ProtocolVersion` (`protocol.go:180`) and `runner.DaemonProtocolVersion`
(`admission_linux.go:74`) **stay at 15**. The build adds a paragraph to the history comment in
`protocol.go` recording that 323 added four optional `confine-report` fields without a bump, as
AIRA-340's branch does for its admit field.

**The precedent, verified on branch `aira-priority-launch` (head `4effa392`, not merged).**
AIRA-340 adds an optional `priority` field to the confine-admit request with no bump: both
constants stay 15 (`internal/daemon/protocol.go:193`, `internal/runner/admission_linux.go:89`
on that branch). Its history comment (`protocol.go:180-191`) records a measurement against the
live proto-15 daemon: the old daemon refuses the unknown field with `E_DAEMON_PROTOCOL:
unexpected admit field "priority"`. The client sends the field only when it is set
(`admission_linux.go:470-478`), recognises exactly that refusal (`:673-675`, matched on the
code and the text) and re-asks once without the field with a printed notice
(`admitThroughDaemon`, `:283-293`).

**What an old daemon does with a new report** (master, code reading):

- The version check (`server.go:850-853`) passes: both sides are 15.
- `confine-report` is answered on its own connection, one frame, then the connection ends
  (`server.go:888-894`).
- The handler refuses before any write, with code `E_DAEMON_PROTOCOL`, by one of **two**
  paths. If the frame has more than six fields (`confine_report.go:23-25`), the count guard
  answers "confine-report requires signature and oom, plus optional ...". A confine report with
  a peak and a budget pair already has five fields, so the new client's usual frame (nine
  fields) takes this path. Otherwise the allowlist loop (`:26-30`) answers "unexpected
  confine-report field" naming whichever unknown field Go's map order reaches first. Every
  `E_DAEMON_PROTOCOL` answer of this verb precedes the write at `:99`, and the frame-level
  refusals (`server.go:840-842`, `:850-853`) precede dispatch.
- The client today turns any refusal into an error (`admission_linux.go:840-845`), so the
  launch site's `peakAcknowledged` is false and the sample is lost. That is why the re-send is
  needed.

**The re-send rule** (in `ReportPeakSample`, so it serves every caller):

- When the daemon answers a **well-formed refusal with code `E_DAEMON_PROTOCOL`** and the frame
  sent carried **at least one** of the four new fields, the client dials again and sends the
  same frame without the four fields, **once**, under the same context (250 ms at the confine
  site). The second answer is the function's result.
- No re-send on a transport error, a missing reply or any other code. The daemon commits before
  it replies (`confine_report.go:99-105`), so a re-send after a lost reply could write the row
  twice. A protocol refusal is always answered before the write (above), so the re-send never
  duplicates a row.
- Matched on the code, not the text, unlike AIRA-340. The old daemon has two texts (above), and
  a refused report is not a launch decision: a re-send that is refused again loses exactly what
  the first refusal lost. On a new daemon the same rule catches a value the handler refuses (a
  clock before 2000, or a client bug); the row is then stored with NULL facts, which T1 and T4
  catch, because they assert the stored and sent values.
- No notice is printed: the report is sent at teardown, nothing in 323 reads the facts, and the
  trailer is unchanged. The skew is visible in the store as NULL facts on new-client rows.

**Old/new matrix.** Launches, queued jobs, running jobs and their re-declare are not affected
in any cell: the admit frame does not change in 323. A daemon restart keeps its usual
consequences (an exclusive job loses its exclusivity, a running job's lease re-declares), as on
every restart; nothing in 323 adds to them.

| Client \ daemon | Old daemon (six-field allowlist) | New daemon (ten-field allowlist) |
|---|---|---|
| Old client | today | accepted; the four columns NULL |
| New client | first frame refused (`E_DAEMON_PROTOCOL`, one of the two paths); re-sent once without the four fields; the row is stored as today; **the four facts of that run are lost** | all stored |

**What is lost in the install-to-restart window**, whichever side is installed first: only the
four new facts of the runs whose report lands in the window. Peak, OOM and budget are kept.
Two residual losses, both unmeasured: if the first exchange plus the re-send exceed the 250 ms
report deadline, the row is lost (two local Unix-socket exchanges; the typical time is not
measured); and runs that ended before the client was installed are as today.

**Unverified:** the live daemon was not probed with a `confine-report` frame carrying a new
field; the behaviour above is master's code, and AIRA-340's live measurement is of the analogous
admit allowlist, not this one. T9 is the executable check of the two refusal paths.

#### 5.1.7 What is client, daemon and store

| Part | Files | Ships as |
|---|---|---|
| client | `runner/confine_linux.go` (`confineDeps`: `wallNow`, `monoNow`, `repoIdentity`; timers, start time, report fields), `runner/confine_budget.go` (`ConfinePeakReport`), `runner/admission_linux.go` (`ReportPeakSample`: optional fields and the once-only re-send), new `runner/repo_identity.go` | client install |
| daemon | `daemon/confine_report.go` (allowlist and the only validation of the new fields), `daemon/protocol.go` (history comment only; no bump) | daemon restart |
| store | `store/store.go` (DDL, ALTER list), `store/confine_peak_history.go` (observation, writer) | daemon restart; columns added at open |

One release. The client install and the daemon restart can happen in either order (the matrix
above); until both have, the four facts are lost and nothing else is.

### 5.2 Flat charge discount for pinned jobs (AIRA-324)

(Rev 2 design, unchanged. Citations are now by function, because `admit.go` line numbers moved
after AIRA-328.)

**Who is discounted.** Only a waiter that is scope-backed AND whose pin the operator gave with
`--memory-reserve` (cap-source `operator:--memory-reserve`). Not `--memory-max`, not `aira run`
admissions (their pin has no flag to attribute), not scope-less `confine-reserve` waiters
(they have no cap), not a `--delegate-ram` parent, not an exclusive job, not aitest workers.
The client sends one new boolean on the admit request, `discountable`, set only in that case;
the daemon never infers it from `pinned`. In ci-shim mode (no physical reading) nothing is
discounted. (`discountable` is an admit-frame field; 324's plan decides between a protocol bump
and AIRA-340's optional-field pattern.)

**The rule.** `charge = clamp(ceil(factor x pin), 1 MiB, pin)`; `factor` defaults to 0.8
(install knob; 1.0 = off). The grant still echoes the pin, the client still sets `memory.max`
= pin. **No change to the cap.**

**Physical fit (replaces the valve).** A discounted job is granted only if its **full pin** fits
real free memory: `pin <= ceiling - headroom - effectiveCurrent`, where `effectiveCurrent` is
the slice's `memory.current` less reclaimable cache (the reading `checkedAvailable` already
takes). So as real use climbs, large pins stop being admitted before the discount can
overcommit them, and an idle box admits as freely as the ledger allows. No tuned constants.

**Retired invariant, named.** Today `sum(reserve) <= ceiling` (the fit check in
`evaluateAdmitQueue`, `admit.go:2331`) also bounds `sum(memory.max)`. After this change
`sum(charge) <= ceiling` bounds only the booking. What replaces it is a **grant-time bound, not
a standing invariant**: at the instant a discounted job is granted, its cap is at most
`charge / factor`, so the caps of the discounted jobs are at most `sum(charge) / factor`
(1.25 x at 0.8), and non-discounted jobs have cap = charge. It is valid only while the factor is
fixed and no exception applies. Exceptions, each of which breaks the bound for the jobs it
touches: `--resize` (lowers the booking, not the cap, by design); the AIRA-325 history charge
(below `factor x pin`: it must set its own floor ratio, proposed 0.5, and restate the bound as
`ceiling / 0.5`); a ceiling that falls after the grant (the throttle); and a restart re-declare
charged above the ceiling. It is used only to reason about how bad a slice-level overrun can
be, never as a gate. The OOM-steering argument that a job's cap is within its budget
(`admitScopeBudgets`, `oomsteer.go:453`, and its doc comment) must be restated against it.

**What can still go wrong, accepted explicitly.** Per-job caps cannot cause a new kill. A
**slice-level** OOM can now kill a job that is below its own pin, because the caps can
collectively exceed the slice's limit; steering prefers the job that is over its charge
(`evaluateOOMSteer`, `oomsteer.go:252`), but only biases the victim. The watchdog exempts
confined work (`watchdog.go:816`) and is no backstop. The first symptom is more likely
memory.high throttling (`MemoryHigh=60G`) than a kill, so the gates watch both (section 6).

**Charge lifecycle, every site decided** (build must also add a guard test that fails if
`.reserve` is read in admission arithmetic outside this allow-list):

| Site | Decision |
|---|---|
| grant echo / scope `memory.max` | pin (`reserve`), unchanged |
| `ledgerCharge()` (`admit.go:324`) and every sum through it | charge |
| fit check in `evaluateAdmitQueue` | charge, plus the physical-fit rule |
| head eligibility `admitCouldFitEmptyLocked` (`admit.go:2663`) | ledger part in charge bytes, AND the physical part in pin bytes: a discounted waiter is head-eligible only if `charge <= effectiveMaximum - headroom` **and** `pin <= effectiveMaximum - headroom` (the full pin must be feasible on an empty slice). Charge alone would make a pin above the throttled ceiling a head it can never leave, and a pushed one would wedge the machine (the guard commented in `evaluateAdmitQueue` near `admit.go:2600` exists to prevent exactly that) |
| bypass budget `admitBypassBudget` (`admit.go:429`) and `head.bypassed` | charge bytes throughout |
| queued-ahead bytes, queue position, `--list`, `aira top`, `ci_dump` | show both pin and charge; sums use charge |
| `confine --resize` (`confine_resize.go:121-132`) | writes the charge; "must name a smaller reserve" is judged against the charge; floor stays the job's live use |
| re-declare after reconnect or daemon restart | the ARDR re-declare record is frozen and carries only scope id, RAM bytes (the granted pin), cores and parent (`redeclare.Record`, `internal/redeclare/redeclare.go:74`; the keeper sends `grant.Reserve`, `lease_keeper_linux.go:100`), so **eligibility cannot be reconstructed**. Reconnect to a live daemon: the granted waiter is re-anchored in place and **keeps its existing charge**. After a daemon restart (no memory of the grant): charged the **full pin** (fail-safe, as `--resize` today). Accepted consequence: a restart under load makes the ledger jump to the sum of pins, signed and possibly over the ceiling, so new admissions wait until releases recover it; it is the existing restart behaviour, now with more discount to lose. The frozen ARDR record is not changed by this plan. A change of factor or mode applies to new grants only |
| OOM-steering budget (`admitScopeBudgets`) | charge, as `--resize` does today |
| aitest workers (`worker_admit.go`) | never discounted; they consume the same ledger sums |

**Modes** `off | observe | enforce` on the install-record pattern of the slice ceiling. Observe
computes and logs `would-charge=X charged=Y` per admission and changes nothing. Factor rolls
out 0.9 then 0.8.

### 5.3 History refinement (AIRA-325, gated)

`charge = min(factor x pin, max x 1.25 of the last 20 clean peaks at the finest key level with
3+ clean samples)`, the levels and the key function being those the first reader builds
(section 10, hand-off; rev 2's replay levels are the starting point). **"OOM on record"** means
any row with `oom=1` in that key's window of 20, at the key level being used; such a key is
charged its flat charge. At 0.8 it reaches 1.52x with a p99 of 0.92-0.93 (section 2, argv-only
keys), a margin too thin to enable on the replay alone. Ships only if the enforce data on the
flat discount, replayed by start time (`started_at_ms`) on repo-keyed rows, shows headroom, and
decided with a measured exceedance target, not by default.

### 5.4 Fast-lane queue order (AIRA-326)

A waiter is **fast** if, at enqueue, its key has 3+ clean samples, no OOM in the window, p90
`wall_ms` under 5 minutes, p90 peak under 4 GiB, **and its own ledger charge is at most
4 GiB** (a quick job pinned at 32 GB would otherwise become a blocked head with a large
bypass budget). Unknown is never fast, and that includes a row with no key (NULL `repo`) or a
NULL `wall_ms`. The class and the bonus are **frozen on the waiter at enqueue**, from the
history read the daemon already makes before taking `queue.mu`; the evaluator never reads the
database. If 326 is the first reader, it builds the key function, its level and its index
(section 10, hand-off), on 323's repo-keyed data.

To compute the key at enqueue, 326 needs the repo identity on the **admit** request (one more
optional admit-frame field) and must compute the key from the same signature the report
carries. The admit frame's signature is truncated for display to
`runner.ConfineReservationSignatureWireLimit` (512 characters, `confine_manage.go:860`;
`boundedAdmitSignature`, `admit.go:1144`), while the stored signature is the full argv. 326 must
establish which signature reaches the key function, or the enqueue key and the stored key can
differ for long argvs.

`admitOrderLocked` already derives the order and stores nothing. Its second tier changes from
enqueue order to ascending `enqueued - bonus x fast`, ties by `seq`. `bonus` defaults to 10
minutes (install knob; 0 = today's order exactly). Pushed waiters still go first, latest push
first. Worker waiters have no key and are never fast. One order source remains for the
evaluator, the "queue position" line and `aira top`. If AIRA-340 has landed, its `high` tier sits
between pushed and normal waiters, and the fast bonus orders waiters only within the normal tier
(proposed; 326's plan confirms).

**What the bonus guarantees, precisely.** A heavy waiter that has waited more than `bonus`
is ahead of every job that arrives after it. It does **not** prevent delay: a fast job arriving
in the heavy job's first `bonus` minutes goes ahead, and, being ahead rather than behind a
head, is not metered by the AIRA-307 budget. That is bounded (the window ends) and cheap (fast
jobs are small). Head selection, the freeze duty cycle, the exclusive gate and bypass
accounting derive from the order each pass and tolerate displacement (a push already does
this). The client's position can move backwards when a fast job arrives; that is displayed,
not hidden.

It fixes who gets freed memory first, not capacity; capacity comes from 5.2.

## 6. Rollout and the gates that decide it

1. **AIRA-323 alone, first** (release v0.43). It is pure data collection, and data takes weeks
   to accrue.
   - The cutover is the usual client install plus `systemctl --user restart
     aira-daemon.service`, after `aira confine --list` and a heads-up to peers (the shared
     daemon-restart care). With no protocol bump nothing is refused: launches, queued jobs and
     stale clients are unaffected, and only the four new facts are lost for runs that end
     between the two steps (section 5.1.6). Stale binaries and MCP servers keep writing rows
     with the four columns NULL until they are restarted; peers are told so, not warned of
     refusals.
   - Smoke test after cutover. Each probe run carries a fresh nonce in its argv, so its
     signature (the effective argv joined with NUL, `ResourceSignature`,
     `resource_estimate.go:82`) is its own, and the check counts only rows inserted after the
     probe started. The daemon commits the row before it acknowledges the report, and the
     report is sent before `aira confine` exits (`confine_linux.go:1445-1460`), so the row is
     there when the command returns.

     ```
     db=~/.local/state/aira/state.db
     nonce=aira-323-smoke-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
     before=$(sqlite3 -readonly "$db" "select coalesce(max(rowid),0) from confine_peak_history")
     aira confine -- true "$nonce"      # from a git checkout
     sqlite3 -readonly "$db" \
       "select count(*), count(wall_ms), count(wait_ms), count(started_at_ms),
               sum(repo glob '[0-9a-f]*' and length(repo)=16)
        from confine_peak_history
        where rowid > $before and instr(signature, '$nonce') > 0"
     ```

     The output must be exactly `1|1|1|1|1`: one new row for this probe, all four facts
     non-NULL, a 16-hex-digit repo. The same probe with a second nonce, run from `/`, must give
     `1|1|1|1|0`, and that row's `repo` must be `none`. `0|0|0|0|` (no row) is a failure: the
     probe's report was lost or not sent.
     The check must survive two interleavings, both of which fooled rev 3's
     `order by rowid desc limit 1`: a stale client's report (NULL facts) landing after the
     probe's row (rev 3 read the stale row: a false failure), and the probe's report suppressed
     while another session's populated row is the latest (rev 3 read that row: a false pass).
     The nonce and the `rowid > $before` bound make another session's row unable to match in
     either case.
   - The branch follows Batch A, which edits the same report lines.
2. AIRA-324 in observe for a week of normal load; then enforce at 0.9 for a week, then 0.8.
   **Gate to start enforce:** a counterfactual replay of the real arrival/start/end timeline
   (start = `started_at_ms`, arrival = start - `wait_ms`, from AIRA-323) that books jobs at the
   discounted charge, admits more jobs than before, and shows the concurrent total of
   `memory.peak` stays under 0.9 x the slice's `MemoryHigh` at the busiest hour, using
   burst-weighted and duration-weighted draws. The replay states the coverage limits of section
   2: evicted rows of frequent commands and failed runs are missing. The pre-enforce kill count
   is **not** a gate (zero kills before enforce proves nothing about after).
   **Rollback triggers** (any one flips the mode back to observe and files a finding): an
   in-slice `oom_kill` of a job whose peak was under its pin; `memory.events` `high` count or
   `memory.pressure` `some avg10` over their pre-enforce weekly 99th percentile for two
   consecutive hours; more than 5% of discounted jobs above their charge in a week.
3. AIRA-325 only after (2) has two clean weeks at 0.8, same gates, factor lowered only via the
   combined rule.
4. AIRA-326 once AIRA-323 has two weeks of `wall_ms`; independent of 324/325. Gate: fast-class
   p95 `wait_ms` falls by half against the prior fortnight, heavy-class p95 `wait_ms` rises by
   at most 10%. The "prior fortnight" baseline exists only from 323's cutover onward.

## 7. Invariants the build must preserve

- A job's `memory.max` is never below what its client asked for; only the booking shrinks.
- `sum(charge) <= ceiling - headroom`, signed, as today; no clamp that hides a deficit. At each
  discounted grant, that job's cap <= charge / factor (the grant-time bound of section 5.2,
  with its exceptions; not a standing invariant).
- A discounted job's full pin fits `ceiling - headroom - effectiveCurrent` at grant.
- The order has one source; `queue.waiters` is never reordered; the class is frozen at enqueue.
- Absent or insufficient history means "no discount from history", never a guess or a zero.
- A check that cannot establish its result reports `unevaluated`.
- (323) A value the client could not establish is absent on the wire and NULL in the store,
  never 0. A present but invalid value refuses the whole report; the client's once-only
  re-send then stores the sample without the four facts. Admission, the estimator and
  retention are unchanged by 323.
- (323) `repo` is stored only when established. Outside any git repository the established
  value is `none`; an unevaluated identity is NULL, never `none`.
- (323) A report is re-sent at most once, only after a well-formed `E_DAEMON_PROTOCOL` refusal
  of a frame that carried a new field, and the re-sent frame differs from the first only by
  lacking the four new fields.

## 8. Tests required (each must fail against the wrong implementation)

AIRA-323 (each names the mutation that must turn it red; T6-T9 renumbered in rev 3, see the
rev 3 changes):

- **T1. Real client-to-daemon frame.**
  - Setup: a test in package `daemon` starts a real `Server` on a temporary socket with a
    temporary store. It calls the real `runner.ReportPeakSample` with a `ConfinePeakReport`
    carrying a peak, a budget pair, `WallMS`, `WaitMS` (a real 0), `StartedAtMS` and a
    16-hex-digit `Repo`. It then reads the row back with SQL.
  - **Delayed delivery:** `StartedAtMS` is set to one hour before the call and `WallMS` to
    1 000, so `at - wall_ms` is about one hour away from `started_at_ms`. The stored
    `started_at_ms` must equal the sent value exactly.
  - Pass: every stored value equals the sent one, and the server saw exactly one
    `confine-report` request (counted with `Server.OnRequest`, `server.go:889`). No frame is
    hand-built on either side. `TestConfineReportCarriesTheBudgetPair`
    (`confine_budget_test.go:159`) calls the handler directly and is not a model for this test.
  - Mutations, each red:
    - drop a field from the sender: NULL;
    - leave any one of `wall_ms`, `wait_ms`, `started_at_ms`, `repo` out of the allowlist (four
      mutations): the first frame is refused, the re-send stores NULL facts and the request
      count is 2;
    - copy the `> 0` idiom for `wait_ms`: a real 0 is lost;
    - the daemon derives `started_at_ms` as `at - wall_ms` instead of storing the sent value
      (off by an hour).
- **T2. Absent means NULL; no kind gate.**
  - A report without the new fields stores NULL in all four columns, asserted with `IS NULL`.
  - A `pytest-worker` report through the handler carrying a present `repo` and timings stores
    them as given; a `pytest-worker` report without them stores NULL. The present-`repo` case
    tells a kind gate apart from an absent repo.
  - Mutations, each red: store 0 (or `""`) for absent; drop the four fields for any kind other
    than `confine`.
- **T3. Validation table**, at the handler, the one layer that validates (5.1.2). A `daemon`
  test sends each `Request.Args` map to `Server.confineReport` over a temporary store; the map
  is the test's input (T1 covers the real sender).
  - Refused with `E_DAEMON_PROTOCOL` and no row written (every bound of every field has its own
    case one past it, so a bound deleted from one field's check alone is red):
    - `wall_ms` of -1 and of 31,622,400,001 (366 days + 1 ms);
    - `wait_ms` of -1 and of 31,622,400,001;
    - each of `wall_ms`, `wait_ms` and `started_at_ms` as 1.5 and as `"abc"`;
    - `started_at_ms` of 946,684,799,999, of 4,102,444,800,000, of 0 and of -1;
    - `repo` of `""`, `"NONE"`, 15 or 17 hex digits, a 64-hex-digit value, upper-case hex, or
      a path.
  - Accepted (boundaries), each stored as sent: `wall_ms` and `wait_ms` each of 0 and of
    31,622,400,000 (exactly 366 days); `started_at_ms` of 946,684,800,000 and of
    4,102,444,799,999; a `started_at_ms` later than the daemon's own clock (a clock step is
    not refused); `repo` of `none` and of 16 hex digits.
  - Mutations, each red: remove each of the seven bounds in turn (lower and upper of
    `wall_ms`, of `wait_ms` and of `started_at_ms`, and the `repo` pattern; deleting only the
    `wait_ms` upper bound is caught by `wait_ms` = 31,622,400,001 alone); move any bound by one
    (`<` for `<=` and the reverse, caught by the accepted boundaries); skip the check of any
    one field (the writer does not re-check, so the bad value is stored and the "no row"
    assertion fails); refuse `started_at_ms` later than `at`.
- **T4. Launch-site timing** (fake `confineDeps` with the `wallNow`/`monoNow` seam of 5.1.1,
  captured `reportPeak`). Every expectation is on the client's own fake clocks; none compares
  the client's figure with the daemon's `waitedMS`.
  - **One composite scenario, every phase moving both clocks by a distinct amount**, so each
    interval's start and end points are pinned together rather than one property per case.
    The fake clocks start at mono 0 and wall T0.
    - Phase A, inside the fake `repoIdentity` (called before admission, 5.1.3): `monoNow`
      advances 7 ms and `wallNow` advances 11 ms.
    - Phase B, inside the fake `admit`: `monoNow` advances 30 000 ms (12 000 ms queued, the
      connection dropped, one 500 ms reconnect gap, 17 500 ms queued again) and `wallNow`
      advances 29 000 ms. It returns a lease with `waitedMS = 17 500`. Let T = T0 + 29 011,
      the wall reading at the admission return.
    - Phase C, inside the fake `start` (`deps.start`, `confine_linux.go:1229`, which runs
      between the admission return at `:744` and the report at `:1456`): `monoNow` advances
      2 000 ms and `wallNow` steps back one hour.
    - Pass, all three asserted together on the one captured report: `WaitMS` = 30 000,
      `WallMS` = 2 000, `StartedAtMS` = T (the pre-step reading at the admission return).
  - `WaitMS` is set iff `deps.admit` returned a lease; it is nil for `bypassed`, `disabled` and
    `no-daemon`. A daemon `unevaluated` grant (with a lease) sends `WaitMS`; the local
    `no-daemon` `unevaluated` does not.
  - Mutations, each red (the value in brackets is what the composite case then sees):
    - start the wall timer before `deps.admit` (`WallMS` = 32 000), or before `repoIdentity`
      (`WallMS` = 32 007);
    - read `wallNow` for the start time before `deps.admit` (`StartedAtMS` = T - 29 000), or
      before `repoIdentity` (T0 = T - 29 011);
    - start the wait timer before `repoIdentity` (`WaitMS` = 30 007);
    - end the wait timer at report time instead of at the admission return (`WaitMS` =
      32 000);
    - send `admission.waitedMS` for a granted admission (`WaitMS` = 17 500);
    - read the start time at report time as `wallNow() - wall_ms` (`StartedAtMS` =
      T - 3 600 000 - 2 000 = T - 3 602 000);
    - measure `wall_ms` as a difference of `wallNow` readings (gives a negative value);
    - send `admission.waitedMS` (the Go zero) for a lease-less admission (lease-less case);
    - key the rule on `state == "unevaluated"` instead of `release` (lease-less case).
- **T5. `RepoIdentity` on real temporary repositories:**
  - plain repo and a subdirectory: the same ID;
  - linked worktree: equals the main checkout;
  - submodule: distinct;
  - outside any repository: `("none", true)`;
  - a symlink, **outside the repository**, that targets a **subdirectory** of it (`link ->
    repo/sub`), passed as the logical path the way `os.Getwd` returns it: equals the repo's
    ID. (A link to the repository root would pass without step 0, because `stat` follows the
    link; it is not a sufficient case.)
  - a working-directory path that no longer exists: `("", false)`;
  - an unreadable or malformed `.git` file: `("", false)`;
  - equality with `app` ProjectID's first 16 hex digits for a plain repo and a worktree (this
    check sits in an `app` package test, because `runner` cannot import `app`).
  - Mutations, each red: hash the worktree's own `gitdir` instead of its `commondir`; skip
    step 0 (gives `none` for the symlink case); return `("none", true)` on an error.
- **T6. Repo identity at the launch site** (fake `confineDeps.repoIdentity`, captured
  `reportPeak`; the fake `admit` records how many times `repoIdentity` had been called when it
  was entered).
  - `("0123456789abcdef", true)`: the captured report's `Repo` equals it.
  - `("none", true)`: `Repo` is `none`.
  - `("", false)`: `Repo` is `""`, so the field is absent on the wire (T2 covers NULL).
  - In every case `repoIdentity` is called exactly once, and the count seen at `admit` entry
    is 1.
  - Mutations, each red: drop the assignment to the report; send `none` when `ok` is false;
    compute the identity after admission (count at `admit` entry is 0); call it twice.
- **T7. Nothing else moves.**
  - 21 rows of one signature from two repos leave 20 retained;
  - `ResourcePeakHistory` and `ConfinePeakP90` return the same values with and without the new
    columns populated.
  - Mutation: retention per `(signature, repo)`, red.
- **T8. Schema upgrade.** A store created with the pre-323 DDL opens to the new shape; old rows
  are NULL in the new columns; DDL and ALTER column types match.
- **T9. Version skew and the re-send** (package `daemon`, real `Server`, real
  `runner.ReportPeakSample`, requests counted with `Server.OnRequest`). An **old daemon** is the
  real handler with `confineReportFields` swapped, for the test, to master's six-field set;
  master's count guard and allowlist loop read that map (`confine_report.go:23-30`), so both
  refusal paths are master's. The swap is a package variable, so these cases do not run in
  parallel and restore the map with `t.Cleanup`.
  - (a) Old daemon, a report with peak, budget pair and the four facts (nine fields: the count
    guard path): `ReportPeakSample` returns nil; the row holds the peak, the budget pair, the
    sent `oom` and NULL facts; 2 requests.
  - (b) Old daemon, a report with only `signature`, `oom`, `wall_ms` and `started_at_ms` (four
    fields: the unknown-field path): the same outcome.
  - (a) and (b) each run twice, once with `OOM = true` and once with `OOM = false`, and assert
    the stored `oom` column (1 and 0) on the row the re-send wrote. The estimator escalates on
    a recorded OOM (`estimate:oom-escalated`, `admit.go:1548`), so the bit must survive the
    re-send; the two values together catch a re-send that hard-codes either one.
  - (c) New daemon, a report with `Repo = "NONE"` (the client does not pre-check) and
    `OOM = true`: stored with the peak, `oom = 1` and NULL facts; 2 requests.
  - (d) New daemon, a report with no new field and an unknown `Kind`: refused, error returned,
    1 request.
  - (e) A report with an unknown `Kind` and `WallMS`: refused twice, error returned, exactly 2
    requests.
  - (f) A test listener that reads the frame and closes without replying: error returned,
    exactly 1 connection.
  - The protocol pin `TestRunnerDaemonProtocolVersionMatchesTheDaemon`
    (`internal/runner/protocol_pin_linux_test.go:25`) stays at 15, unchanged.
  - Mutations, each red: no re-send ((a), (b)); match only the "unexpected confine-report
    field" text ((a)); re-send keeping the new fields ((a), (b), (c)); re-send also dropping
    peak or budget ((a)); re-send with `oom` false ((a), (b) and (c) with `OOM = true`);
    re-send with `oom` true ((a), (b) with `OOM = false`); re-send when the frame carried no
    new field ((d)); re-send until success ((e)); re-send on a transport error ((f)).

AIRA-324-326 (unchanged from rev 2, except that test 7 moved to 323 and was then cut with the
key (rev 3), and test 8 moved to the readers):

1. Discountable job: ledger charge = ceil(0.8 x pin), grant echoes the pin, scope
   `memory.max` = pin. Mutation: grant echoes the charge -> red.
2. Non-discountable (`--memory-max`, `aira run`, scope-less, delegate parent, exclusive,
   worker, shim mode): charge = pin. Mutation: infer from `pinned` -> red.
3. Physical fit: with real free memory below the pin the job waits though the ledger has room;
   a mutation removing the rule -> red.
4. Every row of the lifecycle table has a test; plus a guard test failing on any new
   `.reserve` use in admission arithmetic outside the allow-list.
5. Resize against the charge; reconnect to a live daemon keeps the existing charge; after a
   daemon restart the re-declared lease is charged the full pin (not f(pin), not the resized
   value); a restart under load leaves a signed, possibly negative, available that recovers on
   release; a factor or mode change leaves granted charges alone.
6. Head eligibility: a discounted waiter whose pin is above the (throttled) ceiling is never a
   head even when its charge fits, pushed or not (mutation: charge-only eligibility -> a pushed
   one freezes the queue -> red); bypass budget accumulates in charge bytes over several passes.
7. (Cut: it pinned the key function, which moved to the first reader.)
8. Each reader's window query (325, 326, 331): a 200k-row fixture, one indexed read on the
   index that reader adds, under 250 ms.
9. Order: fast job enqueued 5 min after a heavy one goes ahead, one enqueued 11 min after does
   not; a fast job whose own charge is over 4 GiB is not fast; pushed first; position line and
   `aira top` equal `admitOrderLocked`; class frozen (history changes after enqueue do not move
   it); `bonus = 0` equals today's order (property test over random queues); worker waiters
   never fast; NULL `wall_ms` or NULL key never fast. Multi-pass: freeze anchor, accumulated
   bypass and yield refills unchanged by displacement.
10. A heavy waiter older than `bonus` is ahead of every later arrival, any arrival rate.

## 9. Risks

- The independence assumption and end-time look-ahead (section 2) are the weak points of the
  evidence; section 6's counterfactual replay on real timestamps is the control, and the plan
  stops at observe if it fails.
- Slice-level OOM can kill a job below its pin (section 5.2): accepted, gated, reversible by a
  mode switch with no migration.
- A first large run of a command after only small ones is invisible to a history charge; the
  flat discount does not depend on history, which is part of why it comes first.
- The discount adds a client-side flag and an admit wire field (AIRA has no compatibility
  obligation; 324's plan chooses between a bump and AIRA-340's optional-field pattern).
- Re-opens, narrowly, the live-number-in-admission trade of 2026-09-10: the physical-fit rule
  reads `effectiveCurrent`, which `checkedAvailable` already reads. No per-job read.
- (323) Until both the client install and the daemon restart are done, the four facts are lost
  (section 5.1.6); peak, OOM and budget are kept by the re-send. A re-send that runs past the
  250 ms report deadline loses the row (unmeasured).
- (323) The re-send also hides a client bug in a new field from the operator (the row is stored
  with NULL facts, silently). T1 and T4 assert the sent and stored values, so a bug is caught in
  tests, not in production.
- (323) Repo-scoped keys change coverage in a direction not yet measured (section 2: the OOM
  veto means a split can raise it as well as lower it). If it falls a lot, 326 and 331 have less
  to work with; the first reader measures it on 323's data before it is built.
- (323) **RANT-133 is not fixed by 323.** The estimator still keys on argv alone, so one
  `./runall.sh` OOM still escalates every repo's `./runall.sh` estimate. The reported 9 GiB
  (`estimate:oom-escalated`, 1.5 x a 6 GiB pin) is a report, not a measurement. The estimator
  fix is AIRA-321. 323 only records the repo that 321 and the reader keys use.

## 10. Follow-ups and sequence notes

**Hand-off: the command key belongs to the first reader** (AIRA-326, 331 or 325, whichever is
built first). That ticket, not 323:

- tunes the key function on real repo-keyed rows from 323 (at least two weeks of them): the
  unwrap and masking rules, the level or chain of levels, and the repo-split coverage of
  section 2, measured;
- writes the Go function and, if it wants one, its own golden table or reference; the replay
  script's `keys()` (rev 2's levels N1-N3) is a starting point with no frozen contract;
- adds the index its query needs, next to that query and its latency test (test 8 below);
- builds the key for every 323 row exactly from `signature` and `repo` (section 5.1.4), as a
  stored column filled by one `UPDATE` or at read time; rows without a `repo` get no
  repo-split key;
- if it keys at enqueue (326), puts the repo on the admit request and computes the key from the
  same full signature the report carries (section 5.4).

Follow-ups, deliberately not in this plan:

- Live per-job usage correction, only if the enforce data says the discount is not enough.
- Held-back headroom for the fast lane, only if its wait target is missed.
- aitest worker pools keep their own cost-ranked ordering (AIRA-261).
- (323 deferrals) Showing `wall_ms`/`wait_ms` in `confine --budget`, `confine --dump` or
  `aira top`; timing on the `aira confine-report` pool face; recording failed runs (AIRA-264
  decided against it); a printed notice when the re-send fires; any command key (hand-off
  above).

Sequence notes (verified where marked):

- **Batch A, then 323.** Batch A edits `confine_linux.go` and `admission_linux.go`, the report
  path. The 323 build branches after it merges. Line numbers cited in 5.1 are master at
  `4d935a0c` and will move. The functions named stay.
- **AIRA-340** (`--priority high|normal`, on branch `aira-priority-launch`, head `4effa392`,
  not merged; verified). It adds an optional admit-frame field with no bump (proto stays 15) and
  a tier in `admitOrderLocked`. 323 follows the same no-bump pattern on a different frame
  (`confine-report`); both edit `admission_linux.go` in different functions (`admitExchangeOnce`
  and `admitThroughDaemon` for 340, `ReportPeakSample` for 323) and both add a paragraph to the
  `protocol.go` history comment, so whichever lands second rebases a comment. Neither bumps,
  so neither takes a protocol number from the other. 326 must compose its bonus with 340's
  tiers (section 5.4).
- **AIRA-341** (delete the machine-wide p90 prior; backlog plan B#3a). Its ticket file is on
  branch `origin/aira-housekeeping-2` (commit `eeb752f9`), not on master (verified). It
  interacts with 323 only through the shared table: 323 adds nothing to `ConfinePeakP90`
  (`confine_peak_history.go:201`), and deleting it removes the AIRA-314 full-table `GROUP BY`
  that section 5.1.5 warns readers against.
- **AIRA-228** (collapse the per-slice queue map): `admit.go` and `reload.go` only. There is no
  file overlap with 323. It lands before 324 and 326, which edit the same evaluator.
- **AIRA-225** (moving the peak report after teardown): the same lines as 323's report change
  (`confine_linux.go:1446-1462`). It is built after 323 and must keep the new fields and the
  `wall_ms` end point (the report's construction).
- **AIRA-321** (estimator subject = argv + repo): it reuses `runner.RepoIdentity`. When it lands,
  retention becomes per argv-and-repo, which ends section 5.1.5's cross-repo eviction.
- **AIRA-331** (cold-start cap from a coarse key): a candidate first reader (hand-off above). The
  AIRA-329 diagnosis behind it used argv-only keys.
- **AIRA-236** (time tracking): 323 records the timing half, not the `--ticket/--phase/--label`
  attribution.

## 11. Change log

- **Rev 2 -> 3** (this revision, for AIRA-323's plan gate): see "Rev 3 changes" at the top.
  Verification for the final form:
  - The replay script equals rev 2's: `git diff 4d935a0c -- docs/dev/aira-history-charge-replay.py`
    prints nothing. The section 2 figures are rev 2's measurements with that script; they were
    not re-run for rev 3, and nothing in rev 3 changes what the script reads.
  - Retention exposure (6.2%): the query in section 5.1.5 plus `count(distinct signature)`,
    re-run read-only on 2026-11-14 for this form (figures in section 2).
  - Every code citation in 5.1 was re-read on master `4d935a0c` and, for AIRA-340, on
    `aira-priority-launch` at `4effa392`. Unverified items are marked where they occur (the
    live daemon's `confine-report` refusal; the re-send's time against the 250 ms deadline).
  - The key golden table, its mutation counts and the `--self-test` results recorded by the
    earlier rev-3 rounds are withdrawn with the key (cut 1); no count is carried forward.
- Rev 2 after confirmation review: re-declare reconstruction fixed (frozen ARDR record: live
  reconnect keeps the charge, restart charges the full pin); head eligibility needs full-pin
  feasibility; the cap bound restated as a grant-time bound with its exceptions; replay `time`
  option parsing fixed.
- Rev 1 -> 2: flat discount first; history predictor demoted (AIRA-325) with "OOM on record"
  defined; valve dropped for the physical-fit rule; lifecycle table; retired invariant named
  and restated as `sum(memory.max) <= ceiling / factor`; discount restricted to scope-backed
  `--memory-reserve` pins via a new client flag; fast class frozen at enqueue and charge-capped;
  starvation claim corrected; `aitest-parent` namespace kept, `time` flags handled; replay
  corrected (OOM veto, `memory.peak`, leakage and retention caveats stated); gates rebuilt
  (counterfactual replay, memory.high/PSI, explicit rollback); monthly re-selection removed.
