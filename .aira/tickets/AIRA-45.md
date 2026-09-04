---
{"schema":1,"id":"AIRA-45","project":"aira","title":"E_DAEMON_PROTOCOL classifier bucket conflates request-sizing errors with protocol-version mismatches","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
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

## Resolution — done (Phase 1 Fix 2, backlog-remediation plan §3.3)

Plan: `docs/superpowers/plans/2026-09-04-aira42-45-structured-outcome-channel-plan.md`.

Fixed **structurally, not by a finer substring**. The ticket's own candidate
direction was to match more of the sentence (`"daemon protocol is"`); that would
have been a seventh substring in the pattern AIRA-42 exists to end. Instead:

- `runnerAdmitResponseFrame` now decodes the response frame's `proto` field.
  `protocolMismatchFrame` (`internal/daemon/protocol.go`) is the ONLY daemon
  frame that sets it; `errorFrame` leaves it zero.
  `TestProtocolMismatchFrameCarriesProtoAndArgumentRejectionDoesNot` pins both
  directions.
- `classifyWorkerAdmitDaemonError` splits on that fact: a version skew is
  `class=admission-unusable reason=protocol-version-mismatch` (fall back, with a
  detail naming the remedy — reinstall and restart the daemon), the same
  disposition `aira confine`'s own admission client takes, which this ticket
  itself observed was the more useful answer. An argument rejection stays
  `class=request-invalid`.

This was live-reachable as of this very change, which bumps
`daemon.ProtocolVersion` **6 → 7** — the exact scenario the ticket predicted.
(Written as 5 → 6 while in review; AIRA-39 landed first and took 6.)

The terminal exception is also RENAMED `WorkerAdmitRequestTooLarge` →
`WorkerAdmitRequestInvalid`, matching its class token. This ticket's whole
complaint is that "cannot be admitted at this sizing" is the factually wrong
diagnostic; AIRA-39 then added two more members (`worker-scope-create-failed`,
`worker-id-space-exhausted`) that are about neither sizing nor the request.
Leaving the old name would have kept the wrong words on the most visible
surface while claiming the bucket was fixed.

`aitest/__init__.py`'s stale comment claiming the classifier "does not
recognize" `E_DAEMON_PROTOCOL` is corrected (the ticket's item 2).

Mutation M3 (drop the `proto != 0` discriminator) reproduces the original bug
and is killed by three tests.
