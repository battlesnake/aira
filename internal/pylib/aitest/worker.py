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
