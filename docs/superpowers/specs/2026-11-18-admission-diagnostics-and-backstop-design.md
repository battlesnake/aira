# Admission diagnostics (AIRA-353), with AIRA-352 held

Status: plan, round 4, for the Fable plan gate. **The release change is AIRA-353 alone.** AIRA-352
is held (§2), and the owner's decision on it is open (§11, Q2). AIRA-353 touches the evaluator
(`internal/daemon/admit.go`), so it gets the full two-loop: Opus builds, Fable reviews.

**The owner's decision (2026-11-18).** AIRA-350's small-job lane is held: measure first (353), plus
the small backstop fix (352), batched. This spec designs no lane and builds none.

Code references are to `a7484074` (master). The data is not committed, because it names other
sessions' jobs: the journal extract `~/tmp/aira-350/freeze-journal.txt` and the store copy
`~/tmp/aira-350/state-copy.db`. The committed 352 reproduction is
`docs/dev/aira-352-pushed-backstops.py`, with its output in `aira-352-pushed-backstops.out.txt`.

## 0. Round 4: the non-convergence rule fired, and the re-base

Rounds 1, 2 and 3 each ended with blockers, so this round re-bases the design instead of patching it.
Round 3 raised 11 findings, two of them duplicates (§10). By this round's classification, seven were
induced by earlier fixes, three were there from the start, and one (finding 10) was structural. The
three original ones are the `waited` gate, the self-test and the parser, all present in round 1
(`29b751e8`). The findings fall into four families:

| Family | Findings (round) | Re-base that makes the family impossible |
|---|---|---|
| A. Per-pass held-time accounting | 3, 4 (r2); c, e (r3 on old §M); 10 (r3) | Cut it. One timestamp, `firstBudgetAt`, and no interval accounting. This is finding 10's collapse, which came from a reviewer, not from the builder. |
| B. Population and censoring | 5, 9 (r2); 1, 2, 6, 7, 11 (r3) | Log every job-level waiter at its terminal transition, with no `waited` gate. Each sub-reservation filter has a killing test. An unclean end reopens the window. A graceful split is bounded on the safe side (`S_upper`, §3.5). |
| C. AIRA-352 | 1, 2, 6, 7, 10-13 (r2); 3, 4, 9 (r3) | Not built. §2 is a decision record, not a design. |
| D. The script's oracle | 5, 8 (r3) | Exact verdict vectors, one line literal shared with the Go test, and script mutations. |

## 1. The greenfield minimum, and the gap

The task is to measure what a lane could address against the rule in §3.5. The bare minimum, per
job-level waiter, is its booking, its wait, and the instant the head's budget first withheld it while
it fitted. That is one field, one line at the grant, and a script that reads the journal.

| Addition over the minimum | Why it is needed | Size |
|---|---|---|
| A line at removal, for a waiter that leaves ungranted | Otherwise those waits vanish from W, the denominator. It is one call at the single removal point (`admit.go:3060`). | about 8 lines |
| `S_upper` in the script | A graceful restart splits a wait, and the second segment loses span. 24 graceful stops in 22 days (journal, measured, §5). | script only |
| Guard tests T6 and T7 | Invariant 1, and round-3 finding d: a guard that cannot fail proves nothing. | tests only |
| `noteBudgetHold`, a 3-line method | It gives the guard a single write site to pin. | 3 lines |

## 2. AIRA-352: held, not in this release

**Decision.** AIRA-352 is not built in this release. This commit sets its ticket to `hold: true`
and milestone `v0.47`, beside AIRA-350. The owner asked for 352 to be batched with 353 on
2026-11-18, but 352 cannot meet the acceptance criterion that came with that request (item 1 below).
That conflict is the owner's to resolve (§11, Q2). A waiver of the criterion reopens 352 as its own
spec round and plan gate. Nothing in this section is a build instruction.

**The defect, confirmed.** The backstop pass clears the AIRA-59 anchor at its start
(`admit.go:2525-2540`), before the head is known. When the head is pushed, the pushed case comes
before the yield case (`admit.go:2690-2697`), so the pass grants nothing behind it, and the anchor
is already gone. The next budget withholding then arms a fresh hold (`admit.go:2762-2769`). The
candidate fix deferred the clear to the first pass whose head is not pushed.

**What the backstop is documented to guarantee** (queue-order spec §3.5, "Bounds"):
- unpushed head: one backfilling pass per `maxHold` of strict hold;
- everyone else: the slice is never strictly frozen for longer than `maxHold` without one full
  backfill pass;
