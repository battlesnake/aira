package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/store"
)

// AIRA-336. `aira confine --budget` is bounded by default: the worst 50
// actionable subjects, an envelope that counts every hidden row, and a human
// render whose size does not grow with the history.

// largeBudgetPages is a 10,000-subject history served in five pages: 100 under,
// 400 over, 500 acceptable, 500 well-fitted and 8,500 unevaluated, every subject
// 300 bytes long (longer than the 200-rune human elision).
func largeBudgetPages() []any {
	const perPage = 2000
	var rows []runner.ConfineBudgetRow
	for i := 0; i < 10000; i++ {
		subject := fmt.Sprintf("confine / cmd-%05d-", i) + strings.Repeat("x", 300-len(fmt.Sprintf("confine / cmd-%05d-", i)))
		row := runner.ConfineBudgetRow{Kind: "confine", Subject: subject, UsableSamples: 3, TotalSamples: 3}
		budget, peak := int64(1<<30), int64(1<<20+int64(i))
		switch {
		case i < 100:
			raise := int64(2<<30 + i)
			row.Direction, row.Budget, row.ObservedMax, row.RecommendedBudget = store.ResourceBudgetUnderProvisioned, &budget, &peak, &raise
			row.Recommendation = subject + ": granted 1G — under-provisioned; consider --memory-reserve 2G. NOT applied — nothing was changed."
		case i < 500:
			lower := int64(1<<20 + i)
			row.Direction, row.Budget, row.ObservedMax, row.RecommendedBudget = store.ResourceBudgetOverProvisioned, &budget, &peak, &lower
			row.Recommendation = subject + ": granted 1G — over-provisioned; consider --memory-reserve 2M. NOT applied — nothing was changed."
		case i < 1000:
			row.Direction, row.Budget, row.ObservedMax = store.ResourceBudgetAcceptable, &budget, &peak
		case i < 1500:
			row.Direction, row.Budget, row.ObservedMax = store.ResourceBudgetWellFitted, &budget, &peak
		default:
			row.Direction, row.Unevaluated, row.UnevaluatedReason = store.ResourceBudgetUnevaluated, true, "fallback:insufficient-samples:n=1"
			row.UsableSamples, row.TotalSamples = 1, 1
		}
		rows = append(rows, row)
	}
	var pages []any
	for start := 0; start < len(rows); start += perPage {
		page := runner.ConfineBudgetResult{Verdict: "ok", Scope: store.ResourceBudgetUniverseScope(), Subjects: rows[start : start+perPage]}
		if start+perPage < len(rows) {
			page.Next = &runner.ConfineHistoryCursor{Kind: "confine", Signature: fmt.Sprintf("%05d", start+perPage-1)}
		}
		pages = append(pages, page)
	}
	return pages
}

func runBudgetCLI(t *testing.T, peer *historyPeer, jsonOutput bool, selection ...string) (int, string, string) {
	t.Helper()
	args := map[string]any{"owner": "session-a"}
	for i := 0; i+1 < len(selection); i += 2 {
		args[selection[i]] = selection[i+1]
	}
	var stdout, stderr bytes.Buffer
	exit := dispatchConfineManagementRequest(context.Background(), core.Request{Verb: "confine-budget", Args: args}, jsonOutput, &stdout, &stderr, peer.dispatcher())
	return exit, stdout.String(), stderr.String()
}

func decodeBudgetJSON(t *testing.T, out string) runner.ConfineBudgetResult {
	t.Helper()
	var envelope struct {
		Data runner.ConfineBudgetResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("not JSON: %v\n%.300s", err, out)
	}
	return envelope.Data
}

