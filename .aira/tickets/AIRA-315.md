---
{"schema":1,"id":"AIRA-315","project":"aira","title":"Rant-triage small batch: aitest --collect-only, eject diagnostic, pylib nested confine, flake bound, docs-only pre-push, confine --list PEAK (RANT-41/43/44/45/46/47)","status":"in-progress","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.40","labels":[],"hold":false,"relations":[]}
---
From the 2026-10-25 rant triage; each item verified.
1. RANT-41: internal/pylib/aitest/__init__.py pytest_runtestloop ignores --collect-only and collection errors; return None (hand back to pytest's own loop) when option.collectonly or (session.testsfailed and not continue_on_collection_errors). Pin with a test.
2. RANT-45: eject.go:189 E_EJECT_UNVERIFIED names the project, the missing and present roots (and the remedy only if verified). No new verb.
3. RANT-46: internal/pylib/pytest_aitest_e2e_test.go:337 and restart_merge_gate_worker_e2e_test.go:164 wrap go build in a nested aira confine whose unbounded admission wait sits inside the 20m go-test timeout; build directly like internal/daemon and cmd/aira do (optionally once per package).
4. RANT-43: internal/daemon/confine_reserve_wait_bound_linux_test.go:354-366: separate the 2s admission window from the dwell (bound ~10s or testdeadline-scaled; dwell = bound + margin measured from the grant).
5. RANT-44: .githooks/pre-push fail-closed fast path: every changed path under docs/ -> skip; every path under docs/ or .aira/ -> run only ./internal/store tests; anything else or any git doubt -> existing make ci. Do NOT allowlist *.md or Makefile etc. Keep internal/install/githooks_test.go fixtures non-docs-only.
6. RANT-47(2a): add a PEAK column to the human 'aira confine --list' table (cmd/aira/main.go ~3660; JSON already has it); unevaluated stays unevaluated, never 0.
Not built (see the held owner-decision ticket): live charge for pinned reserves, p90-prior deletion, queued-wait top-holder clause.
