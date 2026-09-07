---
{"schema":1,"id":"AIRA-133","project":"aira","title":"confine's kill trailer doesn't distinguish an auto-estimated cap from a user-supplied --memory-max","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","estimator","honesty"],"hold":false,"relations":[]}
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

## Resolution

Adds `ScopeMemoryCapSource` to `ConfineStatus` (`internal/runner/confine.go`), a
new provenance vocabulary (`operator:--memory-max`, `operator:--memory-reserve`,
`auto:daemon-reserve`, `auto:delegate-ram`, `unevaluated`) written by the SAME
branch in `confineWithDeps` (`internal/runner/confine_linux.go`) that CHOOSES
the cap — never re-derived downstream by decoding reserve-basis or
pattern-matching a byte count, so the provenance and the number can never
disagree. Rendered on the trailer as `cap-source=` beside
`scope-memory.max=enforced=N` (always present when a cap is enforced, absent
when it is not — the same always-rendered discipline as `terminated-by`).
`formatConfineReserveAdvisory` now names a concrete, source-specific next step
on OOM: an operator-supplied cap says re-running will not help and names the
flag to raise; an AIRA-chosen cap says to re-run the identical command (the
kill has been recorded against the signature and the next admission is sized
higher) and names a genuine-bug threshold if that still fails. Also updated
`internal/core/skill.go`'s existing `terminated-by=oom` guidance to point at
`cap-source=` rather than leaving the reader to decode `reserve-basis=`
themselves.

**Tests**, all real, none simulated:
- `TestFormatConfineStatusRendersCapSource` / `TestConfineCapSourceIsOperator`
  (`internal/runner/confine_cap_source_test.go`): the four sources render as
  four DIFFERENT trailer strings (a discriminating assertion, not just
  presence), the no-cap case carries no `cap-source=`, and an unrecorded
  source on an enforced cap reads as `unevaluated`, never either party's
  choice.
- `TestFormatConfineReserveAdvisory` (extended,
  `internal/runner/confine_linux_test.go`): the same cap/peak/kill produces
  four different next-step messages keyed only on the recorded source.
- `TestOOMTrailerDistinguishesAnEstimatedCapFromAnOperatorSuppliedOne`
  (`internal/daemon/oom_cap_source_real_cgroup_linux_test.go`): against a
  REAL cgroup, a real daemon and a real kernel OOM kill — two workloads killed
  the same way, at caps of the same order, report distinguishably different
  provenance and are told to do different things, asserted in both
  directions (the auto leg's stderr must say re-run and must NOT say "your
  own flag"; the operator leg the reverse). A fourth real launch covers
  `--memory-reserve` as its own branch on the launch path, not assumed to
  follow `--memory-max`.

Gate: `aira confine -- go build ./...` exit 0; `aira confine -- go vet ./...`
exit 0; `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` exit 0
except a **pre-existing, unrelated** flake in `internal/daemon` — filed as
**AIRA-139** — where `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission`
(AIRA-128, committed before this ticket) fails against ITSELF at `-count=2`
with zero other tests involved. Investigated at length before concluding this:
shrinking this ticket's own new test 5x and adding a host-MemAvailable
settle-wait both had ZERO effect on the failure, which is what proved it was
not a resource-contention side effect of this ticket's real OOM churn. AIRA-139
records the full reproduction and next-step guidance; not fixed here, as it is
squarely AIRA-128's own fixture, out of this ticket's scope.

## Review record (Fable build-review, 2026-09-07) — MERGE, PR #86 → `91e7992`

Verified from source, not from the PR narrative:

- `ScopeMemoryCapSource` has exactly one writer
  (`internal/runner/confine_linux.go:1008`, inside the `scopeMemoryMax > 0`
  block that writes the cap), fed by a `capSource` local that is assigned
  only in the four branches that set `scopeMemoryMax`; readers are
  `FormatConfineStatus` and `formatConfineReserveAdvisory` only. Nothing
  decodes `reserve-basis` or a byte count. `--memory-max` under
  `--delegate-ram` keeps `operator:--memory-max`, matching the "explicit cap
  wins" precedence already in that branch.
- Trailer: `cap-source=` is emitted only inside the `enforced=` else-branch
  and defaults to `unevaluated` when empty (`confine.go:734-738`). The shim
  launch path never writes `ScopeMemoryMax`, so it cannot render a
  source-less cap; the detached `--status` record serialises
  `*ConfineStatus` as JSON, so the field flows through unchanged.
- Advisory: the four messages are distinct and correctly paired (operator
  sources say re-running "will not change it", auto sources say re-run); the
  delegate-ram claim that the ceiling "climbs with this signature's recorded
  peaks" matches `resolveDelegateRAMScopeCeiling` (`admit.go:1550-1554`).
- Real-cgroup test: three real `memory.events` OOM kills (auto at the p90
  prior, `--memory-max`, `--memory-reserve`), asserted in both directions on
  stderr; passes at `-count=2` against itself (0.43s/0.35s), so it is not
  itself subject to AIRA-139.

Gate, all exact exit codes, run in a detached worktree at `2130218`:
`aira confine -- go build ./...` 0; `aira confine -- go vet ./...` 0;
`AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` 0 (14 packages
ok, 0 FAIL); `internal/daemon` alone twice more, 0 and 0 (78.4s, 90.6s).

AIRA-139 independently reproduced on this box: the AIRA-128 test at
`-count=2` passes iteration 1 (4.71s) and fails iteration 2 at line 211 with
`E_ADMIT_SATURATED` after the full 30s, "queue position 1 of 1, 0B queued
ahead", exit 1 — exactly as the ticket records. Phase 2 (`p90-prior`) still
passes in iteration 2, so the per-test state is fresh and the fragility is
specific to the `oom-escalated` phase-3 admission on a second `Server` in the
process. The filename-ordering mitigation is accepted as an interim,
documented workaround: AIRA-128's file is alphabetically the first file in
the package that calls `startServer` at all, the mitigation touches no
AIRA-128/AIRA-139 file, and AIRA-139 stays open for the real fix. Known limit:
`go test -shuffle=on`, or any future file sorting before
`confine_oom_selfheal_...` that starts a real Server+slice, re-exposes it.

Dogfood with the PR binary as client against the live daemon: an auto-capped
2 GiB workload was killed at the 1.2 GB `p90-prior` cap with
`cap-source=auto:daemon-reserve` and the re-run advisory (exit 137); the
identical re-run was admitted at `estimate:oom-escalated` (4G) and completed
(exit 0); `--memory-max 128M` and `--memory-reserve 128M` each rendered their
own `operator:` source and the "re-running will not change it" line (137).

Accepted wording nits, not blocking (the actionable half of each line is
correct and `reserve-basis=` sits beside it on the trailer):
- the `auto:daemon-reserve` advisory says "from this command's peak-RSS
  history", which over-specifies when the basis is the machine-wide
  `estimate:p90-prior` or a daemon fallback — the constant's own doc comment
  already concedes this;
- "The kill has now been recorded against this command's signature" is
  best-effort: `reportPeak` (`confine_linux.go:1294`) discards its error on
  a 250ms timeout, so the line asserts a recording it cannot always confirm.
