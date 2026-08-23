package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	dashboardPage      = "dashboard"
	palettePage        = "palette"
	paletteConfirmPage = "palette-confirm"
	paletteResultPage  = "palette-result"
	inlinePage         = "inline"
	executePage        = "execute"
	executeConfirmPage = "execute-confirm"
)

type tuiRuntime struct {
	app                  *tview.Application
	outerPages           *tview.Pages
	panelPages           *tview.Pages
	tabs                 *tview.TextView
	tables               map[tuiView]*tview.Table
	details              map[tuiView]*tview.TextView
	footers              map[tuiView]*tview.TextView
	insights             *tview.TextView
	paletteList          *tview.List
	paletteConfirmForm   *tview.Form
	paletteSubmitButton  *tview.Button
	paletteSubmit        func()
	paletteConfirmButton *tview.Button
	paletteCancelButton  *tview.Button
	paletteConfirmAction func()
	paletteCancelAction  func()
	executeSubmitButton  *tview.Button
	executeSubmit        func()
	executeConfirmButton *tview.Button
	executeCancelButton  *tview.Button
	executeConfirmAction func()
	executeCancelAction  func()
	state                tuiState
	descriptors          []core.DispatchDescriptor
	executor             *tuiExecutor
	executeDispatcher    Dispatcher
	canExecute           bool
	executeRunning       atomic.Bool
	suspend              func(func()) bool
	scope                daemon.WorktreeScope
	stdin                io.Reader
	stdout               io.Writer
	stderr               io.Writer
	ctx                  context.Context
	cancel               context.CancelFunc
	pumpDone             chan struct{}
	coordDone            chan struct{}
	// queueUpdateStarted is a test-only observation seam. It never carries
	// state and is nil in production.
	queueUpdateStarted chan<- struct{}
}

func runTUI(ctx context.Context, dispatcher, executeDispatcher Dispatcher, scope daemon.WorktreeScope, stdin io.Reader, stdout, stderr io.Writer) int {
	runtime := newTUIRuntime(ctx, dispatcher, executeDispatcher, scope, stdin, stdout, stderr, nil)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	go runTUISignalLoop(runtime.ctx, signals, runtime.executeRunning.Load, runtime.cancel, nil)
	return runTUIRuntime(runtime, stderr)
}

func runTUIWithScreen(ctx context.Context, dispatcher, executeDispatcher Dispatcher, scope daemon.WorktreeScope, stdin io.Reader, stdout, stderr io.Writer, screen tcell.Screen) int {
	runtime := newTUIRuntime(ctx, dispatcher, executeDispatcher, scope, stdin, stdout, stderr, screen)
	return runTUIRuntime(runtime, stderr)
}

func runTUIRuntime(runtime *tuiRuntime, stderr io.Writer) int {
	if err := runtime.run(); err != nil {
		_, _ = fmt.Fprintf(stderr, "E_INTERNAL: tui: %v\n", err)
		return store.ExitForCode("E_INTERNAL")
	}
	return 0
}

func newTUIRuntime(parent context.Context, dispatcher, executeDispatcher Dispatcher, scope daemon.WorktreeScope, stdin io.Reader, stdout, stderr io.Writer, screen tcell.Screen) *tuiRuntime {
	ctx, cancel := context.WithCancel(parent)
	runtime := &tuiRuntime{
		app: tview.NewApplication(), state: newTUIState(512), descriptors: core.New(nil).DispatchDescriptors(),
		ctx: ctx, cancel: cancel, pumpDone: make(chan struct{}), coordDone: make(chan struct{}),
		tables: make(map[tuiView]*tview.Table), details: make(map[tuiView]*tview.TextView), footers: make(map[tuiView]*tview.TextView),
		executeDispatcher: executeDispatcher, canExecute: executeDispatcher != nil, scope: scope,
		stdin: stdin, stdout: stdout, stderr: stderr,
	}
	runtime.state.CanExecute = runtime.canExecute
	if screen != nil {
		runtime.app.SetScreen(screen)
	}
	runtime.executor = newTUIExecutor(ctx, dispatcher, scope, 4)
	runtime.buildWidgets()
	runtime.suspend = runtime.app.Suspend
	return runtime
}

func (r *tuiRuntime) run() error {
	for _, view := range dataViews {
		var commands []tuiCmd
		r.state, commands = requestPanelRefresh(r.state, view)
		r.submitCommands(commands)
	}
	r.render()
	go r.pump()
	go r.coordinateShutdown()
	err := r.app.Run()
	r.cancel()
	<-r.coordDone
	return err
}

