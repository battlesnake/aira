package main

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"

	"github.com/gdamore/tcell/v2"
)

// overviewFixtureDispatcher answers the overview's per-project count/lease and
// the machine-wide confine with fixtures chosen to render a full card + a job.
type overviewFixtureDispatcher struct{}

func (overviewFixtureDispatcher) Dispatch(ctx context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	raw := "{}"
	switch request.Verb {
	case "count":
		raw = `{"total":8,"distribution":{"planned":3,"done":5}}`
	case "lease":
		raw = `{"total":1,"rows":[{"ticket_id":"AIRA-1","worktree_id":"wt1"}]}`
	case "confine-list":
		raw = `{"verdict":"ok","scopes":[{"name":"job-a","owner":"unknown","command":null,"supervisor_live":null,"rss_bytes":null,"age_seconds":null}]}`
	}
	return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(raw)}
}

// installOverviewFixtureSeams overrides the impure registry/Discover/scope seams
// with two fixture projects (alpha=P1 main checkout, beta=P2 main checkout), so
// the runtime smoke test needs no real filesystem, and restores them on cleanup.
func installOverviewFixtureSeams(t *testing.T) {
	t.Helper()
	prevRegistry, prevDiscover, prevScope, prevExists := overviewRegistrySnapshot, overviewDiscover, overviewBuildScope, overviewRootExists
	t.Cleanup(func() {
		overviewRegistrySnapshot, overviewDiscover, overviewBuildScope, overviewRootExists = prevRegistry, prevDiscover, prevScope, prevExists
	})
	slugByRoot := map[string]string{"/p1/main": "alpha", "/p2/main": "beta"}
	idByRoot := map[string]string{"/p1/main": "P1", "/p2/main": "P2"}
	overviewRegistrySnapshot = func() ([]store.RegistryEntry, error) {
		return []store.RegistryEntry{
			ovEntry("P1", "wt1", "/p1/main", "/p1/main/.git", "AIRA"),
			ovEntry("P2", "wt2", "/p2/main", "/p2/main/.git", "BETA"),
		}, nil
	}
	overviewDiscover = func(_ context.Context, root string) (app.Project, error) {
		project := app.Project{Root: root, ProjectID: idByRoot[root], WorktreeID: "wt-" + idByRoot[root]}
		project.Config.Project.Slug = slugByRoot[root]
		return project, nil
	}
	overviewBuildScope = func(project app.Project) (daemon.WorktreeScope, error) {
		return daemon.WorktreeScope{ProjectID: project.ProjectID, Slug: project.Config.Project.Slug, Root: project.Root}, nil
	}
	overviewRootExists = func(string) bool { return true }
}

// TestOverviewSmokeRendersCardsAndOpens drives the real tview path: the overview
// lists two projects, the focused card lazily loads its distribution, and Enter
// opens the focused project (setting the chosen scope for the outer loop).
func TestOverviewSmokeRendersCardsAndOpens(t *testing.T) {
	installOverviewFixtureSeams(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	rt := newOverviewRuntime(ctx, overviewFixtureDispatcher{}, nil, nil, nil, screen)
	done := make(chan int, 1)
	go func() { done <- runTUIRuntime(rt, nil) }()

	// SetSize AFTER Init reset it to 80x25 (P2.12).
	waitForSimulationText(t, rt, screen, "alpha")
	resizeBoardScreen(t, rt, screen, 160, 40)
	// Both projects are shown; the focused card (alpha, sorted first) lazily loads
	// its distribution.
	text := waitForSimulationText(t, rt, screen, "planned:3")
	for _, needle := range []string{"alpha", "beta", "planned:3", "done:5", "job-a"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("overview render missing %q:\n%s", needle, text)
		}
	}
	// Enter opens the focused (alpha=P1) project → the outer loop's chosen scope.
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("overview did not quit after Enter")
	}
	if rt.state.Overview == nil || rt.state.Overview.Chosen == nil {
		t.Fatalf("Enter did not set a chosen scope")
	}
	if got := rt.state.Overview.Chosen.ProjectID; got != "P1" {
		t.Fatalf("chosen project = %q, want P1 (alpha sorts first)", got)
	}
}

