# AIRA-91 / AIRA-92 — aitest under contention: investigation and fix plan

Status: plan, for review.
Worktree: `aira-aitest-contention`, branch `investigate-aira91-92-aitest-contention`, off `22cedd6`.

## 1. What was reported

- **AIRA-91 (P0)** — a suite under `aitest` stops emitting progress mid-run, prints
  no final summary, and the shell sees exit `0`. Reproduced on three disjoint
  fastest-ee legs (`hosted` ~253 tests, `services` ~1442, `pipeline` ~1800) at
  ~76%–99% completion. Reporter's hypothesis: AIRA-31's TestReport/JUnit replay
  loses the trailer inside an admission fairness-freeze window.
- **AIRA-92 (P0)** — a suite climbs to 84% (one repro) / 99% (another) and then
  genuinely hangs: main process alive, ~0.2% CPU, log untouched for minutes,
  forked workers idle.

Both only under heavy real contention.

## 2. Findings — AIRA-91

### 2.1 The reported hypothesis is refuted, on three independent mechanisms

1. **The fairness freeze cannot touch a running aitest job.** It is one boolean
   in one loop over *queued* admission waiters
   (`internal/daemon/admit.go:863-926`); waiters already granted are filtered out
   at `:865-867`. There is no `cgroup.freeze`, no `SIGSTOP`, and in fact the
   daemon never writes to any cgroup control file at all — its entire cgroup
   interaction is `os.ReadFile` of `memory.current` / `memory.max` /
   `memory.stat`. It also gates only the `admit` verb: `worker-admit` never
   enqueues on a slice queue (`internal/daemon/worker_admit.go:364-478`), so a
   freeze cannot delay, revoke or shrink an aitest worker grant.
2. **The correlated log lines are a different subsystem.**
   `governor active-set active=N parked=M jobs=K` is the cooperative CPU
   governor (`internal/daemon/governor.go:597`), reached only via the `governor`
   verb. `aira confine` never opens a governor connection, and aitest's
   supervisor never runs `pytest_runtest_protocol`, so it never opens a governor
   slot either. The wall-clock coincidence with the freeze line is not evidence
   of a shared mechanism.
3. **aitest's replay path has no silently-discarded error.** Every failure in
   staging/materialisation/replay is either loud on stderr and routed to the
   crash path (`supervisor.py:921-934`, `:954-958`, `:969-973`) or propagates as
   an uncaught exception. `_materialize_events`' two `except` clauses
   (`supervisor.py:1002`, `:1034`) return `None`, which the caller immediately
   turns into a stderr diagnostic plus a requeue. There is no timeout anywhere in
   the replay path and no swallowed error. A replay failure therefore cannot
   produce "exit 0, no summary" — it produces either a loud stderr line or a
   pytest INTERNALERROR with a traceback and exit 3.

### 2.2 What the signature actually means

`pytest_runtestloop` prints aitest's per-nodeid lines and the
`aitest: N passed, ... N unevaluated` summary at `internal/pylib/aitest/__init__.py:87-107`,
**before returning**. `_main` calls `pytest_runtestloop` at
`_pytest/main.py:372`; `pytest_sessionfinish` runs later, from `wrap_session`'s
`finally`.

**Rev 2 correction (Fable plan-review, P2-6).** Rev 1 claimed the missing
`aitest:` line *proves* the process never returned from `pytest_runtestloop`.
That is overstated and is retracted: those are plain `print()`s, block-buffered
under redirection, so a hard death any time between the last flushed progress
write and `pytest_sessionfinish`'s own flushed output loses them too. The sound
claim is weaker and lands in the same place: **a hard, unflushed death somewhere
before `pytest_sessionfinish` wrote anything.** Either way no XML exists for that
run. Combined with a log that ends mid-progress at a
flush boundary (progress dots are block-buffered when stdout is redirected),
this is the signature of a process that died *without flushing* — a signal death
or an `os._exit` — not of a completed run whose trailer was lost.

