import json
import os
import sys
import time


_DEFAULT_MAX_SECONDS = 600
_DEFAULT_MAX_TESTS = 200
# AIRA-35: the proactive-recycle watermark is a fraction of the worker's
# memory.max, not of a memory.high that no longer exists.
#
# 64, not 80, and that reproduces the old trigger point TO WITHIN A PAGE rather
# than being a retune. The old check was `memory.current / memory.high > 80%`
# where the daemon set `memory.high = memory.max * 4/5`, so it fired at
# 0.8 * 0.8 = 64% of memory.max. Reading memory.max with a 64% default lands on
# the same point -- but not bit-for-bit: writeScopeMemoryCap verified the
# PAGE-FLOORED memory.high, so at the 512 MiB default the old threshold was
# 0.80 * floor_page(536870912 * 4/5) and the new one is 0.64 * 536870912, a
# difference of about 2 KiB in ~344 MB. "Exactly" would be an overclaim; the
# point is that this is not a deliberate retune. The exact fraction is a genuinely open question (aitest design
# spec section 6) that needs field data, and AIRA-32 owns it; changing it here
# would have folded an unmeasured behavioural change into a convergence fix.
_DEFAULT_MEMORY_WATERMARK_PCT = 64

# _should_recycle runs after every completed test. Keep a malformed
# environment setting from both killing the worker and repeating the same
# diagnostic for every subsequent test in that worker.
_WARNED_INVALID_ENV_VARS = set()

# Appended to a result line ("<nodeid> <outcome>") when this is the LAST
# test this worker will run before retiring -- never sent as a separate
# message. supervisor.py's _drain_worker strips this suffix to learn the
# same fact atomically with the result, closing the race described in
# run_worker_loop's recycle branch above.
_RECYCLE_SUFFIX = " __recycle_next__"

# Every JSON event line a worker writes carries this explicit sentinel, and
# supervisor.py's _drain_worker matches on it EXPLICITLY -- never on a bare
# line.startswith("{") JSON-shape guess (Sol round-2: a pytest nodeid can in
# principle legally begin with "{", so a shape guess is ambiguous against the
# plain result line whose wire format Slice 2 must not change). \x01 is safe
# here for a positive, verified reason rather than by assumption: pytest runs
# every parametrized id through compat.ascii_escaped(), which translates
# non-printable ASCII into a literal backslash-x-hex escape, so no real nodeid
# can carry a raw \x01 -- and the remaining nodeid prefix is a filesystem path.
# A (pathological) nodeid that DID start with \x01 would make its plain result
# line take the malformed-event crash path: a fail-closed requeue/unevaluated,
# never a silently misattributed result.
_EVENT_LINE_PREFIX = "\x01"

# Tuple-tagging codec markers. json.dumps/json.loads silently degrades every
# tuple to a list, and pytest_report_from_serializable does NOT restore them
# (that restoration only exists on execnet's own typed wire format, which xdist
# uses) -- while junitxml's append_skipped does `assert isinstance(
# report.longrepr, tuple)` (verified against the real installed
# _pytest/junitxml.py). An untagged round trip therefore CRASHES the whole run
# on the first real skip.
_TUPLE_MARKER = "__aitest_tuple__"
_ESCAPED_MARKER = "__aitest_escaped__"


def _tag_tuples(obj):
    if isinstance(obj, tuple):
        return {_TUPLE_MARKER: [_tag_tuples(x) for x in obj]}
    if isinstance(obj, list):
        return [_tag_tuples(x) for x in obj]
    if isinstance(obj, dict):
        tagged = {k: _tag_tuples(v) for k, v in obj.items()}
        if _TUPLE_MARKER in obj or _ESCAPED_MARKER in obj:
            # A REAL dict that happens to already look like one of our
            # markers must be escaped, or decoding it would silently turn
            # it into a tuple (or unwrap a fake "escape") instead of the
            # real dict it actually is. Wrapping is recursive-safe: an
            # already-escaped dict containing another marker-shaped dict
            # gets escaped again, one layer per occurrence, and _untag_tuples
            # peels exactly one layer per marker it encounters.
            return {_ESCAPED_MARKER: tagged}
        return tagged
    return obj


def _untag_tuples(obj):
    if isinstance(obj, dict):
        if _ESCAPED_MARKER in obj and len(obj) == 1:
            return {k: _untag_tuples(v) for k, v in obj[_ESCAPED_MARKER].items()}
        if _TUPLE_MARKER in obj and len(obj) == 1:
            return tuple(_untag_tuples(x) for x in obj[_TUPLE_MARKER])
        return {k: _untag_tuples(v) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_untag_tuples(x) for x in obj]
    return obj


