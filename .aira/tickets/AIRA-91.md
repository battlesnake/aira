---
{"schema":1,"id":"AIRA-91","project":"aira","title":"confine's trailer is indistinguishable from a clean run when systemd-oomd SIGKILLs the whole scope (ROOT CAUSE CONFIRMED — was never exit 0)","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["aitest","confine","dogfood","honesty"],"hold":false,"relations":[]}
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
memory watchdog cannot target aitest at all: it requires an uncapped ancestry,
which a confine scope never has (`internal/daemon/watchdog.go:344`, `:553`).
[CORRECTED 2026-09-05 by AIRA-16 first half: this originally also cited a
blanket `.aira-`-component exemption at selection and re-validation
(`watchdog.go:600-607`, `hasAIRAComponent`). That exemption is DELETED —
capping is now the only exemption. The conclusion is unchanged and rests
entirely on the ancestry clause, which still holds: a confine scope always has
a finite `memory.max` ancestor. Noted here because AIRA-91 Part B is the owner
decision AIRA-16's second half was deferred to, so this ticket must not
describe the watchdog's predicate as it no longer is.]

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

### qual's own migration unblocked via interim mitigations (2026-09-04)

Attempt 5 of qual's fastest-ee merge-gate PR completed cleanly — no crashes,
no truncations, no BLANK legs, `admin` passing clean (confirming their
guard-regex fix). Remaining FAILs are pre-existing drift unrelated to their
branch. Their migration is done and heading to review — unblocked entirely
by the interim mitigations built during this investigation (pin
hosted/services/pipeline off aitest, the `"aitest: "` summary-line
completeness guard on the other 6 legs), NOT by a fix to AIRA-91 itself,
which remains open with its root cause still unestablished. Noting this so
the ticket doesn't read as resolved-by-attrition just because the one
consumer chasing it nightly is now unblocked.

**RETRACTED (qual, minutes later, same 2026-09-04): this `admin` incident was
not real.** On a proper re-read, `admin` completed successfully — full
`"aitest: 211 passed, 0 failed, 2 skipped, ..."` line and pytest's normal
trailer, genuinely not truncated. It did run `E_CONFINE_UNAVAILABLE:
worker-admit unevaluated: unbounded -- falling back to ... UNCONFINED` (the
documented graceful daemon-unreachable degradation path), and completed
correctly unconfined. The false BLANK reclassification was qual's own
guard's regex only matching pytest's native `^[0-9]+ (passed|failed|...)`
summary shape, not aitest's own `"aitest: N passed..."`-prefixed one — a
bug in the guard, not in aitest. So: **still 3 legs
(`hosted`/`services`/`pipeline`), not 4** — the fourth-leg generalization
claim above is withdrawn. The daemon-restart-mid-run hypothesis remains
exactly where it was before this incident: plausible, unconfirmed, no
timestamp chase performed (correctly aborted before it started). Left the
retracted section above in place rather than deleting it, since the
guard-regex gap it exposed is itself a real, useful finding for BL-969-style
work even though the incident itself wasn't AIRA-91.

### BREAKTHROUGH: real exit code captured directly for the first time — 137 (SIGKILL) via systemd-oomd, not exit 0 (qual, 2026-09-04)

A fresh, dedicated investigation was restarted (worktree
`../aira-91-root-cause`, plan/findings to be recorded once it concludes).
While it ran, qual attempted a live, precisely-instrumented repro of `hosted`
under aitest (bypassing their own pin, for diagnosis only): a continuous
`journalctl --user -u aira-daemon -f` tail for the whole attempt, and
`ec=$?` captured directly after the inner command with no pipe.

**Result: `ec=137` (SIGKILL) — not 0.** Every prior "exit 0" observation this
whole investigation was built on had been *inferred* through an outer
wrapper (`aira confine`'s own reported status, or `make`'s), never captured
directly on the actual command until now. This is the single most important
piece of evidence the investigation has had all night.

**Source, confirmed three ways**: not aira-daemon (zero log lines in the
whole window — total silence), not the kernel OOM killer (`dmesg` clean),
but **`systemd-oomd`**, one second before the kill:
```
systemd-oomd[1037]: Considered 75 cgroups for killing, top candidates were:
    Path: .../aira.slice/.aira-CONFINE-@dr-job-2219310-...
    Pressure: Avg10: 86.85% Avg60: 82.50% Avg300: 63.30% Total: 6min
    Current Memory Usage: 7.0G
```
— the job's own confine scope, top candidate, killed. `/etc/systemd/oomd.conf.d/aira-oomd.conf`
is itself aira-managed and deliberately tightens oomd's default thresholds
(`DefaultMemoryPressureLimit=40%`/`DefaultMemoryPressureDurationSec=10s` vs
stock 60%/20s, "to catch growth earlier than kernel OOM") — the job's
86.85% sustained for 6 minutes blew well past even that tightened bar.

**Why this is plausible as a real, general mechanism**: the job was fully
`aira confine`-managed and within its own accounting (`oom.group=set`,
`cap=enforced(64G)`, `reserve=512M pinned:client`) — but systemd-oomd's PSI
pressure metric is per-cgroup while being *driven by system-wide contention*.
With several concurrent heavy jobs sharing `aira.slice`'s ceiling, a single
job's cgroup can show severe pressure from thrashing even while staying
under its own nominal reservation — oomd doesn't know or care about aira's
ledger, it only sees pressure. This tracks every observed correlate: fires
only under real multi-session contention, leaves nothing in aira-daemon's
own log (it isn't aira's kill), and — now confirmed — produces exit 137.

**Open question, actively being chased**: does this mechanism explain the
*original* "exit 0" reports, or is it a separate phenomenon? A first-pass
check (grep, not exhaustive): the confine supervisor places only the child
process into the job's cgroup at spawn (`UseCgroupFD`/`CLONE_INTO_CGROUP`,
`confine_linux.go:769`) — the supervisor itself is never a member of the
scope it manages, so a cgroup-wide oomd kill should not take it down too. It
should survive, `wait()` on its dead child, and — per this investigation's
own earlier confirmed finding that `waitConfineCommand` always returns
`128+signal`, never 0 — correctly report 137, matching what qual just
captured. That suggests the earlier "exit 0" reports may have been a
*measurement* artifact (inferred through a wrapper layer) rather than a
second, distinct root cause — but this is preliminary, not confirmed; the
active investigation is checking it properly, including whether anything
about a cgroup-*wide* kill (as opposed to a kill targeted at one PID) could
still trip an edge case the direct-kill case wouldn't.

Full capture logs (qual's machine): `~/tmp/aira91-instrumented-hosted.log`,
`~/tmp/aira91-daemon-continuous.log`, `~/tmp/aira91-live-capture.log`
(wchan/ps snapshots from the healthy-progress period beforehand, for
contrast — the process was genuinely computing right up to the kill, not
stuck/wedged).

**Follow-up (qual, same night): every prior "exit 0" this ticket was built on
was an INFERENCE, never a direct capture.** Audited on request: one earlier
"0" was the exit status of a backgrounded shell job (`... &`), not a `$?`
captured inside the confine scope; every other "0" came from
`merge_gate.sh`'s own `run_suite()` bookkeeping (`rc=$?` after `eval "$cmd"`),
which qual trusted but never independently re-verified end-to-end — and
their own AIRA-91 completeness guard doesn't even look at `rc` for the
truncation signature, only log content (absence of the summary line). **Zero
verified "genuine exit 0" data points exist from tonight** — every one went
through at least one un-independently-verified intermediary layer. This
substantially strengthens the measurement-artifact reading above: it is now
plausible every AIRA-91 incident tonight has actually been this same
systemd-oomd/exit-137 mechanism, and the "silent exit 0" framing that this
entire ticket was filed and investigated under may itself have been
imprecise from the very first report. qual has offered to re-run one or more
of the OLDER pinned legs (`services`/`pipeline`) with the same direct-capture
instrumentation to check whether they also come back 137 — worth taking up
if it would meaningfully generalize the finding beyond `hosted` alone.

**Definitive audit, no new runs (qual, same night): traced EVERY truncation
incident from tonight — zero were ever a foreground-captured genuine exit
0.** Specifically: every ad-hoc repro was launched backgrounded
(`... > log 2>&1 &`); any "0" logged for those was the shell's own immediate
backgrounding return, not a captured `$?` on the job — and several were
never observed exiting at all (killed manually with `kill -INT` once judged
stalled). Within `merge_gate.sh`'s `run_suite()`, which DOES capture `rc=$?`
directly and in the foreground: no leg ever showed BOTH a genuine mid-run
truncation (dots stop, trailer glued on, no summary) AND `rc=0` through that
path. The `admin` "incident" that looked like this was a confirmed false
positive (a complete run, guard-regex miss — see above). The one time the
whole `make merge-gate` process died mid-leg ("attempt 4"), it took the
script down before `run_suite` ever reached its own `rc=$?` line for that
leg — no rc was captured there either, not captured-as-0. **This is now a
complete, closed audit, not a sample: the "silent exit 0" premise this
ticket was originally filed and investigated under has no surviving
supporting evidence.** The working hypothesis is now that every incident
tonight has been the systemd-oomd/exit-137 mechanism above, and "exit 0"
was never real. qual is now running `pipeline` (a fourth, still-untested
dependency profile under forced aitest) with the same direct-capture
instrumentation to further test this.

---

## ROOT CAUSE CONFIRMED (fresh investigation, worktree `aira-91-root-cause`, 2026-09-04)

**There was never an exit 0.** Root cause: `systemd-oomd` SIGKILLs the entire
confine scope (`cgroup.kill`) under real memory-pressure contention, exactly
matching qual's live capture above. This is proven by synthetic reproduction
matching the real incident artifact on 12 of 13 trailer fields, not inferred.

### The mechanism, end to end

1. `aira confine --delegate-ram` places the job's process tree (`make` ->
   `sh` -> `uv run` -> `pytest`) inside `.aira-CONFINE-*` with
   `memory.oom.group=1`. The `aira confine` **supervisor itself stays outside
   the scope** (confirmed: `UseCgroupFD`/`CLONE_INTO_CGROUP`,
   `confine_linux.go:769`).
2. Each aitest worker sub-scope is deliberately sized with
   `memory.high = estimatedBytes * 4/5` -- 80% of `memory.max`
   (`worker_admit.go:285`) -- so a worker at its intended working set sits in
   **sustained reclaim throttling by design**. qual's earlier live wchan
   capture caught exactly this (`__mem_cgroup_handle_over_high`).
3. That throttling *is* memory PSI pressure, attributed to the confine scope.
4. `systemd-oomd` kills the highest-pressure cgroup under `user-1000.slice`.
   It can reach AIRA's scopes because **AIRA installs the enablement itself**
   (`internal/install/install.go:967-970`): `ManagedOOMMemoryPressure=kill`
   on `user-1000.slice`, a tightened `ManagedOOMMemoryPressureLimit=40%`
   (Ubuntu default 50%), `DefaultMemoryPressureDurationSec=10s` (stock 20s),
   and `ManagedOOMPreference=avoid` on `session.slice` whose own comment
   says heavy work in **`aira.slice` will be preferred** for the kill --
   `aira.slice` itself carries `ManagedOOMPreference=none`.
5. oomd `cgroup.kill`s the scope. `make`/`sh`/`uv`/`pytest`/every worker die
   simultaneously, mid-write, unflushed. `aira confine` survives (it was
   never a member), prints a **completely normal-looking trailer**, and
   correctly returns **137** -- `waitConfineCommand` was right the whole
   time (`confine_linux.go:1334-1348` -> `main.go:956`). **This was never a
   wrong-exit-code bug. It is a missing-diagnostic bug.**

### Why nobody saw it

The only post-run diagnostic, `formatConfineReserveAdvisory`
(`confine_linux.go:872-890`), fires on `memory.events` `oom_kill > 0`. **A
userspace `cgroup.kill` never increments that counter** -- it's the kernel
OOM killer's counter, not systemd-oomd's. So the one mechanism AIRA has for
saying "your job was killed" is structurally blind to a kill mechanism AIRA
itself configured onto the machine. The trailer prints identically to a
clean run.

### Proof, not inference

Synthetic reproduction, no contention manufactured
(`~/tmp/aira91-probe/`, committable): a `selfkill` probe (kills the leaf
python only) produces a `make: *** Error 137` line and shell rc 2. A
`groupkill`/`groupkill_dr` probe (kills the whole scope via `cgroup.kill`,
matching oomd's actual mechanism) produces **no** `make` error line and
shell rc **137** -- and its trailer matches the real incident artifact
(`~/tmp/test-services-retry2.log`) on 12 of 13 fields, including
`reserve=512M reserve-basis=pinned:client`, `oom.group=set`,
`scope-memory.max=enforced=4294967296`, both truncating mid-line with the
trailer glued on with no newline. The one differing field
(`scope-integrity=migrated` on the real run) is corroborative, not
contradictory -- it requires `Monitor.LeaderMigrated`, i.e. the leader
observed alive-but-no-longer-a-member: `make` as a zombie between SIGKILL
and reap, the exact fingerprint of a whole-scope kill's teardown timing.

### Ruled out, with evidence (not just re-asserted)

- Kernel OOM anywhere in the scope tree -- the advisory line's absence on
  every real artifact means recursive `oom_kill == 0`; confirmed the
  advisory *does* fire correctly on a synthetic kernel-OOM probe.
- `aira.slice`'s own 64G cap OOM -- `memory.events.local` shows
  `oom 0, oom_kill 0`; the slice has never hit its cap.
- The memory watchdog -- re-verified against current source, still
  structurally excluded at four sites.
- A shell/pipeline artifact -- `merge_gate.sh`'s `run_suite` has no pipe; a
  leaf-only kill demonstrably produces a `make` error line the real
  artifacts lack.
- aitest reaching a clean `sys.exit(0)` -- both fork sites correctly gate
  child-only cleanup; `testsfailed` accounting makes any incomplete run
  nonzero regardless.
- "No OOM in dmesg" was never real evidence -- the ring buffer on this box
  only reaches back a few hours, well past the earlier incidents' windows.

### Corrections this finding makes to earlier statements on this ticket

- **The BL-969 severity escalation (relayed to qual earlier tonight) was
  wrong.** `leg_verdict.sh`'s `rc=0 -> PASS` first check was never reached --
  rc was always 137. BL-969 is a real *latent* gap in that script, but it
  was never actually triggered by an AIRA-91 incident. Correcting this with
  qual directly.
- The stale-JUnit-XML finding **stands unchanged**: a 137 before
  `pytest_sessionfinish` writes no XML and leaves the previous run's file on
  disk -- still a genuine false-green channel for anyone reading that file
  directly.
- AIRA-92 is confirmed genuinely separate and already fixed -- AIRA-91 is an
  external kill of a *healthy, progressing* process, not a stall.
- qual's `"aitest: "` completeness guard remains the right primitive,
  unaffected by any of this.

### Fix direction (NOT built -- needs the standard two-loop, split in two)

**Part A -- honesty fix, should land regardless of Part B.** A SIGKILLed job
must never print a trailer indistinguishable from a clean run's.
`FormatConfineStatus` already has `result.Exit`; add a terminal facet that
distinguishes what's actually knowable: `oom_kill > 0` -> existing
kernel-OOM advisory; signalled with `oom_kill == 0` -> report it plainly as
an **external whole-cgroup kill** and name the realistic sources
(systemd-oomd, `cgroup.kill`, `aira confine --kill`). A trailer field, not
new machinery.

**Part B -- real owner-level policy fork, not a solo call.** Two
AIRA-owned mechanisms actively conflict: admission's design says "a job
inside its grant is safe, `memory.high` throttles rather than kills";
AIRA's own oomd configuration says "highest PSI dies, no ledger
awareness" -- and `memory.high` is what generates the pressure that trips
it. AIRA's own oomd config comment records this exact tradeoff going wrong
in the other direction on 2026-05-28 (the desktop got killed instead).
Candidates, each trading one failure mode for another: `ManagedOOMPreference=avoid`
on `aira.slice` itself; raising/removing `memory.high` so a well-sized
worker isn't permanently in reclaim; restoring stock oomd thresholds
specifically for `aira.slice`. **This needs explicit owner sign-off before
any of it is built** -- it is not a Fable-plan-review-and-proceed decision
the way most of tonight's work has been.

### Still open: qual's `pipeline` re-run, falsifiable prediction made

The investigation gave qual a specific, falsifiable prediction to test
against: `pipeline` should come back **rc=137 plus a `systemd-oomd`
"Killed .../.aira-CONFINE-..." line within a second or two of the last
progress byte**. Anything truncating with rc neither 137 nor 128+n would be
the first positive evidence of a second, still-unexplained mechanism.
Update this ticket with the result once it lands -- this is the last
confirmatory step before treating the root cause as fully closed rather
than "confirmed with one outstanding falsifiable check."

**`services` re-run: clean negative, not a falsification (qual, same night).**
Completed normally, rc=2 (known pre-existing unrelated billing-gate test
failures, "8 failed, 1446 passed ... in 474.94s"), `make: *** Error 1`
printed normally -- no truncation, no 137. Continuous `systemd-oomd` journal
tail showed no new activity during this run (the only oomd line present was
backlog from the earlier `hosted` kill already reported, printed by
`journalctl -f`'s history replay on startup, not a live event). Consistent
with the mechanism being genuinely contention-dependent -- this run
apparently didn't hit sufficient system-wide memory pressure, which is
exactly what the theory predicts sometimes happens, not a counter-example
to it. `pipeline` (the actual falsification target) is still running,
82%+ with no truncation signs yet as of this update.

**`pipeline` re-run: falsifiable prediction HELD, root cause fully confirmed
(qual, same night).** `REAL_EXIT_CODE=137`, trailer shows
`scope-integrity=migrated` (the exact whole-scope-kill signature), and the
`systemd-oomd` journal shows a direct match within seconds of the last
progress byte:
```
systemd-oomd: Killed .../aira.slice/.aira-CONFINE-@dr-job-2438729-.../ due
to memory pressure for /user.slice/user-1000.slice being 72.96% > 40.00%
for > 10s with reclaim activity
```
`job-2438729` is `pipeline`'s exact outer confine PID -- not a coincidental
correlation, a direct match, on a THIRD disjoint dependency profile
(fastapi/numpy/scikit-rf, vs. `hosted`'s uvicorn/pyjwt/scikit-fem/stripe and
`services`'s httpx/prometheus/stripe). Combined with `hosted`'s earlier
direct match and `services`'s clean (contention-dependent) negative, this is
as strong a confirmation as this investigation is going to get without
manufacturing artificial load. **Root cause is closed: `systemd-oomd`
whole-scope kill under real memory pressure, exit 137, never exit 0.**

## Status: root cause closed. Remaining work is Part A (build) and Part B (owner decision) above.
