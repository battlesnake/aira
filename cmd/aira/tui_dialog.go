package main

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// AIRA-342. Every modal the TUI faces open (`aira top`'s row actions, the
// `aira tui` palette and its confirmations, the foreground launcher, every
// result) is ONE dialog family built here, so a fix to how a dialog looks or
// behaves reaches all of them at once:
//
//   - a bordered, titled box, centred and clamped to the terminal (dialogFrame);
//   - a plain-text body, an optional form (fields, a status line directly under
//     them, buttons), an optional raw-details panel behind `d` (closed by
//     default), and a key hint;
//   - on a short terminal the body and the details shrink first and the form
//     next (its blank rows go, then it scrolls to the focused item), so the
//     buttons stay reachable and the hint stays inside the border; whatever is
//     cut off scrolls with PgUp/PgDn (the details when open, else the body),
//     and the hint says so;
//   - buttons whose focus reads without colour: the focused one is reverse video
//     and bold AND carries a ▶ ◀ marker, the others are dim and unmarked; Tab,
//     Shift-Tab, Left and Right move between them; on a dialog too narrow for
//     them side by side they stack, one per row, and the key hint wraps whole
//     keys onto further rows, so neither the focus marker nor a key is cut.
//
// Nothing here decides what a dialog does: Enter, Esc and the safe default focus
// stay with the callers (and with the runtime's application-level capture).

const (
	// The key hint is built from these parts. Each part is one key and what it
	// does and is never split: a dialog too narrow for the hint on one row puts
	// whole parts on further rows (dialogHintLines).
	dialogHintDetails = "d details"
	dialogHintScroll  = "PgUp/PgDn scroll"
	dialogHintJoin    = " · "
	// dialogDetailsHeader heads the open details panel.
	dialogDetailsHeader = "Details (d hides):\n"

	// dialogMaxWidth is the widest a dialog grows to, whatever its content.
	dialogMaxWidth = 84
	// dialogMinWidth is the narrowest a dialog with a form shrinks to while the
	// terminal allows it, so buttons and a field always fit side by side.
	dialogMinWidth = 44
)

var (
	// dialogHintButtons and dialogHintResult lead the hint of a dialog with
	// buttons and of a result.
	dialogHintButtons = []string{"Tab/←→ move", "Enter presses ▶", "Esc cancels"}
	dialogHintResult  = []string{"Esc closes"}

	// dialogButtonFocused is the focused button: reverse video plus bold over the
	// dialog's own colours. Neither attribute depends on a colour, so it survives
	// 8/16-colour terminals and NO_COLOR (tcell drops colours there, never
	// attributes).
	dialogButtonFocused = tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor).Reverse(true).Bold(true)
	// dialogButtonIdle is every other button: dim and unfilled, on the dialog's
	// background (not the terminal default, which can differ from it).
	dialogButtonIdle = tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor).Dim(true)
	// dialogButtonDisabled is a button that cannot be pressed (the destructive
	// Confirm until its id is typed): dim and struck through.
	dialogButtonDisabled = dialogButtonIdle.StrikeThrough(true)
	// dialogFieldStyle keeps tview's coloured field and adds an underline, so an
	// input field is still visible where colour is not.
	dialogFieldStyle = tcell.StyleDefault.Background(tview.Styles.ContrastBackgroundColor).Foreground(tview.Styles.PrimaryTextColor).Underline(true)
)

// dialogButtonLabel is a button's label in either focus state. Both forms have
// the same width, so moving focus never shifts the button row.
func dialogButtonLabel(label string, focused bool) string {
	if focused {
		return "▶ " + label + " ◀"
	}
	return "  " + label + "  "
}

