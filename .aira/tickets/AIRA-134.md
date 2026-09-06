---
{"schema":1,"id":"AIRA-134","project":"aira","title":"aira tui/aira top panics (not a clean error) when the terminal screen fails to initialize","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["honesty","tui"],"hold":false,"relations":[]}
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
