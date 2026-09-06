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
