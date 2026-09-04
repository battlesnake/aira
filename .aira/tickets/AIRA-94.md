---
{"schema":1,"id":"AIRA-94","project":"aira","title":"aitest has no test co-location/grouping — dynamic FIFO dispatch makes state-leaking tests flake non-deterministically","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","dogfood","reliability"],"hold":false,"relations":[]}
---
Reported by peer session `speed` (Inc-2.6 NF-35 relocation rail work) and independently corroborated by `ems`, both on a QUIET slice (load 8-15) — this is NOT contention/AIRA-91/AIRA-92, it reproduces with no contention at all.

## Symptom

fastest-ee engine + pipeline legs are flaky under aitest: tests pass individually/in isolation, but the SET of tests that fail varies run to run. speed observed run-1 flagging `test_dispatch_coverage` (not `test_copper_si`); run-3 flagging `test_copper_si` (bl286-neutrality) + `test_vr127_corpus_arm_b` + 3x `test_derating_origins` (not `test_dispatch_coverage`). `ems` saw the same class on `pipeline`/`test_live_ingest`. The affected tests are corpus-neutrality / cid-invariant / derating tests — exactly the shape of test that leaks or depends on process-global state (module-level caches, files written but not cleaned up, singletons, etc).

## Root cause, verified against source

`internal/pylib/aitest/supervisor.py` dispatches tests via a single flat FIFO queue with no grouping: `self.queue = [item.nodeid for item in items]` (collection order), and `next_nodeid()` just pops the front of that queue whenever any worker goes idle (`_dispatch_to_idle_workers`). Grepped the whole aitest source for any grouping/pinning/scope-based dispatch concept (the equivalent of pytest-xdists `--dist=loadscope`/`loadfile`) — none exists. Which worker a given test lands on, and which OTHER tests it runs alongside on that same worker, is therefore a function of real-time race timing between workers, not a fixed/reproducible partition.

If a project (fastest-ee here) was previously running under xdist with `--dist=loadscope` or `--dist=loadfile` (grouping all tests in the same module/class onto one worker, preserving their relative order) — worth speed/qual confirming this on the fastest-ee side — then any test that leaks state to a sibling in the same file would have reliably co-located under xdist (deterministic, so appeared stable) but now co-locates only probabilistically under aitests plain dynamic dispatch. This exactly matches the reported symptom: passes in isolation, flaky set under aitest, previously stable under xdist.

## Why this matters beyond one leg (speed)

1. Makes merge-gate NEW-FAILURES base-red triage unreliable — a non-deterministic flake is not reliably in the base-red manifest, so it reads as a false NEW failure.
2. Will bury the signal of NF-35s relocation rail (forces the full engine leg on every git-mv relocation → these flakes read as false NEW on every relocation PR).
3. General reliability concern for any parallel-leg executor built on aitest.

## Fix-path options, not decided here

1. **Root fix**: find and fix the state-leaking tests themselves (correct long-term, real effort proportional to how many tests are affected — unknown until surveyed).
2. **Architectural fix in aitest**: add an optional grouping/co-location dispatch mode (module/file-scoped, similar to xdists loadscope/loadfile) so aitest can restore whatever grouping a migrating project relied on, without requiring every leaking test to be fixed first. Real engineering, but directly closes the gap this ticket documents and would generalize to any future project migrating off xdist with the same latent assumption.
3. **Cheap short-term mitigation**: pin specific known-flaky files/tests to a single worker (no such mechanism exists in aitest today either — would need building, smaller than full grouping).
4. **Flaky-quarantine**: fastest-ee-side only, does not fix the underlying signal-reliability problem and delays the real fix — explicitly flagged by speed as an unwanted default.

No investigation beyond this source read has been done yet — this ticket exists so the finding is not lost, not as a resolved diagnosis.
