VERDICT: The work that pays is the work done early: necessity checks and measure-first steps, plan review by two independent lineages, and the first build-review round. Round 3 and later reviews, plan gates that re-read the first reviewer's findings, Gemini, nits (P3) that nobody forwards, fix agents that run on empty or "reviewer unavailable" input, pre-push full `make ci`, peer release notices and memory-state rewrites are mostly dead weight. GitHub CI is the one gate with unique catches, and in practice it is ignored.

## 0. On the owner's question (Opus for the transcript analysis)
- Not measured: which model ran the six slice analysts. This synthesis step ran on Opus.
- The slice work was counting severities, matching findings to the commits that follow them, and grouping duplicates. It is judgement-light work against a fixed rubric.
- In-repo precedent for Sonnet doing this kind of work: S2's triage used 39 Sonnet agents. They cost 31.7M cache-read tokens (1.6% of that slice). All 39 verdicts were acted on, and the 20 close verdicts exactly matched the 20 tickets closed in PR #204 (reported by S2, not re-checked).
- Of the slice claims I checked (section 1), every one held. That is weak evidence that a stronger model was not needed for the extraction.
- Estimated answer: Sonnet with this rubric is sufficient for the slice analysts. Judgement-heavy synthesis is the only place a stronger model plausibly earns its cost.

## 1. Spot-checks I ran (all matched the analysts' reports)
1. PR #252 title is "...plan NOT approved; AIRA-350 held", merged 2026-11-18T14:54:55Z (gh). M
2. PR #211 has 26 commits, so the reviews changed no code (gh). M
3. AIRA-342: branch debf21c1 vs 4d935a0c = 17 files, +2079/-193. The merged PR #236 = 10 files, +1108/-76 (git diff, gh). M
4. The spec is unchanged after 976705ad: `git diff 976705ad a7484074 -- docs/superpowers/specs` is empty. The only commit between them is ticket hold 9b7aec46, so S6 run-1 round 3 changed nothing. M
5. AIRA-324.md line 8: the trial was rolled back on the 5% trigger, share [5.38%, 21.97%] over 446 discounted grants, no host harm. M
6. PR #219 merged with `test` and `race` red (gh pr checks 219). This confirms escape E6 and "CI is not acting as a gate". M
7. "timed out after 1500s" appears in 5 Nov workflow journals (17 string hits). This is consistent with S4's 6 timed-out Sol relays. M
8. d5075acf subject is "AIRA-328: collapse worker-claim cancel; time-based cancelled-scope retention". This is consistent with S4's collapse claim. M

Not re-verified: the analysts' delivered/waste classifications (their judgement, ±5 per slice), and token totals.

## 2. Aggregate yield (estimated; sums of the slice classifications)
About 1,450 review findings across 6 slices, Oct 8 to Nov 18. About 610 delivered (about 42%).

| Phase | Delivered rate | Source |
|---|---|---|
| Plan review, Oct | 78-88% | S1 33/42; S2 ~79/90 |
| Plan review, Nov spec loops | 31-47% | S4, S6 |
| First build-review round | 26-47% | S1 22/72; S2 26/100; S4/S5 first rounds higher |
| Round 2 | about 4-25% | S1 3/69; S2 ~40/160. Mostly fix residue, which is real but counted as waste under the rubric |
| Round 3 and later | about 0 across about 15 instances | see below |

Round 3+ instances with zero delivery: S1 284 r3 0/14 and 281 r2 0/8; S2 batch32 r3 0/8, flakes r4 0/7, gate-family r3 about 0, v0.34 Fable gates 0/7; S3 owner R2 and small R3; S4 327 r3, 328 r3, 340 r3, BATCH-A r2; S6 r3 in runs 1 and 2 (45 findings, no artefact change, M via git).

Exceptions, where round 3+ caught a regression introduced by the round-2 fix: goal-b R5.1, AIRA-228 Sol r3/r4 (racy tests written by the fix), goal-a's accept-and-discard family.

## 3. Stage-by-stage
Format per stage: cost; delivered / waste; unique catches and saves; verdict and the concrete change.