// The regression: on a 10,000-subject history the DEFAULT output, in both
// modes, stays under a fixed ceiling. The unbounded join this replaces printed
// every subject (about 4 MB of JSON here; 100 MB on the live 180k-row history).
//
// verifies: AIRA-336
func TestConfineBudgetDefaultOutputIsBoundedOnALargeHistory(t *testing.T) {
	const ceiling = 64 << 10
	for _, jsonOutput := range []bool{true, false} {
		peer := &historyPeer{t: t, pages: largeBudgetPages()}
		exit, out, errOut := runBudgetCLI(t, peer, jsonOutput)
		if exit != 0 {
			t.Fatalf("json=%v exit=%d stderr=%q", jsonOutput, exit, errOut)
		}
		if len(out) > ceiling {
			t.Fatalf("json=%v: default output is %d bytes, over the %d-byte ceiling", jsonOutput, len(out), ceiling)
		}
		if len(peer.requests) != 5 {
			t.Fatalf("json=%v: %d page requests, want 5 (the whole history is still read)", jsonOutput, len(peer.requests))
		}
		for _, request := range peer.requests {
			for _, key := range []string{"limit", "direction", "grep"} {
				if _, sent := request[key]; sent {
					t.Fatalf("the client-side selection argument %q reached the daemon: %v", key, request)
				}
			}
		}
	}
	peer := &historyPeer{t: t, pages: largeBudgetPages()}
	_, out, _ := runBudgetCLI(t, peer, true)
	result := decodeBudgetJSON(t, out)
	if len(result.Subjects) != 50 || *result.Total != 10000 || *result.Matched != 500 || *result.Returned != 50 || !*result.Truncated || result.Next != nil || result.Paged {
		t.Fatalf("envelope: rows=%d total=%v matched=%v returned=%v truncated=%v next=%v paged=%v",
			len(result.Subjects), *result.Total, *result.Matched, *result.Returned, *result.Truncated, result.Next, result.Paged)
	}
	// Worst first: every under-provisioned subject's raise is larger than any
	// over-provisioned subject's, so the 50 are the 50 biggest raises.
	if result.Subjects[0].Subject[:20] != "confine / cmd-00099-" || result.Subjects[49].Subject[:20] != "confine / cmd-00050-" {
		t.Fatalf("not worst-first: first %q last %q", result.Subjects[0].Subject[:20], result.Subjects[49].Subject[:20])
	}
	if len(result.Subjects[0].Subject) != 300 {
		t.Fatalf("--json must keep the whole subject, got %d bytes", len(result.Subjects[0].Subject))
	}
	// And everything is still reachable on request.
	peer = &historyPeer{t: t, pages: largeBudgetPages()}
	_, out, _ = runBudgetCLI(t, peer, true, "direction", "all", "limit", "0")
	if all := decodeBudgetJSON(t, out); len(all.Subjects) != 10000 || *all.Truncated {
		t.Fatalf("--direction all --limit 0: %d rows truncated=%v", len(all.Subjects), *all.Truncated)
	}
}

// verifies: AIRA-336 — the human render states the distribution before any
// row, elides long subjects (rows, recommendation and reason lines alike), and
// ends with how many rows were not shown and how to widen.
func TestConfineBudgetHumanRenderCountsElidesAndFooters(t *testing.T) {
	_, out, _ := runBudgetCLI(t, &historyPeer{t: t, pages: largeBudgetPages()}, false)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if want := "subjects: 10000 (under-provisioned 100, over-provisioned 400, acceptable 500, well-fitted 500, unevaluated 8500)"; lines[1] != want {
		t.Fatalf("distribution line = %q, want %q", lines[1], want)
	}
	if want := "9950 more not shown (450 beyond --limit, 9500 in other directions); widen with --limit N (0 = all) and --direction D|all"; lines[len(lines)-1] != want {
		t.Fatalf("footer = %q, want %q", lines[len(lines)-1], want)
	}
	for _, line := range lines[2 : len(lines)-1] {
		if strings.Contains(line, strings.Repeat("x", 250)) {
			t.Fatalf("a subject was not elided: %.120q", line)
		}
	}
	if !strings.Contains(out, "… [+100 bytes; --json has it whole]") {
		t.Fatalf("elision marker missing:\n%.600s", out)
	}
	if !strings.Contains(out, "… [+100 bytes; --json has it whole]: granted 1G — under-provisioned") {
		t.Fatalf("the recommendation line must keep its sentence after the elided subject:\n%.1500s", out)
	}
	// An unevaluated row's reason line is elided the same way.
	_, out, _ = runBudgetCLI(t, &historyPeer{t: t, pages: largeBudgetPages()}, false, "direction", "unevaluated", "limit", "1")
	if !strings.Contains(out, "… [+100 bytes; --json has it whole]: unevaluated — fallback:insufficient-samples:n=1") ||
		!strings.Contains(out, "9999 more not shown (8499 beyond --limit, 1500 in other directions)") {
		t.Fatalf("unevaluated selection:\n%s", out)
	}
}