// styleDialogForm applies the dialog family's form look to form: the button
// styles, the focus marker on every button already added, the field style, and
// Left/Right as Shift-Tab/Tab while a button has focus (inside a text field the
// arrows still move the cursor). Call it after the last AddButton.
func styleDialogForm(form *tview.Form) {
	form.SetButtonStyle(dialogButtonIdle).
		SetButtonActivatedStyle(dialogButtonFocused).
		SetButtonDisabledStyle(dialogButtonDisabled).
		SetFieldStyle(dialogFieldStyle).
		SetButtonsAlign(tview.AlignCenter).
		SetItemPadding(1).
		SetBorderPadding(0, 0, 0, 0)
	for index := 0; index < form.GetButtonCount(); index++ {
		button := form.GetButton(index)
		label := button.GetLabel()
		button.SetLabel(dialogButtonLabel(label, button.HasFocus()))
		button.SetFocusFunc(func() { button.SetLabel(dialogButtonLabel(label, true)) })
		button.SetBlurFunc(func() { button.SetLabel(dialogButtonLabel(label, false)) })
	}
	previous := form.GetInputCapture()
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if _, button := form.GetFocusedItemIndex(); button >= 0 {
			switch event.Key() {
			case tcell.KeyLeft:
				event = tcell.NewEventKey(tcell.KeyBacktab, 0, tcell.ModNone)
			case tcell.KeyRight:
				event = tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone)
			}
		}
		if previous != nil {
			return previous(event)
		}
		return event
	})
}

// dialogFormHeight is the rows a styled dialog form needs with padding blank
// rows after each item: each item takes its own height plus the padding, and the
// button row follows (after one blank row when the padding is zero, which is
// how tview lays a vertical form out).
func dialogFormHeight(form *tview.Form, padding int) int {
	if form == nil {
		return 0
	}
	rows := 1
	if padding == 0 {
		rows++
	}
	for index := 0; index < form.GetFormItemCount(); index++ {
		rows += max(form.GetFormItem(index).GetFieldHeight(), 1) + padding
	}
	return rows
}

// dialogStatusRows is the height of a form's status line: two rows, so a
// message that wraps once is still read whole.
const dialogStatusRows = 2

// newDialogStatus is a form's status line: a read-only, non-scrollable text item
// placed directly under the fields (AIRA-342: a refused value is reported under
// the field, not in a pop-up). A non-scrollable text view in a tview form hands
// focus straight on, so Tab and Shift-Tab pass over it.
func newDialogStatus() *tview.TextView {
	status := tview.NewTextView().SetWrap(true).SetWordWrap(true).SetScrollable(false).SetDynamicColors(true)
	status.SetSize(dialogStatusRows, 0)
	return status
}

// dialogSpec describes one dialog. Body and Details are plain text: they are
// escaped before display, so a job name or a daemon message can never inject a
// tview style tag.
type dialogSpec struct {
	Title string
	Body  string
	// Form holds the fields, the status line (newDialogStatus) and the
	// buttons; nil for a result.
	Form *tview.Form
	// Details is the raw payload or the exact request, behind `d`; empty = none.
	Details string
	// OnClose runs on Esc when no form (whose cancel func owns Esc) is present.
	OnClose func()
}

// dialogView is a built dialog: the frame to add as a page, the primitive to
// focus first, and the details toggle's state.
type dialogView struct {
	frame   *dialogFrame
	root    *tview.Flex
	body    *tview.TextView
	details *tview.TextView
	hint    *tview.TextView
	form    *tview.Form
	// formBox draws form, stacking its buttons when the dialog is too narrow
	// for them side by side; nil without a form.
	formBox     *dialogFormBox
	title       string
	bodyText    string
	detailsText string
	detailsOpen bool
	// baseHint is the key hint's parts before the scroll part, which size adds
	// only while something is cut off.
	baseHint []string
	focus    tview.Primitive
}

