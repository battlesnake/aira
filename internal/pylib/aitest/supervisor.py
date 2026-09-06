import errno
import json
import os
import re
import select
import selectors
import signal
import subprocess
import sys
import time
import urllib.parse

from aitest.worker import _EVENT_LINE_PREFIX, _RECYCLE_SUFFIX, _exit_child, _untag_tuples, fork_worker, run_worker_loop


_STOP_LINE = "__stop__"
_DENIAL_RETRY_SECONDS = 1.0
# AIRA-64. A SPECULATIVE admission request: "answer from what you can obtain
# without waiting". The daemon treats max_wait_ms == 0 as try-acquire on both
# its outer-scope lock and its CPU gate, so such a request never waits on
# another job's critical section -- which matters because every one of them is
# issued from this module's SINGLE-THREADED dispatch loop, where a blocking
# request stalls result draining and nodeid dispatch for every live worker.
#
# It is NOT the same as "non-blocking": a speculative request still performs
# bounded cgroup I/O and, when it GRANTS, the same fork and placement-ack this
# module already pays for every other spawn. What it guarantees is that it never
# polls and never waits on another job.
_SPECULATIVE_MAX_WAIT = "0s"
# How often the dispatch loop may issue one speculative growth probe. Checked on
# EVERY loop iteration rather than only on the select() timeout: a suite whose
# tests complete in well under a second keeps result and pidfd descriptors
# continuously ready, so the timeout branch may never be taken at all and a
# probe attached to it would never fire (Sol plan-review).
_GROWTH_PROBE_INTERVAL_SECONDS = 1.0
# How often (in retry attempts, i.e. roughly every N seconds at the above
# interval) to remind stderr that a run is stalled waiting on a reachable
# but saturated daemon -- see _wait_for_admission_or_disable, shared by
# run()'s startup path and _replace_worker's "this was the last worker"
# path. A stuck run must never be SILENT, even though it must also never
# fall back to unconfined just because the wait is long.
_DENIAL_WARN_EVERY = 30

# AIRA-92. The supervisor is SINGLE-THREADED: while it is blocked reading the
# worker-admit relay it drains no worker's result pipe and dispatches no queued
# nodeid to an idle worker. Every blocking read it performs must therefore be
# bounded, or one wedged relay stops the entire pool with no diagnostic and no
# forward progress -- alive, near-zero CPU, output frozen mid-run.
#
# `aira worker-admit` is NOT self-bounding at the process level. Its socket read
# deadline is max_wait + admitTransportGrace (1s, internal/runner/
# admission_linux.go:81), but three of its own segments sit OUTSIDE that
# deadline with no bound in code: the dial (no Dialer.Timeout and a
# deadline-free ctx, internal/runner/worker_admit_client_linux.go:55-59),
# runner.CreateWorkerScope (passed ctx rather than signalCtx, so not even
# SIGINT-cancellable, cmd/aira/main.go:1074), and daemon.PathsFromEnv's path
# walking (cmd/aira/main.go:1059). The daemon's own evaluateWorkerAdmit
# additionally holds job.mu across two cgroupfs reads and documents itself as
# "uninterruptible and not itself deadline-aware" (internal/daemon/
# worker_admit.go:192-213). So the client must impose its own bound.
#
# The grace is deliberately several times the Go side's own 1s transport grace:
# this bound is the LAST resort against a wedge, never a competing timer that
# races a healthy-but-slow relay into a spurious timeout.
_ADMIT_READ_GRACE_SECONDS = 15.0
# Used only when --max-wait cannot be parsed at all. The CLI itself caps
# --max-wait at 30m (cmd/aira/main.go:962-969), so this can never fire before a
# genuinely-waiting relay would have answered; it exists purely so an
# unparseable value degrades to "bounded but generous" instead of "unbounded".
_MAX_WAIT_FALLBACK_SECONDS = 1800.0
# The post-fork placement ack is pure interpreter work in the child -- close
# inherited fds, fdopen two pipes, write one line -- with no test code and no
# daemon round trip in it. A minute is orders of magnitude more than that costs
# even on a thrashing box, so a timeout here means the child is genuinely
# wedged, not merely slow.
_PLACEMENT_ACK_TIMEOUT_SECONDS = 60.0
# Bound on waiting for an already-signalled child or a released relay to
# actually go away, before escalating to SIGKILL. Reaching this is abnormal;
# blocking on it forever is worse.
_REAP_TIMEOUT_SECONDS = 5.0

# A sentinel distinct from None, for dict lookups where "the key is absent" and
# "the value is None" must not be conflated. See _pool_covers_the_queue.
_UNKNOWN = object()

_GO_DURATION = re.compile(r"^\s*([0-9]+(?:\.[0-9]+)?)\s*(ns|us|µs|ms|s|m|h)?\s*$")
_GO_DURATION_SCALE = {
    None: 1.0, "": 1.0, "ns": 1e-9, "us": 1e-6, "µs": 1e-6,
    "ms": 1e-3, "s": 1.0, "m": 60.0, "h": 3600.0,
}


def _env_seconds(name, default):
    """A positive float override, or the pinned default. A malformed or
    non-positive value falls back to the default rather than disabling the
    bound: an unbounded read is the bug this exists to prevent, so no operator
    typo may ever reintroduce one."""
    raw = os.environ.get(name, "")
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        sys.stderr.write(
            "aira aitest: %s has invalid value %r; using default %r\n" % (name, raw, default)
        )
        return default
    if value <= 0:
        sys.stderr.write(
            "aira aitest: %s must be positive (got %r); using default %r\n" % (name, raw, default)
        )
        return default
    return value


def _parse_max_wait_seconds(max_wait):
    """Best-effort parse of the SAME --max-wait string handed to the relay, so
    the client's own bound is always strictly later than the relay's. Any value
    this cannot parse yields _MAX_WAIT_FALLBACK_SECONDS -- never an exception
    and never an unbounded wait."""
    match = _GO_DURATION.match(max_wait) if isinstance(max_wait, str) else None
    if match is None:
        return _MAX_WAIT_FALLBACK_SECONDS
    return float(match.group(1)) * _GO_DURATION_SCALE[match.group(2)]


def _read_line_deadline(fd, timeout):
    """Read one newline-terminated line from a raw fd, bounded by timeout.

    Returns (line, timed_out). A timeout returns ("", True) -- whatever partial
    bytes arrived are deliberately discarded, because the only caller (the
    worker-admit outcome line) has no use for half a record and must not
    attribute meaning to one.

    Reads the RAW fd rather than a buffered readline for the same reason
    _drain_available_lines does: a buffered wrapper can hold bytes select()
    cannot see. Over-reading past this line's newline is safe here specifically
    because `aira worker-admit` writes exactly one stdout line and then blocks
    on its own stdin reaching EOF (cmd/aira/main.go:1092-1105) -- there is never
    a second line to lose."""
    buf = b""
    deadline = time.monotonic() + timeout
    selector = selectors.DefaultSelector()
    try:
        selector.register(fd, selectors.EVENT_READ)
        while b"\n" not in buf:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not selector.select(remaining):
                return "", True
            try:
                chunk = os.read(fd, 65536)
            except BlockingIOError:
                continue
            if not chunk:
                break
            buf += chunk
    finally:
        selector.close()
    line, sep, _ = buf.partition(b"\n")
    # "replace", not "strict": a stray non-UTF-8 byte must degrade to an
    # unrecognized line handled by the caller's existing malformed-response
    # guards, never an uncaught UnicodeDecodeError that loses the whole run.
    return (line.decode("utf-8", "replace") if sep else ""), False


def _terminate_process(process):
    """Release a subprocess we are done with, without ever blocking forever on
    it. Escalates to SIGKILL rather than swallowing a timed-out wait and leaving
    a live process behind."""
    if process is None:
        return
    try:
        process.wait(timeout=_env_seconds("AIRA_AITEST_REAP_TIMEOUT", _REAP_TIMEOUT_SECONDS))
        return
    except Exception:
        pass
    try:
        process.kill()
    except Exception:
        pass
    try:
        process.wait(timeout=_env_seconds("AIRA_AITEST_REAP_TIMEOUT", _REAP_TIMEOUT_SECONDS))
    except Exception:
        # Nothing further is safe or useful here; never abort a run over the
        # cleanup of a process whose work is already finished.
        pass


def _reap_child(pid):
    """waitpid a child we expect to be exiting, bounded, escalating to SIGKILL.

    The unbounded os.waitpid() this replaces sat directly on the dispatch loop
    (retirement, recycle, shutdown), so a child that reported its result and
    then wedged on the way out -- a coverage save, a wedged atexit, an
    uninterruptible page-fault wait under memory pressure -- froze the whole
    supervisor exactly as a wedged relay does."""
    deadline = time.monotonic() + _env_seconds("AIRA_AITEST_REAP_TIMEOUT", _REAP_TIMEOUT_SECONDS)
    killed = False
    while True:
        try:
            done, _ = os.waitpid(pid, os.WNOHANG)
        except ChildProcessError:
            return
        if done:
            return
        if time.monotonic() >= deadline:
            if killed:
                # It has been SIGKILLed and still is not reapable. Leaving a
                # zombie is strictly better than never returning to the loop.
                return
            try:
                os.kill(pid, signal.SIGKILL)
            except OSError:
                return
            killed = True
            deadline = time.monotonic() + _env_seconds("AIRA_AITEST_REAP_TIMEOUT", _REAP_TIMEOUT_SECONDS)
        time.sleep(0.01)


class WorkerAdmitUnavailable(Exception):
    """Daemon-backed admission is not usable FOR THIS RUN: a dial failure,
    a relay the host could not launch, a client/daemon protocol-version
    skew, or an outer scope that is not a real daemon-admitted scope at
    all. Together with WorkerPlacementFailed it is one of exactly two
    classes that disable daemon-backed admission for the rest of the run
    (_disable_daemon, Task 16).

    The relay reports this as class=admission-unusable. The class names
    the disposition, not a diagnosis of the daemon's health -- the
    unbounded-outer-scope case reaches it with a perfectly healthy daemon
    (design spec section 3.7)."""
    pass


class WorkerAdmitDenied(Exception):
    """The daemon IS reachable and answered, there is simply no room or no
    answer right now: class=contended. Covers "denied" (budget genuinely
    exhausted at this instant), "timeout" (the request waited out its full
    window -- busy, not down), and "unevaluated" (a live memory read could
    not be established on an otherwise-reachable daemon; AIRA-38 and
    AIRA's own "report unevaluated, never a fake pass" rule). Means "don't
    add a worker at this moment", never "abandon containment for the rest
    of the run"."""
    pass


class WorkerAdmitTerminal(Exception):
    """Base class for the two verdicts that are permanent for the affected
    queued work while leaving the daemon available: the queue is marked
    unevaluated, nothing is retried, and nothing runs unconfined. Callers
    catch this base rather than either subclass, so a new terminal class
    can never be silently dropped into the retry path."""
    pass


class WorkerAdmitRequestInvalid(WorkerAdmitTerminal):
    """A verdict that no amount of retrying or waiting can change, from a
    daemon that is answering perfectly well: class=request-invalid.
    Retrying would wait forever against a healthy daemon, while falling back
    to an unconfined worker would silently remove RAM containment for the
    rest of the run -- so this is terminal for the affected queued work and
    nothing else.

    It was called WorkerAdmitRequestTooLarge, which AIRA-45 recorded as an
    actively misleading diagnostic even before AIRA-39: it was already
    raised for protocol-level argument rejections and version skew, neither
    of which is a sizing problem. AIRA-39 then added two members that are
    not about the request at all -- worker-scope-create-failed and
    worker-id-space-exhausted are daemon-side cgroupfs facts about the outer
    scope. The name now matches the class token exactly, which is the same
    one-vocabulary rule the rest of this channel follows.

    Current members: the daemon's exceeds-ceiling (this request's
    estimated-byte sizing can never fit under the outer scope's cap even
    with zero contention), its worker-scope-create-failed and
    worker-id-space-exhausted verdicts, its protocol-level argument
    rejection, and the CLI's own pre-dial argument validation."""
    pass


class WorkerAdmitContractViolation(WorkerAdmitTerminal):
    """The relay and this supervisor disagree about the outcome channel
    itself: class=contract-violation. Raised for an outcome line that does
    not parse, a state or class outside the catalogue, a state/class pair
    that contradicts itself, a granted line missing its placement fields,
    an unintelligible daemon frame, or a daemon error code the relay does
    not know.

    It is deliberately terminal-and-loud rather than a fallback. The whole
    point of the structured channel (AIRA-42) is that an unrecognised
    shape must never be resolved into "the daemon is gone" and used to
    strip RAM containment from the rest of the suite, which is exactly
    what the substring classifier this replaced did by default."""
    pass


