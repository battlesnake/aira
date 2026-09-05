---
{"schema":1,"id":"AIRA-40","project":"aira","title":"worker crash detection can hang if a test's own fork() keeps the result pipe open","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
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

## FIXED 2026-09-05 (backlog-remediation Phase 2)

Worker liveness is now **observed** rather than inferred: `Supervisor._open_pidfd`
opens an `os.pidfd_open` fd per forked worker at both fork sites (`spawn_worker`
after its placement ack, and `_spawn_fallback_worker`), and `run()` puts those
fds in the SAME `select()` set as the result pipes. A ready pidfd means the
tracked pid is gone, and the nodeid takes the existing
`_handle_worker_exit` requeue-once/`unevaluated` path with no EOF required.

Chosen over the ticket's own candidate direction (an `os.waitpid(WNOHANG)` poll
folded into the `select()` timeout tick) for two reasons that turned out to
matter: a pidfd wakes the loop the instant the worker dies instead of up to a
tick later, and observing it does **not** consume the child's exit status, so it
cannot race `_reap_child`'s own `waitpid` into a lost or double reap — the exact
"careful integration ... to avoid a double-reap" hazard this ticket flagged.

Verified against the kernel on the build host before writing any code
(`~/tmp/aira40/probe.py`): with a grandchild holding the write end, `select()`
reports the pidfd ready and the pipe NOT ready; `pidfd_open` on an unreaped
zombie succeeds and is immediately ready; and a pidfd is LEVEL-triggered and
stays readable even after the child is reaped — which is why `_retire_worker`
closes it (a pidfd left open for a retired worker would spin the dispatch loop
at 100% CPU) and why `_child_close_other_workers_fds` drops inherited copies.

Race against a legitimate late-arriving result, the ticket's other named
hazard: `_service_ready_workers` drains a worker before ever concluding it
died, so a result flushed immediately before death is recorded rather than
overwritten by a crash verdict. It runs two passes per wakeup (ready result
pipes, then pidfd exits), but what is load-bearing is the drain inside the exit
branch, not the order of the loops — the honest statement, corrected during
review after a mutation test showed swapping the loops changes nothing. That
drain is also the only thing covering the commonest form of this race:
`select()`'s answer is a snapshot, so a worker's last bytes can land after it
returned, leaving that fd out of `ready` entirely. Every entry is re-checked by
state-dict IDENTITY rather than pid or fd equality, because a pass can retire a
worker and `_replace_worker` can fork a fresh one within the same call — onto a
recycled fd number (ordinary) or even the same pid (astronomically unlikely but
structurally possible).

EOF detection is kept, not replaced: it is sufficient in the common case,
arrives at the same instant there, and is all there is on a host without
`os.pidfd_open` (Python 3.9+ / Linux 5.3+). That degradation is announced once
on stderr rather than left silent, and is `None` in the worker state rather than
an exception.

Seven regression tests (nine cases), verified against the parent commit by
reverting `supervisor.py` alone and re-running. The end-to-end one is the
ticket's literal scenario — a real worker whose test forks a real long-lived
grandchild and then `SIGKILL`s itself — and on the parent commit it fails with
"the supervisor never noticed the killed worker", the hang this ticket
describes; its `SIGALRM` failsafe exists so a future regression fails it rather
than hanging the suite. The rest pin, at unit level with no timing in them, the
crash path firing with no EOF, drain-before-death in both the
`select()`-reported and the after-the-snapshot form, the two identity re-checks,
`_retire_worker` closing the pidfd, and the no-pidfd degradation warning firing
exactly once.

Each of those guards was then mutation-tested individually (six behavioural
mutants applied to `supervisor.py` in turn), and every one was caught by exactly
the intended test — a check worth keeping in mind here, since the first draft of
two of these tests passed against a deliberately broken build. That battery is
also what corrected the ordering claim above.

Spec §3.6 (`docs/superpowers/specs/2026-09-01-aitest-design.md`) now states the
detection mechanism and this limitation instead of leaving "if a worker dies" to
an unsound inference.