def _exit_child(code):
    """The ONE exit path for a forked worker child -- replaces every bare
    os._exit() in the child branches of worker.py, spawn_worker and
    _spawn_fallback_worker.

    Coverage responsibility, deliberately minimal (v3's "own nothing"
    design): aitest makes NO assumption about how, or whether, coverage was
    started. It never originates a coverage config, never calls
    coverage.process_startup(), and NEVER calls combine() -- whoever started
    coverage (pytest-cov, a bare `coverage run`, or nothing at all) owns all
    of that, unchanged, exactly as it would for any other plugin's workers.
    The single narrow thing aitest does owe is this: os._exit() is the one
    call guaranteed to skip coverage's own normal atexit save, so if
    something IS measuring in this process, stop and save it first.
    coverage.py's own sqldata "Looks like we forked!" pid check then gives
    the child its own data file, so this mutates only the forked copy.

    LIMITATION, documented rather than silently assumed: a project using a
    bare `coverage run -m pytest` with NO parallel-mode / data_suffix config
    gets no correctness guarantee from this mechanism, because the child and
    the parent would choose the SAME data filename and the child's own
    first-use erase() would destroy what the parent already flushed (verified
    live: a measurable coverage regression, not merely wasted work). That is
    exactly why the save below is gated on the active instance's own
    run:parallel option. A project wanting correct coverage across aitest
    workers owns its OWN parallel-mode-aware coverage config -- precisely as
    it already would for xdist."""
    try:
        import coverage
        current = coverage.Coverage.current()
        if current is not None:
            current.stop()
            if current.get_option("run:parallel"):
                current.save()
    except BaseException:
        # Best-effort only -- this must never prevent the process from
        # exiting. BaseException (v5, Fable's finding), not Exception: this
        # is called from except BaseException handlers whose entire point
        # is never falling through into the child's COW copy of the
        # supervisor's own control flow -- a KeyboardInterrupt/SystemExit/
        # MemoryError raised inside coverage's own save() must not escape
        # this function and defeat that.
        pass
    finally:
        os._exit(code)


def _init_forked_child(config):
    """Per-process re-initialisation every forked worker child must perform
    before it runs ANY test. Called once at the top of run_worker_loop --
    the single convergence point for BOTH fork sites (spawn_worker's
    confined path and _spawn_fallback_worker's bare os.fork(), which never
    calls fork_worker/place_self at all), so neither can be missed.

    1. MUTE the COW-inherited terminalreporter. Otherwise the child prints its
       own per-test progress line straight to the shared terminal and,
       combined with the supervisor's own replay-driven terminalreporter
       output, every test's progress prints TWICE.

       Mute, NOT unregister -- a deliberate correction to this slice's plan,
       found by its own end-to-end test rather than by review.
       _pytest.assertion.util.assertrepr_compare reaches for
       config.get_terminal_writer()._highlight on EVERY rich assertion
       comparison, and Config.get_terminal_writer() does
       `assert terminalreporter is not None` against the plugin registered
       under exactly that name (verified in the real installed
       _pytest/config/__init__.py and _pytest/assertion/util.py).
       pm.unregister(terminalreporter) therefore turned every `assert a == b`
       failure inside a worker into a bare "AssertionError" plus an internal
       meta-traceback about get_terminal_writer, silently destroying the real
       diagnostic -- exactly the wrong-data failure class this whole slice
       exists to prevent, and invisible to any test that does not compare a
       real failure's rendered traceback against a plain pytest run's.

       Swapping only the underlying FILE, rather than the whole TerminalWriter,
       keeps hasmarkup/code_highlight/terminal width precisely as the parent
       computed them, so the highlighter behaves identically to a plain,
       non-aitest run. Everything the child's reporter would emit -- per-test
       progress, and the OSC 9;4 progress sequences the separate progress
       plugin writes through this SAME writer -- goes to /dev/null instead of
       the shared terminal. It also means a worker's log_cli live-log lines
       (which route through the same reporter) stop reaching the shared
       terminal; forwarding them properly is still deferred.

    2. Replace the COW-inherited global capture object. pytest's FDCaptureBase
       creates ONE TemporaryFile per stream in the PARENT at session start and
       dup2()s it onto real fd 1/2; os.fork() shares the underlying open file
       DESCRIPTION -- including its current offset -- so every worker's
       captured output writes into the SAME file at whatever position sibling
       workers left it, and FDCapture.snap()'s seek(0)/read()/truncate() then
       hands one test its siblings' output. Live-verified with 4 concurrent
       workers each printing 20 tagged lines: one test's captured output held
       73 lines, only 18 of them its own; another captured none of its own.
       That is silently-WRONG <system-out>/<system-err> content attributed to
       the wrong test -- exactly the failure class this whole feature exists
       to prevent, and invisible in Slice 1 only because Slice 1 discarded
       captured output entirely.

    Do NOT "simplify" step 2 into capman.stop_global_capturing(): that public
    method calls pop_outerr_to_orig() FIRST, which snaps the still-shared
    capture file and writes it to the original stream -- stealing whatever
    sibling workers have written into it mid-test (a real risk: _replace_worker
    forks a NEW worker while other workers are mid-test). The sequence below
    instead DISCARDS the inherited (shared, corrupt-by-construction) object
    without reading it, then lets start_global_capturing() create genuinely
    fresh, per-process capture files.

    Run this unconditionally, every time -- do NOT gate it on the capture
    method being "fd". The sys/tee-sys/no modes are unaffected by the
    underlying bug (in-memory, COW-private buffers rather than a shared
    fd-backed file), but this sequence is correct for them too via their own
    stop_capturing()/start_global_capturing() machinery, and a mode-conditional
    branch would add risk for no benefit."""
    pm = config.pluginmanager
    terminalreporter = pm.get_plugin("terminalreporter")
    writer = getattr(terminalreporter, "_tw", None)
    if writer is not None:
        writer._file = open(os.devnull, "w", encoding="utf-8")
    capman = pm.get_plugin("capturemanager")
    if capman is not None:
        old = capman._global_capturing
        if old is not None:
            capman._global_capturing = None
            old.stop_capturing()
        capman.start_global_capturing()
        capman.suspend_global_capture()


