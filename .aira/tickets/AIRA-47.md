---
{"schema":1,"id":"AIRA-47","project":"aira","title":"aitest Slice 1: fork+admission pytest worker pool replacing xdist (previously referenced as AIRA-30)","status":"retired","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","confine","daemon","pytest","scheduler"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-47","to":"AIRA-48"}]}
---
A daemon-optional supervisor that forks pytest workers, admits each through `aira worker-admit` (cgroup placement + memory reservation via AIRA-29's dynamic-reserve foundation, AIRA-36's nested-scope reaper), dispatches tests to idle workers, and recycles/replaces workers on crash, with a daemon-down/unavailable fallback path — replacing pytest-xdist as the parallel test substrate.

Spec: docs/superpowers/specs/2026-09-01-aitest-design.md
Plan: docs/superpowers/plans/2026-09-01-aitest-plan.md

This work was referred to as "AIRA-30" throughout its own spec, plan, and prior session commit messages (e.g. AIRA-36's "Unblocks aitest's nested per-worker scopes (AIRA-30)"), but no ticket record was ever created for it at the time — this ticket is that record, created retroactively at merge time under the allocator's next real ID rather than hand-picking 30 (which the allocator has since passed).

Full two-loop: plan → plan-review (Sol+DeepSeek+Fable) → plan gate → implement (TDD) → work-review → work-fix → Fable final-gate, run to completion over 3 rounds:
- Round 1: real-cgroup test leaks (single-level `removeScopeTree` vs nested scope trees) and an `--estimated-bytes` floor misclassification. Fixed, confirmed resolved round 2.
- Round 2: an env-leak defeating `test_oom.py`'s skip guard, and a nested-bootstrap `unevaluated: unbounded` classification hang. Fixed, confirmed resolved round 3.
- Round 3: gate_verdict PASS. 6 residual P2s — 4 fixed inline (daemon-side `reject:` prefix generalized to match its own spec's §3.3 convention, a second env-leak site, an "unbounded" reason literal pinned directly against its producer, an E_DAEMON_PROTOCOL classifier test), 1 ticketed separately (E_DAEMON_PROTOCOL conflates per-request sizing with a future protocol-version mismatch; not reachable at merge time), 1 flagged as a pre-existing flake already covered by AIRA-20's hardening ticket.

MERGED (2026-09-02, master via PR #2, squash commit `2692053`). Verified: `aira confine -- make ci` (fmt-check, vet, build, full `go test ./... -count=1`) all packages ok, confined; plus the gate rounds' mandatory-real commands (`AIRA_REAL_CGROUP=1 go test ./internal/pylib/... ./internal/runner/...`, `python3 -m pytest -q` in internal/pylib/aitest — 71 passed). DEPLOYED: binary swapped (atomic same-dir rename, the running daemon's old inode kept serving until restart) + `systemctl --user restart aira-daemon.service`, verified active on the new PID immediately after.

relates AIRA-29, AIRA-36.