**Necessity, measure-first and challenge (incl. planner self-cuts and the quality vet)**
- Cost: AIRA-351 one Opus agent, about 15 min, 65k output tokens. Quality vet: 1 Opus agent.
- Delivered:
  - AIRA-351 NO-GO replaced a feature build.
  - The S3 vet rejected 32 of 98 cleanup candidates, including a deliberate runtime.KeepAlive on the lock path.
  - Planners cut AIRA-242, two AIRA-207 sub-items, AIRA-244, AIRA-175's production change and AIRA-296.
  - AIRA-350: the challenge reviewer's r1 showed the incident replay T1 is unreachable. The plan's own figures say the freeze explains about 5 of 115.6 wait-hours.
- Missed saves:
  - AIRA-283 part b: scout-aitest said "hold until measured". It was built, then reverted (c5e0da92, 6dcb93e4, 200 deletions; 7 of 15 plan-review findings spent on it).
  - AIRA-352: the planner itself found its acceptance criterion false (B9) and still recommended building it.
- Verdict: KEEP, MOVE EARLIER. Run a premise measurement with a pre-registered decision rule before any spec of more than about 100 lines. Make "the acceptance criterion is shown unmeetable" an immediate collapse trigger in round 1.

**Plan writing**
- Cost: Opus, 40-50 min, 370-455k output tokens per spec (S6).
- Waste: length growth. AIRA-323 spec went from 821 to 1,056 lines. AIRA-324 gate had about 2,580 replay lines that reviewers called non-evidence in 4 reviews, and they were never cut. AIRA-347 added +1,615 and ended as a DRAFT. PR #252 holds 2,538 doc lines for a held ticket. AIRA-350's own self-review fix C7 induced 4 of 14 round-1 findings.
- Verdict: TRIM.
  - Cap specs at about 450 lines (AIRA-353 already used a 450 target).
  - Strike any section a reviewer flags as non-evidence in round 1.

**Plan review: Sol (Codex)**
- Cost: Sonnet relay about 4-6k output tokens per review. Codex-side cost unmeasured.
- Delivered: the most unique P1s. Examples:
  - 284 BLOCK (4 P1)
  - batch memory bound and the PID-zero unreachable test
  - AIRA-175 premise false
  - 217 overclaim
  - AIRA-350 T22 witness (no lane can work)
  - AIRA-352 unmeetable criterion
  - In S6, Sol produced 17 of 28 P1s.
- Waste: repeats in round 3 (S6 contract-reversal P1 raised 3 times); owner questions restated.
- Verdict: KEEP for rounds 1-2.

**Plan review: Astra**
- Delivered: unique P1s on correctness-critical specs:
  - gate-family false-PASS window (subject-rule tag shipped as gate_subject.go)
  - goal-b positive-counter claim hole
- Verdict: KEEP only for ID allocation, receipts, leases and the honesty core, in place of Sol there.

**Plan review: Gemini**
- Delivered 0 accepted findings. It failed in wf_064f9161 twice and in 3 of 4 S3 plan reviews, and made 2 wrong claims when it did run.
- Verdict: CUT.

**Plan gate (Fable)**
- It delivers when it reviews independently and in parallel:
  - S2: 2-6 delivered per gate, mostly disjoint from Sol.
  - v0.34 gate P1: "reconcile refreshes the index" was false.
  - 281 gate: 7 delivered, 0 overlap with Sol.
  - 284 gate caught a false validation claim in the code.
- It is dead weight when it re-reads Sol's findings: S3 had 39 gate findings, 37 duplicates, and 1 delivery (the AIRA-314 fix-2 cut, a real save). S1 283 gate had 0 independent findings.
- Verdict: TRIM. Run it only as the second, independent lineage that does not see Sol's output. Drop the separate arbitration step and let the fixer arbitrate with evidence.

