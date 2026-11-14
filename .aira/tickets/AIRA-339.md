---
{"schema":1,"id":"AIRA-339","project":"aira","title":"confine --kill honesty (wording only): a delivered-but-unconfirmed kill says so; a dead-supervisor empty scope says it has ended, not 'retry'","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.44","labels":[],"hold":true,"relations":[]}
---


Filed from the 2026-11-14 rant triage (see docs/plans/2026-11-14-backlog-plan.md). Surface: both. Size: S. Correctness-critical (two-loop): no.

## Problem
An operator's own 'aira confine --kill' is misreported twice. (1) The killed job's supervisor trailer says terminated-by=unattributed-sigkill, with the advisory 'SIGKILLed by something this supervisor cannot attribute' (internal/runner/confine_linux.go:2850-2880, 2956). The kill path never tells the supervisor, so the classifier cannot tell aira's own cgroup.kill from systemd-oomd or a stray kill -9 (RANT-74, RANT-102). (2) After cgroup.kill has been written, so SIGKILL is already delivered to every member, the 2s emptiness wait (confine_manage_linux.go:844-850; KillConfine timeout confine_manage.go:1038) can time out, and the reply is U_CONFINE_KILL_UNCONFIRMED 'scope did not become empty'. That reads as a failed kill. The daemon also writes no confine-kill audit line for it (internal/daemon/confine_manage.go:430-438, whose comment wrongly says an unconfirmed kill 'kills nothing').

## Evidence
RANT-102's kill is logged by the daemon ('Nov 05 22:37:54 ... confine-kill: killer=cc-f4d81d1b... target-scope=CONFINE-capchar-ltsim-pilot-3664512-dmnkdz0j769k...'), yet its trailer read unattributed-sigkill. RANT-74 has no confine-kill line on 2026-10-30, which is consistent with the unconfirmed branch skipping the log. xattr probe output: 'set ok, read: b'1'' in scope .aira-CONFINE-rant74-xattr-probe-2474770-dmuxbi811r3d@cc-82df3448-....

## Minimum fix (from-scratch minimum)
Wording and one log line only. (a) After cgroup.kill was written the 2s emptiness wait can time out: keep code U_CONFINE_KILL_UNCONFIRMED but say 'SIGKILL delivered to every process in <scope> (cgroup.kill succeeded); the scope was not yet empty after 2s, processes may still be exiting; check aira confine --list'. The daemon logs confine-kill with confirmed=false for that case (none if the cgroup.kill write itself failed); fix the misleading comment at internal/daemon/confine_manage.go:430-438. (b) The 'unattributed-sigkill' advisory lists its candidates as 'confine --kill, the memory watchdog, or an operator kill -9' without claiming which (no mechanism to tell). (c) Folded in from AIRA-319 (RANT-63/76): --kill on an empty scope whose supervisor is dead answers 'scope is empty and its supervisor is gone; the reaper removes it', not 'nothing to kill yet; retry' (internal/runner/confine_manage_linux.go ~841-842); AIRA-319's 'job may be queued' half is moot after AIRA-328. Deliberately NOT included: the xattr marker so the trailer could report terminated-by=confine-kill (a mechanism for one message; cgroupfs user xattrs on this kernel were probed once, not verified broadly), any daemon-to-supervisor protocol message.

## Tests
Wording tests: an injected waitEmpty timeout gives the 'SIGKILL delivered' message and a confirmed=false daemon log line; an injected scope.Kill error gives neither. A dead-supervisor empty-scope --kill gives the 'has ended' reply and not the retry reply; the live granted-but-unscoped case from AIRA-328 still says retry. The unattributed advisory text lists the three candidates. Mutation-verify each.

## Files
- internal/runner/confine_manage_linux.go
- internal/runner/confine_linux.go
- internal/daemon/confine_manage.go
- internal/core/skill.go

## Related tickets
AIRA-70