- pushed head: "not covered by the backstop". This is the owner's "drain the slice".

No document says that a push consumes the backstop.

**Why it is held:**
1. **The criterion ("no waiter granted today waits longer") cannot be met.** A restored pass grants
   what today's code withholds, and that room can be the room an unpushed head behind the push
   needed. The counterexample below was traced against `admit.go:2538`, `2690-2697` and
   `2762-2769`, and confirmed independently in round 3.
   - Setup: a 64 GiB slice with leases of 49, 6 and 3 GiB (room 6). At the head is a pushed P of
     7 GiB. Behind it are an unpushed H of 8 GiB with its budget spent, and W of 1 GiB.
   - The backstop pass at t=0 is lost to P. The 6 GiB lease ends before t=20, and the 3 GiB lease
     before t=30.
   - Today: at t=20, P is granted (room 5). The phase is idle, so the budget withholds W and arms a
     hold. At t=30 the room is 8, and H is granted.
   - With the fix: at t=20, W is backfilled (room 4). At t=30 the room is 7, which is less than 8,
     so H waits.

   Any fix that grants at the restored pass has this property.
2. **It would not have changed the incident it was filed for.**
   - At 13:21:07 the backstop pass named the pushed head 14816. The phase was idle from 13:21:08,
     and the next `hold` was at 13:22:22 (head 14800). This was re-checked in the journal extract.
   - So no pass from 13:21:08 to 13:22:22 withheld a fitting waiter on budget, since that would have
     armed a hold (`admit.go:2762`). The deferred clear would have landed on the 13:21:47 pass, which
     granted 14816 and had nothing to backfill. That pass is as reported in round 2: the extract
     holds no grant lines, so it was not re-derived this round. Every grant in the incident is
     unchanged.
3. **Its yield is small, and only estimated.**
   - Of 456 backstop passes in 3.5 days, 8 are candidates (`aira-352-pushed-backstops.out.txt`).
   - The candidate rule cannot tell a push from a `--priority high` head
     (`aira-352-pushed-backstops.py:14-18`). So "3 changed" is an estimate, and 2 candidates are
     unresolved.
   - Waiter-hours are not measured, and no per-waiter figure bounds them. A waiter granted at a
     restored pass might otherwise have missed later backstops as well, since a backstop grant still
     needs a fit (`admit.go:2674`).
4. **It does not restore the documented bound.** A push freezes everyone behind it, outside the
   backstop (§3.5, "Pushed head"). The fix shortens some waits after a push and lengthens others
   (item 1).
5. **It brings a second regression.** A pushed waiter can drop out of head eligibility for one pass,
   on a throttled-ceiling dip or a stale VRAM snapshot (`admit.go:2770`). The deferred backfill then
   passes it.

A backstop pass lost to an exclusive drain or hold is the same class, and it is held with 352.

## 3. AIRA-353: per-waiter wait and budget span

### 3.1 What the figures mean, and what they do not claim

All figures are per job-level waiter, and for diagnosis only.
- **`waited-ms`**: from the enqueue to the terminal transition. That is the grant, the rejection,
  or the removal of a still-queued waiter.
- **`since-budget-ms`**: from the first pass at which the head's budget withheld this waiter while
  it fitted RAM, CPU and VRAM (`budgetWithheld`, `admit.go:2694-2696`), to the terminal transition.
  It is 0 if that never happened.
  - It is the first-order saving had a lane granted the waiter at that pass, with everything else
    unchanged.
  - It is wall time, so it includes whatever followed that pass: capacity refusals, the restart
    freeze, unreadable-memory passes and the gate. That is correct for the counterfactual, because
    a waiter granted at that pass would have been running through all of them.
  - Passes before the first budget withholding are excluded (T1).
- **No held time is claimed.** The ticket's literal "how long it was held" split is not delivered.
  The per-pass accounting it needs is the family every round found defects in (§0, family A). It is
  offered to the owner as a choice, with its cost (§11, Q3).
- **No figure bounds a lane's net effect, in either direction.**
  - A lane's grants take room from others, and lane jobs that end early free room early.
  - Within a pass, every waiter behind the head is judged against the same room, because withheld
    waiters change no ledger. So the span overcounts joint feasibility.

  The thresholds in §3.5 are therefore heuristics.

### 3.2 The change (`internal/daemon/admit.go` only)

