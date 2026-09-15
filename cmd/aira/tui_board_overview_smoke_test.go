package main

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
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
			scope, code := runOverviewMode(ctx, overviewFixtureDispatcher{}, daemon.Paths{}, signals, nil, nil, nil, screen)
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

	// Several full cycles must not leak goroutines (watch loop + executor are
	// joined by run() before it returns). A small tolerance covers the pump's
	// documented abandonable-QueueUpdateDraw race (tui.go coordinateShutdown).
	settle := func() {
		for i := 0; i < 20; i++ {
			runtime.GC()
			time.Sleep(15 * time.Millisecond)
		}
	}
	settle()
	before := runtime.NumGoroutine()
	for i := 0; i < 4; i++ {
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
	if after-before > 4 {
		t.Fatalf("goroutine leak across transitions: before=%d after=%d (>4 growth)", before, after)
	}
}
