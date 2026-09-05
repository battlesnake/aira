---
{"schema":1,"id":"AIRA-34","project":"aira","title":"confine scope-integrity reports `migrated` for legitimately-nested sub-scopes (leaf-only check)","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["accepted-gap","aitest","confine","deferred","telemetry"],"hold":false,"relations":[]}
---
Found during the aitest bootstrap-sequence spike (2026-09-01, ~/tmp/aitest-bootstrap-spike.sh): when a confined process relocates itself into a CHILD cgroup of its own scope (e.g. aitest's supervisor moving into `outer/.aira-supervisor` before enabling subtree_control + forking per-worker sub-scopes), confine's scope-integrity facet reports `scope-integrity=migrated` instead of `contained`. The process is still WITHIN the scope subtree (genuinely contained) — the check (#20/#70 ScopeContained attestation) verifies membership of the exact scope LEAF, so any move into a descendant cgroup reads as a migration/escape.

IMPACT: telemetry-only. Per #70, ScopeContained has 0 production consumers — nothing functional breaks; the only cost is that every aitest outer scope (and any future legitimately-nested confine job) reports `migrated` forever, which is misleading noise in the trailer.

DECISION (Opus + aitest, 2026-09-01): **DOCUMENT, do NOT build.** aitest documents `migrated` as its expected outer-scope status. A subtree-aware integrity check (a pid in ANY descendant cgroup of the scope = still contained) would be MORE correct, but per the architectural-simplicity rule (HARD, owner) telemetry-only signals never justify new machinery — "keep the primitive + document the gap" beats new code, exactly the #70 lesson. relates: aitest (nested per-worker sub-scopes make this the norm), #20 (descendant-escape attestation), #70 (ScopeContained telemetry-only, sampling gap accepted).

BUILD ONLY IF: a real production consumer of scope-integrity emerges AND nested sub-scopes become common enough that the `migrated`-noise is actually harmful. Until then: deferred/accepted-gap.

## Re-scoped (2026-09-04, backlog-remediation Phase 0, plan section 2) — one factual correction, decision unchanged

**The ticket's IMPACT claim is stale and is corrected here: "ScopeContained has 0
production consumers — nothing functional breaks" is no longer true.**

There is exactly one production consumer, on one path:
`admissibleScopeIntegrity` (`internal/store/gate_command.go:223`) admits a
command gate's evidence only when scope-integrity is `contained` or `unverified`.
A run reporting `migrated` — which per this ticket is what EVERY legitimately
nested confine job reports, aitest's outer scope included — therefore has its
evidence rejected as inadmissible (`gate_command.go:231`). That is a functional
consequence, not telemetry noise.

It is nonetheless **latent, not live**: a fresh read-only count of
`~/.local/state/aira/state.db` (2026-09-04, recorded in the backlog-remediation
plan section 5 item 2) shows `gates`, `gate_results`, `gate_proofs`,
`gate_attestations`, `gate_baselines`, `gate_baseline_active`, `test_reports` and
`test_report_results` all EMPTY. No gate has ever run in production here, so no
evidence has ever been rejected by this path.

**The DECISION is unchanged: document, do not build.** The architectural-
simplicity rule still applies, and a subtree-aware integrity check is still new
machinery for a consequence nothing currently exercises. What changes is the
BUILD-ONLY-IF trigger, which is now concrete rather than hypothetical: build it
when a gate is actually configured in a project that also runs nested confine
scopes — at that moment the gate's evidence starts being rejected, and this stops
being latent.

(The plan's row for this ticket says to "correct the ticket's stale line
references". There are no line references in the body to correct — it cites
tickets, not files:lines. The stale claim above is the correction that was
actually needed.)

## Resolution (2026-09-06)

Built the subtree-aware fix, per the assigning brief: the BUILD-ONLY-IF trigger
above is concrete rather than hypothetical (a gate's evidence rejection is a
real, if latent, functional consequence), and the fix mirrors machinery the
descendant-escape loop already carries in the same function — no new
machinery, per the architectural-simplicity rule.

**What was built:** `monitorScopeMembership`'s `sample()` closure
(`internal/runner/runner_linux.go`) had a leaf-only leader-migration test:
absence from the scope's own `cgroup.procs` while the leader was alive was
unconditionally `summary.LeaderMigrated = true`. Replaced it with the same
subtree-aware witness the descendant-escape loop later in the same function
already uses — `observeProcessCgroup(leader, scope.Reference())` then
`witnessedEscape`/`pathEqualOrUnder` — so:
- absent + alive + unreadable observation → `summary.Gap = true` (honest,
  never a false positive or a false containment claim);
- absent + alive + readable + genuinely outside the scope subtree → still
  `summary.LeaderMigrated = true` (unchanged real-escape behavior);
- absent + alive + readable + under the scope subtree → neither flag is set
  (correctly contained in a nested sub-scope it created itself).

`classifyLaunchScopeIntegrity`'s precedence and error codes were **not**
touched: a genuinely escaped leader still reads `ScopeMigrated` /
`E_RUN_SCOPE_MIGRATION`. The separate leaf-only `initialMigrated` check (the
immediate post-`Start()` observation, `runner_linux.go` ~line 512) was
deliberately left alone — out of scope per the assigning brief; see "open
questions" below.

**PR:** https://github.com/battlesnake/aira/pull/53
**Merge commit:** `cdf48d8334956dc16fe18e6d861b9e11e2ffc8cb` (squash-merged to
`master`; independently re-verified by reading the diff back off
`origin/master` after merge, not just the PR description — full match).

**Tests added:**
- `internal/runner/scope_monitor_linux_test.go`: three fast, mock-driven
  tests calling `monitorScopeMembership` directly —
  `TestScopeMembershipLeaderRelocatesIntoNestedSubScopeIsNotMigrated`,
  `TestScopeMembershipLeaderRelocatesOutOfScopeIsStillMigrated`,
  `TestScopeMembershipLeaderAbsentWithUnreadableCgroupIsGapNotMigrated`. The
  escape-case sibling path deliberately shares the scope directory's name as
  a string prefix (`aira34-leader-nested-scope-sibling` vs.
  `aira34-leader-nested-scope`) to mutation-test that containment is decided
  by `pathEqualOrUnder`'s component-wise `filepath.Rel`, not a naive
  `strings.HasPrefix` a regression could substitute.
- `internal/runner/descendant_escape_linux_test.go`: two end-to-end
  real-cgroup tests through `Runner.Launch` —
  `TestRealCgroupLeaderSelfRelocatesIntoNestedSubScopeIsNeverMigrated` (never
  migrated; tolerates `ScopeContained` or the sampling-load-dependent
  `ScopeUnverified`, mirroring the existing tolerance
  `TestRealCgroupNestedDescendantIsNeverEscaped` already applies to the
  equivalent descendant-side case) and
  `TestRealCgroupLeaderSelfRelocatesOutOfScopeStillReadsMigrated` (still
  `ScopeMigrated` + `E_RUN_SCOPE_MIGRATION`).

**Mutation check performed by hand:** reverted the production fix back to the
old one-line leaf-only test, reran the new tests — the two nested-contained
tests failed exactly as expected (`LeaderMigrated:true` for a genuinely
in-subtree leader) while the two genuine-escape tests still passed, proving
the new tests catch the real bug without the fix weakening the
escape-detection guard. Restored the fix afterward.

**Verification (exact exit codes, `aira confine --`-wrapped throughout):**
- `go build ./...`: exit 0 (multiple runs, including post-rebase)
- `go vet ./...`: exit 0
- `go test ./...` (whole module, `-count=1`, fresh not cached): exit 0, all
  15 test-bearing packages `ok` — run three times total across the two
  rebases (once manually, twice more automatically via this repo's
  pre-push hook), all green
- `go test ./internal/runner/...` alone: exit 0, run repeatedly (including
  back-to-back reruns) with no flake observed

**Review (single code-reading pass per the assigning brief's build-small
classification):** self-review, then an independent adversarial pass by
codex-sol (a different model/lens than the one that built the fix), given the
full diff plus the surrounding `classifyLaunchScopeIntegrity` /
`witnessedEscape` / `pathEqualOrUnder` context to read directly rather than a
paraphrase. Verdict: **APPROVE WITH NITS** (two P2s):
1. *Applied* — strengthen the unit-test sibling path to share the scope's
   directory name as a string prefix (mutation-tests the `filepath.Rel`
   vs. `strings.HasPrefix` distinction; see tests above).
2. *Left as-is, with reasoning* — the two new real-cgroup tests use a fixed
   guard sleep (matching a real dwell period against the sampler interval)
   rather than an explicit synchronization handshake to sequence the child's
   self-relocation after the launch-time immediate membership check. This is
   a pre-existing, accepted characteristic of every other real-cgroup timing
   -based test already in this file (e.g.
   `TestRealCgroupOutlivingInScopeDescendantIsKilledAndAttested`,
   `TestRealCgroupConfineWitnessesSiblingEscape`); no such handshake hook
   exists in the `Request`/`Runner.Launch` API to synchronize against, and
   introducing one would be new machinery for a theoretical, unobserved race
   — out of proportion for a build-small ticket. No flake was observed across
   repeated runs.

**Open design questions from the assigning brief, and how each was
resolved:**
- *"Keep the existing continue (~:1588-1590) so the leader is not
  double-handled in the everMembers loop"* — confirmed unchanged; the diff
  touches only the leaf-only `if` block immediately above it (now at
  ~1567-1583 after the fix; line numbers had drifted slightly from the
  brief's estimate by the time this was picked up, re-verified against
  current source before editing).
- *"Do NOT change classifyLaunchScopeIntegrity's precedence or codes"* —
  confirmed untouched; verified by reading the function in full before and
  after, and by the pre-existing precedence tests
  (`TestClassifyLaunchScopeIntegrityKeepsLeaderMigrationPrecedence` et al.)
  continuing to pass unmodified.
- *Scope boundary — the separate leaf-only `initialMigrated` check
  (`runner_linux.go` ~line 512, the immediate post-`Start()` observation)* —
  the assigning brief named only `monitorScopeMembership`'s `sample()`
  function as in scope, and this was treated as a deliberate scoping
  decision, not an oversight. Noted for the record: `initialMigrated` shares
  the same leaf-only shape and could in principle produce the same
  false-positive if a leader relocated itself in the extremely narrow window
  between `cmd.Start()` returning and the immediately-following
  `scope.Members()` read in `Runner.Launch` — before the shell has even been
  scheduled to exec, in practice. Not fixed here; if it is ever observed
  live, it is the natural next AIRA-34-shaped follow-up (a properly allocated
  ticket, not a hand-picked ID, per this project's ID-allocation rule).
- *"Verify the build_notes below against current source yourself first"* —
  done before editing: the two leaf-only-check line references in the brief
  (`~:1575-1577` and `~:1588-1590`) had drifted to `~1567-1569` and
  `~1580-1582` respectively by the time this ticket was picked up (other
  tickets had landed on `runner_linux.go` in between); the described
  function shape, field names (`scopeMonitorResult.LeaderMigrated/Gap/
  Escape`), and helper functions (`observeProcessCgroup`, `witnessedEscape`,
  `pathEqualOrUnder`) all matched current source exactly.