- **The field.** `admitWaiter` gets `firstBudgetAt time.Time`, for diagnosis only. It also gets one
  method with no result:
  `func (w *admitWaiter) noteBudgetHold(now time.Time) { if w.firstBudgetAt.IsZero() { w.firstBudgetAt = now } }`.
- **The write.** At `budgetWithheld = true` (`admit.go:2696`), add `waiter.noteBudgetHold(now)`,
  with the pass-start `now` (`2484`). It is the only call. Neither the pushed case (`2691-2692`) nor
  a capacity refusal calls it.
- **The logger.** `logHeldFitLocked(waiter *admitWaiter, outcome string, waitedMS int64)` has no
  result. It sets `since := 0`, and when `firstBudgetAt` is set,
  `since = waitedMS - elapsedMilliseconds(waiter.enqueued, waiter.firstBudgetAt)`. It then writes
  the line in §3.3 with `log.Printf`.
- **At the grant.** In the grant branch, after the existing outcome block (`2718-2723`, unchanged):
  `if !waiter.isSubReservation() { logHeldFitLocked(waiter, "granted", elapsedMilliseconds(waiter.enqueued, waiter.grantedAt)) }`.
  There is no `waited` condition (round-3 finding 1). A re-declared lease is created already granted
  (`newEstablishedWaiter`, `admit.go:2119`), so it never reaches this branch and is never logged.
- **At removal.** `releaseAdmitWaiterLocked` (`admit.go:3060`) is the single point that removes a
  waiter from `queue.waiters`. Before `waiter.state = admitReleased` (`3072`), and only if
  `!waiter.isSubReservation() && queue.server != nil`:
  - **`admitRejected`:** `logHeldFitLocked(waiter, waiter.outcome, waiter.waitedMS)`. The only three
    sites that reject a waiter freeze `waitedMS` at the rejection: `admit.go:2501` (`failfast`),
    `2977` (`saturated`) and `confine_kill_queued.go:225` (`cancelled`).
  - **`admitQueued`:** `logHeldFitLocked(waiter, "removed", elapsedMilliseconds(waiter.enqueued, queue.server.admitNowTime()))`.
    Client EOF, a peer context ending and a daemon stop all reach here, through the handler's return
    (`admit.go:1976-1979`) and its deferred release (`1945-1952`).
  - **`admitGranted`:** nothing, because the grant already logged it.

  `queue.server != nil` is there because 23 seam-test queues are built without a server (grep count).
  The production queue always has one (`admit.go:2182`).
- **Unchanged.** Nothing changes when `waited`, `outcome` and `waitedMS` are set. Those values feed
  the wire's immediate/waited split and the contention latch. Every grant, the ledger, the head,
  `bypassed`, the anchor and every existing log line are also unchanged.

### 3.3 The line, its modes and its volume

```
aira daemon: held-fit seq=<n> outcome=<granted|failfast|saturated|cancelled|removed> booking=<bytes> waited-ms=<n> since-budget-ms=<n>
```

- `booking` is `ledgerCharge()`, the unit of the budget and of AIRA-350's `y`.
- **Both modes, with no branch.** No decision reads the line in either mode. In ci-shim mode the
  daemon logs to its own `shim-daemon.log` (`internal/install/mode.go:360-376`), not to CI output.
  A branch would cost a conjunct and a test, with no reader to justify them.
- **Volume.** One line per job-level waiter, never one per pass.
  - The store copy holds 3,006-4,094 `confine` rows a day for 2026-11-14 to 11-17 (measured on the
    copy). That is a floor, because `aira run` and un-nested `confine-reserve` also enqueue
    job-level waiters.
  - The ceiling is every enqueue, sub-reservations included: 7,300-17,800 a day, as reported in
    round 3.
  - For comparison, the daemon already writes a throttle line every minute (journal extract).

### 3.4 Tests (AIRA-228 seam)

**Setup.** `queueOrderServer`, `queueOf`, `heldLease` and `queuedWaiter`
(`admit_queue_order_test.go:22-50`, `admit_freeze_test.go:30-32`): a 64 GiB slice, zero headroom,
`maxHold` = 1 minute, and the clock stepped by hand. The log is captured as in
`admit_freeze_test.go:653-658`, with flags 0.

