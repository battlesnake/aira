package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
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

// decodeTUIResponseWithWarnings is decodeTUIResponse plus the envelope's
// Response.Warnings, which the plain decoder discards (AIRA-252). W_STALE_INDEX
// rides the response envelope, NOT the row data (TicketRecord.Warnings is
// json:"-"), so the board surfaces it as a board-level banner (spec §5).
func decodeTUIResponseWithWarnings(response core.Response, target any) (code string, warnings []string) {
	code = decodeTUIResponse(response, target)
	return code, append([]string(nil), response.Warnings...)
}

func dispatchTUIDataWithWarnings(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, request core.Request, target any) (code string, warnings []string) {
	if err := ctx.Err(); err != nil {
		return "E_TUI_CANCELLED", nil
	}
	return decodeTUIResponseWithWarnings(dispatcher.Dispatch(ctx, scope, request), target)
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
	case viewBoard:
		// AIRA-252. One composite fetch of the whole board: seven per-status lists,
		// the ready overlay, held leases, and the machine-wide confine listing. The
		// raw envelopes are carried to the reducer, which builds the honest view-model
		// (buildBoardModel) while preserving the interactive selection across refreshes.
		//
		// A shared transport failure (every grid section failed with the same code —
		// the daemon went away) becomes result.Code so the reducer keeps the last-good
		// columns and raises one banner (spec §14); a partial failure keeps the data
		// and shows the failed columns' own error headers.
		data := fetchBoardData(ctx, dispatcher, scope)
		if code := boardHasTransportBanner(*data); code != "" {
			result.Code = code
		} else {
			result.Board = data
		}
	}
	return result
}

// fetchBoardData composes the board's read surface. Each section carries its own
// per-section code so a single failed column never blanks the other six; the
// staleness warnings from every read are deduped into data.Warnings for the
// board banner (spec §5, §14). The confine listing is machine-wide, so it is
// dispatched with an EMPTY worktree scope exactly as viewTop does.
func fetchBoardData(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope) *boardData {
	data := &boardData{}
	seen := map[string]bool{}
	collect := func(list []string) {
		for _, warning := range list {
			if warning != "" && !seen[warning] {
				seen[warning] = true
				data.Warnings = append(data.Warnings, warning)
			}
		}
	}
	for _, status := range domain.AllowedStatusStrings() {
		var env listEnvelope
		code, warnings := dispatchTUIDataWithWarnings(ctx, dispatcher, scope,
			core.Request{Verb: "list", Args: map[string]any{"query": "status:" + status}}, &env)
		collect(warnings)
		data.Columns = append(data.Columns, boardColumnFetch{
			Status: status, Total: env.Total, Truncated: env.Truncated, Rows: env.Rows, Code: code,
		})
	}
	var readyWarnings []string
	data.ReadyCode, readyWarnings = dispatchTUIDataWithWarnings(ctx, dispatcher, scope,
		core.Request{Verb: "ready", Args: map[string]any{}}, &data.Ready)
	collect(readyWarnings)

	var leases struct {
		Total int                  `json:"total"`
		Rows  []store.HeldLeaseRow `json:"rows"`
	}
	data.LeaseCode = dispatchTUIData(ctx, dispatcher, scope,
		core.Request{Verb: "lease", Args: map[string]any{"subverb": "ls"}}, &leases)
	data.Leases = leases.Rows

	var confine runner.ConfineListResult
	confineRequest := core.Request{Verb: "confine-list", Args: map[string]any{
		"slice": runner.ResolveConfineSlice(""), "owner": runner.ConfineUnknownOwner,
	}}
	if data.ConfineCode = dispatchTUIData(ctx, dispatcher, daemon.WorktreeScope{}, confineRequest, &confine); data.ConfineCode == "" {
		data.Confine = &confine
	}
	return data
}

// boardSearchFetch is the raw grep reply for a board content search. Query is
// the ORIGINAL (unquoted) query, so onBoardSearchResult can drop a reply for a
// superseded query. Code carries a refusal (e.g. E_QUERY_INVALID); Unevaluated
// carries grep's {unevaluated:true} (E_INDEX_UNEVALUATED) — either is rendered
// "search unevaluated", never "no results" (spec §10).
type boardSearchFetch struct {
	Query       string
	Rows        []map[string]any
	Code        string
	Unevaluated bool
}

func fetchBoardSearch(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, query string) boardSearchFetch {
	fetch := boardSearchFetch{Query: query}
	var env struct {
		Total       int              `json:"total"`
		Rows        []map[string]any `json:"rows"`
		Unevaluated bool             `json:"unevaluated"`
	}
	fetch.Code = dispatchTUIData(ctx, dispatcher, scope,
		core.Request{Verb: "grep", Args: map[string]any{"query": boardGrepPhrase(query), "kind": "ticket"}}, &env)
	fetch.Rows = env.Rows
	fetch.Unevaluated = env.Unevaluated
	return fetch
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