class WorkerPlacementFailed(Exception):
    """place_self() never completed (or its child-side placement ack never
    arrived) -- the forked child died before we could confirm it actually
    joined its granted cgroup scope. Distinct from a worker that WAS placed
    and crashed later mid-test (Task 15's _handle_worker_exit path): a
    placement failure means the admitted grant was never even used for a
    test. The second of the two containment-stripping classes.

    AIRA-39 removed the relay's own producer of class=placement-failed: the
    CLI no longer creates the worker scope, so a creation failure is now the
    daemon's worker-scope-create-failed verdict (WorkerAdmitRequestInvalid)
    rather than a local placement failure discovered after a grant. This
    supervisor's own fork/ack path above is therefore the only thing that
    raises this today, and class=placement-failed remains its name on the
    channel."""
    pass


# ---------------------------------------------------------------------------
# The worker-admit outcome channel.
#
# `aira worker-admit` writes exactly ONE machine-readable line to stdout in
# every outcome, grant or not:
#
#   aira-worker-admit state=<enum> class=<enum> reason=<token> [grant fields]
#                     [detail=<query-escaped free text>]
#
# `class` is the load-bearing field and maps to exactly one exception below by
# EXACT dictionary lookup. Nothing here inspects prose. This replaced eleven
# substring probes over the relay's stderr sentence whose fallthrough default
# was WorkerAdmitUnavailable -- i.e. whose default was to abandon RAM
# containment for the rest of the suite whenever the Go side reworded an error
# (AIRA-42; six recorded recurrences of "add one more substring").
#
# The two catalogues below are the Python half of a vocabulary defined once in
# Go (internal/runner/worker_admit_outcome.go).
# TestWorkerAdmitOutcomeVocabularyMatchesTheSupervisor (internal/pylib) holds
# the two equal in both directions, so drift fails the build instead of
# misclassifying at runtime.
# ---------------------------------------------------------------------------

_OUTCOME_MARKER = "aira-worker-admit"

_OUTCOME_STATES = frozenset((
    "granted",
    "denied",
    "timeout",
    "unevaluated",
    "unavailable",
    "argument-invalid",
    "placement-failed",
))

_OUTCOME_CLASS_EXCEPTIONS = {
    "granted": None,
    "contended": WorkerAdmitDenied,
    "request-invalid": WorkerAdmitRequestInvalid,
    "admission-unusable": WorkerAdmitUnavailable,
    "placement-failed": WorkerPlacementFailed,
    "contract-violation": WorkerAdmitContractViolation,
}

# AIRA-35 removed memory_high from this contract along with the memory.high
# write it named: a required wire field named after a cgroup control the daemon
# deliberately no longer writes would be a lie in the protocol.
#
# AIRA-123 made `containment` required on every granted line and made it decide
# which of the remaining keys are required, permitted and FORBIDDEN. The
# forbidden half is the load-bearing half: a ledger-only grant that carried a
# scope path would be read as the real thing by every reader that looks at
# `scope` first -- which is all of them, since that is what the field was for.
#
# An ABSENT containment token is a contract violation, never a default. Making
# absence mean "enforced" would be the same defect one level up: the strong
# claim must be stated, never inferred from silence.
_OUTCOME_CONTAINMENTS = frozenset((
    "enforced",
    "advisory(ci-shim,no-cgroup,no-kill-backstop)",
))

# Which grade requires which coordinates. Mirrors Go's
# workerAdmitGrantShapeProblem (internal/runner/worker_admit_outcome.go), whose
# renderer refuses to produce a line these rules would reject.
_OUTCOME_GRANT_REQUIRED = {
    "enforced": ("scope", "worker_id", "memory_max"),
    "advisory(ci-shim,no-cgroup,no-kill-backstop)": ("worker_id", "reserved"),
}
_OUTCOME_GRANT_FORBIDDEN = {
    "enforced": ("reserved",),
    "advisory(ci-shim,no-cgroup,no-kill-backstop)": ("scope", "memory_max"),
}
_OUTCOME_GRANT_POSITIVE_INTS = {
    "enforced": ("memory_max",),
    "advisory(ci-shim,no-cgroup,no-kill-backstop)": ("reserved",),
}

# The aitest ADMISSION BACKEND grades, reported once by the `aitest-bootstrap`
# verb and pinned against Go's runner.AitestAdmission* catalogue.
_ADMISSION_SUB_SCOPE = "cgroup-sub-scope"
_ADMISSION_LEDGER_ONLY = "ledger-only"
_ADMISSION_GRADES = frozenset((
    "cgroup-sub-scope",
    "ledger-only",
))

# The one mapping from a per-grant containment grade to the backend grade that
# must have produced it. Mirrors runner.AitestAdmissionForContainment.
_ADMISSION_FOR_CONTAINMENT = {
    "enforced": _ADMISSION_SUB_SCOPE,
    "advisory(ci-shim,no-cgroup,no-kill-backstop)": _ADMISSION_LEDGER_ONLY,
}


def _describe_outcome(fields, diagnostic=""):
    """One human-readable rendering of a parsed outcome. Built for stderr
    and for the exception message; never parsed by anything."""
    text = "worker-admit state=%s class=%s reason=%s" % (
        fields.get("state", "?"), fields.get("class", "?"), fields.get("reason", "?"),
    )
    detail = fields.get("detail", "")
    if detail:
        text += ": " + detail
    diagnostic = (diagnostic or "").strip()
    if diagnostic:
        text += " [relay stderr: %s]" % diagnostic
    return text


def _parse_worker_admit_outcome(line):
    """Parse the relay's single outcome line into a field dict.

    Every rejection here raises WorkerAdmitContractViolation, never
    WorkerAdmitUnavailable: a line we cannot understand is the two sides of
    this channel disagreeing, and resolving that into "there is no daemon"
    is precisely how containment used to get stripped silently. The marker
    is compared with ==; it is a frame marker, not a prefix search."""
    tokens = line.split()
    if not tokens or tokens[0] != _OUTCOME_MARKER:
        raise WorkerAdmitContractViolation(
            "worker-admit outcome line is not a worker-admit outcome: %r" % line
        )
    fields = {}
    for token in tokens[1:]:
        key, separator, raw = token.partition("=")
        if not separator or not key:
            raise WorkerAdmitContractViolation(
                "worker-admit outcome token %r is not key=value" % token
            )
        fields[key] = urllib.parse.unquote_plus(raw)
    state = fields.get("state")
    klass = fields.get("class")
    if state not in _OUTCOME_STATES:
        raise WorkerAdmitContractViolation(
            "worker-admit outcome state %r is not in this supervisor's catalogue "
            "(relay and supervisor are out of lockstep)" % state
        )
    if klass not in _OUTCOME_CLASS_EXCEPTIONS:
        raise WorkerAdmitContractViolation(
            "worker-admit outcome class %r is not in this supervisor's catalogue "
            "(relay and supervisor are out of lockstep)" % klass
        )
    if (state == "granted") != (klass == "granted"):
        raise WorkerAdmitContractViolation(
            "worker-admit outcome state %r and class %r disagree about grantedness" % (state, klass)
        )
    return fields


def _read_line_blocking(fd, state, timeout=None):
    """Read exactly one line from a raw fd, used only for the one-time
    post-fork placement-ack wait (spawn_worker/_spawn_fallback_worker).
    Shares state["read_buffer"] with _drain_available_lines below (both
    read the SAME fd over the worker's lifetime) so no byte a single
    os.read() call happens to over-read past this line's newline is ever
    lost to a later caller.

    AIRA-92: bounded by `timeout` when one is given, setting
    state["read_timeout"] and returning "" on expiry. EOF alone was not
    sufficient coverage: it catches a child that DIED before acking, but not
    one that is alive and wedged before its ack. This read sits directly on the
    dispatch loop, so that wedge stopped the entire pool -- no drain, no
    dispatch, no diagnostic. The caller must be able to tell the two apart: a
    dead child is a genuine placement failure, a wedged one is a transient that
    must NOT be converted into an unconfined run."""
    buf = state.get("read_buffer", b"")
    deadline = None if timeout is None else time.monotonic() + timeout
    selector = None
    try:
        while b"\n" not in buf:
            if deadline is not None:
                if selector is None:
                    selector = selectors.DefaultSelector()
                    selector.register(fd, selectors.EVENT_READ)
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not selector.select(remaining):
                    state["read_buffer"] = buf
                    state["read_timeout"] = True
                    return ""
            chunk = os.read(fd, 65536)
            if not chunk:
                state["result_eof"] = True
                break
            buf += chunk
    finally:
        if selector is not None:
            selector.close()
    line, sep, buf = buf.partition(b"\n")
    state["read_buffer"] = buf
    # "replace", not "strict" (Fable build-review, final gate): a stray
    # non-UTF-8 byte from a test that writes garbage onto this fd (e.g. a
    # forked-not-execed child inheriting the write end, the same surface
    # AIRA-40 documents for the pipe being held OPEN, except here the
    # inheritor WRITES) must degrade to an ordinary malformed/unmatched
    # line -- handled by the existing nodeid-mismatch/placement-ack
    # guards below -- rather than raise an uncaught UnicodeDecodeError
    # that crashes the whole pytest process and loses every result.
    return line.decode("utf-8", "replace") if sep else ""


def _drain_available_lines(fd, state):
    """Non-blocking read of everything CURRENTLY available on fd, split
    into complete lines; any trailing partial line is kept in
    state["read_buffer"] for the next call. fd must already be in
    non-blocking mode (spawn_worker sets this right after the placement
    ack is read).

    This exists instead of a select()-after-readline() check on a
    buffered file object because that combination is a real, demonstrated
    race: os.fdopen(fd, "r")'s TextIOWrapper commonly pulls MULTIPLE
    already-flushed lines off the kernel pipe in a single underlying read
    to satisfy one readline() call (a worker writes a result line, then --
    if recycling -- an immediate __recycle__ line, both flushed in rapid
    succession with no intervening syscall for a reader to wake between
    them) -- so by the time readline() returns just the first line, the
    kernel pipe can already be EMPTY while the wrapper's own internal
    buffer silently holds the second. select() only sees kernel-level
    readiness, so it reports "nothing more" even though a complete
    __recycle__ line is sitting unread one layer up -- reproducing the
    exact silently-dropped-dispatch race this whole mechanism exists to
    close (spec 3.6). Reading raw bytes directly off a non-blocking fd
    into one buffer this module fully owns removes that blind spot: there
    is no second, invisible buffering layer between "what select saw" and
    "what the caller can see"."""
    buf = state.get("read_buffer", b"")
    while True:
        try:
            chunk = os.read(fd, 65536)
        except BlockingIOError:
            break
        if not chunk:
            state["result_eof"] = True
            break
        buf += chunk
    lines = []
    while b"\n" in buf:
        line, _, buf = buf.partition(b"\n")
        # "replace", not "strict" -- see _read_line_blocking's identical
        # comment just above (Fable build-review, final gate).
        lines.append(line.decode("utf-8", "replace"))
    state["read_buffer"] = buf
    return lines


