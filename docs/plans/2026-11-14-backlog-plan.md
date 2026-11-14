# aira backlog plan, 2026-11-14 (revision 2)

> **Ticket IDs for the plan's NEW items (filed 2026-11-14 in the same PR):** hint-honesty = AIRA-334, foreign-env-refusal = AIRA-335, budget-bounded = AIRA-336, runner-flakes = AIRA-337 (all unheld, v0.42); lease-remedy = AIRA-338 (held, v0.43); kill-attribution = AIRA-339 (held, v0.44). Nothing else in this plan has been actioned yet: retirements, milestone relabels, holds, tag v0.41 and un-holds await the owner's approval.

Master is 692cb252. The live client and daemon both report that revision (`aira version`, diverged=false). The last tag is v0.40 (2026-10-26). PRs #221 to #229 have merged since then without a tag. They cover AIRA-322, 327, 328 and 330, the AIRA-213 decision, the history-charge plan and the AIRA-329 diagnosis.

Inputs read: `aira ready` (11 rows); `aira list` (4 in-review, 34 planned); every open ticket file; the history-charge spec rev 2 (`docs/superpowers/specs/2026-11-13-history-charge-design.md`); the AIRA-329 diagnosis; the dev-loop and review policies; the memory notes; and the four rant investigations. `aira rant ls` lists 19 unreviewed rants: RANT-56 to 60, 63, 68, 73, 74, 76, 87, 102, 105, 108, 109, 111, 119, 130 and 133. The investigators covered exactly that set. I opened every file:line cited below on master.

## Rant outcomes (summary)

| Rants | Outcome | Goes to |
|---|---|---|
| 73, 87, 111, 68, 108, 109 | Real. The trailer advice is wrong for jobs heavy on page cache and for jobs that escaped their scope | NEW **hint-honesty**. The estimator side stays on AIRA-318, 273 and 225 |
| 74, 102 | Real. aira's own `--kill` shows up as "unattributed SIGKILL" or "unconfirmed" | NEW **kill-attribution**, wording only (see A) |
| 63, 76 | Real but minor. `--kill` on a dead, empty scope says "retry", and a retry can never work. The reaper removed both scopes within about 2 minutes | Widen AIRA-319 and merge it into kill-attribution |
| 59 | Fixed by AIRA-313 (v0.40) | Mark reviewed; no ticket |
| 60 | Already covered | Append to AIRA-279 |
| 119, 130 | Real. An overridden HOME or XDG_STATE_HOME produces a wrong "unit absent" message or an endless silent wait. I reproduced both | NEW **foreign-env-refusal** |
| 105 | Real, and worse than reported. `--budget` prints 100.5 MB on one line (180,393 rows) and the client needs about 1 GB of memory. The 100.5 MB figure is the investigator's; `~/tmp/aira-budget.json` is 99.7 MB, which is consistent | NEW **budget-bounded** |
| 56 | Real. A bare E_LEASE_EXPIRED offers no remedy | NEW **lease-remedy** |
| 57, 58 | Flakes in tests only. 57 failed 2 of 20 runs; 58 was found by reading the code | NEW **runner-flakes** |
| 133 | Not a leak. Directories share the signature `./runall.sh` | Evidence for AIRA-321 and decision B#2 |

## A. Inventory of open items

Surface: C means the client binary only, D means a daemon restart, W means the client-daemon wire changes. Size: S is under a day of agent work, M is 1 to 3 days, L is more. "Crit" means correctness-critical, which needs its own two-loop review. Hold shows the value in the ticket file today. "Target" is the milestone this plan assigns.