func newDialog(spec dialogSpec, focusIsTextField func() bool) *dialogView {
	view := &dialogView{
		form: spec.Form, title: spec.Title,
		bodyText: spec.Body, detailsText: spec.Details,
	}
	// Neither text view parses style tags (dynamic colours are off), so the text
	// is set verbatim: escaping it would show the escape ("[1,2[]").
	view.body = tview.NewTextView().SetWrap(true).SetWordWrap(true).SetScrollable(true).SetText(spec.Body)
	view.details = tview.NewTextView().SetWrap(true).SetWordWrap(true).SetScrollable(true)
	view.details.SetTextColor(tview.Styles.SecondaryTextColor)
	hint := dialogHintResult
	if spec.Form != nil && spec.Form.GetButtonCount() > 0 {
		hint = dialogHintButtons
	}
	if spec.Details != "" {
		hint = append(append([]string(nil), hint...), dialogHintDetails)
	}
	view.baseHint = hint
	view.hint = tview.NewTextView().SetWrap(true).SetWordWrap(true).SetText(strings.Join(hint, dialogHintJoin)).SetTextStyle(tcell.StyleDefault.Dim(true))
	view.root = tview.NewFlex().SetDirection(tview.FlexRow)
	view.root.SetBorder(true).SetTitle(" "+strings.TrimSpace(spec.Title)+" ").SetBorderPadding(0, 0, 1, 1)
	view.root.AddItem(view.body, 1, 0, spec.Form == nil)
	view.root.AddItem(view.details, 0, 0, false)
	if spec.Form != nil {
		view.formBox = &dialogFormBox{Form: spec.Form}
		view.root.AddItem(view.formBox, dialogFormHeight(spec.Form, 1), 0, true)
	}
	view.root.AddItem(view.hint, 1, 0, false)
	view.root.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyRune && event.Rune() == 'd' && view.detailsText != "" && (focusIsTextField == nil || !focusIsTextField()) {
			view.toggleDetails()
			return nil
		}
		if event.Key() == tcell.KeyPgUp || event.Key() == tcell.KeyPgDn {
			// Whatever has the focus (a field, a button, the body), PgUp/PgDn
			// scroll the text: the details when open, else the body.
			target := view.body
			if view.detailsOpen {
				target = view.details
			}
			target.InputHandler()(event, func(tview.Primitive) {})
			return nil
		}
		if event.Key() == tcell.KeyEscape && spec.Form == nil && spec.OnClose != nil {
			spec.OnClose()
			return nil
		}
		return event
	})
	// The form when there is one, else the (scrollable) body.
	view.focus = view.body
	if spec.Form != nil {
		view.focus = spec.Form
	}
	view.frame = newDialogFrame(view.root, view.size)
	return view
}

// toggleDetails opens or closes the raw-details panel; the frame re-sizes on the
// next draw.
func (view *dialogView) toggleDetails() {
	view.detailsOpen = !view.detailsOpen
	if view.detailsOpen {
		view.details.SetText(dialogDetailsHeader + view.detailsText).ScrollToBeginning()
	} else {
		view.details.SetText("")
	}
}

