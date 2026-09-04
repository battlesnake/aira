---
{"schema":1,"id":"AIRA-91","project":"aira","title":"aitest silently loses the final report summary under admission fairness-freeze contention (exit 0, truncated output)","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["aitest","confine","dogfood","honesty"],"hold":false,"relations":[]}
---
## Symptom (reported by peer session `qual`, fastest-ee's `hosted` leg, two independent reproductions)

Running `hosted` (~253 tests) under `aitest` on a heavily contended box: progress dots stop partway through, the confine trailer prints immediately after, and the process exits **0** — no final pytest summary, no failure indication, nothing. Reproduced twice with different reserve sizes (512M and 8G), both truncating mid-run rather than completing.

## What was ruled out (verified, not assumed)

- dmesg OOM: none, either time.
- Daemon crash/restart: continuous uptime through both incidents.
- Hidden-progress-bar/capture artifact: `grep -c $'\r'` on the raw log = 0 — the log file is genuinely short (929 bytes), not a terminal-control-character illusion.

## The lead

The governor log for one window lines up exactly with where the dots stopped:
```
03:31:18  active-set active=1 (job admitted)
03:32:46  fairness-freeze hold ... queued-for=1m0s (1 also queued)
03:33:49  active-set active=0 parked=0 jobs=0
```
qual's own hypothesis, explicitly flagged as unconfirmed from outside: this looks like aitest's JUnit-XML/report replay (AIRA-31, built earlier this session) losing the final trailer specifically under admission contention/timing — the run may actually be completing internally, but the replay/report-assembly step gets interrupted or races against something in the fairness-freeze window, and the exit-0 truncation is a REPLAY bug, not a real pytest crash.

## Why this is severity P0

If this hypothesis holds, it's a live, currently-shipping instance of the exact fabricated-honesty class this project has spent tonight hunting down — an exit code and (apparent) clean completion that doesn't actually reflect what happened, specifically triggered by exactly the kind of contention this project's admission system exists to handle gracefully. A test suite silently truncating and reporting exit 0 is worse than a hard failure: nobody notices.

## Suggested direction

Not investigated against source yet. Needs tracing through aitest's report-replay path (`internal/pylib/aitest`, the JUnit-XML/coverage/TestReport replay work from AIRA-31) for anything that could be interrupted, race, or silently swallow an error specifically during a fairness-freeze hold/yield transition — check whether the replay step depends on a live daemon connection or admission state that the freeze window disrupts, and whether any exception/error path there is caught and discarded rather than surfaced. qual has NOT yet gotten a clean quiet-slice repro to separate "aitest-specific contention bug" from general contention flakiness — that's a valuable next data point, in progress on their end.

## Update: reproduced on a THIRD, unrelated leg (qual, 2026-09-04)

Same signature — progress stops, no summary line, exit 0 — now also hit on
fastest-ee's `pipeline` leg (~1800 tests, `fastapi`/`numpy`/`scikit-rf`
dependency profile), stalling at ~76% completion. `hosted` (uvicorn/pyjwt/
scikit-fem/stripe) and `services` (httpx/prometheus) had already reproduced
it independently. **Three legs, three disjoint dependency profiles, same
symptom** — this is now good evidence for a general aitest-under-contention
property, not something leg- or dependency-specific. `pipeline` was also
pinned off aitest for this run (`FASTEST_NO_AITEST=1`), matching `hosted`/
`services`'s prior workaround. qual flags the earlier "backend/admin/obs/
sym-gen clean" verdict as provisional (checked under lighter contention),
not confirmed-safe, given this trend.

qual also raised the load-bearing open question this ticket needs answered
before severity can be assessed accurately: is a truncated run's underlying
result data (JUnit XML etc.) reliably already-complete-and-durable when the
trailer is lost (cosmetic bug — annoying but not a correctness risk), or is
it genuinely incomplete (a real silent-false-pass risk for any caller that
checks only `$?`)?

## Answer to the JUnit-XML question (source-traced, not inferred — 2026-09-04)

**Not cosmetic. It's a hard binary, and AIRA-91's described symptom lands on
the bad side of it.**