Nothing on the Go side can produce exit status 0 mid-run: every termination
vector is signal-derived (`cgroup.kill`/`memory.oom.group`/watchdog → 137,
SIGTERM → 143, SIGINT → pytest exit 2). The memory watchdog specifically cannot
target aitest at all — it excludes any cgroup path containing a `.aira-`
component at both selection and re-validation (`internal/daemon/watchdog.go:344`,
`:553`, `:600-607`), and additionally requires an uncapped ancestry, which a
confine scope never has.

### 2.3 The severity answer (coordinator's priority question)

**The JUnit XML is NOT durable. It is a single one-shot write at session finish.**

In the installed pytest 9.0.3, `_pytest/junitxml.py`:

- `pytest_runtest_logreport` (`:531`) only accumulates reporters in memory
  (`node_reporters_ordered.append`, `:518`). Nothing is written to disk.
- The file is created and written exactly once, in `pytest_sessionfinish`
  (`:639`): `os.makedirs` (`:642`), `open(self.logfile, "w", ...)` (`:644`),
  `logfile.write(...)` (`:654`, `:675`).

Consequences, in order of severity:

1. A run that dies inside `pytest_runtestloop` never reaches
   `pytest_sessionfinish`, so **no JUnit XML is written for that run at all**.
2. Because the file is opened `"w"` only at `:644`, **a stale XML from a previous
   run of the same leg survives untouched on disk.** Any verdict script that
   parses that path without checking mtime/freshness reads the *previous* run's
   result. That is a genuine silent-false-pass channel, not a cosmetic trailer
   loss.
3. This is answer **(b)** to the coordinator's question, and worse than (b): the
   data is not merely incomplete, it can be confidently and wrongly *stale*.

Note the one case that is not lost: if aitest raised a Python exception,
`wrap_session`'s `finally` still calls `pytest_sessionfinish`, so a partial XML
*would* be written and a traceback printed. qual saw neither — consistent with a
hard, unflushed death.

### 2.4 Honest status of AIRA-91

**Root cause NOT established.** What is established is that the reported
hypothesis is wrong, that the failure is a hard process death rather than a
reporting bug, that no AIRA daemon component can cause it, and that the result
data is genuinely lost when it happens. The remaining candidates require evidence
this investigation cannot obtain by reading source. Recommended next evidence, in
priority order, is listed in §5.

## 3. Findings — AIRA-92

### 3.1 The supervisor has an unbounded blocking read on its hot path

`Supervisor.acquire_worker` reads the relay's grant line with
`process.stdout.readline()` — **no timeout** (`supervisor.py:286`). The
supervisor is single-threaded: while blocked there it drains no worker result
pipe and dispatches no queued nodeid to any idle worker.

It is on the hot path. `_replace_worker` (`supervisor.py:814`) calls
`spawn_worker` on **every** recycle and every crash — i.e. every
`AIRA_AITEST_WORKER_MAX_TESTS` (default 200) tests per worker, every
`AIRA_AITEST_WORKER_MAX_SECONDS` (default 600s), and whenever a worker crosses
its `memory.high` watermark (`worker.py:244-265`). Under contention those
watermark recycles are frequent and the box is exactly where the relay is
slowest.

The Go side does **not** bound this. The socket read deadline is
`max_wait + admitTransportGrace` where the grace is **one second**
(`internal/runner/worker_admit_client_linux.go:73-81`,
`internal/runner/admission_linux.go:81`), but three segments of the CLI sit
*outside* that deadline and have no bound in code:

- the dial: `dialer.DialContext(ctx, "unix", ...)` with no `Dialer.Timeout` and a
  deadline-free ctx (`worker_admit_client_linux.go:55-59`);
- `runner.CreateWorkerScope`, passed `ctx` rather than `signalCtx`, so it is not
  even SIGINT/SIGTERM-cancellable (`cmd/aira/main.go:1074`);
- `daemon.PathsFromEnv` → `canonicalPath` path walking (`cmd/aira/main.go:1059`).

