package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// renderConfineBudgetResponse prints AIRA-180's Face 2, worst-first.
//
// Every column is an observation or an explicit unevaluated, and the
// recommendation is printed as a separate indented line rather than as a column
// value, so it can never be read as a setting that is in force. Each one ends in
// the "NOT applied" clause the classifier writes; nothing here applies anything.
func renderConfineBudgetResponse(response core.Response, stdout, stderr io.Writer) int {
	var result runner.ConfineBudgetResult
	data := response.RawData
	if len(data) == 0 {
		data, _ = json.Marshal(response.Data)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return render(core.Response{Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": invalid confine-budget response", Exit: codes.ExitForCode(daemon.CodeProtocol)}, false, stdout, stderr)
	}
	// Stated before the rows, not after: a reader who acts on the first line must
	// already know the history is machine-wide and cross-project.
	_, _ = fmt.Fprintf(stdout, "universe: %s\n", result.Scope)
	if len(result.Subjects) == 0 {
		_, _ = fmt.Fprintln(stdout, "no usage history recorded yet — run something under `aira confine` first")
		return 0
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "DIRECTION\tGRANTED\tOBSERVED-MAX\tUSABLE/TOTAL\tSUBJECT")
	for _, row := range result.Subjects {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%d/%d\t%s\n",
			row.Direction, confineBudgetBytes(row.Budget), confineBudgetBytes(row.ObservedMax),
			row.UsableSamples, row.TotalSamples, row.Subject)
	}
	_ = table.Flush()
	for _, row := range result.Subjects {
		if row.Recommendation != "" {
			_, _ = fmt.Fprintf(stdout, "  %s\n", row.Recommendation)
		}
	}
	return 0
}

// confineBudgetBytes renders an absent term as `unevaluated`, never as 0. A
// fabricated zero here would read as "granted nothing" or "used nothing", both
// of which are claims the data does not support.
func confineBudgetBytes(value *int64) string {
	if value == nil {
		return "unevaluated"
	}
	return runner.FormatConfineBytes(*value)
}
