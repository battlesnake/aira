---
{"schema":1,"id":"AIRA-133","project":"aira","title":"confine's kill trailer doesn't distinguish an auto-estimated cap from a user-supplied --memory-max","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","estimator","honesty"],"hold":false,"relations":[]}
---
Requested by peer session 'field', 2026-09-06, as a follow-up on a real (though likely
cold-start-class, not a formula bug) OOM report: a cargo build sub-target's auto-estimated reserve
was ~30MB against a real compile-path peak of ~315MB, correctly contained but confusingly reported.

## The gap

When a job is OOM-killed in-scope, `aira confine`'s trailer reports `terminated-by=oom` and the
reserve/reserve-basis that was granted, but does not distinguish WHY that reserve was what it was:
- an AUTO-ESTIMATED cap (reserve-basis starting `estimate:...`, sized from
  `EstimateMemoryReserve`'s historical-max-plus-margin formula, internal/runner/resource_estimate.go)
  that may simply not have seen this workload's real peak yet (a cold-start/first-occurrence gap,
  which self-heals on the NEXT run of the same signature per AIRA-128's investigation this session);
  versus
- a user-SUPPLIED `--memory-max`/`--memory-reserve` that the operator explicitly chose and that is
  simply too small for the real workload.

These are different failure modes wanting different responses: the first says 're-run the identical
command, it should self-correct now that this peak is recorded' (or, if it doesn't self-correct,
that IS worth a bug report); the second says 'the operator's own explicit limit needs raising'.
Today both look identical in the trailer/failure output, which cost a real debugging cycle for
field's session (a kill after a source edit reads as flakiness, not as an estimate that hadn't seen
this code path yet).

## Fix

Add a clear signal to the trailer/report (and wherever `aira confine`'s failure surfaces —
CLI stderr, --status, MCP envelope) distinguishing an OOM against an auto-estimated cap from an OOM
against an explicit operator-supplied limit. The reserve-basis field already encodes this
distinction internally (`estimate:*` vs `pinned:client` vs an explicit flag-derived value per
internal/daemon/admit.go's resolveAdmitReserve) — surface it plainly rather than requiring the
reader to decode the reserve-basis string themselves. For the auto-estimate case specifically,
consider naming the concrete next step: 're-run the identical command; if it still under-provisions,
that is a genuine bug' — matching AIRA-128's newly-added Skill-guide prose about
terminated-by=oom/exit 137 (internal/core/skill.go), which this should probably sit beside rather
than duplicate.

## Tests

A regression test proving: a kill against an auto-estimated cap and a kill against an explicit
--memory-max produce distinguishably different trailer/report text, and the distinction is derived
from the actual reserve-basis/provenance data (not guessed), verified by injecting both cases
against a real or simulated OOM.