Additionally the daemon's `evaluateWorkerAdmit` holds `job.mu` across two
cgroupfs reads and is documented in its own comment as "uninterruptible and not
itself deadline-aware" (`internal/daemon/worker_admit.go:192-213`, `:259-267`).

### 3.2 Deterministic synthetic reproduction (no contention required)

`~/tmp/aira-91-92/test_probe_stall.py`, using the same local-stub harness the
existing suite already uses: grant the first two admissions, then never answer
the third. Observed, with a 20s bound:

```
OBSERVED: {'finished_within_20s': False, 'results': 1, 'collected': 6,
           'queue_remaining': 4, 'live_workers': 1,
           'in_flight': ['...::test_a'], 'admit_calls': 3}
```

`run()` never returns; four nodeids stay queued; a live worker's in-flight result
is never drained. That is exactly AIRA-92's reported signature — process alive,
no forward progress, log untouched — and it is **not end-of-run specific**, which
matches the 84% and 99% repros.

### 3.3 A second, related defect in the same function

A transport-deadline expiry surfaces as
`E_CONFINE_UNAVAILABLE: read worker-admit response: i/o timeout`
(`worker_admit_client_linux.go:95`). That text matches none of the recognised
`denied` / `timeout` / `unevaluated` substrings at `supervisor.py:409`, so it
falls through to `WorkerAdmitUnavailable` at `:411` → `_disable_daemon` → **the
rest of the run silently converts to unconfined fallback workers.** A one-second
grace over a lock-serialised, non-deadline-aware daemon evaluation is thin under
N-way worker contention, and the consequence — permanently stripping RAM
containment for the whole run — is far heavier than the situation warrants. The
daemon *was* dialled and the request *was* sent; only the reply was late. That is
"reachable but could not establish a result", which this module already models as
retriable (`WorkerAdmitDenied`, and the `unevaluated` precedent at `:409`).

Corroborating live observation: a healthy 15-worker aitest job on this box holds
one `aira worker-admit` relay per live worker, each a child of the pytest main
process. That is the correct steady state, and it is what qual's `pstree` should
have shown for three workers; seeing only one relay for three workers is itself
consistent with two of them being unconfined fallback workers created after a
`_disable_daemon`.

### 3.4 Ruled out for AIRA-92

- The per-test RAM governor (`aira_xdist_governor`) double-reserving inside
  aitest workers is real and already filed as **AIRA-77** (open, with a confirmed
  finding). It cannot be this hang: it is bounded at
  `_DEFAULT_MAX_WAIT + _GRANT_READ_GRACE` = 302s and then **fails open**
  (`internal/pylib/aira_xdist_governor/__init__.py:366`, `:368-371`), and it
  cannot leave workers *idle* with a non-empty queue.
- The CPU governor's unbounded daemon-side park (`governor.go:728-743`) is not
  reachable: forked aitest workers permanently disable it via the
  `os.register_at_fork` handler (`aira_xdist_governor/__init__.py:58-63`,
  `:163-165`).

## 4. Proposed fix (scoped deliberately narrow)

Both changes are in `Supervisor.acquire_worker`, `internal/pylib/aitest/supervisor.py`.

**Change 1 — bound the relay reads.** Replace the untimed
`process.stdout.readline()` with a deadline-bounded read of one line
(`selectors` on the raw fd, same idiom the governor plugin already uses at
`aira_xdist_governor/__init__.py:274-300`). Deadline = parsed `max_wait` plus a
grace strictly greater than the Go transport grace. On expiry: kill and reap the
relay, emit one stderr diagnostic, and raise **`WorkerAdmitDenied`**.

`WorkerAdmitDenied` is the correct classification, not `WorkerAdmitUnavailable`:
we could not establish a result, so we must not claim the daemon is gone and must
not strip containment. It also lands in already-tested control flow — with other
workers alive `_replace_worker` returns and the pool keeps dispatching; as the
last worker, `_wait_for_admission_or_disable` retries with its existing loud
periodic warning. The blocking `process.stderr.read()` calls at `:288` and `:450`
get the same treatment for the same reason.

