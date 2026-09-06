---
{"schema":1,"id":"AIRA-134","project":"aira","title":"aira tui/aira top panics (not a clean error) when the terminal screen fails to initialize","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["honesty","tui"],"hold":false,"relations":[]}
---
Found dogfooding AIRA-127's deployed binary, 2026-09-06. Reproduced on BOTH `aira top`
and the pre-existing `aira tui` — confirmed pre-existing, not a regression from AIRA-127, just
newly hit because `aira top` is a more likely entry point to be invoked from a non-interactive
context (a script, CI, this very investigation via a tool with no real /dev/tty).

## Repro

`aira tui` (or `aira top`) run with no controlling terminal (e.g. `aira tui < /dev/null` in a
context with no /dev/tty):

```
E_INTERNAL: tui: open /dev/tty: no such device or address
panic: close of nil channel

goroutine N [running]:
github.com/gdamore/tcell/v2.(*tScreen).finish(...)
sync.(*Once).doSlow(...)
sync.(*Once).Do(...)
github.com/gdamore/tcell/v2.(*tScreen).Fini(...)
github.com/rivo/tview.(*Application).Stop(...)
main.(*tuiRuntime).coordinateShutdown(...)
    cmd/aira/tui.go:425
created by main.(*tuiRuntime).run in goroutine 1
    cmd/aira/tui.go:165
```

An honest `E_INTERNAL` error IS printed first, but the process then panics rather than exiting
cleanly on that error — a crash, not a clean failure, which is worse than the honest error already
printed (a caller/script sees a panic+stack trace instead of a clean non-zero exit on a parseable
error code).

## Mechanism traced against source (hypothesis, not yet build-verified — check before fixing)

`(*tuiRuntime).run()` (cmd/aira/tui.go:150-170):
```go
go r.pump()
go r.coordinateShutdown()   // started BEFORE app.Run()
err := r.app.Run()          // blocks; can return immediately if screen init fails
r.cancel()                  // cancels r.ctx as cleanup, unconditionally
<-r.coordDone
return err
```
`coordinateShutdown()` (tui.go:420-426):
```go
defer close(r.coordDone)
<-r.ctx.Done()
r.executor.wait()
<-r.pumpDone
r.app.Stop()                // tview -> tcell Fini() -> close(quit-channel)
```

`coordinateShutdown`'s `ctx.Done()` wakes for TWO structurally different reasons that the current
code does not distinguish: (a) an EXTERNAL cancellation while `app.Run()` is genuinely still live
with a successfully-initialised screen (the normal --detach/SIGINT teardown path, where calling
`Stop()` correctly and safely unblocks the running event loop) -- versus (b) `run()`'s OWN
`r.cancel()` firing as unconditional cleanup AFTER `app.Run()` already returned on its own (e.g.
because screen init itself failed and the screen was NEVER brought up). In case (b), `app.Stop()`
calls into tcell's `Fini()` on a screen whose `Init()` never completed, and `Fini()`'s `sync.Once`
still fires (first call) and closes a channel that was never allocated -- the nil-channel panic.

## Suggested fix direction (verify, do not just implement blindly)

Distinguish the two cases: track whether `app.Run()` has already returned (e.g. an atomic bool or a
channel closed immediately after `err := r.app.Run()`), and have `coordinateShutdown` skip the
`app.Stop()` call when `run()` has already finished on its own -- only call `Stop()` when it is
actually needed to unblock a still-live `Run()` loop from outside. Verify this hypothesis against
tview/tcell's actual semantics (does `Application.Run()` distinguish an init failure from a normal
return in its own state? is there a public/internal signal to check rather than inventing one?)
before committing to this exact shape -- a recover()-based defensive guard around the specific
known tcell panic, with a comment explaining exactly why, is an acceptable fallback if the
flag-based ordering fix turns out to be fragile or if tview's own API does not expose enough to do
it cleanly.

## Tests

A regression test that forces the screen-init-failure path (however tview/tcell allow that to be
simulated/injected in this codebase's existing TUI test harness -- check how the existing TUI tests
already fake tcell/tview, if they do) and asserts the process/function returns the E_INTERNAL error
cleanly with NO panic, verified by reverting the fix and confirming the test panics/fails against
the old code.

## Resolution (in-review)

Branch `aira134-tui-screen-init-panic`. The traced hypothesis is **confirmed exactly**,
line for line, against tview v0.42.0 and tcell v2.13.10.

### Mechanism, build-verified

