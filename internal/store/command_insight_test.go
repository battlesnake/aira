package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"aira/internal/domain"
)

func TestCommandLatencyGaugeUsesPairFloorsHonestFailureRateAndDrills(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	add := func(source domain.CommandKeySource, key string, status domain.CommandOutcome, code *int64, signal string, wall int64) {
		input := domain.CommandEventInput{Key: key, KeySource: source, Program: "go", Status: status, ExitCode: code, Signal: signal, WallMS: &wall}
		if _, err := s.AddCommandEvent(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 6; i++ {
		code := int64(0)
		if i == 6 {
			code = 1
		}
		add(domain.CommandKeyProgramSubcommand, "go test", domain.CommandExited, &code, "", int64(i))
	}
	add(domain.CommandKeyProgramSubcommand, "go test", domain.CommandSignalled, nil, "TERM", 7)
	add(domain.CommandKeyProgramSubcommand, "go test", domain.CommandTimeout, nil, "KILL", 8)
	for i := 0; i < 3; i++ {
		code, wall := int64(0), int64(i+1)
		add(domain.CommandKeyLabel, "go test", domain.CommandExited, &code, "", wall)
	}
	gauge, err := s.ComputeGauge("command-latency")
	if err != nil {
		t.Fatal(err)
	}
	if gauge.Kind != GaugeKindDuration || gauge.Universe.Count != 2 || gauge.Universe.Scope != "recorded aira time runs only" {
		t.Fatalf("gauge=%#v", gauge)
	}
	heuristic := gauge.Breakdown["program-subcommand / go test"]
	if heuristic.Count != 8 || heuristic.Fields["p50_ms"].Value != int64(3) || !heuristic.Fields["p95_ms"].Unevaluated || heuristic.Fields["p95_ms"].UnevaluatedReason != "n=6, need ≥20" {
		t.Fatalf("heuristic=%#v", heuristic)
	}
	failure := heuristic.Fields["failure_rate"]
	if failure.Value != float64(1)/6 || failure.Counts["exited_nonzero"] != 1 || failure.Counts["exited_total"] != 6 || heuristic.Fields["signalled"].Value != 1 || heuristic.Fields["timeout"].Value != 1 {
		t.Fatalf("failure fields=%#v", heuristic.Fields)
	}
	if heuristic.Drilldown == nil || heuristic.Drilldown.Verb != "commands ls" || heuristic.Drilldown.Query != "key-source:program-subcommand key:go test" {
		t.Fatalf("drill=%#v", heuristic.Drilldown)
	}
	label := gauge.Breakdown["label / go test"]
	if !label.Fields["p50_ms"].Unevaluated || label.Fields["p50_ms"].UnevaluatedReason != "n=3, need ≥5" || !label.Fields["p95_ms"].Unevaluated {
		t.Fatalf("label=%#v", label)
	}
	if got := fmt.Sprint(gauge.Universe.AsOf["command_at_seq"]); got != "11" {
		t.Fatalf("watermark=%s", got)
	}
}

func TestCommandLatencyGaugeEmptyIsUnevaluatedNeverZero(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	gauge, err := s.ComputeGauge("command-latency")
	if err != nil {
		t.Fatal(err)
	}
	if !gauge.Unevaluated || gauge.Value != nil || gauge.Universe.Count != 0 {
		t.Fatalf("empty gauge fabricated zero: %#v", gauge)
	}
}