| # | Setup | Assert | Mutations it kills |
|---|---|---|---|
| T1 meaning, exact line | W (1 GiB, seq 3) is enqueued at t=-5 behind an unpushed head H with its budget spent. t=0: the memory read fails. t=5: refused on capacity. t=10: fits, and the budget withholds it. t=20: the memory read fails. t=25: withheld again. H is removed at t=27, and W is granted at t=30. | W's line is exactly `aira daemon: held-fit seq=3 outcome=granted booking=1073741824 waited-ms=35000 since-budget-ms=20000`. H's line has `outcome=removed since-budget-ms=0`. | M1 (35000), M2 (25000), M3 (5000), M12 (the literal). Per-pass crediting would give less than 20000. |
| T2 push is not budget | W fits behind a pushed P at t=0 and t=10. P is granted at t=20, and an unpushed H with its budget spent withholds W at t=20. H is removed, and W is granted at t=30. | `waited-ms=30000 since-budget-ms=10000` | M4 (30000) |
| T3 never-evaluated grant | W is enqueued at t=0. The memory read fails at t=10 and at t=20. W is granted at t=30, its first evaluated pass. | `outcome=granted waited-ms=30000 since-budget-ms=0`. The wire outcome is still `immediate` and `waitedMS` is 0 (unchanged). | M5 (no line), M13 (0) |
| T4 terminal paths, frozen | W is enqueued at t=-4, and the budget withholds it at t=0 and t=10. It ends by: (a) `timeoutAdmitWaiter` at t=14; (b) `cancelQueuedConfineLocked` at t=14; (c) the fail-fast latch at the t=14 pass; (d) client EOF at t=14. (a) to (c) are released at t=19. Separately, (e) W' is enqueued at t=0 and released at t=7 with no pass between; (f) W'' is enqueued at t=0, rejected by the fail-fast latch at the t=5 pass (its first), and released at t=9. | (a) to (d): `waited-ms=18000 since-budget-ms=14000`, outcome `saturated`, `cancelled`, `failfast` or `removed`. (e): `removed waited-ms=7000 since-budget-ms=0`. (f): `failfast waited-ms=5000 since-budget-ms=0`. One line each. | M14, M6 ((e), (f)), M10 (23000), M15 |
| T5 one line per job-level waiter, none for sub-reservations | (a) An immediate grant. (b) A waited grant, released later. (c) A waited sub-reservation, granted and then released. (d) A sub-reservation rejected by the fail-fast latch, then released. (e) A sub-reservation released while still queued, on EOF. (f) A re-declared lease (`newEstablishedWaiter`), released. | Exactly two lines: (a) `waited-ms=0` and (b), both at their grant. | M7 (c), M8 ((d), (e)), M9 ((b), (f)), M5 ((a)) |
| T6 guard, structural | An AST scan of the non-test files in `internal/daemon`, as in `admit_pin_charge_guard_test.go`. | Every `.firstBudgetAt` selector is in `noteBudgetHold` (exactly 2) or `logHeldFitLocked` (exactly 2). Neither function has a result. `noteBudgetHold` is called once, in `evaluateAdmitQueue`. `logHeldFitLocked` is called three times, once in `evaluateAdmitQueue` and twice in `releaseAdmitWaiterLocked`. | M11. A second `noteBudgetHold` call. A result added to either function. |
| T7 guard, differential | One 52 GiB lease (room 12). H is 16 GiB with its budget spent; W1 is 1 GiB and W2 2 GiB. Passes at t=0 (arm), t=30 (hold) and t=61 (backstop: W1 and W2 granted). W3 (1 GiB) arrives at t=62 and re-arms. Run once as is, and once with every waiter's `firstBudgetAt` set to now - 1h before each pass. | The same granted set, `bypassed`, anchor and `freezeHolderSeq` after every pass. | M11, through any path |

**Go mutations** (`docs/dev/aira-353-mutation-check.py`, on the pattern of
`aira-324-mutation-check.py`; each must go RED):

| # | Mutation |
|---|---|
| M1 | The span is measured from the enqueue. |
| M2 | `noteBudgetHold` is also called at a capacity refusal. |
| M3 | `noteBudgetHold` overwrites the field instead of setting it once. |
| M4 | `noteBudgetHold` is also called in the pushed case. |
| M5 | The grant site is gated on `waiter.waited`. |
| M6 | The removal site is gated on `waiter.waited`. |
| M7 | `!isSubReservation()` is dropped at the grant site. |
| M8 | `!isSubReservation()` is dropped at the removal site. |
| M9 | A granted waiter is logged at its release. |
| M10 | A rejected waiter's `waited-ms` is recomputed at removal. |
| M11 | `fits = fits && waiter.firstBudgetAt.IsZero()` in `evaluateAdmitQueue`. |
| M12 | A field is renamed or reordered in the line. |
| M13 | The grant site logs `waiter.waitedMS` instead of the elapsed time to `grantedAt`. |
| M14 | The removal-site line is deleted. |
| M15 | The rejection outcome is hard-coded instead of read from `waiter.outcome`. |

