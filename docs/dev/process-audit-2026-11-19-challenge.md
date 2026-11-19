VERDICT: Two of the five most consequential verdicts are OVERTURNED (cut round 3+; cut external review after an embedded Sol approve), two are WEAKENED (enforce CI / drop pre-push; Gemini), one is CONFIRMED with caveats (measure-first). The top-five list changes.

## Owner's question (Opus for the transcript analysis) — measured from the journals
- The first audit run (`wf_f6805461`, 21:34Z) started all 8 analysts on Opus 5.5. None has a result record; it was abandoned after about 2 minutes (about 21M cache-read, 85k output tokens spent).
- The re-run (`wf_92cd08b0`, 21:36Z) ran the same 8 roles on Sonnet 5.5: 118.5M cache-read, 1.18M output tokens. Synthesis ran on Opus (6 turns, 31k output). So the synthesis's "not measured" is answerable: Sonnet did the extraction.
- The synthesis's 8 spot-checks were all lookups (PR title, commit count, a string). The errors found below are in the delivered/waste classifications, which it did not re-check. "Every claim I checked held" therefore says nothing about classification quality.
- There is no Opus comparison (that run was abandoned), so nothing shows Opus would have classified better. The main error is in the rubric, not the model: delivery was counted per workflow, and round-3 findings are fixed outside the workflow (see 1).

## The five verdicts