def _read_cgroup_int(scope_path, filename):
    with open(os.path.join(scope_path, filename), encoding="ascii") as handle:
        raw = handle.read().strip()
    if raw == "max":
        return None
    return int(raw)


def _warn_invalid_env(name, raw, default):
    if name in _WARNED_INVALID_ENV_VARS:
        return
    _WARNED_INVALID_ENV_VARS.add(name)
    sys.stderr.write(
        "aira aitest: %s has invalid value %r; using default %r\n"
        % (name, raw, default)
    )


def _env_int(name, default):
    raw = os.environ.get(name, str(default))
    try:
        return int(raw)
    except ValueError:
        _warn_invalid_env(name, raw, default)
        return default


def _env_float(name, default):
    raw = os.environ.get(name, str(default))
    try:
        return float(raw)
    except ValueError:
        _warn_invalid_env(name, raw, default)
        return default


def _should_recycle(scope_path, started_at, completed_count):
    max_seconds = _env_int("AIRA_AITEST_WORKER_MAX_SECONDS", _DEFAULT_MAX_SECONDS)
    if time.monotonic() - started_at > max_seconds:
        return True
    max_tests = _env_int("AIRA_AITEST_WORKER_MAX_TESTS", _DEFAULT_MAX_TESTS)
    # >= , not >: with AIRA_AITEST_WORKER_MAX_TESTS=1 and exactly one
    # completed test, 1 > 1 is false and recycle would never fire at all.
    if completed_count >= max_tests:
        return True
    if scope_path is None:
        # No granted cgroup scope to watermark-check; the time/count bounds
        # above still apply, exactly as they do on every other path.
        #
        # TWO modes reach this, and the difference is worth stating because it
        # is the whole of AIRA-123: the daemon-down fallback (Task 16), which is
        # ungoverned, and ci-shim ledger-only admission, where the daemon DID
        # admit this worker against the container's RAM budget and only the
        # cgroup is missing. Neither can read memory.current, so neither gets
        # watermark recycling -- and in ledger-only mode that is the concrete
        # cost of having no cgroup, not an oversight: a worker growing past its
        # declared reservation is neither recycled here nor killed by a
        # backstop, and the container's own OOM killer is what is left.
        return False
    watermark_pct = _env_float(
        "AIRA_AITEST_WORKER_MEMORY_WATERMARK_PCT", _DEFAULT_MEMORY_WATERMARK_PCT
    )
    try:
        current = _read_cgroup_int(scope_path, "memory.current")
        limit = _read_cgroup_int(scope_path, "memory.max")
    except (OSError, ValueError):
        return False
    # Fail OPEN, unchanged from the memory.high era: an unreadable scope, a
    # "max" (unbounded) limit, or a non-positive one means this check could not
    # establish its result, and a recycle decision that cannot be established
    # must not force a recycle.
    if current is None or limit is None or limit <= 0:
        return False
    return (current * 100.0 / limit) > watermark_pct


