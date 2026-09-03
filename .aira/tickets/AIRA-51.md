---
{"schema":1,"id":"AIRA-51","project":"aira","title":"confine's \"waiting for admission\" progress line shows a stale reserve that disagrees with what's actually granted","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["confine","dogfood","ux"],"hold":false,"relations":[]}
---
Surfaced during AIRA-31's build: `aira confine -- python3 -m pytest -q internal/pylib/aitest/` (no estimator history for that exact argv) printed `confine: waiting for memory admission on aira.slice (reserve 4G, waited Ns)` repeatedly while queued (20+ minutes under real contention that day — three jobs holding 41.7G of a 63.2G ceiling), but the job's own eventual grant, reported in the same run's final confine summary line, was `reserve=243094323` (~232MB) — the waiting message and the actually-granted reserve disagree by roughly 17x.

A DIFFERENT invocation of the same underlying test suite with `-p no:cacheprovider` (a different argv signature, which DID have estimator history) was correctly sized at 232MB from the start, with no long wait — confirming the 4G figure in the waiting message wasn't a fluke; it's whatever the (unpinned, no-history) request initially asked for, not what the daemon actually ends up granting once evaluated/admitted.

This is misleading to an operator (or an agent) watching the progress line: "waiting on a 4G reserve" reads as "this job needs 4G and the slice is genuinely that tight," when the job's real footprint turned out to be ~232MB — a completely different picture of what's actually contending for the slice. Worth checking whether the initial estimate itself is the problem (an overly conservative no-history default, relates AIRA-24/AIRA-49's build-review dogfood note about oversized-estimate head-of-line blocking) or just the progress line failing to reflect a resize/re-evaluation that happens before the actual grant — these may be two different bugs wearing one symptom.

Not investigated further — a dogfood observation from AIRA-31's build run, not this ticket's own root-cause analysis. relates AIRA-24, AIRA-49.

## Done + deployed

Root cause confirmed as the second of the two suspected bugs: the progress line was never wrong about the CLIENT's own request — it's a genuine no-history fallback hint (`DefaultConfineMemoryReserve`, unpinned) sent to the daemon as a starting point. The daemon (`resolveAdmitReserve`, `internal/daemon/admit.go`) resolves the real, admission-gating reserve from peak-RSS history or a machine-wide p90 prior before the job is even queued, and that resolved figure — not the client's hint — is what actually gets granted and contended over. The client only learns the real figure on the single blocking admit response (no mid-wait channel exists to poll it, deferred to AIRA-24), so the progress line was presenting its own stale hint as if it were the number the slice was fighting over.

Fix (`internal/runner/confine_linux.go`): the admission-wait progress line now branches on whether the request is pinned. A pinned request (explicit `--memory-reserve`/`--memory-max`) is honored verbatim by the daemon, so its wording is unchanged and still accurate. An unpinned request now reads `confine: waiting for memory admission on <slice> (requested reserve <hint>, unpinned — the daemon resolves the actual grant, which may differ; waited <N>s)` — labelling the figure as a request hint rather than a bare "reserve", and saying up front that the real grant may differ.

Verified live during the build: the fix's own author reproduced the exact bug on a real confined `go build ./...` — showed `reserve 4G` for the full 1305s wait, then was actually granted `reserve=969036K` (~946MB), a ~4.4x disagreement, same shape as this ticket's original ~17x dogfood report.

Tests: `TestConfineAdmissionWaitEmitsProgressDiagnostic` (pinned path keeps the exact wording) + new `TestConfineAdmissionWaitDiagnosticHedgesUnpinnedReserve` (unpinned path carries the hedge). `go vet ./...` and `go build ./...` exit 0; `go test ./internal/runner/...` exit 0, `ok`, 64.114s.

PR #5 (https://github.com/battlesnake/aira/pull/5), squash-merged as `2af4cc050664f5186ad0924cb1ac61e813f2d188` (branch commit `e794e56`). Worktree and branch cleaned up.
