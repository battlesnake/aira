---
{"schema":1,"id":"AIRA-64","project":"aira","title":"aitest per-worker RAM containment holds under contention, but heavy tests can spuriously hit their own pytest-timeout","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","dogfood","scheduler"],"hold":false,"relations":[]}
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

## ESCALATED — direct owner request via peer session `money`, severity raised P2→P1 (2026-09-04)

Relayed as the owner's own words: *"this box is busy and contention is to
be expected... perhaps aitest can deal with this to prevent contention from
timing things out."* Not a suggestion to defer further — a direct request to
build a fix. **This overrides tonight's Fable-sweep-derived backlog
remediation plan's disposition for this ticket** (`docs/superpowers/plans/
2026-09-04-backlog-remediation-plan.md` currently has AIRA-64 as "close with
a one-line doc note, do not build" — that disposition is now superseded by
this direct request and needs updating in the plan once this is scoped).

### Fresh, more severe repro (money, fastest-ee `money-cutover` branch, same night)

`make merge-gate` ran concurrently with another session's (`bl918-any-
boundary`) full merge-gate on the same box. Under that contention the
**engine leg took 82 minutes** (1:22:43; normally a few minutes) and
reported **44 "NEW FAILURES"** spread across totally unrelated engine areas
(copper/geometry/measure/tool_registry/frontdoor/standards_band) — in code
money's own diff provably does not touch (`tools/correctness` doesn't import
`contracts.money`). Classic phantom-timeout-flake signature: money's own
diagnosis was that admission-wait time might be counted against the
per-test timeout.

### Mechanism, corrected against source before any fix is scoped

Grepped all of `internal/pylib/aitest/*.py` for any interaction with
`PYTEST_TIMEOUT`/pytest-timeout: **none exists.** Every "timeout" reference
in aitest's own code is AIRA's own internal admission/relay/placement-ack
timeout machinery, not the third-party `pytest-timeout` plugin fastest-ee
configures per-test. pytest-timeout's own clock starts when a test's
`pytest_runtest_call` fires — i.e. once a worker has already been dispatched
the test and begun running it — not while the test sits in aitest's
admission/dispatch queue. **So money's specific "admission-wait counted
against the timeout" hypothesis is very likely not the literal mechanism**,
though the underlying concern (contention → spurious pytest-timeout
failures) is real and already documented above from `split`'s repro.

**What almost certainly IS the mechanism, and matters more:** `pytest-
timeout`'s deadline is **wall-clock**, not CPU-time. Confirmed via source:
aitest has **zero CPU-concurrency governance of its own** — the only CPU/RAM
cooperative governor in this codebase lives in `internal/pylib/
aira_xdist_governor/__init__.py` (the OLD, xdist-only system), and AIRA-92's
own investigation already established that forked aitest workers
**permanently disable** that governor via `os.register_at_fork` — aitest's
`worker-admit` path governs RAM only. So when two full merge-gates (or more)
run concurrently, their aitest worker pools freely oversubscribe the CPU
against each other with **no cross-process coordination at all** — a test
that normally needs ~2s of CPU can take 60+ real wall-clock seconds under
severe CPU starvation while still making genuine progress the whole time,
and a wall-clock timeout has no way to distinguish that from a real hang.
This is the same class of "false signal under contention" this project has
spent tonight hunting elsewhere (AIRA-91's exit-137/oomd, AIRA-94's
isolation flakes) — here manifesting as false RED rather than false GREEN,
but the same root shape: a wall-clock/black-box signal that can't
distinguish "broken" from "starved."

### Options, corrected against this mechanism (money's own ranking, reworded)

1. **Give aitest workers CPU-concurrency governance, analogous to what the
   old xdist governor provided but adapted to aitest's fork model** — the
   real, durable fix. A machine-wide slot mechanism bounding total
   concurrent CPU-heavy aitest (and other) workers to roughly the core
   count would directly prevent the oversubscription that causes wall-clock
   timeouts to misfire, rather than working around the symptom. Real,
   substantial engineering — comparable in scope to a Phase-1-class
   structural fix, not a quick patch. (Money's original option 1, "don't
   count admission-wait against the timeout," is folded into this: if it
   turns out there IS a real admission-wait-counts-against-timeout
   component once this is actually built and measured, fix that too, but
   evidence so far points at CPU starvation during execution being the
   dominant mechanism.)
2. **Scale per-test/per-leg timeouts by measured load** — a load-factor
   multiplier on `PYTEST_TIMEOUT` under measured contention. Workable
   mitigation, cheaper than (1), but is judgement layered on a
   telemetry-ish signal rather than a structural fix — the class of thing
   `architectural-simplicity` is normally wary of, though the owner's own
   direct request here may justify it as an interim measure while (1) is
   designed.
3. **Serialise/queue heavy gates at the slice level** — reduces how often
   severe oversubscription happens at all, but doesn't fix the underlying
   wall-clock-vs-CPU-time mismatch, and cuts against the whole point of an
   admission system that's supposed to enable safe concurrency rather than
   forcing serialization.

Not scoped or built yet — this ticket is being handed to a dedicated plan
once the currently-running large structural-fix execution effort has
enough bandwidth freed up. Given the direct owner escalation, this is now
explicit next-priority work, not backlog-sweep-disposed-of chore work.
