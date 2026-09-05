---
{"schema":1,"id":"AIRA-96","project":"aira","title":"TestScopeMembershipEventsDeliversModifyAndReleasesFD fails when fs.inotify.max_user_instances is near exhausted","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["environmental","runner","testing"],"hold":false,"relations":[]}
---
Found during the AIRA-16 watchdog fix's independent code review (2026-09-04). Deliberately not filed by the reviewing agent at the time, to avoid racing ticket-ID allocation with other concurrently-running batch agents -- filed now by the coordinating session instead, no longer racing.

TestScopeMembershipEventsDeliversModifyAndReleasesFD (internal/runner) fails when fs.inotify.max_user_instances is near exhausted on the host -- observed at 119/128 instances in use. Reproduced identically on an unmodified base commit, so this is a genuine environmental flake, not a regression introduced by AIRA-16 or anything else in that batch.

Likely a real, if narrow, reliability gap: on a machine running many concurrent AIRA sessions/agents (each of which may hold inotify watches for confine scope membership, aitest worker liveness, or similar), the shared per-user inotify instance limit can genuinely run low, and this test (and potentially the runtime code path it tests) has no graceful degradation for that condition -- it simply fails/would fail when the limit is hit.

Not investigated beyond the reproduction above. Suggested next steps, not decided: (1) check whether the production code path this test covers (scope-membership inotify watching) has any fallback or graceful-degradation behavior when inotify_init fails with EMFILE/ENOSPC, or whether it fails hard; (2) consider whether the test itself should skip or raise a clearer diagnostic when the host is near its inotify instance limit rather than failing with a generic assertion error; (3) check whether AIRA's own processes are contributing disproportionately to inotify instance exhaustion on this shared machine, independent of whether the test itself needs hardening.

## Resolution

Scope actually built (per the coordinating session's later brief): bake the
already-live manual mitigation (`sudo sysctl -w fs.inotify.max_user_instances=4096`,
persisted ad hoc at `/etc/sysctl.d/60-inotify-aira.conf`) into `aira install` as a
managed drop-in, following the existing oomd + delegation `/etc` drop-in
machinery. Item (2) (test-side EMFILE skip/diagnostic handling) was explicitly
out of scope for this ticket and remains open if wanted later. Items (1) and (3)
were not separately investigated beyond what the install-side fix already
implies (raising the limit reduces exhaustion pressure regardless of which
processes contribute to it).

Built in `internal/install/`:
- New embedded asset `assets/sysctl/60-inotify-aira.conf`
  (`fs.inotify.max_user_instances = 4096`, first-line managed marker,
  rationale comment referencing `scopeMembershipEvents`).
- `systemDropin` gained a `sysctl bool` activation class; `systemDropins()`
  appends the new entry (`dst = <etcRoot>/sysctl.d/60-inotify-aira.conf`).
- `installSystemDropins` runs `sysctl --system` when the file changes **or**
  the kernel's live value doesn't match (self-healing, mirroring the existing
  oomd-restart block — a byte-identical file that was never actually applied
  must keep retrying, not just fire once on the change that wrote it). A
  successful `sysctl --system` exit is also post-verified against the live
  kernel value so a later sysctl.d file (or `/etc/sysctl.conf`, applied last)
  silently overriding ours is reported as a real activation failure, never a
  fake pass. Dry-run gets a matching `planned: sysctl --system ...` line.
- `systemDropinsCurrent`/`systemDropinsHealthy` verify the *live* kernel value
  via `/proc/sys/fs/inotify/max_user_instances`, not just that the on-disk
  file matches.
- Install-summary/status wording renamed "oomd + delegation" ->
  "oomd + delegation + sysctl"; the "then re-login" recommendation is now
  shown only when memory delegation itself is pending (oomd/sysctl activation
  is immediate; delegation genuinely needs a re-login).
- Fixed a latent positional-index bug surfaced by inserting the new entry:
  `delegationDropinCurrent` and the test helper `installFakeDelegationDropin`
  both read `rendered[len(rendered)-1]`, silently assuming the delegate
  drop-in was always last in `systemDropins()`. Both now look it up by asset
  path (`delegationDropinAsset` constant) instead.

Tests added/extended in `internal/install/oomd_delegation_test.go`:
`TestOomdDropinsRenderToUniquePathsWithExactLessonContent` (new drop-in's
exact content), `TestRootInstallWritesAndActivatesInotifySysctlDropin`,
`TestSystemDropinsCurrentRequiresLiveSysctlMatch`,
`TestRootInstallRetriesSysctlActivationAfterPriorActivationFailure`,
`TestRootInstallReportsSysctlOverrideAsActivationFailure`,
`TestRootPreflightsEveryOwnedTargetBeforeFirstPublish` (now derives the
actual last-rendered drop-in instead of hardcoding the delegate path), plus
extending the existing dry-run, idempotency, and activation-failure tests to
cover the new `sysctl --system` path. `newFakeRootInstall`'s harness now
models the live kernel value (`state.sysctlLive`) hermetically so none of this
depends on the real test-runner host's actual sysctl state.

Process: implemented directly (light path per
`docs/dev/agentic-development-loop.md`, no plan-review/plan-gate loop), then
self-reviewed, then one independent adversarial pass by Codex (Sol) in place
of Fable (not reachable as an agent from this session) — the project's
designated reviewer/gate role for this class of work. Sol issued two BLOCK
verdicts before PASS:
1. Sysctl activation was gated on file-change only, with no retry once the
   file was already written -- a `sysctl --system` failure would never be
   retried on a later idempotent run. Fixed by self-healing off the live
   kernel value, mirroring the oomd-restart pattern, with a regression test.
2. A successful (`exit 0`) `sysctl --system` was not post-verified against
   the live value -- a later sysctl.d/`sysctl.conf` override would silently
   pass. Fixed with a post-activation readback and a regression test
   modeling the override.
A follow-on nit (the "then re-login" wording being shown even when only
oomd/sysctl, not delegation, was stale) was also fixed. Final verdict: PASS,
no remaining findings.

Verification: `go build ./...` exit 0, `go vet ./...` exit 0. `go test
./internal/install/...` exit 0 on every run. Full `go test ./...` (via this
repo's confined pre-push hook, `-count=1 -timeout 20m`) exit 0 on the run that
actually landed; two earlier attempts hit a real but unrelated pre-existing
flake, `TestM20LauncherDefersACKAndBoundsReadiness/handle_before_ack` in
`internal/runner` (a wall-clock-tight launcher-ACK timing test, same class as
AIRA-20) -- reproduced only under this box's heavy concurrent load (8-10
other confine jobs live at the time) and confirmed to pass reliably (3/3
isolated runs, plus a full clean suite run) once load eased; internal/install
itself was green on every single attempt. Not a regression from this change
(internal/runner is untouched by this diff) and not investigated further here
as it is out of scope for AIRA-96.

GitHub Actions PR checks (`build + vet + gofmt`, `race`, `test`) all passed
before merge.

PR: https://github.com/battlesnake/aira/pull/55
Merge commit: `c7f2449dba378ca3792c4d7109e5e8cbbb0b6039` (squash), independently
re-verified by reading the file content back from `origin/master` after merge
(not just trusting the PR diff/description).
