package main

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// AIRA-342. The look every TUI dialog shares: one bordered box holding a
// plain-text body, an optional notice line (a refused value, said in place), an
// optional form, and one key-hint line. What a dialog does stays with its caller.

const (
	dialogHintButtons = "Tab/←→ move · Enter presses ▶ · Esc cancels"
	dialogHintResult  = "Esc closes"
	dialogHintDetails = " · d details"
	dialogHintScroll  = " · PgUp/PgDn scroll"
	// dialogMaxWidth is the widest a dialog grows; dialogMinWidth the narrowest
	// it shrinks to while the terminal has room, so a field and two buttons fit.
	dialogMaxWidth = 80
	dialogMinWidth = 50
)

var (
	// dialogButtonFocused is reverse video and bold, dialogButtonIdle dim: the
	// focus reads from attributes alone, which survive NO_COLOR and 16-colour
	// terminals. The ▶ ◀ label marker (dialogButtonLabel) says it a second way.
	dialogButtonFocused = tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor).Reverse(true).Bold(true)
	dialogButtonIdle    = tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor).Dim(true)
)

// dialogButtonLabel is a button's label; both forms have the same width, so
// moving the focus never shifts the button row.
func dialogButtonLabel(label string, focused bool) string {
	if focused {
		return "▶ " + label + " ◀"
	}
	return "  " + label + "  "
}

// styleDialogButtons gives form's buttons the shared focus look and makes Left
// and Right move between them (inside a text field they still move the cursor).
// Call it after the last AddButton.
func styleDialogButtons(form *tview.Form) {
	form.SetButtonStyle(dialogButtonIdle).SetButtonActivatedStyle(dialogButtonFocused).SetButtonsAlign(tview.AlignCenter)
	for index := 0; index < form.GetButtonCount(); index++ {
		button := form.GetButton(index)
		label := button.GetLabel()
		button.SetLabel(dialogButtonLabel(label, false))
		button.SetFocusFunc(func() { button.SetLabel(dialogButtonLabel(label, true)) })
		button.SetBlurFunc(func() { button.SetLabel(dialogButtonLabel(label, false)) })
	}
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if _, button := form.GetFocusedItemIndex(); button >= 0 {
			switch event.Key() {
			case tcell.KeyLeft:
				return tcell.NewEventKey(tcell.KeyBacktab, 0, tcell.ModNone)
			case tcell.KeyRight:
				return tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone)
			}
		}
		return event
	})
}

// dialog is one built dialog. body and notice show text verbatim (no style
// tags are parsed), so a job name or daemon message is never reinterpreted.
type dialog struct {
	column             *tview.Flex
	body, notice, hint *tview.TextView
	form               *tview.Form
	bodyText, hintText string
	details            string
	detailsOpen        bool
}

// newDialog builds a dialog. form may be nil (a result); details, when not
// empty, is shown under the body only after `d`. Only a result (no form) is
// given details: its body has the focus, so its arrows and PgUp/PgDn scroll
// what `d` revealed. With a form, the form keeps the focus and PgUp/PgDn are
// passed to the body, so a body taller than the screen can still be read.
func newDialog(title, body string, form *tview.Form, details string) *dialog {
	d := &dialog{form: form, bodyText: body, details: details}
	d.body = tview.NewTextView().SetWrap(true).SetWordWrap(true).SetScrollable(true).SetText(body)
	d.notice = tview.NewTextView().SetWrap(true).SetWordWrap(true)
	d.notice.SetTextStyle(tcell.StyleDefault.Bold(true).Foreground(tcell.ColorRed))
	d.hintText = dialogHintResult
	if form != nil {
		d.hintText = dialogHintButtons
		styleDialogButtons(form)
		form.SetItemPadding(1).SetBorder(false).SetBorderPadding(0, 0, 0, 0)
	}
	if details != "" {
		d.hintText += dialogHintDetails
	}
	d.hint = tview.NewTextView().SetWrap(true).SetWordWrap(true).SetText(d.hintText).SetTextStyle(tcell.StyleDefault.Dim(true))
	d.column = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(d.body, 0, 1, form == nil)
	if form != nil {
		// A blank row, then the notice (a refused value) above the form.
		d.column.AddItem(nil, 1, 0, false).AddItem(d.notice, 0, 0, false).AddItem(form, 1, 0, true)
	}
	d.column.AddItem(d.hint, 1, 0, false)
	d.column.SetBorder(true).SetTitle(" "+strings.TrimSpace(title)+" ").SetBorderPadding(0, 0, 1, 1)
	d.column.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if d.form != nil && (event.Key() == tcell.KeyPgUp || event.Key() == tcell.KeyPgDn) {
			d.body.InputHandler()(event, func(tview.Primitive) {})
			return nil
		}
		if event.Key() == tcell.KeyRune && event.Rune() == 'd' && d.details != "" {
			d.detailsOpen = !d.detailsOpen
			text := d.bodyText
			if d.detailsOpen {
				text += "\n\nDetails (d hides):\n" + d.details
			}
			d.body.SetText(text)
			return nil
		}
		return event
	})
	return d
}

