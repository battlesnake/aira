package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
)

type paletteAttemptFake struct {
	attempt paletteDispatchAttempt
}

func (f paletteAttemptFake) Dispatch(context.Context, daemon.WorktreeScope, core.Request) core.Response {
	panic("outcome-aware palette dispatch must not fall back to Dispatch")
}

func (f paletteAttemptFake) DispatchPalette(context.Context, daemon.WorktreeScope, core.Request) paletteDispatchAttempt {
	return f.attempt
}

// covers: only a valid terminal daemon response or a provably zero-byte send
// can produce a definite result. Every ambiguous post-send shape stays honest.
func TestPaletteResultClassification(t *testing.T) {
	tests := []struct {
		name    string
		attempt paletteDispatchAttempt
		want    paletteOutcome
		text    string
	}{
		{name: "success", attempt: paletteDispatchAttempt{Response: core.Response{OK: true, Code: "OK", RawData: []byte(`{"id":"AIRA-1"}`)}}, want: paletteApplied, text: "APPLIED"},
		{name: "daemon rejection", attempt: paletteDispatchAttempt{Response: core.Response{Code: "E_SELECTOR_INVALID", Error: "bad selector"}}, want: paletteRejected, text: "REJECTED E_SELECTOR_INVALID"},
		{name: "provable pre-send write failure", attempt: paletteDispatchAttempt{Err: errors.New("write failed"), Send: paletteSendNotSent}, want: paletteRejected, text: "REJECTED"},
		{name: "eof after send", attempt: paletteDispatchAttempt{Err: io.EOF, Send: paletteSendMayHaveBeenSent}, want: paletteOutcomeUnknown, text: "UNEVALUATED"},
		{name: "timeout after send", attempt: paletteDispatchAttempt{Err: context.DeadlineExceeded, Send: paletteSendMayHaveBeenSent}, want: paletteOutcomeUnknown, text: "UNEVALUATED"},
		{name: "decode failure", attempt: paletteDispatchAttempt{Response: core.Response{OK: true, Code: "OK"}, Malformed: true, Send: paletteSendMayHaveBeenSent}, want: paletteOutcomeUnknown, text: "UNEVALUATED"},
		{name: "malformed response", attempt: paletteDispatchAttempt{Response: core.Response{OK: true, Code: "E_BROKEN"}, Send: paletteSendMayHaveBeenSent}, want: paletteOutcomeUnknown, text: "UNEVALUATED"},
		{name: "empty response", attempt: paletteDispatchAttempt{Send: paletteSendMayHaveBeenSent}, want: paletteOutcomeUnknown, text: "UNEVALUATED"},
		{name: "missing terminal response", attempt: paletteDispatchAttempt{Err: io.ErrUnexpectedEOF, Send: paletteSendMayHaveBeenSent}, want: paletteOutcomeUnknown, text: "UNEVALUATED"},
		{name: "unprovable send", attempt: paletteDispatchAttempt{Err: errors.New("ambiguous write"), Send: paletteSendUnprovable}, want: paletteOutcomeUnknown, text: "UNEVALUATED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := executePaletteRequest(context.Background(), paletteAttemptFake{attempt: test.attempt}, daemon.WorktreeScope{}, core.Request{Verb: "create"})
			if result.Outcome != test.want || !strings.Contains(result.Text, test.text) {
				t.Fatalf("result=%#v, want outcome=%s containing %q", result, test.want, test.text)
			}
			// The label set is MUTUALLY EXCLUSIVE: exactly one of APPLIED / REJECTED /
			// UNEVALUATED, never a contradictory "APPLIED\nUNEVALUATED" (Sol build-review
			// P2: substring assertions alone allowed such contradictions).
			labels := map[paletteOutcome]string{paletteApplied: "APPLIED", paletteRejected: "REJECTED", paletteOutcomeUnknown: "UNEVALUATED"}
			for outcome, label := range labels {
				present := strings.Contains(result.Text, label)
				if want := outcome == test.want; present != want {
					t.Fatalf("result text %q label %q present=%v want %v", result.Text, label, present, want)
				}
			}
			lower := strings.ToLower(result.Text)
			if strings.Contains(lower, "done") || test.want == paletteOutcomeUnknown && strings.Contains(lower, "failure") {
				t.Fatalf("dishonest result text %q", result.Text)
			}
		})
	}
}

