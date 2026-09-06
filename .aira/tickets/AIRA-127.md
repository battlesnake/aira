---
{"schema":1,"id":"AIRA-127","project":"aira","title":"aira top: live process/reservation dashboard with a colour-coded system-RAM bar","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["observability","tui"],"hold":false,"relations":[]}
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

## Resolution

Built as a new top-level `aira top` verb, with the same panel also reachable as
tab 7 of `aira tui`. One rendering stack, one controller, one palette module.

### Why a verb and not only a panel

`aira tui` resolves a project (`app.Discover` → `scopeForCWD`) before it starts.
Confine state is machine-wide and `aira confine --list` is project-less, so a
slice monitor that only existed inside the project TUI would refuse to start in
most of the directories an operator watching the slice is standing in. `aira top`
is therefore intercepted in `Run` before scope resolution — beside `confine`, and
on the same reasoning — and refuses `--scope-dir` for the same reason.

It is NOT a second stack. `newTopRuntime` builds the existing `tuiRuntime` over a
restricted view set (`topOnlyViews`), with an empty worktree scope, no execute
dispatcher, and the project event-watch loop not started (there is no project to
watch, and a watch there would spin a reconnect backoff forever behind the view).
Adding `viewTop` to `allViews` gives the ordinary dashboard the same panel free.

### Requirements

1. Process/reservation list — `confine --list`'s own rows: name, owner,
   supervisor pid, scope id, subtree-aware LIVE, granted reserve, RSS, age.
   No second data source; the fetch sends the same `confine-list` request.
2. Total-system-RAM bar — `topBar.TotalBytes`, fail-closed: without an
   established MemTotal there is no bar, only a stated reason.
3. Reservations stacked from the LEFT in slot order, one colour-coded region
   each, plus a labelled aggregate region for the scope-less admissions that
   have no row (so the stack is not narrower than the slice's real claim).
4. Soft (memory.high), hard (memory.max) and — only when it genuinely sits below
   memory.max — the AIRA-103/106 effective admission ceiling, marked on the bar.
5. Out-of-slice usage right-anchored in grey; the gap between it and the stack is
   unclaimed RAM. "Used" is MemTotal − MemAvailable on both sides, minus the
   slice's NON-reclaimable footprint (memory.current − file LRU), so the slice's
   own page cache is not double-counted.
6. One colour lookup (`topSlotColour`) feeds both the row and its bar region, so
   the match is structural rather than two call sites agreeing.
7. Stable ordering: `assignTopSlots`. A live scope keeps its slot; an exit frees
   it and leaves a hole (survivors do not reflow); a new admission takes the
   lowest free slot and appends only when the table is full. The table lives in
   `tuiState.TopSlots` so it survives refresh ticks, and slotting happens in the
   reducer, not the fetch goroutine.

### Data source

The daemon's existing `confine-list` reply, extended with the system/slice frame
the same pass already holds: `SystemMemTotalBytes`, `SystemMemAvailableBytes`,
`SliceCurrentBytes`, `SliceReclaimableBytes`, `SliceMaxBytes`, `SliceHighBytes` +
`SliceHighState`, `CeilingEffectiveBytes`. No new admission pipeline: the meminfo
pair comes from the seams the ceiling subsystem already uses, the slice figures
from the memory read whose `ok` already gates the whole `SliceReserve` struct,
and only memory.high is a new (reporting-only) file read. Withheld entirely in
ci-shim mode, where host meminfo is not namespaced to the container.

### Deliberate deferrals / accepted gaps

- `tui_palette.go` is a COMMAND palette; there was no pre-existing colour
  allocation anywhere in the TUI. The slot-colour table is added to that module
  as the ticket directs, so a second palette never appears, but it is the
  module's first colour table rather than an extension of one.
- Slot colours WRAP past `len(topSlotColours)` (8). The slot is the identity the
  bar and the row agree on; a wrap costs legibility, never correctness.
- A scope whose memory.max is `max`, or whose cap could not be read, gets a row
  but NO bar region — an unknown claim has no width — and the bar names how many
  it could not draw.
- `aira top` is as undiscoverable as `aira tui`: neither is in the generated help,
  because neither is a `core.Do` verb. Unchanged parity, not a fix.
