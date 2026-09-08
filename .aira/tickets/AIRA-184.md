---
{"schema":1,"id":"AIRA-184","project":"aira","title":"A job OOM-killed marginally over its own ESTIMATED (non-pinned) cap has no in-run remedy, even with slice headroom to spare","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","estimator"],"hold":false,"relations":[]}
---

Peer report (split, 2026-09-08), refined after their own initial "zero
headroom" framing was checked and withdrawn — the 1.15x safety factor does
apply on the `estimate:p90-prior` path same as every other estimate path;
that part is not a bug. The refined finding is narrower and survives
verification.

## Verified from source

Split's arithmetic on the captured line: cap `1257889792` B, peak-rss
`1257902080` B — the job exceeded its cap by ~12 KiB (0.001%), i.e. the
1.15x-derived estimate was very nearly exactly right, and the job was
killed anyway, twice, on the same command shape, with ~37 GiB free in the
slice. `estimate:p90-prior` is confirmed genuinely machine-wide, not
per-command (`internal/daemon/admit.go:2936`, `internal/core/skill.go:324`:
"the machine-wide p90, which exists only once some signature in this
box's one shared state.db has three or more recorded peaks") — it is the
deliberate cold-start fallback for a signature with no history of its own
yet, distinct from `EstimateMemoryReserve`'s own per-signature
`estimate:max=N,n=S,f=115` path (`internal/runner/resource_estimate.go
:83-124`), which already accumulates history per `ResourceSignature`
(exact argv, `resource_estimate.go:66-79`) — so the "should this be
per-command instead of machine-wide" half of split's hypothesis does not
apply to the escalated path; it only ever applies to a genuinely
never-before-seen signature.

skill.go:324 already documents the exact failure mode split hit as
expected, self-healing-ACROSS-RUNS behaviour: "a job OOM-killed AT that
cap... its own measured peak is then already at what the slice can give:
such a re-run is refused E_ADMIT_TOO_LARGE... rather than retried." If
split's repeated pytest single-test-probe invocations vary by test node ID
(and therefore by argv, and therefore by `ResourceSignature`), each is a
genuinely distinct, never-before-seen signature that independently
cold-starts at the same shared p90-prior and can independently OOM at it —
consistent with, not contradicting, the documented design. This has not
been independently confirmed against split's actual argv.

## The narrower gap that survives

The documented self-healing is CROSS-RUN only (the next IDENTICAL
invocation is admitted higher). Nothing helps the SAME run that just died
marginally over an estimated, non-pinned cap while the slice has spare
capacity to grant a larger one immediately. Split's own framing, offered
without a recommendation: "a policy of 'on OOM at an ESTIMATED (not
user-pinned) cap, when the slice has headroom, retry once at a higher
cap' would have turned two dead runs into two slow ones."

## Not designed here — and a note on scope, for whoever gates this

An automatic in-place retry is new runtime machinery (new state, new
launch path, new safety bounds — retry-once-only, estimated-cap-only,
headroom-gated) and should be weighed against this project's own
architectural-simplicity preference for "keep the primitive, document the
gap" over stacking complexity per feature. A cheaper alternative that
stays within that preference: no retry at all, just a sharper
`terminated-by=oom` line for this specific case — cap-source names this
was an ESTIMATE not an operator pin, and the message could say so plainly
("killed at auto-estimated cap; rerun with --memory-reserve <higher> — the
slice had headroom") rather than requiring the reader to infer it, closer
to split's own suggested wording. Whether the auto-retry is worth building
at all, or whether the message-only version is the right stopping point,
is left for whoever picks this up.
