# Small-job lane (AIRA-350): appendix, history and superseded designs

Companion to `2026-11-18-fixed-small-lane-design.md` (round 6). Nothing here is part of the
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
| Round 4 | Priority and a cap of `x` inside the one ceiling, guard G | Review: 3 P1, 12 P2 (§A7). |
| Round 5 | Round 4 with its review fixed; the pre-pass skipped while a queued sub-reservation fits | Review: 7 P2, no P1 (§A9). |
| Round 6 | Round 5 with its review fixed: guarantee premise and R11, R10, test oracles, yield range | This spec. |

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

## A7. Round 4 review (15 findings: 3 P1, 12 P2) and round 5's disposition

Every finding was checked against the code at `45f68afd` and the AIRA-354 spec at `e9077760`;
every one held, none is rejected. Two pairs were duplicates (T3, T13). None was fix-induced:
round 4 was the first review of the owner's design. Grouped by root cause:

| Family | Findings | Root cause | Disposition (round 5) |
|---|---|---|---|
| P, the price of priority understated | P1 R1 (the head advances only on main releases; the minutes and the `x` bound wrong, headroom outside `x`); P2 a starved suite worker; P2 a `confine-reserve` hold timing out behind lane grants; P2 G guards only the reference | Round 4 priced priority with the all-jobs release rate and guarded one waiter | R1 restated without minutes; the take is `x` plus 64 MiB per small job (I3); the pre-pass is skipped in any pass where a queued sub-reservation fits (new, one conjunct; an in-loop stop was drafted first and missed a worker queued behind a small waiter, T26c) and R6 states the rest; R2 with T24 and a veto form |
| G, model | P1 the VRAM term used `total` for `free`, but the fit charges a running job twice (`vram.go:53-63`, `191`) | Copied from head eligibility, where occupancy is zero | VRAM term `total - occ.vram`; T6 runs through release, ramp-up and a fresh snapshot and fails against round 4's form; R4 states undeclared use |
| C, restart paths | P2 x 2: a SET keeps a discounted charge (`admit.go:2326`) | Establish and SET conflated | §2.4 has two rows; I2, I6, R7 and T13 split |
| T, test oracles | P1 + P2 T3 contradicted the pre-pass; P2 physical fit and 354 untested; P2 the reference's derivation untested | Rows written against the intent, not the pseudo-code | T3 rewritten with passes; T23 (physical witness, room-parity contract); T25 (four reference cases); T26 (three sub-reservation cases) |
| W, wording | P2 generated text promised a total cap; P2 I1 omitted a falling ceiling (`sliceceiling.go:531`); P2 the 354 row was stale; P2 yield rows read as cause | Claims not checked against the code or the sibling spec | §2.7 rewritten with keyword checks; I1 at grant time and T2c; the 354 row against its spec; §7 names the supported rows (0-30%); replay label fixed |

**Replay re-run** (`aira350-r5-replay`, `--memory-reserve 256M`, exit 0, no `hint:` line; peak-rss
equalled the pin again, page cache of the store copy): only output line 91 changed, the hold-overlap
label, now "94.2 h of their 107.9 h (87.3%); 73.5% of all 128.2 small wait-hours".

**Non-convergence check.** Round 5 of the fixed-lane line, the first fix round for the owner's
design. Three P1s again, but none shows the decided design unsound: the one ceiling, the cap and
the host bound all hold. They show its price larger than round 4 stated. R1 is put to the owner as
a residual with its witness, not patched.

## A8. Material moved out of the spec in round 5

- **Data** (not committed: they name other sessions' jobs): `~/tmp/aira-lane2-store/` (store copy
  and `confine --list` snapshots of 2026-11-18 21:12 and 21:20 EET, journal from 11-15 00:00) and
  `~/tmp/aira-350/live-snapshot-1355.json`.
- **`--pin-charge` sites the install flags follow:** `internal/install/install.go:154-158`,
  `218-219`, `471-492`, `552`, `640-650`, `695-706`, `890-895`, `1063`, `1140-1143`, `1632-1670`,
  `2507-2508`; `cmd/aira/main.go:1039`; `internal/install/mode.go:321` (CI renders `off`); usage
  `internal/core/core.go:2241`.
- **Yield biases in full.** Upward: no ceiling-room gate (a hold proves room for one withheld job,
  not for all the replay admits); no CPU gate in the first two rows; the 20-row retention cap thins
  small jobs (186 signatures at the cap, M), so lane occupancy is understated; servers absent
  (replay 1b); suite parents keep their historical waits, so the wait the lane adds to them (R10)
  is not netted out. G never bound in RAM, so leaving it out biases nothing. Downward: in the
  hold-gated rows, small jobs taking freed room before earlier larger waiters outside holds; small
  jobs moved to the lane leave room for others. CPU rows: replay 1e's slot model (5 retained jobs
  ran at the incident, M; 32 reported, R); `x = 8` buys 9-10 points over 4 without a CPU limit, 4 at 24 unseen
  slots, 0 at 27 (E). Why the hold-gated row: a hold is the one state in which today's evaluator
  withholds a job that fits (`admit.go:2762-2769`); outside holds the lane only reorders who gets
  freed room. The journal records no bookings or room at grant, so no row gates on ceiling room.
