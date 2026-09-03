---
{"schema":1,"id":"AIRA-67","project":"aira","title":"Governor-wide reset SIGKILLed a live job with no OOM, no daemon restart — root cause unknown","status":"done","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["daemon","dogfood","governor"],"hold":false,"relations":[]}
---
## Symptom (reported by peer session `qual`, independently re-confirmed)

`aira confine --delegate-ram --memory-max 32G --memory-reserve 512M -- make merge-gate` died at `2026-09-03 21:33:33` with exit 137 (SIGKILL), after real progress (several legs passed clean first). The confine trailer showed `reserve-basis=fallback:daemon-unavailable` and `scope-integrity=migrated`.

At the exact same second, the daemon journal (`journalctl --user -u aira-daemon.service`) shows the CPU-worker governor's entire active-set collapsing from 15 active workers across 2 tracked jobs to 0 active, 0 jobs, via a rapid sequence of single-step decrements all within one wall-clock second:

```
21:33:33 governor active-set active=15 parked=1 jobs=2
21:33:33 governor enforce granted worker=35cd6e84d5d3ffee8e5f66a9bfe9dfb4 job=CONFINE-@dr-job-1239004-dl5uovg2g1yj (fresh acquire)
21:33:33 governor active-set active=15 parked=0 jobs=2
... (single-step drops: 12, 11, 10, 9, 8, 7 [jobs=1], 6, 5, 4, 3, 2, 1) ...
21:33:33 governor active-set active=0 parked=0 jobs=0
```

Critically, `CONFINE-@dr-job-1239004` belongs to a completely unrelated peer session (different worktree) and was reported still alive hours later — so BOTH jobs' entire tracked worker sets were zeroed simultaneously, but only `qual`'s job actually died.

## Ruled out (verified, not assumed)

- **Daemon crash/restart**: `systemctl --user status aira-daemon.service` shows continuous uptime through 21:33:33 with the same PID (1510511) before and after. Independently re-confirmed by re-pulling the exact same journal window with an explicit date.
- **Per-scope cgroup OOM**: zero `oom-kill` entries in dmesg at 21:33:33 for either job's scope. The nearest OOM events are ~19 minutes earlier and are clearly AIRA's own deliberate `.aira-m16-Test*` OOM-test fixtures, unrelated.

## What's been checked so far (from source, before dispatching a full investigation)

`internal/daemon/governor.go` has no sweep, prune, or PID-liveness-check logic anywhere — grepped for `dead|liveness|prune|sweep|orphan|Kill|SIGKILL`, no periodic background check found. `release()` (line 198) appears to be called explicitly/reactively from `governorConnection()` (line 648), not from any timer-driven sweep. This suggests the mass release may be a downstream consequence of something disrupting many worker connections nearly simultaneously, rather than the governor proactively deciding to kill anyone — but this is inference from absence, not a confirmed finding.

A comment near governor.go line 29 references a "stale connect-time deadline (server.go SetDeadline), which otherwise force-closed [something]" — a deadline-based force-close mechanism is exactly the kind of thing that could disconnect many live connections at once under some triggering condition, and is a lead worth chasing.

**Possibly-relevant new context, not yet correlated**: AIRA-62 (filed the same night) found the CLI silently substitutes `--memory-max` for `--memory-reserve` on delegate-ram requests — meaning qual's job, and likely others, were making grossly oversized (32GB+) reservations instead of the intended ~512MB. If multiple sessions were doing this simultaneously, that's a large amount of artificial ledger pressure and request volume hitting the daemon around the same time as this incident. Not established as causal — flagged as context worth checking.

## Status

Full investigation dispatched and in progress as of filing. This ticket exists so the finding isn't lost regardless of how long that takes — update with the actual root cause (or an honest "investigated, couldn't determine" writeup) once it concludes.

## Resolved (investigation complete, 2026-09-03)

**Premise correction first**: `CONFINE-@dr-job-1239004` did NOT survive as originally assumed — it died at 21:33:33 too, ~165ms apart from qual's job. This was not "one death plus a mystery reset" — both confined jobs on the box died together, and the governor faithfully reported the fallout.

**The governor is exonerated by positive proof, not absence of evidence.** `release()` (`governor.go:198`) has exactly one caller — a `defer` inside `governorConnection` (`governor.go:671`). An active worker's connection handler is blocked in a `select` that exits only via (a) its frames channel closing (a client-side EOF/read error) or (b) daemon shutdown (`s.governorStopping()`, only closed once, after the accept loop breaks — confirmed not to have happened: `NRestarts=0`, same `MainPID`, continuous uptime through the incident). Therefore **all 15 releases were client-side disconnects** — the governor was reacting, not acting. The registry-discovery and stale-deadline hypotheses were both cleanly ruled out too (different goroutine/lock path; deadline correctly reset on every governor write).

**Decisively not an OOM.** `aira.slice`'s `memory.events.local` shows `oom 0, oom_kill 0, oom_group_kill 0` — the slice has never hit its own cap since boot. Zero kernel messages in `journalctl -k` for the incident window (a real per-scope OOM 46s later, from a deliberate AIRA test fixture, IS present in the same log — confirming the log would have caught a real one). Neither job's confine trailer printed the OOM advisory.

**Actual root cause: an external SIGKILL hit both job trees within ~165ms of each other.** Three distinct kill paths — (1) SIGINT/SIGTERM delivered to a confine supervisor, which itself triggers `scope.Kill()` then forwards the signal; (2) an external `aira confine --kill` from another session; (3) a slice-level OOM with no scope cap set (ruled out separately, above) — all produce **byte-identical observable output**: supervisor survives, prints a normal-looking trailer, exit 137. None of them log anything identifying the actual trigger. The daemon's own watchdog and reaper are both cleared (watchdog logged nothing all day and cannot structurally target a confined child; the reaper only removes provably-empty cgroups and always logs when it acts — no reaper line anywhere near 21:33:33). The harness and fastest-ee's own scripts were checked too — no `pkill`/`kill`/`trap`/`timeout` anywhere in the relevant Makefile/merge_gate.sh.

**This diagnostic gap — not the identity of whoever/whatever sent the signal — is the actual bug.** AIRA currently cannot distinguish "someone signaled this job" from "someone ran confine --kill on it" from a subtler slice-OOM edge case, because none of the three paths record anything. That is fixable and is the highest-value recommendation below; the specific identity of the external kill on this occasion is not recoverable from evidence that was never recorded, and no further forensic effort on this specific incident is likely to change that.

### Two further live findings from this investigation, each worth their own ticket (filed as AIRA-68, AIRA-69)

- **A large, actively-growing ledger reserve leak** — verified live: 23 "admitted" jobs holding ~60GB+ of a ~62GB ceiling, but only 3 scopes actually exist. This is a major, currently-active contributor to tonight's admission saturation and the `fallback:daemon-unavailable` events several sessions hit (including qual's job, right before it died). AIRA-49 addressed stuck leases but this is a separate, still-open leak.
- **Test cgroups land on the shared production slice** when the cgrouptest binary runs directly inside a confine job scope (`internal/store/cgrouptest_linux.go:50`'s `host := filepath.Dir(current)` resolves to `aira.slice` itself in that case) — confirmed live, with a real `.aira-test-*` scope sitting as a sibling of production job scopes, counted by the admission scan and swept by the reaper.

### Recommended next steps (not implemented — needs the full two-loop, not a solo late-night patch)

1. **Highest value**: record teardown provenance on every silent-137 path — a `terminated-by=signal:SIGTERM|external-cgroup-kill|oom|normal` field in the trailer, plus one log line each in the supervisor's own signal handler and in the daemon's `confine-kill` dispatch (`confine_manage.go:141-152`, which currently has no `log.Printf` at all).
2. Fix the OOM-advisory gate (`confine_linux.go:817`), which returns empty and stays silent precisely when no scope cap was set — the case where a real OOM would be most surprising to see explained as a bare 137.
3. AIRA-62 (already filed) turned out broader than scoped there — see that ticket's own update.
