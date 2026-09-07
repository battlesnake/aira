---
{"schema":1,"id":"AIRA-149","project":"aira","title":"An OOM-escalated reserve clamped to EXACTLY the slice ceiling is ungrantable, and reports a fabricated \"slice contended\"","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","honesty"],"hold":false,"relations":[]}
---

Found while root-causing AIRA-139 (a test flake) and deliberately NOT fixed
there: the flake was removed at fixture level because what it exposed in
production is a sizing/diagnosis decision on the machine-wide admission gate,
not a test concern. Two facets, one cause, both in
`internal/daemon/admit.go`'s `resolveAdmitReserve`.

## Facet 1 — `estimate:oom-escalated` is reported for a value that is not an escalation

```go
reserve := request.reserve                       // UNPINNED => always exactly 4 GiB
basis := "fallback:insufficient-samples"
ordinary := stats; ordinary.OOMCount = 0
if estimated, ok, b := runner.EstimateMemoryReserve(ordinary, 0); ok {
    reserve, basis = estimated, b                // needs >= 3 usable samples
}
if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
    escalated := stats.MaxOOMPeak * 3 / 2
    if escalated > reserve { reserve = escalated }   // a MAX, not a replacement
    ...
    return reserve, "estimate:oom-escalated"
}
```

By `runner.ResolveConfineReserve`, an UNPINNED request's reserve is *always*
exactly `runner.DefaultConfineMemoryReserve` (4 GiB) — an explicit
`--memory-reserve`, an explicit `--memory-max` and `--delegate-ram` each set
`pinned`, and a pinned request returns at the first line of
`resolveAdmitReserve`. So when a signature has an OOM record but fewer than
three clean samples — which is the NORMAL state immediately after the first
OOM, i.e. exactly when the self-heal is supposed to act — the ordinary estimate
is unusable, `reserve` stays the blind 4 GiB constant, and the `max` leaves it
untouched unless 1.5 x the OOM peak exceeds 4 GiB (i.e. an OOM peak above
2.7 GiB).

Measured on the AIRA-139 fixture:

    stats={TotalCount:1 SampleCount:1 PeakMax:56360960 OOMCount:1 MaxOOMPeak:56360960}
    req.reserve=4294967296 -> returned 1031798784 with basis "estimate:oom-escalated"

The 1.5x escalation of that peak is 84541440. Neither 84541440 nor anything
derived from it appears in the answer. `reserve-basis=estimate:oom-escalated`
is operator-facing (it is printed in the confine trailer), and here it names a
provenance the number does not have. That is the class of dishonesty AIRA's own
rules exist to prevent.

## Facet 2 — a reserve clamped to the ceiling can never be granted on a live slice

The same branch clamps a too-large reserve to the ceiling:

```go
// An OOM observed at the present ceiling is genuinely too large. Earlier
// censored caps are allowed to climb to the ceiling so a runnable job is
// never permanently wedged.
if stats.MaxOOMPeak < ceiling && reserve > ceiling { reserve = ceiling }
```

but the grant gate is strictly tighter than the admission gate:

```go
// checkedAvailable
effectiveCurrent := max(current - reclaimable, 0)
charge := max(effectiveCurrent, outstanding)
return ceiling - charge            // waiter granted iff reserve <= this
```

`admitConnection` refuses terminally at `reserve > ceiling`, so `ceiling` is the
largest ADMISSIBLE reserve — but the largest GRANTABLE one is
`ceiling - max(current - reclaimable, outstanding)`. A reserve equal to the
ceiling is therefore grantable only while the slice's charge reads **byte-exact
zero**. On a shared, live `aira.slice` that never happens. The stated intent of
the clamp ("so a runnable job is never permanently wedged") is the opposite of
its effect in that case.

Note the documented, deliberate part: `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`
already says a clamped reserve is "accepted, so the request waits". Waiting is
by design. What is not by design is that the wait can never end.

Measured (AIRA-139, 1 GiB fixture slice, ceiling 1031798784):

    granted:     current=0     -> available == reserve
    never granted: current=4096 for 107 consecutive evaluator passes (~30s)

One residual 4 KiB page decided it.

## Facet 2b — and the refusal that follows is a fabricated diagnosis

When the wait expires the caller gets:

    E_ADMIT_SATURATED: confine: admission rejected after 30s — slice contended,
    no memory admission within the wait (reserve 4G/unknown)

with the wait diagnostic reading `queue position 1 of 1 by enqueue order, 0B
queued ahead`. The slice was EMPTY and nothing was contending: the request could
not fit a ceiling it was itself equal to. "Slice contended" is a manufactured
cause, and the operator has nothing to act on — the one number that would
explain it (the resolved reserve vs the ceiling) is not in the message.

## Reachability

Narrow but real, and it widens as the slice shrinks:

- On the production 64 GiB `aira.slice` the clamp needs a resolved reserve above
  roughly the 62 GiB ceiling, i.e. an OOM peak above ~41 GiB (or a smaller
  configured slice).
- On any small slice — a CI slice, a shim-configured ceiling, a fixture, a box
  with a smaller `aira.slice` — the blind 4 GiB default alone exceeds the
  ceiling, so EVERY unpinned request whose signature carries an OOM and fewer
  than three clean samples resolves to exactly the ceiling and then wedges. That
  is precisely what AIRA-139 was.
- The victim is by construction a job that has just been OOM-killed, i.e. the
  self-heal path, i.e. the case the escalation exists to serve.

## Directions (NOT a plan — this needs the two-loop)

Each has real blast radius on the machine-wide admission gate; pick
deliberately, do not merge one as a "cleanup":

1. Make the escalation a REPLACEMENT rather than a `max` with the blind default
   when no usable ordinary estimate exists. Fixes facet 1 exactly and removes
   most of facet 2's reachability. Cost: it changes self-heal SIZING — the
   AIRA-128 real-cgroup fixture's second run currently succeeds on the 4 GiB
   default and would then get ~80 MiB and be OOM-killed again, converging over
   several rounds instead of one. That fixture would need retuning, and the
   "the very next run succeeds" claim would need restating as a ladder.
2. Leave sizing alone and make the clamp target something the grant gate can
   reach. Needs a principled quantity; `ceiling` minus an arbitrary slack is a
   kludge and should be refused on the simplicity rule.
3. Leave both alone and fix the DIAGNOSIS only: when a waiter times out with
   nothing queued ahead and nothing outstanding, say what actually happened
   (resolved reserve == ceiling, slice charge X) instead of "slice contended".
   Cheapest, honest, and fixes nothing structural — but it converts a silent
   30-minute hang into an actionable message, which is the AIRA-71 lesson.

Recording it, unfixed, is deliberate: see the AIRA-139 resolution for why the
flake was closed at fixture level rather than by changing this.
