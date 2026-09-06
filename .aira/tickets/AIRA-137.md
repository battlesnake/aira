---
{"schema":1,"id":"AIRA-137","project":"aira","title":"aira top: add a CPU-usage bar (per-job stacked, rest-of-system grey) + RAM/CPU columns in the table","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["observability","tui"],"hold":false,"relations":[]}
---
Requested directly by the owner, 2026-09-06, refining AIRA-127/AIRA-135 (both just
shipped). Verified against real source and a real running box before writing this, not assumed.

## The ask, verbatim

A second bar, same mentality as the RAM bar: OUR jobs (aira.slice) fill from the LEFT, stacked,
one region per job; a single GREY region for 'rest of system' anchored to the RIGHT; the bar's
total width represents MAXIMUM CAPACITY = core count × 100%. Also add RAM usage (current, next to
the RESERVATION column) and CPU info to the table.

## What already exists — verified live on this box, reuse it

The RAM bar's exact pattern to mirror (cmd/aira/tui_top.go:429-430):
```go
sliceUsed := topFloor(reserve.SliceCurrentBytes - reserve.SliceReclaimableBytes)
bar.OutsideBytes = topFloor(systemUsed - sliceUsed)
```
i.e. 'rest of system' = a SYSTEM-WIDE aggregate reading minus the SLICE's OWN aggregate reading
(not a sum of the individually-listed jobs, which is what feeds the STACKED regions separately).

For CPU, the exact same shape is available with NO jiffies/`/proc/stat`/CLK_TCK conversion needed
anywhere — checked live on this box:
- `/sys/fs/cgroup/cpu.stat` (the ROOT cgroup) exposes `usage_usec` cleanly — this is the
  system-wide total.
- `aira.slice`'s own `cpu.stat` (at `.../user@<uid>.service/aira.slice/cpu.stat`, NOT under
  `app.slice` — verify the exact live path via whatever this daemon already resolves the slice
  root to, do not hard-code a guess) ALSO exposes `usage_usec` cleanly — this is the slice-wide
  total to subtract, mirroring `SliceCurrentBytes`.
- Both are the SAME format `internal/runner/usage_linux.go`'s existing `parseCPUStat`/
  `readCgroupUsage` already parses for the per-job confine trailer's `cpu=X+Y` display — REUSE
  that parser for all three levels (root, slice, per-job), do not write a second one.
- Per-job: the SAME `cpu.stat` file already exists under every live scope directory (confirmed:
  this is exactly what the confine trailer's `cpu=` field already reads at TEARDOWN via
  `readCgroupUsage`, per AIRA-136's own findings — that call is one-shot at teardown today; this
  ticket needs the LISTING path, `internal/runner/confine_manage_linux.go`'s scope-scanning loop,
  which does NOT currently read cpu.stat per scope — add a live per-scope read there, mirroring
  EXACTLY how `RSSBytes` (`memory.current`) and `Cap` (`memory.max`) are already read live in
  that same loop).
- Core count: `runtime.NumCPU()` is already used elsewhere in this codebase
  (internal/daemon/server.go:212/398, via `desiredCPUSlots`) — reuse it, do not re-derive core
  count a second way.

## The genuinely new part: CPU is a RATE, not a level — get the honesty right

Every cpu.stat/cgroup reading above is a CUMULATIVE counter since the cgroup/machine was created,
not a live percentage. A rate needs TWO samples and a time delta, exactly like `top`/`htop`
compute %CPU. This view already has cross-tick state (`tuiState.TopSlots`) and a periodic refresh
tick, so the natural fit: the daemon's `confine-list` reply carries the cumulative usec counters
(root, slice, and per-scope) it already read PLUS the wall-clock instant it read them (a server-side
timestamp, not client-side tick timing, which is skewed by render/fetch latency); the CLIENT keeps
the previous tick's (counter, timestamp) pairs alongside the slot table and computes
rate = Δusec / Δwallclock per level, in the SAME reducer that already updates `TopSlots`.

Honesty requirements, matching this project's discipline everywhere else — do NOT skip these:
- The FIRST tick a scope is ever seen (no prior sample yet) has NO CPU RATE, not a fabricated zero
  or a fabricated 100%. Render 'unevaluated' for that one tick, exactly like the bar's existing
  MemAvailable-unevaluated case already does.
- A counter that reads LOWER than its own previous sample (a cgroup counter reset, a clock skew, a
  scope that was reaped and a NEW scope reused the same slot before the next tick) must not produce
  a negative or nonsensical rate — treat it as unevaluated for that tick, not clamp-to-zero (clamping
  would silently hide a real anomaly as 'idle').
- A tick whose time delta is implausibly small (e.g. two ticks fired back-to-back with ~0 elapsed
  time, however that could happen) must not produce a divide-by-near-zero spike — decide a sane
  minimum delta below which the rate is unevaluated rather than wild, and say so in the resolution.

## Bar rendering: generalize, do not duplicate

`topBarFor`/`topBarCells`/`topBarRegion`/`topBarMarker` (cmd/aira/tui_top.go,
cmd/aira/tui.go's rendering) already implement exactly this shape (stack-from-left, single
right-anchored 'outside' region, a 'total capacity' concept, markers) for RAM. A CPU bar is the
SAME shape with a different unit (cores/percent-time instead of bytes) and a different total
(core-count × interval instead of a byte ceiling). Look hard at genuinely parameterizing the
existing functions (a shared bar-model type over an abstract 'capacity/used/outside' triple) rather
than copy-pasting a second, parallel set of CPU-specific bar functions — this is exactly the kind
of duplication this project's architectural-simplicity rule exists to prevent. If a clean shared
abstraction turns out to be genuinely awkward for a specific reason (say why), a second bar renderer
is the fallback, not the default.

## Table changes

Current columns (post-AIRA-135): SLOT, NAME, PID, LIVE, RESERVATION, COMMAND. Add:
- RAM usage (current `memory.current`, i.e. the SAME `RSSBytes` already on `ConfineRecord` and
  already used for the bar-shading split — AIRA-135 deliberately did not give it its OWN table
  column, only the bar shading; the owner is now asking for it back as a column too, placed next to
  RESERVATION).
- CPU info: the new live per-job CPU rate from above, in a legible unit (e.g. a percentage of one
  core, or 'N.Nx cores' — decide a clear, consistent presentation; whichever is chosen, make it
  match the unit used in the CPU bar's own legend so the two views read consistently).

## Colour consistency

The CPU bar's per-job regions must use the SAME slot colour as that job's row and its RAM-bar
region (`topSlotColour`, cmd/aira/tui_palette.go) — one colour identity per job across every
surface in this view, exactly as AIRA-127's requirement 6 already established for the RAM bar; do
not invent a second palette or a different colour for the same job in a different bar.

## Tests

Mirror this ticket's predecessors' viewmodel-level test convention (no golden-screenshot tests):
(a) a job's CPU rate is unevaluated on the tick it first appears, and a real number on the next tick
given two real (counter, timestamp) samples; (b) a counter regression (lower than the previous
sample) yields unevaluated, not a negative or clamped-zero rate; (c) an implausibly-small time delta
yields unevaluated, not a spike; (d) the CPU bar's region widths sum correctly against a representative
set of core-count/usage scenarios, mirroring the RAM bar's own (d)-style geometry test; (e) the RAM
and CPU bar regions for the same job share the same colour as its table row.
