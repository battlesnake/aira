package main

import (
	"context"
	"encoding/json"
	"strconv"
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
// over a simulated terminal: which button has focus reads from the cell
// attributes alone, Enter presses the focused button, Cancel is the safe
// default, Esc cancels, a result shows no JSON until `d`, and a refused size is
// said in place, readable at 80 and at 28 columns.
//
// verifies: AIRA-342

const dialogTestJob = "CONFINE-alpha-101-aa@session-a"

// topDialogDispatcher serves one live job (alpha, 8G reserved, 1G in use) and
// answers a confine write with reply, recording it. It carries send-evidence
// (DispatchPalette), as the real daemon dispatcher does.
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
		data, _ := json.Marshal(topTestListing(topTestFrame(), topTestRecord(dialogTestJob, "alpha", 8*gib, gib)))
		return core.Response{OK: true, Code: "OK", RawData: data}
	case "confine-resize":
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
// the given size, as the job's own session, with alpha selected.
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
	resizeBoardScreen(t, runtime, screen, width, height)
	eventuallyOnUI(t, runtime, testdeadline.Wait(10*time.Second), func() bool {
		for index, row := range runtime.state.Panels[viewTop].Model.Rows {
			if row.ID == dialogTestJob {
				runtime.tables[viewTop].Select(index+1, 0)
				return true
			}
		}
		return false
	}, "alpha never reached the top model")
	return runtime, screen, keys
}

// openResizeForm presses `e` on alpha, types size and presses Enter twice: the
// first moves from the field to Continue, the second presses Continue.
func openResizeForm(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, keys *keyTracker, size string) {
	t.Helper()
	keys.press(t, runeKey('e'))
	waitForSimulationText(t, runtime, screen, "New reserve")
	keys.typeText(size)
	keys.press(t, specialKey(tcell.KeyEnter), specialKey(tcell.KeyEnter))
}

func openResizeConfirmation(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, keys *keyTracker) {
	t.Helper()
	openResizeForm(t, runtime, screen, keys, "2G")
	waitForSimulationText(t, runtime, screen, "Confirm mutation")
}

// buttonCell is how a button label is drawn: its first letter's attributes and
// whether the ▶ marker sits two cells before it. The screen is read bottom-up,
// so the button row is found before a title that shares its word.
type buttonCell struct {
	found, reverse, bold, dim, marked bool
}