// TestOverviewSmokeFilter drives the `/` card filter over the real tview path.
func TestOverviewSmokeFilter(t *testing.T) {
	installOverviewFixtureSeams(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	rt := newOverviewRuntime(ctx, overviewFixtureDispatcher{}, nil, nil, nil, screen)
	done := make(chan int, 1)
	go func() { done <- runTUIRuntime(rt, nil) }()
	waitForSimulationText(t, rt, screen, "beta")
	resizeBoardScreen(t, rt, screen, 160, 40)

	screen.InjectKey(tcell.KeyRune, '/', tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	for _, ch := range "beta" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
		time.Sleep(10 * time.Millisecond)
	}
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	// Wait for the filter to be APPLIED (the footer's active-filter marker appears
	// only once Search is active + submitted) — not merely for the beta CARD, which
	// is always present, so the negative assertion below is not checked mid-typing.
	text := waitForSimulationText(t, rt, screen, "Esc clears")
	if strings.Contains(text, "alpha") {
		t.Fatalf("filter 'beta' should hide alpha:\n%s", text)
	}
	if !strings.Contains(text, "beta") {
		t.Fatalf("filter 'beta' should keep beta:\n%s", text)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("overview filter smoke did not quit")
	}
}

// TestBoardOuterLoopTransitions is the outer-loop transition test (spec §12,
// §17.5). It drives the ACTUAL hop functions runBoard sequences —
// runOverviewMode (Enter opens P1) and runProjectBoard (`o` returns) — through a
// test observer that hands the test the runtime each hop builds internally, so
// the flag→return-value GLUE (not just the reducer flag) is exercised. It also
// asserts goroutines return to baseline across several cycles (no watch/goroutine
// leak on teardown, since run() joins the executor+watch before returning).
func TestBoardOuterLoopTransitions(t *testing.T) {
	installOverviewFixtureSeams(t)
	signals := make(chan os.Signal) // never fires; each hop's ctx cancel exits its loop

	// The observer hands each hop-built runtime to the driver so the test can
	// render + inject keys against the SAME runtime the hop function is running.
	runtimes := make(chan *tuiRuntime, 1)
	prev := boardHopObserver
	boardHopObserver = func(rt *tuiRuntime) { runtimes <- rt }
	t.Cleanup(func() { boardHopObserver = prev })

	// overviewHop calls the REAL runOverviewMode and returns its (chosen, code).
	overviewHop := func() (*daemon.WorktreeScope, int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		screen := tcell.NewSimulationScreen("UTF-8")
		type result struct {
			scope *daemon.WorktreeScope
			code  int
		}
		done := make(chan result, 1)
		go func() {
			scope, code := runOverviewMode(ctx, overviewFixtureDispatcher{}, signals, nil, nil, nil, screen)
			done <- result{scope, code}
		}()
		rt := <-runtimes
		waitForSimulationText(t, rt, screen, "alpha")
		resizeBoardScreen(t, rt, screen, 160, 40)
		// Wait for the focused card's distribution so the list has loaded AND index
		// 0 is a real, openable card before Enter.
		waitForSimulationText(t, rt, screen, "planned:3")
		screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
		select {
		case r := <-done:
			return r.scope, r.code
		case <-time.After(4 * time.Second):
			t.Fatal("overview hop did not return")
			return nil, 0
		}
	}

	// boardHop calls the REAL runProjectBoard and returns its (toOverview, code).
	boardHop := func(scope daemon.WorktreeScope) (bool, int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		screen := tcell.NewSimulationScreen("UTF-8")
		type result struct {
			to   bool
			code int
		}
		done := make(chan result, 1)
		go func() {
			to, code := runProjectBoard(ctx, boardSmokeDispatcher{}, scope, signals, nil, nil, nil, screen)
			done <- result{to, code}
		}()
		rt := <-runtimes
		waitForSimulationText(t, rt, screen, "planned")
		screen.InjectKey(tcell.KeyRune, 'o', tcell.ModNone)
		select {
		case r := <-done:
			return r.to, r.code
		case <-time.After(4 * time.Second):
			t.Fatal("board hop did not return")
			return false, 0
		}
	}

	// overview → (Enter) → the chosen project's scope; project → (`o`) → overview.
	scope, code := overviewHop()
	if scope == nil || scope.ProjectID != "P1" {
		t.Fatalf("overview hop chose %v (code %d), want P1", scope, code)
	}
	if to, code := boardHop(*scope); !to {
		t.Fatalf("board `o` hop should return toOverview=true (code %d)", code)
	}

	// MANY full cycles must not leak goroutines (watch loop + executor + the joined
	// per-hop signal loop are all gone before each hop returns). The tolerance is
	// tied to the cycle count: a genuine PER-CYCLE leak (a stuck watch/signal
	// goroutine) would grow by ~cycles, so a small absolute cap far below the cycle
	// count fails it while tolerating the pump's documented abandonable-
	// QueueUpdateDraw race (tui.go coordinateShutdown) (P2 review fix: was >4 over 4
	// cycles, which a +1/cycle leak survived).
	const cycles = 12
	settle := func() {
		for i := 0; i < 20; i++ {
			runtime.GC()
			time.Sleep(15 * time.Millisecond)
		}
	}
	settle()
	before := runtime.NumGoroutine()
	for i := 0; i < cycles; i++ {
		got, _ := overviewHop()
		if got == nil || got.ProjectID != "P1" {
			t.Fatalf("cycle %d overview chose %v, want P1", i, got)
		}
		if to, _ := boardHop(*got); !to {
			t.Fatalf("cycle %d board `o` did not signal return", i)
		}
	}
	settle()
	after := runtime.NumGoroutine()
	if after-before > 3 { // 12 cycles: a +1/cycle leak would be ~12, well past 3
		t.Fatalf("goroutine leak across %d transitions: before=%d after=%d (>3 growth)", cycles, before, after)
	}
}

// overviewCountingDispatcher wraps the fixture and tallies dispatches per verb,
// so a test can assert the overview fetches count LAZILY — one focused card, not
// a fan-out to all N (P2 review fix: the fixture counted nothing, so a fan-out
// mutation passed silently).
type overviewCountingDispatcher struct {
	overviewFixtureDispatcher
	mu     sync.Mutex
	counts map[string]int
}

func (d *overviewCountingDispatcher) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	d.mu.Lock()
	if d.counts == nil {
		d.counts = map[string]int{}
	}
	d.counts[request.Verb]++
	d.mu.Unlock()
	return d.overviewFixtureDispatcher.Dispatch(ctx, scope, request)
}

