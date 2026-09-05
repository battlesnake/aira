---
{"schema":1,"id":"AIRA-108","project":"aira","title":"confine-reserve --pinned --max-wait not honoured: 52min hang past a 300s declared bound, 51.6GB free","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["admission","confine","dogfood"],"hold":false,"relations":[]}
---
Reported by peer session `split` (fastest-ee, `make test-engine`), 2026-09-06.

## The incident

`make test-engine` (`uv run pytest -q -p aitest -n 0 --aitest-workers=auto`) stalled at 99% progress, log idle for 740s. The stuck process, confirmed still running via `ps`:

```
aira confine-reserve --bytes 1047035904 --pinned --signature pytest:tools/correctness/test_dispatch_coverage.py::test_board_temp_range_has_a_reader_and_is_not_on_the_allowlist --slice aira.slice --max-wait 300s
```

**The anomaly, precisely stated:** this process had been alive for ~3118s (52 minutes) at the time split observed it — over **10x** its own declared `--max-wait 300s`. `MemAvailable` was 51.6GB at the time (no real memory pressure that would justify a legitimate long admission wait). So this was not a slow-but-honest wait; the process failed to honour its own declared bound and exit, one way or another.

split recovered by killing their own confine supervisor (`kill -INT`, scope cleaned correctly per `terminated-by=supervisor-signal:SIGINT`), never touched the shared slice, and re-ran successfully. Not reproduced on demand — a single live observation, credible and precise (exact command, exact timings), but not yet independently confirmed.

## What this call is, precisely — traced against source, not assumed

`aira confine-reserve --pinned` is the per-test RAM-governor helper built for AIRA-69 (extends the CPU governor to bound RAM: `internal/runner/confine_reserve*.go`, `runConfineReserveCommand` in `cmd/aira/main.go`). The `-p aitest -n 0` invocation shape strongly suggests this was actually spawned by the **legacy `aira_xdist_governor` plugin's** `_acquire_reservation` (`internal/pylib/aira_xdist_governor/__init__.py`), which is registered by many fastest-ee conftests whenever `AIRA_PY_LIB` is set — regardless of whether aitest is ALSO active for the same run (the exact double-arming shape AIRA-77 documented and closed as "redundant reservation, not correctness-critical, until AIRA-33 deletes the plugin"). **AIRA-33 (retiring that exact plugin) is being built right now** — once it lands, this specific CALLER goes away. That does not necessarily close the underlying question below.

## The open question this ticket is actually about

Independent of which caller triggered it: does the `confine-reserve` CLI/daemon path have a genuine bug where a `--pinned --max-wait N` request can fail to honour its own declared bound and hang indefinitely past it? Candidate mechanisms, not confirmed, for whoever investigates:

1. **CLI-side enforcement gap.** `internal/runner/admission_linux.go`'s `admitThroughDaemon` derives its transport deadline directly from the requested `maxWait` (`AIRA-58`'s own fix, `admission_linux.go:341-358`) — in principle this should tear the connection down at `maxWait + admitTransportGrace` regardless of daemon behaviour. Trace whether this path is actually reached for a `--pinned` reserve request, or whether `confine-reserve` routes through a different code path with a similar-looking but distinct enforcement gap.
2. **Post-admission hang.** AIRA-65 (closed tonight as not-needed) already traced that the reserve helper's SIGINT/SIGTERM handling correctly tears down via `WorkerAdmitLease.Close()` — but that's the *signal-delivery* path. This incident shows a *natural-timeout* hang, which is a different code path (whatever fires when `admissionMaxWait` is naturally exceeded, not when an external signal arrives) — confirm that path is actually exercised and correct, not just the signal path.
3. **A genuinely granted-but-never-read response**, e.g. the caller (Python governor plugin) itself blocking on `process.stdout.readline()` unboundedly (the exact shape AIRA-91's own investigation found in a *different* aitest code path, `Supervisor.acquire_worker()`) — meaning the Go CLI process may have correctly exited or been ready to, while ITS OWN stdout was never drained by the Python side, keeping the process from ever completing an OS-level `wait()`. This would make it a caller-side bug, not a daemon/CLI one.

Not established which of these (if any) is the actual mechanism — that tracing is this ticket's scope.

## Why this matters even after AIRA-33 lands

If the root cause is (1) or (2) above — a genuine CLI/daemon-side enforcement gap in `confine-reserve --pinned --max-wait` — it is latent for ANY future caller of that primitive, not just the plugin being deleted tonight. If it is (3), it's specific to the plugin AIRA-33 removes and this ticket can close as moot once that lands and is confirmed to be the mechanism. Recommend not closing this ticket on AIRA-33 landing alone — confirm which mechanism it actually was first.

## Confirming data point (split, 2026-09-06) — narrows candidate (3)

split confirmed the legacy governor was genuinely double-armed on their run (their worktree is pre-AIRA-33, off #1172; `git grep aira_xdist_governor` hits 10 fastest-ee conftests including the one that fired). More importantly: the wedged process was a **single PID** (1264500) alive for the full ~52 minutes, with `--max-wait 300s` on its own argv — not a re-spawn loop (which would show a fresh PID every ≤300s). One process outliving its own declared bound by ~10x is real evidence of a genuine wait-bound enforcement gap, independent of the caller.

**This does not fully rule out candidate (3) by itself**, though — a single long-lived PID is also consistent with the Go process finishing its wait correctly and then blocking indefinitely on a `write()` to stdout that nothing is reading (an undrained pipe blocks the writer regardless of how correctly the wait itself was bounded). Asked split, if they catch a live repro again before killing it, to check `/proc/<pid>/status` (State field) and `/proc/<pid>/fd/1` (what stdout is connected to) — this distinguishes "still executing its own wait logic" from "finished, blocked writing an unread result" cleanly. split is actively re-running the same suite on the pre-AIRA-33 base right now and offered to hold a wedged process alive rather than killing it, so this may get a live, inspectable repro rather than a second after-the-fact report.

## Candidate (3) ruled out; candidate (1)/(2) confirmed live (coordinating session, 2026-09-06)

split caught a wedged instance LIVE and held it rather than killing it (pid 1736618, `confine-reserve --pinned --max-wait 300s`, at 506s elapsed — 200s past its own bound). Both sessions inspected the real process directly via `/proc` (this machine, no namespacing):

- `readlink /proc/1736618/fd/1` = a pipe **with a live reader** (the pytest process) — rules out candidate (3) (blocked-write-to-undrained-pipe) outright.
- `/proc/1736618/wchan` = `futex_do_wait` for the main thread; all 7 OS threads checked via `/proc/<pid>/task/*/wchan` — six on `futex_do_wait`, one on `anon_pipe_read` (almost certainly the known stdin-EOF-watcher goroutine from AIRA-65's own trace — benign, expected, not itself a symptom). Nothing blocked on real I/O, nothing spinning.
- `SigBlk`/`SigCgt` in `/proc/1736618/status` look like ordinary Go-runtime signal setup, nothing abnormal.

**Conclusion: this is a genuine, confirmed-live Go-side wait-bound enforcement gap** — every goroutine is correctly parked in its own logic (not stuck on I/O, not a caller-side pipe stall), but whatever timer/deadline is supposed to fire at the declared `--max-wait` is not firing. Independent of AIRA-33's governor deletion; latent for any future `--pinned` caller.

Asked split to send SIGQUIT to the held process before killing it (Go's runtime dumps every goroutine's exact stack on an uncaught SIGQUIT) — if captured, that will name the precise function/line each goroutine is parked at, turning this from "confirmed gap, mechanism unknown" into an exact fix location. Not yet received at the time of this note; update this section if/when it arrives, otherwise the diagnosis above stands on its own as sufficient to prioritise and scope the fix.

## Not reproduced by the coordinating session independently

Both observations are split's own live runs, inspected jointly in real time rather than reproduced from scratch by this session — the process itself was directly and independently examined via `/proc` (not just split's report of it), which is why the diagnosis above is stated as confirmed rather than merely credible.

## Correction: AIRA-33 does not and cannot unblock fastest-ee's own conftests

split confirmed fastest-ee `origin/master` (#1175, `7b5785cb5`) still has all its original `aira_xdist_governor` conftest registrations — AIRA-33 only deleted the plugin from the `aira` repo itself. That is by design (the owner's decision was "delete the AIRA-side plugin now, accept fastest-ee migrates its own conftests on its own timeline"), but it means AIRA-33 landing does **not** make this wedge stop recurring on fastest-ee — whoever owns those conftests still needs to do that migration (SKILL.md's guard, `lite/conftest.py:784-803`, is the reference to copy). Immediate workaround given to split: `-p no:aira_xdist_governor` on the pytest invocation, which disables just that plugin without touching `AIRA_PY_LIB` or aitest's own RAM governance.
