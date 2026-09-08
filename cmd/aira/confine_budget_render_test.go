package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// TestRenderConfineBudgetNeverFabricatesAndNamesEveryUnevaluatedReason pins
// Face 2's human render (final build-review, Fable 2026-09-09 — it had none).
// Three honesty terms: an absent budget or peak prints `unevaluated`, never a
// number; a recommendation is a separate indented line ending in the classifier's
// NOT-applied clause, never a column value; and an unevaluated subject's REASON
// reaches the reader — it did not, before this test, on anything but --json.
//
// verifies: AIRA-180
func TestRenderConfineBudgetNeverFabricatesAndNamesEveryUnevaluatedReason(t *testing.T) {
	budget, peak := int64(8<<30), int64(100<<20)
	result := runner.ConfineBudgetResult{Verdict: "ok", Scope: store.ResourceBudgetUniverseScope(), Subjects: []runner.ConfineBudgetRow{
		{
			Kind: "confine", Subject: "confine / make test", Direction: store.ResourceBudgetOverProvisioned,
			Budget: &budget, BudgetBasis: "cap:operator:--memory-reserve", ObservedMax: &peak,
			UsableSamples: 3, TotalSamples: 3,
			Recommendation: "confine / make test: granted 8G, observed max 100M — over-provisioned; consider --memory-reserve 115M. NOT applied — nothing was changed.",
		},
		{
			Kind: "confine", Subject: "confine / go build", Direction: store.ResourceBudgetUnevaluated,
			Unevaluated: true, UnevaluatedReason: "fallback:insufficient-samples:n=2",
			UsableSamples: 2, TotalSamples: 2,
		},
	}}
	for name, response := range map[string]core.Response{
		"typed data": {OK: true, Code: "OK", Data: result},
		"raw frame":  {OK: true, Code: "OK", RawData: mustJSON(t, result)},
	} {
		var stdout, stderr bytes.Buffer
		if exit := renderConfineBudgetResponse(response, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatalf("%s: exit=%d stderr=%q", name, exit, stderr.String())
		}
		out := stdout.String()
		if !strings.HasPrefix(out, "universe: ") || !strings.Contains(out, "cross-project") {
			t.Fatalf("%s: the universe must be disclosed before any row:\n%s", name, out)
		}
		var goBuild, makeTest string
		for _, line := range strings.Split(out, "\n") {
			switch {
			case strings.HasSuffix(line, "confine / go build"):
				goBuild = line
			case strings.HasSuffix(line, "confine / make test"):
				makeTest = line
			}
		}
		if goBuild == "" || makeTest == "" {
			t.Fatalf("%s: both subjects must render as rows:\n%s", name, out)
		}
		// DIRECTION, GRANTED and OBSERVED-MAX all read unevaluated for the row
		// with nothing established — three, not one, and no zero anywhere.
		if strings.Count(goBuild, "unevaluated") != 3 || strings.Contains(goBuild, " 0 ") || strings.Contains(goBuild, "unknown") {
			t.Fatalf("%s: an absent term must render as unevaluated, never 0/unknown: %q", name, goBuild)
		}
		if !strings.Contains(makeTest, "8G") || !strings.Contains(makeTest, "100M") || !strings.Contains(makeTest, "3/3") {
			t.Fatalf("%s: established terms must render as themselves: %q", name, makeTest)
		}
		if strings.Contains(makeTest, "consider") {
			t.Fatalf("%s: a recommendation must never appear as a column value: %q", name, makeTest)
		}
		if !strings.Contains(out, "\n  confine / make test: granted 8G") || !strings.Contains(out, "NOT applied — nothing was changed.") {
			t.Fatalf("%s: the recommendation must be its own indented line ending in the NOT-applied clause:\n%s", name, out)
		}
		if !strings.Contains(out, "\n  confine / go build: unevaluated — fallback:insufficient-samples:n=2") {
			t.Fatalf("%s: an unevaluated subject's reason must reach the reader:\n%s", name, out)
		}
	}
}

// TestRenderConfineBudgetEmptyHistoryIsAnAnswer: zero subjects is a true
// statement about a machine, printed as one, with the universe still disclosed.
//
// verifies: AIRA-180
func TestRenderConfineBudgetEmptyHistoryIsAnAnswer(t *testing.T) {
	var stdout, stderr bytes.Buffer
	response := core.Response{OK: true, Code: "OK", Data: runner.ConfineBudgetResult{Verdict: "ok", Scope: store.ResourceBudgetUniverseScope()}}
	if exit := renderConfineBudgetResponse(response, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "universe: ") || !strings.Contains(stdout.String(), "no usage history recorded yet") {
		t.Fatalf("out=%q", stdout.String())
	}
}

// TestRenderConfineBudgetRefusesAnUnparseableFrame: a frame that is not a
// budget result is a protocol error, not a silently empty report.
//
// verifies: AIRA-180
func TestRenderConfineBudgetRefusesAnUnparseableFrame(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := renderConfineBudgetResponse(core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"subjects":"not-a-list"}`)}, &stdout, &stderr)
	if exit == 0 || !strings.Contains(stderr.String(), "invalid confine-budget response") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
