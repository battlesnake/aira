---
{"schema":1,"id":"AIRA-64","project":"aira","title":"aitest per-worker RAM containment holds under contention, but heavy tests can spuriously hit their own pytest-timeout","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","dogfood","scheduler"],"hold":false,"relations":[]}
---
## Finding (live data, reported by peer session `split`, fastest-ee project)

`make merge-gate` run under `aira confine --delegate-ram --memory-reserve 512M` against a heavily contended slice (23 admitted jobs, MemAvailable ~22.9GB at the time). The engine leg (aitest-covered: `-p aitest -n 0 --aitest-workers=auto`) produced **10 pytest-timeout (>300s) failures** — all heavy OHW-corpus tests (bl848 gap-calibration, copper geometry/si, footprint mirror ×2, vds calibration) that normally pass in ~90s each on a quiet slice.

## What this does and doesn't mean

**aitest's per-worker RAM containment held correctly** — no OOM, no crash, nothing incorrect. This is not a correctness bug in aitest or in tonight's AIRA-30/31 work.

**What it reveals**: aitest bounds per-worker RAM, but has no mechanism to account for CPU/RAM *throughput* contention against a heavy shared slice. A worker that would normally finish a test in ~90s can take longer than that when starved for CPU/memory-bandwidth by other admitted jobs — and pytest's own test-level timeout (300s in this case) doesn't know or care that the slowdown is external contention rather than the test hanging, so it fails the test as a timeout. The failure is real (the test genuinely didn't finish in time) but the CAUSE is machine load, not a defect in the code under test or in aitest's RAM governor.

## Why this matters going forward

This means "just use `--delegate-ram`, it's efficient and safe" (the guidance given to fastest-ee tonight for aitest-covered targets) is accurate for RAM-safety but incomplete for wall-clock reliability under contention — a heavy gate can still want a quiet slice, or contention-aware timeouts, even on aitest-covered legs. `split`'s own interim takeaway: run heavy corpus gates on a quiet slice for now.

## Suggested direction (not scoped/designed — a signal for future scheduler work)

Not an immediate fix — flagged as input for whatever the next slice-scheduling milestone is, per split's own framing. Possible directions worth considering when that work is picked up: aitest-side awareness of slice contention level (could inform an internal soft-timeout multiplier, or surface a warning rather than a bare failure when a timeout occurs under measurably high external load); or purely a documentation/guidance fix (heavy/corpus-style test suites should prefer running when the slice is quiet, or get longer timeouts, independent of any aitest/AIRA change). No implementation attempted here — this ticket exists to record the finding before it's lost, not to prescribe the fix.