The check also asserts that T1's literal appears verbatim in the script's self-test.

### 3.5 The analysis script, and the decision rule fixed before the data

**The script.** `docs/dev/aira-353-held-fit.py` uses the standard library only. It reads the output
of `journalctl --user -u aira-daemon.service -o short-unix --since <cutover> --until <later>`, or a
saved extract, and nothing else.
- It recognises systemd's `Started`, `Stopping` and `Stopped aira-daemon.service` messages, and
  `Failed with result`.
- From the daemon (`aira[PID]:`), it reads `fairness-freeze` lines and `held-fit` lines.
- A `held-fit` line is parsed strictly. The fields are in §3.3's order. The outcome is one of the
  five. Every number is a non-negative integer, and `since-budget-ms <= waited-ms`.
- `end` is the journal time in ms, and `start = end - waited-ms`. All arithmetic is in integer ms.

**The rule.** The owner may change any number marked (O) before the cutover, never after.
1. **Window.**
   - It opens at the first `Started` at or after the cutover.
   - An unclean end reopens it at the next `Started`. An unclean end is any of these: a `Started`
     with no `Stopped` since the previous `Started`; a `Stopped` with no `Stopping` since the
     previous `Started` (a stop not issued through systemd, such as `daemon.Stop` under
     `Restart=always`, `cmd/aira/dispatcher.go:1068`, which rule 3's check could not see); or any
     `Failed with result` message. Such an end can lose the first segment of every wait in flight,
     and nothing in the journal bounds that loss.
   - It closes at U: the first `Stopped` at least 7 days (O) after the opening, with no
     `Failed with result` since the preceding `Started`. A drain timeout exits with status 1
     (`cmd/aira/daemon_command.go:71-73`, `121-124`), so systemd reports it as `Failed`.
2. **Population.** The `held-fit` lines with `end` in [open, U].
   - Small class: `booking <= 1073741824` (1 GiB, O), which is AIRA-350's `y`.
   - Large class: the rest, reported only.
   - W is the sum of `waited-ms`, and S the sum of `since-budget-ms`, per class.
3. **UNEVALUATED** if any of these holds:
   - no complete window exists in the input;
   - a line containing `held-fit` fails the parser;
   - a daemon PID in the window wrote a `fairness-freeze` line but no `held-fit` line, which means an
     old binary;
   - the small class's W is 0;
   - a graceful stop in the window wrote a `removed` line at or after its `Stopping`, and the next
     `Started` T has no `held-fit` line starting in [T, T + N - 1 s]. Then the successors of the
     waits in flight cannot be shown to fall inside N (rule 4).
4. **`S_upper`.** It is S plus a term for every small line whose `start` is in [T, T + N] for some
   `Started` T in the window, the opening one included. N = 60 s (O). The term is
   `end - since-budget-ms - T_prev`, where `T_prev` is the latest `Stopped` before T, or the opening
   instant if that is later. A line within N of several `Started` instants (back-to-back restarts)
   counts once, against the earliest of them, which gives the larger term.
   - Why: a graceful stop logs each queued waiter as `removed`, which is its first segment.
   - The client reconnects every 500 ms, with no deadline (`internal/runner/admission_linux.go:283-341`;
     `leaseKeeperReconnectGap`, `lease_keeper_linux.go:24`). It re-enqueues as a new waiter within
     about 0.5 s of the new daemon listening.
   - The second segment's line then misses at most the time from the stop to its own first budget
     withholding, or to its end if there is none. That is the term.
   - A line that is not a successor only raises `S_upper`, which moves away from DROP.
   - The unit sets no `Type=` (`internal/install/assets/aira-daemon.service.in`), so `Started`
     precedes listening. Rule 3's last check is what establishes that listening came within N.
5. **DROP the lane** (close AIRA-350 and report) iff `100 * S_upper < 10 (O) * W`, or
   `S_upper < 36,000,000` ms (10 h, O). Both comparisons are strict, on the small class.
6. **Otherwise NONE.** There is no outcome. W, S and `S_upper` per class go to the owner for
   AIRA-350.

**Output.**
- The first line is `verdict: DROP`, `verdict: NONE` or `verdict: UNEVALUATED <reason>`.
- The exit status is 0 for DROP or NONE, 3 for UNEVALUATED, and 2 for a usage error.
- The report then gives, per class, the line counts by outcome and W, S and `S_upper` in hours. It
  also gives the parsed and total `held-fit` lines, and each graceful stop with its `removed` lines.
  For each `Started`, it gives the lines starting within N and the earliest line-start offset.

**`--self-test`** runs embedded vectors and asserts each verdict and figure exactly. All vectors
are small class, and the window is 7 days with no restart unless stated.

| # | Input | Verdict |
|---|---|---|
| V1 | W 200 h, S 15 h | DROP (share 7.5%, hours not met) |
| V2 | W 50 h, S 9 h | DROP (hours; share 18%) |
| V3 | W 200 h, S 20 h | NONE (share exactly 10%) |
| V4 | W 50 h, S 10 h | NONE (exactly 10 h) |
| V5 | W 50 h, S 11 h | NONE |
| V6 | large-class lines only | UNEVALUATED (zero W) |
| V7 | `Started` at day 0, an unclean end (a second `Started`, no `Stopped`) at day 2, and graceful stops at days 7.5 and 9.5. Lines before day 2 carry S 50 h; the lines after it are V1's. | DROP (the window is days 2 to 9.5) |
| V8 | V7 with input to day 8.9 | UNEVALUATED (no window) |
| V9 | V7 with `Failed with result` in place of the missing `Stopped` | as V7 |
| V10 | one malformed `held-fit` line | UNEVALUATED |
| V11 | a PID with `fairness-freeze` lines and no `held-fit` line | UNEVALUATED |
| V12 | W 100 h, S 9 h, and one graceful stop that wrote no `removed` line. A line starts 1 s after the next `Started` T, with `waited-ms` 2 h and `since-budget-ms` 0. Also the same input with that line at T + 61 s. | NONE (`S_upper` > 10 h and > 10%); DROP |
| V13 | V12's second half, with a `removed` line written at the stop | UNEVALUATED (listening not shown within N) |
| V14 | T1's literal, behind a journal prefix | its fields parse exactly |
| V15 | Two graceful restarts 5 s apart (`Stopped` S1, `Started` T1, `Stopped` S2, `Started` T2). One line starts at T2 + 1 s. | `S_upper - S` is exactly that line's `end - since-budget-ms - S1` |
| V16 | V7 with a `Stopped` at day 2 that has no `Stopping` before it | as V7 |

**Script mutations** (the mutation check runs `--self-test` on each; each must go RED):

| # | Mutation | Vector that fails |
|---|---|---|
| S1 | `<=` in the share arm | V3 |
| S2 | `<=` in the hours arm | V4 |
| S3 | the share arm removed | V1 |
| S4 | the hours arm removed | V2 |
| S5 | the successor term removed | V12 |
| S6 | the unclean-end reopen removed | V7 (NONE), V8 (evaluated) |
| S7 | a parse failure skipped | V10 |
| S8 | the zero-W check removed | V6 (DROP) |
| S9 | the old-binary check removed | V11 |
| S10 | the successor window unbounded | V12, second half |
| S11 | the listening check removed | V13 |
| S12 | a multiply matched line counted against the latest `Started` | V15 |
| S13 | a `Stopped` with no `Stopping` treated as graceful | V16 |

**Not reused.** AIRA-350's association figure (about 5 of 115.6 small wait-hours, from
retention-capped rows) is not a forecast. Nor are the suite-parent figures (round-3 finding f).

