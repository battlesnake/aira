# AIRA-220 — `confine --list` must not fabricate a 0B/0-job slice reserve when the ledger is absent

## Root cause (verified in source)

`admitSliceSnapshotFor` (`internal/daemon/admit.go:1553-1571`) returns
`admitSnapshot{phase: phase}` — with `present` defaulting to **false** — whenever
`s.admitQueues[path] == nil`. A queue object exists only while it has waiters
(`enqueueAdmitInternal` creates it, `pruneAdmitQueue` destroys it), so a freshly
started or restarted daemon, and any long-idle slice, has none. Its comment asserts
an absent queue "positively establishes that nothing is waiting" — which stopped
being true the day AIRA-29 adoption landed: already-running confine jobs are
adopted only lazily inside `evaluateAdmitQueue`, so before the first admission the
daemon has read no ledger at all.

`confine_manage.go:203-206` then builds `SliceReserve{GrantedBytes: addClamp(0,0),
Jobs: 0, ...}` from that snapshot **without consulting `snapshot.present`**, and the
memory-read gate above it (`if ok`) only proves the *ceiling* is readable, not the
*ledger*. Result: with two scopes live at 26 GB, `confine --list` prints
`slice reserve: 0B granted / … across 0 admitted jobs`. AIRA-178 tells operators and
agents to trust exactly this line over free/MemAvailable, so the designated-trustworthy
surface lies in the "slice is empty, launch freely" direction.

`snapshot.present` is already computed and has **zero production callers** — only the
daemon's own tests read it. The fix is to thread it to every face.

## The fix

**Data.** Add `GrantedEstablished bool` to `runner.ConfineSliceReserve`
(`internal/runner/confine_manage.go:311`), mirroring the existing `*Known`/`heldEstablished`
honesty-bit idiom. `GrantedBytes` and `Jobs` are meaningful only when it is true.
(No-compat: [[aira-not-live-no-compat]], schema may change freely.)

**Daemon.** In `confine_manage.go:203`, set `GrantedEstablished: snapshot.present`. The
ceiling stays populated regardless (it is an independent memory read), so a `!present`
reserve still reports its ceiling — only granted/jobs become unevaluated.

**Three client faces must honour the bit** (verified these are all of them):
1. `cmd/aira/main.go:3374` — the `slice reserve:` text line. When `!GrantedEstablished`,
   render granted+jobs as `unevaluated` while keeping the ceiling clause.
2. `internal/runner/confine_queue_position_linux.go:464` — currently sets
   `heldEstablished = true` whenever `GrantedBytes >= 0 && Jobs >= 0`, i.e. **always**,
   so `confineHeldNote` renders the fabricated zero as "0B already granted across 0
   admitted jobs". Change the gate to `reserve.GrantedEstablished` (keep the `>= 0`
   sanity as a defensive conjunct). This is the *second* render site of the same bug.
3. `cmd/aira/tui_top.go:983` (`topFooter`) — `reserve == nil` already renders
   "slice reserve unevaluated"; extend so `reserve != nil && !GrantedEstablished`
   renders the granted/jobs portion unevaluated too (ceiling still shown).

`--json` and MCP `aira_confine_list` carry the new field for free (same struct → wire).

## Tests (must fail against the current tree)

- **Daemon** (`confine_manage_test.go`): a Server with a readable memory reader and **no
  queue registered** → `result.SliceReserve != nil` (ceiling present) AND
  `GrantedEstablished == false`. Fails today (no such field / fabricated established zero).
- **Client render** (`cmd/aira`): `renderConfineList` on a `SliceReserve{GrantedEstablished:
  false, CeilingBytes: X}` prints "unevaluated" for granted and does **not** print
  "0B granted / … 0 admitted"; and the `!present` queue-position path prints no
  "already granted" clause. Behavioural red recorded first: current render of the
  no-queue case emits "0B granted / … across 0 admitted jobs".
- **Amend** `TestConfineListSliceReserveSummary`: its fixtures use real waiters
  (`present: true`), so expected structs gain `GrantedEstablished: true`. It does **not**
  pin the bug (it never exercised the no-queue case); the amendment is mechanical.

