package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/testdeadline"

	"github.com/gdamore/tcell/v2"
)

// AIRA-342. The owner's complaint, made executable on a real `aira top` runtime
// over a simulated terminal: which button has focus must be readable from the
// cell attributes alone (no colour), Left/Right/Tab move it, Enter presses the
// focused button and nothing else, a result is plain English with no JSON on
// screen until `d` asks for the raw payload, and the dialogs fit 80x24.
//
// verifies: AIRA-342

const dialogTestJob = "CONFINE-alpha-101-aa@session-a"

// topDialogDispatcher serves one live job (alpha, 8G reserved, 1G in use) and
// one queued job, and answers a confine write with reply, recording it. It
// carries send-evidence (DispatchPalette), as the real daemon dispatcher does,
// so a refusal is a provable rejection rather than outcome-unknown.
type topDialogDispatcher struct {
	mu    sync.Mutex
	reply core.Response
	sent  []core.Request
}

func (d *topDialogDispatcher) Dispatch(ctx context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	switch request.Verb {
	case "watch":
		<-ctx.Done()
		return core.Response{Code: daemon.CodeUnavailable, Error: ctx.Err().Error()}
	case "confine-list":
		listing := topTestListing(topTestFrame(), topTestRecord(dialogTestJob, "alpha", 8*gib, gib))
		listing.Queued = []runner.ConfineRecord{topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", 4*gib)}
		data, _ := json.Marshal(listing)
		return core.Response{OK: true, Code: "OK", RawData: data}
	case "confine-resize", "confine-front":
		d.mu.Lock()
		defer d.mu.Unlock()
		d.sent = append(d.sent, clonePaletteRequest(request))
		return d.reply
	}
	return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{}`)}
}

func (d *topDialogDispatcher) DispatchPalette(ctx context.Context, scope daemon.WorktreeScope, request core.Request) paletteDispatchAttempt {
	return paletteDispatchAttempt{Response: d.Dispatch(ctx, scope, request), Send: paletteSendMayHaveBeenSent}
}

func (d *topDialogDispatcher) writes() []core.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]core.Request(nil), d.sent...)
}

// startTopDialogRuntime runs a real `aira top` runtime on a simulated screen of
// the given size, as the job's own session, and returns once alpha is listed.
// It never resizes the screen afterwards, so each test draws at one size.
func startTopDialogRuntime(t *testing.T, dispatcher *topDialogDispatcher, width, height int) (*tuiRuntime, tcell.SimulationScreen, *keyTracker) {
	t.Helper()
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	ctx, cancel := context.WithCancel(context.Background())
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(width, height)
	runtime := newTopRuntime(ctx, dispatcher, nil, nil, nil, screen)
	keys := trackKeys(runtime, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-testdeadline.After(5 * time.Second):
			t.Error("top runtime did not exit after cancel")
		}
	})
	// The simulation screen takes its size at Init, so the size is applied once
	// the loop runs. At 80x24 the three bars leave the table no visible rows, so
	// readiness is the model holding alpha, not alpha on screen.
	resizeBoardScreen(t, runtime, screen, width, height)
	eventuallyOnUI(t, runtime, testdeadline.Wait(10*time.Second), func() bool {
		for _, row := range runtime.state.Panels[viewTop].Model.Rows {
			if row.ID == dialogTestJob {
				return true
			}
		}
		return false
	}, "alpha never reached the top model")
	return runtime, screen, keys
}

// openResizeConfirmation selects alpha, presses `e`, types size into the form
// and presses Enter in the field, which submits it.
func openResizeConfirmation(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, keys *keyTracker, size string) {
	t.Helper()
	onUIDraw(t, runtime, func() struct{} {
		for index, row := range runtime.state.Panels[viewTop].Model.Rows {
			if row.ID == dialogTestJob {
				runtime.tables[viewTop].Select(index+1, 0)
			}
		}
		return struct{}{}
	})
	keys.press(t, runeKey('e'))
	waitForSimulationText(t, runtime, screen, "New reserve")
	keys.typeText(size)
	keys.press(t, specialKey(tcell.KeyEnter))
	waitForSimulationText(t, runtime, screen, "Lower this job's reserve?")
}

// buttonCell is how one button label is drawn: the attributes of its first
// letter, and whether the ▶ marker sits right before it.
type buttonCell struct {
	found   bool
	reverse bool
	marked  bool
}

// readButton finds label on the simulated screen (on the UI goroutine).
func readButton(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, label string) buttonCell {
	t.Helper()
	return onUI(t, runtime, func() buttonCell {
		cells, width, height := screen.GetContents()
		for row := 0; row < height; row++ {
			line := make([]rune, width)
			for column := 0; column < width; column++ {
				line[column] = ' '
				if runes := cells[row*width+column].Runes; len(runes) > 0 {
					line[column] = runes[0]
				}
			}
			at := strings.Index(string(line), label)
			if at < 0 {
				continue
			}
			column := len([]rune(string(line)[:at]))
			_, _, attrs := cells[row*width+column].Style.Decompose()
			marked := column >= 2 && line[column-2] == '▶'
			return buttonCell{found: true, reverse: attrs&tcell.AttrReverse != 0, marked: marked}
		}
		return buttonCell{}
	})
}

func assertFocusedButton(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, focused, other, when string) {
	t.Helper()
	got, idle := readButton(t, runtime, screen, focused), readButton(t, runtime, screen, other)
	if !got.found || !idle.found {
		t.Fatalf("%s: buttons not drawn (%s=%+v %s=%+v)", when, focused, got, other, idle)
	}
	if !got.reverse || !got.marked {
		t.Fatalf("%s: focused %s is not reverse video with a ▶ marker: %+v", when, focused, got)
	}
	if idle.reverse || idle.marked {
		t.Fatalf("%s: unfocused %s looks focused: %+v", when, other, idle)
	}
}

// verifies: AIRA-342 item 1 — the focused button is reverse video AND marked ▶ ◀,
// the other is neither; Cancel is the initial focus of the resize confirmation;
// Left, Right and Tab move the focus; Enter presses the focused button only.
func TestDialogButtonFocusIsReadableWithoutColour(t *testing.T) {
	dispatcher := &topDialogDispatcher{reply: core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{}`)}}
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, 100, 32)
	openResizeConfirmation(t, runtime, screen, keys, "2G")

	text := simulationTextOnUI(t, runtime, screen)
	if !strings.Contains(text, "Tab/←→ move") || !strings.Contains(text, "Esc cancels") {
		t.Fatalf("the confirmation carries no key hint:\n%s", text)
	}
	assertFocusedButton(t, runtime, screen, "Cancel", "Confirm", "initial focus")
	keys.press(t, specialKey(tcell.KeyLeft))
	assertFocusedButton(t, runtime, screen, "Confirm", "Cancel", "after Left")
	keys.press(t, specialKey(tcell.KeyRight))
	assertFocusedButton(t, runtime, screen, "Cancel", "Confirm", "after Right")

	// Enter on the initially focused Cancel closes the dialog and sends nothing.
	keys.press(t, specialKey(tcell.KeyEnter))
	time.Sleep(20 * time.Millisecond) // negative wait: a write must not appear
	if writes := dispatcher.writes(); len(writes) != 0 {
		t.Fatalf("Enter on Cancel sent %+v", writes)
	}

	openResizeConfirmation(t, runtime, screen, keys, "2G")
	keys.press(t, specialKey(tcell.KeyTab))
	assertFocusedButton(t, runtime, screen, "Confirm", "Cancel", "after Tab")
	keys.press(t, specialKey(tcell.KeyEnter))
	testdeadline.Eventually(t, time.Second, func() bool { return len(dispatcher.writes()) == 1 }, "Enter on Confirm sent nothing")
	if write := dispatcher.writes()[0]; write.Verb != "confine-resize" || write.Args["memory_reserve"] != "2G" || write.Args["selector"] != dialogTestJob {
		t.Fatalf("Enter on Confirm sent %+v, want the confine-resize of alpha to 2G", write)
	}
}

