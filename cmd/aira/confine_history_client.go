package main

import (
	"context"
	"encoding/json"
	"fmt"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/store"
)

// maxConfineHistoryPages is a backstop on the paging loop, far above any real
// history: the cursor-advance check below is what actually guarantees progress,
// this only bounds a daemon that advances by one byte a page forever.
const maxConfineHistoryPages = 1 << 20

// dispatchConfineHistory answers confine-dump and confine-budget (AIRA-280).
//
// Their reply grows with the machine-wide peak-RSS history and could exceed the
// 16 MiB frame, so the client asks for it in pages (`paged: true` plus a cursor)
// and joins them into one result: renderers, the dump file writer and the MCP
// face never see a page. A dump or budget is complete or it is an error -- never
// a silently truncated join:
//
//   - a page that is not an OK reply with an "ok" verdict (an error, or the
//     daemon-down UNEVALUATED answer) is returned as the whole answer;
//   - a `next` cursor that does not sort strictly after the previous one is
//     E_DAEMON_PROTOCOL, so a stuck daemon cannot spin the client;
//   - every page after the first must carry the daemon's `paged` echo, so a
//     pre-paging daemon taking over mid-read (its whole-history reply) is
//     E_DAEMON_PROTOCOL rather than a join of duplicate rows;
//   - a page without `next` ends the join, even an empty one: history deleted
//     between two page reads can leave nothing after the cursor.
//
// An old daemon ignores `paged` and answers everything in one reply with no
// `next`; that first reply is returned untouched (a dump) or selected from (a
// budget).
//
// AIRA-336. A budget is SELECTED while it is joined, never accumulated whole:
// every row is counted into the distribution and only the worst --limit rows of
// the selected --direction set are kept (store.ConfineBudgetSelector), so the
// client's memory and output are bounded by --limit, not by the machine-wide
// history. The selection arguments are the client's alone and are not sent to
// the daemon, so any daemon, old or new, is selected from the same way.
func (d *daemonDispatcher) dispatchConfineHistory(ctx context.Context, request core.Request) core.Response {
	var (
		cursor    runner.ConfineHistoryCursor
		haveFirst bool
		dump      runner.ConfineDumpResult
		budget    runner.ConfineBudgetResult
		selector  *store.ConfineBudgetSelector
	)
	isDump := request.Verb == "confine-dump"
	if !isDump {
		var text [3]string
		for i, key := range []string{"direction", "limit", "grep"} {
			value, ok := request.Args[key].(string)
			if !ok && request.Args[key] != nil {
				// Every face sends text (MCP refuses a non-string before it gets
				// here); anything else is refused, never read as the default.
				return confineClientError(fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %s must be a string", key))
			}
			text[i] = value
		}
		filter, err := store.ParseConfineBudgetFilter(text[0], text[1], text[2])
		if err != nil {
			return confineClientError(err)
		}
		selector = store.NewConfineBudgetSelector(filter)
	}
	for page := 0; page < maxConfineHistoryPages; page++ {
		pageRequest := request
		pageRequest.Args = make(map[string]any, len(request.Args)+3)
		for key, value := range request.Args {
			if key == "direction" || key == "limit" || key == "grep" {
				continue
			}
			pageRequest.Args[key] = value
		}
		pageRequest.Args["paged"] = true
		pageRequest.Args["after_kind"] = cursor.Kind
		pageRequest.Args["after_signature"] = cursor.Signature
		response := d.dispatchConfineManagementOnce(ctx, pageRequest)
		if !response.OK {
			return response
		}
		var next *runner.ConfineHistoryCursor
		var verdict string
		var echoedPaged bool
		// The daemon-down fallback carries a typed value, not wire bytes.
		data, err := responsePayload(response)
		if err != nil {
			return historyProtocolError("invalid response data")
		}
		if isDump {
			var result runner.ConfineDumpResult
			if err := json.Unmarshal(data, &result); err != nil {
				return historyProtocolError("invalid confine-dump response")
			}
			verdict, next, echoedPaged = result.Verdict, result.Next, result.Paged
			if verdict == "ok" && haveFirst && !echoedPaged {
				return notPagedError()
			}
			if verdict == "ok" {
				if !haveFirst {
					dump = result
				} else {
					dump.Admissions = append(dump.Admissions, result.Admissions...)
				}
			}
		} else {
			var result runner.ConfineBudgetResult
			if err := json.Unmarshal(data, &result); err != nil {
				return historyProtocolError("invalid confine-budget response")
			}
			verdict, next, echoedPaged = result.Verdict, result.Next, result.Paged
			if verdict == "ok" && haveFirst && !echoedPaged {
				return notPagedError()
			}
			if verdict == "ok" {
				selector.Add(result.Subjects)
				if !haveFirst {
					budget = result
					budget.Subjects = nil
				}
			}
		}
		if verdict != "ok" {
			// An unevaluated answer is the whole answer, whichever page it arrived on.
			return response
		}
		if !haveFirst && next == nil && isDump {
			// One complete reply (an old daemon, or a history that fits one page).
			return response
		}
		haveFirst = true
		if next == nil {
			return joinedHistoryResponse(isDump, dump, budget, selector)
		}
		if !next.After(cursor) {
			return historyProtocolError("confine history page did not advance")
		}
		cursor = *next
	}
	return historyProtocolError("confine history page did not advance")
}

func joinedHistoryResponse(isDump bool, dump runner.ConfineDumpResult, budget runner.ConfineBudgetResult, selector *store.ConfineBudgetSelector) core.Response {
	if isDump {
		dump.Next = nil
		return core.Response{OK: true, Code: "OK", Data: dump}
	}
	budget.Next, budget.Paged = nil, false
	selector.Finish(&budget)
	return core.Response{OK: true, Code: "OK", Data: budget}
}

// notPagedError refuses a later page that does not prove it was served as a page.
// A daemon replaced by a pre-paging one between two reads ignores the cursor and
// answers the whole history; joining that onto the earlier pages would duplicate
// every row, so it is a protocol error, never a join.
func notPagedError() core.Response {
	return historyProtocolError("confine history page was not served as a page (daemon changed mid-read)")
}

func historyProtocolError(why string) core.Response {
	return core.Response{
		Code: daemon.CodeProtocol, Error: fmt.Sprintf("%s: %s", daemon.CodeProtocol, why),
		Exit: codes.ExitForCode(daemon.CodeProtocol),
	}
}