func (r *tuiRuntime) buildWidgets() {
	r.tabs = tview.NewTextView().SetDynamicColors(true)
	r.panelPages = tview.NewPages()
	for _, view := range allViews {
		if view == viewInsights {
			r.insights = tview.NewTextView().SetWrap(true)
			r.insights.SetBorder(true).SetTitle(" Insight gauges ")
			r.panelPages.AddPage(string(view), r.insights, true, false)
			continue
		}
		table := tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
		table.SetBorder(true).SetTitle(" " + strings.Title(string(view)) + " ") //nolint:staticcheck
		r.tables[view] = table
		if view == viewTickets || view == viewFindings || view == viewLeases || view == viewReady {
			selectedView := view
			table.SetSelectionChangedFunc(func(row, _ int) {
				model := r.state.Panels[selectedView].Model
				if row < 1 || row > len(model.Rows) {
					return
				}
				var commands []tuiCmd
				r.state, commands = onTUISelect(r.state, selectedView, model.Rows[row-1].ID)
				r.submitCommands(commands)
			})
		}
		footer := tview.NewTextView().SetWrap(false)
		r.footers[view] = footer
		var content tview.Primitive
		if view == viewTickets || view == viewFindings {
			detail := tview.NewTextView().SetWrap(true).SetScrollable(true)
			detail.SetBorder(true).SetTitle(" Detail ")
			r.details[view] = detail
			content = tview.NewFlex().
				AddItem(tview.NewFlex().SetDirection(tview.FlexRow).AddItem(table, 0, 1, true).AddItem(footer, 1, 0, false), 0, 2, true).
				AddItem(detail, 0, 1, false)
		} else {
			content = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(table, 0, 1, true).AddItem(footer, 1, 0, false)
		}
		r.panelPages.AddPage(string(view), content, true, false)
	}
	dashboard := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(r.tabs, 1, 0, false).AddItem(r.panelPages, 0, 1, true)
	r.paletteList = r.makePaletteList()
	r.outerPages = tview.NewPages().
		AddPage(dashboardPage, dashboard, true, true).
		AddPage(palettePage, centeredPrimitive(r.paletteList, 80, 22), true, false)
	r.app.SetRoot(r.outerPages, true)
	r.app.SetInputCapture(r.captureInput)
}

func (r *tuiRuntime) captureInput(event *tcell.EventKey) *tcell.EventKey {
	if r.state.ExecuteOpen {
		if event.Key() == tcell.KeyEscape && r.executeCancelAction != nil && !r.state.ExecuteRunning {
			r.executeCancelAction()
			return nil
		}
		if r.state.ExecuteConfirm != nil {
			if event.Key() == tcell.KeyRune && event.Rune() == 'y' {
				return nil
			}
			if event.Key() == tcell.KeyEnter {
				switch {
				case r.executeCancelButton != nil && r.executeCancelButton.HasFocus():
					r.executeCancelAction()
				case r.executeConfirmButton != nil && r.executeConfirmButton.HasFocus():
					r.executeConfirmAction()
				}
				return nil
			}
		}
		if event.Key() == tcell.KeyEnter && r.executeSubmit != nil && r.executeSubmitButton != nil && r.executeSubmitButton.HasFocus() {
			r.executeSubmit()
			return nil
		}
		return event
	}
	if r.state.PaletteOpen {
		if r.state.PaletteConfirm != nil {
			if event.Key() == tcell.KeyEscape || event.Key() == tcell.KeyRune && event.Rune() == 'n' {
				r.paletteCancelAction()
				return nil
			}
			if event.Key() == tcell.KeyRune && event.Rune() == 'y' {
				return nil
			}
			if event.Key() == tcell.KeyEnter {
				switch {
				case r.paletteCancelButton != nil && r.paletteCancelButton.HasFocus():
					r.paletteCancelAction()
				case r.paletteConfirmButton != nil && r.paletteConfirmButton.HasFocus():
					r.paletteConfirmAction()
				}
				return nil
			}
		}
		if r.state.PaletteDispatching && event.Key() == tcell.KeyEnter {
			return nil
		}
		// Consume keyboard form submission at the application capture layer.
		// This prevents tview's old form child from restoring its focus after a
		// page replacement performed by its Enter handler.
		if event.Key() == tcell.KeyEnter && r.paletteSubmit != nil && r.paletteSubmitButton != nil && r.paletteSubmitButton.HasFocus() {
			r.paletteSubmit()
			return nil
		}
		return event
	}
	key := event.Rune()
	if event.Key() == tcell.KeyTab {
		key = '\t'
	}
	if key == 0 {
		return event
	}
	var commands []tuiCmd
	r.state, commands = onTUIKey(r.state, key, r.descriptors)
	r.render()
	// Input capture already runs on the tview loop. Submitting value commands
	// and rendering inline avoids QueueUpdateDraw's synchronous self-deadlock.
	r.submitCommands(commands)
	if key == ':' {
		r.showPaletteList()
	} else if key == 'x' && r.state.ExecuteOpen {
		r.showExecuteList()
	} else if r.state.InlineError != "" {
		r.showPaletteResult("ERROR " + r.state.InlineError)
	} else if r.state.InlineAction != nil {
		switch {
		case r.state.PaletteConfirm != nil:
			r.showPaletteConfirmation()
		case r.state.InlineAction.Stage == inlineStagePicker:
			r.showInlinePicker()
		case r.state.InlineAction.Stage == inlineStageMiniForm:
			r.showInlineMiniForm()
		}
	}
	return nil
}

