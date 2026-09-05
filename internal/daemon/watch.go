package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/store"
)

const (
	watchWaitCap              = 25 * time.Second
	watchBatchCap             = 256
	watchMaxConcurrent        = 32
	watchShutdownDrainTimeout = 250 * time.Millisecond
	watchWriteTimeout         = 5 * time.Second
)

// WatchResponse is one finite long-poll result. Cursor is the last scanned
// sequence, including filtered-out rows.
type WatchResponse struct {
	Events []store.WatchEvent `json:"events"`
	Cursor int64              `json:"cursor"`
	EOF    bool               `json:"eof"`
}

func (s *Server) watch(connCtx context.Context, scope WorktreeScope, args map[string]any) core.Response {
	select {
	case s.watchSlots <- struct{}{}:
		defer func() { <-s.watchSlots }()
	default:
		return watchError(CodeBusy, CodeBusy+": too many concurrent watch requests")
	}

	pollInterval := s.watchPollInterval
	if pollInterval == 0 {
		pollInterval = defaultWatchPollInterval
	}
	wait := time.Duration(watchInt64(args["wait_ms"])) * time.Millisecond
	if wait < pollInterval {
		wait = pollInterval
	}
	if wait > watchWaitCap {
		wait = watchWaitCap
	}
	start := time.Now()
	deadline := start.Add(wait)

	view, _, err := s.storeForScope(scope)
	if err != nil {
		code := store.ErrorCode(err)
		if strings.HasPrefix(err.Error(), CodeProjectInvalid+":") {
			code = CodeProjectInvalid
		} else if code == "E_INTERNAL" {
			code = CodeUnavailable
		}
		return watchError(code, err.Error())
	}

	from := watchInt64(args["from"])
	verbs := watchStrings(args["verbs"])
	target, _ := args["target"].(string)
	fromNow, _ := args["from_now"].(bool)
	if fromNow {
		from, err = view.CurrentMaxSeq(connCtx)
		if err != nil {
			return watchError(CodeUnavailable, fmt.Sprintf("%s: %v", CodeUnavailable, err))
		}
		if response, done := s.waitWatchMinimum(connCtx, view, from, verbs, target, start, pollInterval); done {
			return response
		}
		if response, stopping := s.watchStopping(view, from, verbs, target, connCtx); stopping {
			return response
		}
		return watchOK(nil, from, false)
	}

	for {
		if response, stopping := s.watchStopping(view, from, verbs, target, connCtx); stopping {
			return response
		}
		events, next, err := s.eventsSince(connCtx, view, from, watchBatchCap)
		if err != nil {
			return watchError(CodeUnavailable, fmt.Sprintf("%s: %v", CodeUnavailable, err))
		}
		if len(events) > 0 {
			if response, done := s.waitWatchMinimum(connCtx, view, from, verbs, target, start, pollInterval); done {
				return response
			}
			if response, stopping := s.watchStopping(view, from, verbs, target, connCtx); stopping {
				return response
			}
			return watchOK(filterWatchEvents(events, verbs, target), next, false)
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			if response, stopping := s.watchStopping(view, from, verbs, target, connCtx); stopping {
				return response
			}
			return watchOK(nil, from, false)
		}
		pollTimer := time.NewTimer(pollInterval)
		deadlineTimer := time.NewTimer(remaining)
		var wake watchWake
		select {
		case <-pollTimer.C:
			wake = watchWakePoll
		case <-deadlineTimer.C:
			wake = watchWakeDeadline
		case <-s.stopping:
			wake = watchWakeStopping
		case <-connCtx.Done():
			wake = watchWakeCanceled
		}
		stopTimer(pollTimer)
		stopTimer(deadlineTimer)
		switch wake {
		case watchWakeStopping:
			return s.terminalDrain(connCtx, view, from, verbs, target)
		case watchWakeCanceled:
			return watchError(CodeUnavailable, fmt.Sprintf("%s: %v", CodeUnavailable, connCtx.Err()))
		default:
			if s.watchAfterWake != nil {
				s.watchAfterWake()
			}
			if response, stopping := s.watchStopping(view, from, verbs, target, connCtx); stopping {
				return response
			}
			if wake == watchWakeDeadline {
				return watchOK(nil, from, false)
			}
		}
	}
}

