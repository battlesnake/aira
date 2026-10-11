package main

// Shared synchronisation helpers for the TUI tests that drive the real tview loop
// on a SimulationScreen (AIRA-204).
//
// Three rules these helpers exist to keep:
//
//  1. A wait for an expected event is a liveness BACKSTOP (testdeadline), never a
//     bound the test asserts: it returns the instant the event happens.
//  2. A read of UI state goes through ONE queued update with a generous backstop
//     (onUI), never a retrying loop with a short inner t.Fatal.
//  3. An injected key is followed by a BARRIER that proves its handler returned
//     (keyTracker.press), never by a sleep. tview runs the key events from the
//     screen and the updates from QueueUpdate on one loop, but a key and a LATER
//     QueueUpdate are not ordered with each other, so "inject a key, sleep, read
//     state" can read the state from before the key.

import (
	"strings"
	"testing"
	"time"

	"aira/internal/testdeadline"

	"github.com/gdamore/tcell/v2"
)

// onUI runs f on the tview UI goroutine and returns its result. The backstop only
// turns a deadlocked loop into a failure; a slow loop is simply waited for.
func onUI[T any](tb testing.TB, rt *tuiRuntime, f func() T) T {
	tb.Helper()
	result := make(chan T, 1)
	go rt.app.QueueUpdate(func() { result <- f() })
	select {
	case value := <-result:
		return value
	case <-testdeadline.After(time.Second):
		tb.Fatal("the tview loop did not run a queued update: deadlocked")
		panic("unreachable")
	}
}

// onUIDraw is onUI through QueueUpdateDraw, for the callers that also need a draw.
func onUIDraw[T any](tb testing.TB, rt *tuiRuntime, f func() T) T {
	tb.Helper()
	result := make(chan T, 1)
	go rt.app.QueueUpdateDraw(func() { result <- f() })
	select {
	case value := <-result:
		return value
	case <-testdeadline.After(time.Second):
		tb.Fatal("the tview loop did not run a queued update: deadlocked")
		panic("unreachable")
	}
}

// eventuallyOnUI polls cond ON the UI goroutine until it holds. A slow round trip is
// waited for; only a loop that never answers trips onUI's backstop, and only a
// condition that never holds trips the budget's.
func eventuallyOnUI(tb testing.TB, rt *tuiRuntime, budget time.Duration, cond func() bool, format string, args ...any) {
	tb.Helper()
	testdeadline.Eventually(tb, budget, func() bool { return onUI(tb, rt, cond) }, format, args...)
}

// tuiKey is one key to inject.
type tuiKey struct {
	key tcell.Key
	r   rune
	mod tcell.ModMask
}

func runeKey(r rune) tuiKey { return tuiKey{key: tcell.KeyRune, r: r} }

func specialKey(k tcell.Key) tuiKey { return tuiKey{key: k} }

// keySentinel is the barrier's sentinel rune: a supplementary private-use rune, which
// the TUI binds nowhere and a terminal cannot type.
const keySentinel rune = 0xF0000

// keyTracker is the key-completion barrier.
//
// It wraps the application's input capture. press injects its keys and then ONE
// sentinel key through the same screen queue, and returns when the wrapper sees that
// sentinel. Screen events are FIFO into the application's event channel and tview
// runs each key event to completion (capture, root handler, draw) before taking the
// next, so when the sentinel reaches the capture every key injected before it (by
// press, or raw through screen.InjectKey) has had its handler return. There is no
// counting, so a key injected outside press cannot make a later press return early.
//
// Not safe for concurrent use: press and barrier are called serially, each waits for
// its own sentinel before returning, so at most one sentinel is ever in flight. A
// press that times out fails the test, so no stale acknowledgement is read by a later one.
//
// Keys that stop or suspend the loop (q, the execute launcher's run) must be injected
// raw and followed by the existing done/resume wait: a sentinel behind them is never
// processed.
//
// trackKeys must be called after the runtime is constructed (which installs the
// application's capture) and before run().
type keyTracker struct {
	screen tcell.SimulationScreen
	ack    chan struct{}
}

func trackKeys(rt *tuiRuntime, screen tcell.SimulationScreen) *keyTracker {
	tracker := &keyTracker{screen: screen, ack: make(chan struct{}, 1)}
	inner := rt.app.GetInputCapture()
	rt.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyRune && event.Rune() == keySentinel {
			select {
			case tracker.ack <- struct{}{}:
			default:
			}
			return nil // the sentinel never reaches a primitive
		}
		if inner != nil {
			return inner(event)
		}
		return event
	})
	return tracker
}

// press injects the keys and returns once every one of them (and every key injected
// before them) has been handled.
func (k *keyTracker) press(tb testing.TB, keys ...tuiKey) {
	tb.Helper()
	for _, key := range keys {
		k.screen.InjectKey(key.key, key.r, key.mod)
	}
	k.barrier(tb)
}

// barrier returns once every key injected so far has been handled.
func (k *keyTracker) barrier(tb testing.TB) {
	tb.Helper()
	k.screen.InjectKey(tcell.KeyRune, keySentinel, tcell.ModNone)
	select {
	case <-k.ack:
	case <-testdeadline.After(time.Second):
		tb.Fatal("the tview loop did not reach the key barrier: deadlocked")
	}
}

// typeText injects the runes raw. The next press or barrier covers them.
func (k *keyTracker) typeText(text string) {
	for _, ch := range text {
		k.screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
}

// waitForSimulationText waits for needle to appear on the simulated screen and
// returns the screen text. It FAILS, with the last screen text, if the needle never
// appears: returning silently is how a caller went on to dereference a form that was
// never built.
//
// Generous under -race + full-suite contention on a loaded box: the tview event loop
// can lag well past a second while heavy integration tests run in the same binary.
// The wait is a poll, so a fast path still returns immediately.
func waitForSimulationText(tb testing.TB, rt *tuiRuntime, screen tcell.SimulationScreen, needle string) string {
	tb.Helper()
	return waitForSimulationTextWithin(tb, rt, screen, needle, testdeadline.Wait(10*time.Second))
}

// waitForSimulationTextWithin is waitForSimulationText against an already-resolved
// backstop, so the failure path is testable without waiting out the real one.
func waitForSimulationTextWithin(tb testing.TB, rt *tuiRuntime, screen tcell.SimulationScreen, needle string, backstop time.Duration) string {
	tb.Helper()
	deadline := time.Now().Add(backstop)
	text := ""
	for {
		text = simulationTextOnUI(tb, rt, screen)
		if strings.Contains(text, needle) {
			return text
		}
		if !time.Now().Before(deadline) {
			tb.Fatalf("screen text %q never appeared; last screen:\n%s", needle, text)
			return text
		}
		time.Sleep(5 * time.Millisecond) // poll pacing, not synchronisation
	}
}

func simulationTextOnUI(tb testing.TB, rt *tuiRuntime, screen tcell.SimulationScreen) string {
	tb.Helper()
	return onUI(tb, rt, func() string { return simulationText(screen) })
}