- `tview.(*Application).Run()` (application.go:279) creates its own screen only when
  none was injected. Its two early error returns differ in a way that matters:
  `tcell.NewScreen()` failing leaves `a.screen` **nil**, but `a.screen.Init()` failing
  returns the error while **leaving `a.screen` set to the uninitialised screen**.
- `tcell.(*tScreen).Init()` (tscreen.go:189) calls `initialize()` first, which is the
  `NewDevTty()` open of `/dev/tty`. On failure it returns BEFORE
  `t.quit = make(chan struct{})` (tscreen.go:232), so `t.quit` stays nil.
- `tview.(*Application).Stop()` (application.go:624) then calls `screen.Fini()`, and
  `tScreen.Fini` → `finiOnce.Do(finish)` → `close(t.quit)` on a nil channel.
- `coordinateShutdown`'s `<-r.ctx.Done()` did not distinguish an external cancellation
  (a live `app.Run()` that Stop() must unblock) from `run()`'s own unconditional
  `r.cancel()` after `app.Run()` had already returned by itself.

Two findings beyond the report:

1. The crash is worse than "panic after the honest error". `run()` blocks on
   `<-r.coordDone`, so the coordinator panics *before* `run()` returns — the
   `E_INTERNAL` line is never printed at all. Verified: pre-fix `aira top` in a
   terminal-less context emits only the panic.
2. The existing `tcell.SimulationScreen` harness **cannot** reach this bug, for two
   independent reasons: `simscreen.Fini` guards `if s.quit != nil` (simulation.go:153),
   and an injected screen makes tview's `Run()` skip the create-and-Init block entirely,
   so `Run()` never returns early. This is why the regression test is a subprocess.

Rejected alternatives:

- Pre-`Init()`ing our own screen and handing it to `SetScreen`: `SetScreen`
  unconditionally calls `Init()` itself and swallows the error, and a second
  `tScreen.Init()` reallocates `quit`/`eventQ` and then fails `engage()` with
  "already engaged". Actively harmful.
- Gating on "the screen drew at least once" (`SetAfterDrawFunc`): fails in the wrong
  direction — a cancellation arriving before the first draw would skip a Stop() that a
  live `Run()` needs, converting a rare panic into a rare **hang**.
- A `recover()` guard: not needed. The ordering fix is not fragile (see below), and the
  project rule is to keep the primitive rather than stack defensive machinery.

### Fix

`cmd/aira/tui.go`, in the code both `aira tui` and `aira top` share:

- New `tuiRuntime.appRunDone` channel, closed in `run()` **immediately after**
  `app.Run()` returns and **before** `r.cancel()`, so the coordinator woken by that very
  cancel observes it.
- `coordinateShutdown` still joins the executor and the pump, then calls `app.Stop()`
  only if `appRunDone` is still open.

Skipping Stop() once Run() has returned is always correct: a Run() that returned
normally already nil'd tview's screen (Stop() would be a no-op), and a Run() that
returned on a screen-init failure never brought a terminal up to restore. The ordering
is not racy in the direction that panics: the only uninit'd-screen return is tview's
synchronous early return microseconds into `Run()`, which cannot coexist with a
coordinator that has already completed `executor.wait()` and `<-pumpDone` on an
external cancel.

### Tests

`cmd/aira/tui_screen_init_test.go` —
`TestTUIScreenInitFailureExitsCleanlyWithoutPanic`. The test re-execs the test binary
with `SysProcAttr{Setsid: true}` and non-tty standard streams, so the child has no
controlling terminal and tcell's `/dev/tty` open genuinely fails; `TERM=xterm` is pinned
so `tcell.NewScreen()` still succeeds and the failure lands in `Init()` (an unset TERM
would fail earlier, leave tview holding no screen, and false-pass). It asserts no
`panic:`, the `E_INTERNAL: tui:` line, and exit 4. The child checks both preconditions
itself and exits 97 with a marker if either is unmet, so the parent reports
**unevaluated** (t.Skip) rather than a fake pass.

Non-porosity, both directions:

- Reverting to the unconditional `r.app.Stop()` makes the new test FAIL with the exact
  reported stack (`panic: close of nil channel` → `tScreen.finish` → `tview.Stop` →
  `coordinateShutdown`).
- Making the skip unconditional (never Stop()) makes the pre-existing
  `TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight` FAIL with "TUI quit
  deadlocked or did not complete". The normal interactive teardown is therefore still
  pinned by an existing test.

