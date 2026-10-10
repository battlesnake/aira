package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// AIRA-280. The client joins the pages of a paged confine-dump / confine-budget
// into one result, so the renderers, the dump file writer and the MCP face are
// unchanged -- and a dump or budget is complete or not produced at all.

type historyPeer struct {
	t        *testing.T
	pages    []any // runner.ConfineDumpResult or runner.ConfineBudgetResult, or daemon.ResponseFrame
	requests []map[string]any
}

func (p *historyPeer) dispatcher() *daemonDispatcher {
	d := &daemonDispatcher{}
	d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		p.requests = append(p.requests, frame.Request.Args)
		if len(p.requests) > len(p.pages)+5 {
			p.t.Fatalf("the client kept asking after %d requests (a non-advancing cursor must stop it)", len(p.requests))
		}
		index := len(p.requests) - 1
		if index >= len(p.pages) {
			index = len(p.pages) - 1
		}
		switch page := p.pages[index].(type) {
		case daemon.ResponseFrame:
			return page, nil
		default:
			data, err := json.Marshal(page)
			if err != nil {
				p.t.Fatal(err)
			}
			return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
		}
	}
	return d
}

func i64(v int64) *int64 { return &v }

func dumpRow(signature string, peak *int64) runner.ConfineDumpAdmissionRow {
	return runner.ConfineDumpAdmissionRow{
		RecordType: runner.ConfineDumpRecordAdmission, Kind: "confine", Signature: signature,
		ObservedPeakBytes: peak, Outcome: runner.ConfineDumpUnevaluated,
	}
}

func readDump(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dump file: %v", err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func runDump(t *testing.T, d *daemonDispatcher, path string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := runConfineDumpExchange(context.Background(), core.Request{Verb: "confine-dump", Args: map[string]any{"owner": "session-a"}}, path, true, &stdout, &stderr, d)
	return exit, stdout.String(), stderr.String()
}

// verifies: AIRA-280 — the joined dump has every page's admissions in order, the
// first page's live state once, no cursor, and every request after the first
// carries the cursor the previous page returned.
func TestPagedDumpIsJoinedAndTheCursorIsFollowed(t *testing.T) {
	peer := &historyPeer{t: t, pages: []any{
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("a", i64(10))},
			Waiters: []runner.ConfineDumpWaiterRow{{RecordType: runner.ConfineDumpRecordWaiter, Slice: "/x"}},
			Queues:  []runner.ConfineDumpQueueRow{{RecordType: runner.ConfineDumpRecordQueue, Slice: "/x"}},
			Next:    &runner.ConfineHistoryCursor{Kind: "confine", Signature: "a"}},
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("b", i64(20))},
			// Live state belongs to page 1 only; a later page that repeated it must not double it.
			Waiters: []runner.ConfineDumpWaiterRow{{RecordType: runner.ConfineDumpRecordWaiter, Slice: "/y"}},
			Next:    &runner.ConfineHistoryCursor{Kind: "confine", Signature: "b"}},
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("c", i64(30))}},
	}}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	exit, stdout, stderr := runDump(t, peer.dispatcher(), path)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	rows := readDump(t, path)
	var sigs []string
	counts := map[string]int{}
	for _, row := range rows {
		counts[row["record_type"].(string)]++
		if row["record_type"] == runner.ConfineDumpRecordAdmission {
			sigs = append(sigs, row["signature"].(string))
		}
	}
	if strings.Join(sigs, ",") != "a,b,c" || counts[runner.ConfineDumpRecordWaiter] != 1 || counts[runner.ConfineDumpRecordQueue] != 1 {
		t.Fatalf("joined file wrong: admissions %v, counts %v", sigs, counts)
	}
	if len(peer.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(peer.requests))
	}
	for i, request := range peer.requests {
		if request["paged"] != true {
			t.Fatalf("request %d did not opt in to paging: %v", i, request)
		}
	}
	if k, _ := peer.requests[0]["after_kind"].(string); k != "" {
		t.Fatalf("the first request carried a cursor: %v", peer.requests[0])
	}
	if peer.requests[1]["after_kind"] != "confine" || peer.requests[1]["after_signature"] != "a" || peer.requests[2]["after_signature"] != "b" {
		t.Fatalf("cursor not followed: %v / %v", peer.requests[1], peer.requests[2])
	}
}

