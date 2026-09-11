package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// AIRA admission-counter rebuild, S18: `aira confine --dump <file>` (design
// §12). The daemon assembles the structured records (internal/daemon/ci_dump.go);
// this face performs the actual JSONL file write, atomically, on the
// INVOKING process's own filesystem -- the daemon itself never touches a
// path here and opens no network connection anywhere on this path.
func runConfineDumpCommand(ctx context.Context, options map[string]string, dumpPath string, jsonOutput bool, stdout, stderr io.Writer, injected Dispatcher) int {
	owner, err := resolveConfineOwner(ctx, options["owner"])
	if err != nil {
		return render(core.Response{Code: "E_CONFINE_ARGUMENT_INVALID", Error: "E_CONFINE_ARGUMENT_INVALID: --owner: " + err.Error(), Exit: codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID")}, jsonOutput, stdout, stderr)
	}
	request := core.Request{Verb: "confine-dump", Args: map[string]any{"slice": options["slice"], "owner": owner}}
	return runConfineDumpExchange(ctx, request, dumpPath, jsonOutput, stdout, stderr, injected)
}

// runConfineDumpExchange performs the daemon round trip itself (rather than
// through dispatchConfineManagementRequest, which always renders the reply to
// stdout/stderr) because a SUCCESSFUL confine-dump response is not something
// to print as JSON or a table -- it is the payload to write atomically to
// dumpPath, and only failure or an unevaluated verdict gets a printed message.
func runConfineDumpExchange(ctx context.Context, request core.Request, dumpPath string, jsonOutput bool, stdout, stderr io.Writer, injected Dispatcher) int {
	if request.Args == nil {
		request.Args = map[string]any{}
	}
	slice, _ := request.Args["slice"].(string)
	request.Args["slice"] = runner.ResolveConfineSlice(slice)
	dispatcher := injected
	var dispatchErr error
	if dispatcher == nil {
		dispatcher, dispatchErr = newDaemonDispatcher(nil, stdout, stderr, jsonOutput)
		if dispatchErr != nil {
			return render(transportErrorResponse(dispatchErr), jsonOutput, stdout, stderr)
		}
	}
	response := dispatcher.Dispatch(ctx, daemon.WorktreeScope{}, request)
	if !response.OK {
		return render(response, jsonOutput, stdout, stderr)
	}
	var result runner.ConfineDumpResult
	data := response.RawData
	if len(data) == 0 {
		data, _ = json.Marshal(response.Data)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return render(core.Response{Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": invalid confine-dump response", Exit: codes.ExitForCode(daemon.CodeProtocol)}, false, stdout, stderr)
	}
	// AIRA-201-style honesty: an UNEVALUATED verdict (daemon unreachable) must
	// be reported as "we could not look", never silently written as an empty
	// dump file that reads as "we looked and there was nothing to record".
	if result.Verdict == "unevaluated" {
		reason := result.Reason
		if reason == "" {
			reason = "no reason was reported"
		}
		if jsonOutput {
			data, _ := json.Marshal(map[string]any{"verdict": "unevaluated", "reason": reason})
			_, _ = fmt.Fprintln(stdout, string(data))
		} else {
			_, _ = fmt.Fprintf(stdout, "confine dump: unevaluated: %s\n", reason)
		}
		if response.Exit != 0 {
			return response.Exit
		}
		return 3
	}
	if err := runner.WriteConfineDumpJSONL(dumpPath, result); err != nil {
		return render(core.Response{Code: "E_CONFINE_DUMP_WRITE", Error: "E_CONFINE_DUMP_WRITE: " + err.Error(), Exit: codes.ExitForCode("E_CONFINE_DUMP_WRITE")}, jsonOutput, stdout, stderr)
	}
	// AIRA-82 discipline: --json is accepted for this verb (dispatchConfineManagementRequest's
	// generic render() path would honour it for a failure above), so the SUCCESS
	// summary must honour it too rather than silently discarding the flag -- an
	// accepted-and-ignored option is the same defect class AIRA-82 refuses.
	if jsonOutput {
		data, _ := json.Marshal(map[string]any{
			"written": true, "path": dumpPath,
			"admissions": len(result.Admissions), "waiters": len(result.Waiters), "queues": len(result.Queues),
		})
		_, _ = fmt.Fprintln(stdout, string(data))
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "confine dump: wrote %d admission record(s), %d waiter record(s) and %d queue record(s) to %s\n",
		len(result.Admissions), len(result.Waiters), len(result.Queues), dumpPath)
	return 0
}
