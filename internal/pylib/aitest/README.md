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
65,164 queued tests in 20 classes): about 28 ms per pick before and 0.005 ms
after. Building the index takes about 80 ms once per queue.

After the per-test lines and the `aitest: N passed, ...` count, every run
prints one more line:

```text
aitest: pool: live workers peak 64, mean 60.0 over 2020.1 s; supervisor CPU 1500.2 s (74% of one core); workers idle waiting for dispatch 812.3 s, 12.5 ms per test (measured on 65000 of 65164 tests)
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
  result line. If a worker dies, the waits for its unfinished tests are lost.
  That is why the line says how many tests were measured. If no test was
  measured, the figure reads `unevaluated`, never 0.

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
- **Look-ahead dispatch.** `run_one` still passes `nextitem=None`, so
  session/module/class-scoped fixtures are torn down and rebuilt per test.
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