// verifies: AIRA-280 — a cursor that does not advance is E_DAEMON_PROTOCOL, not an
// endless loop, and no file is produced.
func TestNonAdvancingCursorIsAProtocolErrorAndWritesNothing(t *testing.T) {
	stuck := runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("a", nil)},
		Next: &runner.ConfineHistoryCursor{Kind: "confine", Signature: "a"}}
	for name, second := range map[string]runner.ConfineHistoryCursor{
		"same":    {Kind: "confine", Signature: "a"},
		"earlier": {Kind: "confine", Signature: "0"},
	} {
		t.Run(name, func(t *testing.T) {
			again := runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Next: &second}
			peer := &historyPeer{t: t, pages: []any{stuck, again, again, again}}
			path := filepath.Join(t.TempDir(), "dump.jsonl")
			exit, stdout, stderr := runDump(t, peer.dispatcher(), path)
			if exit == 0 || !strings.Contains(stdout+stderr, daemon.CodeProtocol) || !strings.Contains(stdout+stderr, "did not advance") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("a dump file exists after a protocol error (err=%v)", err)
			}
		})
	}
}

// verifies: AIRA-280 — an error on a later page is the whole answer: that error,
// and no partial file.
func TestErrorOnALaterPageProducesNoFile(t *testing.T) {
	peer := &historyPeer{t: t, pages: []any{
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("a", nil)},
			Next: &runner.ConfineHistoryCursor{Kind: "confine", Signature: "a"}},
		daemon.ResponseFrame{Code: "E_DAEMON_INTERNAL", Error: "E_DAEMON_INTERNAL: read usage history: boom", Exit: 4},
	}}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	exit, stdout, stderr := runDump(t, peer.dispatcher(), path)
	if exit != 4 || !strings.Contains(stdout+stderr, "boom") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a partial dump file exists (err=%v)", err)
	}
}

// verifies: AIRA-280 — daemon down is the unchanged unevaluated answer on page 1.
func TestPagedDumpDaemonDownIsStillUnevaluated(t *testing.T) {
	d := &daemonDispatcher{}
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
	}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	exit, stdout, _ := runDump(t, d, path)
	if exit != 3 || !strings.Contains(stdout, `"verdict":"unevaluated"`) {
		t.Fatalf("exit=%d stdout=%q", exit, stdout)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a dump file exists for an unevaluated dump (err=%v)", err)
	}
}

// verifies: AIRA-280 — version skew, new client / old daemon: an old daemon
// ignores `paged` and answers everything with no cursor; that is one exchange and
// a complete result.
func TestOldDaemonThatIgnoresPagingIsOneCompleteExchange(t *testing.T) {
	peer := &historyPeer{t: t, pages: []any{
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("a", nil), dumpRow("b", nil)}},
	}}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	if exit, stdout, stderr := runDump(t, peer.dispatcher(), path); exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if len(peer.requests) != 1 || len(readDump(t, path)) != 2 {
		t.Fatalf("requests=%d rows=%d, want 1 and 2", len(peer.requests), len(readDump(t, path)))
	}
}

// verifies: AIRA-280 — an empty page without a cursor after a full page (possible
// only if history was deleted between two page reads) ends the join cleanly.
func TestEmptyFinalPageEndsTheJoinWithoutAnError(t *testing.T) {
	peer := &historyPeer{t: t, pages: []any{
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("a", nil)},
			Next: &runner.ConfineHistoryCursor{Kind: "confine", Signature: "a"}},
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s"},
	}}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	if exit, stdout, stderr := runDump(t, peer.dispatcher(), path); exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if rows := readDump(t, path); len(rows) != 1 || rows[0]["signature"] != "a" {
		t.Fatalf("rows = %v, want the earlier page's row", rows)
	}
}

