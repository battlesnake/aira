---
{"schema":1,"id":"AIRA-100","project":"aira","title":"test-lite-slowbuild's heavy build subprocesses (uv wheel builds, docker build) are structurally outside aitest worker-pool CPU governance","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","confine","dogfood"],"hold":false,"relations":[]}
---
Reported by peer session `split` (fastest-ee, NF-35 relocation work) — related to but distinct from AIRA-64.

## Incident

`make test-lite-slowbuild` OOM-killed 3 times across 3 attempts (progress 100%/41%/73%), all confirmed as pure `systemd-oomd` PSI-pressure kills with 0 test failures each time:
1. `aira confine --memory-max 12G` — hit its own cap during teardown at the 12G peak.
2. `aira confine --memory-max 24G` — killed at 41%, well under its own cap, with 58G machine-wide free (a PSI-pressure kill under real contention, not a hard cap breach).
3. `aira confine --delegate-ram --memory-max 24G` — killed at 73%.

## Two distinct findings

1. **This specific recipe never engages aitest at all.** `Makefile:350` (fastest-ee) is `cd lite && FASTEST_RUN_SLOWBUILD=1 uv run pytest -q` — runs pytest directly with the lite project's own `-n auto` addopts, never shelling `pytest_worker_flags.sh` the way `test-lite`/`test-engine`/`merge_gate` do. This is a fastest-ee-side routing gap, not necessarily an AIRA defect, but worth knowing when reasoning about AIRA-64's real-world coverage: not every aitest-eligible recipe actually reaches aitest today.

2. **The structural point, relevant regardless of (1) being fixed.** This recipe's heaviest work is not pytest workers — it's build subprocesses spawned from inside test bodies (`uv` wheel builds, `docker build` for on-prem-bundle/desktop-matrix/docker-context images). Those are plain `subprocess`/`docker build` spawns, not pytest workers. **aitest's fork+admit worker pool structurally cannot govern them even if this recipe were correctly wired through aitest** — any CPU-slot mechanism hooking into aitest's worker spawn/retire path (e.g. whatever AIRA-64 builds) only ever covers aitest worker CPU, never arbitrary subprocess CPU a test body happens to launch.

## Why this is a separate ticket from AIRA-64

AIRA-64 is scoped to CPU-concurrency governance for aitest's own worker pool. This is a different, broader problem: governing (or at least making admission-aware of) arbitrary heavy subprocess spawns from *inside* a confined job, which aitest's worker-level mechanism cannot reach by construction. Conflating the two would blow AIRA-64's scope.

## Not decided here

split's own framing: "is 'run slowbuild on a quiet slice' the accepted answer, or is this worth wiring a real fix?" Candidates, not scoped or built:
1. Accept as a documented limitation — heavy build-subprocess recipes want a quiet slice, same disposition as AIRA-64's own original (superseded) framing before the owner escalation.
2. A more general mechanism: something inside `aira confine` itself that can account for or bound descendant subprocess CPU/RAM regardless of whether they're pytest workers, aitest workers, or arbitrary build tooling — a genuinely different, likely much larger piece of work than AIRA-64.
3. fastest-ee-side: serialize/queue heavy build-subprocess-spawning recipes specifically, independent of any AIRA-side fix.

Not urgent per split ("not blocking me — I route packaging-closure relocations through the aitest-governed `test_packaging.py` path and accept the full slowbuild cost proportionately"). Recorded so the finding and the scope boundary aren't lost.

## Finding 1 (routing) closed — fastest-ee #1157 (split, 2026-09-05)

`test-lite-slowbuild` now delegates to `FASTEST_RUN_SLOWBUILD=1 make test-lite`
— the same form `scripts/impacted_test_run.sh`/`run-affected` already use — so
it reaches the one resolver home transitively rather than via a second copy of
the resolver/case block (Fable's review steered away from the initially-planned
duplicated-block shape). It also now inherits `--junitxml` and
`FASTEST_TEST_PATHS` narrowing for free. Behaviour is unchanged by default
(already `-n auto` under `aira_xdist_governor`); the only delta is it now
honours an exported `FASTEST_XDIST_WORKERS`. Deliberately still routes to
**plain xdist, not aitest** — same rationale as the rest of this ticket: the
build/packaging subprocess load would OOM a 512MiB aitest worker. Verified
end-to-end under real `aira confine --delegate-ram` (narrowed to
`test_tiers.py`): 17 passed. Every fastest-ee pytest recipe now reaches the
resolver.

**Finding 2 (the structural build-subprocess-governance gap) is unchanged and
still fully open** — this was a routing/consistency fix, not a governance
mechanism. The three candidate directions above remain undecided.

## Resolution (2026-09-05, backlog-completion triage)

