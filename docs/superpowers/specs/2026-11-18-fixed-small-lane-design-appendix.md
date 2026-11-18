# Small-job lane (AIRA-350): appendix, history and superseded designs

Companion to `2026-11-18-fixed-small-lane-design.md` (round 4). Nothing here is part of the
decided design; it records why earlier forms were dropped, so they are not re-proposed. The full
round-3 text is `git show 0bca5aab:docs/superpowers/specs/2026-11-18-fixed-small-lane-design.md`.
The withdrawn dynamic lane is `2026-11-18-small-job-lane-design.md` (four rounds, not approved).

## A1. Sequence of designs

| Round | Form | Outcome |
|---|---|---|
| Dynamic lane, rounds 1-4 | A per-head allowance inside today's ceiling, so small jobs pass a head within it | Withdrawn: four rounds without approval; it could not show that a head cannot starve. |
| Fixed lane, round 1 | A pool of fixed size `x` on top of today's ceiling | Review: 6 P1, 12 P2. Host exposure (family A), head independence (B), restart (C). |
| Fixed lane, round 2 | A partition: the lane out of the ceiling and gated on the whole-slice fit `W` | Review: 2 P1 and 6 P2, of which six of the eight substantive findings were fix-induced (families A, C). Sol was OOM-killed at its 1 GiB cap. |
| Fixed lane, round 3 | The fork stated with a proof; rows A-D (below); derived pools; blocked on the owner | Superseded by the owner's 2026-11-19 decision. |
| Round 4 | Priority and a cap of `x` inside the one ceiling, guard G | This spec. |

## A2. Why round 4 drops what it drops

- **On top of the ceiling (round 1, round 3 rows A and D).** It was rejected in three review
  rounds. With every booking fully used, the worst-case MemAvailable is `freeMin + H(J) - a`:
  7.6 GiB at 26 jobs for row A, inside the 8 GiB band in which the watchdog kills heavy processes
  outside AIRA (`internal/daemon/watchdog.go:24`). The owner then chose "stolen from the main slice".
- **The partition (rows B, C).** It needed a main ceiling `E_m = min(E - x + a, M - x)` and main's
  room `P` beside the whole-slice fit `W` and a kernel-cap conjunct `K`. It also needed an overflow
  charge `max(0, S - x)`, an after-grant fit for a suite parent's move into main and a suspension
  on tiny slices. Ten readers of the ledger had to be reclassified (round 3 §2.1 table). Round 4
  keeps one ceiling, so main's room is today's `available` and no reader changes.
- **The fork (round 3 §2.0).** F (lane needs only its own room), H (host bound as today) and N
  (main's room independent of the lane) cannot all hold while the pressure ceiling falls. The
  owner's decision drops N, and pays for it with priority. Guard G bounds that cost. Neither F nor
  H is relaxed.
- **Stored pool flag (round 2).** The establish could not read `laneOn` (`enqueueReDeclare` passes
  `maximum` 0, `admit.go:2239-2241`), and every restart re-silted the lane (10 daemon starts in
  93.2 h, M). Round 3 derived pools from frame fields. Round 4 derives one sum.
- **The live-child clause (round 3).** "A lease with a live sub-reservation is not small" existed so
  that suite parents would not fill a partitioned lane. Round 4 excludes parents from eligibility.
  Counting them in `S` costs 0.0 points at `x = 8` (1.0 with silt) (E, replay 1h vs 1h').
- **Round 1's rule 4 (a lane grant leaves the head's CPU slots free now).** It shuts the lane
  whenever the head is slot-bound, which the incident's 32 of 32 slots made the normal state. Round
  4's G asks only that the head fit beside the small leases. It is residual R4's veto option.

## A3. Round 3's costing of the rows, at `x = 4 GiB` (for reference; superseded)

| | A: on top, always open | B: out of main, shut when RAM tightens | C: out of main, always open | D: half and half |
|---|---|---|---|---|
| Big jobs' room against today | same or more | 2.8 GiB less on average | as B | about 0.8 GiB less |
| Big head, model cases (today 60 s / 210 s) | 60 s / 210 s | 240 s / never | 240 s / never | not simulated |
| Free RAM if every job used its whole booking (26 jobs) | 7.6 GiB | 11.6 GiB | 11.6 GiB | 9.6 GiB |
| Small-job wait removed (estimate, no CPU gate) | about 77% | below 77% | about 77% | about 77% |

## A4. Replay sections superseded by 1i

Sections 1, 1b, 1c, 1h and 1h' of `docs/dev/aira-350-fixed-lane-replay.out.txt` used a lane
footprint of booking plus 64 MiB and no ceiling gate. Section 1f (the partition's cost to main) has
no meaning now. They are kept byte-identical in the output for provenance, and round 4 cites them
only for the stored-flag and parent comparisons (R8, the cut list). Section 1i is round 4's model.

## A5. Review log

**Fixed lane round 1** (Sol `xhigh` and a second reviewer): family A, host exposure (lane on top);
family B, head independence; family C, restart (an order-independent establish); D-H, P2s.

**Round 2** (one reviewer; Sol OOM-killed at 1 GiB, peak 1048752 KiB). By root cause:

| Family | Findings | Root cause | Fix-induced | Disposition (round 3) |
|---|---|---|---|---|
| A, the fork | P1 lane not fixed; P1 default reverses the owner; P2 one-sided question; P2 yield omits `W` | `W` gate plus partition default | yes, all four | fork stated, rows costed, owner asked |
| C, restart | P2 establish cannot read `laneOn`; P2 re-silting | stored flag | yes, both | derived pools |
| B, lane state reaching main | P2 a lane parent brings head-exempt workers | client hint skipped for an empty signature | no | unsigned not eligible |

**Non-convergence (round 3).** Six rounds without approval across both designs. The recurring
question was "who pays for `x`?", which only the owner could answer. Owner, 2026-11-19: main pays,
and small jobs get priority.

## A6. Round 4 process and provenance

- **Process.** The design was set by the owner and is not reopened. Every residual is a one-line
  accept-or-veto item (§4). No head-cannot-starve proof is claimed; §2.3 states a bounded guarantee.
- **Runs** (under `aira confine`, named, exit 0, no `hint:` line). `aira350-r4-replay`: section
  1i's first run. Its head reserves did not parse (a lazy regular expression), and its peak-rss
  equalled the 256M pin (page cache of the store copy; no OOM). `aira350-r4-replay-b`: the fixed
  run, peak-rss 80520K. Both used `--db ~/tmp/aira-lane2-store/state-copy.db --snapshot
  .../snapshot-2120.json --journal .../journal-r3.txt`. Sections 0-1h, 1c-1g, 2 and 3 of the output
  are byte-identical to round 3's.
- **Checked by hand, not by script.** The 13:55 snapshot (`~/tmp/aira-350/live-snapshot-1355.json`)
  holds 28 scopes, 13 of them aitest workers booking 28.0 GiB, all in one worktree. Beside them in
  that worktree are two job-level scopes, a 1.0 GiB `make test-engine` and a 2.0 GiB `uv run`. Rows
  carry no parent id. Only `--delegate-ram` launches receive worker-admit
  (`internal/runner/confine_linux.go:1233-1238`, `1255-1256`), which points to the `uv run` job as the
  parent. Its 2 GiB is above `y` in any case.
