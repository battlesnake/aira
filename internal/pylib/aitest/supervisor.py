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

    It was called WorkerAdmitRequestInvalid, which AIRA-45 recorded as an
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

_OUTCOME_GRANT_FIELDS = ("scope", "worker_id", "memory_max", "memory_high")


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
        self.daemon_available = True
        self.max_workers_fallback = max(1, int(os.environ.get("AIRA_AITEST_MAX_WORKERS_FALLBACK", "1")))
        self._fallback_warned = False
        self._admission_terminal_warned = False
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
        for token in result.stdout.split():
            if token.startswith("outer="):
                self.outer_scope = token[len("outer="):]
            elif token.startswith("supervisor_scope="):
                self.supervisor_scope = token[len("supervisor_scope="):]
        if not self.outer_scope:
            self._disable_daemon("aitest-bootstrap did not report an outer scope")

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
        grant = {key: outcome[key] for key in _OUTCOME_GRANT_FIELDS if key in outcome}
        missing = [key for key in _OUTCOME_GRANT_FIELDS if key not in grant]
        malformed = None
        if missing:
            malformed = "missing required field%s: %s" % (
                "s" if len(missing) != 1 else "", ", ".join(missing)
            )
        else:
            for key in ("memory_max", "memory_high"):
                try:
                    value = int(grant[key])
                except ValueError as exc:
                    malformed = "%s must be a positive integer (got %r: %s)" % (key, grant[key], exc)
                    break
                if value <= 0:
                    malformed = "%s must be a positive integer (got %r)" % (key, grant[key])
                    break
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
        return grant, process

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
        dispatch_read, dispatch_write = os.pipe()
        result_read, result_write = os.pipe()
        pid, in_child = fork_worker(grant["scope"])
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
                pipe_out.write(self._PLACED_LINE + "\n")
                pipe_out.flush()
                run_worker_loop(grant["scope"], self.items_by_nodeid, pipe_in, pipe_out)
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
        os.set_blocking(result_read, False)
        state.update({
            "grant": grant,
            "admit_process": admit_process,
            "dispatch_write": os.fdopen(dispatch_write, "w"),
            "in_flight": None,
        })
        self.workers[pid] = state
        return pid

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
                self.spawn_worker(self._run_estimated_bytes, max_wait=self._run_max_wait)
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
        """A worker's result pipe hit EOF without a terminating record for
        its in-flight nodeid: a crash (kernel OOM, host watchdog, any
        non-reporting exit). Requeue once; a second failure here is
        unevaluated -- distinct from failed everywhere results are
        aggregated, never silently folded into either outcome."""
        nodeid = state["in_flight"]
        self._retire_worker(pid, state)
        if nodeid is not None and not self.requeue_once(nodeid):
            self.results[nodeid] = "unevaluated"
            self._unevaluated_reasons.setdefault(
                nodeid,
                "worker %d stopped reporting while running it, and the one "
                "retry did too" % pid,
            )
        self._replace_worker()

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
                if not self.queue:
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
                if not self.queue:
                    break
                self._spawn_fallback_worker()
        self._dispatch_to_idle_workers()
        while self.workers:
            fd_to_pid = {state["result_fd"]: pid for pid, state in self.workers.items()}
            ready, _, _ = select.select(list(fd_to_pid), [], [], 1.0)
            if not ready:
                continue
            for fd in ready:
                pid = fd_to_pid[fd]
                if pid not in self.workers:
                    continue
                self._drain_worker(pid, self.workers[pid])
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