## 4. Invariants

1. **No admission decision reads `firstBudgetAt`** (T6, T7). Every existing test passes unchanged,
   the wire's immediate/waited outcome included.
2. **`0 <= since-budget-ms <= waited-ms`.** The span starts at the first budget withholding of a
   fitting waiter, never at a capacity refusal or a push (T1, T2).
3. **Exactly one line per job-level waiter enqueued in this process**, at its grant or its removal,
   with its figures frozen at the terminal transition (T3-T5). Sub-reservations and re-declared
   leases write no line.

## 5. Risks

- **R1. Volume** (§3.3): about 3,000 to 18,000 lines a day.
- **R2. Diagnosis leaking into decisions** (T6, T7).
- **R3. No clean window.**
  - From 2026-10-27 to 11-18 the journal shows 28 `Started`, 24 `Stopped`, no
    `Failed with result`, and 3 `Started` with no `Stopped` before them (WSL restarts; measured).
    The window reopens after each unclean end. If none completes, the verdict is UNEVALUATED and the
    owner extends `--until`.
  - Graceful stops (14 from 2026-11-14 to 11-18) do not end a window, but each one splits waits
    (rule 4), and a slow start can make the verdict UNEVALUATED (rule 3).
- **R4. The cutover's own successors are not checked.** The old binary writes no `removed` lines,
  so rule 3's listening check cannot apply at the opening `Started`. The effect is at most the waits
  in flight at the cutover, up to their first budget withholding. This is accepted.