type watchWake uint8

const (
	watchWakePoll watchWake = iota
	watchWakeDeadline
	watchWakeStopping
	watchWakeCanceled
)

func (s *Server) waitWatchMinimum(connCtx context.Context, view *store.Store, from int64, verbs []string, target string, start time.Time, interval time.Duration) (core.Response, bool) {
	remaining := time.Until(start.Add(interval))
	if remaining <= 0 {
		return core.Response{}, false
	}
	timer := time.NewTimer(remaining)
	defer stopTimer(timer)
	select {
	case <-timer.C:
		if s.watchAfterWake != nil {
			s.watchAfterWake()
		}
		if response, stopping := s.watchStopping(view, from, verbs, target, connCtx); stopping {
			return response, true
		}
		return core.Response{}, false
	case <-s.stopping:
		return s.terminalDrain(connCtx, view, from, verbs, target), true
	case <-connCtx.Done():
		return watchError(CodeUnavailable, fmt.Sprintf("%s: %v", CodeUnavailable, connCtx.Err())), true
	}
}

func (s *Server) watchStopping(view *store.Store, from int64, verbs []string, target string, connCtx context.Context) (core.Response, bool) {
	select {
	case <-s.stopping:
		return s.terminalDrain(connCtx, view, from, verbs, target), true
	default:
		return core.Response{}, false
	}
}

func (s *Server) terminalDrain(connCtx context.Context, view *store.Store, from int64, verbs []string, target string) core.Response {
	ctx, cancel := context.WithTimeout(connCtx, watchShutdownDrainTimeout)
	defer cancel()
	events, next, err := s.eventsSince(ctx, view, from, watchBatchCap)
	if err != nil {
		return watchError(CodeUnavailable, fmt.Sprintf("%s: terminal watch drain failed: %v", CodeUnavailable, err))
	}
	return watchOK(filterWatchEvents(events, verbs, target), next, true)
}

func (s *Server) eventsSince(ctx context.Context, view *store.Store, from int64, limit int) ([]store.WatchEvent, int64, error) {
	if s.watchEventsSince != nil {
		return s.watchEventsSince(ctx, view, from, limit)
	}
	return view.EventsSince(ctx, from, limit)
}

func filterWatchEvents(events []store.WatchEvent, verbs []string, target string) []store.WatchEvent {
	allowed := make(map[string]struct{}, len(verbs))
	for _, verb := range verbs {
		allowed[verb] = struct{}{}
	}
	filtered := make([]store.WatchEvent, 0, len(events))
	for _, event := range events {
		if len(allowed) > 0 {
			if _, ok := allowed[event.Verb]; !ok {
				continue
			}
		}
		if target != "" && event.Target != target {
			continue
		}
		filtered = append(filtered, event)
	}
	return filtered
}

func watchOK(events []store.WatchEvent, cursor int64, eof bool) core.Response {
	if events == nil {
		events = []store.WatchEvent{}
	}
	return core.Response{OK: true, Code: "OK", Data: WatchResponse{Events: events, Cursor: cursor, EOF: eof}}
}

func watchError(code, message string) core.Response {
	return core.Response{Code: code, Error: message, Exit: codes.ExitForCode(code)}
}

func watchInt64(value any) int64 {
	switch value := value.(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	case string:
		// The client sends the cursor as a decimal string so a > 2^53 sequence
		// is never rounded through float64 by the request-arg decode (Sol build
		// r1 #3). Non-numeric strings degrade to 0 (from-start).
		parsed, _ := strconv.ParseInt(value, 10, 64)
		return parsed
	default:
		return 0
	}
}

func watchStrings(value any) []string {
	switch value := value.(type) {
	case []string:
		return value
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}