`internal/pylib/aitest/__init__.py`'s `pytest_runtestloop` fully preempts
pytest's own loop and calls `Supervisor.run()` (`supervisor.py:1143`) with no
`try/except`. Only *after* `run()` returns does aitest print its own
`"aitest: N passed, M failed, ..."` line (`__init__.py:87-104`); only after
`pytest_runtestloop` itself then returns does pytest's real `wrap_session()`
fire `pytest_sessionfinish` (`_pytest/main.py:347-359`), which is the ONLY
place `junitxml.py`'s `LogXML.pytest_sessionfinish()` runs — a single
all-or-nothing `open()`/`write()` of the whole accumulated tree
(`junitxml.py:644,675`), not incremental. Per-worker results are replayed
into pytest's hooks live as workers report (`supervisor.py:975,1043-1056`),
but that only mutates an in-memory accumulator — nothing about JUnit XML is
durable-as-you-go.

So: **if you saw aitest's own `"aitest: N passed..."` line, JUnit XML is
reliably complete.** If `run()` never returned — dots stop, no summary line,
no pytest INTERNALERROR traceback, which is exactly AIRA-91's reported
signature — `pytest_sessionfinish` never fires and **the JUnit XML for that
run was never written at all** (not partial — you'd get a stale prior-run
file or nothing). Real results genuinely lost, not just the print.

**The original replay-swallows-an-exception hypothesis is now unsupported.**
No broad `except` wraps the dispatch loop, `_synthesize_unevaluated_reports`,
or the summary print in `supervisor.py`/`__init__.py`/`worker.py` — the ones
that exist are narrow, forked-child-only safety nets. An uncaught Python
exception there would surface as a visible `INTERNALERROR` traceback with a
nonzero exit anyway (pytest's own `wrap_session` catches `BaseException` and
still runs its `finally:` → still fires `pytest_sessionfinish`), which does
not match the silent/clean-exit-0 signature reported.

**What does match: the supervisor process being killed externally (SIGKILL,
or SIGTERM — aitest installs no `signal` handlers anywhere) while blocked
inside `Supervisor.run()`'s dispatch loop, before it can return.** Two
concrete, unguarded blocking points identified in that loop, either of which
would produce exactly "dots stop, then silence" during a fairness-freeze
window:
- `Supervisor.acquire_worker()` (`supervisor.py:286`): `process.stdout.readline()`
  on the `worker-admit` CLI subprocess — unbounded, no timeout.
- `Supervisor._wait_for_admission_or_disable()` (`supervisor.py:743-784`):
  retries indefinitely with `time.sleep(1.0)` on `WorkerAdmitDenied`, called
  from `_replace_worker` when a worker retires mid-run — i.e. still inside
  the dispatch loop, before `run()` can return.

**Open thread, not yet closed**: this doesn't explain why the outer process
is observed to exit **0** — an external SIGKILL normally surfaces as a
nonzero/137 exit to the shell. That needs Go-side tracing (`aira confine`'s
child-exit handling and the confine trailer print), separate from this
Python-side trace. Relayed to the running AIRA-91/92 investigation agent to
close that loop, and because both blocking points above are also strong
shared-root-cause candidates for **AIRA-92**'s hang (a hang is simply the
same stall with nothing ever killing the process).

## Downstream impact confirmed: a live false-green hole in fastest-ee's own merge gate (qual, 2026-09-04)

