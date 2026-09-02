---
{"schema":1,"id":"AIRA-45","project":"aira","title":"E_DAEMON_PROTOCOL classifier bucket conflates request-sizing errors with protocol-version mismatches","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
---
Found by Fable build-review (re-gate round 3, AIRA-30).

**The gap.** `acquire_worker`'s classifier (`internal/pylib/aitest/
supervisor.py`) routes ANY message containing `E_DAEMON_PROTOCOL` into
the terminal `WorkerAdmitRequestTooLarge` bucket. This was added
specifically to close the estimated-bytes-above-`admitMaxReserve` gap
(a per-request sizing problem), but `E_DAEMON_PROTOCOL` is also the code
the daemon's own protocol-version check emits
(`server.go`: `"E_DAEMON_PROTOCOL: daemon protocol is N, client requested
M"`) -- a completely different condition with a completely different
correct response.

**Failure scenario.** After a future `ProtocolVersion` bump (bumped
freely under this project's no-compat rule; currently unchanged at both
the branch's merge-base and HEAD, so NOT reachable today), a freshly
built worktree client run against a stale installed `aira-daemon.service`
gets the version-mismatch shape on every `worker-admit` call. The
classifier's substring match fires, `WorkerAdmitRequestTooLarge` is
raised, `_fail_queue_too_large` marks EVERY queued test unevaluated, and
zero tests ever run -- a loud, honest, nonzero exit, but a factually
wrong diagnostic ("cannot be admitted at this sizing") for what is
actually a stale-binary problem, and needlessly sterile (a fresh
`aira-daemon.service` install would fix it, but nothing says so).
Contrast: `aira confine`'s own admission client treats the identical
condition as daemon-unavailable (fallback), which is arguably the more
useful response here too.

**Candidate fix directions (not designed here).** Distinguish the two
`E_DAEMON_PROTOCOL` cases by matching on more than the bare code --
e.g. a distinct sub-string/field for the version-mismatch shape
specifically (`"daemon protocol is"` already appears in that exact
message and nowhere in the sizing-rejection one), routing THAT case to
`WorkerAdmitUnavailable` instead. Also update `aitest/__init__.py`'s
comment claiming the classifier "does not recognize" `E_DAEMON_PROTOCOL`
at all, which is now stale (AIRA-30's re-gate round 3 added recognition,
just not the right granularity of it).

relates AIRA-30, AIRA-42.