**1. Build/plan review round 3+ "CUT, about 0 delivered across about 15 instances" — OVERTURNED (November); October unchecked.**
- Mechanism the synthesis missed: the workflow script ends at `review r3` with no fix step (every November journal: build, r1, fix, r2, fix, r3, end). Round 3 can never deliver inside its own workflow. Its findings are fixed by the coordinator or a follow-up workflow (`-FINISH`, `-COLLAPSE`, `-R4`), which the analysts counted as a fresh build.
- Round 3 runs only when round 2 still had blockers (330 and BATCH-A stopped at round 2), so it is the only review of fix 2's code.
- November tally from the journal verdicts joined to the next commit: 17 round-3 instances, 3 approved (327, 323-FINISH, 339), 14 had P1/P2. At least 10 of the 14 were followed within the hour by a commit matching the findings:
  - AIRA-328 r3 (listed by the synthesis as zero): Sol found "cancelled jobs can run after cancellation-record eviction" and "a lost cancel reply still permits a granted retry". `d5075acf` (23 minutes after r3 ended) fixes both by name. Both bugs were in code fix 2 had added (`30fe7f48`, `05d93379`); a 2-round cap would have merged them.
  - AIRA-324-ENFORCE-GATE r3: 4 P2s, including "unknown (NULL) peaks become known-safe zero samples" and a false-pass in the operator read command; fixed in `3fc11f28`. This was the safety evidence for the enforce trial.
  - AIRA-228 r3: Sol showed by mutation that a test passes with the required wake-up removed; fixed in `88ddb358`.
  - 323-SPEC r3 led to collapse `78e7f813`; TOP-EDIT-UX r3 to collapse `99664a3f`; 338 r3 to collapse `03260217`; 340 r3 to `b4c1bb41`; 271 r3 to `3997292c` (test tightened, override recorded); 350 r3 to hold `9b7aec46`; 352/353 r3 to hold `12ca2626` ("8 blocking at round 3 ... consider dropping it").
  - Not delivered: PRIORITY-ENV r3 (2 P2s, no commit between `0c0764c1` and merge #247, nothing in the ticket), 323-RESUME r3.
- Confirmed part: S6 round 3 changed no spec text. `976705ad`, `398d2b3d` and `da836e9d` all landed before their workflow's r3 started; only ticket holds followed. Commit messages number rounds cumulatively (embedded plus workflow), so "round 4" in a commit is workflow fix 2.
- Real trim available: on code, Fable's r3 produced no blocking code finding in 5 of 5 builds (328, 323-RESUME, 338, 228 approved with nits; 340 a record-keeping P2). Sol carried every r3 code catch. Fable's r3 did deliver on specs and the measurement gate.
- October instances (S1, S2, S3) were not re-derived.

**2. External workflow review after an embedded Sol APPROVE "CUT, 0 changes" — OVERTURNED.**
The four cited cases are a selected subset: two are follow-up passes after a full external loop (228-FINISH, 338-COLLAPSE) and two are small (336, 341). Cases where the builder's embedded Sol had approved and the external round 1 then blocked, with the defect present at the approved head:
- AIRA-324 (embedded Sol r2 APPROVED, head `7345134b`): external Sol BLOCK, "inherited environment pins receive the discount". Fixed in `54ee34cd` by a one-line change to the eligibility predicate that existed at the approved head. Fable added 2 P2s for untested physical-fit terms (3 surviving mutations).
- AIRA-339 (embedded r2 APPROVE, head `420b4dd3`, comments only after approval): external r1 P2 on the "the job has ended" claim (`346a029e`), then r2 found the post-kill fsync bug the coordinator called "a genuine bug" (`f8ba6a17`).
- AIRA-324-ENFORCE-GATE-FINISH (embedded APPROVE at `796cba7b`, then amended): external Sol REVISE, 3 P2 (`fb2c5a1f`). Head differed by the P3 amendments.
- PRIORITY-ENV (embedded APPROVE): external r1 timed out, r2 found 3 P2s (`0c0764c1`).
- AIRA-228 (embedded verdict not stated as approve): external r1 found a stale-clock fairness bug in the evaluator and that the new guard made both real-cgroup restart gates skip silently (`f7684e11`).
Defensible narrower rule: skip the external round only for a FINISH or COLLAPSE pass on a change that already had a full external loop. Side finding: the same model at the same effort approved and then blocked the same head, so a single Sol round is not a stable gate.

**3. GitHub CI "KEEP AND ENFORCE; ignored in practice" plus pre-push "TRIM; 0 real defects" — WEAKENED.**
- I count 34 red merged PRs (not 30) out of 250. In 30 of the 34 the failing check finished after the merge: the merge did not wait for CI. Merged before CI finished: September 77 of 177, October 38 of 42, November 0 of 31. Red merges in November: 0. So "ignored" is stale; the owner's 2026-10-24 merge rule already fixed it.
- What remains: master has no branch protection (API returns 404), so enforcement rests on agent behaviour.
- "Master red 6.1 days" is confirmed (2026-10-25 19:07Z to 10-31 23:48Z), but it started before #219 merged and git has no commits from 10-27 to 10-30. Active exposure was about a day.
- "0 real defects" pre-push is false. On 2026-11-16 02:02Z the gate failed `TestConfinePriorityGuidanceIsOnEveryFace` on the AIRA-324 merge head because conflict resolution deleted the `--priority` guidance from the shipped skill. The synthesis lists this itself under PR #248. No review saw that head.
- The tree-hash skip survives (a merge is a new tree). "Drop the redundant pre-push gate" does not: in October it was the only gate that blocked, and it is the existing merge-integrity check.
- Coverage is asymmetric. `ci.yml` says real-cgroup and real-pytest tests skip on the runner (170 call sites in 33 test files), and `claude-mod-check` reports UNEVALUATED there. Race runs only in GitHub CI. That the real-cgroup tests run locally is inferred from `f7684e11`, not re-run.
- "About 590 pushes" not reproduced: 137 foreground push commands by my regex; background pushes not counted.

**4. Gemini "CUT, 0 accepted findings" — WEAKENED (the cut as a routine reviewer holds, the stated reason does not).**
- 12 calls in all journals: 9 failed with `agentmux ask failed (exit 3/4)`, 3 returned. All 9 failures predate the 2026-10-28 move off the free tier; cause not verified.
- Of the 3 that returned: one rejected with a reason (AIRA-284); one raised the "clean peak equal to OOM peak drops the floor" counterexample on the own-peak-floor spec (whether Sol found the same was not checked); one was the independent check the non-convergence rule requires, called by plan-fix r2 on AIRA-350. It reached the same verdict and is recorded in the spec (lines 93-105, 1408); its strict-FIFO suggestion was declined.
- n = 3 is too small for a quality verdict. Cost is negligible either way; this does not belong in a top five.

**5. Measure-first necessity gate (ranked 1) — CONFIRMED, small sample.**
- AIRA-351: one Opus agent, 15 minutes, 65k output tokens (measured). The decision rule was written at 15:53:18Z, 2 minutes before the first measurement run, after three table-level count queries; the rule states this itself. It was written by the measuring agent and no reviewer checked the method or the NO-GO.
- The AIRA-283(b) reverts exist (`c5e0da92`, `6dcb93e4`, 200 deletions).
- Evidence is n = 1 clean case plus three post-hoc ones. The AIRA-350 and 352 saves were found by plan-review reviewers, so they are also evidence for plan review.

## What the synthesis missed
- Coverage: journals exist only for 2026-10-08..14, 10-24..26, 11-01 and 11-14..19. 177 of 250 merged PRs (71%) predate any journal. Yields come from October-November; most of the escape ledger (E1, PR #88, AIRA-128/201/220/222) comes from September under a different process.
- Right-censoring: the November work is 1 to 5 days old. "No escape is attributable to a missing review round" cannot yet be judged for it.
- Survivorship inside the windows is low: 59 workflows, 527 agents started, 510 results, 2 failed. Abandoned Opus builds are uncounted cost (`wf_18d1413f` AIRA-328, `wf_3b60d783` AIRA-323, `wf_b724b7dd`), as is the abandoned Opus audit run.
- Round-3 findings have no owner once the workflow ends. That is how PRIORITY-ENV's two P2s merged with no fix and no recorded override.
- Reviewer-unavailable placeholders reached fixers as findings in 328 r1, 340 r3 and PRIORITY-ENV r1 (confirms the harness finding).
- Not checked by me: Fable plan-gate duplication (S3 1 of 39), P3 counts, token totals, peer-notice and memory figures, all October round-3 instances.

## Corrected top five
1. Measure-first necessity gate before any spec, with the decision rule written before any query and one method review of the result. (Unchanged in substance.)
2. Keep round 3 as verification of fix 2, and give it an owner. Replaces "cap at 2 rounds". On code, round 3 is Sol alone on the fix diff; Fable stays for specs and measurement gates. Every round-3 P1/P2 ends as a fix commit, a recorded override or a collapse; the non-convergence decision is taken on round 3's output.
3. Fix the review harness: timeouts and OOMs are retried once and never become findings; Sol's timeout excludes admission; the relay gets a sized reserve; no fixer on zero real findings. (Unchanged, minus "no external review after an embedded approve".)
4. Make CI mechanical and keep the pre-push gate: add branch protection requiring the three checks on the merged head, quarantine tests that fail twice, skip pre-push only on a recorded pass of the same tree hash.
5. Keep the external round after an embedded Sol approve for admission, kill, lease and evaluator changes; skip it only for FINISH/COLLAPSE passes. Two independent plan reviewers and P3 discipline stay as proposed (unchecked). Gemini moves to the lower-ranked list: optional independent check at a non-convergence decision.

Scratch files: /home/mark/tmp/aira-audit-prs.json, /home/mark/tmp/aira-audit-prs2.json, /home/mark/tmp/aira-audit-runs.json, /home/mark/tmp/aira-audit-final.py