qual traced fastest-ee's `scripts/leg_verdict.sh` and found its FIRST check
is an unconditional `rc=0 -> PASS`, ahead of any blank/truncation detection —
on the (per this ticket's finding, now known false) assumption that a clean
exit is never blank. Filed on their side as `BL-969` (fastest-ee's tracker,
not this project's). Consequence: **any leg run on aitest that AIRA-91 hits
silently — not just the three legs already pinned off it, every leg,
including ones reported clean tonight** — reads as an ordinary green PASS in
their merge gate today. This is exactly the false-pass path this ticket was
already worried about, now confirmed to have a real, live consumer. Raises
the priority of an actual fix here: it isn't just stopping truncation, it's
closing a currently-open false-green hole in every merge gate that consumes
an aitest-governed leg. qual deliberately did not attempt a fix on their
side (shared gate-verdict machinery, out of scope for their in-flight PR,
correctly declined to rush it) — the durable fix belongs here, upstream.

## Interim client-side mitigation shipped on qual's side (2026-09-04)

Not a fix to aitest/AIRA — a scoped compensating control in fastest-ee's own
`merge_gate.sh`, worth recording since it independently validates the
`"aitest: "` summary-line signal from this ticket as the right detection
primitive: for the 6 legs where `merge_gate.sh` directly resolves its own
pytest flags, a PASS from an aitest-governed leg now also requires that
literal line in the log, else it's reclassified BLANK with a forced nonzero
return — closing part of BL-969's gap. Explicitly documented residual gaps:
doesn't cover `hosted`/`services` (already pinned off aitest) or `obs`
(delegates to a separate `make` invocation this guard can't inspect). Does
not reduce the priority of the real fix here — it's a downstream patch
around the symptom on one consumer, not a fix to the underlying stall/kill
path, and doesn't help any other aitest consumer.

## Investigation result (2026-09-04) — reported hypothesis REFUTED, root cause NOT established

Investigated against source in worktree `aira-aitest-contention`. Plan + full
citations: `docs/superpowers/plans/2026-09-04-aira91-92-aitest-contention-plan.md`
(on PR https://github.com/battlesnake/aira/pull/19). **No fix for AIRA-91 is
claimed.**

### The stated hypothesis (AIRA-31 replay losing the trailer in a freeze window) is refuted, three independent ways

1. **The fairness freeze cannot touch a running aitest job.** It is one boolean in
   one loop over *queued* admission waiters (`internal/daemon/admit.go:863-926`);
   already-granted waiters are filtered out at `:865-867`. There is **no
   `cgroup.freeze` and no `SIGSTOP` anywhere in the tree**. Decisively, it gates
   only the `admit` verb — `worker-admit` never enqueues on a slice queue
   (`internal/daemon/worker_admit.go:364-478`), so a freeze cannot delay, revoke
   or shrink an aitest worker grant at all.
2. **The correlated log lines are a different subsystem.** `governor active-set
   active=N parked=M jobs=K` is the cooperative CPU governor
   (`internal/daemon/governor.go:597`), reached only via the `governor` verb.
   `aira confine` never opens a governor connection, and aitest's supervisor never
   runs `pytest_runtest_protocol` so it never opens a governor slot either. The
   wall-clock coincidence is not evidence of a shared mechanism.
3. **aitest's replay path has no silently-discarded error.** Every staging /
   materialisation / replay failure is loud on stderr and routed to the crash path
   (`supervisor.py:921-934`, `:954-958`, `:969-973`), or propagates as a visible
   pytest INTERNALERROR with a traceback and exit 3. There is no timeout anywhere
   in the replay path. A replay failure therefore **cannot** produce "exit 0, no
   summary".

Also ruled out: **no Go-side vector produces exit status 0 mid-run** — every
termination path is signal-derived (`waitConfineCommand`,
`internal/runner/confine_linux.go:1346-1361`, returns `128+signal`). And the
memory watchdog cannot target aitest at all: it excludes any cgroup path
containing a `.aira-` component at both selection and re-validation
(`internal/daemon/watchdog.go:344`, `:553`, `:600-607`) and separately requires an
uncapped ancestry, which a confine scope never has.

### Severity: confirmed NOT cosmetic — and worse than "data incomplete"

Independently re-derived, agreeing with the parallel trace: JUnit XML is a
**single one-shot write** in `LogXML.pytest_sessionfinish`
(`_pytest/junitxml.py:639-675`, pytest 9.0.3); `pytest_runtest_logreport` (`:531`)
only accumulates reporters in memory (`:518`). Nothing is incremental.

The under-reported consequence: because the file is opened `"w"` **only** at
`:644`, a truncated run does not merely fail to write its XML — **the PREVIOUS
run's XML survives untouched on disk.** Any verdict script parsing that path
without an mtime/freshness check reads the *previous* run's result. Combined with
the confirmed `rc=0 → PASS` first check in the consumer's `leg_verdict.sh`, that
is two independent false-green channels, not one.

**Mitigations to hand the consumer now, before any AIRA fix lands:**
- `set -o pipefail` / check `PIPESTATUS[0]` — if pytest is piped to `tee`, `$?` is
  `tee`'s status, which is a live candidate for the observed "exit 0" itself.
- Treat a `--junitxml` file whose **mtime predates the run start** as a FAIL, not
  a pass. This is cheap and closes the stale-XML channel independently.
- Run as `... -- sh -c 'pytest ...; echo "PYTEST_RC=$?"'` so the real interpreter
  status is recorded independently of any pipeline, plus `PYTHONUNBUFFERED=1` and
  `python3 -X faulthandler` so a hard death leaves a flushed tail and any fatal
  signal leaves a traceback.

### What the signature actually means

A log ending mid-progress at a flush boundary, with no summary and no traceback,
is **a hard, unflushed process death before `pytest_sessionfinish` wrote
anything** — a signal death or an `os._exit`, not a completed run whose trailer
was lost. (Deliberately weaker than an earlier draft of this analysis, which
claimed it *proves* `pytest_runtestloop` never returned; those `print()`s are
block-buffered under redirection, so their absence alone does not prove that. Same
practical conclusion: no XML for that run.)

### Remaining candidates, and the evidence needed to close them

1. **The "exit 0" may be the harness's, not pytest's** — see `PIPESTATUS` above.
   `leg_verdict.sh` already has a recorded history of misclassifying an aitest run
   (AIRA-31).
2. **Daemon restart mid-run** (raised by the Fable review; I had missed it). The
   worker-grant ledger is in-memory only (`worker_admit.go:87-110`) with no restart
   reconstruction — unlike AIRA-74 for confine reserves — and the relay does not
   watch its lease connection (`cmd/aira/main.go:1100-1103` watches only stdin and
   signals). After a restart `committed` resets to 0, the aggregate guard
   (`:275-281`) is defeated, and workers growing into their caps can trip the
   **outer scope's** `memory.oom.group` — a hard, unflushed, whole-tree kill that
   fits this signature well. **Evidence needed: `journalctl --user -u aira-daemon`
   over each incident window.**
3. **`dmesg` completeness.** "No OOM in dmesg" is only as strong as dmesg being
   readable (`kernel.dmesg_restrict`) and the window still being in the ring
   buffer. Worth re-checking explicitly.
4. Whether AIRA-92's stall and AIRA-91 are the same event with and without an
   external killer, as suggested elsewhere in this ticket — plausible but
   **unproven**, and the wchan discriminator recorded on AIRA-92 settles it cheaply
   on any live repro.

### Relationship to AIRA-92

PR #19 fixes AIRA-92's reproduced defect (unbounded blocking reads on the dispatch
loop). That may reduce AIRA-91's exposure if the two share a stall, but **AIRA-91
should not be closed by that PR** — its root cause is still open.

### Candidate 1 (tee/PIPESTATUS harness artifact) ruled out for qual's own repros (2026-09-04)

qual confirmed their `merge_gate.sh` `run_suite` uses `(eval "$cmd") >"$log" 2>&1`
with `rc=$?` capturing the subshell's own exit directly — no `tee`, no pipe
anywhere in the chain. Their standalone ad-hoc repros (`aira confine
--delegate-ram -- make test-services > log 2>&1 &`) are the same: direct `>`,
no pipe. So the observed `rc=0` on qual's three repros is genuinely pytest's
(or aitest's) own reported exit status, not a harness-piping artifact — still
worth checking for *other* callers' harnesses in general, but eliminated as
the explanation for the evidence this ticket is actually built on. Narrows
the remaining live candidates to: the daemon-restart-mid-run
whole-tree-OOM hypothesis (needs `journalctl` correlation against exact
incident timestamps), and whatever else a fresh investigation surfaces.

### Reproduced on a FOURTH, previously-"clean" leg, and a live daemon-restart correlation caught in real time (qual, 2026-09-04)

`admin` (its own fastapi/httpx/cf_access dependency profile — the fourth
disjoint profile, after `hosted`/`services`/`pipeline`) truncated with the
exact signature mid-merge-gate-run: dots stop, confine trailer, exit 0, no
pytest summary. qual's own interim guard (see above) correctly fired and
reclassified it BLANK/nonzero rather than a false PASS — the guard's first
real save, not just a design exercise. `admin` is now also pinned off aitest
(`FASTEST_NO_AITEST=1`), matching `hosted`/`services`/`pipeline`. qual's
earlier "backend/admin/obs/sym-gen clean, but provisional" caveat was
correct to hedge — `admin` is no longer clean.

**Possible live confirmation of candidate 2 (daemon-restart-mid-run), pending
exact timestamp**: this session restarted `aira-daemon.service` twice
tonight for unrelated deploys (AIRA-92, then AIRA-68) — `05:04:33` and
`05:54:09` local. If `admin`'s truncation falls in either window, that is
the strongest evidence this ticket has had: a live, timestamped trigger
rather than a stale incident reconstructed after the fact. Awaiting qual's
exact timestamp to confirm or rule this out. Further daemon restarts are
paused pending that answer.
