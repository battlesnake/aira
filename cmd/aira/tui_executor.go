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
	msgExecuteResume
	msgExecuteDetachedResult
)

type tuiMessage struct {
	Kind           tuiMessageKind
	Fetch          fetchResult
	Events         []store.WatchEvent
	Cursor         int64
	Code           string
	PaletteResult  string
	PaletteOutcome paletteOutcome
	View           tuiView
	Detail         detailResult
	DetachedResult executeDetachedResult
}

type paletteSendEvidence uint8

const (
	paletteSendUnprovable paletteSendEvidence = iota
	paletteSendNotSent
	paletteSendMayHaveBeenSent
)

type paletteDispatchAttempt struct {
	Response  core.Response
	Err       error
	Send      paletteSendEvidence
	Malformed bool
	// Evidenceless is set when the dispatcher provided no transport send-evidence
	// (a plain Dispatcher fallback). A flattened non-success then cannot be
	// distinguished from a committed-then-lost write, so it must classify as
	// outcome-unknown, never rejected (Sol build-review P0).
	Evidenceless bool
}

type paletteResult struct {
	Outcome paletteOutcome
	Text    string
}

type paletteOutcomeDispatcher interface {
	DispatchPalette(context.Context, daemon.WorktreeScope, core.Request) paletteDispatchAttempt
}

type tuiJob struct {
	View       tuiView
	Generation int
	Palette    *core.Request
	Detached   *executeLaunch
	DetailID   string
}

type tuiExecutor struct {
	ctx               context.Context
	dispatcher        Dispatcher
	executeDispatcher Dispatcher
	scope             daemon.WorktreeScope
	mu                sync.Mutex
	queue             []tuiCmd
	wake              chan struct{}
	jobs              chan tuiJob
	messages          chan tuiMessage
	reconnect         chan struct{}
	wg                sync.WaitGroup
}

func newTUIExecutor(ctx context.Context, dispatcher, executeDispatcher Dispatcher, scope daemon.WorktreeScope, workers int) *tuiExecutor {
	if workers < 1 {
		workers = 1
	}
	executor := &tuiExecutor{
		ctx: ctx, dispatcher: dispatcher, executeDispatcher: executeDispatcher, scope: scope,
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

func (e *tuiExecutor) submitDetached(launch executeLaunch) bool {
	launch = cloneExecuteLaunch(launch)
	return e.submit(tuiCmd{Kind: cmdExecuteDetached, Execute: &launch})
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
			case cmdExecuteDetached:
				select {
				case e.jobs <- tuiJob{Detached: command.Execute}:
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
			if job.Detached != nil {
				result := dispatchDetachedExecute(e.ctx, e.executeDispatcher, e.scope, *job.Detached)
				e.deliver(tuiMessage{Kind: msgExecuteDetachedResult, DetachedResult: result})
				continue
			}
			if job.Palette != nil {
				result := executePaletteRequest(e.ctx, e.dispatcher, e.scope, *job.Palette)
				e.deliver(tuiMessage{Kind: msgPaletteResult, PaletteResult: result.Text, PaletteOutcome: result.Outcome})
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

func executePaletteRequest(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, request core.Request) paletteResult {
	if outcomeDispatcher, ok := dispatcher.(paletteOutcomeDispatcher); ok {
		return classifyPaletteDispatch(outcomeDispatcher.DispatchPalette(ctx, scope, request))
	}
	// No send-evidence: never label a non-success as rejected (Sol P0).
	return classifyPaletteDispatch(paletteDispatchAttempt{
		Response: dispatcher.Dispatch(ctx, scope, request), Send: paletteSendMayHaveBeenSent, Evidenceless: true,
	})
}

func classifyPaletteDispatch(attempt paletteDispatchAttempt) paletteResult {
	if attempt.Err != nil {
		if attempt.Send == paletteSendNotSent {
			return paletteResult{Outcome: paletteRejected, Text: fmt.Sprintf("REJECTED (not sent)\n%s", attempt.Err)}
		}
		return paletteResult{Outcome: paletteOutcomeUnknown, Text: fmt.Sprintf("UNEVALUATED — outcome unknown\n%s", attempt.Err)}
	}
	if attempt.Malformed {
		return paletteResult{Outcome: paletteOutcomeUnknown, Text: "UNEVALUATED — outcome unknown\nmalformed daemon response"}
	}
	response := attempt.Response
	if !response.OK {
		// Only a send-evidenced daemon error Code is a provable rejection. Without
		// send-evidence, a flattened error may be a committed-then-lost write, so it
		// is outcome-unknown, never rejected (Sol P0).
		if !attempt.Evidenceless && response.Code != "" && response.Code != "OK" {
			return paletteResult{Outcome: paletteRejected, Text: fmt.Sprintf("REJECTED %s\n%s", response.Code, response.Error)}
		}
		return paletteResult{Outcome: paletteOutcomeUnknown, Text: "UNEVALUATED — outcome unknown\nmissing terminal daemon response"}
	}
	if response.Code != "OK" {
		return paletteResult{Outcome: paletteOutcomeUnknown, Text: "UNEVALUATED — outcome unknown\nmalformed daemon response"}
	}
	formatted, ok := formatPaletteSuccess(response)
	if !ok {
		return paletteResult{Outcome: paletteOutcomeUnknown, Text: "UNEVALUATED — outcome unknown\nmalformed daemon response data"}
	}
	if formatted == "" {
		return paletteResult{Outcome: paletteApplied, Text: "APPLIED"}
	}
	return paletteResult{Outcome: paletteApplied, Text: "APPLIED\n" + formatted}
}

func formatPaletteSuccess(response core.Response) (string, bool) {
	data := response.RawData
	if len(data) == 0 && response.Data != nil {
		var err error
		data, err = json.Marshal(response.Data)
		if err != nil {
			return "", false
		}
	}
	if len(data) == 0 {
		return "", true
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return "", false
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", false
	}
	return string(pretty), true
}