**Plan-fix rounds**
- Cost: Opus, 16-50 min, 186-418k output tokens each.
- Delivered: the collapses (AIRA-350, AIRA-352 cut, AIRA-353 cut to one timestamp, AIRA-323's 717 lines of fixtures deleted).
- Waste: fix-induced findings in the next round, as reported by the fixers: 4/14, 6/16 and 7/11 in S6. AIRA-323's golden table was net-reverted after 2 rounds. Fable had already said to cut the key columns as a P3 in round 1, and nobody acted until round 3.
- Verdict: TRIM. Cap at 2 rounds. When round 2's blockers are mostly fix-induced or in the same family, re-base or collapse instead of patching. Forward any "cut it" item to the fixer whatever its severity.

**Build (builder plus self-review plus embedded Sol)**
- Cost: Opus 41-142 min per build (S4). Sonnet builders on most Oct work.
- Delivered:
  - AIRA-309 self-review caught 3 bugs, including "push can kill the job".
  - Builder-embedded Sol in S5: about 64 of 73 delivered (88%), including the AIRA-271 P1 (a paste sends a kill) and the AIRA-324 gate P1s.
- Gap: correctness-critical v0.34 daemon-lock work was built by Sonnet against the memory rule. Its build review then found a stray-daemon P2 that survived one fix.
- Verdict: KEEP. The embedded Sol round is the cheapest high-yield review in the data.

**Build review, round 1: Opus/Fable lenses (2-4 per round)**
- S1 284: all 4 lenses had unique P1/P2 catches (symlink delete outside the managed directory; false duplicate:true; 262-397 ms stall; cwd mismatch).
- Low-yield lenses:
  - batch install-daemon delivered 0 in round 1
  - three S1 docs-audit lenses delivered 4 of 37 findings
  - S3 three refactor reviewers changed nothing; only the behaviour lens carries assurance value
- Fable's unique contribution is porous-test catches by mutation. It also emits 272 P3s in Nov, 77% of its output, none forwarded.
- Verdict: KEEP 2 lenses (semantic/protocol plus test-porosity), not 3-4. Cap P3s at about 3 per review, or forward them to the round-1 fixer.

**Build review, round 2**
- Real value: catching regressions introduced by fix 1. Examples: 284 "status ok on deleted subdir", goal-b R4.1/R4.2, the v0.34 incomplete stray-daemon fix, the AIRA-323 commondir sibling.
- Waste: 3-4 lenses all reporting the same regression (284: found 4 times). Overhead analyst: Oct re-review r2 cost 326M cache-read tokens for 7 blocking findings.
- Verdict: TRIM to one reviewer reading only the fix diff.

**Build review, round 3 and later**
- About 0 delivered across about 15 instances (section 2). Their only use was triggering collapses that round 2 could already have triggered.
- Verdict: CUT. Replace with a non-convergence decision at the end of round 2.

**External workflow review after an embedded Sol APPROVE on the same head**
- 0 changes in AIRA-336, the 338 collapse, the 228 finish and AIRA-341 (merged head equals reviewed head).
- Verdict: CUT.

**Work-fix agents**
- Delivered: every fix-commit chain cited in the slices.
- Waste:
  - S3: 2 owner fix rounds got an empty findings list and still ran a full `make ci`.
  - S4: 3 Opus fixers ran on "reviewer unavailable", about 105 min.
  - S5: 2 no-op fixers.
  - S6 run 3: the unavailable placeholder was handed over as one of 9 blocking findings.
- Verdict: fix the harness. Do not spawn a fixer when there are 0 real P1/P2 findings. A timeout or OOM is retried once, and is never a finding.

**Sol infrastructure**
- 6 of 26 relays timed out in S4 and 10 of 71 Nov-wide, at up to 1500 s each. The timeout counts admission-queue time. Reviewers were OOM-killed at the 1 GiB auto reserve in S5 (3 times) and S6 (1 time).
- Verdict: fix the harness. Measure the timeout from admission, give the relay a pinned reserve based on its measured peak, and retry once.

**`make ci` pre-push**
- Cost: median about 10.9 min (8 captured runs, M). About 590 pushes; up to about 95 h of gate time (E).
- Delivered: 0 real defects among at least 11 narrated refusals. That count is a lower bound, because output was piped through tail or -q. The builder had already passed the same tree.
- Waste: OOM-killed at the 1 GiB default 3 times; pylib timeouts added about 2.5 h to the S3 merges.
- Verdict: TRIM. Skip it when a recorded `make ci` passed on the same tree hash (aira already writes tree_hash in `--summary-file`). Keep the docs-only fast path.

**GitHub CI (incl. race)**
- 94 failed runs: 74 flakes (79%), 33 of them one VRAM test over 41 days (E7). 20 real runs in 7 incidents, CI-only catches:
  - gofmt (E3)
  - master red for 6.1 days after PR #219 (E6)
  - git-version fixture assumption on PR #237
  - 1 data race on PR #88
- 30 of 250 merged PRs (12%) were merged red; 0 failed runs were re-run.
- Verdict: KEEP AND ENFORCE.
  - Make green CI a merge precondition in the "merge on approval" rule.
  - Allow one automatic re-run, then quarantine a test that fails twice, filing a ticket.
  - Race stays.

**PR/merge mechanics**
- Escapes:
  - E1: a stale squash dropped P0 fixes.
  - PR #247 merged a head that differed from the reviewed head.
  - PR #248's conflict resolution dropped a skill paragraph (caught by the gate, not review).
- Verdict: KEEP, and add a merge-integrity check: the merged tree equals the reviewed head tree plus a clean merge of master.

**Release ceremony and peer notification**
- 11 Oct releases at about 4 turns each. Release plus PR tool time was 4.5 of 8.1 active hours (Oct).
- 33 of 36 peer notices got no reply within 6 h; v0.39 was sent twice.
- Caveat: peers may have acted without replying.
- Verdict: TRIM. Notify only on proto, CLI or skill changes that affect a named peer, one message per batch.

**Memory updates**
- aira-v41-v42-state.md was rewritten 13 times in 5 days and never re-read except to edit it. Memory is about 3% of turns.
- Verdict: TRIM. Write at session seams only.

**Operator monitoring (AIRA-324 trial cron)**
- 6 cron fires plus 49 turns. The cron lapsed at the usage limit, so the latch was read about 3.5 h late.
- Verdict: MOVE to a daemon-side notice or latch file that announces itself (wait on notices, not polls).

**Coordinator briefing and waiting**
- Briefs: 3.9% of turns but 19.4% of output tokens; plan-fix prompts 31-35k characters.
- Wait/poll: 25% of active time.
- The coordinator is 2-9% of cache-read cost. The pipeline is the cost.
- Verdict: TRIM brief length. Otherwise this is needed overhead.

**Triage and rant verification (S2, S3, S4)**
- S2: 39 Sonnet agents closed 20 tickets.
- S3: 5 Opus verifiers, 15 of 15 rants real.
- S4: the critic delivered 12 of 12 findings in 3.5 min and changed 323's scope.
- Verdict: KEEP. Use Sonnet for verification.

**Scouts and recon (S1)**
- Their risks were re-found later as plan P1s; their "hold" advice was ignored.
- Verdict: TRIM to 1 scout. A scout's "hold until measured" becomes a gate question.

## 4. Escape ledger (what got past all gates; from the escapes analyst, M unless marked)
| Id | What escaped | Why |
|---|---|---|
| E1 | PR #35 stale squash dropped P0 fixes | No merge-integrity check |
| E2 | Flaky test shipped (7.5% fail rate) | Its porosity was already in the builder's own record |
| E3 | gofmt violation; 4 PRs merged red | CI ignored |
| E4 | Board navigation keys desynced selection | Found by owner dogfood after Fable reviews |
| E5 | Wrong SKILL `AIRA_CONFINE_OWNER` steer | Needed real shell semantics to see; 2 hotfix tags |
| E6 | AIRA-313: master red for 6.1 days | CI-only, merged red |
| E7 | VRAM flake | 33 red runs over 41 days |
| E8 | Ambient-daemon test dependence at the proto-12 cutover | Environment-dependent |
| E9 | Peer- or rant-found P0/P1 bugs (AIRA-128, 201, 220, 222) | Origin PRs not traced |
| E13 | AIRA-283(b) built, then reverted | A necessity question skipped |
| E14 | AIRA-324 enforce trial rolled back by its own trigger | Designed catch, no harm |

Pattern: no escape is attributable to a missing review round. They come from:
- red CI being ignored (E3, E6, E7)
- merge integrity (E1)
- necessity not asked (E13)
- dogfood-only UX or semantics (E4, E5)

## 5. Five highest-value process changes, ranked
1. **Measure-first necessity gate before any spec.** One agent, a pre-registered decision rule, about 15 min. Collapse at round 1 when the acceptance criterion is shown unmeetable. Evidence: AIRA-351, AIRA-350 (5/115.6), AIRA-352, AIRA-283(b), AIRA-342 (-56% after 3 rounds). This is the biggest saving per unit of cost.
2. **Cap reviews at 2 rounds.** Round 2 is one reviewer on the fix diff only. At the end of round 2, either merge or re-base/collapse. Evidence: about 0 delivered in round 3+ across about 15 instances, at roughly 20-66M cache-read tokens per round (S2 measured). Round 2 keeps its regression-catching value.
3. **Fix the review harness.**
   - Timeout and OOM are retried once and never become findings.
   - Sol's timeout excludes admission; the relay gets a sized reserve.
   - No fixer runs with 0 real P1/P2.
   - No external review after an embedded approve on the same head.
   - Evidence: about 105 Opus min plus 2 S5 no-ops plus 2 S3 empty `make ci` runs, and 10 of 71 Sol runs lost.
4. **Make CI a real merge gate and drop the redundant pre-push gate.** Green CI is required, with one auto re-run and flake quarantine, plus a merge-integrity check. The pre-push gate is skipped when the same tree hash passed. Evidence: 30/250 red merges, E1/E3/E6/E7, 0 real pre-push catches, about 11 min per push.
5. **Two independent plan reviewers, fewer lenses, P3 discipline.** Plan review = Sol (Astra for correctness-critical) plus one Claude lineage that does not see Sol's output. Drop Gemini and the duplicating gate. Build round 1 = 2 lenses. P3s capped, or forwarded in round 1. "Cut it" items are always forwarded. Evidence: S3 gate 1/39; Gemini 0; 272 unforwarded Fable P3s; the AIRA-323 cut was raised in round 1 and acted on in round 3.

Lower-ranked, cheap changes: peer notices only for peer-affecting changes; memory written at seams; daemon-side trial alerts; spec cap of about 450 lines.

## 6. First-principles minimum process per change class
- **Docs-only.** Author, one cold-reader review that executes any command the text prescribes (E5 was a prose instruction that failed when run), docs-only push fast path, green CI, merge. No plan review. Evidence: in S1, 3 of 6 audit lenses gave 4 of 37 deliveries.
- **Small mechanical (refactor, deletion, small-fix batch).** Candidate list, a necessity/behaviour vet, then a build with self-review and `make ci`. Then one review round: behaviour-equivalence lens plus mutation spot-check. Fix if P1/P2, then green CI and merge. Evidence: PR #211 (vet was the save), AIRA-308 (1 round, 9 of 14 delivered).
- **Evaluator / admission / ID / lease correctness-critical.**
  1. Premise measurement.
  2. Spec of at most about 450 lines.
  3. Plan review by Astra or Sol plus one independent Claude lineage, at most 2 rounds.
  4. Opus build with embedded mutation self-review and an embedded Sol round.
  5. Round-1 build review: semantic lens plus porosity lens.
  6. Fix.
  7. Round 2: one reviewer on the fix diff.
  8. At blockers, a re-base/collapse decision.
  9. Green CI incl. race, merge-integrity check, staged observe-then-enforce with daemon-side triggers.

  Evidence: goal-b, gate-family and AIRA-328 saves all came in plan review or rounds 1-2. AIRA-324's trial was the final catch.
- **Measurement-only.** Pre-registered decision rule, one measuring agent, one method review in a single round, then a record merged with the verdict. Evidence: AIRA-351 (1 agent) vs the AIRA-324 gate (7 rounds, about 2,580 replay lines called non-evidence 4 times, and the trial was owner-waived either way).

## 7. Weak evidence and sample sizes
- About 60 workflow runs. Coverage is Oct 8 to Nov 18 only; Sept build reviews appear only in the escapes data.
- Delivered/waste is analyst judgement (±5 per slice). P3 "unacted" was often not independently diffed.
- Fix-induced findings counted as waste are real catches. That undercounts round 2's value, which is why I trim round 2 rather than cut it.
- Saves are counterfactual and cannot be proven.
- Codex/OpenAI token cost was not measured anywhere. Oct output tokens are under-recorded.
- The pre-push refusal count is a lower bound.
- The 5/115.6 freeze share is an upper-bound association, not causal.
- S6 run 3 (wf_1f68ea1a) was still in progress at audit time.
- Peer silence does not prove peers took no action.
- The duplicate-detection heuristic was about 60% precise (overhead analyst).