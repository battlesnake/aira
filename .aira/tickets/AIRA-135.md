---
{"schema":1,"id":"AIRA-135","project":"aira","title":"aira top: replace hex ID columns with PID/command, fix reserve truncation, shade unused-vs-used in the bar","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["observability","tui"],"hold":false,"relations":[]}
---
Requested directly by the owner, 2026-09-06, refining AIRA-127 (just shipped).

## Table columns — replace, not just widen

Current: `SLOT NAME OWNER PID SCOPE-ID LIVE RESERVE RSS AGE` (topViewModel,
cmd/aira/tui_top.go). OWNER and SCOPE-ID render as long, meaningless hex —
not useful to a human scanning the table. RESERVE is truncated in practice
because its column is hard-sized too narrow for the M-suffixed values the
just-shipped formatting change produces (e.g. `42160M` doesn't fit a 4-char
column).

New column set, exactly as specified, nothing extra: **SLOT, NAME, PID, LIVE,
RESERVATION, COMMAND**. Drop OWNER, SCOPE-ID, RSS-as-a-column, and AGE. RSS's
information is not lost — it becomes visible in the bar instead (see below),
which is a better fit for 'how much of this job's reservation is it actually
using' than a bare number next to a bare cap. Size the RESERVATION and
COMMAND columns so real values are never truncated by a hard-coded narrow
width — measure against the actual data (or let tview's table auto-size),
not a guess.

## COMMAND is genuinely new data — not currently tracked anywhere

Verified against source: `runner.ConfineRecord` (internal/runner/confine_manage.go)
has no command/argv field, and the real listing implementation
(confine_manage_linux.go) builds every record from the SCOPE DIRECTORY NAME
and cgroup files alone — it never reads the confined command's argv from
anywhere.

Fix: add `Command *string` (nil = unevaluated, matching every other optional
field's convention here) to `ConfineRecord`, populated the SAME way
`RSSBytes`/`Cap` already are — a live read at listing time, keyed off the
record's existing `SupervisorPID` (the `aira confine` process's own PID,
which already carries the invocation's full argv). Read
`/proc/<SupervisorPID>/cmdline` (NUL-separated), and — since `aira confine`'s
CLI syntax is always `confine [flags] -- <argv...>`, per its own help text —
extract just the part AFTER the first bare `--` argument as the displayed
command (the actual wrapped invocation, not aira's own flags); fall back to
the whole cmdline if no `--` is found (should not normally happen, but never
silently drop the field for it). Leave `Command` nil/unevaluated, exactly
like RSS/Cap already do, when the read fails (process already exited between
the scope listing and this read, permission issue, etc.) — never fabricate
a placeholder.

Wire `Command` through the JSON `confine-list` reply and the daemon-side
ci-shim path (internal/daemon/shim.go) the same way every other optional
ConfineRecord field already crosses that boundary. In the COMMAND column,
render the value or 'unevaluated' consistent with every other cell in this
table; truncate for terminal width the way tview / this table already
handles a long cell (this is a display constraint, not data loss — do not
invent a second truncation scheme).

## Bar: darken the unused portion of each reservation

Currently each admitted scope's region in the RAM bar is drawn as one solid
colour, sized to its RESERVATION (memory.max). The data needed to also show
USAGE within that same region already exists in `ConfineRecord` — no new
plumbing needed here, only rendering: `RSSBytes` (`memory.current`, a LIVE
cgroup read) is the used amount, `Cap` (via `topReserveFor`) is the reserved
amount, both already read by the existing listing code
(internal/runner/confine_manage_linux.go).

Split each drawn region (topBarRegion / topBarCells,
cmd/aira/tui_top.go + tui.go) into two shades of the SAME slot colour: a
brighter/full-intensity sub-span for the LIVE-USED portion (left-aligned
within the region, consistent with the bar's own left-to-right stacking
convention) and a DARKENED sub-span for the remainder — reserved but not
currently used. This directly answers 'how much of this job's reservation is
it actually using', which is exactly what the owner asked for.

Edge cases to get right, not to skip:
- The region's TOTAL WIDTH stays exactly the reservation (unchanged from
  today) — only the internal used/unused split is new. Never let the split
  change where the NEXT region starts.
- `RSSBytes` can exceed the reservation transiently (a monitoring-lag
  overshoot right before an OOM fires) — clamp the used-shade width to the
  region's own full width in that case; never bleed the 'used' shade past
  this region's boundary into a neighbour's.
- If `RSSBytes` is nil (unevaluated) for a scope, draw that region as ONE
  solid shade (today's behaviour) rather than fabricating a used/unused
  split from data that was never established — the same honesty discipline
  every other facet of this view already follows.

## Tests

Extend the existing viewmodel-level test style (no golden-screenshot tests,
per AIRA-127's own convention): (a) Command is populated from a real or
faked /proc/<pid>/cmdline read, split correctly at the first bare `--`
argument, and falls back sanely when no `--` is present; (b) Command is nil/
unevaluated, not fabricated, when the read fails; (c) a region's used-shade
width matches RSSBytes exactly for a representative set of used-vs-reserved
ratios including used > reserved (clamped) and used == 0; (d) a region with
nil RSSBytes draws as one undivided shade; (e) the new column set and widths
render correctly for values that would have truncated under the old
hard-coded widths.

## Resolution (in-review)

Branch `aira135-top-columns-and-shading`. All three parts built as specified.

### 1. Columns

`topViewModel` (cmd/aira/tui_top.go) now emits exactly
`SLOT NAME PID LIVE RESERVATION COMMAND`. OWNER, SCOPE-ID, RSS and AGE are gone.

No hard-coded widths were added or changed — none existed. The truncation was
structural: tview sizes columns greedily left to right and CLAMPS the first one
that no longer fits (`table.go:1018-1036`), so the two long hex columns were
consuming the width the later columns needed. Removing them removes the clamp.
COMMAND is placed LAST because it is the only cell with no natural bound, so it
absorbs whatever clamp remains rather than imposing one on RESERVATION.

### 2. `ConfineRecord.Command`

`Command *string` (`json:"command"`), populated in `listConfinesWithDeps` from a
live `/proc/<SupervisorPID>/cmdline` read at listing time, through a new
`confineScanDeps.readCmdline` seam (nil falls back to the production reader, so
existing callers that build the struct field-by-field keep real behaviour).
`confineCommandFromCmdline` splits at the FIRST bare `--`, falls back to the whole
argv when there is none, and returns ok=false — leaving the field nil and naming
`"command"` in `UnevaluatedFields` — for an empty cmdline or a `--` with nothing
after it. Never a fabricated placeholder.

The read is bounded at `ConfineCommandWireLimit` (4096) for availability, on the
same reasoning as `ConfineReservationSignatureWireLimit`; elision is MARKED with
an ellipsis and the truncated tail is made valid UTF-8, never silently cut.

The registry-only (Pending) rows built by `mergeConfineRegistry` — which is the
ENTIRE listing in ci-shim mode — name `"command"` unevaluated alongside
populated/rss/cap: that function performs no live read of any kind and must
compile where there is no /proc.

`topCommandCell` escapes non-printing runes and neutralises tview's colour-tag
syntax before the value reaches a terminal, exactly as
`confineSignatureForDisplay` already does for the equally untrusted reservation
signature. Width truncation is left to the table, as the ticket requires.

### 3. Bar shading

`topBarRegion` gained `UsedBytes`/`UsedKnown`/`ShadeColour`; `Bytes` is unchanged
and still the whole reservation, so no region's start can move. `topUsedWithin`
holds the three edge cases in one place: nil/negative RSS -> `UsedKnown=false`
(one undivided shade), an established zero -> a fully darkened region, and
usage above the reservation -> clamped to it. `topBarCells` derives the split
boundary from the same absolute-offset mapping as the region's own edges and
paints `[start,shadedFrom)` bright and `[shadedFrom,end)` in
`topShadeColour(colour)` — a 45% darkening of the slot colour that returns ""
for anything it cannot parse, which makes an undarkenable colour draw undivided
rather than invisibly "split".

Beyond the ticket, one addition from dogfooding: the summary line carries
`(bright = in use, dark = reserved and idle)`, and only when a split is actually
drawn. A two-tone bar with no key is not readable, and a key beside an undivided
bar would describe something that is not on screen.

### Known gap, recorded not hidden

`SupervisorPID` is decoded from the scope DIRECTORY NAME, so between a
supervisor's death and the orphan reaper's sweep a reused PID could carry an
unrelated process's argv. That is exactly the trust level the PID column beside
it already has, and which `killConfine`'s pid selector already accepts; COMMAND
inherits it, widens nothing, and participates in no decision. Documented on the
field.

### Verification

Viewmodel-level tests only (AIRA-127's convention; no golden screenshots).
Every new test was checked non-porous by REVERTING the behaviour it pins —
no-`--`-split, split-at-last-`--`, fabricated command on read failure, silent
elision, dropped pending "command", the old column set, unclamped used-span,
nil-RSS-as-zero, cells ignoring the split, cells shading the whole region,
shade == bright colour, a shaded non-scope region, an unescaped command cell,
and an unconditional shade legend. Each mutation failed the suite.

Dogfooded against real confined jobs through a real pty (tmux, 190 cols): the
new columns render, COMMAND is read live from /proc and split at `--`, an argv
containing newlines renders escaped, and the bar's per-slot regions paint the
bright/dark split at exactly the computed column boundaries.

