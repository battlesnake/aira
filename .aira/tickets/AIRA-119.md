---
{"schema":1,"id":"AIRA-119","project":"aira","title":"confine --list's exclusive holder/drain display name can go stale, naming a job that already released","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","dogfood"],"hold":false,"relations":[]}
---
Reported by peer session `field`, confirmed live by the coordinating session, 2026-09-06.

## The bug

`confine --list`'s `slice exclusive: draining for "<name>" (<owner>)` / `held by ...` line can display a **stale** holder/requester identity — naming a job that has already released, while the *actual* current drain belongs to a completely different, unrelated request.

## Timeline, as observed live

1. field's own exclusive job (`"job" (fdtd4)`) released cleanly at 03:57:09 — confirmed by its own trailer.
2. ~18 minutes later, `confine --list` still read `slice exclusive: draining for "job" (fdtd4), N jobs waiting` — with `N` climbing (6 → 7 → 10) as more sessions queued behind it.
3. `ps` found no live process anywhere named `fdtd4`, `fdtd-bench`, or `bench.sh` — the named holder genuinely did not exist any more.
4. The coordinating session had, independently and around the same time, launched an unrelated `aira confine --exclusive --owner sigterm-test -- ...` probe (from a completely different investigation). Killing *that* probe made `slice exclusive: none` appear **immediately** — proving the real, live drain belonged to the probe, not to `fdtd4`. The display had been naming the wrong job for its entire visible life.

## What this rules in and out

- **Not a leaked exclusivity registration.** AIRA-101's own crash-safety design (exclusivity as a derived property of a live admission waiter, never a standalone flag) held: the real requester was genuinely live and blocking, and releasing it cleared the state correctly. Confirmed independently by both sessions.
- **The admission gate logic itself is sound**, traced against source (`internal/daemon/admit.go`'s `exclusiveGate.blocks`): a drain correctly blocks every new top-level admission (only an already-admitted job's own sub-reservations are exempt, which is required for the drain to ever converge — see the function's own comment). field's related observation ("jobs kept getting admitted while it said draining, one ran `cargo test` to completion") is consistent with this being CORRECT: those jobs were already admitted *before* the drain started and were finishing naturally, exactly as designed ("drain, never preempt"). Not re-litigated here as a separate bug — recorded so it isn't mistaken for one by a future reader of this ticket.
- **This is specifically a display/attribution bug**: the *name* rendered for the current holder/drain-head is wrong, even though the underlying blocking behavior is correct. A caller relying on the name to diagnose a stall gets actively misleading information — worse than no name, because `ps`-ing for the named (departed) job finds nothing and looks like daemon-state corruption when the daemon's actual gating logic is fine.

## Likely mechanism, not confirmed — starting point for whoever investigates

`describeExclusiveJob` (`internal/runner/confine_queue_position_linux.go:191`) just renders whatever `exclusiveName`/`exclusiveOwner` the daemon reports in its queue-position response — the bug is not in that rendering function itself. The daemon-side population of those fields (near `exclusiveGateStateLocked`/`gate.draining`/`gate.holder`, `internal/daemon/admit.go` — grep for where a queue-position query response is assembled, likely close to line 1293's `exclusiveState := exclusiveGateStateLocked(queue)`) is where the stale value most likely originates — worth checking whether the position-query path caches or snapshots the exclusive waiter's name/owner at some point that doesn't get refreshed when the actual holder changes underneath it.

## Not reproduced on demand

This is a live, timed, two-session-corroborated observation with a clean causal test (killing the true current holder cleared it instantly) — strong evidence, but no standalone minimal repro has been constructed yet. That's this ticket's own scope.