func (r *tuiRuntime) submitCommands(commands []tuiCmd) {
	for _, command := range commands {
		if command.Kind == cmdQuit {
			r.cancel()
			return
		}
		// submit is lossless, so there is no drop path that could corrupt a
		// panel's InFlight/PendingRefresh state.
		r.executor.submit(command)
	}
}

func (r *tuiRuntime) pump() {
	defer close(r.pumpDone)
	for {
		select {
		case <-r.ctx.Done():
			return
		case message := <-r.executor.messages:
			if r.queueUpdateStarted != nil {
				select {
				case r.queueUpdateStarted <- struct{}{}:
				default:
				}
			}
			// QueueUpdateDraw is SYNCHRONOUS — it blocks until the tview event
			// loop runs the closure. If that loop has already exited (a normal
			// quit, or an abnormal app.Run() return such as a screen-init
			// failure), the send would block forever, so the coordinator would
			// deadlock waiting on pumpDone. Deliver it abandonably: once the
			// context is cancelled, stop waiting and exit (the orphaned goroutine
			// is harmless — the process is on its way out).
			done := make(chan struct{})
			go func() {
				r.app.QueueUpdateDraw(func() {
					if r.state.ShuttingDown {
						return
					}
					r.applyAsync(message)
					r.render()
				})
				close(done)
			}()
			select {
			case <-done:
			case <-r.ctx.Done():
				return
			}
		}
	}
}

func (r *tuiRuntime) applyAsync(message tuiMessage) {
	var commands []tuiCmd
	switch message.Kind {
	case msgFetchResult:
		r.state, commands = onTUIFetchResult(r.state, message.Fetch)
	case msgRefreshDue:
		r.state, commands = onTUIRefreshDue(r.state, message.View)
	case msgWatchBatch:
		r.state, commands = onTUIWatchBatch(r.state, message.Events, message.Cursor, r.descriptors)
	case msgEOF:
		r.state, commands = onTUIEOF(r.state)
	case msgWatchError:
		panel := r.state.Panels[viewEvents]
		panel.Status, panel.ErrorCode = panelError, message.Code
		r.state.Panels[viewEvents] = panel
	case msgPaletteResult:
		r.state, commands = onPaletteResult(r.state, message.PaletteOutcome, r.descriptors)
		// The result still drives controller convergence after the operator
		// closes the flow, but must not pop a modal over the dashboard.
		if r.state.PaletteOpen {
			r.showPaletteResult(message.PaletteResult)
		}
	case msgDetailResult:
		r.state, commands = onTUIDetailResult(r.state, message.Detail)
	case msgExecuteResume:
		r.state, commands = onExecuteResume(r.state)
	}
	r.submitCommands(commands)
}

func (r *tuiRuntime) coordinateShutdown() {
	defer close(r.coordDone)
	<-r.ctx.Done()
	r.executor.wait()
	<-r.pumpDone
	r.app.Stop()
}