func (d *overviewCountingDispatcher) count(verb string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counts[verb]
}

// TestOverviewLazyCountFetchedOnce pins laziness (spec §11.6): with TWO projects,
// opening the overview dispatches `count` for exactly the ONE focused card — not
// a fan-out to both. The counter is read as soon as the focused distribution
// renders (well within the 1s jobs-tick that would later refresh it).
func TestOverviewLazyCountFetchedOnce(t *testing.T) {
	installOverviewFixtureSeams(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	dispatcher := &overviewCountingDispatcher{}
	rt := newOverviewRuntime(ctx, dispatcher, nil, nil, nil, screen)
	done := make(chan int, 1)
	go func() { done <- runTUIRuntime(rt, nil) }()
	waitForSimulationText(t, rt, screen, "alpha")
	resizeBoardScreen(t, rt, screen, 160, 40)
	waitForSimulationText(t, rt, screen, "planned:3") // the focused card's count landed
	if got := dispatcher.count("count"); got != 1 {
		// Mutation guard: a fan-out fetching every card would make this 2 immediately.
		t.Fatalf("overview open dispatched %d count reads, want exactly 1 (lazy, focused card only)", got)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("overview lazy-count smoke did not quit")
	}
}

// TestBoardHopsQuitAndSignal pins the transition GATE (P1 review fix — it was
// porous: no test drove the hop functions to a quit and asserted (false,_)/
// (nil,_), so a mutation making a hop always return a transition stayed green).
// It drives the REAL hop functions to BOTH a `q` keypress and a signal send,
// asserting each returns the QUIT tuple (no transition).
func TestBoardHopsQuitAndSignal(t *testing.T) {
	installOverviewFixtureSeams(t)
	runtimes := make(chan *tuiRuntime, 1)
	prev := boardHopObserver
	boardHopObserver = func(rt *tuiRuntime) { runtimes <- rt }
	t.Cleanup(func() { boardHopObserver = prev })

	// A board hop, driven by `quitKey` (a rune) OR by sending a signal when
	// quitKey==0. It must return (false, _) — a quit, never a transition.
	runBoardHopTo := func(quitKey rune) (bool, int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		signals := make(chan os.Signal, 1)
		screen := tcell.NewSimulationScreen("UTF-8")
		type result struct {
			to   bool
			code int
		}
		done := make(chan result, 1)
		go func() {
			to, code := runProjectBoard(ctx, boardSmokeDispatcher{}, daemon.WorktreeScope{}, signals, nil, nil, nil, screen)
			done <- result{to, code}
		}()
		rt := <-runtimes
		waitForSimulationText(t, rt, screen, "planned")
		if quitKey != 0 {
			screen.InjectKey(tcell.KeyRune, quitKey, tcell.ModNone)
		} else {
			signals <- syscall.SIGINT
		}
		select {
		case r := <-done:
			return r.to, r.code
		case <-time.After(4 * time.Second):
			t.Fatal("board hop did not return")
			return false, 0
		}
	}
	// An overview hop, same shape, must return (nil, _).
	runOverviewHopTo := func(quitKey rune) (*daemon.WorktreeScope, int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		signals := make(chan os.Signal, 1)
		screen := tcell.NewSimulationScreen("UTF-8")
		type result struct {
			scope *daemon.WorktreeScope
			code  int
		}
		done := make(chan result, 1)
		go func() {
			scope, code := runOverviewMode(ctx, overviewFixtureDispatcher{}, signals, nil, nil, nil, screen)
			done <- result{scope, code}
		}()
		rt := <-runtimes
		waitForSimulationText(t, rt, screen, "alpha")
		if quitKey != 0 {
			screen.InjectKey(tcell.KeyRune, quitKey, tcell.ModNone)
		} else {
			signals <- syscall.SIGINT
		}
		select {
		case r := <-done:
			return r.scope, r.code
		case <-time.After(4 * time.Second):
			t.Fatal("overview hop did not return")
			return nil, 0
		}
	}

	if to, _ := runBoardHopTo('q'); to {
		t.Fatalf("board `q` must return toOverview=false (quit), not a transition")
	}
	if to, _ := runBoardHopTo(0); to {
		t.Fatalf("board SIGINT must return toOverview=false (quit), not a transition")
	}
	if scope, _ := runOverviewHopTo('q'); scope != nil {
		t.Fatalf("overview `q` must return a nil chosen scope (quit), got %v", scope)
	}
	if scope, _ := runOverviewHopTo(0); scope != nil {
		t.Fatalf("overview SIGINT must return a nil chosen scope (quit), got %v", scope)
	}
}

// loopDispatcher answers BOTH the overview's count and the board's list/ready/
// lease/confine/watch, so runBoard can be driven through a full mode cycle.
type loopDispatcher struct{ boardSmokeDispatcher }

func (d loopDispatcher) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == "count" {
		return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"total":8,"distribution":{"planned":3,"done":5}}`)}
	}
	return d.boardSmokeDispatcher.Dispatch(ctx, scope, request)
}

// TestBoardRunLoopFullCycle drives runBoard ITSELF (its loop body, previously
// unexercised — P1 review fix) through a full overview → project → overview →
// quit cycle, via the screen-factory + observer seams, asserting it exits 0.
func TestBoardRunLoopFullCycle(t *testing.T) {
	installOverviewFixtureSeams(t)
	screens := make(chan tcell.SimulationScreen, 1)
	runtimes := make(chan *tuiRuntime, 1)
	prevFactory, prevObserver := boardScreenFactory, boardHopObserver
	boardScreenFactory = func() tcell.Screen {
		s := tcell.NewSimulationScreen("UTF-8")
		screens <- s
		return s
	}
	boardHopObserver = func(rt *tuiRuntime) { runtimes <- rt }
	t.Cleanup(func() { boardScreenFactory, boardHopObserver = prevFactory, prevObserver })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- runBoard(ctx, loopDispatcher{}, daemon.WorktreeScope{}, true /* startOverview */, nil, nil, nil)
	}()

	// hop 1: overview → Enter opens P1.
	s1, rt1 := <-screens, <-runtimes
	waitForSimulationText(t, rt1, s1, "alpha")
	resizeBoardScreen(t, rt1, s1, 160, 40)
	waitForSimulationText(t, rt1, s1, "planned:3")
	s1.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)

	// hop 2: the per-project board → `o` returns to the overview.
	s2, rt2 := <-screens, <-runtimes
	waitForSimulationText(t, rt2, s2, "planned")
	s2.InjectKey(tcell.KeyRune, 'o', tcell.ModNone)

	// hop 3: back at the overview → `q` ends the whole loop.
	s3, rt3 := <-screens, <-runtimes
	waitForSimulationText(t, rt3, s3, "alpha")
	s3.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runBoard full cycle exited %d, want 0", code)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runBoard full cycle did not exit")
	}
}
