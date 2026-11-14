# Discounted admission charge for pinned jobs, and a fast lane for quick jobs

Tickets: AIRA-323 (completion telemetry), AIRA-324 (flat charge discount for pinned jobs),
AIRA-325 (history refinement, gated), AIRA-326 (fast-lane queue order). The owner un-held
AIRA-323 on 2026-11-14 for release v0.43 (the ticket file still carries the old `hold` and
`v0.41` label until the coordinator's transition lands). 324, 325 and 326 stay held. Status:
plan, not built. Process class: **correctness-critical admission core** (it changes what the
ledger charges and the order the evaluator runs): full two-loop, Opus builds TDD, Fable
reviews in a throwaway detached worktree. AIRA-323 is a **client + daemon + wire + schema**
change with a protocol bump (section 5.1); the shared-daemon restart care applies at every
cutover.

Revision 3. Reproduction of every measurement: `docs/dev/aira-history-charge-replay.py`
(read-only on `~/.local/state/aira/state.db`; run it under `aira confine`). The command-key
reference is the same script's `keys()`, checked against the committed golden table
`docs/dev/aira-history-charge-keys.golden.json` by `--self-test`; the Go key function must
reproduce the same table.

## Rev 3 changes (what changed since rev 2, and why)

Scope of the revision: only AIRA-323 (section 5.1) is redesigned. Sections 5.2-5.4 keep their
rev-2 design; their code citations are refreshed because `admit.go` moved (AIRA-328 merged
after rev 2), and they gain sequence notes.

1. **Repo identity in the command key** (owner decision, 2026-11-14). The client sends a
   16-hex-digit repo identity, derived from its working directory, or `none` outside any git
   repository; the key is prefixed with it. Two `./runall.sh` scripts in different repos no
   longer share a key (RANT-133). The estimator's subject (the exact-argv signature) does not
   change: no history is reset. Why: the owner chose "new key column only, no reset" over
   waiting for AIRA-321. Section 5.1.3.
2. **The wire change is written down.** Rev 2 said the columns are "written at the existing
   teardown site", but the daemon's report handler takes a closed allowlist and the client's
   frame carries no timing. Rev 3 specifies the new frame fields, which side computes each
   value, the validation rules, the protocol bump, and old/new client and daemon behaviour,
   each verified by reading the code. Section 5.1.1-5.1.2, 5.1.6.
3. **Three key columns, not one.** Rev 2 had one `key` column, three key levels and one index,
   which a reader could not use for a level chain: the command-and-first-positional level is
   not a prefix of the others. Rev 3 stores the three levels as three columns. Rev 2 also
   described the finest level as "first 14 tokens", but the script uses argv[0] plus 14
   arguments. Rev 3 states what the script does. Section 5.1.4.
4. **Cut: the `started_at` column and wire field.** The daemon stamps `at` when the report
   arrives, and `wall_ms` is measured up to the moment the report is built, so the start time
   is `at - wall_ms` to within one local socket write. Storing a third, derivable timestamp
   would add a column, a field and a validation rule whose only possible new content is a
   disagreement with the other two. Section 5.1.1.
5. **Cut from 323: the `(kind, key, at)` index and the 250 ms window-query test.** AIRA-323
   has no reader of the key. Each reader ticket (325, 326, 331) adds the index for the level it
   reads, in the same change as the query that uses it. Adding an index later needs no
   backfill because the columns are written from 323's first day. Section 5.1.5.
6. **`wait_ms` is measured by the client across the whole admission call.** The coordinator's
   brief suggested relaying the grant's `waited_ms` (the trailer's figure). That figure
   restarts when the client reconnects mid-queue, because the daemon enqueues a fresh waiter.
   So 323 measures the felt wait, which is what the fast-lane gate judges. It equals the
   trailer's figure when there was no reconnect. Section 5.1.1.
7. **Honest-evidence additions to section 2.** All rev-2 numbers were measured on argv-only
   keys (no row has a repo), so repo-split coverage is at most the 72% measured. The share of
   history under the 20-row cap was measured (6.2%). Failed runs are never recorded (AIRA-264).
   Section 2.
8. **Correction to the backlog plan** (`docs/plans/2026-11-14-backlog-plan.md`, line 171 and
   open item at line 196). With the protocol bump, a stale client does not just lose its peak
   report: **its new launches are refused** at admission. Only jobs already running at cutover
   lose their row. Verified in section 5.1.6.
9. **Replay script and golden table.** `keys()` takes the repo identity. Legacy rows (no repo)
   are keyed under a replay-only placeholder, which groups them exactly as rev 2 did. This
   holds by construction: every legacy key is a fixed prefix plus the unchanged rev-2
   expression, and the exact-signature level is untouched. So the grouping is unchanged; the
   printed numbers move only because the store keeps growing. The script gains `--self-test`
   against a new committed golden table of 26 cases. Rev 2 promised that table but no file
   existed.
