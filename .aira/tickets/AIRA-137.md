---
{"schema":1,"id":"AIRA-137","project":"aira","title":"aira top: add a CPU-usage bar (per-job stacked, rest-of-system grey) + RAM/CPU columns in the table","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["observability","tui"],"hold":false,"relations":[]}
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

## Resolution

### Wire

`ConfineRecord.CPUUsageUsec *int64` — the scope's cumulative `cpu.stat`
`usage_usec`, read live from the SAME already-open scope directory, through the
SAME `confineScanDeps.readField` seam and in the SAME loop that already reads
`memory.current` and `memory.max`. `nil` plus `"cpu"` in `UnevaluatedFields`
when it could not be established; an idle cgroup's real `usage_usec 0` stays
distinguishable from an unreadable one.

`ConfineSliceReserve` gained `SystemCPUUsageUsec`/`SystemCPUKnown`,
`SliceCPUUsageUsec`/`SliceCPUKnown`, `CPUSampleUnixNano` and `CPUCores`. Each
counter carries its own known bit because — unlike every byte field beside it —
ZERO IS LEGAL for a cumulative counter, so the absence cannot be folded into the
number. The frame is published from the same `ok` memory reading and inside the
same `!shimMode()` gate as AIRA-127's RAM frame, so shim mode withholds it whole:
a container's host-visible ROOT cgroup counts every core of the machine, not the
container's quota.

`runner.ReadConfineCPUFrame(slicePath)` reads root and slice `cpu.stat` and
stamps one instant. The root is resolved through the existing `unifiedMount()`,
not a hard-coded `/sys/fs/cgroup`. Parsing reuses `parseCgroupKeyValues` via a
new one-line `parseCPUUsageUsec` — no second parser. The daemon reaches it
through `cpuFrameReader()`/`cpuCoreCounter()` seams shaped exactly like the
existing `memTotalReader`/`memAvailableReader` pair, so the reply's CPU fields
can be asserted against a fixed frame instead of this host's ever-moving
counters. Core count is `runtime.NumCPU()`, the same source `desiredCPUSlots`
derives the AIRA-49 slot count from.

### Rate, and its honesty rules

`cpu.stat` publishes only cumulative counters, so a rate needs two samples and
the wall clock between them. The previous sample lives in the reducer's
cross-tick state (`tuiState.Top`, now a `topTick` carrying AIRA-127's slot table
AND the CPU sample), and `topViewModel` computes every rate in the SAME pass that
updates the slot table. The interval is established ONCE per tick
(`topCPUDeltaBetween`), so a job's rate, the slice's and the machine's are all
divided by the same number and the parts add up to the whole by construction.

The instant is the DAEMON'S, not the client's tick time: fetch, decode and render
latency skew a client-side clock by tens of milliseconds against a one-second
tick — a percent-level error that is largest exactly when the machine is busiest.

Four cases yield UNEVALUATED, never a number:

- No previous sample (the first tick a scope, or the view, is seen).
- A counter LOWER than its own previous sample. Not clamped to zero: clamping
  renders a counter reset, a reused scope id, or an accounting anomaly as a
  peacefully idle job, hiding the very thing the reading exists to surface.
- An interval below `topCPUMinSampleNanos` = **250ms**. The kernel charges
  `cpu.stat` in ~4ms scheduler quanta, so at 1ms between samples a single quantum
  divides out to hundreds of cores; 250ms is well above that quantisation and
  well below the refresh interval, so no ordinary tick is rejected (pinned by a
  test that asserts `topRefreshInterval` passes the guard). Negative intervals —
  a server clock stepping backwards — fall in the same branch.
- An interval above `topCPUMaxSampleNanos` = **30s**. The top tick stops when the
  operator leaves the view, so a previous sample can be minutes old; the
  difference across it is a true average over those minutes, but it is LABELLED
  as current load, and a 10-minute average presented as "now" is a misreading
  this view would be inviting.

### Bar generalization, not duplication

There is ONE bar model and ONE renderer. `topBar`/`topBarRegion`/`topBarMarker`
lost their byte-specific field names (`TotalBytes`→`Total`, `Bytes`→`Size`,
`StartBytes`→`Start`, `UsedBytes`→`Used`, `ClaimedBytes`→`Claimed`,
`OutsideBytes`→`Outside`, `FreeBytes`→`Free`, marker `Bytes`→`At`) and gained a
single `Kind` discriminator naming the unit. `topBarCells`, `topBarColumn` and
the z-order are unit-blind and unchanged: the geometry question has nothing to do
with what the quantity measures. `renderTopBar` now takes its target panel and
its bar as arguments and is called twice; unit-specific wording is a lookup off
`Kind`. No parallel CPU-specific bar stack exists.

CPU quantities are integer MICRO-CORES (`topCPUMicroCores` = 1e6 = one core's
worth of CPU time per unit wall-clock), so the exact-integer offset arithmetic
that makes adjacent regions abut is reused rather than replaced by floats. Micro
rather than milli because the SUM over many small jobs matters even when each is
individually uninteresting.

Bar composition mirrors the RAM bar exactly: per-job spans stacked from the LEFT
in slot order and slot colour; the slice's own unscoped remainder
(`slice CPU − Σ drawn jobs`) as one labelled region; the idle gap; and a single
right-anchored grey for the rest of the system, derived as a ROOT-cgroup reading
minus the SLICE's own reading — never a sum over the listed scopes. Total
capacity is `NumCPU × one core`, which unlike the RAM total is a hard ceiling.

Two deliberate differences, both documented in code: a CPU region draws NO
bright/dark split (there is no per-job CPU reservation for a job to be idle
against, so `UsedKnown` stays false and the existing undivided path is taken),
and the CPU bar emits NO "no slice limit could be established" line, because this
view derives no CPU ceiling from anything and announcing the absence of something
never expected reads as a fault where there is none.

### Table

`SLOT NAME PID LIVE RESERVATION RAM "CPU CORES" COMMAND`. RAM is the live
`memory.current` reading AIRA-135 moved into the bar shading, back as a column
next to RESERVATION — the split answers "what fraction of its grant", the number
answers "how much is that". CPU is the per-job rate in the SAME unit the CPU
bar's legend prints. Both say `unevaluated` rather than a blank or a zero.
COMMAND stays LAST (asserted), so it still absorbs tview's greedy clamp.

### Colour

Unchanged and structural: `topSlotColour(slot)` is called once per row and the
value is handed to the row, its RAM region and its CPU region. Verified live —
see below.

### Verification

`aira confine -- go build ./...` exit 0; `go vet ./...` exit 0;
`AIRA_REAL_CGROUP=1 ... go test ./... -count=1` exit 0 (foreground, full run).

Every new test was checked NON-POROUS by mutating the implementation and
confirming the test goes red:

- first-tick rate fabricated as zero → (a) and the undrawn-scope test fail;
- counter regression clamped to zero → (b) fails with the literal `"0.00"`;
- minimum-interval guard removed → (c) fails with `"4.00"` cores, i.e. exactly
  the divide-by-near-zero spike the guard exists for;
- maximum-interval guard removed → (c) fails;
- unscoped remainder dropped → (d) fails on `claimed`;
- CPU region coloured by position in `Regions` rather than by slot → (e) fails;
- RAM region coloured by position → (e) and AIRA-127's own colour test fail;
- `parseCPUUsageUsec` keyed on `user_usec` → the runner test fails (its fixture's
  `user+system` sum and `nice_usec` are all distinct from `usage_usec`);
- CPU frame published in shim mode → the shim test fails.

(e)'s first fixture was itself found POROUS during this check — position and slot
agreed in it — and was rebuilt so the slot-0 holder appears in NEITHER bar,
shifting every later job's index one below its slot.

### Dogfood

Built binary driven through a real pty (46×190) against the eight real confined
jobs live on this box at the time, via a private daemon on its own
`XDG_STATE_HOME` with the ceiling, watchdog, oom-steer and scope-reaper
subsystems switched off, so nothing touched the shared daemon or the shared
slice.

- First tick: `UNEVALUATED: CPU is a rate: it needs two samples, and this is the
  first`, and all eight CPU cells `unevaluated`. The honesty rule, live.
- Second tick: `capacity 16.00 cores | slice 14.60 cores | rest of system 1.38
  cores | idle 0.02 cores`, and the eight per-job cells (0.72, 1.21, 1.39, 0.93,
  0.78, 9.34, 0.15, 0.08) sum to exactly 14.60 — the parts reconciling with the
  whole on real data.
- The painted CPU bar row decoded to eight distinct slot colours in slot order
  (75, 77, 209, 141, 80, 185, 205, 107) followed by grey (242) anchored to the
  right edge, with widths proportional to the rates (the 9.34-core job takes 109
  of 188 columns). The same eight colours appear on the table rows and on the RAM
  bar's regions.
- No marker legend line under the CPU bar, as intended.

### Accepted, documented approximation

Per-scope counters are read during the directory scan and the root/slice pair a
few milliseconds later, both stamped with the one server instant. The resulting
error in a rate is the DIFFERENCE between two consecutive ticks' scan durations
over a one-second interval — far below the width of one terminal column, and
below the precision of the kernel's own scheduler-tick accounting. Recorded on
`readConfineCPUFrame` rather than papered over.

### Fix round (Fable BLOCK on PR #84)

Fable's review found a real P2: `topBarLegend` (`cmd/aira/tui.go`) returned a
string ending in its own `\n`, and `renderTopBar` prefixes the marker legend,
OVER-SUBSCRIBED and every note with their OWN leading `\n` — so a panel drawn
at its real fixed height lost its LAST line to the resulting blank line. The
CPU panel has no marker legend to absorb that lost row, so its notes and
OVER-SUBSCRIBED line could never be drawn at all; the RAM panel lost one note
line it used to fit. The fix: drop the trailing `\n` from `topBarLegend`
(restores the RAM panel's layout exactly), and give the CPU panel one more row
(5→6, matching its real worst-realistic-case content: bar, legend, one note,
OVER-SUBSCRIBED). Two simultaneous notes is a real but rare edge neither panel
height budgets for — pre-existing on the RAM panel (unaffected by this fix)
and now consistent on the CPU panel; not chasing it further per this
project's architectural-simplicity rule.

Verified by reintroducing the exact bug and confirming both new tests
(`TestTopBarLegendHasNoTrailingNewline`, `TestTopBarFitsItsPanelHeight`) go
red with the failure signature Fable described, then confirming the fix
restores both to green. Live-verified via tmux capture against the real
production client binary: the RAM panel (which always carries a marker-legend
line, so was live-affected even in the ordinary case) now shows bar / legend /
marker-legend with no blank line, at its real fixed panel height. The CPU
bar's happy-path arithmetic, colour consistency and rest-of-system computation
were already independently verified live by Fable's own dogfood above and are
untouched by this fix (a pure display-layer line-count bug, not a computation
one).

Also fixed Fable's F2 (non-blocking): the ci-shim `mergeConfineRegistry`
pending-row fabricates no `command`, but was missing `cpu` from its
`UnevaluatedFields` list despite `CPUUsageUsec` being nil on that path
(`internal/runner/confine_shim_manage.go`) — added, no test pinned the old
4-item list. F3 (a small duplicated tail between `topBarFor` and
`topCPUBarFor`) is left as-is: Fable itself called it a cleanup opportunity,
not the duplication the ticket warned against, and it is not a correctness
issue.

Gate after the fix round: `aira confine -- go build ./...` exit 0;
`aira confine -- go vet ./...` exit 0; `AIRA_REAL_CGROUP=1 aira confine --
go test ./... -count=1` exit 0, all 15 packages ok.
