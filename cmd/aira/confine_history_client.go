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
//   - a page without `next` ends the join, even an empty one: history deleted
//     between two page reads can leave nothing after the cursor.
//
// An old daemon ignores `paged` and answers everything in one reply with no
// `next`; that first reply is returned untouched.
func (d *daemonDispatcher) dispatchConfineHistory(ctx context.Context, request core.Request) core.Response {
	var (
		cursor    runner.ConfineHistoryCursor
		haveFirst bool
		dump      runner.ConfineDumpResult
		budget    runner.ConfineBudgetResult
	)
	isDump := request.Verb == "confine-dump"
	for page := 0; page < maxConfineHistoryPages; page++ {
		pageRequest := request
		pageRequest.Args = make(map[string]any, len(request.Args)+3)
		for key, value := range request.Args {
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
		data := response.RawData
		if len(data) == 0 {
			// The daemon-down fallback carries a typed value, not wire bytes.
			encoded, err := json.Marshal(response.Data)
			if err != nil {
				return historyProtocolError("invalid response data")
			}
			data = encoded
		}
		if isDump {
			var result runner.ConfineDumpResult
			if err := json.Unmarshal(data, &result); err != nil {
				return historyProtocolError("invalid confine-dump response")
			}
			verdict, next = result.Verdict, result.Next
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
			verdict, next = result.Verdict, result.Next
			if verdict == "ok" {
				if !haveFirst {
					budget = result
				} else {
					budget.Subjects = append(budget.Subjects, result.Subjects...)
				}
			}
		}
		if verdict != "ok" {
			// An unevaluated answer is the whole answer, whichever page it arrived on.
			return response
		}
		if !haveFirst && next == nil {
			// One complete reply (an old daemon, or a history that fits one page).
			return response
		}
		haveFirst = true
		if next == nil {
			return joinedHistoryResponse(isDump, dump, budget)
		}
		if !next.After(cursor) {
			return historyProtocolError("confine history page did not advance")
		}
		cursor = *next
	}
	return historyProtocolError("confine history page did not advance")
}

func joinedHistoryResponse(isDump bool, dump runner.ConfineDumpResult, budget runner.ConfineBudgetResult) core.Response {
	if isDump {
		dump.Next = nil
		return core.Response{OK: true, Code: "OK", Data: dump}
	}
	budget.Next = nil
	store.SortConfineBudgetRows(budget.Subjects)
	return core.Response{OK: true, Code: "OK", Data: budget}
}

func historyProtocolError(why string) core.Response {
	return core.Response{
		Code: daemon.CodeProtocol, Error: fmt.Sprintf("%s: %s", daemon.CodeProtocol, why),
		Exit: codes.ExitForCode(daemon.CodeProtocol),
	}
}
