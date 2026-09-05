---
{"schema":1,"id":"AIRA-94","project":"aira","title":"aitest has no test co-location/grouping — dynamic FIFO dispatch makes state-leaking tests flake non-deterministically","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","dogfood","reliability"],"hold":false,"relations":[]}
---
Reported by peer session `speed` (Inc-2.6 NF-35 relocation rail work) and independently corroborated by `ems`, both on a QUIET slice (load 8-15) — this is NOT contention/AIRA-91/AIRA-92, it reproduces with no contention at all.

## Symptom

fastest-ee engine + pipeline legs are flaky under aitest: tests pass individually/in isolation, but the SET of tests that fail varies run to run. speed observed run-1 flagging `test_dispatch_coverage` (not `test_copper_si`); run-3 flagging `test_copper_si` (bl286-neutrality) + `test_vr127_corpus_arm_b` + 3x `test_derating_origins` (not `test_dispatch_coverage`). `ems` saw the same class on `pipeline`/`test_live_ingest`. The affected tests are corpus-neutrality / cid-invariant / derating tests — exactly the shape of test that leaks or depends on process-global state (module-level caches, files written but not cleaned up, singletons, etc).

## Root cause, verified against source

`internal/pylib/aitest/supervisor.py` dispatches tests via a single flat FIFO queue with no grouping: `self.queue = [item.nodeid for item in items]` (collection order), and `next_nodeid()` just pops the front of that queue whenever any worker goes idle (`_dispatch_to_idle_workers`). Grepped the whole aitest source for any grouping/pinning/scope-based dispatch concept (the equivalent of pytest-xdists `--dist=loadscope`/`loadfile`) — none exists. Which worker a given test lands on, and which OTHER tests it runs alongside on that same worker, is therefore a function of real-time race timing between workers, not a fixed/reproducible partition.

**CORRECTED (speed, same night) — the original hypothesis above (loadscope/loadfile grouping) was checked against the actual fastest-ee repo and is WRONG, in the important, load-bearing way, not a minor detail:**

- fastest-ee runs engine + lite under xdist's **default `load` dist** (unscoped), via `addopts = "-n auto"` — confirmed at `tools/correctness/pyproject.toml:110`, `lite/pyproject.toml:147`. Not loadscope/loadfile anywhere.
- **`--dist loadscope` is explicitly, deliberately FORBIDDEN in this codebase**, and for exactly this failure class: `docs/test-suites.md:38-39` — *"Do not use `--dist loadscope` — it is marginally faster but BREAKS TEST ISOLATION in at least one module (see the stress harness below)."* `tools/correctness/pyproject.toml:97` repeats the ban. An overnight stress harness already validated `load` is safe and loadscope is not, and already identifies the leaking module.

So the mechanism runs the OPPOSITE direction from the original hypothesis: there is a known module whose tests leak state into each other **when co-located on the same worker** — which is precisely *why* loadscope is banned. xdist's `load` mode **spreads** a module's tests across workers (its initial distribution is round-robin across workers from collection order, so two collection-adjacent tests land on *different* workers), so the leaking siblings rarely land together → stable. aitest's flat FIFO-with-greedy-pull apparently co-locates those same siblings *more* than xdist `load` does under real timing variance (plausible mechanism, not yet nailed down precisely: a fast worker pulling several consecutive — i.e. same-module — queue items in a row before slower workers finish their first pull, versus xdist's guaranteed-spread initial round-robin batch) → the leak surfaces → flaky.

## Why this matters beyond one leg (speed)

1. Makes merge-gate NEW-FAILURES base-red triage unreliable — a non-deterministic flake is not reliably in the base-red manifest, so it reads as a false NEW failure.
2. Will bury the signal of NF-35s relocation rail (forces the full engine leg on every git-mv relocation → these flakes read as false NEW on every relocation PR).
3. General reliability concern for any parallel-leg executor built on aitest.

## Fix-path options, corrected (speed, same night)

1. **Root fix — the correct one.** Find and fix the actual state leak in the known-bad module the fastest-ee stress harness already identifies (`docs/test-suites.md`) — fastest-ee has been *avoiding* this defect via `load`'s spread, not fixing it; aitest just removed the accidental cover. This is the test owners' turf, not AIRA's.
2. ~~Add loadscope-equivalent grouping to aitest~~ — **WRONG, retracted.** This is exactly the mode fastest-ee already proved breaks isolation. Building this into aitest would make the leak *deterministically* fail every run instead of flakily failing some runs — strictly worse.
3. ~~Pin known-flaky files to one worker~~ — **also wrong**, same reasoning as 2, if it co-locates the leaking siblings.
4. **New, correct architectural candidate (speed): anti-affinity dispatch.** Make aitest's dispatch actively *spread* collection-order-adjacent (i.e. same-module) tests across different workers — the opposite of loadscope, restoring xdist `load`'s actual spreading behavior rather than accidentally undermining it. Would restore pre-#1124 behavior without requiring every leak to be found and fixed first. Real engineering; the exact mechanism by which the current flat FIFO clusters more than xdist `load` still needs pinning down precisely before this can be designed properly.
5. **Flaky-quarantine**: fastest-ee-side only, does not fix the underlying signal-reliability problem and delays the real fix — explicitly flagged by speed as an unwanted default.

