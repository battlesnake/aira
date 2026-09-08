---
{"schema":1,"id":"AIRA-160","project":"aira","title":"aitest worker-count sizing is memory-blind (CPU-only auto, and worker-admit is deliberately blind to new post-admission host pressure)","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-160","to":"AIRA-180"},{"kind":"relates","from":"AIRA-160","to":"AIRA-181"}]}
---

Filed from a cross-session peer report (speed, gate owner, co-filing on behalf
of qual), 2026-09-08. The report's own framing was investigated and found
**significantly incorrect on its central claim**; this ticket records the
verified facts and the narrower, real residual gap, per the reporter's own
request ("filing it so it's tracked... not lost as a contention anecdote").

## The peer's claim, verbatim summary

aitest sizes its worker pool from `aira.slice`'s cgroup occupancy while the
Claude Code harness's own low-memory guard reads real host-wide
`MemAvailable`. Uncapped sibling sessions' processes appear in the second
number and not the first, so aitest can conclude the slice has headroom while
the host does not, and the harness guard kills the run. Evidence: a
`make merge-gate` run (not `--delegate-ram`) was killed by the harness's guard
mid-run at `free -g` = 78 total / 57 used / 20 available, with several other
sessions' uncapped python at 1.3-2.5 GB RSS each. The reporter's own honest
caveat: the confine banner showed "no aitest line", so this run was the
**plain-xdist fallback path**, not aitest itself.

## Verified against source (not assumed)

**aitest is not a separate project.** `internal/pylib/aitest/` is AIRA's own
in-tree pytest plugin, on `master` right now (introduced by AIRA-33). It is
documented as such in `README.md:97` and `internal/core/skill.go:320`.
`pytest_worker_flags.sh` is not in this repo -- it is referenced only in
ticket prose as an external downstream wrapper script that decides whether to
invoke aitest at all.

**The central claim is wrong: slice-level admission is not blind to
host-wide memory.** `internal/daemon/sliceceiling.go:236-260`
(`sliceCeilingDesired`) computes `affordable = MemAvailable + sliceAnon -
freeMin` specifically to throttle `aira.slice` for memory consumed OUTSIDE
the slice, and `admit.go:2436-2443` wires this into the admission gate every
new `aira confine` job passes through. A job entering the slice already has
host-wide pressure from uncapped siblings taken into account.

**AIRA already exposes real host-wide MemAvailable client-facing** -- a
consumer wanting it does not need to read `/proc/meminfo` itself:
`internal/daemon/confine_manage.go:256,270-271` publishes it as
`SliceReserve.SystemMemAvailableBytes` / `Ceiling.MemAvailableBytes`
(`system_mem_available_bytes` / `mem_available_bytes` in `aira confine --list
--json` and the `aira_confine_list` MCP tool), and `aira confine --list`'s
human text and `aira top`'s system RAM bar both already render it.

**The specific cited incident did not involve aitest at all.** By the
reporter's own evidence, it was the plain-`xdist -n auto` fallback -- which
has zero memory awareness of any kind, by `pytest-xdist`'s own design,
entirely outside AIRA. That kill demonstrates the fallback path has no
memory awareness (true, but unsurprising and not an AIRA defect), not that
aitest's own sizing is blind.

## The real, narrower gap that survives verification

`--aitest-workers=auto` sizes purely off CPU count
(`internal/pylib/aitest/README.md:9`) -- not memory at all. Separately,
`evaluateWorkerAdmit` (`internal/daemon/worker_admit.go:454`) gates a
worker's admission on "the OUTER scope's own live memory.current, read
directly" (comment at line 436) -- deliberately, per
`internal/daemon/sliceceiling.go:730-739`, which states the slice-pressure
ceiling must NOT also reach worker-admit, "which is keyed by an aitest job's
OUTER SCOPE rather than by the slice... throttling its workers would charge
the same pressure twice."

So: once an aitest job's outer scope is admitted (already correctly weighed
against host-wide pressure at that moment), its own worker-pool sizing is
ledger-only against that job's own reservation and is blind to *new*
host-wide pressure appearing after admission -- e.g. an uncapped sibling
session spiking mid-run. This is a real, documented, **deliberate** design
choice (avoiding double-charging the same pressure), not an oversight.

## Resolution (2026-09-08)

Closed without a code change. The central claim (slice admission is blind to
host-wide load) is false -- verified from source, not merely reasserted. The
one real residual gap (worker-admit's mid-run blindness to newly-appearing
host pressure) is an existing, deliberate, already-justified tradeoff in the
codebase, not a defect to fix reactively at the end of a long session without
its own design work -- changing it risks reintroducing the double-charging
problem the current design explicitly avoids.

If this gap is ever worth closing, the shape is: `--aitest-workers=auto`
(and/or a downstream wrapper like `pytest_worker_flags.sh`) should consult
the ALREADY-EXPOSED `system_mem_available_bytes` (`aira confine --list
--json` / `aira_confine_list`) when sizing, rather than AIRA inventing a new
primitive -- the data consumers need already exists. That is a genuine
two-loop sizing decision (worker count vs. host pressure) for whoever picks
it up, not a documentation gap.

Communicated back to the reporting peer (speed) directly with this corrected
picture rather than building against a partially-wrong premise.
