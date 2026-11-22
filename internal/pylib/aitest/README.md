# aitest

A pytest plugin that replaces `pytest-xdist` for AIRA-governed suites: a
fork+admission worker pool with per-worker kernel-enforced cgroup memory
containment, in place of `pytest-xdist`'s execnet-spawned, ungoverned
workers.

Activate with `--aitest-workers=N` (a fixed worker count) or
`--aitest-workers=auto` (up to the host's CPU count). This is a NEW, explicit
flag rather than a reinterpretation of `-n` — a project with `pytest-xdist`
installed for unrelated reasons must not have its flag silently hijacked.

`aitest` is a from-scratch replacement for `aira_xdist_governor`, which was this
package's sibling under `internal/pylib/` and which AIRA-33 has now deleted along
with the `governor-slot` relay and the daemon-side park/active-set scheduler.
`aitest` is therefore AIRA's only pytest plugin. See
[`docs/superpowers/specs/2026-09-01-aitest-design.md`](../../../docs/superpowers/specs/2026-09-01-aitest-design.md)
for the full design and staging (the retirement plan is §3.8).

## Dispatch cost and the pool summary line (AIRA-343, AIRA-344)

The supervisor hands every test out from one thread. Each hand-out picks the
test that fits the worker (its memory reservation and CPU cores) and ranks
highest by `aira_time` (longest first), then by memory need (largest first),
then by queue order. Since AIRA-343 that pick no longer scans the whole ready
queue. The queue keeps one heap per (memory need, CPU need) class, and a pick
looks only at the top of each class that fits, so its cost depends on the
number of classes, not on the number of queued tests. The order it picks in is
exactly the old order, which `test_ready_queue.py` checks against the old
full-scan picker over random queues and operation sequences. Measured with
`AIRA_AITEST_BENCH=1 pytest -s test_ready_queue.py -k bench` (Python 3.12.3,
65,164 queued tests in 20 classes, 200 successful picks timed for each picker):
27-32 ms per pick before and about 0.01 ms after. Building the index takes
80-110 ms once per queue.

After the per-test lines and the `aitest: N passed, ...` count, every run
prints one more line:

```text
aitest: pool: live workers peak 64, mean 60.00 over 2020.1 s; supervisor CPU 1500.2 s (74% of one core); workers idle waiting for dispatch 812.3 s, 12.5 ms per test (measured on 65164 of 65164 tests)
```

- **Live workers.** A worker counts as live from its ready signal to its
  retirement. The time it spends waiting for admission does not count. The
  mean is live-worker time divided by the run's wall time.
- **Supervisor CPU.** The supervisor process's own CPU time (`time.process_time()`)
  from the start of the run to the end of the dispatch loop. Collection, which
  happens before the run starts, is not included.
- **Idle waiting for dispatch.** Each worker measures this for itself, for every
  test it receives. The wait starts when the worker begins sending its previous
  result, or when it enters its loop if this is its first test, and ends when it
  has read the next test's id. So the figure includes the time a result sits
  unread while the supervisor is busy. A worker's wait before `__stop__` is not
  counted, because no test follows it. The worker sends the figure on the
  result line, so the denominator in "measured on N of M tests" is the number
  of completed results, and N is less than M only when a test double sends a
  result line without the figure (a test whose worker died before reporting is
  in neither count). If no test was measured, the figure reads `unevaluated`,
  never 0. One case counts time that is not a dispatch delay: a worker that went
  idle because the queue was empty, and later receives a test requeued after
  another worker crashed, counts its whole idle period as waiting for dispatch.

With `AIRA_AITEST_MEASURE_DIR` set, the same figures go into
`pool-report-<pid>.json` (`live_workers_peak`, `live_workers_mean`,
`run_wall_s`, `supervisor_cpu_s`, `dispatch_wait_s`, `dispatches_measured`,
`tests_completed`). Each worker span in `aitest-trace-<pid>.json` also carries
`dispatch_wait_s` and `dispatches_measured`. Per-dispatch timestamps are not
recorded. The per-worker totals give the trace the resolution of one worker's
lifetime.

## Reporting (Slice 2, AIRA-31)

Each worker streams its real pytest `TestReport` objects — plus
`pytest_runtest_logstart`/`logfinish` — back to the supervisor, which replays
them into its own real pytest hooks. Real JUnit XML, real per-test terminal
progress, real failure tracebacks and captured output all work, and are proven
against a plain, non-aitest run of the same suite by
`test_junit_fidelity.py`.

### Unevaluated results in the terminal summary (AIRA-161)

A test whose worker died — and whose one requeue died too — is recorded as
`unevaluated`, and rendered into the report as a `TestReport` with
`outcome="failed"` whose longrepr begins `unevaluated: `. That shape is
deliberate: `TestReport.outcome` is only ever meaningfully
passed/failed/skipped, and `junitxml` silently DROPS an unrecognised one, so a
synthesized failure is the only shape that keeps the lost test visible at all.

The consequence is that pytest's own `N failed` count mixes real test failures
with infrastructure-caused non-results. So whenever the count is nonzero,
aitest adds an `aitest unevaluated results` block to pytest's terminal summary,
next to that count:

```text
========================= aitest unevaluated results =========================
aitest: 12 of the 370 failures pytest counted are UNEVALUATED, not real test failures.
Unevaluated means aitest never established a result for that test: ...
```

It is silent when nothing was unevaluated, and absent under `-p no:terminal` /
`--no-summary`, where pytest never calls the hook (and its own failure count is
equally absent there). It reports the same number as the plain
`aitest: … N unevaluated` line — one count, printed in two places — and changes
nothing about detection, the requeue-once, or the synthesized report.

### Coverage

`aitest` **owns nothing** about coverage. It never originates a coverage
config, never calls `coverage.process_startup()`, and never calls
`combine()`. Whoever started coverage (`pytest-cov`, a bare `coverage run`, or
nothing at all) remains entirely responsible for combining, exactly as for any
other plugin's workers. Its only responsibility is that a forked worker about
to `os._exit()` — which is precisely the call that skips coverage's own atexit
save — stops and saves whatever `Coverage.current()` already is first.

That save is gated on the active instance's own `run:parallel` option. A
project using a bare `coverage run -m pytest` with **no** parallel-mode /
`data_suffix` config gets no correctness guarantee: parent and child would
choose the same data filename and the child's first-use `erase()` would destroy
what the parent already flushed. A project wanting correct coverage across
aitest workers owns its own parallel-mode-aware coverage config — as it already
would for xdist.

### Consecutive-chunk batching (AIRA-372, off by default)

`AIRA_AITEST_BATCH_MAX_TESTS=c` (an integer, 2 or more) makes the supervisor cut
the final collection order (after every `pytest_collection_modifyitems` hook)
into chunks of up to `c` consecutive tests and hand each chunk to one worker,
which runs member `i` with pytest's real `nextitem` (member `i+1`) and the last
member with `None`. pytest's own `SetupState` then keeps alive exactly what
serial pytest keeps alive for that range, so a module fixture is set up once
per chunk instead of once per test. Unset or `1` is off, and the queue and the
dispatch wire are byte-identical to an unbatched run; a non-integer or a value
below 1 warns once and means 1. The cap is clamped to
`AIRA_AITEST_WORKER_MAX_TESTS` (default 200). A batched run prints one line at
the start: `aitest: batching: N tests in K chunks, cap c (AIRA_AITEST_BATCH_MAX_TESTS)`.

- **Cuts.** Chunks never reorder tests. A chunk holds tests with the same
  `aira_mem` / `aira_cpu` / `aira_time` values; a module's run of such tests is
  not split below the cap; an `@aira_failfast` test always runs alone.
- **Recycling.** Before each member of a chunk the worker checks its time,
  count and memory-watermark limits. A trip never skips the member: it runs
  last, with `nextitem=None`, so every teardown happens inside a real test
  protocol. A worker runs at most `AIRA_AITEST_WORKER_MAX_TESTS` tests. The
  unstarted rest of a recycled chunk goes back to the queue as one chunk.
- **Crashes.** Only the member running when a worker is lost is charged a
  retry; the unstarted rest is requeued as single tests, uncharged. A loss on
  item `k > 1` prints `aira aitest: worker <pid> was lost while running item k
  of a chunk (<nodeid>); k-1 earlier items had run with shared fixtures alive;
  retrying it alone`, because a retried test usually passes alone and a memory
  build-up across the chunk would otherwise stay invisible.
- **Teardown errors** of a shared fixture are reported on the test whose
  teardown ran them: the last user before a scope change, or the chunk's last
  member -- serial pytest's attribution for the same order.
- **`-x`, `--maxfail`.** Inside a worker, its own failures can set pytest's
  `shouldfail`/`shouldstop`, after which pytest forces `nextitem=None`: the rest
  of that worker's life runs unbatched. Sound, only slower.
- **Memory.** A worker keeps a chunk's higher-scope fixtures alive until a
  scope change or the chunk's end, so its peak can rise by one chunk's retained
  fixtures (what serial pytest holds for that range). On ci-shim ledger-only
  admission there is no watermark and no kill backstop for that growth.
- **Measurement** (`AIRA_AITEST_MEASURE_DIR`): `fixture-setup-<pid>.tsv` gets one
  row per real fixture setup (scope, fixture name, scope node, seconds); each
  `worker-<pid>.tsv` row gains a fourth column, the worker's private memory in
  bytes (`/proc/self/smaps_rollup` Private_Clean + Private_Dirty, or
  `unevaluated`); `pool-report-<pid>.json` adds `batch_cap`,
  `chunk_lengths_cut`, `chunk_lengths_run`, `drain_s`, `worker_crashes`,
  `mid_chunk_crashes`, `oom_group_kills` and `unstarted_requeued`.

Design and the A/B plan before the default changes:
[`docs/superpowers/specs/2026-11-22-aitest-fixture-batching-design.md`](../../../docs/superpowers/specs/2026-11-22-aitest-fixture-batching-design.md).

### Known gaps, deliberately deferred

- **`-x` / `--maxfail`.** A replayed report does set `session.shouldfail`
  (pytest's own hook implementation does that), but nothing in
  `Supervisor.run`'s dispatch loop checks it, so `--maxfail` is silently
  ignored under `--aitest-workers`.
- **`pytest_warning_recorded` is not forwarded** from worker to supervisor
  (xdist does forward it). Combined with the child's own terminalreporter
  being muted, a test-time warning is absent from the parent's warnings
  summary.
- **`log_cli` live logging.** The forked child's `LoggingPlugin` writes through
  the same (now muted) terminalreporter, so a worker's live log lines go
  nowhere rather than to the shared terminal. Forwarding them properly is not
  done here.
- **Shared fixtures are rebuilt per test unless batching is on.** With
  `AIRA_AITEST_BATCH_MAX_TESTS` unset or `1` (the default) every test is
  dispatched alone and `run_one` passes `nextitem=None`, so
  session/module/class-scoped fixtures are torn down and rebuilt per test. See
  "Consecutive-chunk batching" below.
- **`record_xml_attribute`/`record_testsuite_property`** write into the
  child's own COW-forked `LogXML` plugin instance and are lost — same
  limitation as xdist workers, not a regression against the design target.
- **`--setup-show`/`--setup-only` output goes nowhere** in a worker: muting
  (rather than unregistering) the terminalreporter keeps assertion-repr
  rendering correct, but these two flags' own output still depends on a live
  terminal writer the child doesn't have wired up for this purpose — silently
  absent rather than crashing (unregistering would have crashed instead).
- **Wording divergence, by design.** For a nodeid that ends the run
  unevaluated, aitest's own plain summary line says `unevaluated` while the
  replayed pytest/JUnit summary counts it as a failure. `unevaluated` is not a
  pytest outcome, and `junitxml` silently ignores an outcome it does not
  recognise — a synthesized failure is the only shape that keeps the test
  visible in the report at all. AIRA-161 does not remove that divergence (the
  report shape stays exactly as it is); it makes it legible in the aggregate,
  via the terminal-summary block described above.
