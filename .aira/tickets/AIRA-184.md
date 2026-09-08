---
{"schema":1,"id":"AIRA-184","project":"aira","title":"A job OOM-killed marginally over its own ESTIMATED (non-pinned) cap has no in-run remedy, even with slice headroom to spare","status":"done","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","estimator"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-184","to":"AIRA-186"}]}
---

Peer report (split, 2026-09-08), refined after their own initial "zero
headroom" framing was checked and withdrawn — the 1.15x safety factor does
apply on the `estimate:p90-prior` path same as every other estimate path;
that part is not a bug. The refined finding is narrower and survives
verification.

## Verified from source

Split's arithmetic on the captured line: cap `1257889792` B, peak-rss
`1257902080` B — the job exceeded its cap by ~12 KiB (0.001%), i.e. the
1.15x-derived estimate was very nearly exactly right, and the job was
killed anyway, twice, on the same command shape, with ~37 GiB free in the
slice. `estimate:p90-prior` is confirmed genuinely machine-wide, not
per-command (`internal/daemon/admit.go:2936`, `internal/core/skill.go:324`:
"the machine-wide p90, which exists only once some signature in this
box's one shared state.db has three or more recorded peaks") — it is the
deliberate cold-start fallback for a signature with no history of its own
yet, distinct from `EstimateMemoryReserve`'s own per-signature
`estimate:max=N,n=S,f=115` path (`internal/runner/resource_estimate.go
:83-124`), which already accumulates history per `ResourceSignature`
(exact argv, `resource_estimate.go:66-79`) — so the "should this be
per-command instead of machine-wide" half of split's hypothesis does not
apply to the escalated path; it only ever applies to a genuinely
never-before-seen signature.

skill.go:324 already documents the exact failure mode split hit as
expected, self-healing-ACROSS-RUNS behaviour: "a job OOM-killed AT that
cap... its own measured peak is then already at what the slice can give:
such a re-run is refused E_ADMIT_TOO_LARGE... rather than retried." If
split's repeated pytest single-test-probe invocations vary by test node ID
(and therefore by argv, and therefore by `ResourceSignature`), each is a
genuinely distinct, never-before-seen signature that independently
cold-starts at the same shared p90-prior and can independently OOM at it —
consistent with, not contradicting, the documented design. This has not
been independently confirmed against split's actual argv.

## The narrower gap that survives

The documented self-healing is CROSS-RUN only (the next IDENTICAL
invocation is admitted higher). Nothing helps the SAME run that just died
marginally over an estimated, non-pinned cap while the slice has spare
capacity to grant a larger one immediately. Split's own framing, offered
without a recommendation: "a policy of 'on OOM at an ESTIMATED (not
user-pinned) cap, when the slice has headroom, retry once at a higher
cap' would have turned two dead runs into two slow ones."

## Not designed here — and a note on scope, for whoever gates this

An automatic in-place retry is new runtime machinery (new state, new
launch path, new safety bounds — retry-once-only, estimated-cap-only,
headroom-gated) and should be weighed against this project's own
architectural-simplicity preference for "keep the primitive, document the
gap" over stacking complexity per feature. A cheaper alternative that
stays within that preference: no retry at all, just a sharper
`terminated-by=oom` line for this specific case — cap-source names this
was an ESTIMATE not an operator pin, and the message could say so plainly
("killed at auto-estimated cap; rerun with --memory-reserve <higher> — the
slice had headroom") rather than requiring the reader to infer it, closer
to split's own suggested wording. Whether the auto-retry is worth building
at all, or whether the message-only version is the right stopping point,
is left for whoever picks this up.

## Resolution — MERGED

PR #114 merged as `dfa0114` (merge commit; branch head `794b11e` on
`19c6e58`). Message-only, per "Not designed here" above: the auto-retry is
NOT built (architectural simplicity — keep the primitive, document the gap).
Admission, estimation and the launch path are untouched; one advisory string
changes. The `cap-source=auto:daemon-reserve` OOM line now says the cap was
AIRA's own AUTO-ESTIMATE, names the slice's own cap (`ConfineStatus.CapBytes`,
established by `deps.readCap` before launch; `unevaluated` when 0, never
assumed roomy), and hands over a concrete `--memory-reserve` pin (1.5x the
larger of cap and peak, the daemon's own `estimate:oom-escalated` factor,
rounded up to a whole MiB, floored at 1 MiB). Kill-time slice occupancy is
deliberately neither measured nor claimed.

## Review (Fable build-review gate) — one-round BLOCK-then-fix

### Round 1 (`19c6e58`): confirmed BLOCK

The no-pin branch (`1.5 x max(cap, peak) >= sliceCap`, i.e. every peak at or
above TWO-THIRDS of the slice cap) asserted that the identical re-run "is
refused E_ADMIT_TOO_LARGE rather than run" and dropped the re-run advice with
it. That generalised skill.go's narrow documented rule (an OOM AT what the
slice can give) to a much wider condition that does not imply refusal: the
daemon's AIRA-151/153 clamp (`internal/daemon/admit.go`,
`fit > 0 && stats.MaxOOMPeak < fit && reserve > ceiling`, with
`FIT(c) = floor(c*100/115)`) clamps an unpinned over-ceiling escalation DOWN
to FIT and admits it for every recorded OOM peak under FIT. Counterexample
taken from the daemon's own fixture
(`internal/daemon/confine_admit_test.go`, `TestConfineEstimatorAndOOMEscalationClamp`,
signature `oom`): 40G peak, ceiling 55G (56G cap minus 1G headroom) -> 60G
escalation clamped to 51352869843, basis
`estimate:oom-escalated,ceiling-clamped`, and RUN. The original boundary row
(slice cap 1M) was degenerate — `SliceFittedReserve` returns 0 below
`MinPinnedScopeCap`, so it never reached an admittable case and could not
have caught this.

