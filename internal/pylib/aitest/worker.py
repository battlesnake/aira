import os
import time


_DEFAULT_MAX_SECONDS = 600
_DEFAULT_MAX_TESTS = 200
_DEFAULT_HIGH_WATERMARK_PCT = 80

# Appended to a result line ("<nodeid> <outcome>") when this is the LAST
# test this worker will run before retiring -- never sent as a separate
# message. supervisor.py's _drain_worker strips this suffix to learn the
# same fact atomically with the result, closing the race described in
# run_worker_loop's recycle branch above.
_RECYCLE_SUFFIX = " __recycle_next__"


def _read_cgroup_int(scope_path, filename):
    with open(os.path.join(scope_path, filename), encoding="ascii") as handle:
        raw = handle.read().strip()
    if raw == "max":
        return None
    return int(raw)


def _should_recycle(scope_path, started_at, completed_count):
    max_seconds = int(os.environ.get("AIRA_AITEST_WORKER_MAX_SECONDS", str(_DEFAULT_MAX_SECONDS)))
    if time.monotonic() - started_at > max_seconds:
        return True
    max_tests = int(os.environ.get("AIRA_AITEST_WORKER_MAX_TESTS", str(_DEFAULT_MAX_TESTS)))
    # >= , not >: with AIRA_AITEST_WORKER_MAX_TESTS=1 and exactly one
    # completed test, 1 > 1 is false and recycle would never fire at all.
    if completed_count >= max_tests:
        return True
    if scope_path is None:
        # Daemon-down fallback mode (Task 16): no granted cgroup scope to
        # watermark-check; time/count bounds above still apply.
        return False
    watermark_pct = float(os.environ.get("AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT", str(_DEFAULT_HIGH_WATERMARK_PCT)))
    try:
        current = _read_cgroup_int(scope_path, "memory.current")
        high = _read_cgroup_int(scope_path, "memory.high")
    except (OSError, ValueError):
        return False
    if current is None or high is None or high <= 0:
        return False
    return (current * 100.0 / high) > watermark_pct


def fork_worker(scope_path):
    """Forks. In the child, places itself into scope_path's cgroup before
    returning. Returns (pid, in_child: bool).

    DELIBERATE DEVIATION from confine's own placement, worth naming
    explicitly rather than letting it slide: aira confine places a NEW
    process atomically via clone3(CLONE_INTO_CGROUP) (Go's
    SysProcAttr{UseCgroupFD}, internal/runner/confine_linux.go) -- a
    successful Start() there IS proof of placement, no gap at all. A worker
    here is forked from an ALREADY-RUNNING Python process instead (the whole
    point being COW-shared warm-imported interpreter state, spec 3.1) --
    Python's stdlib os.fork() is a plain fork(2), with no CLONE_INTO_CGROUP
    binding available (that would need a raw ctypes clone3 syscall, which is
    real added complexity and risk for what this buys). So there IS a brief
    window, between fork() returning in the child and place_self()
    completing, where the child is still a member of the SUPERVISOR's scope,
    not its own worker scope. Two things bound the actual risk to
    negligible: (1) this window is pure interpreter overhead (a syscall
    return, an open(), a write()) -- it ends before any test code runs, so
    no test-driven allocation can happen inside it; (2) cgroup memory.max is
    hierarchical, so the child's usage during that window still counts
    against the OUTER scope's cap, not an unbounded cgroup. Accepted for
    Slice 1 as an architecturally-simpler choice than a raw-syscall
    workaround for a race this narrow (architectural-simplicity: no new
    machinery for a bounded, sub-millisecond gap) -- but call it out plainly
    in plan-review rather than have it read as an oversight.

    Safety note: any exception place_self() raises happens in the CHILD --
    it must never propagate through normal Python control flow from here,
    since that would unwind into the child's COW-duplicated copy of the
    supervisor's own interpreter frames and could run arbitrary supervisor
    cleanup code fully UNCONFINED (a placement failure specifically means
    containment was never established at all). os.fork() itself CAN also
    raise, but only in the PARENT (no child exists yet in that case) --
    that failure is deliberately left to propagate normally here."""
    pid = os.fork()
    if pid == 0:
        try:
            place_self(scope_path)
        except BaseException:
            os._exit(70)
        return 0, True
    return pid, False


