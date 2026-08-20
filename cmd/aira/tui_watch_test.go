package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
)

func TestTUIWatchSeeksHeadThenAdvancesCursorAndReconnects(t *testing.T) {
	dispatcher := &tuiFakeDispatcher{responses: []core.Response{
		{OK: true, Code: "OK", RawData: json.RawMessage(`{"events":[],"cursor":5,"eof":false}`)},
		{OK: true, Code: "OK", RawData: json.RawMessage(`{"events":[],"cursor":6,"eof":true}`)},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages := make(chan tuiMessage, 4)
	reconnect := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		runTUIWatchLoop(ctx, dispatcher, daemon.WorktreeScope{}, messages, reconnect)
		close(done)
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-messages:
		case <-time.After(time.Second):
			t.Fatal("watch message timed out")
		}
	}
	if len(dispatcher.requests) != 2 {
		t.Fatalf("requests=%#v", dispatcher.requests)
	}
	first, second := dispatcher.requests[0], dispatcher.requests[1]
	if first.Verb != "watch" || first.Args["from_now"] != true {
		t.Fatalf("first request=%#v", first)
	}
	if second.Args["from_now"] != false || second.Args["from"] != "5" {
		t.Fatalf("second request=%#v", second)
	}
	select {
	case reconnect <- struct{}{}:
	default:
		t.Fatal("reconnect signal blocked")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch loop did not stop")
	}
}
