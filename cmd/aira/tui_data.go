package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"

	"aira/internal/app"
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
	if root, ok := overviewCardViewRoot(view); ok {
		// AIRA-252 Increment 2. A dynamic per-project overview fetch: the View string
		// carries the project's canonical root, which fetchOverviewCard resolves into
		// a per-project scope for the count/lease dispatch. This is the viewTop
		// scope-override precedent — a plain cmdFetch reaches here untouched, so the
		// overview needs no executor change (spec §12).
		result.OverviewCard = fetchOverviewCard(ctx, dispatcher, root)
		return result
	}
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
	case viewOverview:
		// AIRA-252 Increment 2. The all-projects overview list: the pure-read
		// registry + per-project Discover + one machine-wide confine (spec §11). A
		// registry read that fails wholesale becomes result.Code so the reducer keeps
		// the last-good cards and raises a banner (spec §14); a per-project Discover
		// failure or a confine failure is carried IN the data (that project reads
		// "unavailable"; the jobs strip reads "unevaluated") and never blanks the rest.
		data := fetchOverviewList(ctx, dispatcher)
		if data.RegistryCode != "" {
			result.Code = data.RegistryCode
		} else {
			result.Overview = data
		}
	case viewOverviewJobs:
		// AIRA-252 Increment 2. The light machine-wide confine tick (spec §13),
		// dispatched with an EMPTY scope exactly as viewTop does.
		var confine runner.ConfineListResult
		request := core.Request{Verb: "confine-list", Args: map[string]any{
			"slice": runner.ResolveConfineSlice(""), "owner": runner.ConfineUnknownOwner,
		}}
		if result.Code = dispatchTUIData(ctx, dispatcher, daemon.WorktreeScope{}, request, &confine); result.Code == "" {
			result.Top = &confine
		}
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
	// Dispatch ready directly (not via the decode helper) so the ENVELOPE verdict
	// code survives: the no-selector ready overlay can arrive OK:true with response
	// Code "UNEVALUATED" (core.go:619), which decodeTUIResponse flattens to "". A
	// wholly-unevaluated overlay must not let an absent workable card read "ready".
	if ctx.Err() != nil {
		data.ReadyCode = "E_TUI_CANCELLED"
	} else {
		readyResponse := dispatcher.Dispatch(ctx, scope, core.Request{Verb: "ready", Args: map[string]any{}})
		data.ReadyCode = decodeTUIResponse(readyResponse, &data.Ready)
		if readyResponse.Code == "UNEVALUATED" {
			data.ReadyUnevaluated = true
		}
		collect(readyResponse.Warnings)
	}

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
	Truncated   bool // grep hit the 50-cap (P2.6): the result set is partial
}

func fetchBoardSearch(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, query string) boardSearchFetch {
	fetch := boardSearchFetch{Query: query}
	var env struct {
		Total       int              `json:"total"`
		Rows        []map[string]any `json:"rows"`
		Unevaluated bool             `json:"unevaluated"`
		Truncated   bool             `json:"truncated"`
	}
	fetch.Code = dispatchTUIData(ctx, dispatcher, scope,
		core.Request{Verb: "grep", Args: map[string]any{"query": boardGrepPhrase(query), "kind": "ticket"}}, &env)
	fetch.Rows = env.Rows
	fetch.Unevaluated = env.Unevaluated
	fetch.Truncated = env.Truncated
	return fetch
}

// boardGetFetch is the reply to an id-shaped query's `show` existence probe
// (P2.5). Found distinguishes a resolvable ticket (open it) from a genuine
// E_NOT_FOUND (honest "not found") — never conflated with grep's "no matches".
type boardGetFetch struct {
	Query string
	Found bool
	Title string
}

func fetchBoardGet(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, id string) boardGetFetch {
	fetch := boardGetFetch{Query: id}
	var row map[string]any
	if code := dispatchTUIData(ctx, dispatcher, scope,
		core.Request{Verb: "show", Args: map[string]any{"selector": id}}, &row); code == "" && row != nil {
		fetch.Found = true
		fetch.Title = textCell(row["title"])
	}
	return fetch
}

// The overview's impure seams (AIRA-252 Increment 2), package vars so the pure
// grouping/card logic is unit-tested directly and the runtime smoke test can
// inject a controlled registry + Discover outcome without a real filesystem.
// They default to the real reads: registry file, app.Discover, ScopeFromProject,
// os.Stat — exactly what the daemon's discoverRegistryPass uses.
var (
	overviewRegistrySnapshot = func() ([]store.RegistryEntry, error) {
		paths, err := daemon.PathsFromEnv()
		if err != nil {
			return nil, err
		}
		return store.ListRegistryEntries(paths.RegistryPath)
	}
	overviewDiscover   = app.Discover
	overviewBuildScope = func(project app.Project) (daemon.WorktreeScope, error) {
		paths, err := daemon.PathsFromEnv()
		if err != nil {
			return daemon.WorktreeScope{}, err
		}
		return daemon.ScopeFromProject(project, paths)
	}
	overviewRootExists = func(root string) bool {
		_, err := os.Stat(root)
		return err == nil
	}
)

