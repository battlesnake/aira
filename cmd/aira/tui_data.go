package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/store"
)

const tuiDecodeError = "E_TUI_DECODE"

type tuiLeaseTokenResolver interface {
	TUILeaseToken(daemon.WorktreeScope, string) (string, error)
}

func decodeTUIResponse(response core.Response, target any) string {
	if !response.OK {
		if response.Code != "" {
			return response.Code
		}
		return "E_TUI_UNKNOWN"
	}
	raw := response.RawData
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(response.Data)
		if err != nil {
			return tuiDecodeError
		}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return tuiDecodeError
	}
	if err := json.Unmarshal(trimmed, target); err != nil {
		return tuiDecodeError
	}
	return ""
}

func fetchTUIView(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, view tuiView, generation int) fetchResult {
	result := fetchResult{View: view, Generation: generation}
	switch view {
	case viewTickets:
		var data listEnvelope
		if result.Code = dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "list", Args: map[string]any{}}, &data); result.Code != "" {
			return result
		}
		result.Model = ticketListViewModel(data)
		if len(result.Model.Rows) > 0 && result.Model.Rows[0].ID != "" {
			result.Model.Detail = fetchTicketDetail(ctx, dispatcher, scope, result.Model.Rows[0].ID)
		}
	case viewReady:
		var data listEnvelope
		if result.Code = dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "ready", Args: map[string]any{}}, &data); result.Code != "" {
			return result
		}
		result.Model = readyListViewModel(data)
	case viewLeases:
		var data struct {
			Total int                  `json:"total"`
			Rows  []store.HeldLeaseRow `json:"rows"`
		}
		if result.Code = dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "lease", Args: map[string]any{"subverb": "ls"}}, &data); result.Code != "" {
			return result
		}
		tokens := make(map[string]string)
		if resolver, ok := dispatcher.(tuiLeaseTokenResolver); ok {
			for _, lease := range data.Rows {
				if lease.WorktreeID != scope.WorktreeID {
					continue
				}
				if token, err := resolver.TUILeaseToken(scope, lease.TicketID); err == nil && token != "" {
					tokens[lease.TicketID] = token
				}
			}
		}
		result.Model = leaseListViewModel(data.Rows, tokens)
	case viewFindings:
		var data listEnvelope
		if result.Code = dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "find", Args: map[string]any{"subverb": "ls"}}, &data); result.Code != "" {
			return result
		}
		result.Model = findingListViewModel(data)
		if len(result.Model.Rows) > 0 && result.Model.Rows[0].ID != "" {
			result.Model.Detail = fetchFindingDetail(ctx, dispatcher, scope, result.Model.Rows[0].ID)
		}
	case viewInsights:
		result.Model, result.Code = fetchInsights(ctx, dispatcher, scope)
	case viewEvents:
		result.Model = eventViewModel(nil)
	case viewTop:
		// AIRA-127. The SAME request `aira confine --list` sends, dispatched with
		// an EMPTY worktree scope because confine state is machine-wide and this
		// verb resolves no project. The reply is carried raw to the reducer, which
		// owns the slot table the model depends on.
		//
		// The owner argument is required by the daemon's own validation and is
		// purely a caller identity here: `confine-list` filters nothing by it, so
		// the fixed read-only marker below neither hides nor reveals any job.
		var data runner.ConfineListResult
		request := core.Request{Verb: "confine-list", Args: map[string]any{
			"slice": runner.ResolveConfineSlice(""), "owner": runner.ConfineUnknownOwner,
		}}
		if result.Code = dispatchTUIData(ctx, dispatcher, daemon.WorktreeScope{}, request, &data); result.Code != "" {
			return result
		}
		result.Top = &data
	}
	return result
}

func dispatchTUIData(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, request core.Request, target any) string {
	if err := ctx.Err(); err != nil {
		return "E_TUI_CANCELLED"
	}
	return decodeTUIResponse(dispatcher.Dispatch(ctx, scope, request), target)
}

func fetchTicketDetail(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, id string) string {
	sections := []struct {
		name    string
		request core.Request
	}{
		{name: "Ticket", request: core.Request{Verb: "show", Args: map[string]any{"selector": id}}},
		{name: "Readiness", request: core.Request{Verb: "ready", Args: map[string]any{"selector": id}}},
		{name: "Relations", request: core.Request{Verb: "link", Args: map[string]any{"list": true, "selector": id}}},
		{name: "Findings", request: core.Request{Verb: "find", Args: map[string]any{"subverb": "ls", "query": "ticket:" + id}}},
	}
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		parts = append(parts, fetchDetailSection(ctx, dispatcher, scope, section.name, section.request))
	}
	return strings.Join(parts, "\n\n")
}

func fetchFindingDetail(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, id string) string {
	return fetchDetailSection(ctx, dispatcher, scope, "Finding", core.Request{Verb: "find", Args: map[string]any{"subverb": "show", "selector": id}})
}

func fetchDetailSection(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, name string, request core.Request) string {
	response := dispatcher.Dispatch(ctx, scope, request)
	var data any
	if code := decodeTUIResponse(response, &data); code != "" {
		return name + ": ERROR " + code
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return name + ": ERROR " + tuiDecodeError
	}
	return name + ":\n" + string(raw)
}

func fetchInsights(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope) (panelModel, string) {
	var registry []struct {
		Name string `json:"name"`
	}
	if code := dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "insights", Args: map[string]any{"subverb": "ls"}}, &registry); code != "" {
		return panelModel{}, code
	}
	fetches := make([]gaugeFetch, 0, len(registry))
	for _, item := range registry {
		var gauge store.GaugeResult
		code := dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "insights", Args: map[string]any{"subverb": "show", "name": item.Name}}, &gauge)
		fetches = append(fetches, gaugeFetch{Name: item.Name, Result: gauge, ErrorCode: code})
	}
	return insightViewModel(fetches), ""
}
