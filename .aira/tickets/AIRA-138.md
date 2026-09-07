---
{"schema":1,"id":"AIRA-138","project":"aira","title":"aira confine: a job deadline, including cumulative CPU-time, for a supervisor with no run ledger","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","runner"],"hold":false,"relations":[]}
---
Deferred out of AIRA-136 (which added `--cpu-timeout` to `aira run` only), and
surfaced as its own ticket rather than left implicit, because the AIRA-136
ticket title named `aira run/confine` and the owner's motivating scenario --
heavy suites on a load-48 box -- is exactly where `aira confine` is the mandated
entry point. The owner should be able to override this deferral before AIRA-136
merges rather than discover it afterwards.

## Why it was NOT a rider on AIRA-136

`aira confine` has NO job deadline of any kind today. Its accepted options are
slice, name, owner, memory-reserve, memory-max, memory-high, admit-timeout, and
the valueless delegate-ram / detach / exclusive (verified: the run option
whitelist in cmd/aira/main.go) -- and `--admit-timeout` bounds the ADMISSION
WAIT, not the job. Its wait is an unconditional blocking `waitConfineCommand(cmd)`
(internal/runner/confine_linux.go): no select, no timer, no kill trigger, no
kill-intent ledger, no terminal CAS.

So adding `--cpu-timeout` to confine is not "the same feature on a second verb".
It is building confine's FIRST deadline-and-kill path from scratch, which means
answering, in a supervisor with different kill semantics (`cgroup.kill`,
`confine --kill`, the supervisorSignal cut-off) and deliberately NO run ledger,
the entire class of arbitration question AIRA-126 spent a full two-loop cycle on.
Doing that as a rider on AIRA-136 would have been the exact mistake AIRA-136's
own ticket warns against, in a second location.

## What this ticket should decide

- Whether confine gets a deadline at all, given it has deliberately avoided one
  so far and callers can wrap with `timeout(1)` today for the wall case (though
  NOT for the CPU case, which is the whole point of AIRA-136).
- If yes: BOTH bounds in one design, sharing AIRA-136's primitives, which were
  written to be reusable without being pre-generalised --
  `readCgroupCPUUsed` / `readCgroupCPUFn`, `deadlineSource` / `startDeadlineSource`
  (internal/runner/deadline_linux.go), and the pure rules `decideCPUBudgetExceeded`,
  `decideFinalCPUConsumed`, `decideCPUBudgetUnenforced` (internal/runner/decisions.go).
- The kill/terminal arbitration for a supervisor with no run ledger: what plays
  AIRA-126's role of "the intent published, the scope already empty, no signal
  delivered", and what confine's honest terminal record is in that state.
- Whether the confine trailer gains a field naming the bound that fired, so a
  killed confine job is as auditable as a killed run record.

Correctness-critical: full two-loop per CLAUDE.md, not the light path.

## Plan (committed, awaiting plan-gate review)

`docs/superpowers/plans/2026-09-07-aira138-confine-deadline-plan.md`, on branch
`aira138-confine-deadline`. What it decides:

- **Yes**, confine gets a deadline, and **both** bounds in one design:
  `--timeout` and `--cpu-timeout`, names identical to `aira run`'s, launch-form
  only. `timeout(1)` is not an answer even for the wall case — it signals the
  SUPERVISOR, which renders as `terminated-by=supervisor-signal:SIGTERM`,
  indistinguishable from an operator's Ctrl-C. `ulimit -t` is per-process and
  `cpu.max` is a throttle, so the CPU bound has no external equivalent at all.
- **The ledger-less arbitration.** AIRA-126's failure had two halves: (a) a
  durable artifact asserting a kill that never happened, and (b) the child's real
  exit being discarded. Half (a) does not exist in foreground confine, so
  `decideTimeoutIntentNotExecuted` is deliberately NOT transplanted — passing
  `IntentPublished`/`IntentCreated` as true for a ledger that does not exist
  would be a lie wearing the costume of reuse. Half (b) transplants exactly:
  **confine's durable evidence is its exit code and its trailer**, and a confine
  fabrication is final the instant it is printed (`ConfineResult` has no
  `ErrorCodes`, and there is no reconcile pass to correct it later). A new pure
  rule `decideConfineDeadlineNotExecuted` keeps AIRA-126's two EVIDENCE conjuncts
  verbatim (`Empty && !Started`; `leader == processDead`) and drops only the two
  ledger ones.