def place_self(scope_path):
    """Writes this process's own pid into scope_path/cgroup.procs. Must run
    before any test code executes in the forked child -- see fork_worker's
    docstring for why this is a plain write rather than an atomic
    clone-into-cgroup."""
    with open(os.path.join(scope_path, "cgroup.procs"), "w") as handle:
        handle.write(str(os.getpid()))


class _OutcomeCollector:
    """Captures the worst-of outcome across setup/call/teardown reports for
    one pytest_runtest_protocol call. Registered on item.config.pluginmanager
    only for the duration of that one call -- see run_one."""

    _RANK = {"passed": 0, "skipped": 1, "failed": 2}

    def __init__(self):
        self.worst = "passed"

    def pytest_runtest_logreport(self, report):
        outcome = report.outcome if report.outcome in self._RANK else "failed"
        if self._RANK[outcome] > self._RANK.get(self.worst, 0):
            self.worst = outcome


def run_one(item):
    """Executes one already-collected pytest Item through pytest's own item
    protocol (setup/call/teardown), returning "passed", "failed", "skipped",
    or "error".

    UNCERTAIN, flagged for verification during implementation: calling
    item.ihook.pytest_runtest_protocol(item=item, nextitem=None) directly,
    outside pytest's own Session.main() loop, is not a path this plugin has
    exercised against a real pytest version yet. It is pytest's own
    documented per-item hook (the same one xdist's worker calls per design
    spec 3.2) and SHOULD behave identically to normal collection -- but the
    exact hookimpl/pluginmanager registration dance below needs a real-pytest
    verification pass before it is trusted, not a guess presented as
    certain.

    ACCEPTED SLICE 1 LIMITATION: nextitem=None is pytest's own signal that
    this is the LAST item in the session, so it tears down and rebuilds the
    ENTIRE fixture stack -- including session/module/class-scoped fixtures
    -- after every single test, unlike plain pytest or xdist (which look
    ahead to supply the real next item so a fixture shared across tests
    persists). A suite relying on expensive or stateful session-scoped
    fixtures will see them re-run per test in Slice 1. Real look-ahead
    dispatch is deferred, a candidate for a later slice (see this task's own
    Interfaces note and the test proving this behavior below).
    """
    collector = _OutcomeCollector()
    plugin_manager = item.config.pluginmanager
    plugin_manager.register(collector, name="aitest-outcome-collector")
    try:
        item.ihook.pytest_runtest_protocol(item=item, nextitem=None)
    finally:
        plugin_manager.unregister(collector)
    if collector.worst == "passed":
        return "passed"
    if collector.worst == "skipped":
        return "skipped"
    if collector.worst == "failed":
        return "failed"
    return "error"


def run_worker_loop(scope_path, items_by_nodeid, pipe_in, pipe_out):
    started_at = time.monotonic()
    completed_count = 0
    for line in pipe_in:
        nodeid = line.rstrip("\n")
        if nodeid == "":
            continue
        if nodeid == "__stop__":
            break
        item = items_by_nodeid[nodeid]
        outcome = run_one(item)
        completed_count += 1
        recycling = _should_recycle(scope_path, started_at, completed_count)
        # The recycle decision rides in the SAME line as the result, not a
        # separate write -- two independent write()+flush() calls left a
        # real window (not just a buffering artifact; a genuine scheduling
        # gap between "send result" and "check+send recycle") where the
        # supervisor could see this worker as idle (in_flight cleared) and
        # dispatch it a fresh nodeid before the recycle line ever arrived,
        # silently losing that dispatch with no crash/EOF to detect it by
        # (worker.py's own recycle check runs strictly after the result is
        # already sent). One atomic message removes the window entirely.
        line = "%s %s" % (nodeid, outcome)
        if recycling:
            line += _RECYCLE_SUFFIX
        pipe_out.write(line + "\n")
        pipe_out.flush()
        if recycling:
            return