func (r *tuiRuntime) render() {
	tabNames := make([]string, 0, len(allViews))
	for index, view := range allViews {
		name := fmt.Sprintf("%d:%s", index+1, strings.Title(string(view))) //nolint:staticcheck
		if view == r.state.Active {
			name = "[black:white] " + name + " [-:-]"
		}
		tabNames = append(tabNames, name)
	}
	r.tabs.SetText(strings.Join(tabNames, "  ") + "    r refresh  : palette  x execute  q quit")
	r.panelPages.SwitchToPage(string(r.state.Active))
	for _, view := range allViews {
		if view == viewInsights {
			r.renderInsights(r.state.Panels[view])
			continue
		}
		model := r.state.Panels[view].Model
		if view == viewEvents {
			model = eventViewModel(r.state.Events)
		}
		r.renderTable(view, model, r.state.Panels[view])
	}
}

func (r *tuiRuntime) renderTable(view tuiView, model panelModel, panel panelState) {
	table := r.tables[view]
	table.Clear()
	for column, header := range model.Headers {
		table.SetCell(0, column, tview.NewTableCell(header).SetSelectable(false).SetAttributes(tcell.AttrBold))
	}
	for rowIndex, row := range model.Rows {
		for column, cell := range row.Cells {
			tableCell := tview.NewTableCell(cell)
			switch row.Style {
			case "unevaluated":
				tableCell.SetTextColor(tcell.ColorYellow)
			case "expired":
				tableCell.SetTextColor(tcell.ColorRed)
			case "stale":
				tableCell.SetTextColor(tcell.ColorOrange)
			}
			table.SetCell(rowIndex+1, column, tableCell)
		}
	}
	status := model.Footer
	switch panel.Status {
	case panelLoading:
		status = strings.TrimSpace(status + "  loading…")
	case panelError:
		status = strings.TrimSpace(status + "  ERROR " + panel.ErrorCode)
	}
	r.footers[view].SetText(status)
	if detail := r.details[view]; detail != nil {
		detail.SetText(model.Detail)
	}
	if view == viewEvents && len(model.Rows) > 0 {
		table.Select(len(model.Rows), 0)
	}
}

func (r *tuiRuntime) renderInsights(panel panelState) {
	var out strings.Builder
	if panel.Status == panelError {
		fmt.Fprintf(&out, "ERROR %s\n", panel.ErrorCode)
	} else if panel.Status == panelLoading {
		out.WriteString("loading…\n")
	}
	for _, tile := range panel.Model.Tiles {
		fmt.Fprintf(&out, "\n%s\n  %s", tile.Name, tile.Value)
		if tile.ErrorCode != "" {
			fmt.Fprintf(&out, " %s", tile.ErrorCode)
		}
		if tile.Reason != "" {
			fmt.Fprintf(&out, " — %s", tile.Reason)
		}
		if tile.Direction != "" {
			fmt.Fprintf(&out, "\n  direction: %s", tile.Direction)
		}
		if tile.Baseline != "" {
			fmt.Fprintf(&out, "  baseline: %s", tile.Baseline)
		}
		out.WriteByte('\n')
	}
	r.insights.SetText(out.String())
}

func (r *tuiRuntime) makePaletteList() *tview.List {
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(" Command palette ")
	for _, item := range buildPalette(r.descriptors) {
		entry := item
		marker := string(entry.Safety)
		if entry.Destructive {
			marker += " · DESTRUCTIVE"
		}
		list.AddItem(entryKey(entry), marker+" — "+entry.Summary, 0, func() { r.openPaletteEntry(entry) })
	}
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			r.closePalette()
			return nil
		}
		return event
	})
	return list
}

func (r *tuiRuntime) showPaletteList() {
	r.state.InlineAction = nil
	r.state.InlineError = ""
	r.state.PaletteOpen = true
	r.outerPages.HidePage(paletteConfirmPage).HidePage(paletteResultPage).ShowPage(palettePage).SwitchToPage(palettePage)
	r.app.SetFocus(r.paletteList)
}