Manual confirmation on the real binary: `setsid --wait ./aira tui|top </dev/null` now
prints `E_INTERNAL: tui: open /dev/tty: no such device or address` and exits 4 for both
verbs, with no panic; and driven on a real pty (`script -qec './aira top'`) the
dashboard renders and `q` still quits cleanly with exit 0 — i.e. Stop() is still called
for a live screen.

### Gates

`aira confine -- go build ./...` exit 0; `aira confine -- go vet ./...` exit 0;
`AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` exit 0.

## Review (Fable build-review gate) — MERGED

PR #81 merged as `2daa590` (2026-09-06). Everything below is the reviewer's own
reproduction, not the builder's transcript.

- Mechanism re-read in the library source, not the citations: tview `Run()`
  (application.go:288-297) leaves `a.screen` set on an `Init()` failure; tcell
  `tScreen.Init()` (tscreen.go:190) returns from `initialize()` before `t.quit` is made
  at :232; `Stop()` (:624-634) → `Fini()` → `finish()` → `close(t.quit)` (:374).
  `simscreen.Fini` guards nil (simulation.go:153) and an injected screen skips the
  create-and-Init block entirely, so the subprocess harness is genuinely required.
- Rejected alternatives hold: `SetScreen` (application.go:210) calls `Init()`
  unconditionally and discards the error, and a second `tScreen.Init()` reallocates
  `keychan`/`quit`/`eventQ` (:194, :232-233) under the old loops before `engage()`
  (:1173) refuses "already engaged". A single-arm first-draw gate would indeed hang.
- Original panic reproduced on a pre-fix build (`setsid --wait ./aira {top,tui}
  </dev/null`, `TERM=xterm`): exit 2, `panic: close of nil channel` via
  `coordinateShutdown` (tui.go:425), both verbs. One correction to the Resolution: on
  `tui` the `E_INTERNAL` line DID print before the panic — `defer close(coordDone)` runs
  during unwinding and races `run()`'s print against the runtime's fatal exit — so
  "never printed at all" is one outcome of a race, not a certainty.
- Fixed build, same repro: exit 4, the single
  `E_INTERNAL: tui: open /dev/tty: no such device or address` line, no panic, both verbs.
- Non-porosity re-run by the reviewer: fix reverted → the new test FAILS with the exact
  stack; never-Stop over-correction →
  `TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight` FAILS "TUI quit deadlocked
  or did not complete".
- Real pty (`script -qec`): `q` → exit 0; SIGINT → exit 0; no panic, no leftover process.
- Gates from a clean tree at `0ad3bc9`: build 0, vet 0,
  `AIRA_REAL_CGROUP=1 go test ./... -count=1` 0 (14 packages ok, 0 FAIL).

### Finding: residual startup-window race — ACCEPTED GAP (corrects the Resolution)

The Resolution's claim that tview's failing-`Init()` return "cannot coexist with a
coordinator that has already completed executor.wait() and <-pumpDone on an external
cancel" is FALSE. Reviewer probe (throwaway, not committed): a context already cancelled
at `run()` entry, in a Setsid/no-tty child with `TERM=xterm`, 300 runs against the FIXED
code → **37 panics (12%)** with the identical stack via the fixed `Stop()` (tui.go:462),
263 clean `E_INTERNAL`, 0 hangs. Mechanism: the coordinator wakes on the pending cancel,
joins executor and pump, sees `appRunDone` still open (Run() has not returned yet),
calls `Stop()`, which blocks on `a.Lock()` until Run()'s failing `Init()` unlocks — with
`a.screen` still set — and the nil close fires.

Reachability: production passes `context.Background()` (main.go:189/386), so the only
trigger is a SIGINT/SIGTERM delivered inside the sub-millisecond window between
`go coordinateShutdown()` and the `/dev/tty` open failing, in a context that has no tty
(so no Ctrl-C); the pre-cancelled probe is a strict upper bound on that. If it is ever
hit the outcome is the pre-existing crash (exit 2 instead of 4) in a process that was
failing anyway — no hang (0/300), no state damage. Accepted under the keep-the-primitive
rule rather than stacking a `recover()`; the deterministic P1 as reported is closed and
pinned by the regression test.

If it ever matters: gate the coordinator on BOTH arms —
`select { case <-appRunDone: return; case <-firstDraw: Stop() }` — which closes the window
with no hang (the single-arm first-draw gate rejected above was rightly rejected), noting
the smoke tests' own `SetAfterDrawFunc` use is a single slot that would need a seam; or
a `recover()` scoped to the one `Stop()` call.
