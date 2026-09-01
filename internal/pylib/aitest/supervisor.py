import os
import subprocess
import sys


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


class Supervisor:
    """Owns collection, the pull dispatch queue, and worker-admit relaying.
    Never runs a test itself -- see worker.py for that."""

    def __init__(self):
        self.queue = []
        self.attempts = {}  # nodeid -> attempt count (Task 15's retry-once rule)
        self.outer_scope = None
        self.supervisor_scope = None
        self.daemon_available = True
        self.max_workers_fallback = max(1, int(os.environ.get("AIRA_AITEST_MAX_WORKERS_FALLBACK", "1")))
        self._fallback_warned = False

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