10. **Interactions** with AIRA-340, 341, 228, 225, 321, 331 and 236 are recorded as sequence
    notes in section 10.

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
- **(rev 3) Every key above is argv-only.** No stored row has a repo identity, so the replay
  keys them all under one legacy placeholder, which is rev 2's grouping exactly. A repo
  identity can only split keys, so the 72% history coverage is an **upper bound** on coverage
  with repo-scoped keys. The real figure is unmeasured until AIRA-323 rows accrue. It is
  assumed, not shown, that the split costs little.
- **(rev 3) Retention removes rows, mostly for frequent commands.** Measured 2026-11-14 on the
  live store (`~/tmp` probe, read-only; the query is reproducible with the SQL in section
  5.1.5): 232,319 retained confine rows over 158,743 signatures. 720 signatures sit at the
  20-row cap and hold 14,400 rows, 6.2% of what is retained. How many rows those 720
  signatures have lost to eviction cannot be counted from the table, because evicted rows are
  gone. Any timeline replay (section 6) therefore under-represents the most frequent commands.
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
- (rev 3) The repo identity goes into AIRA-323's command key, in new columns only, with no reset
  of existing history; the estimator's subject stays argv-only until AIRA-321 (2026-11-14,
  backlog plan B#2a).
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
  if the enforced flat discount shows the margin to go lower. The key function is still built
  in AIRA-323, because the fast lane (AIRA-326) and the cold-start cap (AIRA-331) need a
  per-command class and the rows must carry it from the first day.
- **Pressure valve `f(p)`**: dropped. `checkedAvailable` (`admit.go:2726`) already books the
  larger of real use and the ledger, so once real use rises the physical term dominates;
  revision 1 wrongly said live reads had been removed. The one thing the ledger cannot see is
  whether a job's full cap could be backed by real free memory, so the flat discount adds one
  rule for that (section 5.2), and nothing is tuned.
- **Monthly predictor re-selection**: dropped.
- **Per-job live usage in the ledger**: still not built; revisit only if the enforce data says
  the flat discount is not enough.
- **Reserved headroom for the fast lane**: not built; measure fast-class waits first.

AIRA-323's own minimum is "for each finished job, record how long it waited, how long it ran,
and which command it was, so a later reader can group jobs". Rev 3 keeps to that: the client
sends three raw facts (two durations and a repo identity), and the daemon stores them with three
key strings derived from the signature it already receives. A derivable start time, an index
with no reader, and client-side key computation are all cut (rev 3 changes 4-5, section 5.1.3).

## 5. Design

### 5.1 Telemetry (AIRA-323)

Pure data collection: nothing in admission reads the new columns. The estimator
(`ResourcePeakHistory`, `ConfinePeakP90`), retention, `confine --budget` and `confine --dump`
behave exactly as today.

#### 5.1.1 What is measured, and by whom

| Value | Computed by | How | When it is absent |
|---|---|---|---|
| `wall_ms` | client | Monotonic milliseconds from the return of the admission call (`deps.admit` in `confine_linux.go`, about line 744) to the construction of the `ConfinePeakReport` (about line 1456). It covers the run, scope setup and the teardown reads before the report. | Never absent on a confine report. A sub-millisecond run reports 0, which is a real 0. |
| `wait_ms` | client | Monotonic milliseconds across the whole `deps.admit` call. | Sent **only** when the admission ended in a daemon grant. `deps.admit` is `admitConfine` (`confine_linux.go:1900`), which returns the package's `admissionResult` (`admission_linux.go:27-35`). The launch site is in the same package and tests `admission.release != nil`. `release` is set in exactly one place, the lease return at `admission_linux.go:689`, for every daemon grant: `immediate`, `waited`, and the daemon's own `unevaluated` grant. The client-local results carry no `release` and a `waitedMS` that is a Go zero value, not a measurement, so no wait is sent for them. Those results are `disabled` (`admission_linux.go:208`) and `unevaluated` with reason `no-daemon` (`:240`). They share the state name `unevaluated` with a daemon grant, and `release` is what tells them apart. `bypassed` (`:197`) needs `NoAdmit`, which `admitConfine` never sets. |
| `repo` | client | Section 5.1.3, from the client's working directory at launch, computed once before admission. | Absent when the identity cannot be established (section 5.1.3). |
| `at` (existing) | daemon | `time.Now()` when the report is handled (`confine_report.go:101`). | Never. |
| start time | nobody stores it | Readers compute `at - wall_ms`. The error is one local socket write plus the handler's entry, sub-millisecond to a few ms. | Absent when `wall_ms` is absent (pre-323 rows, pool rows). |
| `key_masked`, `key_first4`, `key_cmd` | daemon (store writer) | Section 5.1.4, from the signature and `repo`. | NULL when `kind != confine` or `repo` is absent. |