func (r *tuiRuntime) openPaletteEntry(entry paletteEntry) {
	if len(entry.Args) == 0 {
		request, err := parsePaletteRequest(entry, map[string]string{})
		if err != nil {
			r.showPaletteResult("ERROR " + err.Error())
			return
		}
		r.submitPaletteEntry(entry, request)
		return
	}
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" " + entryKey(entry) + " ")
	fields := make(map[string]*tview.InputField, len(entry.Args))
	for _, arg := range entry.Args {
		label := arg.Spec.Name
		if arg.Required {
			label += " *"
		}
		field := tview.NewInputField().SetLabel(label + ": ")
		if len(arg.Spec.Enum) > 0 {
			field.SetPlaceholder(strings.Join(arg.Spec.Enum, "|"))
		}
		fields[arg.Spec.Name] = field
		form.AddFormItem(field)
	}
	buttonLabel := "Run"
	if entry.Safety != core.SafetyRead {
		buttonLabel = "Continue"
	}
	form.AddButton(buttonLabel, func() {
		r.paletteSubmit()
	})
	r.paletteSubmit = func() {
		values := make(map[string]string, len(fields))
		for name, field := range fields {
			values[name] = field.GetText()
		}
		request, err := parsePaletteRequest(entry, values)
		if err != nil {
			r.showPaletteResult("ERROR " + err.Error())
			return
		}
		r.submitPaletteEntry(entry, request)
	}
	r.paletteSubmitButton = form.GetButton(0)
	form.AddButton("Cancel", r.showPaletteList)
	form.SetCancelFunc(r.showPaletteList)
	r.outerPages.RemovePage(palettePage).AddPage(palettePage, centeredPrimitive(form, 80, len(entry.Args)+6), true, true)
	r.app.SetFocus(form)
}

func (r *tuiRuntime) submitPaletteEntry(entry paletteEntry, request core.Request) {
	var commands []tuiCmd
	r.state, commands = onPaletteSubmit(r.state, entry, request)
	r.submitCommands(commands)
	if r.state.PaletteConfirm != nil {
		r.showPaletteConfirmation()
		return
	}
	if len(commands) > 0 {
		r.showPaletteResult("dispatching…")
	}
}

func (r *tuiRuntime) showPaletteConfirmation() {
	confirm := r.state.PaletteConfirm
	if confirm == nil || r.state.PaletteDispatching {
		return
	}
	details := tview.NewTextView().SetWrap(true).SetText(paletteConfirmationText(confirm))
	details.SetBorder(true).SetTitle(" Resolved request ")
	form := tview.NewForm()
	r.paletteSubmit = nil
	r.paletteSubmitButton = nil
	r.paletteConfirmForm = form
	if confirm.Destructive {
		form.SetBorder(true).SetTitle(" Confirm DESTRUCTIVE mutation ")
	} else {
		form.SetBorder(true).SetTitle(" Confirm mutation ")
	}
	var idField *tview.InputField
	if confirm.Destructive && confirm.ConfirmIDTarget != "" {
		idField = tview.NewInputField().SetLabel("Type " + confirm.ConfirmIDTarget + ": ")
		form.AddFormItem(idField)
	}
	r.paletteConfirmAction = func() {
		var commands []tuiCmd
		r.state, commands = onPaletteConfirm(r.state)
		if len(commands) == 0 {
			return
		}
		r.submitCommands(commands)
		r.showPaletteResult("dispatching…")
	}
	r.paletteCancelAction = func() {
		if r.state.InlineAction != nil {
			r.closeInline()
		} else {
			r.state, _ = onPaletteCancel(r.state)
			r.showPaletteList()
		}
	}
	form.AddButton("Confirm", r.paletteConfirmAction)
	form.AddButton("Cancel", r.paletteCancelAction)
	confirmButton := form.GetButton(0)
	r.paletteConfirmButton = confirmButton
	r.paletteCancelButton = form.GetButton(1)
	confirmButton.SetDisabled(!paletteConfirmEnabled(confirm))
	if idField != nil {
		idField.SetChangedFunc(func(text string) {
			r.state = onPaletteConfirmTypedID(r.state, text)
			confirmButton.SetDisabled(!paletteConfirmEnabled(r.state.PaletteConfirm))
		})
	}
	cancel := r.paletteCancelAction
	form.SetCancelFunc(cancel)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape || event.Key() == tcell.KeyRune && event.Rune() == 'n' {
			cancel()
			return nil
		}
		// There is deliberately no single-key affirmative path.
		if event.Key() == tcell.KeyRune && event.Rune() == 'y' {
			return nil
		}
		return event
	})
	// Buttons follow form items. Confirm is first and Cancel second; focus Cancel
	// so a stray/form-submit Enter cannot affirm the mutation.
	form.SetFocus(form.GetFormItemCount() + 1)
	content := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(details, 0, 1, false).
		AddItem(form, form.GetFormItemCount()+5, 0, true)
	r.outerPages.RemovePage(paletteConfirmPage).
		AddPage(paletteConfirmPage, centeredPrimitive(content, 90, 26), true, true).
		SwitchToPage(paletteConfirmPage)
	r.app.SetFocus(form)
	r.app.SetFocus(form.GetButton(1))
}