// verifies: AIRA-336 — an empty SELECTION over a non-empty history never reads
// as an empty history, and an unreachable daemon is still `unevaluated` with no
// envelope (no fabricated total of zero).
func TestConfineBudgetEmptySelectionIsNotAnEmptyHistory(t *testing.T) {
	onlyQuiet := []any{runner.ConfineBudgetResult{Verdict: "ok", Scope: "s", Subjects: []runner.ConfineBudgetRow{
		budgetRow("a", store.ResourceBudgetWellFitted, i64(100), i64(120), false),
		budgetRow("b", store.ResourceBudgetUnevaluated, nil, nil, true),
	}}}
	exit, out, _ := runBudgetCLI(t, &historyPeer{t: t, pages: onlyQuiet}, false)
	if exit != 0 || strings.Contains(out, "no usage history") || !strings.Contains(out, "no subject in the selected direction; 2 not shown — widen with --direction all") {
		t.Fatalf("exit=%d:\n%s", exit, out)
	}
	_, out, _ = runBudgetCLI(t, &historyPeer{t: t, pages: onlyQuiet}, false, "grep", "zzz")
	if strings.Contains(out, "no usage history") || !strings.Contains(out, `no subject matches --grep "zzz"`) {
		t.Fatalf("grep miss:\n%s", out)
	}
	empty := []any{runner.ConfineBudgetResult{Verdict: "ok", Scope: "s", Subjects: []runner.ConfineBudgetRow{}}}
	_, out, _ = runBudgetCLI(t, &historyPeer{t: t, pages: empty}, false)
	if !strings.Contains(out, "no usage history recorded yet") {
		t.Fatalf("a truly empty history:\n%s", out)
	}

	d := &daemonDispatcher{}
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
	}
	for _, jsonOutput := range []bool{true, false} {
		var stdout, stderr bytes.Buffer
		exit := dispatchConfineManagementRequest(context.Background(), core.Request{Verb: "confine-budget", Args: map[string]any{"owner": "session-a"}}, jsonOutput, &stdout, &stderr, d)
		if exit != 3 || !strings.Contains(stdout.String(), "unevaluated") || strings.Contains(stdout.String(), "no usage history") ||
			strings.Contains(stdout.String(), `"total"`) || strings.Contains(stdout.String(), "subjects: ") {
			t.Fatalf("json=%v daemon down: exit=%d stdout=%q", jsonOutput, exit, stdout.String())
		}
	}
}

// verifies: AIRA-336 — version skew: an old daemon that ignores paging answers
// the whole history in one reply, and the client selects from it exactly as
// from joined pages.
func TestConfineBudgetOldDaemonSingleReplyIsSelected(t *testing.T) {
	var rows []runner.ConfineBudgetRow
	for i := 0; i < 120; i++ {
		rows = append(rows, budgetRow(fmt.Sprintf("s%03d", i), store.ResourceBudgetOverProvisioned, i64(10), i64(100), false))
	}
	rows = append(rows, budgetRow("quiet", store.ResourceBudgetUnevaluated, nil, nil, true))
	peer := &historyPeer{t: t, pages: []any{prePaging{runner.ConfineBudgetResult{Verdict: "ok", Scope: "s", Subjects: rows}}}}
	result := dispatchBudget(t, peer)
	if len(peer.requests) != 1 || len(result.Subjects) != 50 || *result.Total != 121 || *result.Matched != 120 || !*result.Truncated {
		t.Fatalf("requests=%d rows=%d total=%v matched=%v", len(peer.requests), len(result.Subjects), *result.Total, *result.Matched)
	}
}

