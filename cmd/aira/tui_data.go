package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
)

const tuiDecodeError = "E_TUI_DECODE"

func decodeTUIResponse(response core.Response, target any) string {
	if !response.OK {
		if response.Code != "" {
			return response.Code
		}
		return "E_UNKNOWN"
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
		result.Model = leaseListViewModel(data.Rows)
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
	}
	return result
}

func dispatchTUIData(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, request core.Request, target any) string {
	if err := ctx.Err(); err != nil {
		return "E_CANCELLED"
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