### Round 2 (`794b11e`): fix re-verified from source, not from the fix report

- Wording now withdraws only the PIN (the one thing the comparison
  establishes), keeps RE-RUN THE IDENTICAL COMMAND in every daemon-reserve
  outcome, and states refusal as the conditional it is — admission fits the
  next run under the slice cap where the recorded peak leaves room and refuses
  it E_ADMIT_TOO_LARGE only where that peak is already at what the slice can
  give — naming `required` and `cap_minus_headroom`, which `admit.go`'s
  refusal message genuinely emits.
- Reviewer RED: the pre-fix `confine_linux.go` swapped into a throwaway
  worktree at `794b11e` -> exit 1; the new 40G/56G table row emitted the OLD
  refusal wording (so it genuinely reaches the previously-buggy branch), and
  `TestEstimatedCapOOMAdvisoryNeverAssertsARefusalItCannotEstablish` failed on
  exactly the 2 of 4 branch-side sub-cases (discriminating, not vacuous).
- Reviewer mutation: refusal clause re-added while keeping the re-run phrase
  -> exit 1 on the not-contains assertion (line 794) only. Both assertions
  are independently load-bearing.
- Gates on the MERGED tree (`794b11e` + `b186877`; master then advanced only
  by `0c6e0b8`, two ticket files, no code), all under `aira confine`:
  gofmt 0 (no files), `go build ./...` 0, `go vet ./...` 0,
  `go test ./... -count=1` 0 — 14 packages ok, 0 FAIL, no E_ADMIT_SATURATED.
- Non-blocking, fixed in place: the PR body's mutation table named a test row
  the fix had renamed; corrected via the pulls API.

### ACCEPTED GAP (recorded, not fixed; raised non-blocking in round 1)

The pin the room branch offers is checked against the slice's whole CAP, so
a suggestion landing in (cap - headroom, cap] is one admission refuses once
it is PINNED (a pin takes the `pinned:client` arm and gets no fitting).
Narrowing it would mean reproducing the daemon's headroom term client-side,
which is exactly the unestablished arithmetic the advisory exists to refuse.
The reader is not stranded: the daemon's own E_ADMIT_TOO_LARGE names
`required` and `cap_minus_headroom`, so the correct pin is on the very next
line they see. Written into the branch comment and the PR body rather than
left silent.

### Deferred

The auto-retry itself (weighed and declined, see Resolution). Kill-time slice
occupancy (not established at teardown; a post-kill read measures a slice
this job's memory has already left).
