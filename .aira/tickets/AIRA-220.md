---
{"schema":1,"id":"AIRA-220","project":"aira","title":"aira confine --list reports a fabricated 0B/0-job slice reserve after a daemon restart, on the surface AIRA-178 designates as the authoritative check","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","dogfood","honesty"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-220","to":"AIRA-221"}]}
---
> Found during the 2026-09-09 deploy of PR #125, by observing the live machine
> after a routine daemon restart. Verified from source; every claim below was
> re-checked at the cited line.

## SYMPTOM

The daemon was restarted at 13:35:28 with two confine jobs running. For at least
three minutes — and in principle unboundedly, since the window ends at the next
admission request rather than on a timer — `aira confine --list` printed this
directly beneath a table showing both scopes LIVE and populated:

```
slice reserve: 0B granted / 46016M ceiling across 0 admitted jobs
slice scope caps: unevaluated / 94G over-subscription bound (not applied while unevaluated)
  of which: 0 confine scopes 0B, 0 scope-less reservations 0B, 0 adopted scopes 0B
```

At that moment the cgroup directories held 1 and 25 processes at `memory.current`
133,828,608 and 26,247,774,208 bytes, and the daemon's own ceiling log reported
`sliceAnon=26165786536`. So the daemon could see the memory perfectly well; the
summary line asserted zeros anyway.

The per-row `RESERVE` column was honest (`unevaluated`, per AIRA-192). The
SUMMARY was not. That contradiction inside one command's output is the tell.

This is not a new state, and it has been misread before: AIRA-105 records an
earlier investigator seeing `adopted_jobs: 0` immediately post-restart and
concluding it "climbed back within seconds" — a warm-up. It is not a warm-up. It
climbed back because a job happened to launch.

## WHY THIS IS P1 DESPITE ADMISSION BEING CORRECT

Admission itself is sound, and this ticket does NOT claim otherwise. Adoption is
re-derived at the top of the first evaluator pass, ahead of the single grant site
(`admit.go:2787`), and `checkedAvailable` (`admit.go:2878-2895`) charges
`max(memory.current - reclaimable, outstanding + adopted)`, so realised RSS is a
floor on the charge regardless of what the ledger holds. Nothing over-admitted.

The severity comes from WHO is told to trust this line. AIRA-178's own follow-up
instructs operators and agents to use exactly this output in place of `free` /
`MemAvailable`, because two reporters used those and were wrong by 50+ GiB. So
during the window, the surface AIRA designates as the trustworthy one is the one
lying — and it lies in the direction that reads "the slice is empty, launch
freely". A fabricated zero on the headline safety surface is the one thing this
project's rules say never to do.

## ROOT CAUSE

`internal/daemon/admit.go:1562-1570`. `admitSliceSnapshotFor` returns a
zero-valued `admitSnapshot` when `s.admitQueues[path] == nil`, on a premise its
own comment states outright:

```go
// An absent queue is a genuine idle zero, not an unevaluated read: a queue
// exists only while it has waiters, so its absence positively establishes
// that nothing is waiting. Callers must not render this as "unknown".
```

That premise is false, and has been since 2026-08-27. A `sliceQueue` is created
lazily in `enqueueAdmitInternal` (`admit.go:2280-2288`) and destroyed the moment
its last waiter is released (`pruneAdmitQueue`, `admit.go:2977-2982`, which also
closes `queue.stop` and ends the evaluator goroutine). A restarted daemon starts
with an empty `admitQueues` (`server.go:232`) and nothing seeds it. Adoption runs
ONLY inside `evaluateAdmitQueue`, which runs only from `runEvaluator`, which
exists only while a queue exists. So between a restart and the next admission
request there is no queue, no evaluator, no scan, and no ledger.

The scopes are therefore NOT adopted at all — they are not "adopted with a zero
reserve". No predicate rejected them; no adoption pass ran.

The honest bit already exists and is thrown away: `snapshot.present` is read in
exactly ONE place, `admitOutstandingReserve` (`admit.go:1326-1329`), which has
zero callers anywhere in the tree — verified by grep. `confine_manage.go:173-183`
gates the summary on the slice memory read alone and never consults it. The same
fabrication reaches `aira top` through the same struct
(`cmd/aira/tui_top.go:475, 633, 791`).

## PROPOSED FIX

Two parts, and deliberately NOT "keep a queue alive" or "start an evaluator at
boot" — either would add a background scan loop on every slice the daemon has
ever heard of, for a reporting benefit, against the simplicity rule.

1. **Honest reporting.** Distinguish "no queue" from "queue with an empty
   ledger". When `!snapshot.present` AND the cgroupfs scan just returned >= 1
   live scope, the summary must read `unevaluated` — no ledger has been
   established for this slice since the daemon started — rather than
   `0B granted / 0 jobs`. `!present` with zero live scopes stays a genuine zero.
   This is AIRA's own `unevaluated != 0` rule applied to the one accessor that
   skips it. Correct the false comment at `admit.go:1566-1568` in the same change.

2. **Optional, weigh explicitly.** Have the read-only listing path opportunistically
   run the adoption derivation when it finds live scopes and no queue, so the
   summary carries a real number rather than an honest blank. Costs one
   `ListConfines` per `--list` and needs its own throttle. Ship (1) alone first;
   add (2) only if the blank proves unusable in practice.

## HOW TO TEST

Register NO queue, point `admitResolveSlice` at a temp slice containing one
populated finite-cap scope (reuse `reserveScopeDir` / `oversubRecord` from
`confine_scope_reserve_test.go`), and assert the resulting `SliceReserve` does
NOT claim `Jobs == 0 && GrantedBytes == 0` — it must report unevaluated. Fails
today: it prints the fabricated zeros.

False-pass twin: no queue AND an empty slice must still report a genuine
`0B / 0 jobs`, not unevaluated.

Note WHY this was never caught: every existing adoption test manufactures its
queue by hand (`registerAdmitQueue(server, queue)` then a direct
`server.evaluateAdmitQueue(queue)` — `confine_scope_reserve_test.go:277-279`), so
the real post-restart lifecycle has never been exercised. The new test must go
through the listing entry point with no queue registered.

## OPERATING NOTE UNTIL FIXED

After a daemon restart, do not treat the `slice reserve:` line as authoritative
until at least one confine job has launched post-restart. The tell is free: if
every table row shows `RESERVE unevaluated` while the summary claims
`0 admitted jobs`, the summary is fabricated. To force the heal at zero blast
radius, run `aira confine -- true`.
