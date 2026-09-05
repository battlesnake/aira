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

### Build-review (Sol, 2026-09-04) — a claim above is WRONG, and this ticket is REOPENED

**Retracted: "the sequence number these rows consume costs a number and nothing
else."** That is false, and the correction matters more than the original
resolution.

Sol found the real cost, by a different mechanism than the one this ticket
originally named. There is still no contiguity CHECKER over `events.seq` — that
part of the correction stands — but the sequence high-water mark is **not durable
for unjournaled rows**:

- `Rebuild` derives `maxSeq` from the receipts and journal only
  (`internal/store/store.go`, the rebuild's maxSeq computation), then restores
  `event_counters.next_seq = maxSeq + 1`.
- Watchdog events are unjournaled and have no receipt, so they contribute
  NOTHING to `maxSeq`.
- After a database loss and rebuild, the trailing watchdog sequence numbers are
  therefore forgotten and **reissued** to new events.
- A `aira watch` consumer that resumes from its previous cursor
  (`cmd/aira/watch.go`, which resumes at `seq > from`) then silently SKIPS those
  new events, because their reissued seq is not greater than the cursor it
  already holds.

That is a real correctness consequence, narrow (it needs DB loss + rebuild + a
resuming consumer) but real, and it is exactly the "the seq costs nothing" claim
being wrong.

**The DESIGN decision is unchanged and still correct:** these events are
watch-visible and unjournaled, because a host-global decision broadcast into
every project has no per-project provenance to journal. What is now open is the
COUNTER: `Rebuild` should take its high-water mark from the events table's own
`MAX(seq)` as well as from the journal, so a seq that was issued is never
reissued regardless of whether it was journaled.

**Not fixed here.** Changing how the sequence counter is reconstructed on rebuild
is crash-recovery semantics — the class CLAUDE.md names as requiring the full
two-loop, not a documentation commit's tail. The measurement and the design
rationale above stand; the counter fix is this ticket's remaining work.

Also noted by the same review: the new test omits `watchdog.defer` and does not
exercise a rebuild, so it pins the design decision but not this consequence. It
is left as-is (it tests what it claims to test) rather than widened to imply
coverage of a defect that is not yet fixed.

## Resolution (2026-09-06) — remedy retracted, gap documented, ticket closed

The design decision from the first Resolution (watchdog events are
watch-visible, deliberately unjournaled forever) was correct and settled; it is
untouched here. What remained open was the build-review's own proposed
counter-side fix, and that proposed fix does not survive scrutiny.

**Retracted:** "`Rebuild` should take its high-water mark from the events
table's own `MAX(seq)`" is a no-op in every scenario where seq reissue can
actually happen, so no counter change closes this gap:

- A rebuild against an **intact** database never reissues to begin with. Seq
  allocation and event insertion commit as one `BEGIN IMMEDIATE` SQLite
  transaction (the d3-watch design's own commit-order invariant, already
  enforced by test), so `event_counters.next_seq` and `events` can never
  desync on a live DB; `Rebuild`'s own upsert (`ON CONFLICT ... next_seq = CASE
  WHEN event_counters.next_seq < excluded.next_seq THEN excluded.next_seq ELSE
  event_counters.next_seq END`) only ever raises the stored value, never
  lowers it. Adding a second source here would change nothing already correct.
- The only two routes that actually cause reissue — a full database loss
  (fresh schema, every table starts empty) or an `aira eject` (`events` and
  `event_counters` both carry `FOREIGN KEY(project_id) REFERENCES
  projects(project_id) ON DELETE CASCADE`, so ejecting the project row deletes
  both together) — wipe `events` itself alongside `event_counters` in the
  identical operation. So `SELECT MAX(seq) FROM events` reads 0 at exactly the
  moment it would need to contribute anything; there is no surviving copy of a
  watchdog row's seq anywhere once its project's rows are gone. Confirmed by
  grep: no other code path deletes rows from either table.
- The only way a watchdog seq could survive a DB loss is a durable store
  *outside* the database — i.e. journaling it — which is exactly option (a)
  from this ticket's own brief and is rejected for the reason already on
  record (a host-global decision broadcast into every ready project has no
  per-project provenance to journal against). Option (b), an epoch/generation
  token in the watch cursor, is new protocol machinery and rejected under this
  project's architectural-simplicity rule. Neither is implemented here.

**Accepted, documented bound (not fixed, not fixable without one of the two
rejected options):** an `aira watch --from N` consumer that resumes after a
full database loss or an `aira eject` may silently skip up to N_skip new
events, where N_skip = the count of trailing unjournaled (watchdog) seqs
issued between the loss and the resume. This is outside the d3-watch design's
"durable in the DB, recoverable via `--from`" guarantee, for exactly the one
class of row (watchdog) that was deliberately excluded from durability by
design. Recorded in both load-bearing locations: the `AppendWatchdogEvent` doc
comment (`internal/store/watch.go`) and an amendment to Invariant 1 of
`docs/superpowers/specs/2026-08-17-aira-d3-watch-design.md`.

An independent adversarial pass (DeepSeek, pro tier, high reasoning effort —
Fable was unavailable to this subagent; no peer session by that name existed
and no subagent-spawn tool was available in this context) reviewed the
retraction argument against the cited source facts and returned "sound with
caveat": the original wording of "no counter fix closes this" could be read as
an unqualified absolute claim. Both doc locations were tightened in response
to scope it precisely — no fix that reconstructs the counter from data still
inside the same on-disk database (or its cascade boundary) can work, and the
"intact DB never reissues" half is anchored explicitly to the already-verified
commit-order invariant rather than asserted freestanding.

No behaviour change. No code paths altered; only the `AppendWatchdogEvent` doc
comment, the d3-watch design spec, and this ticket file were touched. The
existing pinning test, `TestWatchdogEventsAreWatchVisibleAndNeverJournaled`, is
untouched — it already tests exactly what it claims to test (watch-visible AND
never journaled) and was not asked to grow rebuild/reissue coverage, since this
ticket does not fix the reissue behaviour, only documents it as an accepted gap.

**Build/vet/test, exact exit codes:**
- `go build ./...` — exit 0
- `go vet ./...` — exit 0
- `go test ./...` (full suite, `aira confine`-wrapped) — exit 0. One run hit a
  transient `FAIL` in `internal/runner`
  (`TestLiveTeeCancelDuringNormalJoinKeepsCaptureComplete`, a fixed-50ms-window
  timing test) under heavy concurrent host load (16-18 other admitted
  `aira confine` jobs on this shared machine at the time); confirmed
  load-induced and not a regression by re-running it in isolation (5/5 pass)
  and by a clean full-suite retry (exit 0, all packages `ok`). The repo's own
  `.githooks/pre-push` (`make ci` = `fmt-check vet build test`) additionally
  ran green on both the initial push and the post-rebase force-push.

**PR:** https://github.com/battlesnake/aira/pull/51
**Merge commit:** recorded below once merged.

AIRA-75 -> done.