Verified against master 3251bed. Finding 1 (routing) is closed on the fastest-ee side (#1157, recorded in the ticket at 4523e08). Finding 2 -- "build subprocesses inside a confined job are outside CPU governance" -- is literally true of AIRA-64's gate and remains so: scanSliceWorkerScopes (internal/daemon/cpuslots.go:244-248) counts only `.aira-worker-*` directories, so a `uv` wheel build spawned from a test body contributes zero to the slot total. But re-reading the tree shows the concern does not warrant new AIRA work, for four source-grounded reasons.

(1) The "arbitrary fan-out" harm is already kernel-bounded between scopes. Every confine scope gets a whole-subtree cpu.weight aging schedule (internal/runner/confine_linux.go:71-78 defaultCPUWeightSteps 100->10 over 30 min; applied at :738-744; startCPUWeightDecay :1683; from docs/superpowers/specs/2026-08-30-scheduler-slice1-cpuweight-aging-plan.md). Confirmed LIVE on this box: aira.slice cgroup.subtree_control = `cpu memory pids`, and the two running .aira-CONFINE-* scopes read cpu.weight=30 and cpu.weight=10. CFS group scheduling means a scope that fans out into N build subprocesses competes with sibling scopes as ONE weight unit, not N -- it cannot starve a neighbouring aitest job below that job's weight share, and a long-running build decays toward the floor. The AIRA-64 plan (section 4.12, lines 590-596) names exactly this -- "cgroup-level CPU accounting or weighting at the confine-scope level" -- as the different mechanism such governance would need; it already exists and the ticket never mentions it. The ticket's RAM half is likewise already covered: the scope's memory.max sub-cap + memory.oom.group=1 (AIRA-57/67) bind every descendant. The single true escape is `docker build` (dockerd/buildkit under /system.slice), which is AIRA-102's own explicitly documented scope limit (run-only detection, L5) -- not this ticket's to re-decide.

(2) Candidate 1 ("run slowbuild on a quiet slice") is no longer a hope but a fail-closed primitive: AIRA-101 `aira confine --exclusive` is merged (352387b), deployed, and documented for operators in internal/core/skill.go:318.

(3) The three OOM incidents that motivated the ticket were confirmed systemd-oomd PSI kills; AIRA-91's root-cause work established that mechanism is sustained memory.high reclaim pressure, not CPU, and the owner assigned its fix to AIRA-29 track-actual / AIRA-103 (built, shipped `off`). A descendant-CPU governor would have prevented none of the three kills, so the ticket's CPU framing misattributes its own evidence.

(4) Against this project's stated bar: CLAUDE.md requires coverage gaps to be "written down and accepted by reviewers; never silent" -- this one is written down and was accepted through three adversarial plan-review rounds (AIRA-64 plan section 4.12 and the section 10 deferral list: cpu.max-derived capacity, fan-out weighting), plus AIRA-102's L5. The owner's HARD architectural-simplicity rule ("prefer keep the primitive + document the gap over new machinery") points directly away from candidate 2, which would be either a daemon-managed per-scope cpu.max rebalancer (a second scheduler; and AIRA-64 plan section 4.2 records desiredCPUSlots ignores cpu.max, so workers would still be oversized inside a quota) or counting non-worker load in the slot scan (which can only deny aitest workers harder and cannot bound the build subprocesses themselves -- and slowbuild is now deliberately routed to plain xdist, so it never calls worker-admit at all). Candidate 3 is fastest-ee's, not AIRA's. Reporter's own framing: not blocking, filed "so the finding and the scope boundary aren't lost" -- both are now durably recorded.

This is not needs-owner-decision: the adjacent forks were already decided by the owner (AIRA-64 build option 1, refuse serialising; AIRA-101 exclusive = the quiet-slice answer), and AIRA-64 plan section 8 still holds the one genuinely open policy question (reclassify starved wall-clock timeouts as unevaluated). Not already-done: no mechanism counts non-worker CPU load, and I am not claiming one does.

What I could not establish without running anything: whether cpu.weight fairness in practice keeps a co-resident aitest suite's wall-clock timeouts from misfiring next to a build-heavy scope. If the field ever shows that with data, it is a new ticket against the AIRA-64 section 10 fan-out deferral -- not a reason to keep this one open.

Close with a resolution note stating: Finding 2's gap is real but bounded by live per-scope cpu.weight (CPU) and memory.max+oom.group (RAM); `--exclusive` is the operator answer for build-heavy recipes; `docker build` remains AIRA-102's documented boundary; the PSI-kill incidents belong to AIRA-29/91-B/103.


*Disposition: Closed — not needed, reached via a source-verified triage pass (Fable model) as part of the backlog-completion push, independently spot-checked by the coordinating session before closing.*