Why `wait_ms` is client-measured and not the grant's `waited_ms`: the daemon measures
`waited_ms` from **its** waiter's enqueue (`admit.go`, `waiter.waitedMS =
elapsedMilliseconds(waiter.enqueued, ...)`). A client that reconnects mid-queue
(`errAdmitReconnect`, `admission_linux.go:45`) is enqueued afresh, so the figure restarts. A
second admission timer (`admissionStarted`, `admission_linux.go:295`) is also local to one
exchange. Measuring around `deps.admit` gives the wait the user felt. The trailer keeps printing
the grant's figure, unchanged. The two agree when there was no reconnect, and the build tests
that they agree (test T4).

`at` caveat for readers: `at` is RFC3339Nano text with trailing zeros trimmed, so sorting the
text is not chronological below one second (`...03.98Z` sorts after `...03.984Z`). This is
pre-existing (retention's `ORDER BY at DESC` has it too) and harmless at minute scale. A reader
that needs sub-second order parses the time.

#### 5.1.2 The wire: `confine-report` frame

New optional fields in `Request.Args` of the `confine-report` frame, sent by `ReportPeakSample`
(`admission_linux.go:803`):

| Field | JSON type | Rule |
|---|---|---|
| `wall_ms` | integer | `0 <= wall_ms <= 31,622,400,000` (366 days in ms) |
| `wait_ms` | integer | same bounds |
| `repo` | string | exactly `none`, or exactly 16 lowercase hex digits (`^[0-9a-f]{16}$`) |

Client (`internal/runner`):

- `ConfinePeakReport` (`confine_budget.go:14`) gains `WallMS *int64`, `WaitMS *int64` and
  `Repo string`. Nil or empty means not established.
- `ReportPeakSample` adds each field only when it is set. The `<= 0 -> omit` idiom used for
  `peak_rss` and `budget` must **not** be copied, because 0 is a real duration. Nil is the only
  "absent".
- The client never sends a value the daemon would refuse. A duration outside the bounds, such
  as a run longer than 366 days, is omitted (NULL), and the rest of the report, peak and OOM
  included, still goes out. Under refuse-never-repair, sending it would lose the whole sample.
- The confine launch site fills the three fields per section 5.1.1. Shim mode still sends no
  report (`confine_shim_linux.go:515`, unchanged).
- `aira confine-report` (the aitest pool face, `cmd/aira/main.go:1405-1438`) is unchanged and
  sends none of the new fields. Its rows are `kind = pytest-worker`, whose subject already
  leads with the pytest rootdir (`confine_peak_history.go:33-40`).

Daemon (`internal/daemon/confine_report.go`):

- `confineReportFields` (lines 17-20) gains `wall_ms`, `wait_ms` and `repo`. The count guard at
  line 23 keeps `len(args) <= len(confineReportFields)`. The refusal text at line 24 names the
  new optional fields.
- Integers are parsed with `exactAdmitInt64` (`admit.go:3431`), as `peak_rss` and `budget` are.
  Its accepted forms are reused unchanged: an integer, an integral float64, a `json.Number`, or
  a decimal string.
- **Refuse, never repair.** A present field that fails its rule makes the handler refuse the
  whole report with `E_DAEMON_PROTOCOL`, and nothing is written. This is how every existing
  field behaves (lines 60-93). The cost is that a client bug also loses that run's peak/OOM
  sample. That is accepted for three reasons. The only sender is the same-version binary. That
  sender never sends a value it could not establish (it omits the field instead). Test T1
  drives the real sender into the real handler. Accepting a frame with some fields dropped
  would add a "partly trusted frame" rule that exists nowhere else in this handler.
- An absent field is stored as NULL, never 0.

The store (`RecordConfinePeak`, `confine_peak_history.go:99`):

- `ResourcePeakObservation` gains `WallMS`, `WaitMS *int64` and `Repo string`.
- The writer validates them again with the same rules. Like the existing peak and budget checks
  (lines 103-131), the Go check is the guard that binds both table shapes.
- The writer derives the three keys, so every writer, tests included, gets the same keys.

#### 5.1.3 Repo identity

**Definition.** The first 16 lowercase hex digits of SHA-256 of the canonical (symlink-resolved,
absolute) path of the git **common** directory that contains the client's working directory.
This is the same derivation as the project ID (`app.gitIdentity`, `internal/app/project.go`:
`hashID(EvalSymlinks(common))`), truncated, so the stored value is a prefix of the ProjectID that
`aira init` would give that checkout. All linked worktrees of one repository share one common
directory, and so one identity. That matches the AIRA-321 decision ("not `--show-toplevel`,
which would re-fragment to one key per worktree"). A submodule has its own git directory and so
its own identity.

**Privacy.** The stored value is a truncated hash, never a path. 64 bits is ample for one
machine's repositories. It is not secret: anyone who can read the state DB can confirm a guessed
path, but that DB already stores full argv, absolute paths included, in `signature`.

**Derivation, client-side, pure Go, no git subprocess.** It is a new function
`runner.RepoIdentity(cwd string) (id string, ok bool)` in `internal/runner`. It cannot live in
`internal/app`, which imports `runner`. Its input is `os.Getwd()` at launch.

1. Walk from `cwd` up to `/`. At each directory, `os.Stat(dir + "/.git")`:
   - not found (`ENOENT`/`ENOTDIR`): go to the parent;
   - any other error: **unevaluated**;
   - a directory: `gitdir` is that path;
   - a regular file: read at most 4 KiB. It must be one line `gitdir: <path>` (a relative path
     resolves against `dir`). Anything else is **unevaluated**.
2. The common dir is `gitdir`, unless the file `gitdir/commondir` exists. Then it is that file's
   trimmed content, resolved against `gitdir` when relative. A read error other than not-found
   is **unevaluated**.
3. `filepath.EvalSymlinks(common)`. An error is **unevaluated**. This differs from
   `app.gitIdentity`, which falls back to the unresolved path. Here a fallback would silently
   give one repository two identities.
4. Reaching `/` with no `.git` gives the literal `none`, an established fact: the job ran outside
   any git repository.

`GIT_DIR`, `GIT_WORK_TREE`, `GIT_CEILING_DIRECTORIES` and bare repositories are ignored on
purpose. The identity is a property of the directory, the same for every run from it. A bare
repository as the working directory has no `.git` entry, so the walk continues upward. Cost: one
`stat` per ancestor plus at most two small file reads. That is sub-millisecond, against the 2-5
ms and retry loop of a `git rev-parse` subprocess (`gitValue`, `project.go:790`).
"Unevaluated" sends no `repo`, never fails or delays the launch, and gives the row NULL keys.
Detached launches start the child with `cmd.Dir` set to the launcher's working directory
(`detach_linux.go:324`). The builder confirms that the confine detach path computes the identity
from that same directory (not verified for the confine path).

AIRA-321 must reuse `runner.RepoIdentity`. One derivation serves both the key and the future
estimator subject.

#### 5.1.4 The command key

The daemon (inside `RecordConfinePeak`) computes, for `kind = confine` rows with a `repo`:

```
tokens  = signature split on NUL                     (runner.ResourceSignature joins on NUL)
u       = unwrap(tokens): strip leading `env` / `/usr/bin/env` and `NAME=value` tokens, and
          `time` / `/usr/bin/time` with only its known options (-o FILE, -f FMT, --output X,
          --format X, -p, -v, -a, --portability, --verbose, --append); an unknown option, or
          `time` with no command after it, leaves the argv as is; `aitest-parent`, `timeout`,
          `nice`, `flock`, `uv run`, `make` are never unwrapped; an argv that unwraps to
          nothing stays as is
