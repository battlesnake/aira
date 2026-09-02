import os
import select
import subprocess
import sys
import time

from aitest.worker import _RECYCLE_SUFFIX, fork_worker, run_worker_loop


_STOP_LINE = "__stop__"
_DENIAL_RETRY_SECONDS = 1.0
# How often (in retry attempts, i.e. roughly every N seconds at the above
# interval) to remind stderr that a run is stalled waiting on a reachable
# but saturated daemon -- see _wait_for_admission_or_disable, shared by
# run()'s startup path and _replace_worker's "this was the last worker"
# path. A stuck run must never be SILENT, even though it must also never
# fall back to unconfined just because the wait is long.
_DENIAL_WARN_EVERY = 30


class WorkerAdmitUnavailable(Exception):
    """The daemon is genuinely unreachable at the connection level: a dial
    failure, the worker-admit CLI itself could not even be launched, or its
    response was malformed/garbage -- there is no daemon to talk to. This
    is the ONLY failure class that should ever disable daemon-backed
    admission for the rest of the run (_disable_daemon, Task 16)."""
    pass


class WorkerAdmitDenied(Exception):
    """The daemon IS reachable and responded normally with "denied" (budget
    genuinely exhausted right now), "timeout" (the request waited out its
    full window -- the daemon is just busy/contended, not down), or
    "unevaluated" (a live memory read failed on an otherwise-reachable
    daemon -- AIRA-38: treated as retriable, not as daemon-unavailable,
    since the daemon plainly answered, it just could not establish a
    result this instant). Means "don't add a worker at this moment", never
    "abandon containment for the rest of the run"."""
    pass


class WorkerAdmitRequestTooLarge(Exception):
    """A PERMANENT, STATIC verdict about this specific request that no
    amount of retrying or waiting can change -- either the daemon's own
    reject:exceeds-ceiling (this request's estimated-byte sizing can
    never fit under the outer scope's cap, even without transient
    contention) or the CLI's own pre-dial E_CONFINE_ARGUMENT_INVALID
    rejection (a malformed client argument, e.g. an estimated_bytes value
    below the daemon's floor -- Fable build-review, final gate: this used
    to fall through to WorkerAdmitUnavailable, misdiagnosing a purely
    local, static client mistake as the daemon being unreachable and
    stripping containment for the rest of the run). Retrying would wait
    forever (or dial a perfectly healthy daemon) with zero chance of
    success, while falling back to an unconfined worker would silently
    remove RAM containment for the rest of the run. It is instead a
    terminal failure for the affected queued work only, which is marked
    unevaluated without declaring the daemon unavailable."""
    pass


class WorkerPlacementFailed(Exception):
    """place_self() never completed (or its child-side placement ack never
    arrived) -- the forked child died before we could confirm it actually
    joined its granted cgroup scope. Distinct from a worker that WAS placed
    and crashed later mid-test (Task 15's _handle_worker_exit path): a
    placement failure means the admitted grant was never even used for a
    test."""
    pass