// verifies: AIRA-280 — honesty through the join: an unknown measurement stays
// absent (never 0) in the written dump, a known one is unchanged.
func TestJoinedDumpKeepsUnknownMeasurementsAbsent(t *testing.T) {
	budget := dumpRow("b", nil)
	budget.DeclaredReserveBytes, budget.DeclaredReserveBasis = i64(77), "cap:operator:--memory-reserve"
	peer := &historyPeer{t: t, pages: []any{
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("a", i64(10))},
			Next: &runner.ConfineHistoryCursor{Kind: "confine", Signature: "a"}},
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{budget}},
	}}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	if exit, stdout, stderr := runDump(t, peer.dispatcher(), path); exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	rows := readDump(t, path)
	if rows[0]["observed_peak_bytes"] != float64(10) {
		t.Fatalf("known peak changed: %v", rows[0])
	}
	if _, present := rows[0]["declared_reserve_bytes"]; present {
		t.Fatalf("an unknown budget became a value: %v", rows[0])
	}
	if _, present := rows[1]["observed_peak_bytes"]; present {
		t.Fatalf("an unknown peak became a value (0?): %v", rows[1])
	}
	if rows[1]["declared_reserve_bytes"] != float64(77) {
		t.Fatalf("known budget changed: %v", rows[1])
	}
}

func budgetRow(subject, direction string, observed, budget *int64, unevaluated bool) runner.ConfineBudgetRow {
	return runner.ConfineBudgetRow{
		Kind: "confine", Subject: "confine / " + subject, Direction: direction,
		Unevaluated: unevaluated, ObservedMax: observed, Budget: budget,
	}
}

func dispatchBudget(t *testing.T, peer *historyPeer) runner.ConfineBudgetResult {
	t.Helper()
	response := peer.dispatcher().Dispatch(context.Background(), daemon.WorktreeScope{},
		core.Request{Verb: "confine-budget", Args: map[string]any{"owner": "session-a"}})
	if !response.OK {
		t.Fatalf("response = %+v", response)
	}
	data, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	var result runner.ConfineBudgetResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// verifies: AIRA-280 — joined budget pages equal the unpaged order (rows
// re-sorted worst-first across pages), and honesty survives the join: an
// unevaluated subject stays unevaluated, an absent measurement stays nil.
func TestJoinedBudgetIsResortedAndKeepsHonesty(t *testing.T) {
	peer := &historyPeer{t: t, pages: []any{
		runner.ConfineBudgetResult{Verdict: "ok", Scope: "s",
			Subjects: []runner.ConfineBudgetRow{
				budgetRow("a-unknown", "unevaluated", nil, nil, true),
				budgetRow("b-fitted", "well-fitted", i64(100), i64(120), false),
			},
			Next: &runner.ConfineHistoryCursor{Kind: "confine", Signature: "b-fitted"}},
		runner.ConfineBudgetResult{Verdict: "ok", Scope: "s",
			Subjects: []runner.ConfineBudgetRow{
				budgetRow("c-under", "under-provisioned", i64(900), i64(500), false),
				budgetRow("d-nomeasure", "unevaluated", nil, nil, true),
			}},
	}}
	result := dispatchBudget(t, peer)
	var order []string
	for _, row := range result.Subjects {
		order = append(order, strings.TrimPrefix(row.Subject, "confine / "))
	}
	if want := "c-under,b-fitted,a-unknown,d-nomeasure"; strings.Join(order, ",") != want {
		t.Fatalf("joined order = %v, want %s (worst first, ties by subject)", order, want)
	}
	for _, row := range result.Subjects {
		switch row.Subject {
		case "confine / a-unknown", "confine / d-nomeasure":
			if !row.Unevaluated || row.Direction != "unevaluated" || row.ObservedMax != nil || row.Budget != nil {
				t.Fatalf("unevaluated subject was upgraded or zeroed: %+v", row)
			}
		case "confine / c-under":
			if row.ObservedMax == nil || *row.ObservedMax != 900 || row.Budget == nil || *row.Budget != 500 {
				t.Fatalf("established values changed: %+v", row)
			}
		}
	}
	if result.Next != nil || result.Verdict != "ok" {
		t.Fatalf("joined result: verdict=%q next=%v", result.Verdict, result.Next)
	}
}

// verifies: AIRA-280 invariant 3 — an `unevaluated` answer on a LATER page is the
// whole answer, exactly as on page 1: exit 3, the unevaluated message, and no
// (silently truncated) dump file. This window is real: AIRA-203 restarts the
// daemon on every binary install, so the daemon can vanish between two pages.
func TestUnevaluatedOnALaterPageIsTheWholeAnswerAndWritesNothing(t *testing.T) {
	first, err := json.Marshal(runner.ConfineDumpResult{Verdict: "ok", Scope: "s",
		Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("a", i64(10))},
		Next:       &runner.ConfineHistoryCursor{Kind: "confine", Signature: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	d := &daemonDispatcher{}
	calls := 0
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		calls++
		if calls == 1 {
			return daemon.ResponseFrame{OK: true, Code: "OK", Data: first}, nil
		}
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": restarting")}
	}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	exit, stdout, stderr := runDump(t, d, path)
	if calls != 2 {
		t.Fatalf("exchanges = %d, want 2 (page 1 then the vanished daemon)", calls)
	}
	if exit != 3 || !strings.Contains(stdout, `"verdict":"unevaluated"`) {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want exit 3 and the unevaluated message", exit, stdout, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a partial dump file exists after an unevaluated later page (err=%v)", err)
	}
}