// verifies: AIRA-342 item 2 — a refusal reads as the verdict, the code, the
// message with sizes instead of byte counts and the job's name instead of its
// scope id, and what to do; no JSON is on screen until `d`, which shows the raw
// payload and hides it again.
func TestDialogRefusalIsPlainEnglishWithDetailsBehindD(t *testing.T) {
	dispatcher := &topDialogDispatcher{reply: core.Response{
		Code:  runner.CodeConfineResizeNotReduction,
		Error: runner.CodeConfineResizeNotReduction + ": " + dialogTestJob + " reserves 8589934592 bytes; a resize must name a smaller reserve (asked for 9663676416)",
	}}
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, 100, 32)
	openResizeConfirmation(t, runtime, screen, keys, "9G")
	keys.press(t, specialKey(tcell.KeyTab), specialKey(tcell.KeyEnter))
	text := waitForSimulationText(t, runtime, screen, "Not applied")
	for _, want := range []string{
		"REJECTED (" + runner.CodeConfineResizeNotReduction + "): nothing was changed.",
		"alpha reserves 8G; a resize must name a smaller reserve (asked for 9G)",
		"What to do: a resize only ever lowers a reserve",
		"Esc closes · d details",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "8589934592") || strings.Contains(text, "CONFINE-alpha") {
		t.Fatalf("the refusal shows raw bytes or the scope id before d:\n%s", text)
	}
	keys.press(t, runeKey('d'))
	text = simulationTextOnUI(t, runtime, screen)
	if !strings.Contains(text, "Details (d hides)") || !strings.Contains(text, "8589934592") {
		t.Fatalf("d did not reveal the raw message:\n%s", text)
	}
	keys.press(t, runeKey('d'))
	if text = simulationTextOnUI(t, runtime, screen); strings.Contains(text, "8589934592") {
		t.Fatalf("a second d did not hide the details:\n%s", text)
	}
}

