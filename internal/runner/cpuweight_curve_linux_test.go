package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// verifies: AIRA-208 -- the trailer's cpu-weight facet reports the schedule that
// was ARMED (start weight written, decay goroutine started), keeps the two-valued
// cpu_weight enum, and names a rejected schedule instead of dropping it silently.

func clearCPUWeightEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_START", "")
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_FLOOR", "")
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE", "")
}

func TestFormatConfineStatusCarriesTheArmedCPUWeightCurve(t *testing.T) {
	aging := FormatConfineStatus(ConfineStatus{CPUWeight: ConfineCPUWeightAging, CPUWeightCurve: "100->10@30m0s"})
	if !strings.Contains(aging, " cpu-weight=aging(100->10@30m0s)") {
		t.Fatalf("aging trailer lacks the armed curve: %q", aging)
	}
	// A record written by an older binary has no curve: never a guessed one.
	old := FormatConfineStatus(ConfineStatus{CPUWeight: ConfineCPUWeightAging})
	if !strings.Contains(old, " cpu-weight=aging(unevaluated)") {
		t.Fatalf("aging without a curve must say unevaluated: %q", old)
	}
	for _, status := range []ConfineStatus{
		{CPUWeight: ConfineCPUWeightUnavailable},
		{},
		// A curve on a non-aging status is a contradiction; it is not shown.
		{CPUWeight: ConfineCPUWeightUnavailable, CPUWeightCurve: "100->10@30m0s"},
	} {
		line := FormatConfineStatus(status)
		if !strings.Contains(line, " cpu-weight=unavailable") || strings.Contains(line, "cpu-weight=unavailable(") || strings.Contains(line, "100->10") {
			t.Fatalf("unavailable must carry no parentheses or curve: %q", line)
		}
	}
}

func TestConfineStatusJSONKeepsCPUWeightEnumAndAddsCurve(t *testing.T) {
	data, err := json.Marshal(ConfineStatus{CPUWeight: ConfineCPUWeightAging, CPUWeightCurve: "100->10@30m0s"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || fields["cpu_weight"] != "aging" || fields["cpu_weight_curve"] != "100->10@30m0s" {
		t.Fatalf("status JSON: %s (%v)", data, err)
	}
	var back ConfineStatus
	if err := json.Unmarshal(data, &back); err != nil || back.CPUWeight != ConfineCPUWeightAging || back.CPUWeightCurve != "100->10@30m0s" {
		t.Fatalf("round trip: %+v err=%v", back, err)
	}
	// Absent curve is omitted, not an empty string.
	data, _ = json.Marshal(ConfineStatus{CPUWeight: ConfineCPUWeightUnavailable})
	if strings.Contains(string(data), "cpu_weight_curve") {
		t.Fatalf("empty curve must be omitted: %s", data)
	}
}

func TestCPUWeightCurveOfTheDefaultAndOfARejectedSchedule(t *testing.T) {
	clearCPUWeightEnv(t)
	config := confineCPUWeightConfig()
	if config.ScheduleRejected || config.curve() != "100->10@30m0s" {
		t.Fatalf("default config=%+v curve=%q", config, config.curve())
	}
	defaultSteps := append([]cpuWeightStep(nil), config.Steps...)

	// A non-empty schedule that does not parse is NAMED, and the compiled
	// default still runs (never inert-by-accident).
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE", "garbage")
	rejected := confineCPUWeightConfig()
	if !rejected.ScheduleRejected {
		t.Fatalf("a schedule that does not parse was not marked rejected: %+v", rejected)
	}
	if len(rejected.Steps) != len(defaultSteps) {
		t.Fatalf("rejected schedule steps=%+v, want the clamped compiled default %+v", rejected.Steps, defaultSteps)
	}
	for i := range defaultSteps {
		if rejected.Steps[i] != defaultSteps[i] {
			t.Fatalf("rejected schedule steps=%+v, want the clamped compiled default %+v", rejected.Steps, defaultSteps)
		}
	}
	if got := rejected.curve(); got != "100->10@30m0s,schedule-rejected" {
		t.Fatalf("rejected curve=%q", got)
	}

	// An unset or blank schedule is not "rejected".
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE", "   ")
	if blank := confineCPUWeightConfig(); blank.ScheduleRejected {
		t.Fatalf("a blank schedule was marked rejected: %+v", blank)
	}
}

func TestCPUWeightCurveIsBuiltFromThePostClampSteps(t *testing.T) {
	clearCPUWeightEnv(t)
	// The schedule's own last weight (50) is overwritten by the floor (10), and
	// the curve must show what was ARMED, not what was typed.
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE", "1s:100,2s:50")
	config := confineCPUWeightConfig()
	if got := config.curve(); got != "100->10@2s" {
		t.Fatalf("curve=%q, want the armed floor overwrite 100->10@2s", got)
	}
	if config.ScheduleRejected {
		t.Fatalf("a valid schedule was marked rejected: %+v", config)
	}
}

func TestCPUWeightFloor100TurnsAgingFlatButStillArmed(t *testing.T) {
	clearCPUWeightEnv(t)
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_FLOOR", "100")
	config := confineCPUWeightConfig()
	for _, step := range config.Steps {
		if step.Weight != 100 {
			t.Fatalf("FLOOR=100 left a decaying step: %+v", config.Steps)
		}
	}
	if got := config.curve(); got != "100->100@30m0s" {
		t.Fatalf("flat curve=%q", got)
	}
}

func TestConfineTrailerCarriesCurveOnlyWhenTheFirstWriteSucceeded(t *testing.T) {
	clearCPUWeightEnv(t)
	run := func(write func(Scope, int64) bool) ConfineResult {
		t.Helper()
		scope := &confineFakeScope{}
		deps := confineUnitDeps(scope)
		deps.writeScopeCPUWeight = write
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
		}, deps)
		if err != nil || result.Exit != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		return result
	}
	armed := run(func(Scope, int64) bool { return true })
	if armed.Status.CPUWeight != ConfineCPUWeightAging || armed.Status.CPUWeightCurve != "100->10@30m0s" {
		t.Fatalf("armed status: weight=%q curve=%q", armed.Status.CPUWeight, armed.Status.CPUWeightCurve)
	}
	if line := FormatConfineStatus(armed.Status); !strings.Contains(line, " cpu-weight=aging(100->10@30m0s)") {
		t.Fatalf("armed trailer: %q", line)
	}
	failed := run(func(Scope, int64) bool { return false })
	if failed.Status.CPUWeight != ConfineCPUWeightUnavailable || failed.Status.CPUWeightCurve != "" {
		t.Fatalf("failed first write must leave unavailable and no curve: weight=%q curve=%q", failed.Status.CPUWeight, failed.Status.CPUWeightCurve)
	}
	// A rejected schedule is carried through to the trailer of an armed scope.
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE", "nonsense")
	rejected := run(func(Scope, int64) bool { return true })
	if line := FormatConfineStatus(rejected.Status); !strings.Contains(line, " cpu-weight=aging(100->10@30m0s,schedule-rejected)") {
		t.Fatalf("rejected-schedule trailer: %q", line)
	}
}