class Supervisor:
    """Owns collection, the pull dispatch queue, and worker-admit relaying.
    Never runs a test itself -- see worker.py for that."""

    _PLACED_LINE = "__placed__"

    def __init__(self, config=None):
        # config is the supervisor's own pytest.Config -- the hook caller
        # every replayed report/logstart/logfinish goes through (Slice 2).
        # It stays optional: Supervisor(config=None) is fully usable and
        # simply replays nothing, which is what Slice 1's own dispatch/
        # admission tests construct.
        self.config = config
        # Nodeids for which a REAL (worker-produced) report was replayed.
        # Synthesized reports deliberately do NOT go in here at replay time --
        # they are added by the synthesis pass itself, so a synthesized report
        # can never make a nodeid look "already reported" to that same pass.
        self._replayed_nodeids = set()
        # nodeid -> the specific reason it ended up unevaluated, so a
        # synthesized report can say something true and specific instead of a
        # generic string.
        self._unevaluated_reasons = {}
        self.queue = []
        self.attempts = {}  # nodeid -> attempt count (Task 15's retry-once rule)
        self.outer_scope = None
        self.supervisor_scope = None
        # AIRA-123. Which per-worker admission backend this run is using.
        # bootstrap() sets it from the verb's own `admission=` token and refuses
        # to start without one, so this initial value is only ever what a
        # supervisor that has NOT bootstrapped believes.
        #
        # It is the ENFORCED backend deliberately, and the direction is the
        # whole point: this value decides which grants are ACCEPTED, so
        # believing "enforced" means an advisory grant arriving here is REFUSED
        # as a contract violation. The opposite default would accept an advisory
        # grant into a run that never learned it was advisory -- containment
        # silently downgraded, which is the one outcome this ticket must make
        # impossible.
        self.admission_mode = _ADMISSION_SUB_SCOPE
        self.daemon_available = True
        self.max_workers_fallback = max(1, int(os.environ.get("AIRA_AITEST_MAX_WORKERS_FALLBACK", "1")))
        self._fallback_warned = False
        self._admission_terminal_warned = False
        self._pidfd_warned = set()
        self.items_by_nodeid = {}
        self.workers = {}
        # Worker scopes whose rmdir failed, for a later retry. See
        # _forget_worker_scope: AIRA-39 turned an unremoved scope from a stray
        # empty directory into a permanent charge against this run's own budget.
        self._unremoved_scopes = set()
        self.results = {}
        self._run_estimated_bytes = 0
        self._run_max_wait = "30s"
        self._run_worker_count = 1
        # AIRA-64 growth probe bookkeeping.
        self._last_growth_probe = 0.0
        self._cpu_slots_warned = False
        self._swap_cap_warned = False

    def bootstrap(self):
        """Relocate this process into its own child scope so the outer scope
        can delegate controllers to worker children. Must run before any
        worker is admitted."""
        command = os.environ.get("AIRA_AITEST_BOOTSTRAP_CMD", "")
        if not command:
            self._disable_daemon("AIRA_AITEST_BOOTSTRAP_CMD is unset")
            return
        try:
            result = subprocess.run(
                [command, "aitest-bootstrap", "--supervisor-pid", str(os.getpid())],
                capture_output=True, text=True, timeout=30,
            )
        except Exception as exc:
            self._disable_daemon(str(exc))
            return
        if result.returncode != 0:
            self._disable_daemon((result.stderr or "").strip() or "aitest-bootstrap failed")
            return
        admission = None
        for token in result.stdout.split():
            if token.startswith("outer="):
                self.outer_scope = token[len("outer="):]
            elif token.startswith("supervisor_scope="):
                self.supervisor_scope = token[len("supervisor_scope="):]
            elif token.startswith("admission="):
                admission = token[len("admission="):]
        if not self.outer_scope:
            self._disable_daemon("aitest-bootstrap did not report an outer scope")
            return
        # AIRA-123. The backend grade is REQUIRED and is not defaulted. A
        # bootstrap that does not state one is out of lockstep with this
        # supervisor, and guessing "cgroup-sub-scope" would be the unsafe guess:
        # this run would then expect enforced grants, and an advisory grant would
        # be refused as a mismatch far later, mid-suite. Disabling daemon-backed
        # admission with one honest warning is the same disposition every other
        # bootstrap failure already takes.
        if admission not in _ADMISSION_GRADES:
            self._disable_daemon(
                "aitest-bootstrap reported admission=%r, which is not in this supervisor's "
                "catalogue (bootstrap and supervisor are out of lockstep)" % (admission,)
            )
            return
        self.admission_mode = admission
        if admission == _ADMISSION_LEDGER_ONLY:
            # Said ONCE, on the run's own output, exactly like the swap-cap and
            # cpu-slots notices: a governance mechanism that is weaker than it
            # looks must never be silent about it on the run it governs.
            sys.stderr.write(
                "aira aitest: per-worker admission is LEDGER-ONLY (advisory): every worker is "
                "admitted against the container's RAM budget, but there is NO cgroup sub-scope, "
                "no memory.max, no kill backstop and no memory-watermark recycling -- a worker "
                "that exceeds what it declared is not killed\n"
            )

    def _disable_daemon(self, reason):
        self.daemon_available = False
        if not self._fallback_warned:
            self._fallback_warned = True
            sys.stderr.write(
                "aira aitest: %s -- falling back to n_workers<=%d, UNCONFINED (no per-worker RAM containment)\n"
                % (reason, self.max_workers_fallback)
            )

    def _fail_queue_terminal(self, reason):
        """Mark queued work unevaluated after a WorkerAdmitTerminal verdict,
        preserving daemon availability and therefore never triggering an
        unconfined fallback for this condition.

        The message deliberately does NOT assert a sizing problem: the two
        terminal classes are a per-request rejection (which may or may not
        be about sizing) and a channel contract violation (which never is).
        The verdict's own reason token, carried in `reason`, says which."""
        if not self._admission_terminal_warned:
            self._admission_terminal_warned = True
            sys.stderr.write(
                "aira aitest: %s -- remaining queued tests cannot be admitted; "
                "marking them unevaluated rather than waiting forever or running unconfined\n" % reason
            )
        while self.queue:
            nodeid = self.queue.pop(0)
            self.results.setdefault(nodeid, "unevaluated")
            self._unevaluated_reasons.setdefault(nodeid, reason)

    def collect(self, items):
        """items: the pytest-collected Item objects. Session collection
        already ran in this process before bootstrap; the forked worker
        inherits self.items_by_nodeid by COW, so only nodeids (strings)
        cross the dispatch/result pipes (Task 13)."""
        self.items_by_nodeid = {item.nodeid: item for item in items}
        self.queue = [item.nodeid for item in items]

    def next_nodeid(self):
        if not self.queue:
            return None
        nodeid = self.queue.pop(0)
        self.attempts[nodeid] = self.attempts.get(nodeid, 0) + 1
        return nodeid

    def requeue_once(self, nodeid):
        """A worker died mid-test. Requeue exactly once; the second failure
        is the caller's job to report unevaluated, not this method's."""
        if self.attempts.get(nodeid, 0) >= 2:
            return False
        self.queue.insert(0, nodeid)
        return True

    def acquire_worker(self, estimated_bytes, max_wait="30s"):
        """Returns (grant: dict, process: subprocess.Popen) on success.
        process.stdin stays open as the daemon lease -- close it to release.

        The relay classifies its own outcome and reports it as one
        structured stdout line; this method's only job is an exact
        dictionary lookup from that line's `class` field to one of the four
        exception types (_OUTCOME_CLASS_EXCEPTIONS). It does NOT inspect
        the relay's stderr, which carries a human diagnostic only.

        The caller MUST treat the exception types differently (Task 16):
        only WorkerAdmitUnavailable and WorkerPlacementFailed may disable
        daemon-backed admission for the rest of the run."""
        if not self.daemon_available:
            raise WorkerAdmitUnavailable("daemon unavailable")
        command = os.environ.get("AIRA_AITEST_WORKER_ADMIT_CMD", "")
        if not command:
            raise WorkerAdmitUnavailable("AIRA_AITEST_WORKER_ADMIT_CMD is unset")
        try:
            process = subprocess.Popen(
                [command, "worker-admit", "--job-id", str(os.getpid()), "--outer-scope", self.outer_scope,
                 "--estimated-bytes", str(estimated_bytes), "--max-wait", max_wait],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, close_fds=True,
            )
        except OSError as exc:
            # EAGAIN/ENOMEM here mean this HOST could not fork a process right
            # now -- a transient resource condition, and one that peaks under
            # exactly the contention this whole path is about (Fable
            # plan-review, P2-4). It says nothing whatsoever about the daemon,
            # so reporting it as WorkerAdmitUnavailable permanently stripped RAM
            # containment for the rest of the run over a momentary fork failure
            # on a perfectly healthy daemon -- the same misdiagnosis class as
            # the transport branch below. Every other OSError (ENOENT, EACCES on
            # the aira binary) IS a static, permanent local fact and stays
            # unavailable.
            if exc.errno in (errno.EAGAIN, errno.ENOMEM):
                raise WorkerAdmitDenied(
                    "worker-admit state=denied class=contended reason=fork-unavailable: %s" % exc
                )
            raise WorkerAdmitUnavailable(str(exc))
        # AIRA-92: BOUNDED, never a bare readline(). See _read_line_deadline and
        # the _ADMIT_READ_GRACE_SECONDS comment for why the relay cannot be
        # trusted to bound itself, and why this single untimed read was able to
        # freeze the whole pool -- no result drained, no nodeid dispatched to an
        # already-idle worker, no diagnostic, for as long as the relay stayed
        # wedged.
        #
        # "replace", not "strict" (Sol build-review, AIRA-38 review wave): a
        # corrupted/truncated write or a stray binary byte from a
        # misbehaving relay build must degrade to a line that fails to parse
        # (WorkerAdmitContractViolation below) rather than raise an uncaught
        # UnicodeDecodeError that crashes the whole pytest process.
        read_timeout = _parse_max_wait_seconds(max_wait) + _env_seconds(
            "AIRA_AITEST_ADMIT_READ_GRACE", _ADMIT_READ_GRACE_SECONDS
        )
        line, timed_out = _read_line_deadline(process.stdout.fileno(), read_timeout)
        line = line.strip()
        if timed_out:
            # The relay is alive but has answered nothing at all inside its own
            # declared budget plus a generous grace. Release it and treat this
            # as a DENIAL, never as WorkerAdmitUnavailable: we could not
            # establish a result, which is precisely the state AIRA requires be
            # reported as such rather than resolved into a confident verdict.
            # Calling it "unavailable" would assert the daemon is gone -- an
            # unproven claim whose consequence is stripping RAM containment for
            # the rest of the run. Calling it a denial keeps containment, keeps
            # the surviving pool dispatching, and lets the existing retry path
            # try again.
            #
            # Accepted, documented consequence: if the daemon granted in the
            # microscopic window between our deadline and this kill, it releases
            # that grant on peer disconnect (internal/daemon/worker_admit.go:
            # 456-464, idempotent at :295-306) but the scope directory it
            # created is orphaned until AIRA-36's reaper sweeps it. A leaked
            # empty directory is strictly better than a frozen run.
            _terminate_process(process)
            sys.stderr.write(
                "aira aitest: worker-admit relay did not answer within %.1fs "
                "(--max-wait %s); treating as a transient denial, containment preserved\n"
                % (read_timeout, max_wait)
            )
            raise WorkerAdmitDenied(
                "worker-admit state=denied class=contended reason=relay-unresponsive "
                "after %.1fs" % read_timeout
            )
        if not line:
            # The relay produced NO outcome line at all: it died, was killed,
            # or exited without writing. That is not a classification we can
            # make -- it is the absence of one -- and the honest reading is
            # that daemon-backed admission is not usable through this relay.
            # It is a NAMED condition, not the fallthrough default of a
            # substring chain, and the relay's stderr is attached purely as a
            # human diagnostic: nothing below inspects it.
            #
            # Release BEFORE reading stderr (AIRA-92): no grant was issued, so
            # the relay owes us nothing further and this ordering cannot lose a
            # diagnostic the way it would on the granted path below (whose
            # relay genuinely blocks on stdin EOF first). Reading an unbounded
            # stderr from a relay that has NOT exited was itself an untimed
            # read on the dispatch loop.
            _terminate_process(process)
            diagnostic = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            raise WorkerAdmitUnavailable(
                "worker-admit relay produced no outcome line [relay stderr: %s]"
                % (diagnostic.strip() or "none")
            )
        try:
            outcome = _parse_worker_admit_outcome(line)
        except WorkerAdmitContractViolation as exc:
            _terminate_process(process)
            diagnostic = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            raise WorkerAdmitContractViolation(
                "%s [relay stderr: %s]" % (exc, diagnostic.strip() or "none")
            ) from exc
        if outcome["state"] != "granted":
            _terminate_process(process)
            diagnostic = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            # ONE exact dictionary lookup. The relay owns the classification;
            # this side owns only the mapping from its class to the exception
            # that carries the disposition. _parse_worker_admit_outcome has
            # already refused any class outside the catalogue, so there is no
            # default branch here to fall through to -- which is the whole
            # point: the old default was "abandon RAM containment".
            #
            # The lookup cannot yield the None sentinel here: the only class
            # mapped to None is "granted", and the parser's own
            # grantedness-agreement check guarantees class == "granted" iff
            # state == "granted", which this branch has already excluded.
            # That check is load-bearing for this line, not decorative.
            raise _OUTCOME_CLASS_EXCEPTIONS[outcome["class"]](
                _describe_outcome(outcome, diagnostic)
            )
        containment = outcome.get("containment")
        grant, malformed = self._validate_grant(outcome, containment)
        if malformed is not None:
            # A granted outcome line missing (or corrupting) its placement
            # fields is the relay and this supervisor disagreeing about the
            # channel: the Go side refuses to RENDER such a line
            # (WorkerAdmitOutcomeLine), so seeing one means the two are out
            # of lockstep or the stream was corrupted. Terminal and loud,
            # not a silent unconfined fallback -- see
            # WorkerAdmitContractViolation's own docstring. Release it
            # exactly as for every other non-usable response, rather than
            # leaving its lease and pipes around.
            #
            # ORDER IS LOAD-BEARING, and both halves of it are:
            #
            # stdin MUST close BEFORE any blocking stderr read (Sol
            # build-review, AIRA-38 review wave): per runWorkerAdmitCommand's
            # own confirmed contract, once a granted outcome is printed the
            # CLI relay blocks on ITS OWN stdin reaching EOF before it exits
            # (or writes anything further to stderr) -- reading stderr
            # first deadlocks unconditionally against a process that will
            # never close its own stderr until stdin closes.
            #
            # And _terminate_process MUST come before the read, not after
            # (this revision, second review round): closing stdin makes a
            # WELL-BEHAVED relay exit, but a wedged one that ignores its own
            # stdin EOF left this unbounded read sitting on the
            # single-threaded dispatch loop -- the exact AIRA-92 hazard, and
            # the one remaining untimed read on this path. Terminating first
            # costs nothing in the normal case (a relay that already exited
            # on stdin EOF is reaped, not signalled) and bounds the wedged
            # case; only a relay that was mid-write to stderr AND ignoring
            # stdin can lose part of its diagnostic, and such a relay was not
            # going to finish that write anyway.
            try:
                process.stdin.close()
            except BrokenPipeError:
                pass
            # Bounded, and escalates to SIGKILL rather than leaving a live relay
            # behind (AIRA-92): the previous `process.wait()` after kill() was
            # itself unbounded.
            _terminate_process(process)
            stderr = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            # Mirrors _retire_worker's best-effort scope cleanup (Fable
            # build-review, final gate): a malformed grant can still name
            # a real, already-created scope (e.g. a bad memory_max value
            # alongside a fine scope path) -- "scope" itself may be one of
            # the missing fields, so this is guarded, unlike the
            # unconditional rmdir in spawn_worker's placement-failure path
            # where "grant" is always fully well-formed by construction.
            # AIRA-39 made this removal load-bearing rather than best-effort:
            # the daemon's ledger now sums memory.max over the outer scope's
            # real children, so a scope left behind KEEPS CHARGING this run's
            # budget. _forget_worker_scope remembers a failed rmdir for the
            # retry sweep instead of merely warning.
            self._forget_worker_scope(grant.get("scope"))
            raise WorkerAdmitContractViolation(
                "worker-admit granted outcome is malformed: %s [relay stderr: %s]"
                % (malformed, stderr.strip() or "none")
            )
        self._note_cpu_slots_state(outcome.get("cpu_slots", ""))
        self._note_swap_cap_state(outcome.get("swap_cap", ""))
        return grant, process

    def _validate_grant(self, outcome, containment):
        """Return (grant, malformed_reason). malformed_reason is None exactly
        when the grant is usable.

        AIRA-123. The grade decides the shape, in BOTH directions -- required
        keys and FORBIDDEN keys -- and must agree with the backend this run
        bootstrapped into. Every rejection becomes a
        WorkerAdmitContractViolation at the call site: terminal and loud, never
        a silent fallback to unconfined, because a grant this side cannot
        understand is the two ends of the channel disagreeing and the Go
        renderer refuses to PRODUCE any line these rules reject.

        A REJECTED grant still comes back as a best-effort dict rather than
        None, because the caller's cleanup needs `scope` out of it: a malformed
        line can still name a real, already-created cgroup, and AIRA-39 made
        removing it load-bearing (a scope left behind keeps charging the run's
        budget). It carries `scope` only where a scope could legitimately exist
        -- never for an advisory grant, whose whole claim is that there is no
        cgroup, and which must not be able to talk this supervisor into rmdir'ing
        a path it named anyway.

        `grant["scope"]` is None for a ledger-only grant rather than absent, so
        every existing consumer (spawn_worker's fork, _retire_worker's cleanup,
        _forget_worker_scope's rmdir) sees an explicit "there is no scope"
        instead of a KeyError or, worse, a stale value.
        """
        if containment not in _OUTCOME_CONTAINMENTS:
            # Unknown or absent grade: no shape rules apply, so `scope` is
            # passed through for cleanup exactly as the pre-AIRA-123 parser did.
            return {"scope": outcome.get("scope")}, (
                "containment %r is not in this supervisor's catalogue; an absent or "
                "unrecognised grade must never be read as enforced" % (containment,)
            )
        enforced = containment == "enforced"
        grant = {key: outcome[key] for key in _OUTCOME_GRANT_REQUIRED[containment] if key in outcome}
        grant["containment"] = containment
        grant["scope"] = outcome.get("scope") if enforced else None
        expected_backend = _ADMISSION_FOR_CONTAINMENT[containment]
        if expected_backend != self.admission_mode:
            # The bootstrap said one backend and the daemon granted from
            # another: two processes that disagree about what this box is.
            # Neither answer can be trusted, so this is a contract violation
            # rather than something to reconcile silently.
            return grant, (
                "grant containment %r implies the %r backend, but this run bootstrapped "
                "into %r" % (containment, expected_backend, self.admission_mode)
            )
        forbidden = [key for key in _OUTCOME_GRANT_FORBIDDEN[containment] if key in outcome]
        if forbidden:
            return grant, (
                "containment %r must not carry %s" % (containment, ", ".join(sorted(forbidden)))
            )
        missing = [key for key in _OUTCOME_GRANT_REQUIRED[containment] if key not in outcome]
        if missing:
            return grant, "missing required field%s: %s" % (
                "s" if len(missing) != 1 else "", ", ".join(missing)
            )
        for key in _OUTCOME_GRANT_POSITIVE_INTS[containment]:
            try:
                value = int(grant[key])
            except ValueError as exc:
                return grant, "%s must be a positive integer (got %r: %s)" % (key, grant[key], exc)
            if value <= 0:
                return grant, "%s must be a positive integer (got %r)" % (key, grant[key])
        return grant, None

    def _note_cpu_slots_state(self, state):
        """AIRA-64. Say ONCE, on this run's own output, when the daemon could
        not establish CPU governance for it.

        The CPU dimension deliberately fails OPEN (a reading it cannot establish
        must not stall every aitest run on the machine, unlike RAM where an
        unestablished reading risks an outer-scope OOM kill of a whole run). The
        cost of failing open is that a broken gate is INVISIBLE -- which is
        exactly how the AIRA-59 watchdog shipped inert on every real host. So
        the run that is affected gets told, rather than the condition living
        only in the daemon journal.

        An ABSENT token means an older daemon that predates this field, not
        "ok": both are silent here, because inventing a warning for a daemon
        that never claimed anything would be a fabricated diagnosis. The
        real-cgroup test is what proves the gate fires; this is what makes a
        fail-open visible when it does not."""
        if state != "unevaluated" or self._cpu_slots_warned:
            return
        self._cpu_slots_warned = True
        sys.stderr.write(
            "aira aitest: the daemon could not establish CPU-concurrency governance for "
            "this run (cpu_slots=unevaluated); workers are RAM-governed but NOT bounded "
            "against other jobs on this machine, so heavy concurrent runs can still "
            "oversubscribe the CPU\n"
        )

    def _note_swap_cap_state(self, state):
        """AIRA-35. Say ONCE, on this run's own output, when the daemon could
        not bound this run's worker swap.

        cgroup-v2's memory.max bounds MEMORY, not memory+swap. Where swap is
        available and memory.swap.max cannot be set, a worker that exceeds its
        cap is reclaimed into swap and runs to completion instead of being
        killed -- measured: a 512 MiB allocation inside a 32 MiB cap exiting 0
        with half a gigabyte paged out. The per-worker containment this run
        believes it has simply does not happen, and the daemon's aggregate
        sum-over-memory.max guard stops bounding the real footprint too.

        Same shape and same reason as _note_cpu_slots_state: the grant still
        proceeds (refusing would stall every aitest run on such a host), so the
        cost of failing open is that a lost guarantee is invisible -- unless the
        run it affects is told.

        Silent for "enforced" (the cap is set and verified), silent for
        "not-applicable" (this kernel has no swap support at all, proved, so
        memory.max already bounds everything), and silent for an ABSENT token,
        which means a daemon predating this field rather than "ok". Inventing a
        warning for a daemon that never claimed anything would be a fabricated
        diagnosis."""
        if state != "unavailable" or self._swap_cap_warned:
            return
        self._swap_cap_warned = True
        sys.stderr.write(
            "aira aitest: the daemon could not bound this run's worker swap "
            "(swap_cap=unavailable); a worker that exceeds its memory.max will be "
            "reclaimed into swap rather than killed, so per-worker RAM containment "
            "is NOT in force for this run\n"
        )

    def _open_pidfd(self, pid):
        """A pidfd for one forked worker, or None if this host cannot provide
        one.

        AIRA-40. Worker liveness used to be inferred SOLELY from the result
        pipe reaching EOF, and that inference is not sound: a test that itself
        calls os.fork() (or uses multiprocessing's default `fork` start method
        on Linux) without closing inherited fds leaves the grandchild holding a
        DUPLICATE of the worker's result-pipe write end. CLOEXEC cannot help --
        it fires on exec(), not fork() -- so if that grandchild outlives the
        worker (the worker OOM-killed, the grandchild spared), the kernel never
        delivers EOF: one live write end is enough to keep the pipe open. The
        worker's in_flight nodeid was then never cleared, _handle_worker_exit
        never fired, and run()'s stop condition (every worker idle) was never
        satisfiable -- the whole run hung, alive and silent. Verified directly
        on this host: with a grandchild holding the write end, select() reports
        the pidfd ready and the pipe NOT ready.

        A pidfd is the right instrument rather than an os.waitpid(WNOHANG) poll
        on run()'s timeout tick (the ticket's own candidate direction): it goes
        into the SAME select() set as the result pipes, so a death is noticed
        the instant it happens instead of up to a tick later, and -- crucially
        -- observing it does NOT consume the child's exit status, so it cannot
        race _reap_child's own waitpid into a lost or double reap.

        Degrades to None, never to a raised exception: os.pidfd_open needs
        Python 3.9+ and Linux 5.3+, and can fail per-call (ESRCH on a pid
        already reaped by something else). Without it this supervisor keeps
        exactly today's EOF-only behaviour, which is why the loss of the
        independent check is said out loud once rather than left silent."""
        opener = getattr(os, "pidfd_open", None)
        if opener is None:
            self._warn_no_pidfd("this Python has no os.pidfd_open (needs 3.9+ on Linux 5.3+)")
            return None
        try:
            return opener(pid)
        except OSError as exc:
            self._warn_no_pidfd("os.pidfd_open failed for worker %d: %s" % (pid, exc))
            return None

    def _warn_no_pidfd(self, reason):
        # Once per DISTINCT reason, not once per process (build-review): the
        # two conditions have different scope -- a missing os.pidfd_open
        # affects every worker for the whole run, a per-call OSError affects
        # only that one -- so a single flag let one transient ESRCH on the
        # first worker permanently suppress a later report of the permanent
        # condition. Keyed on the reason text with the worker number stripped,
        # so a repeating per-call failure still cannot spam the log.
        key = reason.split(" for worker ")[0]
        if key in self._pidfd_warned:
            return
        self._pidfd_warned.add(key)
        # "for any worker without one", not a flat "crash detection is now
        # EOF-only": the missing-attribute case really does affect every
        # worker, but a per-call OSError affects only that one, and claiming
        # more than was established is the exact habit this project's honesty
        # rules exist to prevent.
        sys.stderr.write(
            "aira aitest: %s -- crash detection falls back to result-pipe EOF alone for any "
            "worker without one, which a test's own forked grandchild can hold open "
            "indefinitely (AIRA-40)\n" % reason
        )

    def _child_close_other_workers_fds(self):
        """A forked child inherits DUPLICATES of every fd already open in
        the parent's fd table -- fork() copies the whole table and there is
        no exec() here for CLOEXEC to ever fire. Without this, a
        later-forked worker keeps a live copy of an EARLIER worker's
        admit-lease pipe (and dispatch/result pipes). So when the
        supervisor later closes ITS OWN copy of an earlier worker's
        admit_process.stdin to retire it, the daemon-side `aira
        worker-admit` CLI's stdin-EOF read never sees EOF (some OTHER fd
        still holds the write end open), and admit_process.wait(timeout=5)
        hangs/raises. Must run before this child does anything else --
        BEFORE closing its own inherited copies of ITS OWN pipes too, so
        order this first in both spawn_worker and _spawn_fallback_worker
        (Task 16)."""
        for state in self.workers.values():
            try:
                state["dispatch_write"].close()
            except Exception:
                pass
            try:
                os.close(state["result_fd"])
            except OSError:
                pass
            # An inherited pidfd (AIRA-40) cannot wedge the parent's own
            # liveness detection the way an inherited pipe write end can --
            # readiness follows the tracked process's exit, not this fd's
            # reference count -- but it is still one more fd this child has no
            # use for, and leaking one per already-running worker into every
            # later fork is exactly the fd-table-copy hygiene the rest of this
            # method exists to enforce.
            pidfd = state.get("pidfd")
            if pidfd is not None:
                try:
                    os.close(pidfd)
                except OSError:
                    pass
            admit_process = state.get("admit_process")
            if admit_process is not None:
                for stream in (admit_process.stdin, admit_process.stdout, admit_process.stderr):
                    if stream is not None:
                        try:
                            stream.close()
                        except Exception:
                            pass

    def spawn_worker(self, estimated_bytes, max_wait="30s"):
        """Admits and forks one worker, returning its pid. Raises
        WorkerAdmitUnavailable/WorkerAdmitDenied if admission fails, a
        WorkerAdmitTerminal subclass (WorkerAdmitRequestInvalid or
        WorkerAdmitContractViolation) if this request can never succeed, or
        WorkerPlacementFailed if the forked child died before confirming it
        joined its granted cgroup scope -- the caller (run()) decides
        fallback/retry/terminal-queue policy for each.

        Safety: the ENTIRE forked-child branch below is wrapped in one
        broad try/except that _exit_child()s (worker.py's coverage-safe
        os._exit wrapper) on ANY exception. A forked child
        must never be allowed to fall through to normal Python control
        flow / interpreter shutdown -- that risks running supervisor-level
        cleanup code fully UNCONFINED. (place_self() itself is separately
        guarded the same way inside fork_worker, Task 12, since it can
        raise before this function's own try even starts.)"""
        grant, admit_process = self.acquire_worker(estimated_bytes, max_wait=max_wait)
        # AIRA-123. None here is a LEDGER-ONLY grant: the daemon really admitted
        # this worker against the container's RAM budget, there is simply no
        # cgroup sub-scope to place it in. Every cgroup-dependent step below is
        # skipped for it -- place_self's cgroup.procs write (inside fork_worker),
        # the placement ack that exists solely to PROVE that write happened, and
        # _should_recycle's memory.current/memory.max watermark read (inside
        # run_worker_loop) -- while everything else, admission included, is
        # unchanged. That last clause is the difference from
        # _spawn_fallback_worker, which skips the same steps but is also
        # completely UNGOVERNED and is only ever reached when the daemon itself
        # is unreachable.
        scope = grant["scope"]
        dispatch_read, dispatch_write = os.pipe()
        result_read, result_write = os.pipe()
        pid, in_child = fork_worker(scope)
        if in_child:
            try:
                self._child_close_other_workers_fds()
                os.close(dispatch_write)
                os.close(result_read)
                admit_process.stdin.close()
                admit_process.stdout.close()
                admit_process.stderr.close()
                pipe_in = os.fdopen(dispatch_read, "r")
                pipe_out = os.fdopen(result_write, "w")
                # Placement is already verified by the time we get here --
                # fork_worker's own child branch os._exit()s before ever
                # returning if place_self failed (Task 12) -- so reaching
                # this line already IS the placement proof. One line down
                # the result pipe lets the parent (below) tell "placed
                # fine, died/recycled later" apart from "never even got
                # placed" for its crash-handling logic (spec 4).
                if scope is not None:
                    pipe_out.write(self._PLACED_LINE + "\n")
                    pipe_out.flush()
                run_worker_loop(scope, self.items_by_nodeid, pipe_in, pipe_out)
            except BaseException:
                _exit_child(70)
            _exit_child(0)
        os.close(dispatch_read)
        os.close(result_write)
        # result_read is handled as a RAW fd with manual line-buffering
        # (_read_line_blocking / _drain_available_lines below) for this
        # worker's whole lifetime, never wrapped in os.fdopen()'s buffered
        # TextIOWrapper -- see _drain_available_lines's docstring for why:
        # a buffered readline() can silently pull more than one line off
        # the kernel pipe in a single underlying read, and a later
        # select()-on-the-wrapped-object check cannot see bytes already
        # sitting in the wrapper's own buffer rather than the pipe.
        state = {"result_fd": result_read, "read_buffer": b"", "result_eof": False}
        if scope is not None:
            self._await_placement_ack(pid, grant, admit_process, state, result_read, dispatch_write)
        # AIRA-123. A LEDGER-ONLY worker has no ack, and skipping it is the
        # honest choice rather than a shortcut. The ack exists to PROVE the
        # child joined its granted cgroup (spawn_worker's own comment: "reaching
        # this line already IS the placement proof"); with no cgroup there is
        # nothing to prove, and inventing an "I started" handshake here would
        # give the two paths a shared failure surface whose only real meaning --
        # WorkerPlacementFailed, i.e. "the local cgroup mechanism is broken" and
        # therefore strip containment for the rest of the run -- would be a
        # diagnosis this mode cannot support. A ledger-only child that dies at
        # startup is handled where every other mid-run death already is
        # (_handle_worker_exit), exactly as the daemon-down fallback pool's
        # children are.
        os.set_blocking(result_read, False)
        state.update({
            "grant": grant,
            "admit_process": admit_process,
            "dispatch_write": os.fdopen(dispatch_write, "w"),
            "in_flight": None,
            # Opened only now, on the path where this worker is actually going
            # to be registered: every failure branch above has already reaped
            # the child, and a pidfd for a reaped pid is either invalid or --
            # worse -- refers to whatever process later reuses that pid. The
            # guarantee here is precisely that NOTHING HAS REAPED this one, so
            # its pid cannot be reused underneath us; it is deliberately not a
            # claim that the child is still running (the ack proves it was
            # alive when it wrote, and it may be a zombie by now, which
            # pidfd_open handles).
            "pidfd": self._open_pidfd(pid),
        })
        self.workers[pid] = state
        return pid

    def _await_placement_ack(self, pid, grant, admit_process, state, result_read, dispatch_write):
        """Block until the forked child confirms it joined its granted cgroup
        scope, or fail. Extracted from spawn_worker unchanged (AIRA-123) so the
        ledger-only path, which has no cgroup and therefore nothing to confirm,
        can skip it without duplicating the surrounding fd bookkeeping.

        Raises WorkerAdmitDenied (transient, containment preserved) or
        WorkerPlacementFailed (the local cgroup mechanism is broken)."""
        # AIRA-92: BOUNDED (Sol plan-review, P0). EOF alone covered only a child
        # that DIED before acking; a child alive but wedged before its ack held
        # this untimed read -- and therefore the whole single-threaded dispatch
        # loop -- open indefinitely.
        ack = _read_line_blocking(
            result_read, state,
            timeout=_env_seconds("AIRA_AITEST_PLACEMENT_ACK_TIMEOUT", _PLACEMENT_ACK_TIMEOUT_SECONDS),
        )
        if ack != self._PLACED_LINE:
            ack_timed_out = bool(state.get("read_timeout"))
            # The child died (os._exit'd, above, or from fork_worker's own
            # guard) before ever confirming placement -- it never joined
            # its granted cgroup scope. This is a PLACEMENT failure, not a
            # mid-test crash (Task 15's _handle_worker_exit is for a worker
            # that WAS placed and later died) -- release the now-dead
            # admit lease and raise distinctly so the caller does not
            # spend this nodeid's one-and-only crash-retry budget on it.
            if ack_timed_out:
                # Alive but wedged: it must be killed, or closing our pipe ends
                # below leaves an orphan holding a placed grant, and the scope
                # rmdir below would fail with EBUSY forever.
                try:
                    os.kill(pid, signal.SIGKILL)
                except OSError:
                    pass
            os.close(result_read)
            os.close(dispatch_write)
            _reap_child(pid)
            admit_process.stdin.close()
            _terminate_process(admit_process)
            # Mirrors _retire_worker's best-effort scope cleanup (Fable
            # build-review, final gate): the daemon already granted and
            # CreateWorkerScope already made this directory before the
            # child died -- without this, every placement failure leaks
            # one empty scope directory under the outer scope, and the
            # AIRA-36 reaper cannot sweep it until the whole job's
            # supervisor process is gone (>=2 minutes later).
            self._forget_worker_scope(grant["scope"])
            if ack_timed_out:
                # A TIMEOUT is not proof the local cgroup mechanism is broken,
                # which is exactly what WorkerPlacementFailed asserts and what
                # makes _replace_worker strip containment for the rest of the
                # run. We killed a child that had not yet said anything -- an
                # unestablished result, not a diagnosis. Report it as a denial:
                # containment preserved, the surviving pool keeps dispatching,
                # and the existing retry path tries again. A genuinely broken
                # cgroup mechanism still reaches WorkerPlacementFailed below via
                # the EOF path, which is real evidence.
                sys.stderr.write(
                    "aira aitest: worker %d did not confirm cgroup placement within the "
                    "ack timeout and was killed; treating as a transient denial, "
                    "containment preserved\n" % pid
                )
                raise WorkerAdmitDenied(
                    "worker-admit state=denied class=contended reason=placement-ack-timeout "
                    "for worker %d" % pid
                )
            raise WorkerPlacementFailed("worker %d exited before confirming cgroup placement" % pid)


    def _spawn_fallback_worker(self):
        """No admission, no cgroup placement -- reuses worker.py's own
        execution loop with scope_path=None. The already-emitted
        _disable_daemon warning is the ONLY notice; never warn again per
        worker. Wrapped the same way spawn_worker (Task 13) is: the entire
        forked-child branch _exit_child()s (worker.py's coverage-safe
        os._exit wrapper) on any exception rather than ever
        falling through to normal Python control flow, and every OTHER
        already-known worker's fds are closed before this child does
        anything else (same fd-inheritance hazard as spawn_worker -- this
        is a second, independent os.fork() call site with the identical
        fd-table-copy problem)."""
        dispatch_read, dispatch_write = os.pipe()
        result_read, result_write = os.pipe()
        pid = os.fork()
        if pid == 0:
            try:
                self._child_close_other_workers_fds()
                os.close(dispatch_write)
                os.close(result_read)
                pipe_in = os.fdopen(dispatch_read, "r")
                pipe_out = os.fdopen(result_write, "w")
                run_worker_loop(None, self.items_by_nodeid, pipe_in, pipe_out)
            except BaseException:
                _exit_child(70)
            _exit_child(0)
        os.close(dispatch_read)
        os.close(result_write)
        # Same raw-fd, non-blocking treatment as spawn_worker (Task 13) --
        # no placement ack to wait for here (scope_path=None, nothing to
        # place into), so just flip to non-blocking immediately.
        os.set_blocking(result_read, False)
        self.workers[pid] = {
            "grant": None,
            "admit_process": None,
            "dispatch_write": os.fdopen(dispatch_write, "w"),
            "result_fd": result_read,
            "read_buffer": b"",
            "result_eof": False,
            "in_flight": None,
            # AIRA-40, exactly as on the confined path: this fork site needs
            # the independent liveness signal just as much -- a fallback worker
            # runs the same arbitrary test code, so it inherits the same
            # forked-grandchild-holds-the-pipe hazard. Safe on a child that has
            # already died in the microseconds since the fork: it is a zombie,
            # not reaped, so its pid is still valid and cannot have been reused
            # (verified on this host: pidfd_open on a zombie succeeds and is
            # immediately ready).
            "pidfd": self._open_pidfd(pid),
        }
        return pid

    def _dispatch_to_idle_workers(self):
        """CORRECTED by a second review round: a worker that reports its
        result (cleared to idle by _drain_worker) can still die -- crash,
        OOM -- in the gap between that drain and this dispatch, one
        select() wakeup later than the same-pass EOF case the previous
        fix closed. Without a guard here, the write below raises an
        unguarded BrokenPipeError straight out of run(), crashing the
        whole suite and losing every remaining result. list(...) snapshots
        self.workers since _handle_worker_exit (via _retire_worker) can
        mutate it mid-iteration.

        CORRECTED AGAIN (Sol build-review, AIRA-38 review wave): the
        BrokenPipeError branch's _handle_worker_exit call can itself add a
        FRESH replacement worker to self.workers (via _replace_worker) --
        but that new worker is invisible to a `list(...)` snapshot this
        pass already took, so it would never get a nodeid dispatched to it
        THIS pass. If it is the only live worker left (--aitest-workers=1,
        or simply the last one standing), nothing would ever make its
        result_fd ready in run()'s select() loop, since it has nothing in
        flight to report -- the run hangs forever with queue work still
        pending, since run()'s own select-timeout branch never re-calls
        this method on its own (it only re-dispatches after an actual
        drain). Re-scan whenever a pass crashes a worker, so a same-pass
        replacement gets its first nodeid immediately rather than waiting
        on an event that may never arrive.

        SIMPLIFIED (Fable build-review, final gate): an earlier version of
        this fix re-invoked itself recursively, terminated by comparing
        `set(self.workers)` before and after each pass -- correct in every
        practically reachable case, but with two real (if unlikely) edges
        an iterative loop is immune to: a systemic instantly-dying-worker
        pathology could in principle recurse deep enough to hit CPython's
        default 1000-frame limit (bounded only by requeue_once's own
        per-nodeid cap, roughly 2x the remaining queue), and the pid-set
        diff has a pid-reuse blind spot (astronomically rare, but
        structurally avoidable). Looping on "did this pass crash a
        worker" instead of "did the pid set change" removes both -- no
        recursion depth at all, and no dependence on pid identity."""
        while True:
            crashed_this_pass = False
            for pid, state in list(self.workers.items()):
                if state["in_flight"] is not None:
                    continue
                nodeid = self.next_nodeid()
                if nodeid is None:
                    continue
                state["in_flight"] = nodeid
                # Fresh staging buffer per dispatch -- nothing a previous
                # nodeid left behind may ever be replayed against this one.
                state["pending_events"] = []
                try:
                    state["dispatch_write"].write(nodeid + "\n")
                    state["dispatch_write"].flush()
                except BrokenPipeError:
                    # in_flight already correctly holds the nodeid we just
                    # tried to send -- _handle_worker_exit's normal
                    # requeue-once/unevaluated bookkeeping applies exactly
                    # as it does for a worker that dies mid-test.
                    crashed_this_pass = True
                    self._handle_worker_exit(pid, state)
            if not crashed_this_pass:
                return

    def _retire_worker(self, pid, state):
        try:
            state["dispatch_write"].close()
        except BrokenPipeError:
            # close() on a buffered writer flushes first -- if the
            # worker's already dead (found by the same test that caught
            # the dispatch/stop-write gaps above), the flush itself
            # raises. Retirement must proceed regardless; there is
            # nothing left to flush TO.
            pass
        try:
            os.close(state["result_fd"])
        except OSError:
            pass
        # AIRA-40: closing the pidfd here is not mere tidiness. A pidfd is
        # LEVEL-triggered and stays readable for as long as it is open, even
        # after the child has been reaped (verified on this host) -- so a pidfd
        # left open for a retired worker would make every subsequent
        # select() in run() return instantly, spinning the dispatch loop at
        # 100% CPU. Retirement is the single point where a worker leaves
        # self.workers, so it is the single point that must drop this fd.
        pidfd = state.get("pidfd")
        if pidfd is not None:
            state["pidfd"] = None
            try:
                os.close(pidfd)
            except OSError:
                pass
        # AIRA-92 (Sol plan-review, P1): BOUNDED. This sits directly on the
        # dispatch loop -- retirement, recycle and the end-of-run __stop__
        # broadcast all reach it -- so a child that reported its last result and
        # then wedged on the way out (a coverage save, a wedged atexit, an
        # uninterruptible page-fault wait under memory pressure) froze the whole
        # supervisor exactly as a wedged relay did. Its results are already
        # recorded by the time we get here, so escalating to SIGKILL costs
        # nothing and cannot lose data.
        _reap_child(pid)
        if state["admit_process"] is not None:
            state["admit_process"].stdin.close()
            # Bounded, then SIGKILL. A wedged admit-relay process must never
            # abort the whole run over a best-effort wait -- but silently
            # swallowing the timeout left a LIVE relay behind still holding its
            # daemon grant, so the ledger entry outlived the worker it was for.
            _terminate_process(state["admit_process"])
        grant = state.get("grant")
        if grant is not None:
            self._forget_worker_scope(grant["scope"])
        # An earlier retirement whose rmdir failed is still charging the ledger;
        # this is the natural moment to try again.
        self._sweep_unremoved_scopes()
        del self.workers[pid]

    def _forget_worker_scope(self, scope):
        """Remove one worker's cgroup scope, remembering it for retry on failure.

        AIRA-39 made this load-bearing rather than best-effort. The daemon's
        ledger is now SUM(memory.max) over the outer scope's real
        .aira-worker-* children, so a scope that is not removed KEEPS CHARGING
        against this run's budget for the rest of the run -- where the previous
        in-memory ledger released the grant when the relay closed. One EBUSY (a
        reaped worker that left a short-lived descendant behind) would otherwise
        permanently shrink the worker budget, and in the worst case leave
        _wait_for_admission_or_disable retrying forever against capacity that is
        never coming back (found by Sol build-review round 2).
        """
        if not scope:
            return
        try:
            os.rmdir(scope)
        except FileNotFoundError:
            self._unremoved_scopes.discard(scope)
        except OSError as exc:
            self._unremoved_scopes.add(scope)
            sys.stderr.write(
                "aira aitest: could not remove worker scope %s: %s (will retry)\n" % (scope, exc)
            )
        else:
            self._unremoved_scopes.discard(scope)

    def _sweep_unremoved_scopes(self):
        """Re-attempt every removal that failed earlier.

        At most a handful of rmdir calls, and called exactly where a stuck scope
        would otherwise hurt: before retiring another worker, and on every
        admission retry -- so a transient EBUSY self-heals into freed budget
        instead of a permanently smaller pool or a stalled run.
        """
        for scope in sorted(self._unremoved_scopes):
            try:
                os.rmdir(scope)
            except FileNotFoundError:
                self._unremoved_scopes.discard(scope)
            except OSError:
                continue
            else:
                self._unremoved_scopes.discard(scope)

    def _wait_for_admission_or_disable(self, spawn):
        """Retries spawn() (a zero-arg callable performing one admission
        attempt) INDEFINITELY on WorkerAdmitDenied, with a loud periodic
        stderr warning so a stalled run is never silent. Returns on
        success, or on WorkerAdmitUnavailable/WorkerPlacementFailed
        (which _disable_daemon and the caller's own fallback handle).
        WorkerAdmitTerminal (either subclass) deliberately propagates to
        the caller, which marks the affected queue unevaluated rather than
        retrying or silently falling back unconfined. Never returns on
        denial-exhaustion, because there is no such thing here:
        a daemon that stays reachable but saturated forever means the run
        genuinely waits forever, which is the honest outcome, not this
        method's job to silently degrade safety instead.

        Shared by two callers that hit the identical "zero workers, queue
        still has work, no other retirement left to hook a retry off of"
        hazard: run()'s startup path (before any worker has ever been
        admitted) and _replace_worker's "this was the LAST worker"
        path (found by a second review round -- the original fix only
        covered the startup case; retiring the last worker on a plain
        denial left the exact same hazard unaddressed one level later,
        since _dispatch_to_idle_workers never fires and the main loop's
        `while self.workers:` would simply exit with the queue still
        non-empty, dropping every remaining nodeid to unevaluated)."""
        attempt = 0
        while self.daemon_available:
            try:
                spawn()
                return
            except WorkerAdmitDenied:
                pass
            except (WorkerAdmitUnavailable, WorkerPlacementFailed) as exc:
                self._disable_daemon(str(exc))
                return
            attempt += 1
            # A denial may be caused by capacity a failed rmdir is still holding.
            # Retrying it here is what turns "retry forever" back into progress.
            self._sweep_unremoved_scopes()
            time.sleep(_DENIAL_RETRY_SECONDS)
            if attempt % _DENIAL_WARN_EVERY == 0:
                sys.stderr.write(
                    "aira aitest: still waiting for worker admission after %d attempts "
                    "(daemon reachable, budget contended) -- containment preserved, "
                    "not falling back to unconfined\n" % attempt
                )

    def _replace_worker(self):
        """Acquire a fresh worker if queue work remains -- shared by the
        recycle and crash/retry paths.

        WorkerAdmitDenied (the daemon IS reachable, it just declined this
        particular request right now -- budget exhausted or contended)
        leaves daemon_available untouched: simply don't replace this
        worker yet -- UNLESS this was the last worker (self.workers is
        already empty by the time this runs; the caller always retires
        before replacing), in which case there is no other retirement
        left to hook a later retry off of, so wait it out the same way
        run()'s startup path does rather than let the run end with queue
        work still undone.

        WorkerAdmitTerminal is instead a permanent verdict about this
        request (a per-request rejection) or about the channel itself (a
        contract violation): drain the remaining queue to unevaluated
        without disabling the still-healthy daemon or spawning an
        unconfined fallback worker.

        WorkerAdmitUnavailable (no daemon to talk to at all) and
        WorkerPlacementFailed (the cgroup mechanism itself is broken
        locally, not just momentarily busy) both fall back to an
        unconfined worker for the rest of the run -- these are the only
        two failure classes that mean the daemon path is genuinely not
        going to work."""
        if not self.queue:
            return
        if self.daemon_available:
            try:
                # AIRA-64: a replacement made while OTHER workers are still
                # alive is speculative. It used to use the run's full
                # `max_wait` (30s by default), which the daemon honours by
                # POLLING to that deadline -- so under CPU contention, where
                # denials become the common case rather than a rarity, every
                # retirement would have frozen this single-threaded dispatch
                # loop for up to 30 seconds with idle workers waiting for
                # nodeids. The indefinite wait is kept below for the
                # last-worker case, where waiting really is the honest thing
                # to do.
                self.spawn_worker(
                    self._run_estimated_bytes,
                    max_wait=self._run_max_wait if not self.workers else _SPECULATIVE_MAX_WAIT,
                )
                return
            except WorkerAdmitDenied:
                if self.workers:
                    # Another worker is still running; ITS eventual
                    # retirement calls _replace_worker again and retries.
                    return
                try:
                    self._wait_for_admission_or_disable(
                        lambda: self.spawn_worker(self._run_estimated_bytes, max_wait=self._run_max_wait)
                    )
                except WorkerAdmitTerminal as exc:
                    self._fail_queue_terminal(str(exc))
                    return
                if self.daemon_available:
                    return  # the wait succeeded -- a confined worker now exists
                # else: the wait's own WorkerAdmitUnavailable/
                # WorkerPlacementFailed branch already called
                # _disable_daemon -- fall through to the SAME
                # fallback-spawn every other daemon-unavailable path in
                # this function already uses, rather than return here and
                # leave the pool empty with queue work still undone.
            except WorkerAdmitTerminal as exc:
                self._fail_queue_terminal(str(exc))
                return
            except (WorkerAdmitUnavailable, WorkerPlacementFailed) as exc:
                self._disable_daemon(str(exc))
        # Enforce the SAME min(worker_count, max_workers_fallback) TOTAL
        # pool cap run()'s own startup path already enforces (Fable
        # build-review, final gate): this call used to spawn a fallback
        # worker unconditionally on every mid-run retirement once the
        # daemon went unavailable, so a run that had admitted N confined
        # workers before a mid-run daemon crash converged back to N
        # concurrent UNCONFINED workers instead of draining down to the
        # promised cap -- directly contradicting _disable_daemon's own
        # warning text ("falling back to n_workers<=%d").
        if len(self.workers) < min(self._run_worker_count, self.max_workers_fallback):
            self._spawn_fallback_worker()

    def _maybe_grow_pool(self):
        """AIRA-64. One speculative attempt to grow the pool back toward its
        requested size, rate-limited to once a second.

        This exists because the startup pool loop `break`s PERMANENTLY on its
        first denial and nothing else ever tries to grow again: `_replace_worker`
        replaces one worker on a retirement, it does not restore a pool to its
        target size, and the run()'s follow-up wait only fires when the pool is
        completely EMPTY. Before the CPU gate that was harmless, because a RAM
        denial at startup was rare. With a machine-wide CPU bound a denial at
        startup is the NORMAL case on a busy box, so without this a run that got
        1 of its 15 workers would keep exactly one worker for its entire
        lifetime -- a far worse regression than the contention this change
        exists to fix (Sol plan-review).

        It is also what makes the bound FAIR rather than first-come-first-served:
        an incumbent that recycles a worker re-requests immediately from its own
        retirement path, so without a periodic probe from everyone else it would
        simply reclaim its own slot forever while a newcomer sat at its floor.

        Called from every dispatch-loop iteration, not just the idle branch: a
        suite of sub-second tests keeps the loop's descriptors continuously
        ready, so the select() timeout may never be reached at all.

        Returns True when the pool actually grew, so the caller can dispatch to
        the new worker immediately instead of leaving it idle for a tick."""
        if not self.daemon_available or not self.queue:
            return False
        if len(self.workers) >= self._run_worker_count or self._pool_covers_the_queue():
            return False
        now = time.monotonic()
        if now - self._last_growth_probe < _GROWTH_PROBE_INTERVAL_SECONDS:
            return False
        self._last_growth_probe = now
        try:
            self.spawn_worker(self._run_estimated_bytes, max_wait=_SPECULATIVE_MAX_WAIT)
            return True
        except WorkerAdmitDenied:
            # The ordinary outcome of a probe on a busy machine. Silent by
            # design: this fires once a second for the life of a contended run,
            # and a warning per attempt would bury the diagnostics that matter.
            pass
        except WorkerAdmitTerminal as exc:
            # A permanent verdict is NOT made less permanent by having been
            # discovered speculatively; it gets the same treatment it would on
            # any other path rather than being swallowed.
            self._fail_queue_terminal(str(exc))
        except (WorkerAdmitUnavailable, WorkerPlacementFailed) as exc:
            self._disable_daemon(str(exc))
        return False

    def _drain_worker(self, pid, state):
        """Handles every line CURRENTLY AVAILABLE for this worker's result
        pipe in one pass via _drain_available_lines (module-level, defined
        alongside WorkerPlacementFailed), not just the first -- multiple
        already-flushed result lines (e.g. from a burst of fast tests) can
        legitimately be available at once by the time select() wakes the
        caller. Must run to completion for a ready worker BEFORE
        _dispatch_to_idle_workers is called for this select() wakeup. See
        _drain_available_lines's own docstring for why this reads a raw fd
        directly rather than select()-checking a buffered file object.

        A result line ending in worker._RECYCLE_SUFFIX means this worker
        is retiring after this test -- worker.py sends that as PART OF the
        same line as the result, never as a separate later message,
        specifically so this method never has a window where it has
        cleared state["in_flight"] (making the worker look idle to
        _dispatch_to_idle_workers) without ALSO already knowing the worker
        is retiring. An earlier design sent a standalone "__recycle__"
        line after the result; that left a real race (not just a buffering
        artifact -- a genuine scheduling gap between the worker sending its
        result and separately checking+sending recycle) where the
        supervisor could dispatch a fresh nodeid to a worker that had
        already decided, but not yet said, that it was retiring --
        silently losing that dispatch with nothing to detect the loss by.

        EOF (_drain_available_lines set state["result_eof"]) means the
        worker's result pipe closed without a terminating record for its
        in-flight nodeid -- a crash (kernel OOM, host watchdog, any
        non-reporting exit).

        CORRECTED by build-review: a worker can flush its LAST result and
        then crash immediately afterward (e.g. an OOM triggered by
        whatever it does right after reporting, between tests -- spec 3.4
        calls exactly this "a normal, expected event"), so a single
        _drain_available_lines call can return that final result line AND
        set result_eof TOGETHER, in one pass. The original version only
        checked result_eof when `lines` was completely empty, so this case
        fell through: the result got recorded, in_flight was cleared
        (making the worker look idle), and the caller's very next
        _dispatch_to_idle_workers (or a __stop__ broadcast) tried to write
        into the dead worker's dispatch pipe -- an unguarded
        BrokenPipeError that propagated out of run() and lost every
        remaining result, on a crash the spec explicitly requires this
        loop to tolerate gracefully. Checking result_eof AFTER the loop,
        unconditionally, closes this: by then in_flight is already None
        (this result's own outcome, correctly recorded), so
        _handle_worker_exit's nodeid = state["in_flight"] is None and it
        skips straight to retiring and replacing the worker -- no
        double-write to results, just cleanup, exactly like a worker that
        crashed with no trailing result at all."""
        lines = _drain_available_lines(state["result_fd"], state)
        for line in lines:
            if line.startswith(_EVENT_LINE_PREFIX):
                # A Slice 2 event line: STAGE it against the current in-flight
                # nodeid, never replay it here. Nothing from this batch may
                # reach a real pytest hook until (a) the whole batch has been
                # validated/deserialized AND (b) this nodeid's plain result
                # line has been confirmed -- both, not either. Staging is what
                # makes replay crash-atomic: a worker that dies before its
                # result line leaves its staged batch discarded with the
                # worker's own state dict, so a later retry's events can never
                # be replayed twice.
                #
                # An event line with NOTHING in flight, or one whose JSON does
                # not even parse, takes the crash path IMMEDIATELY (v5, Fable's
                # precision fix) rather than being staged for the later batch
                # check to catch: it keeps the crash diagnostic accurate about
                # where things actually went wrong.
                if state["in_flight"] is None:
                    sys.stderr.write(
                        "aira aitest: worker %d sent a report event with no test in flight; "
                        "treating worker as crashed\n" % pid
                    )
                    return self._handle_worker_exit(pid, state)
                try:
                    event = json.loads(line[len(_EVENT_LINE_PREFIX):])
                except ValueError as exc:
                    sys.stderr.write(
                        "aira aitest: worker %d sent an unparseable report event while running %r "
                        "(%s); treating worker as crashed\n" % (pid, state["in_flight"], exc)
                    )
                    return self._handle_worker_exit(pid, state)
                state.setdefault("pending_events", []).append(event)
                continue
            recycling = line.endswith(_RECYCLE_SUFFIX)
            if recycling:
                line = line[: -len(_RECYCLE_SUFFIX)]
            # rpartition, not partition: a parametrized pytest nodeid can
            # legitimately contain a space (e.g. test_p[a b]) -- outcome
            # never does (always one of passed/failed/skipped/error), so
            # splitting on the LAST space is the only correct split.
            # partition() split on the FIRST space instead, truncating any
            # space-bearing nodeid and losing its real result -- found by
            # build-review via live repro against real pytest (both
            # review lineages independently, same root cause and fix).
            nodeid, _, outcome = line.rpartition(" ")
            if nodeid != state["in_flight"]:
                # A corrupted result record must never be attributed to a
                # garbage nodeid. Leave the real in-flight nodeid intact
                # and use the ordinary crash path so it receives its
                # requeue-once/unevaluated treatment instead.
                sys.stderr.write(
                    "aira aitest: worker %d reported result for %r while running %r; treating worker as crashed\n"
                    % (pid, nodeid, state["in_flight"])
                )
                return self._handle_worker_exit(pid, state)
            # BOTH conditions, never either alone: the whole staged batch must
            # validate/deserialize AND this nodeid's plain result line must be
            # confirmed (the check just above) before ANY of it reaches a real
            # pytest hook. Validating the ENTIRE batch first -- rather than
            # dispatching as we parse -- is Sol round-2's requirement: a
            # malformed LATER event in an otherwise-valid batch would otherwise
            # leave earlier events already replayed with no way to un-replay
            # them.
            materialized = self._materialize_events(state.get("pending_events", ()), nodeid)
            if materialized is None:
                sys.stderr.write(
                    "aira aitest: worker %d sent an invalid report batch for %r; "
                    "treating worker as crashed\n" % (pid, nodeid)
                )
                return self._handle_worker_exit(pid, state)
            for event in materialized:
                self._replay_event(event)
            state["pending_events"] = []
            self.results[nodeid] = outcome
            state["in_flight"] = None
            if recycling:
                self._retire_worker(pid, state)
                self._replace_worker()
                return
        if state.get("result_eof"):
            self._handle_worker_exit(pid, state)

    def _materialize_events(self, raw_events, nodeid):
        """Validates and fully deserializes an ENTIRE staged batch, returning
        a list of replayable ("kind", payload) pairs, or None if ANY entry in
        it is unusable. Calls no pytest hook that has an observable effect --
        pytest_report_from_serializable is a pure constructor.

        Returns [] when this Supervisor has no config: with no hook caller
        there is nothing to replay into (Slice 1's own tests construct
        Supervisor() that way), and staging silently degrades to a no-op
        rather than to a fabricated result."""
        if self.config is None:
            return []
        materialized = []
        for raw in raw_events:
            try:
                event = _untag_tuples(raw)
            except Exception:
                return None
            if not isinstance(event, dict):
                return None
            # EVERY event's own nodeid must match in_flight, not just the plain
            # result line's (Sol round-2): a WELL-FORMED event for the WRONG
            # nodeid -- e.g. from the AIRA-40-class inherited-fd surface, where
            # some other forked process holds this pipe's write end -- must go
            # through the same crash path as a malformed one, never be
            # attributed to the test actually running here.
            if event.get("nodeid") != nodeid:
                return None
            kind = event.get("kind")
            if kind in ("logstart", "logfinish"):
                location = event.get("location")
                if not isinstance(location, tuple):
                    return None
                materialized.append((kind, (nodeid, location)))
            elif kind == "report":
                data = event.get("data")
                # pytest's own serialization stamps $report_type
                # (pytest_report_to_serializable). Checking it here catches a
                # malformed or foreign payload earlier and far more precisely
                # than waiting for a downstream AttributeError -- and
                # pytest_report_from_serializable itself returns None for a
                # payload without it, which would be a silent drop.
                if not isinstance(data, dict) or data.get("$report_type") != "TestReport":
                    return None
                try:
                    report = self.config.hook.pytest_report_from_serializable(
                        config=self.config, data=data
                    )
                except Exception:
                    return None
                if report is None or getattr(report, "nodeid", None) != nodeid:
                    return None
                materialized.append(("report", report))
            else:
                return None
        return materialized

    def _replay_event(self, event):
        """Fires ONE already-validated event into the supervisor's own real
        pytest hooks -- the same hooks junitxml and terminalreporter listen on,
        which is the entire point of Slice 2."""
        kind, payload = event
        if kind == "report":
            self.config.hook.pytest_runtest_logreport(report=payload)
            self._replayed_nodeids.add(payload.nodeid)
            return
        nodeid, location = payload
        if kind == "logstart":
            self.config.hook.pytest_runtest_logstart(nodeid=nodeid, location=location)
        else:
            self.config.hook.pytest_runtest_logfinish(nodeid=nodeid, location=location)

    def _synthesize_unevaluated_reports(self):
        """ONE structurally complete final pass, run after the dispatch loop
        finishes -- not a hunt for every call site that can leave a nodeid
        unevaluated (v4's approach, which review found still missed a third
        site).

        Iterating self.items_by_nodeid is MANDATORY, not a style choice (Sol
        round-3): it is the full universe of collected items, so it is the only
        collection that can catch a nodeid missing from self.results ENTIRELY --
        exactly the `results.get(nodeid, "unevaluated")` default case that
        motivated this redesign. Iterating self.results' own keys would leave
        that gap completely intact.

        Report shape: outcome="failed", never the literal string "unevaluated".
        TestReport.outcome is only ever meaningfully passed/failed/skipped to
        junitxml and terminalreporter; an unrecognized outcome is silently
        IGNORED by junitxml's own rendering, which is precisely the
        silently-missing-result failure this exists to prevent. This mirrors
        xdist's own handle_crashitem precedent: a synthetic failure report,
        never a fabricated pass, whenever a real report can never arrive."""
        if self.config is None:
            return
        # pytest.TestReport, the PUBLIC export (verified identical to
        # _pytest.reports.TestReport on the installed version), imported here
        # rather than at module scope so supervisor.py keeps importing cleanly
        # in a context without pytest on the path.
        from pytest import TestReport

        for nodeid, item in self.items_by_nodeid.items():
            if self.results.get(nodeid, "unevaluated") != "unevaluated":
                continue
            if nodeid in self._replayed_nodeids:
                continue
            reason = self._unevaluated_reasons.get(
                nodeid, "no worker ever reported a result for it"
            )
            location = getattr(item, "location", None)
            if not isinstance(location, tuple) or len(location) != 3:
                fspath = nodeid.split("::")[0]
                location = (fspath, None, nodeid)
            now = time.time()
            report = TestReport(
                nodeid=nodeid,
                location=location,
                keywords={name: 1 for name in getattr(item, "keywords", ())},
                outcome="failed",
                longrepr="unevaluated: %s" % reason,
                # when="call" is a deliberate choice, verified against real
                # junitxml output: it renders as <failure message="unevaluated:
                # ..."/>, whereas a synthetic non-call phase renders as
                # <error message='failed on setup with "..."'/> -- text that
                # would be actively untrue here, since setup was never even
                # reached.
                when="call",
                sections=[],
                duration=0.0,
                # --durations and junit's own time= attribute read start/stop
                # directly; leaving them at the constructor's 0 default is a
                # small but real, easily-avoided fidelity gap.
                start=now,
                stop=now,
                user_properties=[],
            )
            self.config.hook.pytest_runtest_logstart(nodeid=nodeid, location=location)
            self.config.hook.pytest_runtest_logreport(report=report)
            self.config.hook.pytest_runtest_logfinish(nodeid=nodeid, location=location)
            self._replayed_nodeids.add(nodeid)

    def _handle_worker_exit(self, pid, state):
        """A worker stopped reporting without a terminating record for its
        in-flight nodeid: a crash (kernel OOM, host watchdog, any non-reporting
        exit). Requeue once; a second failure here is unevaluated -- distinct
        from failed everywhere results are aggregated, never silently folded
        into either outcome.

        Reached from TWO independent detections since AIRA-40, and the second
        is the authoritative one: the result pipe reaching EOF, and the
        worker's own pidfd going ready in run()'s select() set. EOF is an
        INFERENCE about the process from the state of a pipe, and it is
        unsound in one direction -- a grandchild the test itself forked can
        hold a duplicate write end open past the worker's death, so EOF never
        arrives (see _open_pidfd). The pidfd observes the process itself, so it
        cannot be held open by a third party. EOF is kept because it is
        genuinely sufficient in the overwhelmingly common case, arrives at the
        same instant there, and is all there is on a host too old for
        pidfds."""
        nodeid = state["in_flight"]
        # BEFORE _retire_worker, which removes the scope directory
        # (_forget_worker_scope's os.rmdir): once it is gone the evidence for
        # WHY this worker died is gone with it.
        death = self._describe_worker_death(pid, state)
        self._retire_worker(pid, state)
        if nodeid is not None and not self.requeue_once(nodeid):
            self.results[nodeid] = "unevaluated"
            self._unevaluated_reasons.setdefault(nodeid, death)
        self._replace_worker()

    def _describe_worker_death(self, pid, state):
        """Why this worker stopped reporting, in the terms an operator can act
        on -- read from the worker scope's own memory.events BEFORE the scope
        is removed.

        AIRA-35 makes this necessary rather than decorative. Until it landed,
        nothing capped a worker's swap, so a worker over its memory.max was
        reclaimed into swap and its test PASSED; with memory.swap.max=0 that
        same worker is now group-killed, requeued once, and reported
        unevaluated. Turning a silent pass into an unevaluated is correct --
        it is the containment this product claims -- but only if the report
        says which limit was hit and which knob raises it. The per-worker cap
        is a flat AIRA_AITEST_ESTIMATED_BYTES (512 MiB by default; per-suite
        history-based sizing is still deferred), so the remedy is a single
        environment variable.

        Attribution is sound because memory.events counters are per-scope and
        propagate only upward: oom_group_kill > 0 on THIS scope means THIS
        scope's own memory.oom.group fired, not an ancestor's.

        Every uncertainty falls back to the generic sentence rather than
        guessing: a worker with no grant (a containment-stripped fallback
        worker has none), an already-removed or unreadable scope, and a kernel
        whose memory.events omits oom_group_kill all reach it. A fabricated OOM
        claim would be worse than a vague true one."""
        generic = (
            "worker %d stopped reporting while running it, and the one "
            "retry did too" % pid
        )
        grant = state.get("grant")
        if not grant:
            return generic
        scope = grant.get("scope")
        if not scope:
            return generic
        try:
            # errors="replace": a decode failure must not escape as a
            # UnicodeDecodeError (a ValueError, which OSError does not catch)
            # and take the dispatch loop down with it. Unreachable on today's
            # kernels, which is exactly why it would never be found later --
            # this function's contract is that EVERY uncertainty falls back to
            # the generic sentence, and an exception is not a fallback.
            with open(
                os.path.join(scope, "memory.events"), encoding="ascii", errors="replace"
            ) as handle:
                events = handle.read()
        except OSError:
            return generic
        killed = False
        for line in events.splitlines():
            parts = line.split()
            if len(parts) == 2 and parts[0] == "oom_group_kill":
                try:
                    killed = int(parts[1]) > 0
                except ValueError:
                    return generic
                break
        else:
            return generic
        if not killed:
            return generic
        return (
            "worker %d was killed by its own per-worker memory cap "
            "(memory.max=%s bytes; raise AIRA_AITEST_ESTIMATED_BYTES), and the "
            "one retry was too" % (pid, grant.get("memory_max", "unknown"))
        )

    def _service_ready_workers(self, ready, result_fd_owners, pidfd_owners):
        """Service one select() wakeup: drain every ready result pipe, THEN
        handle every worker whose pidfd says it has exited (AIRA-40).

        Both maps are {fd: (pid, state)} as run() built them for its select()
        call, taken before any of this ran.

        What is load-bearing is DRAIN BEFORE CONCLUDING DEATH, not the order of
        the two passes as such. A worker that flushes its final result line and
        dies immediately afterwards produces a real, already-established
        outcome that must be recorded, never overwritten by a crash verdict --
        so the exit branch drains that worker itself before acting, and would
        remain correct even if these two loops were swapped. Splitting them is
        what makes "results first" well defined without depending on select()'s
        ready-list order at all. CPython does in fact return that list in the
        order the fds were passed (selectmodule.c's set2list walks its fd2obj
        array, not the fd_set; measured on this host, 3.12.3, by passing ready
        fds in descending order and getting them back in descending order) --
        but that is an implementation detail POSIX does not promise, and
        iterating the two owner maps rather than `ready` means this code never
        has to care either way. An earlier revision of this docstring asserted
        the opposite ("ascending fd order") and used it as the justification for
        the split; the split is right, that reason was not.

        The exit branch's own drain is therefore not redundant with the first
        pass, and is the only thing covering the commonest version of this
        race: select()'s answer is a SNAPSHOT, so a worker's last bytes can
        land after it returned and before this runs, leaving that fd absent
        from `ready` entirely. _drain_available_lines is non-blocking, so
        draining an fd with nothing on it costs one EAGAIN read. If that drain
        finds EOF it takes the crash path itself, which is why what follows
        re-checks the worker is still registered.

        Every entry is re-checked by state-dict IDENTITY (`is`), never by pid
        or fd equality, because a pass can retire a worker and _replace_worker
        can fork a fresh one within this same call -- and both a pid and an fd
        NUMBER can be reused by that replacement (astronomically unlikely for a
        pid, ordinary for an fd). Identity cannot confuse the two; equality on
        a recycled number can, and would service, or worse crash-handle, a
        brand-new healthy worker. Same pid-reuse blind spot
        _dispatch_to_idle_workers' own docstring records designing out."""
        ready = set(ready)
        for fd, (pid, state) in result_fd_owners.items():
            if fd not in ready or self.workers.get(pid) is not state:
                continue
            self._drain_worker(pid, state)
        for fd, (pid, state) in pidfd_owners.items():
            if fd not in ready or self.workers.get(pid) is not state:
                continue
            self._drain_worker(pid, state)
            if self.workers.get(pid) is state:
                # The tracked pid is gone but its pipe did not say so -- the
                # AIRA-40 case exactly. Treat it as the crash it is, through
                # the ordinary requeue-once/unevaluated path, rather than
                # waiting on an EOF that a surviving grandchild may never let
                # the kernel deliver.
                self._handle_worker_exit(pid, state)

    def _pool_covers_the_queue(self):
        """True when every nodeid still waiting in the queue already has an
        idle worker to run it, so admitting/forking another one would buy
        nothing.

        AIRA-37 residue #2. run()'s two startup spawn loops guarded only on
        `if not self.queue`, but nothing decrements the queue until the LATER
        _dispatch_to_idle_workers() pass -- the pool is spawned first and
        dispatched to second. So one collected test with --aitest-workers=4
        admitted and forked four workers, three of which never received a
        nodeid and simply sat there until the end-of-run __stop__ broadcast.

        That is not only wasted fork+admission overhead. On the confined path
        each of those surplus workers holds a real daemon grant against this
        run's aggregate memory ledger (AIRA-39: the ledger sums memory.max over
        the outer scope's real children) for its entire useless lifetime, so
        needless over-spawn makes hitting the aggregate cap -- and stalling in
        _wait_for_admission_or_disable waiting for capacity this run is itself
        holding -- more likely than the requested pool size warrants.

        Counting IDLE workers rather than all of them (or keeping a local
        counter decremented per spawn, the other shape AIRA-37 suggested) makes
        this correct at any call site rather than only in a context where
        nothing is in flight yet: a busy worker is not cover for a queued
        nodeid, and derived-from-live-state cannot drift out of step with the
        queue the way a separate counter can.

        The `_UNKNOWN` default is deliberate and directional (build-review): a
        state dict that has not reached its final shape must count as NOT idle.
        A plain `.get("in_flight")` would return None for a missing key and
        therefore count such a worker as cover for a queued nodeid -- claiming
        capacity that was never established, which is the one direction this
        function must not fail in. Inert today (both registration sites set
        in_flight before publishing the state), and it stays inert by
        construction rather than by convention."""
        idle = sum(
            1 for state in self.workers.values()
            if state.get("in_flight", _UNKNOWN) is None
        )
        return idle >= len(self.queue)

    def run(self, estimated_bytes, worker_count=1, max_wait="30s"):
        """Slice 1's whole dispatch loop: spawn up to worker_count workers,
        pull-dispatch the queue to whichever are idle, collect results via
        select() over each worker's result pipe, until the queue is drained
        and every worker has retired. Recycle (Task 14), crash/retry (Task
        15), and daemon-down fallback (Task 16) extend this method in
        place."""
        self.bootstrap()
        import gc
        gc.freeze()
        self._run_estimated_bytes = estimated_bytes
        self._run_max_wait = max_wait
        self._run_worker_count = worker_count
        if self.daemon_available:
            for _ in range(worker_count):
                if self._pool_covers_the_queue():
                    break
                try:
                    self.spawn_worker(estimated_bytes, max_wait=max_wait)
                except WorkerAdmitDenied:
                    # Contended/no budget RIGHT NOW -- the daemon is still
                    # there. Stop trying to grow the pool this instant and
                    # start dispatching to however many DID get admitted;
                    # a later retirement's _replace_worker tries for more.
                    break
                except WorkerAdmitTerminal as exc:
                    self._fail_queue_terminal(str(exc))
                    break
                except (WorkerAdmitUnavailable, WorkerPlacementFailed) as exc:
                    self._disable_daemon(str(exc))
                    break
            # A denied/contended daemon must never silently strip
            # containment (denied != unavailable, above) -- but with ZERO
            # workers admitted yet there is also no later retirement to
            # hook a retry off of (_replace_worker only fires when an
            # EXISTING worker retires). Wait it out via the same shared,
            # INDEFINITELY-retrying-on-denial helper _replace_worker uses
            # for its own "last worker" case -- see
            # _wait_for_admission_or_disable's docstring for the full
            # rationale (spec 3.7; a daemon that stays saturated forever
            # means this run genuinely waits forever, which is the honest
            # outcome, never a silent degrade to unconfined).
            if self.daemon_available and not self.workers and self.queue:
                try:
                    self._wait_for_admission_or_disable(lambda: self.spawn_worker(estimated_bytes, max_wait=max_wait))
                except WorkerAdmitTerminal as exc:
                    self._fail_queue_terminal(str(exc))
        if not self.daemon_available:
            # Cap TOTAL concurrent workers (already-admitted + fallback) at
            # the configured pool size -- min(worker_count,
            # max_workers_fallback), never NumCPU regardless of what
            # --aitest-workers actually asked for, and never on top of
            # whatever got admitted before the daemon was marked
            # unavailable mid-startup-loop.
            remaining_pool = max(0, min(worker_count, self.max_workers_fallback) - len(self.workers))
            for _ in range(remaining_pool):
                if self._pool_covers_the_queue():
                    break
                self._spawn_fallback_worker()
        self._dispatch_to_idle_workers()
        while self.workers:
            result_fd_owners = {
                state["result_fd"]: (pid, state) for pid, state in self.workers.items()
            }
            # AIRA-40: worker liveness, watched independently of the result
            # pipe and in the SAME select() set. A worker with no pidfd (an
            # unsupported host, see _open_pidfd) simply contributes nothing
            # here and keeps today's EOF-only behaviour.
            pidfd_owners = {
                state["pidfd"]: (pid, state)
                for pid, state in self.workers.items()
                if state.get("pidfd") is not None
            }
            # ACCEPTED, NAMED GAP (build-review, P3): select() refuses any fd
            # number >= FD_SETSIZE (1024) with a ValueError that would
            # propagate out of run() and lose the whole suite. That vector
            # predates AIRA-40, but this change does roughly double this
            # loop's fd consumption, so it is written down rather than left
            # implicit. Not fixed here: it needs the loop moved onto
            # selectors.DefaultSelector (epoll, no such limit), which this
            # module already uses for its two bounded single-fd reads, and
            # that is a hot-loop rewrite well outside a liveness fix. Reaching
            # it at all requires the pytest process to hold >1024 fds, since
            # the limit is on the NUMBER, not the count passed here.
            ready, _, _ = select.select(
                list(result_fd_owners) + list(pidfd_owners), [], [], 1.0
            )
            # AIRA-64: BEFORE the `if not ready: continue` below, so it runs on
            # every iteration rather than only on the idle branch. Its own rate
            # limit is what bounds the cost; attaching it to the timeout instead
            # made it unreachable for any suite whose tests finish in under a
            # second (Sol plan-review).
            if self._maybe_grow_pool():
                # Feed the fresh worker now rather than leaving it idle until
                # the next event: on the `not ready` path below there may not
                # BE a next event for a full tick.
                self._dispatch_to_idle_workers()
            if not ready:
                continue
            self._service_ready_workers(ready, result_fd_owners, pidfd_owners)
            self._dispatch_to_idle_workers()
            if not self.queue and all(state["in_flight"] is None for state in self.workers.values()):
                for pid, state in list(self.workers.items()):
                    try:
                        state["dispatch_write"].write(_STOP_LINE + "\n")
                        state["dispatch_write"].flush()
                    except BrokenPipeError:
                        pass  # already dead -- nothing to signal, retire below regardless
                    self._retire_worker(pid, state)
        self._cleanup_supervisor_scope()
        # ONE structurally complete pass, after every other path has had its
        # say -- see _synthesize_unevaluated_reports for why this is a single
        # post-run pass over items_by_nodeid rather than per-call-site fixes.
        self._synthesize_unevaluated_reports()
        return self.results

    def _cleanup_supervisor_scope(self):
        """Best-effort: rmdir the supervisor's OWN child scope this run
        relocated itself into (bootstrap, Task 2/3/11). The OUTER scope
        itself is `aira confine`'s own job to tear down when the whole
        launch process exits -- this is only about the new child scope
        aitest itself created. NOTE: in the real-cgroup case this
        typically still fails here (EBUSY) since the supervisor process
        calling rmdir is itself still a live member of the scope it is
        trying to remove -- it only ever succeeds AFTER this process
        exits, which is after this call returns. Attempted anyway because
        it is free and occasionally correct (e.g. non-real-cgroup test
        doubles).

        #72's orphaned-scope reaper IS the real backstop for the nested
        case this rmdir usually can't finish itself (a crashed
        supervisor/worker -- spec 3.6's normal, expected death-by-OOM
        path, where nothing here runs -- leaves live-then-dead child
        scopes under the outer scope). This was a real, confirmed gap
        for a while (the reaper only did a single-level rmdir, which the
        kernel refuses on a cgroup with live children, so nested orphans
        accumulated unbounded) -- fixed and deployed as AIRA-36
        (reapEmptyConfineScopeTree, internal/runner/confine_manage_linux.go,
        master 826f33b): whole-subtree-empty positive-proof-gated,
        fd-anchored, never touches a scope with a live worker anywhere
        in its subtree. Live-verified sweeping this exact nested shape.
        """
        if not self.supervisor_scope:
            return
        try:
            os.rmdir(self.supervisor_scope)
        except OSError as exc:
            # EBUSY here is the EXPECTED outcome documented above (found live via
            # fastest-ee-dc dogfooding, 2026-09-02: even with cgroup.procs already
            # empty, cgroup-v2 destruction is not synchronous with the last process
            # leaving -- the kernel's own deferred css-offline accounting can hold
            # the directory busy for a brief settling window after this call, well
            # before the #72/AIRA-36 reaper's grace period would ever consider it
            # orphaned). Printing an alarming "could not remove" line for this on
            # EVERY real-cgroup run trains users to ignore aitest's stderr output
            # entirely, which is exactly the failure mode this project's honesty
            # discipline exists to prevent for messages that ARE diagnostic. Any
            # OTHER errno is still surfaced -- that is genuinely unexpected.
            if exc.errno != errno.EBUSY:
                sys.stderr.write("aira aitest: could not remove supervisor scope %s: %s\n" % (self.supervisor_scope, exc))