a0      = basename of u[0]
rest    = mask(t) for t in u[1:]:   "<n>" if t is all hex (7+) or all digits;
                                     "<p>" if t contains "/" or ends in .py .rs .sh .txt .json .toml;
                                     "<n>" if t contains 3+ consecutive digits; else t
pos     = rest without tokens starting with "-"
P       = repo + "\x1f"                              (unit separator)
key_masked = P + a0 + "|" + join(rest[:14], " ")    argv[0] plus at most 14 arguments
key_first4 = P + a0 + "|" + join(rest[:3],  " ")    the first four tokens
key_cmd    = P + a0 + "|" + (pos[0] or "")          command and first positional
```

The authority is `keys()` in the replay script (its `N1`, `N2` and `N3`) together with the golden
table. A Go test and `--self-test` must both reproduce all 26 cases byte for byte. When the
prose and the script disagree, the script and the table win, and the prose is fixed. The
argv part is rev 2's function unchanged. Collisions in it are possible (`a0` containing `|`, a
token containing a space) and are accepted: a collision only merges two histories.

The function is `runner.CommandKeys(signature, repo string) (masked, first4, cmd string)`. It is
pure, has no I/O, and is called by the store writer.

Why the daemon computes the keys rather than the client: the daemon is the table's single writer,
so the key function's version is fixed by one binary, and the client sends only raw facts. A key
rule change then needs only a daemon restart, plus a decision on old rows.

**Rows without a repo get NULL keys, and nothing is backfilled.** Old rows have no recorded
working directory, so any backfill would key them argv-only and mix them with repo-split keys,
undoing the decision. Readers start from the rows 323 writes.

#### 5.1.5 Schema and retention

`confine_peak_history` gains six nullable columns:

```
wall_ms INTEGER, wait_ms INTEGER, repo TEXT,
key_masked TEXT, key_first4 TEXT, key_cmd TEXT
```

- In the `CREATE TABLE` DDL (`store.go:875-880`), with table checks
  `CHECK(wall_ms IS NULL OR wall_ms>=0)`, `CHECK(wait_ms IS NULL OR wait_ms>=0)` and
  `CHECK(repo IS NULL OR length(repo)>0)`, following the AIRA-180 pattern.
- Added to the `ensureConfinePeakHistorySubject` list (`store.go:1313-1321`) through
  `ensureColumnAdded`, as plain `ADD COLUMN <name> <type>`. That follows the AIRA-180 precedent,
  whose `budget` ALTER also carries no CHECK. The Go validation in `RecordConfinePeak` is the
  guard on both shapes.
- **No data migration.** Existing rows read NULL in every new column, which means unevaluated,
  never 0. No reader in 323 selects them, and `scanResourceBudgetRow` is unchanged.
- **No index in 323** (rev 3 change 5). A reader adds
  `CREATE INDEX IF NOT EXISTS ... ON confine_peak_history(kind, key_<level>, at)` in its own
  ticket, next to its window query and that query's latency test. A full-table `GROUP BY`
  already misses its 250 ms deadline on this box (AIRA-314), so a reader's query must be one
  indexed read.

**Retention is unchanged: 20 rows per exact `(kind, signature)`** (`confinePeakHistoryLimit`,
`confine_peak_history.go:13`, delete at lines 157-161). It is not per key and not per repo.
Readers must not confuse the two windows:

- A per-key window (`WHERE kind=? AND key_x=? ORDER BY at DESC LIMIT 20`) sees whichever rows
  survived their own signature's retention. A key that spans many signatures (temp paths) can
  have hundreds of retained rows. A key with one busy signature has at most 20.
- **Cross-repo eviction.** Two repos running the same argv (`make test`, `./runall.sh`) share one
  signature, and so one 20-row window. Repo A's runs can evict repo B's rows for that argv. Every
  surviving row is still correctly attributed, so B's history is thinner but never contaminated.
  This is accepted for 323, because changing retention to `(kind, signature, repo)` would widen
  the window the estimator aggregates (`ResourcePeakHistory` reads every row of the signature).
  That would be an admission behaviour change, and 323 is data collection only. AIRA-321, by
  putting the repo into the signature itself, makes retention per argv-and-repo as a side
  effect.

Exposure measurement, reproducible read-only:

```
select count(*) from confine_peak_history where kind='confine';
select count(*), sum(n) from (select signature, count(*) n from confine_peak_history
  where kind='confine' group by signature having n>=20);