// setNotice shows text (a refused value, why) on its own full-width line above
// the form; empty hides it. Only a dialog with a form has the line.
func (d *dialog) setNotice(text string) { d.notice.SetText(text) }

// page is the dialog centred on the terminal, sized to its content.
func (d *dialog) page() tview.Primitive { return newDialogFrame(d.column, d.size) }

// size fits the dialog in maxWidth x maxHeight. The form and the hint keep
// their rows; the notice gets what is left after them (its head, the code,
// first); the body shrinks first, so the buttons and a refusal stay on screen
// and nothing is drawn outside the box. When a form's body does not fit, the
// hint says PgUp/PgDn scroll it.
func (d *dialog) size(maxWidth, maxHeight int) (int, int) {
	width := max(textWidth(d.body.GetText(false)), textWidth(d.hintText)) + 4
	if d.form != nil {
		width = max(width, dialogMinWidth)
	}
	width = min(width, dialogMaxWidth, maxWidth)
	inner := max(width-4, 1)
	rows := 2 // the border
	bodyRows := wrappedRows(d.body.GetText(false), inner)
	noticeRows := wrappedRows(d.notice.GetText(false), inner)
	formRows := 0
	if d.form != nil {
		formRows = 1 + 2*d.form.GetFormItemCount() // each field and the blank row under it, then the buttons
		d.column.ResizeItem(d.form, formRows, 0)
		rows += 1 + formRows // the blank row and the form
	}
	hint := d.hintText
	if d.form != nil && rows+wrappedRows(hint, inner)+noticeRows+bodyRows > maxHeight {
		hint += dialogHintScroll
	}
	d.hint.SetText(hint)
	hintRows := wrappedRows(hint, inner)
	d.column.ResizeItem(d.hint, hintRows, 0)
	rows += hintRows
	if d.form != nil {
		noticeRows = min(noticeRows, max(maxHeight-rows, 0))
		d.column.ResizeItem(d.notice, noticeRows, 0)
		rows += noticeRows
	}
	return width, rows + bodyRows
}

// wrappedRows is how many rows text takes word-wrapped at width.
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

// dialogFrame centres its content on the page and clamps it to the page less a
// one-cell margin, so no dialog runs off an 80x24 terminal and the dashboard
// stays visible around it. size runs on every draw, so a dialog whose text
// changes (a notice, the details) re-sizes.
type dialogFrame struct {
	*tview.Box
	content tview.Primitive
	size    func(maxWidth, maxHeight int) (int, int)
}

func newDialogFrame(content tview.Primitive, size func(maxWidth, maxHeight int) (int, int)) *dialogFrame {
	return &dialogFrame{Box: tview.NewBox(), content: content, size: size}
}

func (f *dialogFrame) Draw(screen tcell.Screen) {
	x, y, width, height := f.GetRect()
	maxWidth, maxHeight := max(width-2, 1), max(height-2, 1)
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
