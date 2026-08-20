package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
)

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
	executor := newTUIExecutor(ctx, &countingDispatcher{}, daemon.WorktreeScope{}, 2)

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