```

#### 5.1.6 Protocol bump and version skew (each statement verified by reading the code)

**Bump.** `daemon.ProtocolVersion` (`protocol.go:180`) and `runner.DaemonProtocolVersion`
(`admission_linux.go:74`) both go up by one from master at branch time, in lockstep. Today that
is 15 to 16. AIRA-340's branch `aira-priority-launch` (head `18148d7f`) still has both at 15
(verified). If 340 lands first with a bump, 323 takes the next number. Add a `ProtocolVersion 16`
paragraph to the history comment in `protocol.go`. `TestRunnerDaemonProtocolVersionMatchesTheDaemon`
(`internal/runner/protocol_pin_linux_test.go:25`) pins the pair.

Why bump rather than adding optional fields without one: an old daemon's closed allowlist refuses
any unknown field (`confine_report.go:26-29`). Without a bump, a new client reporting to an old
daemon would lose every row in the window between client install and daemon restart. The only
visible sign would be the OOM advisory reading "unconfirmed". The AIRA-284 precedent
(`protocol.go:171-178`) bumps so that version skew is a loud `E_DAEMON_PROTOCOL`.

**What the bump does**, from reading the code:

- The daemon compares `proto` before dispatching any verb (`server.go:850-852`) and answers a
  mismatch with `protocolMismatchFrame`. So the matrix is set by the version check, not by the
  allowlist.
- **New daemon, stale (proto-15) client:**
  - The stale client's **new launches are refused**: its admit frame gets `E_DAEMON_PROTOCOL`,
    which `admitExchangeOnce` treats as a terminal refusal, "refusing to launch ungoverned"
    (`admission_linux.go:620-636`; an exclusive request takes the branch at 467-479). This
    corrects the backlog plan (rev 3 change 8).
  - Jobs **already running** at cutover keep running and stay booked. Their lease keeper
    re-declares through the frozen ARDR frame, which is sniffed before the version check
    (`server.go`, the S7 magic sniff just above line 830). Their teardown `confine-report`
    (proto 15) is refused, so **that run's row is lost**: peak, OOM bit and budget, the
    estimator's input. The client prints any OOM advisory as unconfirmed (`peakAcknowledged`
    false, `confine_linux.go:1440-1459`).
  - Every other verb from a stale binary is refused too: `confine --list/--kill/--front`,
    `aira top` and long-lived MCP servers, until they restart.
- **Old (proto-15) daemon, new client:** the same in reverse. Launches are refused until the
  daemon restarts. The window is the gap between client install and daemon restart. The cutover
  is the existing atomic "install, then `systemctl --user restart aira-daemon.service`", as for
  protos 6-15.
- **Same version, fields absent:** only these senders omit fields, and the result is NULL:
  - the `aira confine-report` CLI face (pool rows, none of the new fields);
  - a confine run with no daemon grant (no `wait_ms`);
  - a run whose repo identity was unevaluated (no `repo`, so NULL keys).

  A true old client never reaches the handler, so "old client gives NULL columns" is not a
  reachable case and needs no test beyond the version check's existing ones.

**Unverified:** how many stale clients and MCP servers are alive at the cutover. Peers are warned
to restart them (section 6).

#### 5.1.7 What is client-only, daemon, wire and store

| Part | Files | Ships as |
|---|---|---|
| client | `runner/confine_linux.go` (timers, repo at launch, report fields), `runner/confine_budget.go` (`ConfinePeakReport`), `runner/admission_linux.go` (`ReportPeakSample`, proto const), new `runner/repo_identity.go`, new `runner/command_key.go` (`CommandKeys`, called by the store) | client install |
| wire | proto bump on both sides, new allowlist fields | lockstep; atomic install and restart |
| daemon | `daemon/confine_report.go` (allowlist, validation), `daemon/protocol.go` | daemon restart |
| store | `store/store.go` (DDL, ALTER list), `store/confine_peak_history.go` (observation, writer, keys) | daemon restart; columns added at open |
| docs | the replay script and the golden table, already committed by this revision | none |

Nothing is client-only: the whole of 323 ships in one release with one atomic install and
restart.

### 5.2 Flat charge discount for pinned jobs (AIRA-324)

(Rev 2 design, unchanged. Citations are now by function, because `admit.go` line numbers moved
after AIRA-328.)

**Who is discounted.** Only a waiter that is scope-backed AND whose pin the operator gave with
`--memory-reserve` (cap-source `operator:--memory-reserve`). Not `--memory-max`, not `aira run`
admissions (their pin has no flag to attribute), not scope-less `confine-reserve` waiters
(they have no cap), not a `--delegate-ram` parent, not an exclusive job, not aitest workers.
The client sends one new boolean on the admit request, `discountable`, set only in that case;
the daemon never infers it from `pinned`. In ci-shim mode (no physical reading) nothing is
discounted. (`discountable` is an admit-frame field and brings its own protocol bump, after
323's.)

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
3+ clean samples)`, the levels being `key_masked`, then `key_first4`, then `key_cmd` (section
5.1.4; each needs its own index, added by 325). **"OOM on record"** means any row with `oom=1`
in that key's window of 20, at the key level being used; such a key is charged its flat charge.
At 0.8 it reaches 1.52x with a p99 of 0.92-0.93 (section 2, argv-only keys), a margin too thin
to enable on the replay alone. Ships only if the enforce data on the flat discount, replayed by
start time (`at - wall_ms`) on repo-keyed rows, shows headroom, and decided with a measured
exceedance target, not by default.

