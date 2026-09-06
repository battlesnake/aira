---
{"schema":1,"id":"AIRA-119","project":"aira","title":"confine --list's exclusive holder/drain display name can go stale, naming a job that already released","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","dogfood"],"hold":false,"relations":[]}
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

## Resolution (2026-09-06, branch `aira119-exclusive-holder-label-staleness`)

### What was established, and what it rules out

The hypothesis in "Likely mechanism" above — that the daemon caches or snapshots
the exclusive waiter's name/owner somewhere that is not refreshed when the holder
changes underneath it — was investigated directly and is **not what is happening**.
There is no cache anywhere on that path: `admitSliceSnapshotFor` re-derives the
identity from `exclusiveGateLocked(queue)` on **every read**, in the same locked
pass as the counts beside it, and `confine_manage.go` copies that pass straight to
the wire. A waiter leaves `queue.waiters` only through `releaseAdmitWaiterLocked`,
and `admitConnection`'s `defer release()` runs on every return path, so the
identity cannot outlive the waiter it names.

That is now **pinned by a mutation-verified regression test** rather than asserted:
`TestExclusiveIdentityOnTheWireFollowsTheGateAcrossReleaseAndReadmission`
(`internal/daemon/admit_exclusive_identity_test.go`) drives holder A through
draining -> held -> released -> a different requester B draining -> B held, reads
the **confine-list wire reply** at every step, and fails if A's name, owner, or
scope id ever reappears after A released. It keeps the slice occupied throughout
so the same `sliceQueue` object survives the handover — without that the queue is
pruned between holders and any carry-over defect would be discarded with it, which
made a first draft of this test porous. Verified non-porous by mutation: caching
the first identity on the queue makes it fail with exactly the reported symptom
(`the wire named the RELEASED exclusive job A: {State:draining Name:joba ...}`).

A second, previously **untested** half of AIRA-101's un-wedge proof was also
closed. That proof covered a SIGKILLed exclusive HOLDER only; the state this
ticket was actually observed in is a DRAIN HEAD, which is the harder case —
a queued waiter owns no cgroup scope, so none of the daemon's scope-based
backstops (the stale-lease sweep, the orphan reaper) can ever see it and its only
liveness signal is the admission socket. `TestSIGKILLingTheExclusiveDrainHeadStopsNamingIt`
covers it against a real socket and a real SIGKILL; mutation-verified (suppressing
the queued-exclusive release makes it fail with this ticket's title, verbatim).

### The defect that was actually fixed

The name was not stale — but the **line could not be acted on**, and that is what
made a live, correct report indistinguishable from a stale one. Three facts the
daemon already knew were missing from it:

1. **Whether the named job is running at all.** `held` means it is running alone;
   `draining` means it has **not started**. `aira confine` creates its cgroup
   scope and launches its target only *after* admission is granted, so a draining
   job owns no process, no scope, and has no row in the `--list` table above.
   `draining for "job" (fdtd4)` reads as though the job were running, so an
   operator greps for it, finds nothing, and concludes the daemon is naming a job
   that already released. **That misreading is this ticket.**
2. **The scope id** — the only unique, greppable handle, and the selector
   `aira confine --kill` takes. It has been on the wire since AIRA-101
   (`ConfineExclusiveState.ScopeID`) and **no face has ever rendered it**, leaving
   `Name` (which defaults to `"job"` for every unnamed confine) as the sole
   identifier, while `Owner` may come from `AIRA_CONFINE_OWNER` and so appears in
   no argv anywhere — which is very likely why the reporter's `ps` found nothing.
3. **How long the state has lasted** — the discriminator between a routine
   30-second drain and an 18-minute wedge. Nothing reported it.

Changes:

- `runner.ConfineExclusiveState` gains `SinceMS`: how long **this** state has been
  in effect. `held` measures from the waiter's **grant**, `draining` from its
  **enqueue**. The split anchor is load-bearing, not tidiness: a benchmark that
  queued twenty minutes and has now run alone for five seconds has held the slice
  for five seconds, and reporting twenty would name it as the wedge an operator is
  hunting — the AIRA-49 v3 enqueue/grant conflation in the one place it would say
  the opposite of the truth. Pinned by
  `TestExclusiveAgeAnchorsOnEnqueueWhileDrainingAndOnTheGrantOnceHeld`;
  mutation-verified (collapsing both anchors to `enqueued` fails it).
- Derived in the **same locked pass** as the identity, off the same single clock
  reading the rest of the snapshot uses, so the age and the job it describes can
  never come from different instants. Zero is an unestablished age, never "0s".
- `confine --list` renders `scope=<id>`, the age, and — for a drain — an explicit
  `not started yet`, stated independently of the age so it still appears when the
  age cannot be established.

Before / after:

```
slice exclusive: draining for "job" (fdtd4), 10 jobs waiting
slice exclusive: draining for "job" (fdtd4) scope=CONFINE-job-4242-1@fdtd4, not started yet, draining for 18m3s, 10 jobs waiting
```

### Scope, and what is deliberately NOT changed

- Admission behaviour is untouched. `exclusiveGateLocked`, `blocks`, the
  single-exclusive refusal and every release path are unchanged; only what is
  *reported* changed.
- The blocked launcher's own progress line (`describeExclusiveJob`,
  `internal/runner/confine_queue_position_linux.go`) is left alone, as the ticket
  directs — it is a renderer of the same daemon facts and out of scope here.
- **Left open for the coordinator to judge:** if the field observation is taken at
  face value — the wire naming `fdtd4` while the live drain provably belonged to
  the `sigterm-test` probe — then two exclusive waiters were active on one slice at
  once, which `enqueueAdmitInternal`'s refusal makes unrepresentable, and no
  interleaving that produces it was found. The most economical reading consistent
  with every recorded fact is that the display was **correct**: a second, still-
  *draining* `--owner fdtd4` request that had not launched (hence no process for
  `ps` to find, and `fdtd4` visible only in `AIRA_CONFINE_OWNER`, not in any argv),
  blocked behind whatever was keeping the slice non-empty. This is recorded rather
  than asserted, and the tests above mean a genuine daemon-side regression of that
  shape would now be caught rather than argued about.

Done: PR #65 merged as `cd618f49348882267b6af1ec14caf44965cf370d` (build-review verified the identity test non-porous by re-running the cached-identity mutation; the field symptom itself was not reproduced — see the open note above).
