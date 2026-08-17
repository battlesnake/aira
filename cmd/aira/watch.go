package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
)

const (
	watchExchangeTimeout = 30 * time.Second
	watchBackoffMin      = 50 * time.Millisecond
	watchBackoffMax      = 2 * time.Second
)

func runWatchLoop(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, request core.Request, jsonOutput bool, stdout, stderr io.Writer) int {
	if request.Args == nil {
		request.Args = map[string]any{}
	}
	cursor := watchRequestInt64(request.Args["from"])
	backoff := watchBackoffMin
	for {
		if ctx.Err() != nil {
			return 0
		}
		exchangeCtx, cancel := context.WithTimeout(ctx, watchExchangeTimeout)
		response := dispatcher.Dispatch(exchangeCtx, scope, request)
		cancel()
		if ctx.Err() != nil {
			return 0
		}
		if !response.OK {
			if watchFatal(response.Code) {
				return render(response, jsonOutput, stdout, stderr)
			}
			if !waitWatchBackoff(ctx, backoff) {
				return 0
			}
			if backoff < watchBackoffMax {
				backoff *= 2
				if backoff > watchBackoffMax {
					backoff = watchBackoffMax
				}
			}
			continue
		}

		batch, err := decodeWatchResponse(response)
		if err != nil {
			return render(core.Response{Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": " + err.Error(), Exit: store.ExitForCode(daemon.CodeProtocol)}, jsonOutput, stdout, stderr)
		}
		for _, event := range batch.Events {
			if err := printWatchEvent(stdout, event, jsonOutput); err != nil {
				_, _ = fmt.Fprintf(stderr, "E_INTERNAL: write watch event: %v\n", err)
				return store.ExitForCode("E_INTERNAL")
			}
		}
		if flusher, ok := stdout.(interface{ Flush() error }); ok {
			if err := flusher.Flush(); err != nil {
				_, _ = fmt.Fprintf(stderr, "E_INTERNAL: flush watch events: %v\n", err)
				return store.ExitForCode("E_INTERNAL")
			}
		}
		cursor = batch.Cursor
		request.Args["from_now"] = false
		request.Args["from"] = cursor
		backoff = watchBackoffMin
		if batch.EOF {
			_, _ = fmt.Fprintln(stderr, "aira watch: daemon stopped")
			return 0
		}
	}
}

func decodeWatchResponse(response core.Response) (daemon.WatchResponse, error) {
	var batch daemon.WatchResponse
	data := response.RawData
	if len(data) == 0 {
		var err error
		data, err = json.Marshal(response.Data)
		if err != nil {
			return batch, err
		}
	}
	if err := json.Unmarshal(data, &batch); err != nil {
		return batch, err
	}
	return batch, nil
}

func printWatchEvent(out io.Writer, event store.WatchEvent, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(event)
	}
	_, err := fmt.Fprintf(out, "%d %s %s %s %s\n", event.Seq, event.At, event.Actor, event.Verb, event.Target)
	return err
}

func watchFatal(code string) bool {
	switch code {
	case daemon.CodeProtocol, daemon.CodeProjectInvalid, "E_SELECTOR_INVALID", "E_NOT_PROJECT", "E_CONFIG_INVALID":
		return true
	default:
		return false
	}
}

func waitWatchBackoff(ctx context.Context, upper time.Duration) bool {
	if upper < watchBackoffMin {
		upper = watchBackoffMin
	}
	lower := upper / 2
	jitter := lower + time.Duration(rand.Int64N(int64(upper-lower)+1))
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func watchRequestInt64(value any) int64 {
	switch value := value.(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}