// size lays the dialog out for at most maxWidth x maxHeight cells and returns
// the size it wants, which always fits: the body and the details shrink (and
// scroll) first, then the form loses its blank rows, then the form itself
// shrinks and scrolls to its focused item, so the hint stays inside the border
// and the buttons stay reachable.
func (view *dialogView) size(maxWidth, maxHeight int) (int, int) {
	natural := textWidth(view.bodyText)
	if open := textWidth(view.detailsText); view.detailsOpen && open > natural {
		natural = open
	}
	natural = max(natural, tview.TaggedStringWidth(view.title)+6, tview.TaggedStringWidth(strings.Join(view.baseHint, dialogHintJoin)))
	width := natural + 4 // border and side padding
	if view.form != nil && width < dialogMinWidth {
		width = dialogMinWidth
	}
	width = min(width, dialogMaxWidth, maxWidth)
	inner := max(width-4, 1)
	// Buttons too wide to sit side by side go one per row, so none is clipped
	// (a clipped focused button loses its ▶ ◀ marker).
	stackRows := 0
	if view.formBox != nil {
		view.formBox.stacked = dialogButtonRowWidth(view.form) > inner
		if view.formBox.stacked {
			stackRows = view.form.GetButtonCount() - 1
		}
	}

	bodyWant := wrappedRows(view.bodyText, inner)
	detailsWant := 0
	if view.detailsOpen {
		detailsWant = wrappedRows(dialogDetailsHeader+view.detailsText, inner)
	}
	avail := max(maxHeight-2, 1) // inside the border
	// lay out the form, the gap under the body and the text for hintRows of hint.
	layout := func(hintRows int) (formRows, gap, bodyRows, detailsRows int) {
		left := avail - hintRows
		minText := min(bodyWant+detailsWant, 1)
		if view.form != nil {
			padding := 1
			formRows = dialogFormHeight(view.form, padding) + stackRows
			if formRows+1+minText > left {
				padding = 0
				formRows = dialogFormHeight(view.form, padding) + stackRows
			}
			view.form.SetItemPadding(padding)
			formRows = max(min(formRows, left-minText), 1)
			if bodyWant > 0 && formRows+1+bodyWant+detailsWant <= left {
				gap = 1
			}
		}
		room := max(left-formRows-gap, 0)
		bodyRows, detailsRows = bodyWant, detailsWant
		if bodyRows+detailsRows > room {
			if detailsRows > 0 {
				bodyRows = min(bodyRows, max(room/2, min(room, 1)))
				detailsRows = max(room-bodyRows, 0)
			} else {
				bodyRows = room
			}
		}
		return formRows, gap, bodyRows, detailsRows
	}
	// The hint takes every row it needs (the keys are what makes the dialog
	// usable), leaving at least one row for the rest; the text shrinks first.
	hintRows := func(lines []string) int { return max(min(len(lines), avail-1), 1) }
	hint := dialogHintLines(view.baseHint, inner)
	formRows, gap, bodyRows, detailsRows := layout(hintRows(hint))
	if bodyRows < bodyWant || detailsRows < detailsWant {
		hint = dialogHintLines(append(append([]string(nil), view.baseHint...), dialogHintScroll), inner)
		formRows, gap, bodyRows, detailsRows = layout(hintRows(hint))
	}
	view.hint.SetText(strings.Join(hint, "\n"))
	view.root.ResizeItem(view.body, bodyRows+gap, 0)
	view.root.ResizeItem(view.details, detailsRows, 0)
	if view.formBox != nil {
		view.root.ResizeItem(view.formBox, formRows, 0)
	}
	view.root.ResizeItem(view.hint, hintRows(hint), 0)
	return width, 2 + bodyRows + gap + detailsRows + formRows + hintRows(hint)
}

// dialogHintLines lays the hint's parts out in rows of at most width cells,
// joined by " · ", never splitting a part (a part wider than width gets a row of
// its own and wraps there).
func dialogHintLines(parts []string, width int) []string {
	var lines []string
	for _, part := range parts {
		last := len(lines) - 1
		if last >= 0 && tview.TaggedStringWidth(lines[last]+dialogHintJoin+part) <= width {
			lines[last] += dialogHintJoin + part
			continue
		}
		lines = append(lines, part)
	}
	return lines
}

// dialogButtonRowWidth is the width tview's form gives its buttons side by side:
// each label plus four cells, one cell between buttons.
func dialogButtonRowWidth(form *tview.Form) int {
	width := -1
	for index := 0; index < form.GetButtonCount(); index++ {
		width += tview.TaggedStringWidth(form.GetButton(index).GetLabel()) + 5
	}
	return width
}

// dialogFormBox draws a dialog's form. tview lays a form's buttons out in one
// row and clips whichever does not fit; when stacked is set (the row is wider
// than the dialog) it draws them one per row instead, centred, in the rows the
// dialog's layout added under the form. The buttons stay the form's own, so
// focus, keys and mouse are unchanged.
type dialogFormBox struct {
	*tview.Form
	stacked bool
}

