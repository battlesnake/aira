---
{"schema":1,"id":"AIRA-40","project":"aira","title":"worker crash detection can hang if a test's own fork() keeps the result pipe open","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
---
Found by Sol build-review (AIRA-38 review wave). A real, if exotic-to-trigger,
gap in the crash-detection model -- documented here as an accepted Slice 1
limitation rather than rushed into the AIRA-38 review-response fix pass,
since a correct fix needs real design care (a second, independent
liveness-detection path alongside the existing EOF-based one, without
introducing races against the atomic result+recycle wire protocol or the
normal drain/dispatch flow).

**The gap.** `internal/pylib/aitest/supervisor.py`'s worker
liveness/crash detection relies solely on the result pipe's write end
reaching EOF (`_drain_available_lines`'s `result_eof` flag,
`_handle_worker_exit`). A test running inside a worker that itself calls
`os.fork()` directly (or uses `multiprocessing` with Linux's default
`fork` start method) without closing inherited fds in the child causes
that grandchild to inherit a duplicate of the worker's own result-pipe
write end. `CLOEXEC` has no effect across `fork()` (only across `exec()`),
so there is no OS-level flag that prevents this inheritance.

**Failure scenario.** If the grandchild outlives the worker (e.g. the
worker itself is OOM-killed but the grandchild survives), the kernel never
delivers EOF on the supervisor's `result_fd` -- one live duplicate of the
write end is enough to keep the pipe "open" from the kernel's perspective,
even though the tracked worker pid is dead. `state["in_flight"]` for that
nodeid is never cleared, `_handle_worker_exit` never fires, the nodeid is
never requeued or marked `unevaluated`, and `run()`'s own stop condition
(every worker's `in_flight` is `None`) is never satisfied -- the whole run
hangs.

**Candidate fix direction (not designed here).** Track worker liveness
independently of the pipe, e.g. an `os.waitpid(pid, os.WNOHANG)` check
folded into `run()`'s main loop on every `select()` timeout tick (already
fires every ~1s regardless), treating a pid that has actually exited as a
crash via the existing `_handle_worker_exit` path even when its pipe
hasn't signalled EOF. Needs careful integration with the atomic
result+recycle protocol and the normal drain/dispatch flow to avoid a
double-reap or a race against a legitimate late-arriving result.

relates AIRA-30, AIRA-38.
