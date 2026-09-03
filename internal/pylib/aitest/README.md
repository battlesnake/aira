# aitest

A pytest plugin that replaces `pytest-xdist` for AIRA-governed suites: a
fork+admission worker pool with per-worker kernel-enforced cgroup memory
containment, in place of `pytest-xdist`'s execnet-spawned, ungoverned
workers.

Activate with `--aitest-workers=N` (a fixed worker count) or
`--aitest-workers=auto` (up to the host's CPU count). This is a NEW, explicit
flag rather than a reinterpretation of `-n` — a project with `pytest-xdist`
installed for unrelated reasons must not have its flag silently hijacked.

`aitest` is a from-scratch replacement for `aira_xdist_governor` (this
package's sibling under `internal/pylib/`), which is retired once `aitest`
reaches feature parity — see
[`docs/superpowers/specs/2026-09-01-aitest-design.md`](../../../docs/superpowers/specs/2026-09-01-aitest-design.md)
for the full design, staging, and the governor's retirement plan (§3.8).

## Reporting (Slice 2, AIRA-31)

Each worker streams its real pytest `TestReport` objects — plus
`pytest_runtest_logstart`/`logfinish` — back to the supervisor, which replays
them into its own real pytest hooks. Real JUnit XML, real per-test terminal
progress, real failure tracebacks and captured output all work, and are proven
against a plain, non-aitest run of the same suite by
`test_junit_fidelity.py`.

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
- **Wording divergence, by design.** For a nodeid that ends the run
  unevaluated, aitest's own plain summary line says `unevaluated` while the
  replayed pytest/JUnit summary counts it as a failure. `unevaluated` is not a
  pytest outcome, and `junitxml` silently ignores an outcome it does not
  recognise — a synthesized failure is the only shape that keeps the test
  visible in the report at all.