def fork_worker(scope_path):
    """Forks. In the child, places itself into scope_path's cgroup before
    returning, UNLESS scope_path is None. Returns (pid, in_child: bool).

    scope_path is None in the two modes that have no cgroup to place into,
    and they are NOT the same thing: the daemon-down fallback (no admission
    at all, fully ungoverned) and AIRA-123's ci-shim ledger-only admission
    (the daemon really did admit this worker against the container's RAM
    budget; only the kernel-enforced sub-scope is missing). This function
    cannot tell them apart and does not need to -- what it needs is that
    "no scope" is a representable, first-class input here rather than a
    None crashing its way into place_self and being reported as a placement
    FAILURE, which is what it did before AIRA-123 and which would strip
    containment for the whole run.

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
    not its own worker scope.

    RETRACTED (AIRA-37), not softened: an earlier version of this docstring
    called that window "pure interpreter overhead (a syscall return, an
    open(), a write())", i.e. claimed no arbitrary code can run in it. That
    is false. CPython's os.fork() calls PyOS_AfterFork_Child() -- which runs
    every handler registered via os.register_at_fork(after_in_child=...) --
    BEFORE fork() returns to Python, so those handlers execute inside this
    window, in the child, while it is still unplaced. Verified directly on
    this host (CPython 3.12.3): a handler registered before the fork
    observes itself running ahead of os.fork()'s return. That ordering is
    pinned, by name rather than by line number, in test_worker.py's
    test_atfork_after_in_child_handlers_run_before_fork_returns
    -- so this docstring cannot quietly rot back to the old claim.

    Who actually registers such a handler (the audit AIRA-37 asked for --
    answered with real registrants, not a "shouldn't happen" assurance):
      - aitest itself: NONE. Since AIRA-33 deleted the sibling
        aira_xdist_governor plugin, AIRA's Python contains ZERO real
        os.register_at_fork CALLS. (A grep finds the ordering test named
        above: that registration lives in a source string the test runs as a
        throwaway SUBPROCESS, on purpose, so it never arms in aitest's own
        interpreter.)
        Until AIRA-33 that one site was aira_xdist_governor/__init__.py, at
        module scope, and it was not hypothetical: forked aitest workers
        permanently disabling that governor via its after_in_child handler
        was established behaviour (AIRA-92). That co-registration cannot
        happen any more, so AIRA no longer contributes a handler to this
        window itself. The window is still real, because of the stdlib and
        third-party registrants below.
      - The stdlib: logging, threading and random all register
        after_in_child handlers at import, and pytest imports all three
        (checked against pytest 9.0.3); asyncio.events and
        concurrent.futures.thread add more when a suite imports them. A
        user's own conftest or plugins may register further handlers that
        neither aitest nor AIRA controls.

    What still bounds the risk, restated accurately rather than dropped:
    (1) it is still before any TEST code runs, so no test-driven allocation
    happens inside it; (2) cgroup v2 memory.max is hierarchical, so whatever
    a handler does allocate is still charged to the OUTER confine scope's
    cap -- what is briefly coarser is the GRANULARITY of containment, not
    containment itself. Say OUTER precisely, not "the scope it is in": the
    unplaced child sits in `.aira-supervisor`, which is DELIBERATELY given
    no memory.max of its own (worker_admit.go's workerScopeChildPrefix and
    readWorkerSupervisorMemory notes), so the bound comes entirely from the
    outer scope one level up -- and that one is guaranteed finite by
    precondition, not by assumption: worker-admit refuses the grant outright
    (WorkerAdmitReasonOuterScopeUnbounded, "outer scope ... has no finite
    memory.max") when it is not, and fork_worker is only ever reached on the
    granted path. The sharper hazard is therefore not
    a containment break at all (the reviewer-synthesis nuance AIRA-37
    records) but fork-across-threads locking: an after_in_child handler that
    blocks on a lock some other thread held at fork time deadlocks the
    child. The supervisor is single-threaded by design (supervisor.py's
    AIRA-92 note), which bounds that from AIRA's side without eliminating
    it, since plugins sharing the interpreter may start threads of their own.
    The deleted governor's own handler was exactly this class (it closed
    buffered streams, taking buffered-IO locks); it is named here as the
    worked example of the hazard, not as a live registrant.

    Still accepted for Slice 1, and for a stronger reason than "the window
    is tiny": a raw ctypes clone3(CLONE_INTO_CGROUP) would place the child
    atomically but would not run PyOS_AfterFork_Child at all unless the
    caller re-invoked it by hand -- so it would skip the very handlers that
    reinitialise the stdlib's own locks in the child, trading this window
    for a larger hazard and MORE machinery, not less
    (architectural-simplicity). Called out plainly rather than left to read
    as an oversight.

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
            if scope_path is not None:
                place_self(scope_path)
        except BaseException:
            _exit_child(70)
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
    one pytest_runtest_protocol call, AND records the full ordered event
    stream (logstart, every TestReport, logfinish) the supervisor replays
    into its own real pytest hooks. Registered on item.config.pluginmanager
    only for the duration of that one call -- see run_one.

    Reports are serialized here, in the child, via pytest's own
    pytest_report_to_serializable hook (cited by NAME, never by line number
    -- those drift by version and were wrong in more than one earlier draft
    of this design). logstart/logfinish are forwarded separately, exactly as
    xdist forwards them."""

    _RANK = {"passed": 0, "skipped": 1, "failed": 2, "error": 3}

    def __init__(self, config):
        self.worst = "passed"
        self.events = []
        self._config = config

    def pytest_runtest_logstart(self, nodeid, location):
        self.events.append({"kind": "logstart", "nodeid": nodeid, "location": location})

    def pytest_runtest_logfinish(self, nodeid, location):
        self.events.append({"kind": "logfinish", "nodeid": nodeid, "location": location})

    def pytest_runtest_logreport(self, report):
        data = self._config.hook.pytest_report_to_serializable(
            config=self._config, report=report
        )
        self.events.append({"kind": "report", "nodeid": report.nodeid, "data": data})
        outcome = report.outcome if report.outcome in self._RANK else "failed"
        # Pytest's terminal reporter calls setup/teardown failures "errors",
        # reserving "failed" for a failure in the test call itself.
        if outcome == "failed" and report.when in ("setup", "teardown"):
            outcome = "error"
        if self._RANK[outcome] > self._RANK.get(self.worst, 0):
            self.worst = outcome