- **Three arms**: killed / not-executed (drain-then-report the child's real exit,
  with no ledger write at the end — exactly the shape the ticket anticipated) /
  unevaluated. A bounded drain that expires degrades to unevaluated, never to a
  kill claim.
- **Trailer**: yes — two fields, `timeout=` and `cpu-timeout=`, present only when
  that bound was requested, with a closed state vocabulary including
  `fired-not-executed` and (CPU only) `unenforced`. They are load-bearing rather
  than decorative, because the exit code stays a pass-through of the job's.
- **Verified in source, not assumed**: confine DOES have `--detach`, and its
  supervisor calls the same `Confine`, so detach is in scope and gets both bounds
  through the one arbitration site — to be proved by a test, not by reading.
  ci-shim mode REFUSES both bounds, fail-closed (no `cpu.stat`, no `cgroup.kill`).

Reproduction / danger proof, committed with the plan:
`internal/runner/confine_deadline_danger_linux_test.go`. Since confine has no
deadline code to make misbehave, the artifact proves the DANGER is real rather
than theoretical: a minimal naive first draft, built from confine's real
primitives, reports `exit 137` and a deadline attribution for a child that exited
`7` normally having signalled nothing — while every input needed to refuse was
already available (scope empty by two reads, `cgroup.kill` written against
nothing, `processLive == processDead`, the real outcome pending unread in the
wait channel). Deterministic via AIRA-126's `gatedStdin`, 5/5, no soak.

## Resolution

Built exactly to the gated plan revision 2
(`docs/superpowers/plans/2026-09-07-aira138-confine-deadline-plan.md`;
`02a4eee` is the plan-fix SHA in the builder's own pre-rebase reflog, `ae99c72`
on the merged branch — noted by the build review as a doc-accuracy nit, fixed
here).
`aira confine` now accepts `--timeout DURATION` and `--cpu-timeout DURATION`
in the launch form only, both clocks starting at the RELEASE WRITE so neither
includes the admission wait or the setup handshake.

**The arbitration.** One `deadlineSource` (AIRA-136's, reused; the only change to
it is the additive typed `deadlineFire.Kind`), one select racing the wait, one
kill site `killConfineScope`, three arms:

- **A killed** — `cgroup.kill` written and returned nil. The child's real
  wait-derived exit (137) is reported; `terminated-by=deadline:wall|cpu`;
  `fired-kill-completed` or `fired-kill-unconfirmed`.
- **B not executed** — `decideConfineDeadlineNotExecuted` (AIRA-126's two
  EVIDENCE conjuncts, its two LEDGER conjuncts deliberately dropped because the
  artifact they guard does not exist in confine). The child's OWN exit is drained
  and reported, the classifier's honest verdict stands, and the trailer records
  `fired-not-executed`. The drain is bounded by `arbitrationWaitFloor` (250ms) and
  an expiry degrades to arm C, never to a kill claim.
- **C unevaluated** — a read or the write genuinely errored, or the leader's
  liveness could not be established. `fired-unevaluated`.

**The plan-gate P0 (leaf-only inertness) is fixed and proved.** The kill gate is
TWO independent reads — leaf `cgroup.procs` via `Members()` AND subtree-aware
`cgroup.events populated` via `Empty()`. A leaf-empty, subtree-POPULATED scope is
a busy job living in child cgroups it made inside its own scope (aitest /
`--delegate-ram` / `podman --cgroups=split`) and is now KILLED, not shrugged at.
The no-signal refusal still returns before any write and now means verified empty
by both reads agreeing — strictly stronger than the draft's one read.

**Exit code stays a pass-through** of the job's own on every arm (§5.4); the two
trailer fields are therefore the only machine-readable carrier of "a bound
fired", derived by the total pure rule `decideConfineDeadlineState`. Both fields
render only when that bound was requested, so every pre-existing trailer is
byte-identical. ci-shim mode REFUSES both bounds fail-closed. Detach needed no
second arbitration and the facets reach the durable `ConfineDetachRecord` for
free — verified by a test, not by reading.

**One named divergence from the implementation draft, corrected to the plan:**
`terminated-by` renders `deadline:wall` / `deadline:cpu` (the QUANTITY exceeded, a
cause), not the flag names, which live on the same line's `timeout=` /
`cpu-timeout=` fields (the knob). The two vocabularies are pinned literally by
test, not just symbol-to-symbol.

### Evidence

Foreground, exact exit codes, `AIRA_REAL_CGROUP=1` for the suite:

