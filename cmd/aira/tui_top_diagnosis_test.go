package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// verifies: AIRA-370 — a queued row's LIVE cell names the lane's verdict and
// then the daemon's wait diagnosis, after pushed/high, in the daemon's token
// order; "cpu slots", never a bare "cpu" that reads as load; "behind head" once
// (the diagnosis carries it, the lane reason's copy is not repeated). A row
// with no diagnosis (an older daemon) keeps the lane reason alone, and an
// unknown token names nothing. Mutations: drop the lane word beside a
// diagnosis (the "lane full, ram" rows go red); prefix topLaneReason instead
// of the lane word ("behind head, behind head"); map cpu to "cpu"; drop the
// fallback.
func TestTopQueuedRowNamesTheWaitDiagnosis(t *testing.T) {
	cases := []struct {
		name     string
		blocked  []string
		laneWait string
		held     bool
		priority string
		want     string
	}{
		{"ram", []string{"ram"}, "", false, "", "ram"},
		{"ram, ceiling cut", []string{"ram", "ceiling-cut"}, "", false, "", "ram, ceiling cut"},
		{"cpu slots", []string{"cpu"}, "", false, "", "cpu slots"},
		{"all resources", []string{"ram", "ceiling-cut", "cpu", "vram"}, "", false, "", "ram, ceiling cut, cpu slots, vram"},
		{"behind head", []string{"head"}, "", true, "", "behind head"},
		{"kept back, behind head", []string{"head"}, "kept", true, "", "kept back, behind head"},
		{"exclusive", []string{"exclusive"}, "", false, "", "exclusive"},
		{"freeze", []string{"restart-freeze"}, "", false, "", "freeze"},
		{"lane full, ram", []string{"ram"}, "full", false, "", "lane full, ram"},
		{"behind push, cpu slots", []string{"cpu"}, "pushed", false, "", "behind push, cpu slots"},
		{"high first", []string{"cpu"}, "", false, runner.ConfinePriorityHigh, "high, cpu slots"},
		{"high, lane full, ram", []string{"ram", "ceiling-cut"}, "full", false, runner.ConfinePriorityHigh, "high, lane full, ram, ceiling cut"},
		{"older daemon: lane reason", nil, "full", false, "", "lane full"},
		{"unknown token: lane reason", []string{"someday"}, "kept", false, "", "kept back"},
		{"unknown token alone", []string{"someday"}, "", false, "", "queued"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			record := topQueuedTestRecord("CONFINE-q-201-dd@session-q", "q", gib)
			record.BlockedBy, record.LaneWait, record.HeldByHead, record.Priority = testCase.blocked, testCase.laneWait, testCase.held, testCase.priority
			if got := topQueuedRow(record, false, 1).Cells[3]; got != testCase.want {
				t.Fatalf("LIVE cell=%q, want %q", got, testCase.want)
			}
		})
	}
}

// verifies: AIRA-370 — the footer says once that the box CPU is busy, only for
// an established pressure at or above runner.ConfineBoxCPUBusyPercent.
// Mutations: > for >= (40 goes red); render a nil pressure.
func TestTopFooterSaysTheBoxCPUIsBusy(t *testing.T) {
	for _, test := range []struct {
		pressure *float64
		want     string
	}{
		{nil, ""},
		{floatPointer(39.9), ""},
		{floatPointer(40), "box CPU busy 40%"},
		{floatPointer(70), "box CPU busy 70%"},
	} {
		reserve := &runner.ConfineSliceReserve{GrantedEstablished: true, Queued: 4, FreezePhase: "idle", CPUPressureSomeAvg10: test.pressure}
		footer := topFooter(runner.ConfineListResult{SliceReserve: reserve})
		got := ""
		if index := strings.Index(footer, "box CPU busy"); index >= 0 {
			got = strings.SplitN(footer[index:], " |", 2)[0]
		}
		if got != test.want || strings.Count(footer, "box CPU") > 1 {
			t.Fatalf("pressure %v: footer %q, want clause %q", test.pressure, footer, test.want)
		}
	}
}

func floatPointer(value float64) *float64 { return &value }

// verifies: AIRA-370 — `confine --list` puts the machine's CPU pressure on the
// slice line when the daemon read it (a real 0 included), and prints nothing
// for it otherwise; --json carries blocked_by on queued rows and the pressure
// and slot ledger in slice_reserve. Mutation: print the clause for a nil
// pressure (the absent case goes red).
func TestConfineListShowsTheBoxCPUPressure(t *testing.T) {
	render := func(reserve runner.ConfineSliceReserve) string {
		var stdout, stderr bytes.Buffer
		result := runner.ConfineListResult{SliceReserve: &reserve}
		if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatalf("render exit=%d stderr=%q", exit, stderr.String())
		}
		return stdout.String()
	}
	base := runner.ConfineSliceReserve{GrantedEstablished: true, GrantedBytes: 3 << 30, CeilingBytes: 52 << 30, Jobs: 5}
	if out := render(base); strings.Contains(out, "box CPU") {
		t.Fatalf("an unread pressure printed:\n%s", out)
	}
	for _, value := range []float64{0, 70} {
		reserve := base
		reserve.CPUPressureSomeAvg10 = floatPointer(value)
		out := render(reserve)
		want := "across 5 admitted jobs; box CPU pressure " + map[float64]string{0: "0%", 70: "70%"}[value] + " (some task waited for a CPU, last 10 s)\n"
		if !strings.Contains(out, want) {
			t.Fatalf("pressure %v: missing %q in\n%s", value, want, out)
		}
	}

	pressure := 70.0
	data, err := json.Marshal(runner.ConfineListResult{
		Queued:       []runner.ConfineRecord{{ScopeID: "CONFINE-q-1-a", BlockedBy: []string{"ram", "ceiling-cut"}}},
		SliceReserve: &runner.ConfineSliceReserve{CPUOutstandingCores: 32, CPUCeilingCores: 32, CPUPressureSomeAvg10: &pressure},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"blocked_by":["ram","ceiling-cut"]`, `"cpu_outstanding_cores":32`, `"cpu_ceiling_cores":32`, `"cpu_pressure_some_avg10":70`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("json lacks %s: %s", want, data)
		}
	}
	empty, _ := json.Marshal(runner.ConfineListResult{Queued: []runner.ConfineRecord{{ScopeID: "CONFINE-q-1-a"}}, SliceReserve: &runner.ConfineSliceReserve{}})
	for _, absent := range []string{"blocked_by", "cpu_outstanding_cores", "cpu_ceiling_cores", "cpu_pressure"} {
		if strings.Contains(string(empty), absent) {
			t.Fatalf("an absent %s is on the wire: %s", absent, empty)
		}
	}
}
