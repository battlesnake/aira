---
{"schema":1,"id":"AIRA-127","project":"aira","title":"aira top: live process/reservation dashboard with a colour-coded system-RAM bar","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["observability","tui"],"hold":false,"relations":[]}
---
Requested directly by the owner, 2026-09-06. Explicitly marked LOWER PRIORITY —
queue behind the current backlog and CI-mode work; do not build ahead of them.

## The ask, verbatim requirements

A live, refreshing 'top'-style view of aira.slice:

1. A process/reservation list: every job currently in the slice with its
   metrics (at minimum whatever `confine --list` already surfaces per job --
   name/owner, PID, granted memory.max, current usage, cpu -- reuse that data
   source, do not build a second one).
2. A wide horizontal bar representing TOTAL SYSTEM RAM.
3. Each active reservation drawn as a colour-coded region in that bar,
   ACCUMULATING FROM THE LEFT (stacked, left-to-right, in reservation order).
4. The slice's soft limit (memory.high, where set) AND hard limit
   (memory.max / the effective ceiling) marked on the bar.
5. RAM used OUTSIDE the slice (the rest of the system) shown as a GREY region,
   RIGHT-ALIGNED (anchored to the bar's right edge) -- so the visual gap
   between the slice's stacked reservations (growing from the left) and the
   rest-of-system usage (anchored from the right) is genuinely free system
   RAM, read directly off the bar.
6. Colour-coding MUST match between the bar's regions and the process list's
   rows -- the same reservation is the same colour in both places.
7. ORDERING MUST BE STABLE. Do not re-sort the whole list/bar on every
   refresh tick (e.g. by current size or by remaining time) -- a new
   admission or exit must not reshuffle every other entry's position/colour.
   Assign each reservation a stable slot (e.g. on first-seen order, or a
   small free-list of colour/slot indices reused only after their prior
   occupant's slot is confirmed gone) so existing entries hold their position
   and colour for their whole lifetime; new entries append, exited entries
   free their slot for reuse rather than triggering a global re-layout.

## Existing infrastructure to reuse, not duplicate

AIRA already has a real, structured TUI (tview/tcell-based, cgo-free) --
cmd/aira/tui.go, tui_data.go, tui_viewmodel.go, tui_controller.go,
tui_executor.go/tui_execute.go, tui_inline.go -- with its OWN existing colour
palette module, cmd/aira/tui_palette.go. 'aira top' should be a new view/mode
built on this SAME infrastructure (data source, viewmodel/controller split,
palette), not a second parallel live-refresh terminal rendering stack --
matches this project's architectural-simplicity rule and the 'thin faces over
one core' rule (CLAUDE.md: CLI/MCP/Skill/daemon/TUI are thin faces over
core.Do). Decide at build time whether this is a new top-level `aira top`
verb that launches straight into this view, or a new panel/mode inside the
existing TUI reachable from there -- but the underlying data plumbing and
palette should be shared either way.

## Data source

Whatever daemon-side listing already backs `confine --list` (admitted jobs,
their granted memory.max, current memory.current, owner/name) plus the slice
ceiling machinery already built for AIRA-106/103/114 (soft/hard limits,
aggregate reserve) should be the ONLY sources of truth here -- this ticket is
a rendering/UX layer, not a new admission-data pipeline.

## Tests

A viewmodel-level test (not a terminal-rendering/golden-screenshot test,
consistent with how the existing TUI is tested) proving: (a) a reservation
keeps the same colour/slot across multiple refresh ticks while it stays
admitted; (b) a new admission is appended without moving any existing
entry's slot; (c) an exited reservation's slot becomes free and is only
reused by a LATER new admission, not by immediately reflowing the remaining
entries; (d) the bar's stacked-from-left width matches the sum of admitted
reservations, the grey right-aligned region matches (system-used minus
slice-used), and the soft/hard limit markers land at the correct offsets for
a few representative RAM/limit configurations.
