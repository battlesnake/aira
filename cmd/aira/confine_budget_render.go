package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/store"
)

// renderConfineBudgetResponse prints AIRA-180's Face 2, worst-first.
//
// Every column is an observation or an explicit unevaluated, and the
// recommendation is printed as a separate indented line rather than as a column
// value, so it can never be read as a setting that is in force. Each one ends in
// the "NOT applied" clause the classifier writes; nothing here applies anything.
//
// AIRA-336. grep is the --grep the request carried, so an empty selection can
// say which question had no answer: "no usage history" is said only when the
// whole history is empty, never when a filter hid every row.
func renderConfineBudgetResponse(response core.Response, grep string, stdout, stderr io.Writer) int {
	var result runner.ConfineBudgetResult
	data, _ := responsePayload(response)
	if err := json.Unmarshal(data, &result); err != nil {
		return render(core.Response{Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": invalid confine-budget response", Exit: codes.ExitForCode(daemon.CodeProtocol)}, false, stdout, stderr)
	}
	// AIRA-201. An UNEVALUATED result also carries zero subjects, so this must be
	// tested before the empty-history line below — otherwise "we could not look"
	// renders as "we looked and there is nothing", which is the exact class of
	// fabrication this verb exists to avoid. Checked on the verdict rather than on
	// the reason string, so a future unevaluated path that forgets its reason
	// still cannot fall through to the wrong sentence.
	if result.Verdict == "unevaluated" {
		reason := result.Reason
		if reason == "" {
			reason = "no reason was reported"
		}
		_, _ = fmt.Fprintf(stdout, "confine budget: unevaluated: %s\n", reason)
		// Mirrors renderConfineListResponse (main.go:3302-3307): carry the
		// response's own exit when it set one, else the project's unevaluated 3.
		if response.Exit != 0 {
			return response.Exit
		}
		return 3
	}
	// Stated before the rows, not after: a reader who acts on the first line must
	// already know the history is machine-wide and cross-project.
	_, _ = fmt.Fprintf(stdout, "universe: %s\n", result.Scope)
	if result.Total != nil {
		_, _ = fmt.Fprintln(stdout, confineBudgetDistributionLine(result, grep))
	}
	if len(result.Subjects) == 0 {
		switch {
		case result.Total == nil || *result.Total == 0 && grep == "":
			_, _ = fmt.Fprintln(stdout, "no usage history recorded yet — run something under `aira confine` first")
		case *result.Total == 0:
			_, _ = fmt.Fprintf(stdout, "no subject matches --grep %q\n", grep)
		default:
			_, _ = fmt.Fprintf(stdout, "no subject in the selected direction; %d not shown — widen with --direction all\n", *result.Total)
		}
		return 0
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "DIRECTION\tGRANTED\tOBSERVED-MAX\tUSABLE/TOTAL\tSUBJECT")
	for _, row := range result.Subjects {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%d/%d\t%s\n",
			row.Direction, confineBudgetBytes(row.Budget), confineBudgetBytes(row.ObservedMax),
			row.UsableSamples, row.TotalSamples, elideBudgetSubject(row.Subject))
	}
	_ = table.Flush()
	for _, row := range result.Subjects {
		if row.Recommendation != "" {
			// The classifier's sentence opens with the subject; elide it there too.
			recommendation := row.Recommendation
			if rest, ok := strings.CutPrefix(recommendation, row.Subject); ok {
				recommendation = elideBudgetSubject(row.Subject) + rest
			}
			_, _ = fmt.Fprintf(stdout, "  %s\n", recommendation)
		}
	}
	// An `unevaluated` DIRECTION with no reason would leave the reader guessing
	// between "too few samples", "budget never recorded" and "this launch shape
	// has no evaluable sizing" — three different next actions. The reason is
	// part of the answer, not --json-only detail (final build-review fix).
	for _, row := range result.Subjects {
		if row.Unevaluated && row.UnevaluatedReason != "" {
			_, _ = fmt.Fprintf(stdout, "  %s: unevaluated — %s\n", elideBudgetSubject(row.Subject), row.UnevaluatedReason)
		}
	}
	if result.Truncated != nil && *result.Truncated && result.Total != nil && result.Returned != nil {
		_, _ = fmt.Fprintf(stdout, "%d more not shown; widen with --limit N (0 = all) and/or --direction D|all\n", *result.Total-*result.Returned)
	}
	return 0
}

// confineBudgetDistributionLine counts every subject by direction, worst
// first, so a reader sees what the selection left out before any row.
func confineBudgetDistributionLine(result runner.ConfineBudgetResult, grep string) string {
	var line strings.Builder
	_, _ = fmt.Fprintf(&line, "subjects: %d", *result.Total)
	if grep != "" {
		_, _ = fmt.Fprintf(&line, " matching --grep %q", grep)
	}
	known := []string{store.ResourceBudgetUnderProvisioned, store.ResourceBudgetOverProvisioned,
		store.ResourceBudgetAcceptable, store.ResourceBudgetWellFitted, store.ResourceBudgetUnevaluated}
	seen := map[string]bool{}
	var parts []string
	for _, direction := range known {
		seen[direction] = true
		parts = append(parts, fmt.Sprintf("%s %d", direction, result.Distribution[direction]))
	}
	var extra []string
	for direction := range result.Distribution {
		if !seen[direction] {
			extra = append(extra, direction)
		}
	}
	sort.Strings(extra)
	for _, direction := range extra {
		parts = append(parts, fmt.Sprintf("%s %d", direction, result.Distribution[direction]))
	}
	_, _ = fmt.Fprintf(&line, " (%s)", strings.Join(parts, ", "))
	return line.String()
}

// confineBudgetSubjectRunes bounds a subject in a human row. A signature is a
// whole argv and has been seen at 1 MiB; --json keeps it whole.
const confineBudgetSubjectRunes = 200

// elideBudgetSubject cuts a subject longer than confineBudgetSubjectRunes runes
// and says how much was cut.
func elideBudgetSubject(subject string) string {
	runes := 0
	for index := range subject {
		if runes == confineBudgetSubjectRunes {
			return fmt.Sprintf("%s… [+%d bytes; --json has it whole]", subject[:index], len(subject)-index)
		}
		runes++
	}
	return subject
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
