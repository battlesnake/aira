---
{"schema":1,"id":"AIRA-92","project":"aira","title":"aitest worker coordination wedges near the tail of a large run under contention (genuine stall, not exit)","status":"done","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["aitest","dogfood"],"hold":false,"relations":[]}
---
## Symptom (reported by peer session `qual`, fastest-ee's `services` leg, ~1442 tests)

Running `services` under `aitest --aitest-workers=auto` on a heavily contended box: progress climbed cleanly to 99% then went completely silent — not an exit, a genuine hang. Confirmed via direct process inspection at t=927s elapsed: the pytest main process (verified alive, not exited, not OOM'd) sitting at 0.2% CPU, log file untouched for 5.5 minutes. `pstree` showed 3 forked worker `python`s under the main process — one with its own `aira` helper + thread pool still attached, the other two with no visible children (i.e. genuinely idle, not doing anything). Main process not progressing.

## What was ruled out

The old xdist-governor was correctly NOT involved — no `enforce granted` entries for this job's scope in the governor log during the stall window, confirming aitest-workers runs correctly skip that path. This is not the old governor fighting the new system; it looks like a genuine wedge in aitest's own worker coordination, specifically near the tail of a large run (99% complete when it stuck).

qual handled it correctly and safely: killed their own supervisor PID (SIGINT) per the kill-your-own-job rule rather than leaving it running indefinitely.

## Why this matters

A worker-coordination deadlock/wedge near run completion is a real reliability bug independent of the honesty-contract class above (AIRA's sibling ticket for the `hosted` leg's silent-truncation finding) — this one just hangs, it doesn't lie about the result. Both were only reproduced under heavy concurrent contention (qual's own merge-gate running its engine/lite/hosted/services legs in parallel both times); qual has not yet gotten a clean quiet-slice repro to separate contention-triggered flakiness from a real coordination bug in aitest itself — in progress on their end, results to follow.

## Suggested direction

Not investigated against source yet. Needs tracing aitest's worker-completion/join logic (`internal/pylib/aitest`) for a plausible deadlock or missed-wakeup near the tail of a run — e.g. a worker finishing and signaling completion in a way the main process's event loop can miss under specific timing, or a resource (lock, queue, pipe) that two of the three forked workers never got handed correctly ("no visible children" on two of three is a real, specific clue — worth understanding why those two never spawned real work while the third did). Given the near-100%-completion timing, this may be specific to end-of-run bookkeeping/aggregation rather than general worker dispatch.

## Likely shared root cause with AIRA-91 (source-traced, 2026-09-04)

A dedicated read of `internal/pylib/aitest/supervisor.py` for AIRA-91's
separate JUnit-XML question turned up two unguarded blocking points inside
`Supervisor.run()`'s dispatch loop, neither with a timeout:

- `Supervisor.acquire_worker()` (`supervisor.py:286`): `process.stdout.readline()`
  on the `worker-admit` CLI subprocess — unbounded.
- `Supervisor._wait_for_admission_or_disable()` (`supervisor.py:743-784`):
  indefinite `time.sleep(1.0)`-paced retry loop on `WorkerAdmitDenied`,
  reached from `_replace_worker` when a worker retires mid-run.

Both are near-zero-CPU-while-stuck (a blocking read, or a 1s-paced sleep
loop) — consistent with qual's `pstree` observation (process alive, 0.2%
CPU, two of three forked workers idle with no children: exactly what you'd
see if `_replace_worker` tried to admit a replacement near the tail of the
run and got stuck retrying against a denied/slow `worker-admit` response
during contention). AIRA-91's signature is the SAME stall, differing only in
whether something later externally kills the process (91: exit 0, no
summary) or nothing does (92: true hang, no exit ever). Not yet confirmed as
definitively the same code path — relayed to the running investigation agent
to verify directly rather than accepting this as settled from inference
alone.

## FIX LANDED IN PR — https://github.com/battlesnake/aira/pull/19 (2026-09-04)

Branch `investigate-aira91-92-aitest-contention`, commit `fdc4995`. **Reproduced
deterministically with NO contention**, fixed, reviewed by two lineages,
mutation-tested. Not merged yet.

### Root cause, and the honest limit of the claim

The supervisor is single-threaded. While blocked reading the `aira worker-admit`
relay it drains no worker's result pipe and dispatches no queued nodeid to an
already-idle worker — so one untimed read there does not delay a replacement, it
**freezes the whole pool**. Confirmed unbounded reads, all on the dispatch loop:

- `acquire_worker`'s grant-line `process.stdout.readline()` (`supervisor.py:286`)
- the post-fork placement ack `_read_line_blocking` (`:558`) — EOF covered a
  child that DIED, never one alive-but-wedged