func paletteConfirmationText(confirm *paletteConfirm) string {
	if confirm == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Safety: %s\nAction: %s\n", confirm.Safety, confirm.Verb)
	if confirm.Summary != "" {
		fmt.Fprintf(&out, "Summary: %s\n", confirm.Summary)
	}
	names := make([]string, 0, len(confirm.Request.Args))
	for name := range confirm.Request.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&out, "%s → %v\n", name, confirm.Request.Args[name])
	}
	if confirm.ConfirmBlockedReason != "" {
		fmt.Fprintf(&out, "\n⚠ cannot confirm: %s\n", confirm.ConfirmBlockedReason)
	}
	return out.String()
}

func (r *tuiRuntime) showPaletteResult(text string) {
	r.paletteSubmit = nil
	r.paletteSubmitButton = nil
	r.paletteConfirmAction = nil
	r.paletteCancelAction = nil
	r.paletteConfirmButton = nil
	r.paletteCancelButton = nil
	result := tview.NewTextView().SetWrap(true).SetScrollable(true).SetText(text)
	result.SetBorder(true).SetTitle(" Palette result (Esc to close) ")
	result.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			r.closePalette()
			return nil
		}
		return event
	})
	r.state.PaletteOpen = true
	r.outerPages.RemovePage(paletteResultPage).
		AddPage(paletteResultPage, centeredPrimitive(result, 90, 24), true, true).
		SwitchToPage(paletteResultPage)
	r.app.SetFocus(result)
}

func (r *tuiRuntime) closePalette() {
	r.state, _ = onPaletteCancel(r.state)
	r.state.PaletteOpen = false
	r.paletteSubmit = nil
	r.paletteSubmitButton = nil
	r.paletteConfirmAction = nil
	r.paletteCancelAction = nil
	r.paletteConfirmButton = nil
	r.paletteCancelButton = nil
	r.outerPages.HidePage(palettePage).HidePage(paletteConfirmPage).HidePage(paletteResultPage).HidePage(inlinePage).SwitchToPage(dashboardPage)
	if table := r.tables[r.state.Active]; table != nil {
		r.app.SetFocus(table)
	} else {
		r.app.SetFocus(r.insights)
	}
}

func (r *tuiRuntime) showInlinePicker() {
	pending := r.state.InlineAction
	if pending == nil || pending.Stage != inlineStagePicker {
		return
	}
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(" " + pending.Action.Label + " — " + pending.TargetID + " ")
	for _, option := range pending.Options {
		value := option
		list.AddItem(value, "", 0, func() {
			var commands []tuiCmd
			r.state, commands = onInlineActionPick(r.state, value, r.descriptors)
			r.submitCommands(commands)
			switch {
			case r.state.InlineError != "":
				r.showPaletteResult("ERROR " + r.state.InlineError)
			case r.state.PaletteConfirm != nil:
				r.showPaletteConfirmation()
			case r.state.InlineAction != nil && r.state.InlineAction.Stage == inlineStageMiniForm:
				r.showInlineMiniForm()
			}
		})
	}
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			r.closeInline()
			return nil
		}
		return event
	})
	r.outerPages.RemovePage(inlinePage).AddPage(inlinePage, centeredPrimitive(list, 72, len(pending.Options)+4), true, true).SwitchToPage(inlinePage)
	r.app.SetFocus(list)
}

func (r *tuiRuntime) showInlineMiniForm() {
	pending := r.state.InlineAction
	if pending == nil || pending.Stage != inlineStageMiniForm {
		return
	}
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" waive " + pending.TargetID + " ")
	fields := make(map[string]*tview.InputField, len(pending.FormArgs))
	for _, name := range pending.FormArgs {
		field := tview.NewInputField().SetLabel(name + " *: ")
		fields[name] = field
		form.AddFormItem(field)
	}
	r.paletteSubmit = func() {
		values := make(map[string]string, len(fields))
		for name, field := range fields {
			values[name] = field.GetText()
		}
		var commands []tuiCmd
		r.state, commands = onInlineMiniFormSubmit(r.state, values, r.descriptors)
		r.submitCommands(commands)
		if r.state.InlineError != "" {
			r.showPaletteResult("ERROR " + r.state.InlineError)
			return
		}
		if r.state.PaletteConfirm != nil {
			r.showPaletteConfirmation()
		}
	}
	form.AddButton("Continue", func() { r.paletteSubmit() })
	r.paletteSubmitButton = form.GetButton(0)
	form.AddButton("Cancel", r.closeInline)
	form.SetCancelFunc(r.closeInline)
	r.outerPages.RemovePage(inlinePage).AddPage(inlinePage, centeredPrimitive(form, 80, len(fields)+6), true, true).SwitchToPage(inlinePage)
	r.app.SetFocus(form)
}