// verifies: AIRA-336 — a bad selection is refused with the stable argument code
// before any daemon exchange; an MCP caller's JSON-number --limit is read as its
// integer, and a fractional one is refused rather than truncated.
func TestConfineBudgetSelectionArgumentsAreValidated(t *testing.T) {
	rows := []any{runner.ConfineBudgetResult{Verdict: "ok", Scope: "s", Subjects: []runner.ConfineBudgetRow{
		budgetRow("a", store.ResourceBudgetOverProvisioned, i64(1), i64(9), false),
		budgetRow("b", store.ResourceBudgetOverProvisioned, i64(1), i64(9), false),
	}}}
	for name, args := range map[string]map[string]any{
		"direction": {"direction": "sideways"},
		"limit":     {"limit": "-3"},
		"fraction":  {"limit": float64(1.5)},
	} {
		peer := &historyPeer{t: t, pages: rows}
		args["owner"] = "session-a"
		response := peer.dispatcher().Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{Verb: "confine-budget", Args: args})
		if response.OK || response.Code != "E_CONFINE_ARGUMENT_INVALID" || response.Exit != 2 || len(peer.requests) != 0 {
			t.Fatalf("%s: ok=%v code=%q exit=%d requests=%d", name, response.OK, response.Code, response.Exit, len(peer.requests))
		}
	}
	peer := &historyPeer{t: t, pages: rows}
	response := peer.dispatcher().Dispatch(context.Background(), daemon.WorktreeScope{},
		core.Request{Verb: "confine-budget", Args: map[string]any{"owner": "session-a", "limit": float64(1)}})
	data, _ := json.Marshal(response.Data)
	var result runner.ConfineBudgetResult
	if err := json.Unmarshal(data, &result); err != nil || len(result.Subjects) != 1 {
		t.Fatalf("numeric limit 1: ok=%v rows=%d err=%v", response.OK, len(result.Subjects), err)
	}
}

// verifies: AIRA-336 — both CLI spellings carry --limit/--direction/--grep to
// the dispatch, and the management form refuses them beside any flag but
// --budget (an accepted-and-ignored option would be silently discarded).
func TestConfineBudgetSelectionFlagsReachTheDispatch(t *testing.T) {
	for _, argv := range [][]string{
		{"confine", "--budget", "--limit", "7", "--direction", "over", "--grep", "make test"},
		{"confine-budget", "--limit", "7", "--direction", "over", "--grep", "make test"},
	} {
		var seen map[string]any
		injected := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
			seen = request.Args
			return core.Response{OK: true, Code: "OK", Data: runner.ConfineBudgetResult{Verdict: "ok", Scope: "s", Subjects: []runner.ConfineBudgetRow{}}}
		})
		var stdout, stderr bytes.Buffer
		if exit := RunWithDispatcher(argv, &stdout, &stderr, injected); exit != 0 {
			t.Fatalf("%v: exit=%d stderr=%q", argv, exit, stderr.String())
		}
		if seen["limit"] != "7" || seen["direction"] != "over" || seen["grep"] != "make test" {
			t.Fatalf("%v: dispatched args %v", argv, seen)
		}
	}
	for _, argv := range [][]string{
		{"confine", "--list", "--limit", "5"},
		{"confine", "--list", "--grep", "x"},
		{"confine", "--kill", "job", "--direction", "all"},
	} {
		injected := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
			t.Fatalf("%v reached the dispatch", argv)
			return core.Response{}
		})
		var stdout, stderr bytes.Buffer
		exit := RunWithDispatcher(argv, &stdout, &stderr, injected)
		if exit != 2 || !strings.Contains(stdout.String()+stderr.String(), "is valid only with --budget") {
			t.Fatalf("%v: exit=%d stdout=%q stderr=%q", argv, exit, stdout.String(), stderr.String())
		}
	}
}
