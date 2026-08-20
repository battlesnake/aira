package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
)

const tuiRefreshDebounce = 250 * time.Millisecond

type tuiMessageKind uint8

const (
	msgFetchResult tuiMessageKind = iota + 1
	msgWatchBatch
	msgEOF
	msgWatchError
	msgPaletteResult
	msgRefreshDue
	msgDetailResult
)

type tuiMessage struct {
	Kind          tuiMessageKind
	Fetch         fetchResult
	Events        []store.WatchEvent
	Cursor        int64
	Code          string
	PaletteResult string
	View          tuiView
	Detail        detailResult
}

type tuiJob struct {
	View       tuiView
	Generation int
	Palette    *core.Request
	DetailID   string
}

type tuiExecutor struct {
	ctx        context.Context
	dispatcher Dispatcher
	scope      daemon.WorktreeScope
	mu         sync.Mutex
	queue      []tuiCmd
	wake       chan struct{}
	jobs       chan tuiJob
	messages   chan tuiMessage
	reconnect  chan struct{}
	wg         sync.WaitGroup
}

func newTUIExecutor(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, workers int) *tuiExecutor {
	if workers < 1 {
		workers = 1
	}
	executor := &tuiExecutor{
		ctx: ctx, dispatcher: dispatcher, scope: scope,
		wake: make(chan struct{}, 1), jobs: make(chan tuiJob, workers*2),
		messages: make(chan tuiMessage, 64), reconnect: make(chan struct{}, 1),
	}
	executor.wg.Add(1)
	go executor.commandLoop()
	for i := 0; i < workers; i++ {
		executor.wg.Add(1)
		go executor.worker()
	}
	executor.wg.Add(1)
	go func() {
		defer executor.wg.Done()
		runTUIWatchLoop(ctx, dispatcher, scope, executor.messages, executor.reconnect)
	}()
	return executor
}

// submit is LOSSLESS and non-blocking for the UI goroutine: it appends to an
// unbounded mutex-backed queue and nudges the command loop. It never drops a
// command, so a saturated executor can never leave a panel with InFlight/
// PendingRefresh stuck (the bug a bounded, droppable channel caused).
func (e *tuiExecutor) submit(command tuiCmd) bool {
	e.mu.Lock()
	e.queue = append(e.queue, command)
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return true
}

func (e *tuiExecutor) submitPalette(request core.Request) bool {
	return e.submit(tuiCmd{Kind: cmdPalette, Palette: &request})
}

func (e *tuiExecutor) wait() { e.wg.Wait() }

func (e *tuiExecutor) drainQueue() []tuiCmd {
	e.mu.Lock()
	commands := e.queue
	e.queue = nil
	e.mu.Unlock()
	return commands
}

func (e *tuiExecutor) commandLoop() {
	defer e.wg.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.wake:
		}
		for _, command := range e.drainQueue() {
			switch command.Kind {
			case cmdFetch:
				select {
				case e.jobs <- tuiJob{View: command.View, Generation: command.Generation, DetailID: command.DetailID}:
				case <-e.ctx.Done():
					return
				}
			case cmdPalette:
				select {
				case e.jobs <- tuiJob{Palette: command.Palette}:
				case <-e.ctx.Done():
					return
				}
			case cmdScheduleRefresh:
				e.wg.Add(1)
				go e.deliverAfter(tuiRefreshDebounce, tuiMessage{Kind: msgRefreshDue, View: command.View})
			case cmdReconnect:
				e.wg.Add(1)
				go e.reconnectAfter(command.Backoff)
			}
		}
	}
}

func (e *tuiExecutor) deliverAfter(delay time.Duration, message tuiMessage) {
	defer e.wg.Done()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		e.deliver(message)
	case <-e.ctx.Done():
	}
}