func (r *tuiRuntime) closeInline() {
	r.state, _ = onInlineActionCancel(r.state)
	r.paletteSubmit = nil
	r.paletteSubmitButton = nil
	r.paletteConfirmAction = nil
	r.paletteCancelAction = nil
	r.paletteConfirmButton = nil
	r.paletteCancelButton = nil
	r.outerPages.HidePage(inlinePage).HidePage(paletteConfirmPage).HidePage(paletteResultPage).SwitchToPage(dashboardPage)
	if table := r.tables[r.state.Active]; table != nil {
		r.app.SetFocus(table)
	}
}

func (r *tuiRuntime) showExecuteList() {
	r.state = onExecuteOpen(r.state)
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(" Foreground execute ")
	for _, item := range buildExecuteList(r.descriptors, r.canExecute) {
		entry := item
		label := entry.Verb
		secondary := entry.Summary
		if entry.PrintOnly {
			secondary = "PRINT ONLY — " + secondary
		}
		if !entry.Enabled {
			label = "[gray]" + entry.Verb + "[-]"
			secondary = "[gray]" + entry.Unavailable
		}
		list.AddItem(label, secondary, 0, func() {
			r.state = onExecuteSelect(r.state, entry)
			if r.state.ExecuteError != "" {
				list.SetTitle(" Foreground execute — " + r.state.ExecuteError + " ")
				return
			}
			r.showExecuteArgForm()
		})
	}
	r.executeCancelAction = r.closeExecute
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			r.closeExecute()
			return nil
		}
		return event
	})
	r.outerPages.RemovePage(executePage).AddPage(executePage, centeredPrimitive(list, 84, 12), true, true).SwitchToPage(executePage)
	r.app.SetFocus(list)
}

func (r *tuiRuntime) showExecuteArgForm() {
	entry := r.state.ExecuteSelected
	if entry == nil {
		return
	}
	form := tview.NewForm()
	field := tview.NewInputField().SetLabel("arguments (include --): ")
	field.SetPlaceholder("-- command arg")
	form.AddFormItem(field)
	form.SetBorder(true).SetTitle(" " + entry.Verb + " foreground arguments ")
	r.executeSubmit = func() {
		var err error
		r.state, err = onExecuteSubmit(r.state, field.GetText())
		if err != nil {
			form.SetTitle(" " + entry.Verb + " — " + err.Error() + " ")
			return
		}
		r.showExecuteConfirmation()
	}
	form.AddButton("Continue", func() { r.executeSubmit() })
	r.executeSubmitButton = form.GetButton(0)
	r.executeCancelAction = r.closeExecute
	form.AddButton("Cancel", r.closeExecute)
	form.SetCancelFunc(r.closeExecute)
	r.outerPages.RemovePage(executePage).AddPage(executePage, centeredPrimitive(form, 92, 8), true, true).SwitchToPage(executePage)
	r.app.SetFocus(form)
}

func (r *tuiRuntime) showExecuteConfirmation() {
	launch := r.state.ExecuteConfirm
	if launch == nil || r.state.ExecuteRunning {
		return
	}
	text := launch.Entry.Verb
	if launch.Entry.PrintOnly {
		text = "PRINT ONLY — no command will be launched\n\n" + launch.ConfineText
	} else {
		text += "\n\n" + executeRequestText(launch.Request)
	}
	details := tview.NewTextView().SetWrap(true).SetText(text)
	details.SetBorder(true).SetTitle(" Resolved foreground request ")
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" Confirm foreground action ")
	r.executeSubmit = nil
	r.executeSubmitButton = nil
	r.executeConfirmAction = r.confirmExecuteOnUI
	r.executeCancelAction = r.closeExecute
	form.AddButton("Confirm", r.executeConfirmAction)
	form.AddButton("Cancel", r.executeCancelAction)
	r.executeConfirmButton = form.GetButton(0)
	r.executeCancelButton = form.GetButton(1)
	form.SetCancelFunc(r.executeCancelAction)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			r.executeCancelAction()
			return nil
		}
		if event.Key() == tcell.KeyRune && event.Rune() == 'y' {
			return nil
		}
		return event
	})
	// As with palette mutations, a stray Enter defaults to Cancel.
	form.SetFocus(1)
	content := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(details, 0, 1, false).
		AddItem(form, 5, 0, true)
	r.outerPages.RemovePage(executeConfirmPage).AddPage(executeConfirmPage, centeredPrimitive(content, 94, 24), true, true).SwitchToPage(executeConfirmPage)
	r.app.SetFocus(form.GetButton(1))
}

