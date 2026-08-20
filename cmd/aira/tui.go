package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	dashboardPage     = "dashboard"
	palettePage       = "palette"
	paletteResultPage = "palette-result"
)

type tuiRuntime struct {
	app         *tview.Application
	outerPages  *tview.Pages
	panelPages  *tview.Pages
	tabs        *tview.TextView
	tables      map[tuiView]*tview.Table
	details     map[tuiView]*tview.TextView
	footers     map[tuiView]*tview.TextView
	insights    *tview.TextView
	paletteList *tview.List
	state       tuiState
	descriptors []core.DispatchDescriptor
	executor    *tuiExecutor
	ctx         context.Context
	cancel      context.CancelFunc
	pumpDone    chan struct{}
	coordDone   chan struct{}
	// queueUpdateStarted is a test-only observation seam. It never carries
	// state and is nil in production.
	queueUpdateStarted chan<- struct{}
}

func runTUI(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, stderr io.Writer) int {
	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runTUIWithScreen(signalCtx, dispatcher, scope, stderr, nil)
}

func runTUIWithScreen(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, stderr io.Writer, screen tcell.Screen) int {
	runtime := newTUIRuntime(ctx, dispatcher, scope, screen)
	if err := runtime.run(); err != nil {
		_, _ = fmt.Fprintf(stderr, "E_INTERNAL: tui: %v\n", err)
		return store.ExitForCode("E_INTERNAL")
	}
	return 0
}

func newTUIRuntime(parent context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, screen tcell.Screen) *tuiRuntime {
	ctx, cancel := context.WithCancel(parent)
	runtime := &tuiRuntime{
		app: tview.NewApplication(), state: newTUIState(512), descriptors: core.New(nil).DispatchDescriptors(),
		ctx: ctx, cancel: cancel, pumpDone: make(chan struct{}), coordDone: make(chan struct{}),
		tables: make(map[tuiView]*tview.Table), details: make(map[tuiView]*tview.TextView), footers: make(map[tuiView]*tview.TextView),
	}
	if screen != nil {
		runtime.app.SetScreen(screen)
	}
	runtime.executor = newTUIExecutor(ctx, dispatcher, scope, 4)
	runtime.buildWidgets()
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
		if view == viewTickets || view == viewFindings {
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
	if r.state.PaletteOpen {
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
	r.state, commands = onTUIKey(r.state, key)
	r.render()
	// Input capture already runs on the tview loop. Submitting value commands
	// and rendering inline avoids QueueUpdateDraw's synchronous self-deadlock.
	r.submitCommands(commands)
	if key == ':' {
		r.showPaletteList()
	}
	return nil
}

func (r *tuiRuntime) submitCommands(commands []tuiCmd) {
	for _, command := range commands {
		if command.Kind == cmdQuit {
			r.cancel()
			return
		}
		if !r.executor.submit(command) && r.ctx.Err() == nil {
			panel := r.state.Panels[command.View]
			panel.Status, panel.ErrorCode, panel.InFlight = panelError, "E_TUI_BUSY", false
			r.state.Panels[command.View] = panel
		}
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
			r.app.QueueUpdateDraw(func() {
				if r.state.ShuttingDown {
					return
				}
				r.applyAsync(message)
				r.render()
			})
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
		r.showPaletteResult(message.PaletteResult)
	case msgDetailResult:
		r.state, commands = onTUIDetailResult(r.state, message.Detail)
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
	r.tabs.SetText(strings.Join(tabNames, "  ") + "    r refresh  : palette  q quit")
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
	list.SetBorder(true).SetTitle(" Read-only palette ")
	for _, item := range buildPalette(r.descriptors) {
		entry := item
		list.AddItem(entryKey(entry), entry.Summary, 0, func() { r.openPaletteEntry(entry) })
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
	r.state.PaletteOpen = true
	r.outerPages.ShowPage(palettePage).SwitchToPage(palettePage)
	r.app.SetFocus(r.paletteList)
}

func (r *tuiRuntime) openPaletteEntry(entry paletteEntry) {
	if len(entry.Args) == 0 {
		request, err := parsePaletteRequest(entry, map[string]string{})
		if err != nil {
			r.showPaletteResult("ERROR " + err.Error())
			return
		}
		r.runPaletteRequest(request)
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
	form.AddButton("Run", func() {
		values := make(map[string]string, len(fields))
		for name, field := range fields {
			values[name] = field.GetText()
		}
		request, err := parsePaletteRequest(entry, values)
		if err != nil {
			r.showPaletteResult("ERROR " + err.Error())
			return
		}
		r.runPaletteRequest(request)
	})
	form.AddButton("Cancel", r.showPaletteList)
	form.SetCancelFunc(r.showPaletteList)
	r.outerPages.RemovePage(palettePage).AddPage(palettePage, centeredPrimitive(form, 80, len(entry.Args)+6), true, true)
	r.app.SetFocus(form)
}

func (r *tuiRuntime) runPaletteRequest(request core.Request) {
	if !r.executor.submitPalette(request) {
		r.showPaletteResult("ERROR E_TUI_BUSY")
		return
	}
	r.showPaletteResult("loading…")
}

func (r *tuiRuntime) showPaletteResult(text string) {
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
	r.outerPages.RemovePage(paletteResultPage).AddPage(paletteResultPage, centeredPrimitive(result, 90, 24), true, true)
	r.app.SetFocus(result)
}

func (r *tuiRuntime) closePalette() {
	r.state.PaletteOpen = false
	r.outerPages.HidePage(palettePage).HidePage(paletteResultPage).SwitchToPage(dashboardPage)
	if table := r.tables[r.state.Active]; table != nil {
		r.app.SetFocus(table)
	} else {
		r.app.SetFocus(r.insights)
	}
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