func (e *tuiExecutor) reconnectAfter(upper time.Duration) {
	defer e.wg.Done()
	if !waitWatchBackoff(e.ctx, upper) {
		return
	}
	select {
	case e.reconnect <- struct{}{}:
	case <-e.ctx.Done():
	}
}

func (e *tuiExecutor) worker() {
	defer e.wg.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case job := <-e.jobs:
			if job.Palette != nil {
				response := e.dispatcher.Dispatch(e.ctx, e.scope, *job.Palette)
				e.deliver(tuiMessage{Kind: msgPaletteResult, PaletteResult: formatPaletteResponse(response)})
				continue
			}
			if job.DetailID != "" {
				detail := ""
				switch job.View {
				case viewTickets:
					detail = fetchTicketDetail(e.ctx, e.dispatcher, e.scope, job.DetailID)
				case viewFindings:
					detail = fetchFindingDetail(e.ctx, e.dispatcher, e.scope, job.DetailID)
				}
				e.deliver(tuiMessage{Kind: msgDetailResult, Detail: detailResult{View: job.View, Generation: job.Generation, ID: job.DetailID, Detail: detail}})
				continue
			}
			result := fetchTUIView(e.ctx, e.dispatcher, e.scope, job.View, job.Generation)
			e.deliver(tuiMessage{Kind: msgFetchResult, Fetch: result})
		}
	}
}

func (e *tuiExecutor) deliver(message tuiMessage) {
	select {
	case e.messages <- message:
	case <-e.ctx.Done():
	}
}

func runTUIWatchLoop(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, messages chan<- tuiMessage, reconnect <-chan struct{}) {
	request := core.Request{Verb: "watch", Args: map[string]any{"from_now": true, "wait_ms": int64(20_000)}}
	for {
		if ctx.Err() != nil {
			return
		}
		exchangeCtx, cancel := context.WithTimeout(ctx, watchExchangeTimeout)
		response := dispatcher.Dispatch(exchangeCtx, scope, request)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if !response.OK {
			if !sendTUIMessage(ctx, messages, tuiMessage{Kind: msgWatchError, Code: response.Code}) ||
				!sendTUIMessage(ctx, messages, tuiMessage{Kind: msgEOF}) || !waitTUIReconnect(ctx, reconnect) {
				return
			}
			continue
		}
		batch, err := decodeWatchResponse(response)
		if err != nil {
			if !sendTUIMessage(ctx, messages, tuiMessage{Kind: msgWatchError, Code: tuiDecodeError}) ||
				!sendTUIMessage(ctx, messages, tuiMessage{Kind: msgEOF}) || !waitTUIReconnect(ctx, reconnect) {
				return
			}
			continue
		}
		if !sendTUIMessage(ctx, messages, tuiMessage{Kind: msgWatchBatch, Events: batch.Events, Cursor: batch.Cursor}) {
			return
		}
		request.Args["from_now"] = false
		request.Args["from"] = strconv.FormatInt(batch.Cursor, 10)
		if batch.EOF {
			if !sendTUIMessage(ctx, messages, tuiMessage{Kind: msgEOF}) || !waitTUIReconnect(ctx, reconnect) {
				return
			}
		}
	}
}

func sendTUIMessage(ctx context.Context, messages chan<- tuiMessage, message tuiMessage) bool {
	select {
	case messages <- message:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitTUIReconnect(ctx context.Context, reconnect <-chan struct{}) bool {
	select {
	case <-reconnect:
		return true
	case <-ctx.Done():
		return false
	}
}

func formatPaletteResponse(response core.Response) string {
	if !response.OK {
		return fmt.Sprintf("ERROR %s\n%s", response.Code, response.Error)
	}
	data := response.RawData
	if len(data) == 0 {
		data, _ = json.Marshal(response.Data)
	}
	var value any
	if len(data) == 0 || json.Unmarshal(data, &value) != nil {
		return "ERROR " + tuiDecodeError
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "ERROR " + tuiDecodeError
	}
	return string(pretty)
}
