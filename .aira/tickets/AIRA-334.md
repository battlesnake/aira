---
{"schema":1,"id":"AIRA-334","project":"aira","title":"confine trailer hints: drop the non-OOM 'reached N% of the reserved cap' line, skip the over-reserve hint on an escaped scope, say the suggestion is from one run","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.42","labels":[],"hold":false,"relations":[]}
---


Filed from the 2026-11-14 rant triage (see docs/plans/2026-11-14-backlog-plan.md). Surface: client-only. Size: S. Correctness-critical (two-loop): no.

## Problem
Three advisory lines on the confine trailer give wrong advice. (1) `confine: peak RSS X reached N% of the reserved cap; consider a higher --memory-reserve` (internal/runner/confine_linux.go:1836-1838) fires for any non-OOM run at >=90% of its cap. Peak is cgroup memory.peak (internal/runner/usage_linux.go:182), which includes page cache, and the kernel keeps usage at memory.max by reclaiming cache, so every file-heavy job reads ~100%. Reproduced on master 692cb252: a 2 MB-maxrss `cat` of a 996 MB file under --memory-reserve 256M printed peak-rss=256M and the line. Because every scope has memory.swap.max=0 (confine_linux.go:960-969), a run that was not OOM-killed never needed more anonymous memory than its cap, so the line is wrong advice that pushes sessions to over-reserve the shared slice (RANT-73/87/111/68). (2) The ACTION REQUIRED over-reserve hint (internal/runner/confine.go:1179-1220) ignores scope-integrity, so a snap tool that moves itself out of the scope gets 'peaked at 17M, use 256M' from a peak that excludes it (RANT-108). (3) The over-reserve suggestion is this run's peak + 50% (confine.go:1142-1154) but reads as a sizing for the command; a warm build at 1148M was told 1792M and its cold rebuild OOM-killed (RANT-109).

## Evidence
Probe output (2026-11-14, master 692cb252): `maxrss_kb=1988 ... anon 466944 / file 264204288 ... terminated-by=normal peak-rss=256M ... confine: peak RSS 256M reached 100% of the reserved cap 256M; consider a higher --memory-reserve or --delegate-ram for suites`. Code: usage_linux.go:182 reads memory.peak; confine_linux.go:960-969 sets swap.max=0 unconditionally; confine_linux.go:1836-1838 at-cap line; confine.go:1179-1220 hint without an integrity guard; confine.go:1142-1154 suggestion from a single peak.

## Minimum fix (from-scratch minimum)
Client-only, three edits plus docs. (a) Delete the final non-OOM branch of formatConfineReserveAdvisory (confine_linux.go:1833-1838); the OOM branches are unchanged. (b) In formatConfineReserveHint, return "" unless status.ScopeIntegrity is a value that proves the scope held the job's processes (refuse descendant-escaped and migrated). (c) Reword the hint so the number reads as coming from this run only, e.g. '...this run peaked at X (this run only; a command whose peak depends on a build cache needs its cold-run peak)...', keeping the imperative AIRA-311 form. (d) One sentence in the skill (internal/core/skill.go:320 peak-rss paragraph) and the trailer docs: peak-rss is the scope's cgroup memory.peak and includes reclaimable page cache, so it is an upper bound, not process RSS. Deliberately NOT included: renaming the peak-rss facet (other projects parse it and the --summary-file keys), an anonymous-memory peak (cgroup v2 has no anon peak counter, so it would need sampling on the 50 ms monitor tick, which under-reads short spikes), a PSI-based 'slowed by reclaim' signal (measured: the pure cache stream still showed 0.16 s full stall in 1.5 s, so it does not separate cache from need), having the hint read the daemon's history (a protocol change), and any estimator change (AIRA-318, AIRA-273, AIRA-225 decide those).

## Tests
Unit: formatConfineReserveAdvisory(oom=false, peak=cap) returns "" (replaces the expectations at internal/runner/confine_linux_test.go:805-839); mutation: restore the branch and it goes red. formatConfineReserveHint with an otherwise eligible status and ScopeIntegrity=descendant-escaped (and migrated) returns "", and with unverified or descendant-killed still returns the line; mutation: drop the guard and it goes red. The hint text contains the 'this run only' qualifier. Real-cgroup regression (skips without delegation, runs without -race): stream a file several times the cap through `cat` under a 256M pin and assert exit 0, peak-rss at or near the cap and no 'reserved cap' advisory line (the RANT-73 shape). Run `aira confine -- make ci` for the codes catalogue and skill-text checks.

## Files
- internal/runner/confine_linux.go
- internal/runner/confine.go
- internal/runner/confine_linux_test.go
- internal/core/skill.go

## Related tickets
AIRA-318, AIRA-225, AIRA-273, AIRA-310, AIRA-311
