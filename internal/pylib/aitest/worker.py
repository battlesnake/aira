import os


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
    """Child-side loop: read one nodeid per line from pipe_in, run it via
    run_one, write "<nodeid> <outcome>" back to pipe_out per completed test.
    An empty line means no work right now -- read again. The line
    "__stop__" ends the loop cleanly. scope_path is unused until Task 14's
    recycle checks; kept as a parameter now for API stability."""
    for line in pipe_in:
        nodeid = line.rstrip("\n")
        if nodeid == "":
            continue
        if nodeid == "__stop__":
            break
        item = items_by_nodeid[nodeid]
        outcome = run_one(item)
        pipe_out.write("%s %s\n" % (nodeid, outcome))
        pipe_out.flush()