// verifies: AIRA-342 item 2 — a success says what changed to which job, with no
// JSON on screen; the daemon's payload is behind `d`.
func TestDialogSuccessIsASentenceNotJSON(t *testing.T) {
	data, _ := json.Marshal(runner.ConfineResizeResult{
		Status: "resized", ScopeID: dialogTestJob, Name: "alpha", Owner: "session-a",
		OldReserveBytes: 8 * gib, NewReserveBytes: 2 * gib,
	})
	dispatcher := &topDialogDispatcher{reply: core.Response{OK: true, Code: "OK", RawData: data}}
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, 100, 32)
	openResizeConfirmation(t, runtime, screen, keys, "2G")
	keys.press(t, specialKey(tcell.KeyTab), specialKey(tcell.KeyEnter))
	text := waitForSimulationText(t, runtime, screen, "Applied")
	// The sentence wraps inside the dialog, so it is checked in its two halves.
	for _, want := range []string{"APPLIED — the reserve of alpha (owner session-a) is now 2G, down from 8G;", "went back to the pool for queued jobs."} {
		if !strings.Contains(text, want) {
			t.Fatalf("the result does not say %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "{}") || strings.Contains(text, "new_reserve_bytes") {
		t.Fatalf("the result shows JSON before d:\n%s", text)
	}
	keys.press(t, runeKey('d'))
	if text = simulationTextOnUI(t, runtime, screen); !strings.Contains(text, `"new_reserve_bytes": 2147483648`) {
		t.Fatalf("d did not reveal the raw reply:\n%s", text)
	}
}

// verifies: AIRA-342 item 3 — the size form previews the typed value as it is
// typed, and a value that cannot be sent is reported under the field, not in a
// pop-up.
func TestDialogSizeFormValidatesInPlace(t *testing.T) {
	dispatcher := &topDialogDispatcher{}
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, 100, 32)
	onUIDraw(t, runtime, func() struct{} {
		for index, row := range runtime.state.Panels[viewTop].Model.Rows {
			if row.ID == dialogTestJob {
				runtime.tables[viewTop].Select(index+1, 0)
			}
		}
		return struct{}{}
	})
	keys.press(t, runeKey('e'))
	text := waitForSimulationText(t, runtime, screen, "New reserve")
	if !strings.Contains(text, "Reserve: 8G now, live use 1G") || !strings.Contains(text, "enter a size smaller than 8G") {
		t.Fatalf("the size form does not state the job's reserve and the prompt:\n%s", text)
	}
	keys.typeText("2G")
	keys.barrier(t)
	waitForSimulationText(t, runtime, screen, "✓ 8G → 2G, frees 6G for queued jobs")
	keys.press(t, specialKey(tcell.KeyBackspace2), specialKey(tcell.KeyBackspace2))
	keys.typeText("2")
	// The live preview already flags "2" as it is typed; the code suffix is only
	// written by the SUBMIT, so it proves Enter in the field submitted the form
	// and the refusal landed under the field.
	if text = simulationTextOnUI(t, runtime, screen); strings.Contains(text, "(E_CONFINE_ARGUMENT_INVALID)") {
		t.Fatalf("the code suffix is on screen before the submit:\n%s", text)
	}
	keys.press(t, specialKey(tcell.KeyEnter))
	text = waitForSimulationText(t, runtime, screen, "(E_CONFINE_ARGUMENT_INVALID)")
	if !strings.Contains(text, `✗ "2" is a number of bytes: add a unit`) {
		t.Fatalf("the refused size is not explained under the field:\n%s", text)
	}
	page := onUI(t, runtime, func() string { name, _ := runtime.outerPages.GetFrontPage(); return name })
	if page != inlinePage || strings.Contains(text, "Not applied") {
		t.Fatalf("a refused size left the form (front page %q):\n%s", page, text)
	}
}

