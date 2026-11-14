---
{"schema":1,"id":"AIRA-342","project":"aira","title":"aira top: make the job-edit dialogs (reserve size, front, confirmations, results) clear and pleasant: obvious button focus, plain-English results, no raw JSON","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.42","labels":[],"hold":false,"relations":[]}
---

## Owner complaint (2026-11-14, verbatim)

"in aira top, when editing a job (e.g. reservation size, front) then the UI/UX is
kind of ugly. Not certain which of two buttons is the focussed one (colour/style
isn't clear), at some point it shows me a window full of json. Make it nicer, more
intuitive."

## Reproduced (base 4d935a0c, real `aira top` in a private tmux at 120x40 and 80x24)

- Buttons: unfocused `Confirm` is white on bright blue, focused `Cancel` is bright
  blue on bright white (tview defaults): two filled boxes, no marker, which one is
  focused depends on reading the colour scheme.
- A successful `e` resize shows `APPLIED` followed by the daemon's JSON payload
  pretty-printed (`{ "name": ..., "new_reserve_bytes": 134217728, ... }`).
- A refusal shows `REJECTED E_X` then the daemon's message, in a fixed 90x24 box
  mostly empty; a mistyped size replaces the form with an `ERROR` pop-up.
- The confirmation is two stacked boxes (90x26) listing `selector → CONFINE-…`,
  `slice →` (empty), `steal → false` above a separate button box; at 80x24 both are
  clipped on the left (the box is wider than the terminal).
- A verdict reply (`gate check` with no gates: code `UNEVALUATED`, data code
  `U_GATE_SET_EMPTY`) is shown as "malformed daemon response".

## Spec (the greenfield minimum)

1. Button focus unmistakable without colour: the focused button is reverse video
   and bold with a `▶ label ◀` marker, the others dim and unmarked. Tab /
   Shift-Tab / Left / Right move focus, Enter presses the focused button, Esc
   cancels, Cancel is the initial focus of a confirmation (unchanged). One fixed
   key-hint line on every dialog.
2. No raw JSON by default. ONE result formatter builds the text only from fields
   the daemon reply carries; a missing field is never shown as 0 or empty, and a
   change is stated only when the reply carries both its before and after
   values. An unrecognised reply reads "the daemon accepted the request" or "the
   daemon refused the request" with its code and message, and nothing more. A
   verdict reply keeps its verdict and code. The raw reply is behind `d`.
3. Edit form: the current value shown, the CLI's size syntax, and a refused value
   said on a plain full-width line ABOVE the form, wrapped to the dialog width.

Not in scope: what any action does (resize, push-to-front, kill), daemon/protocol
behaviour. Client-only change.

## As built

- `cmd/aira/tui_dialog.go`: one dialog shape for the `aira top` row-action form,
  every confirmation and every result: a bordered, titled box holding the body
  text, then (with a form) a blank row, the notice line and the form, then the
  key hint. `styleDialogButtons` gives every button the shared focus look
  (focused: reverse + bold + `▶ ◀`; idle: dim) and maps Left/Right to
  Shift-Tab/Tab while a button has focus; the palette's argument form uses it too.
  `dialogFrame` centres the box, sized to its content (50 to 80 columns), clamped
  to the terminal less a one-cell margin; the body is what shrinks when rows run
  short, so the form, the notice and the hint keep their rows.
- `cmd/aira/tui_result.go`: the one reply formatter. First line: `APPLIED: the
  daemon accepted the request.`, `VERDICT <PASS|FAIL|UNEVALUATED>: <meaning>.` or
  `REJECTED: the daemon refused the request.` plus the message exactly as the CLI
  prints it (`CODE: message`). Then one line per field the reply carries:
  `Name` (with owner), `Id`, `Status`, `Code`, `Message`, `Reason`, `Error`,
  `Hint`, `Reserve: <old> → <new>` only when both byte counts are present, and the
  reply's warnings. Details (`d`) is the reply envelope as indented JSON, closed by
  default, shown in the same scrollable body. Outcome-unknown and not-sent results
  keep their existing wording; the classifier now accepts a verdict code in place
  of OK (the old `Code != "OK"` check made every verdict "malformed").
- The `e` form says the job, its owner, current reserve and live use (as of the
  keypress) and the size syntax; a refused value (bad size, required field
  missing) is said above the form, which stays open with the field focused, and
  editing the field clears it. Enter in the field moves to Continue; Enter there
  submits. The confirmation puts the request's exact arguments behind `d` when it
  has a Summary (the `aira top` row actions), and in the body otherwise (the
  palette).
- Accepted limit: below about 60 columns a dialog is clipped on the right (the
  button row) and its body may be cut short; at 80x24 the focused button, the
  notice and the hint are all on screen, and the notice stays whole down to 28
  columns.
- Tests (`cmd/aira/tui_dialog_test.go`, a real `aira top` runtime on a tcell
  simulation screen): focus attributes asserted directly (reverse, bold, dim,
  marker) at 80x24, Left/Right/Tab, Enter on the focused button only, Cancel
  default, Esc; no JSON before `d`, `d` shows and hides it; the refused size whole
  and above the field at 80 and 28 columns, cleared by an edit; the verdict reply
  keeps `UNEVALUATED` and `U_GATE_SET_EMPTY`; a reply with only the new size
  claims no change. Each was mutation-checked: the implementation was broken and
  the test went red.

## Why collapsed

The first build (+2079 lines: a 490-line dialog family with stacked-button and
hint-wrapping layout, a 338-line formatter with per-verb sentences, size
humanising, scope-id rewriting and per-code remedies, and a daemon/runner constant
export) went through three review rounds that each ended with a blocker (9, 4
and 3 P2 findings), every time in that new machinery rather than in the core
fixes. Per the owner's rule it was re-based on the minimum above. Deleted: the
`dialogFormBox` stacked-button wrapper, multi-row hint wrapping, the
shrink-then-scroll layout and PgUp/PgDn plumbing, per-verb sentences
(`appliedSentence`, request-value outlines), `humanSize` and message humanising,
the remedy table and its ci-shim cause split, and the exported resize-reason
constants in `internal/daemon` and `internal/runner` (reverted: the change is
client-only).

## Deferrals

- The dashboard is still blanked behind a dialog (pages switch, as before).
- The `aira tui` foreground-launcher dialogs (`x`) keep tview's default buttons.
- Daemon messages are shown verbatim (byte counts and scope ids are not
  rewritten); per-code "what to do" lines are not added.
