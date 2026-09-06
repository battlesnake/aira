---
{"schema":1,"id":"AIRA-128","project":"aira","title":"aira confine's default reservation estimate under-provisions heavy workloads (test-lite), producing phantom OOM-kill failures","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["admission","confine","estimator","honesty"],"hold":false,"relations":[]}
---
Reported by peer session 'speed' relaying peer 'money's' evidence, 2026-09-06 (landing
their #1199). Money's own numbers, quoted directly:

- Default `aira confine -- make merge-gate` (test-lite leg): **371 failed / exit 0** — verified
  fake: the leg's real peak is ~3.3GB (targeted subset) or more (full), while the default estimate
  caps it around ~1.26GB, so tests+workers get OOM-killed mid-run and the leg reports mass phantom
  failures rather than the run being honestly flagged as OOM'd/unevaluated.
- Re-run at `aira confine --memory-max 16G`: **5900 passed / 1 failed** (the 1 was an unrelated
  pre-existing base-red, now cleared).
- The narrower/targeted affected surface at `--memory-max 8G`: **555/0**.

Speed's framing: 'same PSI-OOM class as the earlier 44-phantom incident' — a precedent in their own
history for this failure shape, not (as far as I can find) an AIRA ticket already covering this
specific one. AIRA-64 (built, PR #42) is a related but DIFFERENT mechanism — admission-WAIT time
counted against a per-test timeout — not this estimate-too-low-for-the-workload problem.

## Why this is serious, not just a nuisance

This hits EVERY merge-gate run using the default estimate for this workload class. Worse than a
simple failure: 371 failures under exit 0 can either waste a whole gate run investigating fake
failures, or — the real danger — MASK a genuine failure hiding among the fakes. This is exactly the
class of thing AIRA's own honesty rule exists to prevent ("a check that cannot establish its result
reports unevaluated, never a fake pass or zero") — here inverted: a check that could not establish
its result (because its own execution environment killed it) is reporting a fake FAIL, which is
just as dishonest as a fake pass and arguably worse (it looks like real, actionable signal).

## Investigation started here, not finished — hand-off notes for whoever builds this

Read `resolveAdmitReserve` (internal/daemon/admit.go:~1452) before assuming a fix. It already has
a self-healing path: `stats.OOMCount > 0 && stats.MaxOOMPeak > 0` escalates the reserve to
1.5x the observed OOM peak ("estimate:oom-escalated") for the NEXT confine call with the same
signature. Open questions to resolve before designing a fix, not to be assumed either way:

1. **Did money's FIRST-run OOM actually get recorded against this signature's ConfinePeakHistory
   at all?** If oom.group's group-kill fired and was correctly attributed, a SECOND run at the
   plain default (no explicit --memory-max) should already show "estimate:oom-escalated" and a
   materially higher reserve — i.e. the system may already be DESIGNED to self-heal within one
   cycle, and the real gap is only the cold-start cost of the first, currently-undocumented OOM
   (which is an accepted, deliberate property of the whole peak-RSS estimator design going back to
   AIRA-50/52/67 -- NOT necessarily a bug to fix by itself). Verify with a real re-run at plain
   default (not --memory-max) immediately after the phantom-371 run, same signature, and check
   `confine --list`'s reserve-basis field.
2. **Or did the OOM NOT get recorded** (attribution gap) -- e.g. a nested aitest worker sub-scope
   absorbing the kill rather than the outer confine scope whose signature feeds the estimator, or
   some interaction where the outer scope's own OOM event isn't observed/counted. If so, THAT
   attribution gap is the real defect, not the estimate's starting value.
3. **Timing coincidence worth checking explicitly, not assumed:** AIRA-110 (this same session,
   merged only hours before this report landed) changed every confine scope's default from
   'swap allowed' to `memory.swap.max=0`. If this exact workload previously survived a transient
   overshoot past its (already low) estimate by swapping, rather than by the estimate being
   adequate, AIRA-110 could have converted a previously-silent-but-harmless overshoot into a hard
   OOM-kill for the first time. This does not make AIRA-110 wrong (letting a confined job swap
   defeats the whole point of a hard cap, and was a deliberate, reasoned decision, recorded in that
   ticket) but it may explain why this is surfacing NOW rather than being a long-standing latent
   bug, and is relevant context for how urgently the estimator side needs to catch up.
4. **The 'exit 0 despite 371 failed' detail is suspicious on its own** and may be entirely outside
   AIRA's domain (a consumer-side test-runner/wrapper tallying issue in `make merge-gate`/
   `merge_gate.sh`, not this repo) — do not conflate the two. If a real, full-cgroup OOM group-kill
   (memory.oom.group=1) fires, the top-level process should die non-gracefully, not produce a
   clean-looking pytest summary. Establish which of AIRA's own signals (confine's own exit/trailer)
   the consumer's wrapper is or isn't honouring before concluding this needs an AIRA-side fix at all
   beyond the estimate.

## Fix directions (not decided here)

- If (1) above is confirmed (self-heals in one cycle): the fix may be as small as ensuring the
  cold-start/first-OOM case is itself reported honestly (unevaluated, not a phantom failure) rather
  than changing the estimator's starting value at all — consistent with architectural-simplicity
  (don't build new estimation machinery for a system that already self-heals).
- If (2) is confirmed (attribution gap): fix the specific attribution path, scoped narrowly to
  whatever nesting/timing hides the OOM from ConfinePeakHistory.
- Speed's suggested workaround (pin an explicit `--memory-max` floor on the test-lite target) is a
  CONSUMER-repo change (merge_gate.sh / the test-lite Makefile target), not an AIRA-side fix, and is
  explicitly out of this ticket's scope -- coordinate with peer session 'speed' (who sequences
  gate-machinery edits) if that workaround is what ships short-term while this ticket is open.

## Tests

Whatever the root cause turns out to be, the regression test must reproduce the ACTUAL failure
mode end-to-end under a real cgroup (a workload whose peak genuinely exceeds a cold-start estimate,
run under aira confine, memory.swap.max=0) and assert either: the run is honestly reported as
OOM/unevaluated rather than a phantom failure tally, or (if fix direction 1) that a second run for
the same signature is admitted at a materially higher, OOM-escalated reserve. A test that merely
calls resolveAdmitReserve directly with synthetic OOMCount>0 stats would be porous for hypothesis
2 -- it must exercise a REAL OOM event's attribution path, not just the escalation arithmetic.
