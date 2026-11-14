---
{"schema":1,"id":"AIRA-336","project":"aira","title":"aira confine --budget prints the whole machine-wide history (100 MB, 180k rows, one JSON line): bound the default output and say so","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.42","labels":[],"hold":false,"relations":[]}
---


Filed from the 2026-11-14 rant triage (see docs/plans/2026-11-14-backlog-plan.md). Surface: both. Size: M. Correctness-critical (two-loop): no.

## Problem
`aira confine --budget` returns every subject in confine_peak_history: 180,393 rows and 100,554,719 bytes on one JSON line (measured 2026-11-14; 87 MB on 2026-11-06), and the client needs about 1 GB RSS to join and sort it. 93% of the rows are `unevaluated` one-offs. The global CLAUDE.md and the SKILL (internal/core/skill.go:321,335-336) tell agents to run it, and piping to `head` does not protect them because the output is one line, so it fills tool-output buffers in every project (RANT-105, from flavour-kernel).

## Evidence
aira confine --budget > file: rc=0, wc -c -l = '1 100554719'. 180,393 subjects (unevaluated 168,100; over 6,250; well-fitted 2,825; under 2,160; acceptable 1,058). Confined client peak-rss=1015860K (96% of 1G). confine_history_client.go:122-130 joins every page and sorts; confine_budget_render.go:51-75 prints every row; core.go:2139 has no limit flag.

## Minimum fix (from-scratch minimum)
Do what AIRA-327 did for rant ls. By default return only actionable rows (under-provisioned, then over-provisioned), worst-first, capped at --limit N (default about 50). Return an envelope {subjects, total, returned, truncated, distribution:{direction:count}} so the hidden rows are counted, never silently dropped. Add `--direction D|all` to widen the set, `--grep SUBSTR` on the subject (so the SKILL's 'see what was recorded for this command' advice still works), and --limit 0 for everything. In human mode, print the distribution line and a 'N more not shown; --limit/--direction' footer. Elide subjects longer than about 200 chars in human rows (JSON keeps the full subject). Place the filter and top-N where it bounds memory: either keep a bounded heap while joining pages client-side (client-only, no proto change), or apply it in the daemon's single streamed pass (internal/daemon/confine_history_page.go) so the reply fits one frame. Prefer whichever deletes more: if the daemon does it, the budget half of the client page-join can go. Update the SKILL text and the global-CLAUDE.md wording ('lists the over-provisioned commands') to match. Deliberately NOT included: any history row cap or age prune (AIRA-246 was retired, and pruning would move learned reserves and the p90 prior), any change to classification, and any change to --dump. PLAN CORRECTION: do the cap in the DAEMON (a client-only filter still receives all 180k rows). The direction/recommendation is computed in Go by ClassifyResourceBudget (internal/daemon/confine_budget.go:46-53) after the rows are read, so filter and cap AFTER classification, not with a SQL LIMIT. One optional request argument; decide in the plan whether that needs a proto bump (a new client sending it to an old daemon is the case to check).

## Tests
Unit: a seeded history with more than N actionable rows plus many unevaluated ones returns exactly N rows worst-first, total/returned/truncated correct, and distribution counts summing to total. --direction all and --limit 0 return everything. --grep matches a subject substring. An unevaluated result still renders 'unevaluated', never 'no history'. A human-mode test checks the footer and the elision. A regression test proves the default output is bounded (byte count under a fixed ceiling for a 10k-row seeded history) and fails against the current unbounded code.

## Files
- cmd/aira/confine_budget_render.go
- cmd/aira/confine_history_client.go
- cmd/aira/main.go
- internal/daemon/confine_budget.go
- internal/daemon/confine_history_page.go
- internal/core/core.go
- internal/core/skill.go

## Related tickets
AIRA-246, AIRA-280, AIRA-327, AIRA-180