## Accepted consequence (name it, don't fix it)

`present` is false for a genuinely long-idle slice too, so `confine --list` on an idle
slice now reads `slice reserve: unevaluated / … ceiling` instead of `0B / 0 jobs`. This is
correct per the honesty doctrine (`unevaluated` ≠ fake zero) and per the ticket's own
finding — an absent queue is no longer a positive zero. It is a visible UX change and is
accepted, not a regression.

## Deferrals (explicit)

- **No scope-scan cross-check** to recover a "confident zero" from live-scope enumeration:
  new machinery on a reporting fix (simplicity HARD rule), and the ledger and the cgroupfs
  scan are different populations.
- **No adopt-on-read**: `--list` must not mutate admission state to establish the ledger.
- **AIRA-221 is out of scope** (owner chose fix-220 only); it ships as a known issue in the
  v0.4 notes.

## Process

Plan reviewed (advisor / Fable). TDD build (Opus). Independent adversarial build-review of
the diff (fresh subagent: false-pass/false-fail + porous-test hunt). Confined `make ci`
green, exit code recorded. PR → merge. Then v0.4 tag on merged master.

## Build-review folded (independent adversarial review, 2026-09-09)

Verdict SOUND_WITH_FIXES. All findings folded:

- **P1 — missed render surface (fixed).** The population-split line
  (`  of which: N confine scopes …, N adopted scopes …`, cmd/aira/main.go) and the
  `aira top` footer's population clause (cmd/aira/tui_top.go) are derived from the SAME absent
  snapshot and were still printing fabricated zeros directly under the fixed headline — and the
  `0 adopted scopes` there is the exact AIRA-105 misreading the ticket names in its symptom. Both
  are now gated on `GrantedEstablished` and read `unevaluated` in lockstep. Render/footer tests
  extended to assert the split does NOT appear when the ledger is absent.
- **False comment corrected (ticket-required).** `internal/daemon/admit.go`'s absent-queue comment
  claimed absence "positively establishes that nothing is waiting … must not render as unknown";
  corrected to say absence establishes nothing-waiting/no-holder (true) but says nothing about the
  granted/adopted ledger (the AIRA-220 point).
- **Accepted-consequence wording corrected.** The ledger is absent whenever nothing is
  connection-held right now — `pruneAdmitQueue` deletes the queue (adopted ledger included) on the
  last release — not only on a fresh/restart or long-idle slice. So a slice with live adopted jobs
  reads `unevaluated` between connections. The rendered line now carries a reason
  (`unevaluated (no admission ledger)`). Struct + daemon comments corrected to match.
- **`GrantedEstablished` meaning documented (not tightened).** It means "a queue/ledger object
  exists; its adopted half is as fresh as the last successful scan." Two accepted gaps a true bit
  does not rule out — a failed adopted scan (stale/zero adopted with the `slice scope caps:
  unevaluated` line beside it signalling the failure) and a sub-ms first-scan window — are
  documented rather than machined away, per the simplicity HARD rule.
- **JSON/MCP wire contract.** The bit rides the wire as `granted_established`, matching the
  struct's existing `*Known`-companion idiom (CapAggregateKnown, SystemCPUKnown). AIRA-178's
  operating note is amended so machine readers check it before trusting `granted_bytes`/`jobs`.
- **Correction to this plan's earlier claims:** the queue-position "already granted" note
  (confine_queue_position_linux.go) is NOT reachable with an absent ledger from a REAL daemon (its
  gate runs only after `QueuePosition > 0`, which requires a queue ⇒ present). The change there is
  a harmless client-contract hardening and its new subcase is a valid contract test, but it is not
  a "second live render site of the same bug" as first stated.
- **Deferral recorded:** the ticket's suggested "!present with zero live scopes stays a genuine
  zero, with a false-pass twin test" (cross-checking the cgroupfs scope scan) is deliberately NOT
  built. Real reason: a ci-shim post-restart job has neither a scope nor a lease, and the scope
  scan is up to ~1s stale, so cross-checking would trade one honesty edge for another while adding
  machinery the simplicity rule discourages. `unevaluated` on an absent ledger is the honest
  answer regardless of live-scope count.
