---
{"schema":1,"id":"AIRA-61","project":"aira","title":"aira confine supervisor burns 25-65% CPU from a 2ms O(tree-size) scope-membership sampler","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["confine","dogfood","performance"],"hold":false,"relations":[]}
---
## Symptom

Live-observed by the project owner via `top`: two `aira confine --delegate-ram` supervisor processes dominating CPU (65.0% and 25.2%, later 44.3%) on an otherwise-idle shared box, while their actual child work (pytest workers under a `make merge-gate` run) each showed only 2-4% CPU individually. An initial guess that this was legitimate output-relay overhead from verbose test output was explicitly rejected by the owner (no comparable CPU signature on the producing side) and investigated properly instead of assumed.

## Root cause (measured, not guessed)

`monitorScopeMembership` in `internal/runner/runner_linux.go` ticked every **2ms** for the entire job lifetime, doing an O(scope-tree-size) sweep of `/proc` on every tick: `cgroup.procs`, `/proc/<pid>/stat`, `boot_id`, `/proc/<pid>/cgroup`, and a `mountinfo` re-scan. A separate `scopeMembershipEvents` inotify watcher busy-polled its fd at 1kHz on top of that.

Evidence: the live `make merge-gate` supervisor showed 13,000 `/proc` reads/sec against only 86 actual output writes/sec (a ~150x ratio, directly refuting the output-relay theory) at 25.4% CPU with a 3:1 sys:user split (syscall-bound, not computation-bound). Controlled reproduction: a quiet `sleep` under confine cost 12.8% CPU; a 31-process tree of sleeps cost 112.5%; ~1M lines/sec of synthetic stdout changed nothing (14.8%, unchanged) because stdout is handed to the child as the supervisor's own fd and never relayed through user-space at all. CPU tracked scope-tree size, not output volume, conclusively. The `--delegate-ram`/aitest layout makes it worse: the tracked tree moves into a nested sub-cgroup, so tracked members take the expensive path on every single tick.

**The buffering theory (owner's suggested direction, ~10ms/64KiB) does not hold for the observed case.** Child stderr genuinely is relayed one syscall pair per child write, and that path does cost real CPU under synthetic load (97% CPU at ~200k stderr writes/sec) — but the live job that started this investigation wrote only 86 lines/sec, nowhere near enough to explain what was observed. Recorded as a legitimate future optimization, not the cause of this bug.

## Fix

`scopeMembershipSampleInterval` raised from 2ms to a named 50ms constant (every "sub-2ms" claim in code comments and the 2026-08-24 descendant-escape-attestation design spec updated to match — this is a documented coverage reduction, not a silent one). The inotify watcher now parks in the runtime poller instead of busy-polling, with a robust exactly-once close.

Verified before → after, same measurement harness: quiet job 12.8% → 0.8%; chatty-stdout job 14.8% → 1.0%; 31-process tree 112.5% → 3.5%.

Went through external plan review (Sol, REVISE verdict, addressed) before implementing, given this touches the same scope-membership/descendant-escape-attestation machinery AIRA-20's original design depended on for correctness, not just performance. Two new tests proven red against the old code in a throwaway worktree before going green on the fix (sample-period floor; syscall count over a 200ms idle window). Six existing dwell-tuned tests re-tuned to the new interval (five in `internal/runner`, one in `internal/store` exposed by the first full `make ci` run). Full suite: build 0, vet 0, `go test ./...` 0, 12/12 packages.

## Deferred, worth their own follow-up if anyone wants the extra headroom

1. **Stderr relay cost.** Real under heavy synthetic load (97% CPU at ~200k writes/sec) even though it wasn't the live cause here — worth a cheap buffering pass on its own merits if a genuinely chatty job (not the 86 lines/sec case here) is ever profiled.
2. **Per-tick `boot_id`/`mountinfo` re-reads** could be cached once per job rather than re-read on every sample tick.
3. **`unverified` outnumbers `contained` 1695:118 in tonight's logs** — noticed in passing during this investigation, not explained; worth understanding whether that ratio is expected or itself a symptom of something.
4. **Pre-existing flaky test found, not touched**: `TestM20LauncherDefersACKAndBoundsReadiness` failed once during this work, passed 5/5 in isolation afterward — a real, pre-existing flake unrelated to this fix, noted here so it isn't lost.

## Done — merged `af407be1cee362d746d48e534dc68710def02c66` (PR #9), 2026-09-03

Not yet deployed — the installed binary still predates this fix, same as tonight's other merges (AIRA-51/53/54/55). Deploy is being batched rather than restarting the daemon repeatedly.
