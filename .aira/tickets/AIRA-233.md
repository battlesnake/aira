---
{"schema":1,"id":"AIRA-233","project":"aira","title":"aira top: re-add job age column in compact <=2-unit form","status":"done","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[{"kind":"supersedes","from":"AIRA-233","to":"AIRA-234"}]}
---
Owner-requested (2026-09-13): aira top should show each job's age. AIRA-135 dropped the AGE column in the trim that removed OWNER/SCOPE-ID hex (terminal crowding); met here by a COMPACT format, not the wide Go-duration confine --list prints. Format (owner rule): most-significant non-zero unit + the next unit below, dropping the second when zero -- 1d2h3m4s->1d2h; 1d0h5m->1d; 2h0m4s->2h; 3m4s->3m4s; 45s->45s. Data exists (ConfineRecord.AgeSeconds); display-only change in cmd/aira/tui_top.go (topAgeCell + formatConfineAgeShort + AGE header after LIVE). Unknown/negative -> unevaluated, never 0s. No protocol change.

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** done. **State on master v0.34:** already-fixed.

Feature fully shipped and tested on master; nothing left to build. Just close the ticket (transition to done/closed).

**Evidence:** Merged as PR #136 (merge b75aec1, impl aa63974 'feat(aira): AIRA-233 — aira top job-age column in compact <=2-unit form', 2026-09-13), ancestor of master. cmd/aira/tui_top.go:553-580 has topAgeCell (nil/negative -> "unevaluated") and formatConfineAgeShort; used in the table row at tui_top.go:691 and in board views (tui_board_viewmodel.go:397, tui_board_overview_viewmodel.go:407). Tests: cmd/aira/tui_top_age_test.go (verifies: AIRA-233) and tui_top_test.go:771. Ticket file status is still 'in-progress' only because it was never closed.