// overviewCardResult is one project's raw lazy count/lease reply (spec §11.6).
// Code carries a count dispatch failure — E_NOT_ADOPTED means the project was
// ejected though its registry entries persist (§11.5); any other code is
// unevaluated. LeaseCode carries the lease read's own failure separately.
type overviewCardResult struct {
	ProjectID    string
	Distribution map[string]int
	Total        int
	Stale        bool // the count reply's envelope carried W_STALE_INDEX (§14)
	Code         string
	LeaseCount   int
	LeaseCode    string
}

// fetchOverviewList composes the overview's list read: the pure-read registry,
// grouped/deduped/dead-root-skipped, each canonical root Discovered for its slug
// + scope, plus one machine-wide confine. Every failure is carried per-section —
// a registry read failure is the only whole-list error (no cards to show).
func fetchOverviewList(ctx context.Context, dispatcher Dispatcher) *overviewListData {
	data := &overviewListData{Discoveries: map[string]overviewDiscovery{}, Scopes: map[string]daemon.WorktreeScope{}}
	entries, err := overviewRegistrySnapshot()
	if err != nil {
		data.RegistryCode = decodeErrorCode(err)
		return data
	}
	data.Groups = overviewGroupProjects(entries, overviewRootExists)
	for _, group := range data.Groups {
		if err := ctx.Err(); err != nil {
			data.Discoveries[group.ProjectID] = overviewDiscovery{Code: "E_TUI_CANCELLED"}
			continue
		}
		project, discoverErr := overviewDiscover(ctx, group.Canonical.Root)
		if discoverErr != nil {
			data.Discoveries[group.ProjectID] = overviewDiscovery{Code: decodeErrorCode(discoverErr)}
			continue
		}
		discovery := overviewDiscovery{Slug: project.Config.Project.Slug, Prefixes: project.Config.Project.Prefixes}
		if scope, scopeErr := overviewBuildScope(project); scopeErr == nil {
			data.Scopes[group.ProjectID] = scope
		} else {
			// Discovered but the scope could not be built: available in name only,
			// so it reads unavailable rather than pretending it is dispatchable.
			discovery.Code = decodeErrorCode(scopeErr)
		}
		data.Discoveries[group.ProjectID] = discovery
	}
	var confine runner.ConfineListResult
	confineRequest := core.Request{Verb: "confine-list", Args: map[string]any{
		"slice": runner.ResolveConfineSlice(""), "owner": runner.ConfineUnknownOwner,
	}}
	if data.ConfineCode = dispatchTUIData(ctx, dispatcher, daemon.WorktreeScope{}, confineRequest, &confine); data.ConfineCode == "" {
		data.Confine = &confine
	}
	return data
}

// fetchOverviewCard is one project's lazy count/lease read (spec §11.6). It
// re-Discovers the root (a project can vanish between the list and the card
// fetch) and dispatches count --by status + lease ls with the per-project scope.
// It ALWAYS returns a non-nil result carrying the ProjectID (or "" when Discover
// failed) so the reducer can key it — the honest join never fabricates a "0".
func fetchOverviewCard(ctx context.Context, dispatcher Dispatcher, root string) *overviewCardResult {
	result := &overviewCardResult{}
	project, err := overviewDiscover(ctx, root)
	if err != nil {
		result.Code = decodeErrorCode(err)
		return result
	}
	result.ProjectID = project.ProjectID
	scope, scopeErr := overviewBuildScope(project)
	if scopeErr != nil {
		result.Code = decodeErrorCode(scopeErr)
		return result
	}
	var count struct {
		Total        int            `json:"total"`
		Distribution map[string]int `json:"distribution"`
	}
	// Use the warnings-carrying decode so W_STALE_INDEX on the count envelope is
	// surfaced as a per-card stale marker (spec §14) rather than the distribution
	// reading authoritative when a reconcile is pending (P2 review fix).
	var warnings []string
	result.Code, warnings = dispatchTUIDataWithWarnings(ctx, dispatcher, scope,
		core.Request{Verb: "count", Args: map[string]any{"query": "", "by": "status"}}, &count)
	if result.Code == "" {
		result.Distribution, result.Total = count.Distribution, count.Total
		for _, warning := range warnings {
			if warning == "W_STALE_INDEX" {
				result.Stale = true
			}
		}
	}
	var leases struct {
		Total int                  `json:"total"`
		Rows  []store.HeldLeaseRow `json:"rows"`
	}
	result.LeaseCode = dispatchTUIData(ctx, dispatcher, scope,
		core.Request{Verb: "lease", Args: map[string]any{"subverb": "ls"}}, &leases)
	if result.LeaseCode == "" {
		result.LeaseCount = len(leases.Rows)
	}
	return result
}

// decodeErrorCode extracts a stable error code from an app/store error string
// (its "E_CODE: message" prefix), for the honest per-project state code.
func decodeErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if code := store.ErrorCode(err); code != "" {
		return code
	}
	return "E_INTERNAL"
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