**Closed out for tonight (qual + speed, same night).** qual found the actual
evidence rather than re-running a fresh experiment: a 2026-07-09 overnight
stress-harness run (`~/tmp/overnight-stress-logs/`) already caught genuine
co-location-dependent failures, in a `device_db`-resolution-adjacent module
cluster — a shared mutable `device_db` cache not resetting between
co-located tests. qual's own clean small-scale reruns (the 6 tonight-flaky +
7 historical cases, all pass in isolation) suggest the leak only manifests
at full-suite scale, not in a small targeted rerun. Joint decision: don't
burn a dedicated full-leg experiment just to reconfirm live-ness — the bug
class is already confirmed real, and NF-35's relocation rail will recreate
full-scale co-location pressure regardless, so mitigation is needed either
way. Capture variance opportunistically whenever a full leg runs for other
reasons instead.

**Disposition, agreed by both peers:** option 1 (root fix — reset the shared
`device_db` cache; test-owner turf, not AIRA's) is primary. The anti-affinity
dispatch idea stays the right AIRA-side complement *if* a tooling mitigation
is ever wanted, but only once the exact FIFO-vs-xdist-`load` clustering
mechanism is actually pinned down (still open, not done). loadscope-style
grouping stays retracted. No urgency — this remains a prerequisite for the
relocation rail, which is itself behind other work on speed's side.

## Resolution (2026-09-05, backlog-completion triage)

Verified against current source (master 3251bed), not the ticket's prose.

AIRA side, unchanged and deliberate: /home/mark/claude/aira/internal/pylib/aitest/supervisor.py still dispatches from a flat FIFO — collect() L619 `self.queue = [item.nodeid for item in items]`, next_nodeid() L621-626 pops the front, _dispatch_to_idle_workers() L1166-1227 hands one nodeid to each idle worker; run() L1919-1965 spawns the whole pool first, then dispatches one per worker. No affinity/anti-affinity/grouping concept exists on any ref (git log --all and git grep across all branches for affinity/loadscope/co-locat: only the ticket's own chore commits). This is not an omission: the design spec docs/superpowers/specs/2026-09-01-aitest-design.md §2 lists "loadscope/loadgroup-equivalent fixture-affinity grouping" as an explicit v1 non-goal — "v1 is a flat dynamic queue; add affinity only if a real suite needs it" — and §3.5 repeats "No affinity grouping in v1". None of the intervening changes (AIRA-64 pool growth via _maybe_grow_pool, AIRA-101/102/103, etc.) touched dispatch order.

The one thing the ticket left "not yet nailed down" — why aitest would co-locate same-module siblings MORE than xdist `load` — I pinned from source, and it inverts the ticket's premise. pytest-xdist 3.8.0 (fastest-ee's venv, tools/correctness/.venv/.../xdist/scheduler/load.py): LoadScheduling.schedule() sends "batches of consecutive tests" (initial chunk = items_per_node // 4, contiguous via _send_tests popping pending[:num]); check_schedule() refills each node with further CONTIGUOUS chunks; round-robin spreading happens ONLY when len(pending) < 2*len(nodes) (tiny suites). So xdist `load` co-locates collection-adjacent tests far MORE strongly than aitest's one-at-a-time pull (which puts adjacent items on the same worker only ~1/N of the time). What actually differs is determinism: xdist's contiguous partition is fixed for a given (collection, N) — fastest-ee's `-n auto` on the same box — so the device_db leak either always fires or never, and it never did purely because the chunk boundaries happened to split the leaking pair; aitest's pull partition depends on run-time timing, so the pair lands together on some runs → non-deterministic flake. "Anti-affinity dispatch" therefore cannot "restore xdist load's spreading behaviour", because xdist load does not spread. The AIRA-side candidate is built on a false premise, and loadscope-style grouping was already retracted by the ticket itself.

Root cause is a fastest-ee test-fixture defect, tracked there: BL-979 in fastest-ee origin/master docs/backlog.md ("Reset the shared device_db/corpus cache between co-located tests"), P2, status `queued`, filed 2026-09-04, no fix commit on origin/master (only docs commits 646febdc1/5829d6c8e reference it; the local fastest-ee master is 163 commits behind origin/master, which is why a naive grep misses it). That is the ticket's own agreed primary disposition, and it is not AIRA's turf.

Why this meets the project bar rather than dodging it: CLAUDE.md says AIRA is primitives, not judgement, and the owner's HARD architectural-simplicity rule says prefer "keep the primitive + document the gap" over new machinery — the gap is already documented in the spec's §2 deferral. A suite with a shared-mutable-cache leak does not "need" affinity; it needs the cache reset, and any dispatch-policy mitigation in aitest would mask a real isolation defect (exactly the flaky-quarantine outcome speed flagged as unwanted). Nothing to build on the AIRA side; close with a resolution note that (a) points at fastest-ee BL-979 as the live tracker, (b) records the xdist contiguous-chunk mechanism above so nobody re-derives the inverted hypothesis, and (c) notes that if BL-979 ever proves intractable, the right AIRA-side primitive to file fresh is a per-worker execution-order trace for reproducing co-location failures — an observability primitive — not an anti-affinity scheduler.


*Disposition: Closed — not needed, reached via a source-verified triage pass (Fable model) as part of the backlog-completion push, independently spot-checked by the coordinating session before closing.*