- **Round 4's R1 minutes, withdrawn:** 0.6, 1.4 and 3.2 min per head, from the window's all-jobs
  release rate (about 2.7 GB/min, round-3 query part 4). They assumed small-job turnover reaches the
  head, which the pre-pass prevents while small demand lasts. Round 4's acceptance threshold
  ("large-job wait rises by at most 1.4 min, R1's p90") went with them.

## A9. Round 5 review (7 findings, all P2) and round 6's disposition

Every finding was checked against the code at `45f68afd` (the worktree's `internal/` and `cmd/`
are identical to it) and against the replay; every one held, none is rejected. Four were
fix-induced, written in round 5 (`e663dd4a`): T24's fixture, T7a's extra pass, the 0-30% yield
claim and R6's containment ceiling. T6's unspecified budget dates from round 4 and survived round
5's rewrite. The guarantee's missing premise and R10 date from round 4. By root cause:

| Family | Findings | Root cause | Disposition (round 6) |
|---|---|---|---|
| G, guarantee premises | The guarantee omitted the RAM physical floor. Witness: `E` 45056, head 40960, small pins 1792 at equality with `H(4)` 2304, real use 2048 (256 in no lease), room 40704 | §2.3 argued on the ledger and pins, but today's fit charges `max(effectiveCurrent, outstanding)` (`admit.go:2906-2921`) | Premise added to §2.3; R11 with the operator's view (`slice_current_bytes`, `slice_reclaimable_bytes`, `confine_manage.go:758-759`); T4e |
| T, test oracles | T6 and T24 pass 3 ignored the main fallback: a fresh budget grants the `kept` waiter in main (`admit.go:2688-2697`). T24's fixture gave room 40832, not 10000. T7a added a pass: once a pushed head is granted no head is left, so the main walk grants fitting small waiters in the same pass | Rows hand-computed against the intent, not the pseudo-code (the same family as round 4's T) | The seam defines "budget spent" (`bypassed` at budget, hold phase, no backstop), and T6 uses it. T24 gains a 30768 main lease and asserts the fallback grant (`bypassed` 512). T7a and §2.2 assert the same-pass main grant. Three new mutation entries |
| Y, yield | 0-30% conflated occupied slots with the replay's window-wide unseen-slot allowance (`aira-350-fixed-lane-replay.py:266-268`) | An instantaneous count read as a window-wide parameter | §7 states a 0-84% range with conditional rows and picks none. The 24-slot row (29 occupied beside the incident's 5 retained jobs) gives 65-69% |
| W, wording | R6 cited the retired AIRA-15 containment ceiling | A stale code comment (`admit.go:1189-1192`) was trusted over `protocol.go:133-136` and `confine_linux.go:1121-1137` | R6 says the parent's cap is its pin, at or above its booking (discount, resize, `confine_resize.go:124-127`). The stale comment is left for the build to correct |
| P, price of priority | Missed residual: small jobs the lane does not serve are passed by every later eligible job | Excluding parents was costed only for their workers | R10 with its witness and a veto; a §7 bias; §8 counts parents in "small" and reports them alone (base about 81 s per job: 20.4 h over 903 rows) |

**Re-derived here:** T24: `45056 - H(3) - (2048 + 30768) = 10000`; eight grants leave `10000 - 8 x
1088 = 1296`; pass 2 `1296 + 2112 = 3408`, A's grant back to 1296; pass 3 `1296 + 1088 = 2384`;
G against B `45056 - H(9) - 7680 = 34752`; the veto form stops at 5 (`45056 - H(7) - 6144 = 36416
< 36864`). T6 from `vramAdmitAvailable` (`vram.go:53-63`): pass 1 G 16384 then 12288, pass 2 head
fit `min(21504, 19456 - 1024) - 2048 = 16384`. T4e: `45056 - H(4) - max(2048, 1792) = 40704`.
Acceptance bases: `128.2 h / 9281 = 49.7 s`, `83.7 h / 2660 = 113.3 s` (replay §0). No replay was
re-run; every figure in §7 is from the committed `.out.txt`.

**Non-convergence check.** Round 6 is the second fix round of the owner's design. Severity fell
(round 4: 3 P1; round 5: none), and no finding shows the design unsound: the one ceiling, the cap,
the host bound and G hold; the findings are a missing premise, two missed or misstated residuals,
a misread replay parameter and oracle arithmetic. Family T has now recurred in two reviews: oracle
rows computed by hand in prose. The change that removes it is to compute them by running the seam,
which is the build's first, RED-first step; another prose round cannot. Recommendation: put the
residuals (§4) and the build order to the plan gate and the owner now, and let the build's seam
tests settle oracle arithmetic, with any oracle the build finds wrong corrected in the spec in the
same PR.
