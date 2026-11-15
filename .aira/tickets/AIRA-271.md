---
{"schema":1,"id":"AIRA-271","project":"aira","title":"aira top: kill the selected confine job via double-uppercase K (KK, ownership-guarded) — a TUI kill action, follow-on to AIRA-269","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.44","labels":["feature"],"hold":false,"relations":[]}
---

## Spec (the greenfield minimum)

`aira top` kills the selected job with the CLI's own `aira confine --kill`
(verb `confine-kill`), never `--steal`: the daemon's owner check
(`runner.CheckConfineOwner`) decides, and its refusal is what the screen shows.
No daemon, wire or protocol change; a client-only change.

- `K` on a row ARMS a kill of that row's job and does nothing else; the status
  line says what is armed (`KILL ARMED: <name> (owner …, command …)`, or
  `CANCEL ARMED` for a queued row). A second `K` on the SAME row opens the
  confirmation; `K` on another row arms that one instead. Any other key
  (arrows, Esc, Enter included) disarms and then does what it does. The arm
  times out after five top refreshes (about 5 s) and drops at once when its job
  leaves the listing. `K` is not a key inside a dialog, the palette, the
  execute launcher or while a request is in flight.
- The confirmation is AIRA-342's dialog (title `Confirm DESTRUCTIVE mutation`,
  Cancel focused first, `y` does nothing): it names the job (name, owner,
  command head of 60 runes), says a running job is killed at once (cgroup.kill)
  and a queued one is cancelled before it starts (and killed if admitted
  meanwhile), and that it is sent as the caller without `--steal`.
- The result is AIRA-342's formatter, which now also says the reply's `phase`
  (`Status: killed` / `Phase: running`, `Status: cancelled` / `Phase: queued`).
  `U_CONFINE_KILL_UNCONFIRMED` (cgroup.kill may have been written, the end not
  proven) reads `UNEVALUATED — outcome unknown` with the daemon's code, never
  "refused" or "killed", and refreshes the views.

## As built

- `cmd/aira/tui_inline.go`: `onTopKillKey` (arm / re-arm / open),
  `topKillConfirm` (the page and the request), `onTopKillDisarm`.
- `cmd/aira/tui_controller.go`: `tuiState.TopKill` (`topKillArm`), the disarm
  on any non-K rune, the time-out in the top fetch result, `tableRow.KillText`.
- `cmd/aira/tui_top.go`: `topKillText`, `topPrintable` (split out of
  `topUntrustedCell`).
- `cmd/aira/tui.go`: the disarm on non-rune keys, the legend (`KK kill job`)
  and the armed status line (it replaces the whole tab row, job first, so the
  full dashboard's long tab row cannot push it off screen), the destructive
  title for `confine-kill`, and bracketed paste on (Sol round 1: a pasted
  `KK<Tab><Enter>` reached the key handlers as four keys and could confirm a
  kill in one gesture; a paste now goes to the focused widget as text, on
  every TUI face).
- `cmd/aira/dispatcher.go`: `DispatchPalette` admits `confine-kill` through the
  one-exchange management arm `f` and `e` use (no old-daemon hint for it).
  With the daemon down the TUI says the request was not sent; the CLI's
  daemon-down local cgroup.kill fallback is not reached from the TUI.
- `cmd/aira/tui_executor.go`, `cmd/aira/tui_result.go`: `unconfirmedResult`
  for `U_CONFINE_KILL_UNCONFIRMED`; `phase` in the reply fields.
- `internal/core/skill.go`: the `--kill` sentence names `K` pressed twice.

Not built: `--steal` from the TUI; a typed-name confirmation; a timer for the
time-out (it rides the top refresh tick).