func (b *dialogFormBox) Draw(screen tcell.Screen) {
	count := b.GetButtonCount()
	x, y, width, height := b.GetRect()
	extra := count - 1
	if !b.stacked || count < 2 || height-extra < 1 {
		b.Form.Draw(screen)
		return
	}
	// tview draws the form, its button row included, in the rows above the
	// extra ones; the full rect is restored after, so a click on a stacked
	// button still reaches the form.
	b.Form.SetRect(x, y, width, height-extra)
	b.Form.Draw(screen)
	b.Form.SetRect(x, y, width, height)
	_, row, _, _ := b.GetButton(0).GetRect()
	if row < y || row >= y+height-extra {
		return // the form has scrolled its buttons out of view
	}
	background := tcell.StyleDefault.Background(b.GetBackgroundColor())
	for line := row; line <= row+extra; line++ {
		for column := x; column < x+width; column++ {
			screen.SetContent(column, line, ' ', nil, background)
		}
	}
	for index := 0; index < count; index++ {
		button := b.GetButton(index)
		buttonWidth := min(tview.TaggedStringWidth(button.GetLabel())+4, width)
		button.SetRect(x+(width-buttonWidth)/2, row+index, buttonWidth, 1)
		button.Draw(screen)
	}
}

// wrappedRows is how many rows text takes when word-wrapped at width, by the
// wrapping tview's TextView itself uses. An empty line still takes a row.
func wrappedRows(text string, width int) int {
	if text == "" {
		return 0
	}
	rows := 0
	for _, line := range strings.Split(text, "\n") {
		rows += max(1, len(tview.WordWrap(tview.Escape(line), width)))
	}
	return rows
}

// textWidth is the widest line of text, in cells.
func textWidth(text string) int {
	widest := 0
	for _, line := range strings.Split(text, "\n") {
		widest = max(widest, tview.TaggedStringWidth(tview.Escape(line)))
	}
	return widest
}

// dialogFrame centres its content inside the page it is drawn on and clamps it
// to that page minus a one-cell margin, so no dialog overflows a small terminal
// (80x24 included) and the dashboard stays visible around it. size reports the
// content's wanted size for the room available; it runs on every draw, so a
// dialog whose content changes (a status line, the details toggle) re-sizes.
type dialogFrame struct {
	*tview.Box
	content tview.Primitive
	size    func(maxWidth, maxHeight int) (int, int)
}

func newDialogFrame(content tview.Primitive, size func(maxWidth, maxHeight int) (int, int)) *dialogFrame {
	return &dialogFrame{Box: tview.NewBox(), content: content, size: size}
}

// centeredPrimitive frames primitive at a fixed preferred size, clamped to the
// terminal like every other dialog.
func centeredPrimitive(primitive tview.Primitive, width, height int) tview.Primitive {
	return newDialogFrame(primitive, func(int, int) (int, int) { return width, height })
}

func (f *dialogFrame) Draw(screen tcell.Screen) {
	x, y, width, height := f.GetRect()
	room := func(total int) int {
		if total > 4 {
			return total - 2
		}
		return total
	}
	maxWidth, maxHeight := room(width), room(height)
	if maxWidth < 1 || maxHeight < 1 {
		return
	}
	w, h := f.size(maxWidth, maxHeight)
	w, h = max(min(w, maxWidth), 1), max(min(h, maxHeight), 1)
	left, top := x+(width-w)/2, y+(height-h)/2
	background := tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor)
	for row := top; row < top+h; row++ {
		for column := left; column < left+w; column++ {
			screen.SetContent(column, row, ' ', nil, background)
		}
	}
	f.content.SetRect(left, top, w, h)
	f.content.Draw(screen)
}

func (f *dialogFrame) Focus(delegate func(tview.Primitive)) { delegate(f.content) }

func (f *dialogFrame) HasFocus() bool { return f.content.HasFocus() }

func (f *dialogFrame) InputHandler() func(*tcell.EventKey, func(tview.Primitive)) {
	return f.WrapInputHandler(func(event *tcell.EventKey, setFocus func(tview.Primitive)) {
		if handler := f.content.InputHandler(); handler != nil {
			handler(event, setFocus)
		}
	})
}

func (f *dialogFrame) PasteHandler() func(string, func(tview.Primitive)) {
	return f.content.PasteHandler()
}

func (f *dialogFrame) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	return f.content.MouseHandler()
}