// verifies: AIRA-342 item 4 — at 80x24 the longest dialog (the resize
// confirmation) is drawn whole and inside the screen: its top-left corner is
// past column 0 on the title row, its bottom border is on screen in the same
// column, and the buttons and the key hint sit between the two.
func TestDialogFitsAn80By24Terminal(t *testing.T) {
	dispatcher := &topDialogDispatcher{}
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, 80, 24)
	openResizeConfirmation(t, runtime, screen, keys, "2G")
	text := simulationTextOnUI(t, runtime, screen)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	top, column := -1, -1
	for index, line := range lines {
		if strings.Contains(line, "Lower this job's reserve?") {
			runes := []rune(line)
			for at, r := range runes {
				if r == '╔' || r == '┌' {
					top, column = index, at
					break
				}
			}
			if !strings.ContainsAny(line, "╗┐") {
				t.Fatalf("the title row has no top-right corner (clipped on the right):\n%s", text)
			}
		}
	}
	if top < 0 || column < 1 {
		t.Fatalf("the dialog's top-left corner is missing or at column 0 (top=%d column=%d):\n%s", top, column, text)
	}
	bottom := -1
	for index := top + 1; index < len(lines); index++ {
		if runes := []rune(lines[index]); column < len(runes) && (runes[column] == '╚' || runes[column] == '└') {
			bottom = index
			break
		}
	}
	if bottom < 0 {
		t.Fatalf("the dialog's bottom border is not on screen:\n%s", text)
	}
	inside := strings.Join(lines[top:bottom], "\n")
	for _, needle := range []string{"Confirm", "Cancel", "Esc cancels", "Change: 8G → 2G"} {
		if !strings.Contains(inside, needle) {
			t.Fatalf("%q is not inside the 80x24 dialog:\n%s", needle, text)
		}
	}
}

// verifies: AIRA-342 — the size preview's verdicts, including the two the
// daemon decides (warned, never refused here) and the bare-number case.
func TestTopResizePreview(t *testing.T) {
	current, usage := 8*gib, gib
	job := &topJob{Name: "alpha", Owner: "session-a", Reserve: &current, Usage: &usage}
	for _, test := range []struct {
		raw  string
		tone dialogTone
		want string
	}{
		{"", toneInfo, "enter a size smaller than 8G"},
		{"2", toneError, `"2" is a number of bytes: add a unit, e.g. 2G or 2M`},
		{"2Q", toneError, "invalid unit"},
		{"8G", toneWarn, "8G → 8G is not smaller: the daemon will refuse it"},
		{"512M", toneWarn, "512M is below the job's live use of 1G: the daemon will refuse it"},
		{"1.5G", toneOK, "8G → 1.5G, frees 6.5G for queued jobs"},
	} {
		tone, line := topResizePreview(job, test.raw)
		if tone != test.tone || !strings.Contains(line, test.want) {
			t.Errorf("preview(%q) = %v %q, want %v containing %q", test.raw, tone, line, test.tone, test.want)
		}
	}
	if tone, line := topResizePreview(&topJob{Name: "alpha"}, "2G"); tone != toneOK || !strings.Contains(line, "unevaluated") {
		t.Errorf("preview with no known reserve = %v %q, want ok and unevaluated", tone, line)
	}
}

// verifies: AIRA-342 — the message humaniser rewrites byte counts and scope ids
// and nothing else.
func TestHumanizeDaemonMessage(t *testing.T) {
	code := runner.CodeConfineResizeBelowUsage
	got := humanizeDaemonMessage(code, code+": "+dialogTestJob+" is using 1610612736 bytes right now; the new reserve (536870912) may not be below it")
	if want := "alpha is using 1.5G right now; the new reserve (512M) may not be below it"; got != want {
		t.Fatalf("humanised %q, want %q", got, want)
	}
	if got := humanizeDaemonMessage("E_X", "selector 2145998 matched nothing"); got != "selector 2145998 matched nothing" {
		t.Fatalf("a bare number that is not a byte count was rewritten: %q", got)
	}
}