- `_retire_worker`'s `os.waitpid(pid, 0)` (`:721`), reached by retirement,
  recycle and the end-of-run `__stop__` broadcast
- relay release waits that swallowed their timeout and left a **live relay still
  holding its daemon-side grant**

`aira worker-admit` is not self-bounding: its socket deadline is `max_wait` plus
a **one second** grace (`internal/runner/admission_linux.go:81`), while its dial
(`worker_admit_client_linux.go:55-59`) and `CreateWorkerScope` (`cmd/aira/main.go:1074`,
passed `ctx` not `signalCtx`, so not even SIGINT-cancellable) sit outside it with
no bound in code, and the daemon's `evaluateWorkerAdmit` holds `job.mu` across two
cgroupfs reads while documenting itself "uninterruptible and not itself
deadline-aware" (`internal/daemon/worker_admit.go:192-213`).

**Deterministic synthetic reproduction** (stub relay that answers nothing, no real
contention): `run()` never returns, 1 of 6 results recorded, 4 nodeids still
queued, a live worker's in-flight result never drained.

**HONEST LIMIT — this is not proof of attribution.** The fix closes a reproduced
defect that produces AIRA-92's exact signature and can fire at any progress point
(matching both the 84% and 99% repros). It is **not** established that this defect
caused the two live incidents. Fable's review found the pstree evidence partly
contradicts it: if a run were already in fallback mode, `acquire_worker` raises at
its `daemon_available` guard *before* spawning a relay, so a readline hang is
impossible in that state.

### Cheap live discriminator (verified empirically on this box)

On a live stall, `cat /proc/<pytest-pid>/wchan`:

| value | meaning |
| --- | --- |
| `poll_schedule_timeout` | healthy `select()` dispatch loop |
| `anon_pipe_read` | blocked in `acquire_worker`'s relay read |
| `hrtimer_nanosleep` | in `_wait_for_admission_or_disable`'s retry sleep |

Confirmed against a healthy live 15-worker aitest job on this box
(`poll_schedule_timeout`; one `aira worker-admit` relay per live worker, **each a
child of the pytest main process** — that is the correct steady state to compare a
stalled pstree against, and it re-reads qual's "one aira under a worker"
observation).

### An alternative cause that is NOT excluded (Fable P1-3)

A worker whose working set sits between its scope's `memory.high` (80% of
`memory.max`) and `memory.max` is kernel-throttled and reclaim-bound: **~0% CPU
with `in_flight` still set**, indistinguishable from "idle worker" by `top`. The
watermark recycle only fires *between* tests (`worker.py:244-265`), so it cannot
rescue a test already running. This box currently has ~14 GiB of 20 GiB swap in
use. Discriminator: per-worker-scope `memory.events` (`high` counter),
`memory.current` vs `memory.high`, and `/proc/<worker>/stack`.

### Also fixed — two pre-existing misclassifications of the same honesty class

Both peak under exactly this contention and both silently converted the remaining
run to **unconfined** workers on a perfectly healthy daemon:

- a transport-deadline overrun (`read worker-admit response: ... i/o timeout`) was
  classified `WorkerAdmitUnavailable` → `_disable_daemon`
- an `EAGAIN`/`ENOMEM` fork failure launching the relay, likewise

### Ruled out for AIRA-92

- **AIRA-77** (the `aira_xdist_governor` per-test `confine-reserve` running inside
  aitest workers) is real and confirmed, but cannot be this hang: bounded at 302s
  and then **fails open**, and it cannot leave workers *idle* with a non-empty queue.
- The CPU governor's unbounded daemon-side park is unreachable — forked aitest
  workers permanently disable it via `os.register_at_fork`.

### Evidence

114 tests pass. **Every new test fails against the pre-fix tree, three of them by
hanging forever.** Mutation testing 10/10 caught (one earlier survivor exposed a
porous test of my own — a tautology against the constant it was meant to pin).
`go build`/`go vet` 0; `go test ./internal/pylib/...` ok.

## Merged

PR #19 merged to master as `ac901cb`
(https://github.com/battlesnake/aira/pull/19). Reviewed by two independent
lineages (Codex/Sol BLOCK → 2×P0 adopted; Fable GATE-PASS-WITH-CHANGES → 2×P1
+ 6×P2 adopted, one deliberate disagreement resolved in Fable's favour on
evidence). ## Deployed

Binary rebuilt from merged master (`ac901cb` + the follow-up ticket-bookkeeping
commit `3694bc2`, confined build, smoke-tested before install), skill
reinstalled (`aira skill install --force`), `aira-daemon.service` restarted.
AIRA-91 remains open and is explicitly NOT closed by this — its root cause
is separate and unestablished; see AIRA-91.
