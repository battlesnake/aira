---
{"schema":1,"id":"AIRA-128","project":"aira","title":"aira confine's default reservation estimate under-provisions the standalone `make test-lite` signature, producing phantom OOM-kill failures","status":"in-review","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["admission","confine","estimator","honesty"],"hold":false,"relations":[]}
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

## Resolution (2026-09-06) — verified: hypothesis 1 confirmed, hypothesis 2 refuted, no estimator defect

Investigated by reproduction under `AIRA_REAL_CGROUP=1` on the live daemon before any
code was written, exactly as the hand-off notes required. **Every number below is a
measured exit code or a row read out of the daemon's own `state.db`, not an assertion.**

### What was reproduced

The incident's mechanism was reproduced exactly, including money's own `~1.26GB` figure.

1. **The cold-start cap is the machine-wide p90 prior, and on this box it is money's
   number to the byte.** A never-before-seen command:

   ```
   $ aira confine -- /bin/sh -c 'echo AIRA128-probe-a1 >/dev/null; sleep 2'
   confine: ... reserve=1260470067 reserve-basis=estimate:p90-prior ...
             scope-memory.max=enforced=1260466176 ... terminated-by=normal
   EXIT=0
   ```

   `1260470067 B = 1.17 GiB`. That is the "~1.26GB estimate" in the report: it is not a
   per-command estimate at all, it is `estimate:p90-prior`, the p90 of every OTHER
   signature's peak, applied as this scope's hard `memory.max` because the command has no
   history of its own.

2. **A cold-start OOM is already unambiguous — AIRA reports it honestly.** Same command
   shape, a workload whose real peak exceeds that cap:

   ```
   $ aira confine -- /usr/bin/python3 ~/tmp/aira128-hog.py 2000 --tag=A128R1
   confine: ... reserve=1260470067 reserve-basis=estimate:p90-prior ... scope-swap.max=enforced
             terminated-by=oom peak-rss=1230924K
   confine: job OOM-killed at its memory cap 1230924K (peak RSS 1230924K); raise the cap ...
   confine: the OOM fired at this scope's OWN memory limit.
   EXIT=137
   ```

   The workload's own success line never printed. `terminated-by=oom`, exit `137`, two
   advisory lines, all on stderr (so a consumer's stdout parsing is not corrupted).

3. **The reported "371 failed / exit 0" shape cannot be produced by AIRA.** The parallel
   -worker aggregate-overflow shape was run directly (four concurrent children, aggregate
   2 GiB, cap 1.17 GiB): `memory.oom.group` group-killed the whole scope, the trailing
   `echo ALL-CHILDREN-DONE-EXIT-0` never ran, `terminated-by=oom`, `EXIT=137`. A phantom
   tally under exit 0 therefore requires a consumer-side wrapper that swallows the status
   (`|| true`, a tee'd report parsed for a summary) — hand-off note 4's suspicion,
   confirmed, and consumer-repo territory as the ticket says.

4. **The self-heal works, in ONE cycle, with no operator action.** Re-running the identical
   command immediately, at plain default (no `--memory-max`, no `--memory-reserve`):

   ```
   allocated 2000 MiB
   DONE-OK
   confine: ... reserve=4G reserve-basis=estimate:oom-escalated ...
             scope-memory.max=enforced=4294967296 ... terminated-by=normal peak-rss=2064492K
   EXIT=0
   ```

5. **The OOM really is recorded against this command's own signature.** Read straight out
   of `~/.local/state/aira/state.db` (`confine_peak_history`; note SQLite `LIKE` stops at
   the `\x00` argv separator, so the filter has to be done outside SQL):

   ```
   peak=1260466176 oom=1 at=2026-09-06T12:00:50Z | /usr/bin/python3 .../aira128-hog.py 2000 --tag=A128R1
   peak=2114039808 oom=0 at=2026-09-06T12:00:56Z | /usr/bin/python3 .../aira128-hog.py 2000 --tag=A128R1
   ```

   One OOM row keyed to the exact signature, then one clean row from the healed re-run.

   A second, entirely incidental confirmation landed mid-investigation: `aira confine -- go
   vet ./internal/daemon/` OOM-killed at its own `estimate:max=119705600,n=20,f=115` cap,
   and the immediately following identical invocation was admitted at
   `reserve-basis=estimate:oom-escalated reserve=201648K` and passed.

### Findings

- **Hypothesis 1 (hand-off note 1) is CONFIRMED.** The OOM is observed at teardown,
  reported over the admit socket, durably recorded against the command's own signature,
  and the very next identical invocation is admitted at an escalated reserve. No operator
  action, no flag, no re-configuration.
- **Hypothesis 2 (hand-off note 2, the attribution gap) is REFUTED.** `reportPeak` reads
  the HIERARCHICAL `memory.events` counter deliberately (`confine_linux.go:1248-1254`), so
  an OOM anywhere in the job — including a nested aitest worker sub-scope or a container at
  its own `--memory` — marks this signature's history. There is nothing for a sub-scope to
  absorb.
- **Hand-off note 3 (AIRA-110's `memory.swap.max=0`) is consistent** with this surfacing
  now: every trailer above reads `scope-swap.max=enforced`, so an overshoot that a swapping
  scope would once have absorbed is now a hard kill. That does not make AIRA-110 wrong.
- **Hand-off note 4 is CONFIRMED as consumer-side**, per item 3 above.
- **The estimator is not defective and is NOT changed here.** Its one genuine limitation is
  the one speed identified: escalation learns from a CENSORED observation (the recorded OOM
  peak equals the cap that killed the job — `peak=1260466176` is `memory.max` exactly), so
  convergence is geometric at 1.5x per OOM cycle from a `max(4 GiB no-history default,
  1.5 x observed)` floor. For a workload needing ~20 GiB from a 1.17 GiB cold start that is
  several wasted runs. Chasing that with new estimation machinery is exactly what speed's
  concluding architectural point rules out, and what architectural-simplicity rules out: an
  `-n auto` signature has no fixed peak to learn, so the input must be made deterministic
  (worker-count pinning, a consumer-repo change already shipped as money's workaround) and
  the existing per-signature estimator then learns it correctly.

**Scope reduction CONFIRMED, not assumed: no AIRA-side behavioural change is warranted.**
Every signal a consumer needs already exists and is already correct.

### What was built

The defect that remained is the one the brief predicted: AIRA never TOLD anyone to read
those signals, and the self-heal loop was only ever covered by synthetic-stats unit tests.

1. **`internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go`** — a real-cgroup,
   real-daemon, end-to-end regression test for the loop the ticket's Tests section demands:
   three real seeding runs establish a genuine machine-wide p90 prior; the target command's
   cold start is admitted at `estimate:p90-prior` and genuinely OOM-killed
   (`terminated-by=oom`, exit `137`, no completion marker, OOM advisory on stderr); the
   identical next run is admitted at `estimate:oom-escalated` above the 1.5x floor and
   completes. It runs entirely inside a throwaway `cgrouptest.IsolatedScopeParent` and
   never touches `aira.slice`. `internal/daemon/main_test.go` gains the `__confine-setup`
   dispatch a real `runner.Confine` needs, as `internal/install` and `internal/runner`
   already have.

   Not porous — established by mutation, with the surviving mutant recorded in the test's
   own comment: forcing `reportPeak`'s `oom` argument false → RED (attribution); disabling
   `resolveAdmitReserve`'s escalation branch → RED (resolution); disabling
   `classifyConfineTermination`'s OOM branch → RED (honesty, reports
   `unattributed-sigkill`). Swapping the hierarchical counter for the local one SURVIVES
   this test, because this fixture's workload is the leader in its own scope so both
   counters rise together. **Build-review correction (Fable, 2026-09-06):** that mutant is
   killed by the existing `TestConfineGrantedReserveIsScopeCapAndPeakIsReported`
   (`internal/runner/confine_linux_test.go`), which feeds `reportPeak` a usage with only
   the hierarchical `OOMKill` raised and asserts `oom=true` (re-run by the reviewer: RED).
   The `descendantOOM`/`drainedOOM` rows of `TestClassifyConfineTermination` originally
   cited here pin the VERDICT's local read — the opposite direction — and do not cover it.
   The accepted gap is narrower than "uncovered": no real-cgroup test drives a
   nested-victim OOM through `reportPeak`'s attribution end to end.

2. **Generated Skill + agent guide** (`internal/core/skill.go`) gain a section, "Reading a
   confined job's outcome (an OOM kill is not a test result)". The generated agent-facing
   guidance named `terminated-by` nowhere at all before this — an agent reading only a
   truncated run's failure tally has no instruction telling it otherwise. It teaches the
   verdict vocabulary, exit `137`, that a killed run is UNEVALUATED rather than failing,
   that a first-run OOM is an expected cold-start cost whose correct response is to re-run
   the identical command, and that a workload whose own parallelism scales with the machine
   must pin its worker count because no per-signature estimate can track a moving input.
   Guarded by `TestSkillTeachesTheOOMVerdictAndTheColdStartSelfHeal` (verified red without
   the prose) so it cannot be silently dropped.

### Explicitly not done

- Merge-gate's own ~38-41 GB estimate: untouched, as the scope correction requires.
- The estimator's starting value (`estimate:p90-prior`) and the 1.5x escalation formula:
  untouched. Both work as designed; the cold-start cost is a deliberate, documented
  property going back to AIRA-50/52/67.
- Nothing that would cap or model a workload's own internal parallelism. That is the
  consumer's pytest/Makefile invocation, and pinning it is the correct fix there.