func executeRequestText(request core.Request) string {
	var out strings.Builder
	fmt.Fprintf(&out, "verb: %s\n", request.Verb)
	names := make([]string, 0, len(request.Args))
	for name := range request.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&out, "%s → %v\n", name, request.Args[name])
	}
	return out.String()
}

func (r *tuiRuntime) executeLaunchOnUI(launch executeLaunch) {
	if !r.executeRunning.CompareAndSwap(false, true) {
		return
	}
	r.executeLaunchOnUIGuarded(launch)
}

func (r *tuiRuntime) confirmExecuteOnUI() {
	// Publish the atomic signal guard before changing reducer state so there is
	// no confirm→Suspend window in which SIGINT could cancel the TUI.
	if !r.executeRunning.CompareAndSwap(false, true) {
		return
	}
	var launch *executeLaunch
	r.state, launch = onExecuteConfirm(r.state)
	if launch == nil {
		r.executeRunning.Store(false)
		return
	}
	r.executeLaunchOnUIGuarded(*launch)
}

func (r *tuiRuntime) executeLaunchOnUIGuarded(launch executeLaunch) {
	// The atomic clear is intentionally the final deferred action. Signals stay
	// swallowed through screen restoration, Sync, and source-of-truth refresh.
	defer func() {
		r.state = onExecuteComplete(r.state)
		r.resetExecuteWidgets()
		r.outerPages.HidePage(executePage).HidePage(executeConfirmPage).SwitchToPage(dashboardPage)
		r.render()
		r.executeRunning.Store(false)
	}()
	callback := func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				_, _ = fmt.Fprintf(r.executeStderr(), "E_INTERNAL: execute callback panic: %v\n", recovered)
			}
			_, _ = fmt.Fprint(r.executeStdout(), "\nPress Enter to return to AIRA…")
			_, _ = bufio.NewReader(r.executeStdin()).ReadString('\n')
		}()
		report := dispatchExecuteLaunch(r.ctx, r.executeDispatcher, r.scope, launch)
		_, _ = fmt.Fprintf(r.executeStdout(), "\n%s\n", report.String())
	}
	if r.suspend == nil || !r.suspend(callback) {
		_, _ = fmt.Fprintln(r.executeStderr(), "E_TUI_EXECUTE_SUSPEND: not launched; terminal suspension failed")
		return
	}
	r.app.Sync()
	r.applyAsync(tuiMessage{Kind: msgExecuteResume})
}

func (r *tuiRuntime) closeExecute() {
	r.state = onExecuteCancel(r.state)
	r.resetExecuteWidgets()
	r.outerPages.HidePage(executePage).HidePage(executeConfirmPage).SwitchToPage(dashboardPage)
	if table := r.tables[r.state.Active]; table != nil {
		r.app.SetFocus(table)
	} else {
		r.app.SetFocus(r.insights)
	}
}

func (r *tuiRuntime) resetExecuteWidgets() {
	r.executeSubmitButton = nil
	r.executeSubmit = nil
	r.executeConfirmButton = nil
	r.executeCancelButton = nil
	r.executeConfirmAction = nil
	r.executeCancelAction = nil
}

func (r *tuiRuntime) executeStdin() io.Reader {
	if r.stdin == nil {
		return strings.NewReader("\n")
	}
	return r.stdin
}

func (r *tuiRuntime) executeStdout() io.Writer {
	if r.stdout == nil {
		return io.Discard
	}
	return r.stdout
}

func (r *tuiRuntime) executeStderr() io.Writer {
	if r.stderr == nil {
		return io.Discard
	}
	return r.stderr
}

func centeredPrimitive(primitive tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(primitive, height, 0, true).
			AddItem(nil, 0, 1, false), width, 0, true).
		AddItem(nil, 0, 1, false)
}
