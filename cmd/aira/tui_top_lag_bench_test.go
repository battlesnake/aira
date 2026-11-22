package main

// AIRA-374. `aira top` lagged on keystrokes. These benchmarks price the two
// UI-goroutine paths separately on a realistic listing: the refresh TICK (decode
// the reply, reduce it into the view model, paint, draw) and the KEYPRESS (an
// up/down arrow moving the table selection, then the full-screen draw tview does
// after every key event). Both end with the screen read back as text at the
// operator's 200x50, so what is timed is what reaches the terminal.
//
// The listing is the recorded shape from the owner's box (23 live jobs and 30
// queued, nine of them with ~4 KB commands). AIRA_TOP_BENCH_REPLY may name a
// captured `confine-list` reply (the whole envelope or its data) to run on the
// real one instead.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"aira/internal/runner"
)

const (
	topBenchWidth  = 200
	topBenchHeight = 50
)

// topBenchReply is the raw JSON the tick decodes.
func topBenchReply(tb testing.TB) []byte {
	tb.Helper()
	if path := os.Getenv("AIRA_TOP_BENCH_REPLY"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			tb.Fatalf("read %s: %v", path, err)
		}
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Data) > 0 {
			return envelope.Data
		}
		return raw
	}
	return topBenchSyntheticReply(tb)
}

// topBenchSyntheticReply rebuilds the recorded listing's shape: 23 live and 30
// queued jobs, command lengths as recorded (nine at 3958 bytes, two at 1509).
func topBenchSyntheticReply(tb testing.TB) []byte {
	tb.Helper()
	lengths := []int{7, 11, 14, 14, 16, 16, 16, 16, 16, 18, 18, 18, 24, 26, 30, 38, 40, 40, 42, 43, 50, 77, 89, 108,
		109, 125, 125, 135, 136, 136, 136, 136, 164, 180, 203, 239, 244, 295, 308, 347, 501, 558, 1509, 1509,
		3958, 3958, 3958, 3958, 3958, 3958, 3958, 3958, 3958}
	command := func(n int) *string {
		var b strings.Builder
		for b.Len() < n {
			fmt.Fprintf(&b, "/home/user/tmp/wt-%d/libs/pkg%d ", b.Len(), b.Len()%97)
		}
		text := b.String()[:n]
		return &text
	}
	i64 := func(v int64) *int64 { return &v }
	intp := func(v int) *int { return &v }
	result := runner.ConfineListResult{Verdict: "pass"}
	// Interleave so long commands land in both the live and the queued rows, as
	// they did on the box.
	for index := range lengths {
		length := lengths[(index*7)%len(lengths)]
		cwd := "/home/user/tmp/wt-" + fmt.Sprint(index)
		record := runner.ConfineRecord{
			Name: fmt.Sprintf("job-%d", index), Owner: "cc-ebea40b7-e039-4f13-a1d3-7ce7b470a62e",
			SupervisorPID: intp(3100000 + index), ScopeID: fmt.Sprintf("CONFINE-job-%d-%d@owner", index, 3100000+index),
			Command: command(length), Cwd: &cwd,
		}
		if index < 23 {
			record.Populated = intp(1)
			record.RSSBytes, record.PeakRSS, record.AgeSeconds = i64(1<<30), i64(2<<30), i64(int64(60*index))
			capText := "3221225472"
			record.Cap = &capText
			result.Scopes = append(result.Scopes, record)
		} else {
			record.ReserveBytes = i64(1 << 30)
			result.Queued = append(result.Queued, record)
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

// newTopBenchRuntime is an `aira top` runtime drawn onto a 200x50 simulation
// screen without app.Run: the benchmark calls the same render and Draw the event
// loop would, on this goroutine.
func newTopBenchRuntime(tb testing.TB) (*tuiRuntime, tcell.SimulationScreen, context.CancelFunc) {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newTopRuntime(ctx, &tuiSmokeDispatcher{started: make(chan struct{})}, nil, nil, nil, screen)
	// AFTER the runtime: tview's SetScreen runs screen.Init(), which resets a
	// simulation screen to 80x25, so a size set before it is silently lost.
	screen.SetSize(topBenchWidth, topBenchHeight)
	if width, height := screen.Size(); width != topBenchWidth || height != topBenchHeight {
		tb.Fatalf("simulation screen is %dx%d, want %dx%d", width, height, topBenchWidth, topBenchHeight)
	}
	runtime.outerPages.SetRect(0, 0, topBenchWidth, topBenchHeight)
	return runtime, screen, cancel
}

// topBenchDraw is tview's Application.draw: clear, draw the root, show.
func topBenchDraw(runtime *tuiRuntime, screen tcell.SimulationScreen) {
	screen.Clear()
	runtime.outerPages.Draw(screen)
	screen.Show()
}

// topBenchCPU is the process's user+system CPU time. The owner's box runs at a
// load far above its core count, where wall-clock ns/op mostly measures waiting
// for a CPU; cpu-ns/op (GC workers included) is the cost that does not depend on
// the neighbours.
func topBenchCPU() int64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}

func topBenchReportCPU(b *testing.B, start int64) {
	b.ReportMetric(float64(topBenchCPU()-start)/float64(b.N), "cpu-ns/op")
}

// topBenchTick is one refresh tick on the UI goroutine plus the decode the
// fetch worker does before it.
func topBenchTick(tb testing.TB, runtime *tuiRuntime, screen tcell.SimulationScreen, raw []byte) {
	var result runner.ConfineListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		tb.Fatal(err)
	}
	panel := runtime.state.Panels[viewTop]
	panel.Status = panelReady
	panel.Model, runtime.state.Top = topViewModel(runtime.state.Top, result)
	runtime.state.Panels[viewTop] = panel
	runtime.render()
	topBenchDraw(runtime, screen)
}

func BenchmarkTopTick(b *testing.B) {
	raw := topBenchReply(b)
	runtime, screen, cancel := newTopBenchRuntime(b)
	defer cancel()
	topBenchTick(b, runtime, screen, raw)
	b.ReportAllocs()
	b.ResetTimer()
	start := topBenchCPU()
	for i := 0; i < b.N; i++ {
		topBenchTick(b, runtime, screen, raw)
		_ = simulationText(screen)
	}
	topBenchReportCPU(b, start)
}

// BenchmarkTopKeypress is one up/down arrow: the application input capture,
// the table's own input handler moving the selection, and the full-screen draw
// tview runs after every key event. No fetch and no view-model rebuild happen
// on this path.
func BenchmarkTopKeypress(b *testing.B) {
	raw := topBenchReply(b)
	runtime, screen, cancel := newTopBenchRuntime(b)
	defer cancel()
	topBenchTick(b, runtime, screen, raw)
	table := runtime.tables[viewTop]
	handler := table.InputHandler()
	down := tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	up := tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	b.ReportAllocs()
	b.ResetTimer()
	start := topBenchCPU()
	for i := 0; i < b.N; i++ {
		event := down
		if i%2 == 1 {
			event = up
		}
		if runtime.captureInput(event) != nil {
			handler(event, func(tview.Primitive) {})
		}
		topBenchDraw(runtime, screen)
		_ = simulationText(screen)
	}
	topBenchReportCPU(b, start)
}