func readButton(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, label string) buttonCell {
	t.Helper()
	return onUI(t, runtime, func() buttonCell {
		cells, width, height := screen.GetContents()
		for row := height - 1; row >= 0; row-- {
			line := make([]rune, width)
			for column := range line {
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
			return buttonCell{
				found: true, reverse: attrs&tcell.AttrReverse != 0, bold: attrs&tcell.AttrBold != 0,
				dim: attrs&tcell.AttrDim != 0, marked: column >= 2 && line[column-2] == '▶',
			}
		}
		return buttonCell{}
	})
}

func assertFocusedButton(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, focused, idle, when string) {
	t.Helper()
	got, other := readButton(t, runtime, screen, focused), readButton(t, runtime, screen, idle)
	if !got.found || !other.found {
		t.Fatalf("%s: buttons not drawn (%s=%+v %s=%+v)\n%s", when, focused, got, idle, other, simulationTextOnUI(t, runtime, screen))
	}
	if !got.reverse || !got.bold || got.dim || !got.marked {
		t.Fatalf("%s: focused %s is not reverse, bold, undimmed and ▶-marked: %+v", when, focused, got)
	}
	if other.reverse || other.bold || !other.dim || other.marked {
		t.Fatalf("%s: idle %s is not plain, dim and unmarked: %+v", when, idle, other)
	}
}

// verifies: AIRA-342 item 1, at 80x24 — the resize confirmation starts on
// Cancel; focus reads from attributes and the marker; Left, Right and Tab move
// it; Enter presses only the focused button; Esc cancels.
func TestDialogButtonFocusEnterAndEsc(t *testing.T) {
	dispatcher := &topDialogDispatcher{reply: core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{}`)}}
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, 80, 24)
	openResizeConfirmation(t, runtime, screen, keys)
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "Tab/←→ move · Enter presses ▶ · Esc cancels") {
		t.Fatalf("the confirmation carries no key hint:\n%s", text)
	}
	assertFocusedButton(t, runtime, screen, "Cancel", "Confirm", "initial focus")
	keys.press(t, specialKey(tcell.KeyLeft))
	assertFocusedButton(t, runtime, screen, "Confirm", "Cancel", "after Left")
	keys.press(t, specialKey(tcell.KeyRight))
	assertFocusedButton(t, runtime, screen, "Cancel", "Confirm", "after Right")

	keys.press(t, specialKey(tcell.KeyEnter))
	keys.barrier(t)
	if writes := dispatcher.writes(); len(writes) != 0 || onUI(t, runtime, func() bool { return runtime.state.PaletteOpen }) {
		t.Fatalf("Enter on Cancel sent %+v or left the dialog open", writes)
	}

	openResizeConfirmation(t, runtime, screen, keys)
	keys.press(t, specialKey(tcell.KeyTab))
	assertFocusedButton(t, runtime, screen, "Confirm", "Cancel", "after Tab")
	keys.press(t, specialKey(tcell.KeyEscape))
	keys.barrier(t)
	if writes := dispatcher.writes(); len(writes) != 0 || onUI(t, runtime, func() bool { return runtime.state.PaletteOpen }) {
		t.Fatalf("Esc sent %+v or left the dialog open", writes)
	}

	openResizeConfirmation(t, runtime, screen, keys)
	keys.press(t, specialKey(tcell.KeyTab), specialKey(tcell.KeyEnter))
	testdeadline.Eventually(t, time.Second, func() bool { return len(dispatcher.writes()) == 1 }, "Enter on Confirm sent nothing")
	if write := dispatcher.writes()[0]; write.Args["memory_reserve"] != "2G" || write.Args["selector"] != dialogTestJob {
		t.Fatalf("Enter on Confirm sent %+v, want alpha's resize to 2G", write)
	}
}

// verifies: AIRA-342 item 2 — the result shows no JSON until `d`, which shows
// the raw reply and hides it again.
func TestDialogResultHidesJSONBehindD(t *testing.T) {
	data, _ := json.Marshal(runner.ConfineResizeResult{Status: "resized", ScopeID: dialogTestJob, Name: "alpha", Owner: "session-a", OldReserveBytes: 8 * gib, NewReserveBytes: 2 * gib})
	dispatcher := &topDialogDispatcher{reply: core.Response{OK: true, Code: "OK", RawData: data}}
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, 80, 24)
	openResizeConfirmation(t, runtime, screen, keys)
	keys.press(t, specialKey(tcell.KeyTab), specialKey(tcell.KeyEnter))
	text := waitForSimulationText(t, runtime, screen, "APPLIED")
	for _, want := range []string{"Name: alpha (owner session-a)", "Reserve: 8192M → 2048M", "Esc closes · d details"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the result does not say %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "{}") || strings.Contains(text, "new_reserve_bytes") {
		t.Fatalf("the result shows JSON before d:\n%s", text)
	}
	keys.press(t, runeKey('d'))
	keys.barrier(t)
	if text = simulationTextOnUI(t, runtime, screen); !strings.Contains(text, `"new_reserve_bytes": 2147483648`) {
		t.Fatalf("d did not reveal the raw reply:\n%s", text)
	}
	keys.press(t, runeKey('d'))
	keys.barrier(t)
	if text = simulationTextOnUI(t, runtime, screen); strings.Contains(text, "new_reserve_bytes") {
		t.Fatalf("a second d did not hide the details:\n%s", text)
	}
}

// verifies: AIRA-342 item 3 — a refused size is said on its own line above the
// form, whole and inside the dialog, at 80 and at 28 columns; the form stays.
func TestDialogRefusedSizeIsShownAboveTheForm(t *testing.T) {
	_, parseErr := runner.ParseConfineResizeReserve("2")
	want := "E_CONFINE_ARGUMENT_INVALID: new reserve " + strconv.Quote("2") + ": " + parseErr.Error()
	for _, width := range []int{80, 28} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			runtime, screen, keys := startTopDialogRuntime(t, &topDialogDispatcher{}, width, 24)
			openResizeForm(t, runtime, screen, keys, "2")
			keys.barrier(t)
			inside, field := dialogInterior(t, runtime, screen, "New reserve")
			if !strings.Contains(inside, strings.ReplaceAll(want, " ", "")) {
				t.Fatalf("the refusal %q is not whole inside the dialog:\n%s", want, simulationTextOnUI(t, runtime, screen))
			}
			if field < 0 || strings.Index(inside, "E_CONFINE") > field {
				t.Fatalf("the refusal is not above the field:\n%s", simulationTextOnUI(t, runtime, screen))
			}
			if page := onUI(t, runtime, func() string { name, _ := runtime.outerPages.GetFrontPage(); return name }); page != inlinePage {
				t.Fatalf("a refused size left the form for %q", page)
			}
		})
	}
}

// dialogInterior is the text between the dialog's side borders, row after row,
// with spaces removed (wrapping moves words between rows), and where marker
// begins in it (-1 when absent).
func dialogInterior(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, marker string) (string, int) {
	t.Helper()
	var inside strings.Builder
	for _, line := range strings.Split(simulationTextOnUI(t, runtime, screen), "\n") {
		// A focused box's border is double-lined, an unfocused one single.
		line = strings.ReplaceAll(line, "║", "│")
		left, right := strings.Index(line, "│"), strings.LastIndex(line, "│")
		if left >= 0 && right > left {
			inside.WriteString(strings.ReplaceAll(line[left+len("│"):right], " ", ""))
		}
	}
	text := inside.String()
	return text, strings.Index(text, strings.ReplaceAll(marker, " ", ""))
}

// verifies: AIRA-342 item 2 — a verdict reply keeps its verdict and its code,
// never "malformed"; a reply missing a field never has it made up.
func TestPaletteResultKeepsVerdictsAndInventsNothing(t *testing.T) {
	report, _ := json.Marshal(map[string]any{"Verdict": "unevaluated", "Code": "U_GATE_SET_EMPTY", "Results": []any{}})
	verdict := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{OK: true, Code: "UNEVALUATED", RawData: report}})
	if verdict.Outcome != paletteApplied || !strings.Contains(verdict.Text, "VERDICT UNEVALUATED: ") ||
		!strings.Contains(verdict.Text, "not a pass and not a fail") || !strings.Contains(verdict.Text, "Code: U_GATE_SET_EMPTY") {
		t.Fatalf("verdict reply = %#v", verdict)
	}
	if strings.Contains(verdict.Text, "malformed") || strings.Contains(verdict.Text, "APPLIED") {
		t.Fatalf("verdict reply worded as something else: %q", verdict.Text)
	}

	partial := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"new_reserve_bytes":2147483648}`)}})
	if partial.Text != "APPLIED: the daemon accepted the request." {
		t.Fatalf("a reply with only the new size = %q, want the bare acceptance (no before value, no change claimed)", partial.Text)
	}
	unknown := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"queued":[1,2]}`)}})
	if unknown.Text != "APPLIED: the daemon accepted the request." || !strings.Contains(unknown.Details, `"queued"`) {
		t.Fatalf("an unrecognised reply = %#v, want the bare acceptance and the raw reply in Details", unknown)
	}
	refusal := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{Code: "E_X", Error: "it broke", RawData: json.RawMessage(`{"hint":"pass --steal"}`)}})
	if refusal.Text != "REJECTED: the daemon refused the request.\nE_X: it broke\nHint: pass --steal" {
		t.Fatalf("refusal = %q", refusal.Text)
	}
}