def run_one(item):
    """Executes one already-collected pytest Item through pytest's own item
    protocol (setup/call/teardown), returning
    (outcome, events) -- outcome being "passed", "failed", "skipped", or
    "error", and events the ordered list of serializable dicts the
    supervisor replays into its own real pytest hooks (Slice 2).

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
    collector = _OutcomeCollector(item.config)
    plugin_manager = item.config.pluginmanager
    plugin_manager.register(collector, name="aitest-outcome-collector")
    try:
        item.ihook.pytest_runtest_protocol(item=item, nextitem=None)
    finally:
        plugin_manager.unregister(collector)
    if collector.worst == "passed":
        outcome = "passed"
    elif collector.worst == "skipped":
        outcome = "skipped"
    elif collector.worst == "failed":
        outcome = "failed"
    else:
        outcome = "error"
    return outcome, collector.events


def run_worker_loop(scope_path, items_by_nodeid, pipe_in, pipe_out):
    # The single convergence point for BOTH fork sites -- spawn_worker's
    # confined path (which goes through fork_worker/place_self) and
    # _spawn_fallback_worker's separate bare os.fork() (which does not).
    # Child-init MUST live here, not beside either fork call, or the fallback
    # workers -- the exact path the fd-capture P0 was first found on -- would
    # stay contaminated.
    for item in items_by_nodeid.values():
        _init_forked_child(item.config)
        break
    started_at = time.monotonic()
    completed_count = 0
    for line in pipe_in:
        nodeid = line.rstrip("\n")
        if nodeid == "":
            continue
        if nodeid == "__stop__":
            break
        item = items_by_nodeid[nodeid]
        outcome, events = run_one(item)
        completed_count += 1
        recycling = _should_recycle(scope_path, started_at, completed_count)
        # Every event line goes out BEFORE this nodeid's plain result line,
        # into the same stream, with ONE flush after the result line: the
        # supervisor's stage-then-replay contract depends on never seeing the
        # result line ahead of the events it confirms.
        #
        # default=str is required, not optional (v5/v6): a test calling
        # record_property("x", <non-JSON-serializable object>) otherwise makes
        # json.dumps raise TypeError inside this child's own hookimpl, which
        # propagates out through run_one into the broad
        # `except BaseException: _exit_child(70)` guard and kills the WORKER --
        # turning a genuinely PASSING test into "unevaluated" once the retry
        # crashes the same way. str, NOT repr: the real installed junitxml
        # plugin applies str(propvalue) to a property value, so repr would
        # silently emit DIFFERENT XML than a plain, non-aitest run for any
        # object whose __str__/__repr__ differ.
        for event in events:
            pipe_out.write(
                _EVENT_LINE_PREFIX + json.dumps(_tag_tuples(event), default=str) + "\n"
            )
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
