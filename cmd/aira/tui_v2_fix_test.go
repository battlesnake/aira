package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
)

// verifies: DispatchPalette NEVER executes a client-routed verb locally; a client
// route (stale entry / adapter / parser regression) is a provable pre-send
// rejection, so no client handler or store is touched (Sol build-review P0).
func TestDispatchPaletteRejectsClientRoutedRequestWithoutLocalExecution(t *testing.T) {
	// Use an ALREADY-CANCELLED context: the routed-only guard returns its
	// E_SELECTOR_INVALID rejection BEFORE any ctx use / dial / dispatchClient. If the
	// guard were removed and the method fell through to client execution or a socket
	// exchange, the cancelled ctx would surface a different (dial/timeout/ctx) error,
	// not E_SELECTOR_INVALID — so the exact error discriminates "no local execution
	// attempted" without needing a live handler seam (Sol confirm P0-b).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, verb := range []core.Request{
		{Verb: "run", Args: map[string]any{}},
		{Verb: "reconcile", Args: map[string]any{}},
		{Verb: "gate", Args: map[string]any{"subverb": "run"}},
	} {
		attempt := (&daemonDispatcher{}).DispatchPalette(ctx, daemon.WorktreeScope{}, verb)
		if attempt.Send != paletteSendNotSent || attempt.Err == nil || !strings.Contains(attempt.Err.Error(), "E_SELECTOR_INVALID") {
			t.Fatalf("client route %q dispatch=%#v, want E_SELECTOR_INVALID not-sent rejection before any I/O", verb.Verb, attempt)
		}
		if classifyPaletteDispatch(attempt).Outcome != paletteRejected {
			t.Fatalf("client route %q not classified rejected", verb.Verb)
		}
	}
}

// verifies: the outcome-unknown wrapper delegates its message so string-prefix
// code extraction (store.ErrorCode) still recovers the transport code for
// non-TUI callers, while the type still carries the outcome-unknown meaning
// (Sol build-review P1).
func TestRequestOutcomeUnknownPreservesErrorCodeForNonTUICallers(t *testing.T) {
	wrapped := &daemon.RequestOutcomeUnknownError{Err: fmt.Errorf("%s: lost response", "E_TIMEOUT")}
	if got := store.ErrorCode(wrapped); got != "E_TIMEOUT" {
		t.Fatalf("ErrorCode(outcome-unknown wrapper)=%q, want E_TIMEOUT", got)
	}
	if !daemon.IsRequestOutcomeUnknown(wrapped) {
		t.Fatal("wrapper lost its outcome-unknown type")
	}
	notSent := &daemon.RequestNotSentError{Err: fmt.Errorf("%s: dial", "E_UNAVAILABLE")}
	if got := store.ErrorCode(notSent); got != "E_UNAVAILABLE" {
		t.Fatalf("ErrorCode(not-sent wrapper)=%q, want E_UNAVAILABLE", got)
	}
}

// verifies: a destructive op with a CANONICAL id dispatches exactly once after the
// exact id is typed, while a shorthand/non-canonical selector is blocked with a
// reason and can never dispatch (Sol build-review P1 + the destructive test that
// previously only cancelled, P2).
func TestControllerDestructiveExactIDDispatchesAndShorthandIsBlocked(t *testing.T) {
	entry := paletteEntry{Verb: "rant", Operation: "redact", Summary: "redact", Safety: core.SafetyMutate, Destructive: true}

	// Canonical id: type it exactly → enabled → exactly one dispatch.
	canonical := core.Request{Verb: "rant", Args: map[string]any{"subverb": "redact", "selector": "RANT-7"}}
	state, commands := onPaletteSubmit(newTUIState(8), entry, canonical)
	if len(commands) != 0 || state.PaletteConfirm == nil || state.PaletteConfirm.ConfirmIDTarget != "RANT-7" || state.PaletteConfirm.ConfirmBlockedReason != "" {
		t.Fatalf("canonical destructive submit=%#v commands=%#v", state.PaletteConfirm, commands)
	}
	state = onPaletteConfirmTypedID(state, "RANT-7")
	if !paletteConfirmEnabled(state.PaletteConfirm) {
		t.Fatal("canonical typed id did not enable confirmation")
	}
	want := core.Request{Verb: "rant", Args: map[string]any{"subverb": "redact", "selector": "RANT-7"}}
	state, commands = onPaletteConfirm(state)
	assertSinglePaletteCommand(t, commands, want)
	if !state.PaletteDispatching || state.PaletteConfirm != nil {
		t.Fatalf("post-confirm state dispatching=%v confirm=%#v", state.PaletteDispatching, state.PaletteConfirm)
	}

	// Non-canonical selectors are blocked with a reason and never dispatch. The
	// whitespace case is the Sol confirm P1 bug: the gate must bind to the EXACT
	// dispatched value, so " RANT-7 " must NOT be confirmable as RANT-7. The
	// non-string case must block, not silently make confirmation impossible.
	for _, selector := range []any{"latest", " RANT-7 ", "rant-7", 7, nil} {
		blocked, commands := onPaletteSubmit(newTUIState(8), entry, core.Request{Verb: "rant", Args: map[string]any{"subverb": "redact", "selector": selector}})
		if len(commands) != 0 || blocked.PaletteConfirm == nil || blocked.PaletteConfirm.ConfirmIDTarget != "" || blocked.PaletteConfirm.ConfirmBlockedReason == "" {
			t.Fatalf("non-canonical selector %#v submit=%#v", selector, blocked.PaletteConfirm)
		}
		// Even typing the exact selector string cannot enable a blocked target.
		blocked = onPaletteConfirmTypedID(blocked, fmt.Sprint(selector))
		if paletteConfirmEnabled(blocked.PaletteConfirm) {
			t.Fatalf("non-canonical selector %#v enabled destructive confirmation", selector)
		}
		if _, commands = onPaletteConfirm(blocked); len(commands) != 0 {
			t.Fatalf("non-canonical selector %#v dispatched: %#v", selector, commands)
		}
	}
}
