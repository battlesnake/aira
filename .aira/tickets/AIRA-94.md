---
{"schema":1,"id":"AIRA-94","project":"aira","title":"aitest has no test co-location/grouping — dynamic FIFO dispatch makes state-leaking tests flake non-deterministically","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","dogfood","reliability"],"hold":false,"relations":[]}
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

Still pending: qual's controlled fixed-commit experiment to confirm the trigger (both prior observations were cross-commit, so the exact causal link isn't fully pinned yet). No AIRA-side investigation beyond source reading has been done — this ticket exists so the finding is not lost, not as a resolved diagnosis.
