import os
import select
import subprocess
import sys
import time

from aitest.worker import fork_worker, run_worker_loop


class WorkerAdmitUnavailable(Exception):
    """The daemon is genuinely unreachable at the connection level: a dial
    failure, the worker-admit CLI itself could not even be launched, or its
    response was malformed/garbage -- there is no daemon to talk to. This
    is the ONLY failure class that should ever disable daemon-backed
    admission for the rest of the run (_disable_daemon, Task 16)."""
    pass


class WorkerAdmitDenied(Exception):
    """The daemon IS reachable and responded normally with "denied" (budget
    genuinely exhausted right now) or "timeout" (the request waited out its
    full window -- the daemon is just busy/contended, not down). Means
    "don't add a worker at this moment", never "abandon containment for the
    rest of the run"."""
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
    return line.decode("utf-8", "strict") if sep else ""


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
        lines.append(line.decode("utf-8", "strict"))
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
        self.items_by_nodeid = {}
        self.workers = {}
        self.results = {}
        self._run_estimated_bytes = 0
        self._run_max_wait = "30s"

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
        (denied or timeout) -- the caller MUST treat these differently
        (Task 16): only WorkerAdmitUnavailable may disable daemon-backed
        admission for the rest of the run."""
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
        line = process.stdout.readline().decode("utf-8", "strict").strip()
        if not line.startswith("granted "):
            stderr = process.stderr.read().decode("utf-8", "replace") if process.stderr else ""
            try:
                process.wait(timeout=5)
            except Exception:
                process.kill()
            message = line or stderr or "worker-admit exited without a grant"
            # RequestWorkerAdmit (Task 8) wraps a daemon-reachable-but-
            # declined response as "worker-admit <state>: <reason>" on
            # stderr -- ANYTHING else (a dial failure, a launch failure, a
            # malformed response) means there is no daemon to talk to at
            # all. This distinction is load-bearing (Task 16).
            if "worker-admit denied" in message or "worker-admit timeout" in message:
                raise WorkerAdmitDenied(message)
            raise WorkerAdmitUnavailable(message)
        grant = {}
        for field in line[len("granted "):].split():
            key, _, value = field.partition("=")
            grant[key] = value
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
        WorkerAdmitUnavailable/WorkerAdmitDenied if admission fails, or
        WorkerPlacementFailed if the forked child died before confirming it
        joined its granted cgroup scope -- the caller (run()) decides
        fallback/retry policy for each.

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
            try:
                os.waitpid(pid, 0)
            except ChildProcessError:
                pass
            admit_process.stdin.close()
            try:
                admit_process.wait(timeout=5)
            except Exception:
                pass
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

    def _dispatch_to_idle_workers(self):
        for state in self.workers.values():
            if state["in_flight"] is not None:
                continue
            nodeid = self.next_nodeid()
            if nodeid is None:
                continue
            state["in_flight"] = nodeid
            state["dispatch_write"].write(nodeid + "\n")
            state["dispatch_write"].flush()

    def _retire_worker(self, pid, state):
        state["dispatch_write"].close()
        try:
            os.close(state["result_fd"])
        except OSError:
            pass
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
        state["admit_process"].stdin.close()
        state["admit_process"].wait(timeout=5)
        del self.workers[pid]

    def run(self, estimated_bytes, worker_count=1, max_wait="30s"):
        """Slice 1's whole dispatch loop: spawn up to worker_count workers,
        pull-dispatch the queue to whichever are idle, collect results via
        select() over each worker's result pipe, until the queue is drained
        and every worker has retired. Recycle (Task 14), crash/retry (Task
        15), and daemon-down fallback (Task 16) extend this method in
        place."""
        self.bootstrap()
        # gc.freeze() moves already-imported objects into the permanent
        # generation before any fork, per design spec 3.1: post-fork COW
        # pages a worker's own GC scanning would otherwise touch (and
        # dirty) shrink to near nothing.
        import gc
        gc.freeze()
        self._run_estimated_bytes = estimated_bytes
        self._run_max_wait = max_wait
        for _ in range(worker_count):
            if not self.queue:
                break
            self.spawn_worker(estimated_bytes, max_wait=max_wait)
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
                state = self.workers[pid]
                for line in _drain_available_lines(fd, state):
                    nodeid, _, outcome = line.partition(" ")
                    self.results[nodeid] = outcome
                    state["in_flight"] = None
            self._dispatch_to_idle_workers()
            if not self.queue and all(state["in_flight"] is None for state in self.workers.values()):
                for pid, state in list(self.workers.items()):
                    state["dispatch_write"].write("__stop__\n")
                    state["dispatch_write"].flush()
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
        # doubles); #72's existing orphaned-scope reaper is the real
        # backstop that cleans this up machine-wide once the process is
        # actually gone.
        if self.supervisor_scope:
            try:
                os.rmdir(self.supervisor_scope)
            except OSError as exc:
                sys.stderr.write("aira aitest: could not remove supervisor scope %s: %s\n" % (self.supervisor_scope, exc))
        return self.results