### 5.4 Fast-lane queue order (AIRA-326)

A waiter is **fast** if, at enqueue, its key has 3+ clean samples, no OOM in the window, p90
`wall_ms` under 5 minutes, p90 peak under 4 GiB, **and its own ledger charge is at most
4 GiB** (a quick job pinned at 32 GB would otherwise become a blocked head with a large
bypass budget). Unknown is never fast, and that includes a NULL key or NULL `wall_ms`. The class
and the bonus are **frozen on the waiter at enqueue**, from the history read the daemon already
makes before taking `queue.mu`; the evaluator never reads the database. Which of the three
stored key levels (or which chain) 326 reads is 326's plan decision, made on 323's repo-keyed
data. 326 adds that level's index.

To compute the key at enqueue, 326 needs the repo identity on the **admit** request (one more
admit-frame field, with its own protocol bump) and must compute the key from the same signature
the report carries. The admit frame's signature is truncated for display to
`runner.ConfineReservationSignatureWireLimit` (512 characters, `confine_manage.go:860`;
`boundedAdmitSignature`, `admit.go:1144`). 326 must establish which signature reaches the key
function, or the enqueue key and the stored key can differ for long argvs.

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
   - It ships client, daemon, wire and schema together (section 5.1.7). The cutover is one atomic
     client install plus `systemctl --user restart aira-daemon.service`, after `aira confine
     --list` and a heads-up to peers.
   - Peers are told before the restart that any stale `aira` binary, long-lived MCP server or
     open `aira top` must be restarted. Their launches and queries are refused until then
     (section 5.1.6).
   - Jobs straddling the cutover finish normally but lose their history row.
   - Smoke test after cutover: one small confined run in a git checkout, then

     ```
     sqlite3 -readonly ~/.local/state/aira/state.db \
       "select wall_ms,wait_ms,repo,key_cmd from confine_peak_history order by rowid desc limit 1"
     ```

     which must show non-NULL values. A run from `/` must show `repo = none`.
   - The branch follows Batch A, which edits the same report lines.