Accepted consequence, documented not hidden: if the relay is killed in the
microscopic window after the daemon granted but before we read the line, the
daemon releases the grant on peer disconnect
(`worker_admit.go:456-464`) but the created scope directory is orphaned until
AIRA-36's reaper sweeps it. That is strictly better than hanging the run.

**Change 2 — stop misclassifying a late reply as an absent daemon.** Add
`read worker-admit response` (the transport-deadline shape) to the retriable
branch at `supervisor.py:409`, alongside the existing `unevaluated` precedent, so
a contention-induced late reply no longer permanently disables containment.

**Explicitly deferred**, because I am not confident enough to change it here: the
placement-ack read `_read_line_blocking` (`supervisor.py:558`) is also untimed. Its
realistic failure mode (child dies before acking) is already covered by EOF; the
residual "child alive but wedged" case is narrow, and a false timeout there would
strip containment via `WorkerPlacementFailed`. Recorded as a known gap rather
than fixed speculatively.

## 5. Recommended next evidence for AIRA-91 (cannot be resolved by reading source)

1. `PIPESTATUS` / `set -o pipefail` in the leg script. If pytest is piped to
   `tee`, `$?` is `tee`'s status and the "exit 0" is the harness's, not pytest's.
   fastest-ee's `leg_verdict.sh` already has a history of misclassifying an
   aitest run (recorded in AIRA-31).
2. The mtime of the leg's `--junitxml` file versus the run's start time. Per §2.3
   a truncated run leaves the *previous* run's XML in place; if mtime predates
   the run, that alone proves the false-pass channel is live.
3. `dmesg -T` readability on this host (`kernel.dmesg_restrict`) and whether the
   window was still in the ring buffer — "no OOM in dmesg" is only as strong as
   dmesg being complete and readable.
4. Run the leg under `aira confine ... -- sh -c 'pytest ...; echo "PYTEST_RC=$?"'`
   so the real interpreter status is recorded independently of the pipeline, and
   with `python3 -X faulthandler` plus `PYTHONUNBUFFERED=1` so a hard death leaves
   a flushed tail and any fatal signal leaves a traceback.

## 6. Test plan

- New regression test: a non-responding relay must not wedge the pool — `run()`
  completes, every collected nodeid gets an honest outcome, and a live idle
  worker keeps receiving dispatches. Derived from the probe in §3.2, which fails
  against the current implementation.
- New regression test: a `read worker-admit response: i/o timeout` stderr shape
  raises `WorkerAdmitDenied`, not `WorkerAdmitUnavailable`, and does not set
  `daemon_available = False`.
- The existing 100+ aitest tests must stay green, in particular the
  denial/unavailable/too-large classifier tests and both recycle tests.
- Mutation testing on the new deadline logic: at minimum, remove the deadline,
  make the timeout raise `WorkerAdmitUnavailable` instead of `WorkerAdmitDenied`,
  and skip the relay kill — each must be caught.

## 7. Invariants preserved

- A transient or unestablishable admission result never strips RAM containment
  (`WorkerAdmitDenied`, never `WorkerAdmitUnavailable`).
- A genuinely saturated daemon still makes the run wait, loudly and indefinitely,
  rather than silently degrading to unconfined — `_wait_for_admission_or_disable`
  is unchanged.
- No new fabricated outcome: a nodeid that cannot be run still ends `unevaluated`
  through the existing paths.

---

# Rev 2 — review record and corrections

Two independent plan reviews: Codex/Sol (**BLOCK**, 2×P0) and a Claude Fable
reviewer (**GATE-PASS-WITH-CHANGES**, 2×P1 + 6×P2). They disagreed with each
other on one point, recorded below. Everything here is a change made *because of*
review, not a restatement of Rev 1.

## Corrections adopted

**P0 (Sol) — Change 2's substring was unsafe as written.**
`"read worker-admit response: %w"` (`worker_admit_client_linux.go:95`) wraps
*every* `readRunnerAdmitFrame` failure (`admission_linux.go:562-576`), not only a
deadline. Matching the bare prefix would have retried permanent protocol skew
forever — reintroducing the exact hang class this change removes.