- **R5. The span is a first-order counterfactual, not a bound** (§3.1). The thresholds are
  heuristics.

## 6. Expected yield

| Change | What it addresses | Share | Basis |
|---|---|---|---|
| 353 | No decision changes. It measures W, S and `S_upper` per class, and DROP or NONE, so AIRA-350 is decided on the figures. | — | — |
| 352 (held) | Backstop passes lost to a pushed head | about 3 of 456 backstop passes in 3.5 days; waiter-hours unmeasured | estimated (§2) |
| 352 (held) | The 13:21:07 incident | none | measured (§2, item 2) |

## 7. Deferrals

- **AIRA-352**, and a backstop pass lost to an exclusive drain or hold: held, with the owner's
  decision open (§2, Q2).
- **AIRA-350's lane:** held until §3.5's rule is read.
- **Held time by cause (budget or push):** the owner's choice (Q3).
- **A CPU diagnostic for capacity refusals:** the pre-existing gap at `admit.go:2729-2735`.
- **Waits lost to an unclean end:** excluded by reopening the window (rule 1).

## 8. Cut list

| Option | Why cut |
|---|---|
| AIRA-352 in this change | §2. |
| Building any lane | The owner's decision (AIRA-350 held). |
| Held time by budget and by push (B, P): per-pass interval accounting, a close loop over all waiters, the 30 s cap, the room-consistent counter, and a TRIAL threshold on B | They decide nothing (finding 10), and every round found accounting defects in them. Q3 offers them to the owner. |
| The `waited` gate on the line | It drops real waits (finding 1). |
| Outcome `stopped` | Its only reader, the cut share, is gone. Rule 3 finds a drain's removals by journal time. |
| The cut-share validity rule | It cannot bound missing data (findings 2 and 6). Replaced by rules 1 and 4. |
| A "line starts before the window" check | It cannot happen. The window opens at a `Started`, and every wait is timed from an enqueue in that process. |
| A per-pass log line | Passes reach 4 a second at the default 250 ms poll (`internal/daemon/paths.go:47`). The per-waiter line carries the totals. |
| A store column or telemetry row | History rows are retention-capped and carry no scope id (round-3 finding b). |
| A ci-shim branch | It has no reader. |
| A daemon listen or shutdown line | systemd's messages, plus rule 3's check, cover it. |
| Logging sub-reservations | They are exempt from the head rule, and logging them would multiply the volume. |
| The old D1-D5, and §M.6's single-review path | D1 pinned the wrong meaning, and D5 could not fail. This is an evaluator change, so it gets the full two-loop. |

## 9. Files, review and rollout

- **Files.**
  - `internal/daemon/admit.go`: the field, `noteBudgetHold`, `logHeldFitLocked`, one write and two
    log sites.
  - Tests T1-T7 in `internal/daemon/admit_held_fit_test.go`.
  - `docs/dev/aira-353-held-fit.py` and `docs/dev/aira-353-mutation-check.py`.
  - There is no protocol, wire, configuration, CLI, MCP, TUI or generated-text change.
- **Review.** The full two-loop: Codex Sol at `xhigh` reviews the build, and Fable gives the final
  review.
- **Rollout.**
  - Send the shared daemon-restart heads-up to peer sessions, after `aira confine --list`.
  - Run `aira install`. It restarts a present daemon whose binary is stale. The cutover's `Started`
    opens §3.5's window: record that instant.
  - No skill reinstall is needed.

## 10. Review logs

**Round 3 on this spec (`d85aaafb`).**