| ID | Short title | Sev | Hold now | Target | Surface | Crit | Size | Depends on | Still needs building? |
|---|---|---|---|---|---|---|---|---|---|
| 307, 308, 309, 310 | queue bypass, queued rows in top, --front, reserve hint | P2 | no | — | — | — | — | — | **No.** Merged in v0.37 to v0.39 (PRs #212, #213, #214) but still marked in-review. Move to done |
| 311 | short wait line, ACTION REQUIRED hint | P2 | no | — | — | — | — | — | **No.** Merged in PR #215 (v0.40). Move to done |
| 312 | --resize | P2 | no | — | — | — | — | — | **No.** Merged in PR #216 (v0.40). Move to done (acceptance list not re-checked) |
| 327 | rant ls default | P2 | no | v0.41 | — | — | — | — | **No.** Merged in PR #225. Move to done |
| 305 | VRAM test flake | P2 | yes | — | — | — | — | — | **No.** Fixed by 05dbc38c (AIRA-322): the test now samples at `admitWriteFrame`. Close |
| NEW hint-honesty | delete the non-OOM "reached N% of cap" line, skip the hint on an escaped scope, say "this run only" | P2 | — | v0.42 | C | no | S | — | **Yes.** Reproduced: a process using 2 MB read as 100% of a 256M cap |
| NEW foreign-env-refusal | name the cause when HOME or XDG_STATE_HOME point elsewhere | P2 | — | v0.42 | C | no | S | — | **Yes.** It stalls fastest-ee jobs. Extend `unreachableDaemonError` (cmd/aira/dispatcher.go:961) so it refuses only when no daemon answers (`status.Ready` false). The override alone never triggers a refusal. A false-refusal test is required: overridden HOME or XDG_STATE_HOME with a reachable daemon must still run. 68 `Setenv("HOME"/"XDG_STATE_HOME")` sites in tests set these variables, so a refusal keyed on the override would break them |
| NEW budget-bounded | by default `--budget` returns only actionable rows, worst first, top N, plus counts per direction; `--all` returns everything | P2 | — | v0.42 | D (daemon handler; one optional request arg) | no | S | — | **Yes.** The global CLAUDE.md tells every agent to run it. This must be fixed in the daemon, because a client-only filter still receives all 180k rows. The verdict is computed in Go (`internal/daemon/confine_budget.go:50-53`), so filter and cap after classification, not with a SQL LIMIT |
| NEW runner-flakes | two internal/runner test races | P2 | — | v0.42 | tests only | no | S | — | **Yes.** Each flake costs a 10-minute `make ci` re-run |
| 332 | log the error and elapsed time when the history read fails; re-measure after a reboot | P3 | no | v0.42 (label says v0.41) | D | no | S | — | **Yes.** The log line ships in v0.42. **Not done until** the first WSL restart after cutover has produced the line and its reading is recorded on the ticket. The ticket asks for this ("Then re-measure after a reboot", AIRA-332.md:4). Keep it in-review until then |
| NEW lease-remedy | E_LEASE_EXPIRED names the holder and `--steal` | P3 | — | v0.43, held | D (store) | **yes** (lease CAS) | S | — | **Yes, but it is not wording only.** `ErrLeaseExpired` is returned as a bare sentinel inside the CAS (`internal/store/lease.go:275-276`). Naming the holder changes the error value at that point. Build it as its own PR, Opus building and Fable reading that hunk specifically, not inside the Terra batch |
| 323 | history: wall, wait, start, key columns | P2 | yes | v0.43 (label says v0.41) | **C + D + W**, proto bump | yes | M | owner un-hold; Batch A merged first | **Yes.** It is the critical path for 324, 326 and 331. **Not daemon-only.** The daemon's report handler takes a closed allowlist (`internal/daemon/confine_report.go:17-20,27-29`: signature, peak_rss, oom, kind, budget, budget_basis) and rejects anything else with a protocol error. The client frame (`internal/runner/admission_linux.go:818-831`) carries no scope id, wall time, wait time or start time, so the daemon cannot join a report to its grant. The client must send `wall_ms`, `wait_ms`, `started_at` and the argv for the key. The client already holds the wait figure: `confine_linux.go:754` sets `AdmissionWaitedMS`, and the report is sent at `confine_linux.go:1446-1459`. A new client talking to an old daemon is refused, so bump `DaemonProtocolVersion` (now 15, `admission_linux.go:74`) together with the daemon's. Spec §5.1 says "written at the existing teardown site" and is silent on the wire, so revise the spec |
| 228 | collapse the per-slice queue map; generality audit | P3 | no | v0.43, to be held | D | yes | M | — | **Yes, before 324 and 326.** `admitQueues map[string]*sliceQueue` is still live (`internal/daemon/admit.go:2123, 2940`). Deleting it simplifies the code that 324 and 326 will touch |
| 324 | 0.8 × pin charge plus a physical-fit rule | P2 | yes | v0.44 (label says v0.41) | D + W (`discountable`), proto bump | yes | L | build: 228 merged; enforce: 323 data | Yes. Build after 228; ship in observe mode, then enforce |
| 326 | fast lane | P3 | yes | v0.45 (label says v0.41) | D | yes | M | 323 plus 2 weeks of `wall_ms`; independent of 324 and 325 (spec §6 item 4) | Yes. Its build follows 324's build in admit.go, but it does not wait for 324's rollout |
| 331 | cold-start cap from the coarse key | P3 | yes | v0.45 (label says v0.41) | D | yes | S–M | 323 | Owner decision (it reopens AIRA-304) |
| 325 | history refinement of the charge | P2 | yes | later (label says v0.41) | D | yes | M | 324 at 0.8 for 2 clean weeks | Gated, and may never ship |
| NEW kill-attribution (+319) | wording only: "killed by SIGKILL from outside (confine --kill, watchdog or operator)"; the dead-supervisor `--kill` reply says "scope is empty and its supervisor is gone; the reaper removes it", not "retry" | P3 | — | v0.44, held | C + D | no | S | — | Yes, later. **No xattr marker**: it would be a mechanism for one message, and cgroupfs user xattrs on this kernel are not verified. 319's "may be queued" case is moot after 328 |
| 319 | not-found or dead-scope wording | P3 | yes | v0.44 | D | no | S | — | **Merge into kill-attribution** (same reply function) |
| 271 | top: KK kills the selected job | P2 | no | v0.44, to be held | C | no | S | — | Yes, small, but no other project needs it |
| 225 | snap tools escape the scope | P2 | no | later, to be held | C | no | S | owner choice (B#6) | Owner decision. Verified: the peak report is sent at `confine_linux.go:1456` before the escape is classified at 1462 onward, so escaped runs feed reserve history today. Moving the report after teardown touches the same lines as 323's frame change. Build 225 after 323 |
| 226 | over-reserve warning at admission time | P2 | no | — | — | — | — | — | **Retire**, after copying its load-bearing principle into the non-goals of `docs/proposals/2026-09-09-aira-cloud-command-family-proposal.md`. The principle is "never wire a cloud-offload trigger to admission pressure" (AIRA-226.md:12; the proposal does not state it today, by grep). The 310/311 exit hint, 312 `--resize` and 324 cheaper over-pins answer the warning itself |
| 195 | an outer kill orphans sibling scopes | P2 | no | — | — | — | — | — | **Retire**, after recording the accepted gap in the skill (`internal/core/skill.go`; no sentence there today, by grep). The sentence: "killing an outer job does not reach scopes its children confined themselves; find them in `confine --list` as LIVE=unsupervised and kill them by scope id". The listing half shipped in v0.36. AIRA-194 is retired. An automatic reaper would be judgement, not a primitive |
| 236 | time tracking | P3 | no | later, to be held | — | — | — | — | **Hold whole.** 323 persists wall and wait time but not option B's `--ticket/--phase/--label` attribution (AIRA-236.md:12), so 323 does not absorb B. Add a note that 323 covers the timing half |
| 197 | `aira container run` (podman) | P2 | no | later, to be held | C + D | no | L | live podman probes | Owner decision (B#11). No current requester found (not verified) |
| 317 | pinned live charge, p90 prior, top-holder line | P2 | yes | — | D | yes | — | — | Owner decision (B#3, B#4). Part (1) is superseded by 324 |
| 320 | p90 index and cache | P3 | yes | — | D | no | S | 317(2) | **Retire if B#3a** |
| 318 | page cache ratchets the estimate | P3 | yes | v0.45 | D | yes | S–M | owner (B#7) | Owner decision. The cheap rule is in B#7 |
| 273 | bimodal command OOM creep | P2 | yes | v0.45 | D | yes | M | 317(2), 331 | Owner decision; pair it with 318 |
| 321 | estimator subject = argv + repo | P2 | yes | later | C + D | yes | M | owner timing | Owner decision on timing (B#2) |
| 279 | CPU slots `--cpus` | P2 | yes | later | C + D + W | no | M–L | owner | The skill sentence ships in v0.42 (B#8); the flag stays held |
| 306 | name the top memory consumer outside the slice | P3 | yes | later | D | no | S | — | Keep held |
| 296 | registry compaction | P3 | yes | later | D | no | S | owner | Keep held. Reported as 4,291 lines and 1.4 MB (was 2,247 lines on 2026-10-10; not re-measured). Not urgent |
| 295 | check across worktrees (2 questions) | P3 | yes | — | — | — | — | owner | Recommend "no" to both questions, then retire |
| 300 | fixture canary seed | P3 | yes | later | C | no | S | owner | Keep held |
| 316 | `aira projects` verb | P3 | yes | — | — | — | — | — | **Retire.** The ticket itself says "build only if a second need appears" |
| 248 | GPU/VRAM killer and exclusive mode | P3 | yes | icebox | — | — | — | — | Keep in the icebox. Admission gating shipped (AIRA-268, v0.23). Per-process VRAM reads [N/A] on WSL2 |
| 250 | secrets broker | P2 | yes | icebox | — | — | — | — | Keep in the icebox |
| 260, 272 | aitest design cycle, measured profile | P2–3 | yes | later | Python | yes | L | deploy's contract | Keep held, waiting on deploy |
| 333 | traceability (shelved) | P3 | yes | later (label says v0.42) | — | — | — | owner | Keep shelved. Relabel so it no longer shows up under v0.42 |

## B. Decisions for the owner

I clear no holds. Each "Do" line is what the owner would type or say. Setting a hold or a milestone is an agent step. Clearing a hold is always the owner's.

### B.1 Design and scope decisions

| # | Item | Options in plain terms | Recommendation | Effect |
|---|---|---|---|---|
| 1 | **AIRA-323**: record each job's run time, wait time, start time and a normalised command name | (a) start now; (b) wait | **(a).** It is pure data collection, and anything that uses the data needs about 2 weeks of it first. Its spec revision can start at once. The build branches after Batch A merges, because both touch the report sender. Do: `aira set AIRA-323 hold=false` | Starts the critical path |
| 2 | Put the **repo identity** into AIRA-323's command key? Example: two different `./runall.sh` scripts in different repos share one history today, and one OOM was reported as putting a 9 GiB reserve on both (RANT-133; the 9 GiB is not verified) | (a) yes, in the new key column only, with no reset of existing history; (b) no, leave it to AIRA-321 | **(a)**, with a cost. It adds a second field the client must send. It also departs from the gated spec's `keys()` reference (spec §5.1), so it needs a spec revision and a fresh plan gate, with the replay script and golden fixtures changed together. The 323 spec revision already has to happen for the wire change, so both go through one gate | 326 and 331 get more precise keys |
| 3 | **AIRA-317 (2)**: delete the machine-wide "p90" prior | (a) delete it and retire 320; (b) keep it and build 320 | **(a).** It misses its 250 ms deadline on this box (spec §5.1), so 16,299 of 19,899 first runs got the 1 GiB default anyway. AIRA-331 is the better-targeted replacement | 320 retired; 273 option A redefined |
| 4 | **AIRA-317 (1) and (3)** | (1) live charge for pins; (3) name the biggest idle holder on the wait line | Retire (1): 324 supersedes it. Reject (3): it lengthens the line that AIRA-311 deliberately shortened | — |
| 5 | **AIRA-331** cold-start cap | Raise the cap for a never-seen command from the history of similar commands. It avoids about 34% of cold OOMs, but about 30% of cold runs then reserve more (a median of 2.9 × their real peak) | Decide once 323 data exists. I lean yes | — |
| 6 | **AIRA-225**: snap gcloud and terraform escape the memory cap | (a) a loud trailer line; (b) refuse to run them; (c) document the gap only | **(a)**, plus: stop learning reserve history from escaped runs (verified that they feed it today, `confine_linux.go:1446-1462`) | hint-honesty already hides the wrong hint |
| 7 | **AIRA-318 + 273**: estimator ratchets (page cache, bimodal jobs) | Cheap rule: a clean run that reached its cap never grows the estimate beyond the cap it ran under (swap is off, so it fitted). For 273, keep the big mode longer | Approve the cheap rule for 318. Look at 273 after #3 and #5 | Fewer OOM loops in other projects |
| 8 | **AIRA-279** CPU slots | (a) a skill sentence telling agents to set `CARGO_BUILD_JOBS` or `make -j`; (b) a `--cpus N` flag; (c) a shared jobserver | **(a)** in v0.42; keep (b) held | — |
| 9 | Retirements | 226 (after the proposal note), 195 (after the skill sentence), 316, 305 (fixed), 320 (if #3a), 317(1); 295 ("no" to both questions) | Approve as a list | A cleaner ready list |
| 10 | Holds and milestones for unheld items outside v0.42 | 228 → v0.43; 271 → v0.44; 225, 236, 197 → later. All held | The agent sets these holds. Each one is released by a row in B.2 | `aira ready` shows only the current release |
| 11 | **AIRA-197** container launcher | Keep it, or retire it until someone needs it | Hold it as "later". It is an L-size feature with three unmeasured podman questions | — |
| 12 | **Tag v0.41 now** from 692cb252 | (a) tag now; (b) wait for Batch A | **(a).** The code is already live on the box and was reviewed when merged. Peers have not been told that `--kill` now cancels queued jobs (328). The tag then does not wait for new work | Batch A becomes v0.42 |

### B.2 Owner un-hold steps, one per release

Every ticket the plan builds after v0.42 is held, so each release opens only when the owner releases its tickets. No agent clears any of these holds.

| When | Owner releases | Do |
|---|---|---|
| Now | 323 (B#1) | `aira set AIRA-323 hold=false` |
| When v0.42 is tagged (v0.43 opens) | 228, lease-remedy | `aira set AIRA-228 hold=false`, and the same for the lease-remedy ticket |
| When v0.43 is tagged (v0.44 opens) | 324; kill-attribution, 319, 271 | `aira set AIRA-324 hold=false`, and the same for the others |
| When 323 has 2 weeks of `wall_ms` (v0.45 opens) | 326; 331 if B#5 is yes; 318 and 273 if B#7 is approved | `aira set AIRA-326 hold=false`, and so on |
| After 324 has run 2 clean weeks at 0.8 | 325, if its gate passes | `aira set AIRA-325 hold=false` |

The v0.42 tickets (hint-honesty, foreign-env-refusal, budget-bounded, runner-flakes) are filed unheld with milestone v0.42. 332 is already unheld. They unblock other projects, which is what the repo rule allows in the next release. runner-flakes is aira's own, but it costs no extra review cycle in the batch.

## C. Release structure

| Release | Theme | Items | Restart and proto | Batching |
|---|---|---|---|---|
| **v0.41** (tag now) | What is already on master | 327, 328, 330, 322, plus docs (the 213 decision, the charge spec, the 329 diagnosis, the 333 proposal) | The daemon already runs 692cb252, so there is no restart. Proto unchanged at 15 (not re-verified against v0.40) | Tag and notify only. The notice states the 328 behaviour change |
| **v0.42** | Honest messages and the stalls other projects are hitting | **Batch A**: hint-honesty, foreign-env-refusal, runner-flakes, 332, the 279 skill sentence, the 195 skill sentence. **budget-bounded**: its own PR | Restart needed (332, budget handler). Proto: budget-bounded adds one optional request argument. Decide in its plan whether that needs a bump; a new client sending the argument to an old daemon is the case to check | Batch A on one branch with one review. budget-bounded on its own branch with one review loop. Neither is correctness-critical |
| **v0.43** | Measure first; simplify admission | 323 (with the repo key term if B#2a); 228; lease-remedy | Restart. **Proto bump** (323's frame). Schema change with no migration | 323, 228 and lease-remedy each get their own two-loop |
| **v0.44** | Discounted charge in observe mode | 324 in observe mode (the moves to 0.9 and then 0.8 are mode changes and need no release); small batch: kill-attribution + 319, 271 | Restart. **Proto bump** (324's `discountable` field) | 324 gets a two-loop. The small batch is separate |
| **v0.45** | Quick jobs first; cold start | 326; 331 if approved; the 318 rule and 273 | Restart | 326 and 331 each get a two-loop. 318 and 273 form one estimator track |
| later | — | 325 (gated), 225, 236, 197, 306, 296, 300, 279 flag, 321, 248, 250, 260/272, 333 | — | Held |

## D. Execution plan

**Critical path, with builds serialised in admit.go and rollouts running in parallel:**

| Day (estimate) | Event |
|---|---|
| 0 | Tag v0.41. Housekeeping PR. Batch A and budget-bounded start. The owner un-holds 323, and its spec revision and re-gate start in parallel (no code files touched) |
| ~2–3 | Batch A merges; v0.42 ships. The 323 build branches from master after Batch A. 228 starts in parallel; it is in admit.go, while 323 is in the report path and store (assumed not to overlap; check at branch time) |
| ~7 | v0.43 ships (323 + 228). `wall_ms` data starts accruing |
| ~8–13 | 324 builds on top of 228 and ships in observe mode as v0.44 (about day 14) |
| ~14–20 | 326 builds after 324's build merges (admit.go order: 332 → 228 → 324 → 326 → 331). 331 follows |
| ~21 | 2 weeks of `wall_ms` exist. 326 (and 331 if approved) ship as v0.45, in week 3 to 4. The counterfactual replay gate for 324 enforce at 0.9 can run |
| ~28 | 324 enforce at 0.8, if the gate holds. 325's two clean weeks start counting |

v0.41 and v0.42 do not sit on this path. The capacity gain from 324 at 0.8 arrives about 4 weeks out. 326 does not wait for it.

**Parallel worktrees, and where they collide:**

| Track | Files it touches | Conflicts with |
|---|---|---|
| Batch A | runner/confine_linux.go, runner/confine.go, cmd/aira/dispatcher.go, runner/admission_linux.go, two runner tests, daemon/admit.go (332 log line only), core/skill.go | budget-bounded (skill.go); 323 (confine_linux.go and admission_linux.go): **land Batch A first, then branch 323** |
| budget-bounded | cmd/aira/confine_budget_render.go, confine_history_client.go, main.go, daemon/confine_budget.go, core/core.go, core/skill.go | Batch A (skill.go); 323 (main.go: `ReportPeakSample` caller at main.go:1438) |
| 323 | runner/admission_linux.go (frame), runner/confine_linux.go (report site), runner/confine_budget.go (`ConfinePeakReport`), cmd/aira/main.go:1438, daemon/confine_report.go (allowlist), store/confine_peak_history.go, a new key function, the replay script and golden fixtures, both proto constants | Batch A, budget-bounded; later 225 (same report lines) |
| 228 | **daemon/admit.go** (queue map, prune and recreate), reload.go | 332 (land first), 324, 326. **Never run in parallel with 324 or 326** |
| lease-remedy | store/lease.go (CAS), its callers' error rendering | nothing scheduled |

The hotspots are `internal/core/skill.go` (almost every item edits it, so land one branch and rebase the other), `internal/daemon/admit.go` (serialise the builds as above) and the runner report path (Batch A, then 323, then 225).

**Who builds and who reviews:**

| Class | Builder | Reviews |
|---|---|---|
| Batch A, budget-bounded, 271, kill-attribution | Terra (Codex, effort `high`), with a self-review before it returns | Sol `high` (both directions), then Fable as the final gate |
| 323, 228, lease-remedy, 331, 318/273 | Opus, TDD | Fable two-loop in a throwaway detached worktree. Plan gate first: 323's revised spec (wire plus key) and a short spec for 228. For lease-remedy, Fable reads the `lease.go:275-276` hunk specifically |
| 324, 326 | Opus, TDD | Spec already gated. Astra `xhigh` plus a Fable two-loop. 324 changes what the shared ledger charges |

**Per-ticket pipeline:**

1. Confirm that `aira ready` shows ready:true, then `aira claim` and `aira worktree register`.
2. Challenge pass: what is the from-scratch minimum, and can anything be deleted?
3. Plan or spec, then plan review and the gate. Skip this for Batch A, where each ticket already states its fix.
4. Build with TDD. Show by mutation that every test fails against the wrong implementation. foreign-env-refusal also needs its false-refusal test.
5. Work review, then fixes. Every confirmed counterexample becomes a regression test.
6. Gate: `aira confine -- make ci`. Record the exit code and read the whole failure, not just the tail. Also run the real-cgroup tests by hand without `-race`.
7. **Commit the ticket transitions inside the PR branch** (to in-review, and to done in the merge-ready commit), as PR #228 did (commit d82ed6f8 is in that PR). Tickets are git files, so a transition made after merge needs a second commit, and that commit gets forgotten. That is how 307 to 312 drifted. Merge on approval. If three rounds in a row end with a blocker, stop and re-evaluate the design.

**Cutover and release steps (per release):**

1. `aira confine -- make dist` (static amd64 and arm64 builds plus sha256).
2. Install the client: copy it, then move it into `~/.local/bin/aira`.
3. Run `aira confine --list` and send a one-line heads-up to active sessions.
4. Restart the daemon explicitly with `systemctl --user restart aira-daemon.service`. Installing alone skips the restart. v0.41 needs no restart.
5. Run `aira skill install ~/.claude/skills/aira --force`.
6. Confirm that `aira version` reports diverged=false.
7. Smoke test: a small confined run, checking the new trailer lines. **On the proto bumps (v0.43 and v0.44)**, long-running MCP servers and old clients in other sessions keep speaking the old proto until they restart. With 323, an old client's peak report is simply refused, so its sample is lost, but its job still runs (not verified). Warn peers to restart their MCP servers.
8. Tag `v0.N`. The tag push also runs `make ci`.
9. Notify deploy, speed and subpipe (whoever is up in `ListAgents`) with the changelog, the proto, verb and schema changes, the sha256 sums and what comes next. Skip subpipe for releases that only change how output is displayed.
10. **v0.42 only:** after the next WSL restart, read the 332 log line (error, elapsed ms, DB state), record it on AIRA-332, and only then move it to done.

**Cost and time (estimates, not measured):**

- One `make ci` takes about 10 minutes on a quiet box and 20 minutes or more when the box is saturated (internal/pylib has hit its 20-minute timeout before).
- One review round takes hours.
- Batch A: about 1 day. budget-bounded: under a day (S, once fixed in the daemon).
- 323: about 3 days plus the spec re-gate. 228: 3 to 4 days. 324: about 5 days of build plus about 2 weeks of rollout after observe.
- Run tracks in parallel only where their files do not collide.

**Measure per PR:** agent count, total tokens, dispatch-to-merge time, number of `make ci` runs with wall time and flake re-runs, review rounds, blockers per round, and how many fixes the reviews caused. Report these per release in the tag notice.

## E. Risks, open questions, first steps

**Risks:**

- 323's wire change plus the repo key term (B#2a) depart from the gated spec. The spec must be revised and re-gated before the build. The replay script and golden fixtures must change together.
- After 324, a slice-level OOM can kill a job that is still under its own pin. The spec's rollback triggers are the control.
- 228 refactors the queue code that 328 just changed. Fable should get the 328 race tests as fixtures.
- lease-remedy touches the lease CAS. If naming the holder cannot be done without widening the CAS, drop it to "later": the bare code is honest, only unhelpful.
- Ticket statuses drifted (7 shipped tickets not marked done), and `aira ready` showed 311 and 312 as buildable when they had shipped. Pipeline step 7 removes the cause.

**Open (not verified):** the environment of the original RANT-130 run; why RANT-74's scope took more than 2 s to empty; 312's acceptance list; whether anyone still needs 197; the RANT-133 9 GiB figure; the current size of registry.jsonl; whether an old client survives a refused peak report on v0.43.

**First things to do on approval:**

1. Tag v0.41 from 692cb252 (B#12), with no restart, and notify peers of the 328 change.
2. Housekeeping, **in its own worktree on a branch with a PR**, never on the root checkout:
   - mark the 7 shipped tickets done and close 305;
   - file the new tickets (hint-honesty, foreign-env-refusal, budget-bounded, runner-flakes as v0.42 unheld; lease-remedy and kill-attribution as held, v0.43 and v0.44) with the investigators' fixes, tests and evidence, and with the corrections above (the foreign-env false-refusal test, the daemon-side budget fix, wording-only kill-attribution);
   - **relabel milestones**: 332 → v0.42; 323 → v0.43; 324 → v0.44; 326 and 331 → v0.45; 325 → later; 333 → later;
   - set the holds and milestones in B#10;
   - add the notes to 318, 225, 321, 273, 279, 236 (323 covers the timing half), 246, 135, 148 and 70; widen 319 and point it at kill-attribution;
   - copy the AIRA-226 principle into the cloud proposal's non-goals before retiring 226;
   - mark the 19 rants reviewed;
   - apply the retirements the owner approves.
3. Start Batch A and budget-bounded in two worktrees (Terra builds; Sol then Fable review), aiming at v0.42.
4. Owner: un-hold 323 and answer B#2 and B#3. Opus then revises the 323 spec and puts it through the gate while Batch A builds, and branches the 323 build once Batch A has merged.

## Review changes

Applied from the critique (each checked on master 692cb252):

- **P1-1, 323 is not daemon-only: accepted.** Confirmed at `confine_report.go:17-29` (closed allowlist, refusal on unknown fields) and `admission_linux.go:818-831` (frame fields). 323 is now C + D + W with a proto bump, and B#2a carries an explicit spec revision and re-gate. The proto bump falls at **v0.43, not v0.42**, only because adopting "tag v0.41 now" renumbers the releases: 323 is still the first release after the honest-messages batch. The stale-MCP warning now names v0.43 and v0.44.
- **P1-2, hold bookkeeping: accepted.** Confirmed that 323, 324, 325, 326, 331, 318, 273 and 319 are `hold:true`, and that 228 is `hold:false` with no milestone. B.2 adds one owner un-hold row per release. For 228 I chose "hold with milestone v0.43, released in B.2" over leaving it unheld, because the repo rule holds work that does not unblock another project until its release opens.
- **P2-3, parallelism: accepted.** The report sender is `admission_linux.go:790-803`, it is wired at `confine_linux.go:274`, and it is called at `confine_linux.go:1456`. I also found the second caller at `cmd/aira/main.go:1438`, which budget-bounded's file list shares. Batch A lands first; 323's spec work runs in parallel.
- **P2-4, 326 critical path: accepted.** Spec §6 item 4 makes it independent of 324 and 325. The builds are serialised, the rollouts are not, and 326 ships in week 3 to 4.
- **P2-5, milestone relabelling: accepted.** Confirmed that 323 to 326 and 331 are labelled v0.41 and 333 is v0.42. 332 is also labelled v0.41 and is relabelled too.
- **P2-6, transitions in the PR branch: accepted.** Commit d82ed6f8 is in PR #228. Housekeeping now runs as a worktree and PR.
- **P3-7, 236: accepted.** Option B includes `--ticket/--phase/--label` attribution (AIRA-236.md:12). It is held whole.
- **P3-8, 226: accepted.** The principle is at AIRA-226.md:12, and a grep of the proposal for it found nothing.
- **P3-9, 195: accepted.** No unsupervised or sibling sentence exists in `internal/core/skill.go`.
- **P3-10, lease-remedy: accepted.** `lease.go:275-276` is a bare sentinel inside the CAS. lease-remedy is split out of the Terra batch, marked critical and moved to v0.43, held.
- **P3-11, foreign-env-refusal: accepted.** `unreachableDaemonError` is at dispatcher.go:961. I count 68 `Setenv` sites for HOME or XDG_STATE_HOME in tests, which matches the critique's count.
- **P3-12, the 332 reboot step: accepted** (AIRA-332.md:4). Added as cutover step 10 and as the done criterion.
- **Removals:** kill-attribution is reduced to wording only (accepted). Tag v0.41 now (accepted, B#12). budget-bounded is **accepted with one refinement**: the fix must be daemon-side, as the critique says, but it cannot be a SQL `LIMIT`. The direction and recommendation are computed in Go by `ClassifyResourceBudget` after the rows are read (`confine_budget.go:46-53`), so the cap and counts apply after classification. It is still S.
- **Rejected:** nothing outright.
- **Newly verified:** `confine_linux.go:1446-1462` shows the peak report sent before the escape is classified, so escaped runs do feed history. Still not verified: RANT-133's 9 GiB and registry.jsonl's size.
