---
{"schema":1,"id":"AIRA-138","project":"aira","title":"aira confine: a job deadline, including cumulative CPU-time, for a supervisor with no run ledger","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","runner"],"hold":false,"relations":[]}
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
