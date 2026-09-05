---
{"schema":1,"id":"AIRA-108","project":"aira","title":"confine-reserve --pinned --max-wait not honoured: 52min hang past a 300s declared bound, 51.6GB free","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","dogfood"],"hold":false,"relations":[]}
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

## Not reproduced here

This is a single live observation from split, not independently reproduced by the coordinating session. Recorded honestly, matching this project's own standard for reports of this kind (see AIRA-105 for the same disposition on a different finding).
