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
- A refusal shows `REJECTED E_X` then the daemon's message with raw byte counts and
  scope ids, in a fixed 90x24 box mostly empty.
- The confirmation is two stacked boxes (90x26) listing `selector → CONFINE-…`,
  `slice →` (empty), `steal → false` above a separate button box; at 80x24 both are
  clipped on the left (the box is wider than the terminal).
- The dashboard behind every dialog is blanked.

## Spec

1. Button focus unmistakable without colour: focused button reverse video + bold
   with a `▶ label ◀` marker, unfocused plain/dim with no marker. Tab / Shift-Tab /
   Left / Right move focus, Enter presses the focused button, Esc cancels. A one-line
   key hint on every dialog. Destructive / queue-affecting confirmations start on
   Cancel (unchanged).
2. No raw JSON by default. Every palette / row-action result (applied, rejected,
   outcome unknown, client-side refusal) is one ONE shared formatter's plain-English
   lines: what happened to which job (name, owner), what changed (resize: "Reserve
   for <job> lowered from 256M to 128M; 128M returned to the pool"), and for an
   error the stable code, the daemon's message (byte counts shown as sizes) and a
   remedy line for the codes a top action can return. The raw payload stays behind
   a labelled `d` Details toggle, closed by default.
3. Edit forms (the `e` reserve form, the waive reason/actor form): title, context
   line (current reserve, live use), field with units, a live status line under the
   field (invalid size / not smaller / below live use / "256M → 128M, frees 128M"),
   buttons, hint. Validation on Enter reports inline under the field, not as a
   pop-up. Same layout for every dialog.
4. Dialogs size to their content, are centred, are clamped to the terminal (80x24
   included), and leave the dashboard visible around them.
5. Help/legend text updated where it describes these dialogs.

Not in scope: what any action does (resize, push-to-front, kill), daemon/protocol
behaviour, AIRA-340 --priority. Client-only change.

## As built: how the dialogs behave (the operator-facing reference)

`aira top` has no dispatch-table help entry (`top` is in `verbsWithoutHelpEntry`), so
each dialog carries its own key line, and this section is the reference for it.

- Every dialog is one family (`cmd/aira/tui_dialog.go`): title, plain-text body, an
  optional form (fields, a status line under them, buttons), an optional raw
  `Details` panel, and a key hint.
- Buttons: the focused one is reverse video and bold with `▶ label ◀`; the others are
  dim. Tab / Shift-Tab / ← / → move, Enter presses the marked button, Esc cancels.
  Confirmations start on Cancel.
- Results: one or two sentences led by APPLIED, REJECTED, NOT SENT or UNEVALUATED. A
  daemon error shows its code, its message (byte counts as sizes, scope ids as job
  names) and a "What to do" line; it claims nothing about whether a change was made.
  `d` opens and closes the raw reply.
- Sizes print exactly when they are whole MiB (`2047M`, `2G`, `1.5G`) and with `~`
  when rounded (`~1177.4M`, a live memory reading).
- On a short terminal the body and details shrink first, then the form drops its
  blank rows, then the form scrolls to the focused item; the hint stays inside the
  border, and whatever is cut off scrolls with PgUp/PgDn (the details when open,
  else the body), which the hint then says.