// verifies: AIRA-280 — the same for the budget join: an unevaluated later page
// returns the unevaluated verdict, never a one-page budget.
func TestUnevaluatedOnALaterBudgetPageIsTheWholeAnswer(t *testing.T) {
	first, err := json.Marshal(runner.ConfineBudgetResult{Verdict: "ok", Scope: "s",
		Subjects: []runner.ConfineBudgetRow{budgetRow("a", "well-fitted", i64(1), i64(2), false)},
		Next:     &runner.ConfineHistoryCursor{Kind: "confine", Signature: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	d := &daemonDispatcher{}
	calls := 0
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		calls++
		if calls == 1 {
			return daemon.ResponseFrame{OK: true, Code: "OK", Data: first}, nil
		}
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": restarting")}
	}
	response := d.Dispatch(context.Background(), daemon.WorktreeScope{},
		core.Request{Verb: "confine-budget", Args: map[string]any{"owner": "session-a"}})
	data, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	var result runner.ConfineBudgetResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "unevaluated" || len(result.Subjects) != 0 {
		t.Fatalf("verdict=%q subjects=%d, want the whole answer unevaluated (response %+v)", result.Verdict, len(result.Subjects), response)
	}
}

// verifies: AIRA-280 — the cursor advances across a KIND boundary: the history
// holds both `confine` and `pytest-worker` subjects, and a pytest-worker
// signature can sort below the last confine signature. Comparing signatures
// alone would call that crossing "did not advance" and fail every full dump.
func TestCursorAdvancesAcrossAKindBoundary(t *testing.T) {
	peer := &historyPeer{t: t, pages: []any{
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("zzz", nil)},
			Next: &runner.ConfineHistoryCursor{Kind: "confine", Signature: "zzz"}},
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s", Admissions: []runner.ConfineDumpAdmissionRow{dumpRow("aaa", nil)},
			Next: &runner.ConfineHistoryCursor{Kind: "pytest-worker", Signature: "aaa"}},
		runner.ConfineDumpResult{Verdict: "ok", Scope: "s"},
	}}
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	exit, stdout, stderr := runDump(t, peer.dispatcher(), path)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q (a kind crossing is progress)", exit, stdout, stderr)
	}
	if rows := readDump(t, path); len(rows) != 2 {
		t.Fatalf("rows = %v, want both pages", rows)
	}
}
