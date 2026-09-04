---
{"schema":1,"id":"AIRA-75","project":"aira","title":"52% of this project's watchdog events are journaled=0, voiding the journal's gap-detection for them","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["daemon","dogfood","watchdog"],"hold":false,"relations":[]}
---
Found during the whole-project simplification review (PR #12). 245 watchdog event rows for this project — 52% of all of this project's recorded events — are `journaled=0`. Watchdog events consume the project's shared `seq` counter but are never actually written to the journal. This means the journal's own gap-detection mechanism (which presumably assumes `seq` and journaled entries stay in lockstep) is void by construction for over half of this project's real event history. Rants also have no replay path per the same review. Not investigated further — needs tracing to whether this is a deliberate omission (watchdog events genuinely don't need journaling) that should be documented as such, or a real gap that should be fixed so gap-detection means what it claims to mean.

## Resolution (2026-09-04, backlog-remediation Phase 0, plan section 2)

**The premise is wrong in one specific way, and correcting it is most of the
answer: there is no gap-detection mechanism over `events.seq` to void.**
Re-verified against source — nothing in `internal/store` reads seq contiguity,
and the journal is consulted by `(project, seq)` LOOKUP (`journalEventFor`),
never by "every seq must be present". So the sequence number these rows consume
costs a number and nothing else. The ticket's stated harm ("the journal's own
gap-detection mechanism ... is void by construction") does not exist to be
harmed.

### The two readings of "stop minting a sequence number" are different changes

The plan's row header proposed stopping the mint. Its own correction caught that
this conflates two things, and the correction is right:

- `AppendWatchdogEvent` exists **to broadcast host-level kill decisions into each
  ready project's `aira watch` stream**. `aira watch` reads `events` via
  `EventsSince`, ordered by seq. Demoting these to a daemon log line — the
  natural way to "stop minting a seq" — would silently remove the only surface on
  which a session sees the watchdog kill something. That is a worse change than
  the thing being fixed.
- Keeping the row and marking it **unjournaled by design** costs nothing and is
  the honest description of what these events are.

### Why unjournaled is correct, not merely tolerated

There is nothing to journal them AGAINST. The journal is this project's durable
record of its own allocations and mutations, replayed and cross-checked against
git-file content. A host-global watchdog kill is not a fact about this project:
the identical decision is broadcast verbatim into every ready project's stream. A
journal entry would fabricate a per-project provenance record for a machine-level
event — the same class of "confidently wrong recorded metadata" this repository
files as a bug elsewhere.

### Measured, not assumed (2026-09-04, read-only)

Of this project's 541 events, **245 (45.3%) are `journaled=0`**, and every single
one is an `aira-watchdog` actor row — `watchdog.trip`, `.intent`, `.outcome`,
`.recovered`, `.defer`. No other producer leaves an unjournaled event anywhere in
the database. (The ticket's 52% was measured earlier against a smaller event
population; the shape is identical.)

**`rant.redacted` is NOT part of this population, contrary to the plan's row
header.** `RedactRant` calls `s.journalEvent(...)` immediately after inserting
its event, so it journals like every other mutation. The ticket's separate remark
that "rants have no replay path" is about replay, not about seq, and is untouched
here.

### Landed

No behaviour change. The decision is recorded where it is load-bearing —
`AppendWatchdogEvent`'s doc comment — and pinned by
`TestWatchdogEventsAreWatchVisibleAndNeverJournaled`, which asserts BOTH halves at
once (watch-visible AND journaled=0), so a future change cannot satisfy one by
breaking the other.

AIRA-75 -> done.
