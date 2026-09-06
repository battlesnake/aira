---
{"schema":1,"id":"AIRA-128","project":"aira","title":"aira confine's default reservation estimate under-provisions the standalone `make test-lite` signature, producing phantom OOM-kill failures","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["admission","confine","estimator","honesty"],"hold":false,"relations":[]}
---
Reported by peer session 'speed' relaying peer 'money's' evidence, 2026-09-06 (landing
their #1199), NARROWED by speed's own follow-up correction the same day. Money's own numbers,
quoted directly:

- Default `aira confine -- make test-lite` run **STANDALONE** (its own command signature, keyed
  to its own ~1.26GB estimate): **371 failed / exit 0** — verified fake: the leg's real peak is
  ~3.3GB (targeted subset) or more (full), so tests+workers get OOM-killed mid-run and the leg
  reports mass phantom failures rather than the run being honestly flagged as OOM'd/unevaluated.
- Re-run at `aira confine --memory-max 16G`: **5900 passed / 1 failed** (the 1 was an unrelated
  pre-existing base-red, now cleared).
- The narrower/targeted affected surface at `--memory-max 8G`: **555/0**.

**SCOPE CORRECTION (speed, same day, with live evidence):** the original report over-generalised
to "every merge-gate run." It does NOT. `make merge-gate` keys to its OWN, separately-estimated,
CORRECTLY-SIZED signature (~38-41GB, reserve-basis estimate:max=38GB n=20), and test-lite's leg
runs INSIDE that outer confine scope via plain xdist, with NO nested confine call of its own — so
inside merge-gate it inherits the full ~41GB budget, not the standalone 1.26GB one, and does not
OOM there. Speed's own gate run (BL-969) was live proof: 24GB→37GB under a 41GB scope, healthy.
Speed has RETRACTED their earlier directive to pin `--memory-max 16G` on their merge-gate agent —
doing so would have shrunk the correctly-provisioned ~41GB merge-gate scope down to 16GB and could
have OOM'd the full engine leg instead, a worse regression than the one being fixed. **Do NOT
touch merge-gate's own estimate — it is correct.** This ticket's actual, narrower target is the
STANDALONE `aira confine -- make test-lite` command's own signature/estimate only — i.e. whatever
a developer or a differently-configured CI leg runs when it invokes test-lite directly rather than
through merge-gate's outer scope.

**FURTHER REFINEMENT (speed, same day, sharpens the standalone case):** the full standalone
`make test-lite` at unbounded `-n auto` (xdist's own worker-count auto-detection, unrelated to
aitest) STILL OOMs even at `--memory-max 16G` under box contention — the AGGREGATE across xdist's
own parallel workers exceeds 16G, not a single process's peak. money's verified workaround:
`FASTEST_XDIST_WORKERS=8 --memory-max 20G` (pin worker count AND raise the floor together; 16G
alone, speed's earlier number, is insufficient for the full unbounded run). This matters for the
fix design, not just the numbers: `-n auto` scales worker count with ambient CPU availability, so
a peak observed on a busy box (fewer workers spawned) under-predicts the peak on a quiet box (more
workers spawned) for the SAME command signature. The existing self-heal path in
`resolveAdmitReserve` (escalate to 1.5x a single observed OOM peak) assumes next-run usage tracks
past-run usage for the same signature — that assumption may not hold here if the workload's own
internal parallelism is not itself bounded by anything confine controls. Whoever builds this should
treat "does the workload's own concurrency scale with ambient CPU, independent of the confine
budget" as a real design input, not an edge case to special-case away — a fix that only escalates
the estimate without addressing unbounded internal parallelism may still under-provision on a
quieter box than the one that produced the training sample.

**CONCLUDING ARCHITECTURAL POINT (speed): the recommended fix is to make the input deterministic,
not to make the estimator smarter.** An `-n auto` signature's peak isn't a fixed property of the
command — it varies with ambient CPU, so no per-signature estimate (self-healing or otherwise) can
correctly predict it. Pinning worker count (`FASTEST_XDIST_WORKERS=8`, money's own workaround)
turns the aggregate into a function of the pin instead of the box's mood at run time, which makes
it something AIRA's EXISTING per-signature estimator can actually learn correctly — no new AIRA
machinery required. This matches architectural-simplicity: fix the input's determinism rather than
build estimation machinery to chase a moving target. Practical implication for whoever scopes the
build: worker-count pinning is a CONSUMER-repo change (the test-lite Makefile/xdist invocation),
already implemented as money's workaround — it may turn out AIRA-side code changes for this ticket
are limited to reporting the cold-start/under-provisioned case honestly (unevaluated, not a phantom
failure) rather than any change to the estimate itself. Confirm this scope reduction is actually
correct (not assumed) before starting a build — verify with a real re-run at plain default (no
--memory-max) against a WORKER-COUNT-PINNED test-lite invocation and check whether the existing
estimator already handles it correctly once the input is deterministic.

Speed's framing: 'same PSI-OOM class as the earlier 44-phantom incident' — a precedent in their own
history for this failure shape, not (as far as I can find) an AIRA ticket already covering this
specific one. AIRA-64 (built, PR #42) is a related but DIFFERENT mechanism — admission-WAIT time
counted against a per-test timeout — not this estimate-too-low-for-the-workload problem.

## Why this is serious, not just a nuisance

Even narrowed to the standalone signature, this hits every direct `aira confine -- make test-lite`
invocation. Worse than a simple failure: 371 failures under exit 0 can either waste a whole run
investigating fake failures, or — the real danger — MASK a genuine failure hiding among the fakes.
This is exactly the class of thing AIRA's own honesty rule exists to prevent ("a check that cannot
establish its result reports unevaluated, never a fake pass or zero") — here inverted: a check that
could not establish its result (because its own execution environment killed it) is reporting a
fake FAIL, which is just as dishonest as a fake pass and arguably worse (it looks like real,
actionable signal).

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
- Speed's originally-suggested workaround (pin an explicit `--memory-max` floor) is RETRACTED for
  merge-gate itself -- merge-gate's own ~41GB estimate is correct and must not be touched; pinning
  16G there would shrink a correctly-sized scope and could OOM the full engine leg instead. A
  `--memory-max` floor remains a legitimate consumer-side workaround ONLY for a direct standalone
  `aira confine -- make test-lite` invocation specifically, and would still be a CONSUMER-repo
  change (whatever Makefile target runs test-lite standalone), not an AIRA-side fix -- coordinate
  with peer session 'speed' (who sequences gate-machinery edits) if that ships short-term.

## Tests

Whatever the root cause turns out to be, the regression test must reproduce the ACTUAL failure
mode end-to-end under a real cgroup (a workload whose peak genuinely exceeds a cold-start estimate,
run under aira confine, memory.swap.max=0) and assert either: the run is honestly reported as
OOM/unevaluated rather than a phantom failure tally, or (if fix direction 1) that a second run for
the same signature is admitted at a materially higher, OOM-escalated reserve. A test that merely
calls resolveAdmitReserve directly with synthetic OOMCount>0 stats would be porous for hypothesis
2 -- it must exercise a REAL OOM event's attribution path, not just the escalation arithmetic.
