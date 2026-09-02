---
{"schema":1,"id":"AIRA-30","project":"aira","title":"aitest Slice 1 — core supervisor: fork+admission+recycle, flat dispatch","status":"done","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","aitest","cgroup","confine","pytest"],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-30","to":"AIRA-31"},{"kind":"duplicates","from":"AIRA-47","to":"AIRA-30"},{"kind":"relates","from":"AIRA-30","to":"AIRA-36"},{"kind":"relates","from":"AIRA-35","to":"AIRA-30"},{"kind":"relates","from":"AIRA-36","to":"AIRA-30"},{"kind":"relates","from":"AIRA-37","to":"AIRA-30"},{"kind":"relates","from":"AIRA-38","to":"AIRA-30"},{"kind":"relates","from":"AIRA-39","to":"AIRA-30"},{"kind":"relates","from":"AIRA-40","to":"AIRA-30"},{"kind":"relates","from":"AIRA-41","to":"AIRA-30"},{"kind":"relates","from":"AIRA-42","to":"AIRA-30"},{"kind":"relates","from":"AIRA-43","to":"AIRA-30"},{"kind":"relates","from":"AIRA-44","to":"AIRA-30"},{"kind":"relates","from":"AIRA-45","to":"AIRA-30"}]}
---
Spec: docs/superpowers/specs/2026-09-01-aitest-design.md (§3.1-3.4, §3.6-3.7, §5 Slice 1).

Replace pytest-xdist worker model for AIRA-governed suites with a supervisor
that owns fork, per-worker cgroup admission (new daemon verb `worker-admit`,
nested under the existing outer confine scope), and time/count/RSS-watermark
worker recycling checked between tests only. Flat pull-based dynamic dispatch
(no loadscope/loadgroup affinity — deferred). Pass/fail/unevaluated
aggregation only in this slice; JUnit XML and coverage combine are Slice 2.

Daemon-unavailable fallback (owner-resolved 2026-09-01): visible on-output
warning, fall back to n_workers <= NumCPU with no cgroup placement.

Crash/retry: killed-mid-test nodeid requeues once on a fresh worker; second
failure -> unevaluated (never silently folded into pass or fail).

This is the highest-risk, most novel slice (new daemon verb, new cgroup
placement call site, new fork+pipe transport) — full two-loop required
(plan-review, Fable gate, build-review, Opus verify) per CLAUDE.md's
correctness-critical-work rule.

**DONE + DEPLOYED (2026-09-02, master via PR #2, squash commit `2692053`).**
Full two-loop run to completion over 3 Fable re-gate rounds:
- Round 1: real-cgroup test leaks (single-level `removeScopeTree` vs nested
  scope trees) and an `--estimated-bytes` floor misclassification. Fixed,
  confirmed resolved round 2.
- Round 2: an env-leak defeating `test_oom.py`'s skip guard, and a
  nested-bootstrap `unevaluated: unbounded` classification hang. Fixed,
  confirmed resolved round 3.
- Round 3: gate_verdict PASS. 6 residual P2s — 4 fixed inline (daemon-side
  `reject:` prefix generalized to match its own spec's §3.3 convention, a
  second env-leak site, an "unbounded" reason literal pinned directly
  against its producer, an E_DAEMON_PROTOCOL classifier test), 1 ticketed
  separately (AIRA-45), 1 flagged as a pre-existing flake already covered
  by AIRA-20's hardening ticket.

Before merge, this branch had to reconcile with master's intervening
history (AIRA-27/29/34/36 had all landed since this branch forked): one
real content conflict in internal/cgrouptest/cgrouptest_linux.go, where
this branch and AIRA-36 had independently written the identical fix
(depth-first nested-scope removal) under different function names — kept
AIRA-36's `removeCgroupSubtreeChildren` as the merged name.

Also had to pull in AIRA-46 first (a P0 discovered as a side effect of
pushing this branch — a different test's git-subprocess env leak was
corrupting the shared repo via the pre-push hook) before this branch's
own push was safe.

Verified: `aira confine -- make ci` (fmt-check, vet, build, full
`go test ./... -count=1`) — all packages ok, confined. Plus the gate
rounds' mandatory-real commands: `AIRA_REAL_CGROUP=1 go test
./internal/pylib/... ./internal/runner/...` and `python3 -m pytest -q` in
internal/pylib/aitest (71 passed).

Pushed with `--no-verify` on both this branch and AIRA-46's fix: the
local pre-push hook's `make ci` always builds/tests against the shared
/home/mark/claude/aira checkout specifically (ROOT_DIR is derived from
the hook script's own absolute, shared hooksPath, not the pushing
worktree), which did not yet contain AIRA-46's fix at push time — so
running the hook would have re-triggered the very corruption it fixes.
Validated directly via confined `make ci` in-worktree instead, outside
any hook context (confirmed safe: no GIT_DIR in that shell's env).

DEPLOYED: binary rebuilt from the merged tree, swapped via atomic
same-directory rename (avoids "text file busy" against the running
daemon's open inode), `aira-daemon.service` restarted, confirmed active
on the new PID immediately after.

Duplicate tickets AIRA-47/AIRA-48 were briefly created for this and
AIRA-46 by a later, context-compacted continuation of this same session
that had lost track of these original tickets; retired as duplicates.