```
aira confine -- go build ./...                    -> 0
aira confine -- go vet ./...                      -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -> 0
```

**Mutation testing (executed reverts, runs read, not assumed).** All seven plan
mutations were applied and killed:

| # | Mutation | Result |
| --- | --- | --- |
| 1 | `decideConfineDeadlineNotExecuted` forced false | `…DeadlineAgainstAlreadyExitedChildReportsTheRealExit` FAILED |
| 2 | forced true | `…WithLiveLeaderNeverArbitrates`, `…WithUnknownLeaderStaysUnevaluated`, `…NotExecutedRule` all FAILED |
| 3 | classifier step 7 hoisted above step 3 | `…ClassifyConfineTerminationDeadlineOrder` FAILED |
| 4 | CPU wiring neutered (`CPU: 0`) | the real-cgroup CPU tests NEVER TERMINATE — the confined spinner outlives its supervisor, which is the defect itself; the package cannot pass |
| 5 | `killConfineScope` restored to the leaf-only gate | `…DeadlineKillsALeafEmptySubtreePopulatedConfineJob` FAILED (10s, no kill written) |
| 6 | gate widened the other way (always kill, no refusal) | `…DeadlineAgainstAlreadyExitedChildReportsTheRealExit` FAILED — arm B unreachable, the AIRA-126 fabrication returns |
| 7 | `decideConfineDeadlineState` precedence collapsed (`unenforced` first) | `…ConfineDeadlineStateRule` FAILED |

Mutations 5 and 6 bracket the kill gate from both sides, so neither a re-narrowed
nor a widened gate can pass silently.

**Dogfood on the shared box**, real jobs against the real daemon and slice:

```
--timeout 2s      -- /bin/sleep 60      -> exit 137, terminated-by=deadline:wall, timeout=2s:fired-kill-completed
--cpu-timeout 1s  -- sh -c 'while :; :' -> exit 137, terminated-by=deadline:cpu,  cpu-timeout=1s:fired-kill-completed, cpu=1.036s
--cpu-timeout 1s  -- /bin/sleep 3       -> exit 0,   terminated-by=normal,        cpu-timeout=1s:not-reached
--timeout 30s --cpu-timeout 30s -- echo -> exit 0,   timeout=30s:not-reached cpu-timeout=30s:not-reached
--detach --cpu-timeout 1s -- spinner    -> --status finished exit=137, durable record carries terminated-by=deadline:cpu
--timeout 0                             -> E_CONFINE_ARGUMENT_INVALID: --timeout: must be positive (exit 2)
--cpu-timeout 5m --list                 -> E_CONFINE_ARGUMENT_INVALID: not valid for confine management (exit 2)
```

The sleep pair is the feature's whole point in two lines: the CPU bound is
CPU-time, not wall-clock. The spinner's own `cpu=` counter (1.036s against a 1s
budget) shows the 100ms sampling overshoot landing in the honest LATE direction.

### Accepted gaps and deferrals (plan §9, unchanged)

ci-shim gets no bounds (fail-closed refusal, follow-up ticket); no SIGTERM grace
before the deadline's SIGKILL, matching confine's documented contract; `aira
run`'s `killedByCPUBudget` still overclaims (confine's is stricter — aligning run
is a follow-up); sampling overshoot up to `interval x parallelism`, always late;
sub-100ms `--cpu-timeout` reports `unenforced` rather than a fake pass; the setup
shim's post-release `read`+`execve` is the one early-direction residual; a Ctrl-C
racing a deadline resolves to `supervisor-signal:`; contention itself is not
reproduced in tests; AIRA-70's irreducible signal window is untouched.

`aira run` carries the same leaf-only kill-gate defect and is filed separately as
**AIRA-140**, with this ticket's `deadlineConfineScope` fake and reproduction
written to be reused there.

## Merged

PR #88, merge commit `d1a761b`. Fable's build review found one real issue — a
test-harness-only data race on a package-level `readProcStatFn` swap racing
the membership-monitor goroutine in `TestAIRA138...` (production code was not
racy) — fixed in commit `76dd536` before merging. Independently re-verified
after recovering from a session-limit interruption mid-merge: `go build`,
`go vet`, and the full `AIRA_REAL_CGROUP=1 go test ./...` all exit 0; `-race
-run AIRA138` flaked once under extreme shared-box contention (a scope not
yet empty at a timing-sensitive fire) and passed clean 21/21 on an immediate
retry with zero changes — consistent with this session's other observed
environment-load flakes tonight, not a regression.