| # | Sev | Finding | Checked against | Disposition |
|---|---|---|---|---|
| 1 | P1 | The journal denominator omits real waits | `admit.go:2418` (`waited` starts false), `2506-2517` (returns without setting it), `2718-2723` | **Confirmed.** The `waited` gate is deleted. `waited-ms` is timed from the enqueue at each terminal transition, and the wire fields are unchanged. T3, T4 (e) and (f), T5 (a); M5, M6. |
| 2 | P1 | The cut share cannot bound missing data | spec r3 rule 3 | **Confirmed.** The cut share is deleted. An unclean end reopens the window (rule 1), and graceful splits go into `S_upper` (rule 4). |
| 3 | P1 | AIRA-352 is unresolved for the requested release | the counterexample, re-traced in round 3 | **Confirmed. A plan fix cannot close it**: the owner's criterion is unmeetable. 352 is held at v0.47, and §2 is self-contained, with no build instruction. The release is 353-only, and Q2 records the conflict with the batch instruction. |
| 4 | P2 | The `maxHold` saving cap is false | `admit.go:2674` | **Confirmed.** Removed from §2 and Q2. |
| 5 | P2 | The analysis decision has no oracle | spec r3 §3.6 | **Confirmed.** V1-V16 with exact verdicts, integer comparisons, zero W UNEVALUATED, and script mutations S1-S13. |
| 6 | P2 | The split's S loss sits in the successor line | `admission_linux.go:283-341`, `admit.go:2418` | **Confirmed.** `S_upper` as hinted, plus the gap since the last `Stopped`, and the listening check (rule 3). Graceful stops are added to R3. The reviewer's 1.4-in-flight figure is an estimate and is not re-derived. |
| 7, 11 | P2 | Removal-site conjuncts are unkilled | `worker_admit.go:365-371`, `494-512` | **Partly confirmed.** `!isSubReservation()` was unkilled; T5 (d) and (e) now kill it (M8). The cited path is wrong: a worker probe (`max_wait_ms=0`) answers from a snapshot and never enqueues (`worker_admit.go:365-371`). A sub-reservation is rejected only by the fail-fast latch. The `waited` conjunct is deleted (finding 1), so 7(b) and 11(b)'s "no line" is superseded: that waiter now writes one (T4 (e)). |
| 8 | P2 | Nothing ties the line format to the parser | spec r3 §3.6 | **Confirmed.** T1's exact literal is V14, and the mutation check asserts it appears in both. A failed parse is UNEVALUATED, and parsed/total is reported. |
| 9 | P2 | AIRA-352's disposition is unsafe both ways | `.aira/tickets/AIRA-352.md` | **Confirmed.** `hold: true` and milestone v0.47 in this commit. A waiver reopens 352 as its own spec round and plan gate. |
| 10 | P2 | B and P decide nothing | spec r3 §3.6, rule 5 | **Confirmed. This is the re-base** (§0). Collapsed to `firstBudgetAt` and one line. The span is renamed `since-budget-ms`, and it claims no held time. |

**Round 3 on the old §M (lane spec), as re-disposed by the collapse.**
- (a) No figure is called an upper bound, and T1 pins the meaning.
- (b) W is in the same line, and the script reads the journal alone.
- (c) No held time is claimed. Unevaluated passes after the first withholding are part of the
  counterfactual, and those before it are excluded (T1, §3.1).
- (d) Guards T6 and T7, and the full two-loop.
- (e) Moot: nothing is accounted across passes.
- (f) Not reused.
- Ungranted waiters: a line at removal (T4).

**Round 2 (`5f2e596e`), in brief.**
- 352's findings (1, 2, 6, 7 and 10-13) led to the hold (§2).
- 353's interval-accounting findings (3 and 4) are moot after the collapse.
- The stop and crash censoring findings (5 and 9) are now rules 1, 3 and 4.
- The causal-claim and decorative-threshold findings (8 and 14) stand as fixed: no bound is claimed,
  and only S decides.

## 11. Open questions (owner)

1. **The rule's numbers** (O in §3.5): the 7-day window, the 1 GiB class, 10% and 10 h for DROP, and
   N = 60 s.
2. **AIRA-352.** Your 2026-11-18 instruction batched it with 353, but its acceptance criterion
   ("no waiter granted today waits longer") cannot be met (§2, item 1). The choices:
   - close it (recommended);
   - keep it held;
   - waive the criterion, which reopens 352 as its own spec round and plan gate.

   Restoring the pass shortens some waits after a push and lengthens others. It would not have
   changed the 13:21 incident.
3. **Held time by cause.** The ticket asked "how long each waiter fitted but was held", split by
   budget and by push. This spec delivers the span since the first budget withholding instead. The
   split costs per-pass interval accounting: a struct, a close loop over every waiter at each pass
   start, a cap for gaps between passes, and about four more tests. That is the family every review
   round found defects in, and nothing in the decision rule reads it. Recommended: no.