// countingDispatcher is concurrency-safe (the executor runs multiple workers)
// and answers every request with an empty-but-valid list envelope.
type countingDispatcher struct {
	mu    sync.Mutex
	count int
}

func (d *countingDispatcher) Dispatch(_ context.Context, _ daemon.WorktreeScope, _ core.Request) core.Response {
	d.mu.Lock()
	d.count++
	d.mu.Unlock()
	return core.Response{OK: true, Code: "OK", RawData: []byte(`{"total":0,"rows":[]}`)}
}

// TestExecutorSubmitIsLosslessUnderSaturation proves submit never drops a
// command even when many are enqueued faster than they drain — the bug a
// bounded, droppable channel caused (a lost cmdScheduleRefresh left a panel
// PendingRefresh forever; a lost detail cmd corrupted a real panel fetch).
func TestExecutorSubmitIsLosslessUnderSaturation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor := newTUIExecutor(ctx, &countingDispatcher{}, nil, daemon.WorktreeScope{}, 2)

	const n = 500
	for i := 0; i < n; i++ {
		if !executor.submit(tuiCmd{Kind: cmdFetch, View: viewTickets, Generation: i}) {
			t.Fatalf("submit reported a drop at %d — submit must be lossless", i)
		}
	}
	// Every submitted fetch must produce exactly one result message; none dropped.
	seen := 0
	deadline := time.After(10 * time.Second)
	for seen < n {
		select {
		case msg := <-executor.messages:
			if msg.Kind == msgFetchResult {
				seen++
			}
		case <-deadline:
			t.Fatalf("only %d/%d fetch results delivered — commands were dropped", seen, n)
		}
	}
}

// TestExecutorDeliveryIsCancelSafeWhenChannelSaturated proves delivery never
// blocks past ctx cancellation even if the messages channel is full and
// unconsumed — the property that lets the coordinator join the executor without
// a stuck worker.
func TestExecutorDeliveryIsCancelSafeWhenChannelSaturated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	executor := &tuiExecutor{ctx: ctx, messages: make(chan tuiMessage, 1)}
	executor.messages <- tuiMessage{Kind: msgEOF} // saturate (cap 1, now full)
	cancel()

	done := make(chan struct{})
	go func() {
		executor.deliver(tuiMessage{Kind: msgFetchResult}) // must return via <-ctx.Done, not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliver blocked on a full channel after cancel — shutdown would deadlock")
	}
}

// plainDispatcherFake implements ONLY Dispatcher (no DispatchPalette), forcing
// executePaletteRequest down the evidenceless fallback.
type plainDispatcherFake struct{ response core.Response }

func (f plainDispatcherFake) Dispatch(context.Context, daemon.WorktreeScope, core.Request) core.Response {
	return f.response
}

// verifies: without transport send-evidence, a flattened non-success is
// outcome-unknown, NEVER rejected — a committed-then-lost write must not read as
// rejected + skip refresh (Sol build-review P0).
func TestExecutePaletteRequestEvidencelessFallbackNeverRejects(t *testing.T) {
	unknown := executePaletteRequest(context.Background(),
		plainDispatcherFake{response: core.Response{Code: "E_TIMEOUT", Error: "lost response"}},
		daemon.WorktreeScope{}, core.Request{Verb: "create"})
	if unknown.Outcome != paletteOutcomeUnknown || strings.Contains(unknown.Text, "REJECTED") {
		t.Fatalf("evidenceless flattened error=%#v, want outcome-unknown", unknown)
	}
	applied := executePaletteRequest(context.Background(),
		plainDispatcherFake{response: core.Response{OK: true, Code: "OK", RawData: []byte(`{"id":"AIRA-1"}`)}},
		daemon.WorktreeScope{}, core.Request{Verb: "create"})
	if applied.Outcome != paletteApplied {
		t.Fatalf("evidenceless success=%#v, want applied", applied)
	}
}