**Reviewer disagreement, and how it was resolved.** Sol wanted only the deadline
shape (`i/o timeout`) treated as retriable. Fable argued EOF/reset are *also*
retriable, because the daemon's own stopping path returns without writing a frame
(`worker_admit.go:436-438`) and, decisively, the *next* attempt disambiguates for
free: a genuinely dead daemon fails at the **dial** instead, producing
`dial daemon: ... connection refused`, which stays `WorkerAdmitUnavailable`. So at
most one extra attempt is spent. Fable's argument is correct and strictly safer in
the honesty direction, and it still satisfies Sol's actual requirement (a
conjunction, never a blanket prefix). Adopted: retriable iff the message contains
`read worker-admit response` **and** one of `i/o timeout` / `EOF` /
`connection reset`. `invalid daemon admission frame size` and `json:` unmarshal
failures stay terminal. Pinned by
`test_transport_frame_failures_are_retriable_but_protocol_skew_is_not`, and
mutation M4 (bare prefix) is caught.

**P0 (Sol) — the placement-ack read must be bounded too; Rev 1's deferral was
wrong.** Adopted. `_read_line_blocking` now takes a deadline. Rev 1's reasoning
was that EOF already covers a dead child — true, but it does *not* cover a child
that is alive and wedged, and that read sits on the same dispatch loop. On expiry
the child is SIGKILLed, the grant released and the scope removed. It raises
`WorkerAdmitDenied`, **not** `WorkerPlacementFailed`: a timeout is not evidence
the local cgroup mechanism is broken, and `WorkerPlacementFailed` is what makes
`_replace_worker` strip containment for the rest of the run. Mutation M9 pins
this.

**P1 (Sol) — `_retire_worker`'s unbounded `os.waitpid`.** Adopted (`_reap_child`:
bounded, escalating to SIGKILL). It sits on the dispatch loop via retirement,
recycle and the end-of-run `__stop__` broadcast, so a child that reported its last
result and then wedged on the way out froze the supervisor exactly as a wedged
relay did. Its results are already recorded by then, so SIGKILL cannot lose data.

**P1 (Sol) — swallowed relay `wait(timeout=5)` left live relays behind**, each
still holding its daemon-side grant. Adopted (`_terminate_process`: bounded, then
SIGKILL).

**P2-4 (Fable) — same misclassification one line up.** `Popen` raising
`OSError` went to `WorkerAdmitUnavailable`, so an EAGAIN/ENOMEM fork failure —
which peaks under exactly this contention — permanently stripped containment on a
healthy daemon. Adopted: EAGAIN/ENOMEM are denials; every other errno (a
permanent local fact) stays unavailable.

**P2-1/P2-3 (Fable) — grace sizing and signal.** Grace is 15s (Fable asked for
5–10s, not merely ">1s"), because `CreateWorkerScope` runs *after* the grant and
*before* the `granted` line, so a tight grace would kill just-granted relays under
the very contention it targets. SIGKILL, not SIGTERM: `signalCtx` cancels only the
dial (`cmd/aira/main.go:1064-1074`), and daemon-side release is by socket close.

**P2-6 (Fable) — wording.** §2.2's "proves" retracted inline above. §2.1's "never
writes to any cgroup control file" is literally true but the daemon *does* rmdir
cgroups (`eject.go:317,325`) under positive-proof guards. §3.1's `PathsFromEnv` is
env lookup plus `canonicalPath` — a real absence of a bound, but not a realistic
stall; it is listed for completeness only, not as a likely cause.

## Corrections adopted about the CLAIM, not the code

**P1-2 (Fable) — the AIRA-92 root-cause claim was too strong, and Rev 1's own
evidence partly contradicts it.** If a run were already in fallback mode,
`acquire_worker` raises at its `daemon_available` guard *before* ever spawning a
relay, so a readline hang is impossible in that state — which does not fit the
"one relay for three workers" pstree. Rev 2 therefore states the honest version:

> This change closes a **reproduced, deterministic defect that produces AIRA-92's
> exact signature**. It is not proof that this defect caused the two live
> incidents. That attribution requires evidence from a live stall.

**Live discriminator (new, and cheap).** `wchan` distinguishes the candidate
states without attaching a debugger, and was verified empirically on this box:

| supervisor state | `cat /proc/<pytest-pid>/wchan` |
|---|---|
| healthy `select()` dispatch loop | `poll_schedule_timeout` |
| blocked in `acquire_worker`'s relay read | `anon_pipe_read` |
| in `_wait_for_admission_or_disable`'s retry sleep | `hrtimer_nanosleep` |

Confirmed against a healthy live 15-worker aitest job on this box
(`poll_schedule_timeout`, one `aira worker-admit` relay per live worker, each a
child of the pytest main process — which is also the correct steady state to
compare any future pstree against). For a live stall, also capture
`ls -l /proc/<pid>/fd` for a pipe to an `aira worker-admit` child, and
`py-spy dump --pid`.

**P1-3 (Fable) — an alternative AIRA-92 cause that is NOT excluded.** A worker
whose working set sits between its scope's `memory.high` (80% of `memory.max`,
`worker_admit.go:285`) and `memory.max` is kernel-throttled and reclaim-bound: it
shows ~0% CPU with `in_flight` still set, and the watermark recycle only fires
*between* tests (`worker.py:244-265`), so it cannot rescue a test already running.
This box currently has ~14 GiB of 20 GiB swap in use. That is indistinguishable
from "idle worker" by `top` alone. Discriminator: per-worker-scope `memory.events`
(the `high` counter) and `memory.current` vs `memory.high`, plus
`/proc/<worker>/stack`. No code change proposed until confirmed.

**P2-5 (Fable) — an AIRA-91 candidate Rev 1 missed.** The worker-grant ledger is
in-memory only (`worker_admit.go:87-110`) with no restart reconstruction — unlike
AIRA-74 for confine reserves — and the relay does not watch its lease connection
(`cmd/aira/main.go:1100-1103` watches only stdin and signals). After a daemon
restart mid-run, `committed` resets to 0, the aggregate guard (`:275-281`) is
defeated, and workers growing into their caps can trip the **outer scope's**
`memory.oom.group` — a hard, unflushed, whole-tree kill. That fits AIRA-91's
signature well. Added to the evidence list: `journalctl --user -u aira-daemon`
over the incident window.

## Consequence stated rather than hidden (Fable)

Under sustained contention every timed-out replacement now costs the dispatch
loop `max_wait + grace` (~45s at defaults) and drops one worker, converging
toward a pool of one and then waiting loudly forever rather than silently running
unconfined. That is the designed honest outcome, but it is a real throughput cost
and is stated here, not left to be discovered.

Two further gaps are recorded, not fixed: the scope directory orphaned by killing
a just-granted relay is swept only when the whole outer job is dead (Fable
corrected Rev 1's more optimistic "AIRA-36 sweeps it" —
`ReapOrphanedConfineScopes` scans only `.aira-CONFINE-*` at the slice root); and
`_DENIAL_WARN_EVERY`'s comment assumes ~1s attempts, so its 30-attempt cadence is
much slower for timed-out attempts — harmless here only because each expiry emits
its own diagnostic.

## Verification

- `pytest internal/pylib/aitest/` — **114 passed**, exit 0 (106 pre-existing + 8 new).
- Every new test verified to FAIL against the pre-fix tree; **three of them HANG
  FOREVER** there (grant read, placement ack, retirement waitpid), which is the
  defect itself rather than a mere assertion failure.
- Mutation testing, 10 mutants, **10/10 caught**. One earlier survivor (M8) was a
  porous test of my own — it asserted `_parse_max_wait_seconds("garbage") ==
  _MAX_WAIT_FALLBACK_SECONDS`, a tautology that survived setting that constant to
  `0`. Rewritten to pin the property (generous *and* finite).
- `go build ./...` exit 0, `go vet ./...` exit 0, `go test ./internal/pylib/...`
  `ok 42.518s`.