2. AIRA-324 in observe for a week of normal load; then enforce at 0.9 for a week, then 0.8.
   **Gate to start enforce:** a counterfactual replay of the real arrival/start/end timeline
   (start = `at - wall_ms`, arrival = start - `wait_ms`, from AIRA-323) that books jobs at the
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
  never 0. A present but invalid value refuses the whole report. Admission, the estimator and
  retention are unchanged by 323.
- (323) A key is written only with an established repo identity (`none` included). A row without
  one is never keyed argv-only.

## 8. Tests required (each must fail against the wrong implementation)

AIRA-323 (each names the mutation that must turn it red):

- **T1. Real client-to-daemon frame.**
  - Setup: a test in package `daemon` starts a real `Server` on a temporary socket with a
    temporary store. It calls the real `runner.ReportPeakSample` with a `ConfinePeakReport`
    carrying `WallMS`, `WaitMS` (including a real 0) and `Repo`, for an argv taken from the golden
    table. It then reads the row back with SQL.
  - Pass: every value and all three keys equal the golden case. No frame is hand-built on either
    side. `TestConfineReportCarriesTheBudgetPair` (`confine_budget_test.go:159`) calls the
    handler directly and is not a model for this test.
  - Mutations:
    - drop a field from the sender: NULL, red;
    - leave it out of the allowlist: refused, red;
    - copy the `> 0` idiom for `wait_ms`: a real 0 is lost, red.
- **T2. Absent means NULL.** A report without the new fields stores NULL in all six columns,
  asserted with `IS NULL`. Mutation: store 0 for absent, red.
- **T3. Validation table.**
  - Refused with `E_DAEMON_PROTOCOL` and no row written:
    - `wall_ms` of -1 or of 366 days + 1 ms;
    - `wait_ms` of -1;
    - 1.5 and `"abc"`;
    - `repo` of `""`, `"NONE"`, 15 or 17 hex digits, upper-case hex, or a path.
  - Accepted (boundaries): 0, exactly 366 days, `none`, 16 hex digits.
  - Mutations: remove each bound in turn, red.
- **T4. Launch site** (fake `confineDeps`, captured `reportPeak`):
  - `WaitMS` is set iff `deps.admit` returned a lease;
  - it is nil for `bypassed`, `disabled` and `no-daemon`;
  - `WallMS` covers an injected clock advance between admission return and report. Add a clock
    seam to `confineDeps` if none exists.
  - With no reconnect, `WaitMS` equals the grant's `waitedMS` within the fake clock's step.
  - A daemon `unevaluated` grant (with a lease) sends `WaitMS`; the local `no-daemon`
    `unevaluated` does not.
  - A duration over 366 days is omitted by the client, not sent; the report still goes out.
  - Mutations, each red:
    - send `admission.waitedMS` (the Go zero) for a lease-less admission;
    - key the rule on `state == "unevaluated"` instead of `release`;
    - send an out-of-range duration.
- **T5. `RepoIdentity` on real temporary repositories:**
  - plain repo and a subdirectory: the same ID;
  - linked worktree: equals the main checkout;
  - submodule: distinct;
  - outside any repository: `none`;
  - a symlinked working directory: equals the physical path;
  - an unreadable or malformed `.git` file: unevaluated;
  - equality with `app` ProjectID's first 16 hex digits for a plain repo and a worktree (this
    check sits in an `app` package test, because `runner` cannot import `app`).
  - Mutation: hash the worktree's own `gitdir` instead of its `commondir`, red.
- **T6. Key golden table.** A Go test loads `docs/dev/aira-history-charge-keys.golden.json` and
  asserts `CommandKeys` on all 26 cases. `python3 -I docs/dev/aira-history-charge-replay.py
  --self-test` checks the same file. Mutations (already run on the script, section 11): drop the
  repo prefix (21 mismatches), 13 arguments instead of 14 (1), drop `-p` from the `time` flags
  (1). Each is red.
- **T7. No key without established inputs.** `kind = pytest-worker` and absent `repo` give NULL
  keys. Mutation: key argv-only when `repo` is absent, red.
- **T8. Nothing else moves.**
  - 21 rows of one signature from two repos leave 20 retained;
  - `ResourcePeakHistory` and `ConfinePeakP90` return the same values with and without the new
    columns populated.
  - Mutation: retention per `(signature, repo)`, red.
- **T9. Schema upgrade.** A store created with the pre-323 DDL opens to the new shape; old rows
  are NULL in the new columns; DDL and ALTER column types match.
- **T10. Protocol pin.** `TestRunnerDaemonProtocolVersionMatchesTheDaemon` passes at the new
  number, and the existing mismatch test still refuses an old-proto frame.

AIRA-324-326 (unchanged from rev 2, except that test 7 moved to T6 and test 8 moved to the readers):

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
7. (Moved to T6.)
8. Each reader's window query (325, 326, 331): a 200k-row fixture, one indexed read on its level's
   `(kind, key_x, at)` index, under 250 ms.
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
- The discount adds a client-side flag and an admit wire field (protocol bump; AIRA has no
  compatibility obligation).