def _read_line_blocking(fd, state):
    """Blocking read of exactly one line from a raw fd, used only for the
    one-time post-fork placement-ack wait (spawn_worker/
    _spawn_fallback_worker) -- blocking IS the desired behaviour there.
    Shares state["read_buffer"] with _drain_available_lines below (both
    read the SAME fd over the worker's lifetime) so no byte a single
    os.read() call happens to over-read past this line's newline is ever
    lost to a later caller."""
    buf = state.get("read_buffer", b"")
    while b"\n" not in buf:
        chunk = os.read(fd, 65536)
        if not chunk:
            state["result_eof"] = True
            break
        buf += chunk
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

    def __init__(self):
        self.queue = []
        self.attempts = {}  # nodeid -> attempt count (Task 15's retry-once rule)
        self.outer_scope = None
        self.supervisor_scope = None
        self.daemon_available = True
        self.max_workers_fallback = max(1, int(os.environ.get("AIRA_AITEST_MAX_WORKERS_FALLBACK", "1")))
        self._fallback_warned = False
        self._admission_too_large_warned = False
        self.items_by_nodeid = {}
        self.workers = {}
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

    def _fail_queue_too_large(self, reason):
        """Mark queued work unevaluated after a permanent daemon sizing
        rejection, preserving daemon availability and therefore never
        triggering an unconfined fallback for this condition."""
        if not self._admission_too_large_warned:
            self._admission_too_large_warned = True
            sys.stderr.write(
                "aira aitest: %s -- remaining queued tests cannot be admitted at this sizing; "
                "marking them unevaluated rather than waiting forever or running unconfined\n" % reason
            )
        while self.queue:
            nodeid = self.queue.pop(0)
            self.results.setdefault(nodeid, "unevaluated")

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
        Raises WorkerAdmitUnavailable when there is no daemon to talk to at
        all (dial/launch failure, malformed response); raises
        WorkerAdmitDenied when the daemon responded normally but declined
        transiently (denied or timeout), and WorkerAdmitRequestTooLarge
        when reject:exceeds-ceiling makes this sizing permanently
        inadmissible. The caller MUST treat these differently (Task 16):
        only WorkerAdmitUnavailable may disable daemon-backed admission
        for the rest of the run."""
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
            raise WorkerAdmitUnavailable(str(exc))
        # "replace", not "strict" (Sol build-review, AIRA-38 review wave): a
        # corrupted/truncated write or a stray binary byte from a
        # misbehaving relay build must degrade to a malformed, unrecognized
        # line (falling through to WorkerAdmitUnavailable below, the
        # documented "malformed response" treatment) rather than raise an
        # uncaught UnicodeDecodeError that crashes the whole pytest process.
        line = process.stdout.readline().decode("utf-8", "replace").strip()
        if not line.startswith("granted "):
            stderr = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            try:
                process.wait(timeout=5)
            except Exception:
                process.kill()
                process.wait()  # reap immediately rather than leave a zombie
            message = line or stderr or "worker-admit exited without a grant"
            # RequestWorkerAdmit (Task 8) wraps a daemon-reachable-but-
            # declined response as "worker-admit <state>: <reason>" on
            # stderr -- ANYTHING else (a dial failure, a launch failure, a
            # malformed response) means there is no daemon to talk to at
            # all. This distinction is load-bearing (Task 16).
            #
            # "worker-admit unevaluated" (AIRA-38) is included here
            # deliberately, not an oversight: evaluateWorkerAdmit returns
            # State="unevaluated" when the daemon IS reachable but a live
            # memory.current read (outer or supervisor scope) failed --
            # AIRA's own rule that a check which cannot establish its
            # result reports unevaluated rather than a fake pass/fail
            # (never "no daemon at all"). A single transient read glitch on
            # an otherwise-live daemon must not permanently strip
            # containment for the rest of the run, so this is classified
            # exactly like a plain denial: retriable, not terminal -- EXCEPT
            # for the specific "unbounded" reason handled separately just
            # below, which is a structural fact, not a transient glitch.
            # "worker-admit local-placement-failed" (AIRA-38, Sol build-
            # review): runWorkerAdmitCommand prints this specific marker
            # when the DAEMON already granted admission (reachable,
            # healthy) but the LOCAL cgroup scope creation then failed --
            # distinct from every state above, which all describe the
            # daemon's own admission verdict. Classified as
            # WorkerPlacementFailed (same fallback behavior as
            # WorkerAdmitUnavailable downstream, per _replace_worker's own
            # docstring) rather than the generic unavailable bucket, so a
            # future reader of this diagnostic is not misled into
            # suspecting the daemon itself.
            if "worker-admit local-placement-failed" in message:
                raise WorkerPlacementFailed(message)
            # "worker-admit unevaluated: unbounded" (Fable re-gate) is a
            # DIFFERENT case from the generic "unevaluated" handled below,
            # deliberately NOT folded into it: it means the OUTER scope's
            # own memory.max read came back "unbounded" (readSliceMemory,
            # admit.go) -- and a genuine confine-launched outer scope is
            # ALWAYS given a finite memory.max by the daemon as part of the
            # SAME atomic admission grant that launches it, before it is
            # ever queryable (AIRA-27/67). An "unbounded" outer scope is
            # therefore not a transient read glitch retrying could fix --
            # it is a structural sign the discovered "outer_scope" is not
            # a real, daemon-admitted confine scope at all. Found live: a
            # SECOND aitest-enabled pytest invocation inside one
            # --delegate-ram confine job (e.g. `make test` running pytest
            # twice) can discover a PRIOR run's own uncapped
            # .aira-supervisor scope as its "outer", since that prior run's
            # own controlling process (make, a shell) was itself drained
            # into that scope during ITS bootstrap (drainIntoScope moves
            # EVERY pid in outer, not just the supervisor's own pid --
            # aitest_bootstrap_linux.go). Without this check, that
            # structural-not-transient "unevaluated" was classified
            # retriable, and _wait_for_admission_or_disable retried
            # INDEFINITELY under a misleading "budget contended" warning --
            # a genuine, deterministic hang. Classified WorkerAdmitUnavailable
            # instead (not retriable): this run's workers still end up
            # hierarchically bounded by the REAL outer confine job's own
            # cap either way -- cgroup memory limits enforce down the whole
            # tree regardless of what an uncapped descendant itself
            # reports -- so falling back is safe, and it is strictly better
            # than hanging forever against a scope that will never become
            # capped. The deeper root cause (bootstrap discovering the
            # wrong scope when nested) is a separate, tracked gap -- this
            # is the safety net that keeps it from hanging the run.
            if "worker-admit unevaluated" in message and "unbounded" in message:
                raise WorkerAdmitUnavailable(message)
            # "E_CONFINE_ARGUMENT_INVALID" (Fable build-review, final
            # gate): the CLI's own pre-dial argument validation (e.g. the
            # --estimated-bytes 1MiB floor, AIRA-38) rejects BEFORE ever
            # talking to the daemon -- a permanent, static fact about
            # THIS request's arguments, not a daemon condition at all.
            # _resolve_estimated_bytes (aitest/__init__.py) now clamps
            # the one user-facing knob that could realistically trigger
            # this before it ever reaches the wire, but this classifies
            # it correctly regardless -- ANY future CLI argument
            # validation failure reaching here must never be conflated
            # with genuine daemon unavailability.
            #
            # "E_DAEMON_PROTOCOL" (Fable re-gate): the DAEMON's own
            # protocol-level argument validation (validateWorkerAdmitArgs,
            # worker_admit.go -- e.g. estimated_bytes above admitMaxReserve,
            # the mirror-image case of the CLI floor above) is likewise a
            # permanent, static fact about THIS request, discovered one
            # hop further along but still never a reason to distrust the
            # daemon itself. _resolve_estimated_bytes now clamps the top
            # end too, but as with the floor, this classifies it correctly
            # regardless of whether some future value or field slips past
            # that clamp.
            # "worker-admit denied: reject:*" (Fable re-gate) generalizes
            # what used to be a single hand-picked substring
            # ("reject:exceeds-ceiling" only) to the daemon's own designed
            # "reject:" (permanent) vs "fallback:" (transient) reason-prefix
            # convention (worker_admit.go's evaluateWorkerAdmit/
            # workerAdmitConnection: EVERY "denied" reason starting
            # "reject:" -- exceeds-ceiling, outer-scope-owned-by-another-
            # job, and any future one -- deliberately breaks the daemon's
            # OWN poll loop immediately as a stable "never going to
            # resolve" fact, exactly like exceeds-ceiling; only
            # "fallback:"-prefixed reasons keep polling). The ownership
            # rejection was previously left out, matched only "worker-admit
            # denied" below, and was retried INDEFINITELY against a
            # permanently (by design -- workerJobFor's own ownership
            # binding is never released) impossible request, under a
            # misleading "budget contended" warning. Scoped to "worker-admit
            # denied" specifically, NOT "worker-admit timeout": a timeout's
            # own reason text ("reject:saturated") also contains "reject:"
            # by coincidental wording, but state=timeout means the CLIENT's
            # own wait budget merely expired -- genuinely retriable with a
            # fresh request, never a stable daemon-side verdict.
            if (
                ("worker-admit denied" in message and "reject:" in message)
                or "E_CONFINE_ARGUMENT_INVALID" in message
                or "E_DAEMON_PROTOCOL" in message
            ):
                raise WorkerAdmitRequestTooLarge(message)
            if "worker-admit denied" in message or "worker-admit timeout" in message or "worker-admit unevaluated" in message:
                raise WorkerAdmitDenied(message)
            raise WorkerAdmitUnavailable(message)
        grant = {}
        for field in line[len("granted "):].split():
            key, _, value = field.partition("=")
            grant[key] = value
        required = ("scope", "worker_id", "memory_max", "memory_high")
        missing = [key for key in required if key not in grant]
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
            # A malformed grant means the relay cannot be trusted as a
            # daemon-backed admission. Release it exactly as for every
            # other non-usable response, rather than leaving its lease and
            # pipes around until the supervisor later falls back.
            #
            # stdin MUST close BEFORE the blocking stderr read below (Sol
            # build-review, AIRA-38 review wave): per runWorkerAdmitCommand's
            # own confirmed contract, once "granted" is printed the CLI
            # relay blocks on ITS OWN stdin reaching EOF before it exits
            # (or writes anything further to stderr) -- reading stderr
            # first deadlocks unconditionally against a process that will
            # never close its own stderr until stdin closes.
            try:
                process.stdin.close()
            except BrokenPipeError:
                pass
            stderr = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            try:
                process.wait(timeout=5)
            except Exception:
                process.kill()
                process.wait()  # reap immediately rather than leave a zombie
            # Mirrors _retire_worker's best-effort scope cleanup (Fable
            # build-review, final gate): a malformed grant can still name
            # a real, already-created scope (e.g. a bad memory_max value
            # alongside a fine scope path) -- "scope" itself may be one of
            # the missing fields, so this is guarded, unlike the
            # unconditional rmdir in spawn_worker's placement-failure path
            # where "grant" is always fully well-formed by construction.
            scope = grant.get("scope")
            if scope:
                try:
                    os.rmdir(scope)
                except OSError as exc:
                    sys.stderr.write("aira aitest: could not remove worker scope %s: %s\n" % (scope, exc))
            raise WorkerAdmitUnavailable("worker-admit malformed grant: %s" % malformed)
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
        WorkerAdmitUnavailable/WorkerAdmitDenied if admission fails,
        WorkerAdmitRequestTooLarge if this sizing can never fit, or
        WorkerPlacementFailed if the forked child died before confirming it
        joined its granted cgroup scope -- the caller (run()) decides
        fallback/retry/terminal-queue policy for each.

        Safety: the ENTIRE forked-child branch below is wrapped in one
        broad try/except that os._exit()s on ANY exception. A forked child
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
                os._exit(70)
            os._exit(0)
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
        ack = _read_line_blocking(result_read, state)
        if ack != self._PLACED_LINE:
            # The child died (os._exit'd, above, or from fork_worker's own
            # guard) before ever confirming placement -- it never joined
            # its granted cgroup scope. This is a PLACEMENT failure, not a
            # mid-test crash (Task 15's _handle_worker_exit is for a worker
            # that WAS placed and later died) -- release the now-dead
            # admit lease and raise distinctly so the caller does not
            # spend this nodeid's one-and-only crash-retry budget on it.
            os.close(result_read)
            os.close(dispatch_write)
            try:
                os.waitpid(pid, 0)
            except ChildProcessError:
                pass
            admit_process.stdin.close()
            try:
                admit_process.wait(timeout=5)
            except Exception:
                pass
            # Mirrors _retire_worker's best-effort scope cleanup (Fable
            # build-review, final gate): the daemon already granted and
            # CreateWorkerScope already made this directory before the
            # child died -- without this, every placement failure leaks
            # one empty scope directory under the outer scope, and the
            # AIRA-36 reaper cannot sweep it until the whole job's
            # supervisor process is gone (>=2 minutes later).
            try:
                os.rmdir(grant["scope"])
            except OSError as exc:
                sys.stderr.write("aira aitest: could not remove worker scope %s: %s\n" % (grant["scope"], exc))
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
        forked-child branch os._exit()s on any exception rather than ever
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
                os._exit(70)
            os._exit(0)
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
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
        if state["admit_process"] is not None:
            state["admit_process"].stdin.close()
            try:
                state["admit_process"].wait(timeout=5)
            except Exception:
                # A wedged admit-relay process must never abort the whole
                # run over a best-effort wait -- matches the identical
                # guard on this same call in spawn_worker's own failure
                # path (build-review P2: this one was the sole unguarded
                # copy).
                pass
        grant = state.get("grant")
        if grant is not None:
            try:
                os.rmdir(grant["scope"])
            except OSError as exc:
                sys.stderr.write("aira aitest: could not remove worker scope %s: %s\n" % (grant["scope"], exc))
        del self.workers[pid]

    def _wait_for_admission_or_disable(self, spawn):
        """Retries spawn() (a zero-arg callable performing one admission
        attempt) INDEFINITELY on WorkerAdmitDenied, with a loud periodic
        stderr warning so a stalled run is never silent. Returns on
        success, or on WorkerAdmitUnavailable/WorkerPlacementFailed
        (which _disable_daemon and the caller's own fallback handle).
        WorkerAdmitRequestTooLarge deliberately propagates to the caller,
        which marks the affected queue unevaluated rather than retrying or
        silently falling back unconfined. Never returns on
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

        WorkerAdmitRequestTooLarge is instead a permanent sizing verdict:
        drain the remaining queue to unevaluated without disabling the
        still-healthy daemon or spawning an unconfined fallback worker.

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
                except WorkerAdmitRequestTooLarge as exc:
                    self._fail_queue_too_large(str(exc))
                    return
                if self.daemon_available:
                    return  # the wait succeeded -- a confined worker now exists
                # else: the wait's own WorkerAdmitUnavailable/
                # WorkerPlacementFailed branch already called
                # _disable_daemon -- fall through to the SAME
                # fallback-spawn every other daemon-unavailable path in
                # this function already uses, rather than return here and
                # leave the pool empty with queue work still undone.
            except WorkerAdmitRequestTooLarge as exc:
                self._fail_queue_too_large(str(exc))
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
            self.results[nodeid] = outcome
            state["in_flight"] = None
            if recycling:
                self._retire_worker(pid, state)
                self._replace_worker()
                return
        if state.get("result_eof"):
            self._handle_worker_exit(pid, state)

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
                except WorkerAdmitRequestTooLarge as exc:
                    self._fail_queue_too_large(str(exc))
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
                except WorkerAdmitRequestTooLarge as exc:
                    self._fail_queue_too_large(str(exc))
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
        # Best-effort: rmdir the supervisor's OWN child scope this run
        # relocated itself into (bootstrap, Task 2/3/11). The OUTER scope
        # itself is `aira confine`'s own job to tear down when the whole
        # launch process exits -- this is only about the new child scope
        # aitest itself created. NOTE: in the real-cgroup case this
        # typically still fails here (EBUSY) since the supervisor process
        # calling rmdir is itself still a live member of the scope it is
        # trying to remove -- it only ever succeeds AFTER this process
        # exits, which is after this call returns. Attempted anyway because
        # it is free and occasionally correct (e.g. non-real-cgroup test
        # doubles).
        #
        # #72's orphaned-scope reaper IS the real backstop for the nested
        # case this rmdir usually can't finish itself (a crashed
        # supervisor/worker -- spec 3.6's normal, expected death-by-OOM
        # path, where nothing here runs -- leaves live-then-dead child
        # scopes under the outer scope). This was a real, confirmed gap
        # for a while (the reaper only did a single-level rmdir, which the
        # kernel refuses on a cgroup with live children, so nested orphans
        # accumulated unbounded) -- fixed and deployed as AIRA-36
        # (reapEmptyConfineScopeTree, internal/runner/confine_manage_linux.go,
        # master 826f33b): whole-subtree-empty positive-proof-gated,
        # fd-anchored, never touches a scope with a live worker anywhere
        # in its subtree. Live-verified sweeping this exact nested shape.
        if self.supervisor_scope:
            try:
                os.rmdir(self.supervisor_scope)
            except OSError as exc:
                sys.stderr.write("aira aitest: could not remove supervisor scope %s: %s\n" % (self.supervisor_scope, exc))
        return self.results