- Re-opens, narrowly, the live-number-in-admission trade of 2026-09-10: the physical-fit rule
  reads `effectiveCurrent`, which `checkedAvailable` already reads. No per-job read.
- (323) The protocol bump refuses stale clients' launches during and after the cutover until they
  restart (section 5.1.6). Mitigation: the atomic cutover and the peer warning. Jobs straddling
  the cutover lose their history row, a one-off loss.
- (323) Repo-scoped keys can only shrink coverage. If the shrink is large, 326 and 331 have less
  to work with; that is measured on 323's data before either is built.
- (323) **RANT-133 is not fixed by 323.** The estimator still keys on argv alone, so one
  `./runall.sh` OOM still escalates every repo's `./runall.sh` estimate. The reported 9 GiB
  (`estimate:oom-escalated`, 1.5 x a 6 GiB pin) is a report, not a measurement. The estimator
  fix is AIRA-321. 323 only makes the new key, used by 325, 326 and 331, repo-split.

## 10. Follow-ups and sequence notes

Follow-ups, deliberately not in this plan:

- Live per-job usage correction, only if the enforce data says the discount is not enough.
- Held-back headroom for the fast lane, only if its wait target is missed.
- aitest worker pools keep their own cost-ranked ordering (AIRA-261).
- (323 deferrals) Showing `wall_ms`/`wait_ms` in `confine --budget`, `confine --dump` or
  `aira top`; timing on the `aira confine-report` pool face; recording failed runs (AIRA-264
  decided against it); any backfill of keys for pre-323 rows.

Sequence notes (verified where marked):

- **Batch A, then 323.** Batch A edits `confine_linux.go` and `admission_linux.go`, the report
  path. The 323 build branches after it merges. Line numbers cited in 5.1 are master at
  `4d935a0c` and will move. The functions named stay.
- **AIRA-340** (`--priority high|normal`, in progress on `aira-priority-launch`, head `18148d7f`,
  proto still 15 there: verified). It adds an admit-frame field and a tier in
  `admitOrderLocked`. There is no file overlap with 323's frame (`confine-report`, not `admit`),
  but both may bump the protocol: whichever lands second takes the next number. 326 must compose
  its bonus with 340's tiers (section 5.4).
- **AIRA-341** (delete the machine-wide p90 prior; backlog plan B#3, owner "(a)"). **Not
  verified:** no ticket file exists in any ref. `aira get AIRA-341` answers
  `E_TICKET_NOT_IN_WORKTREE`, allocated at `/home/mark/tmp/aira-hk`. It interacts with 323 only
  through the shared table: 323 adds nothing to `ConfinePeakP90` (`confine_peak_history.go:201`),
  and deleting it removes the AIRA-314 full-table `GROUP BY` that section 5.1.5 warns readers
  against.
- **AIRA-228** (collapse the per-slice queue map): `admit.go` and `reload.go` only. There is no
  file overlap with 323. It lands before 324 and 326, which edit the same evaluator.
- **AIRA-225** (moving the peak report after teardown): the same lines as 323's report change
  (`confine_linux.go:1446-1462`). It is built after 323 and must keep the new fields and the
  `wall_ms` end point (the report's construction).
- **AIRA-321** (estimator subject = argv + repo): it reuses `runner.RepoIdentity`. When it lands,
  retention becomes per argv-and-repo, which ends section 5.1.5's cross-repo eviction. The
  keys' repo prefix becomes redundant but stays harmless.
- **AIRA-331** (cold-start cap from a coarse key): it reads one of 323's three levels, adds that
  level's index, and is measured on repo-keyed rows. The AIRA-329 diagnosis behind it used
  argv-only keys.
- **AIRA-236** (time tracking): 323 records the timing half, not the `--ticket/--phase/--label`
  attribution.

## 11. Change log

- **Rev 2 -> 3** (this revision, for AIRA-323's plan gate): see "Rev 3 changes" at the top.
  Verification run for it:
  - `python3 -I -m py_compile` on the script: exit 0.
  - `--self-test`: 26 cases, 0 mismatches, exit 0. The three mutations named in T6 each exit 1.
  - The full replay (`part1`-`part3`, which exercises the new `history_sql()` and the legacy
    keying) ran to completion on the live store under `aira confine` on 2026-11-14, exit 0. It
    read 51,102 pinned jobs since 2026-10-20, against rev 2's 50,006, because the store grew.
    History coverage was 0.72, and flat 0.8 x pin gave 1.25x at a mixed p99 of 0.78 and a burst
    p99 of 0.88. These are within noise of the section 2 table.
  - Legacy neutrality is by construction (rev 3 change 9), not by a measurement.
  - Retention exposure: the query in section 5.1.5, 2026-11-14.
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
