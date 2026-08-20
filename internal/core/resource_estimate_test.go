package core

import (
	"strings"
	"testing"

	"aira/internal/runner"
)

func TestResourceSignatureMatchesRunnerEffectiveArgv(t *testing.T) {
	tests := []struct {
		name          string
		commandPrefix []string
		reqPrefix     []string
		argv          []string
	}{
		{name: "all packages", argv: []string{"go", "test", "./..."}},
		{name: "store package", argv: []string{"go", "test", "./internal/store"}},
		{name: "configured valgrind", commandPrefix: []string{"valgrind"}, argv: []string{"go", "test", "./..."}},
		{name: "requested timeout", reqPrefix: []string{"timeout", "600"}, argv: []string{"go", "test", "./..."}},
		{name: "empty request suppresses configured", commandPrefix: []string{"valgrind"}, reqPrefix: []string{}, argv: []string{"go", "test", "./..."}},
	}
	signatures := make(map[string]string)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resourceSignature(test.commandPrefix, test.reqPrefix, test.argv)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := runner.EffectivePrefix(test.commandPrefix, test.reqPrefix)
			if err != nil {
				t.Fatal(err)
			}
			effective, err := runner.EffectiveArgv(selected, test.argv)
			if err != nil {
				t.Fatal(err)
			}
			want := nulJoin(effective)
			if got != want {
				t.Fatalf("signature=%q want %q from effective argv %q", got, want, effective)
			}
			signatures[test.name] = got
		})
	}
	for _, pair := range [][2]string{{"all packages", "store package"}, {"all packages", "configured valgrind"}, {"all packages", "requested timeout"}, {"configured valgrind", "empty request suppresses configured"}} {
		if signatures[pair[0]] == signatures[pair[1]] {
			t.Fatalf("%q and %q signatures unexpectedly match: %q", pair[0], pair[1], signatures[pair[0]])
		}
	}
	if signatures["all packages"] != signatures["empty request suppresses configured"] {
		t.Fatalf("explicit empty request did not suppress configured prefix: plain=%q suppressed=%q", signatures["all packages"], signatures["empty request suppresses configured"])
	}
}

func TestResourceSignatureRejectsInvalidLaunchTopology(t *testing.T) {
	for _, test := range []struct {
		name          string
		commandPrefix []string
		argv          []string
		code          string
	}{
		{name: "empty argv", argv: nil, code: "E_RUN_ARGUMENT_INVALID"},
		{name: "malformed prefix", commandPrefix: []string{"timeout", "--", "600"}, argv: []string{"true"}, code: "E_RUN_PREFIX_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resourceSignature(test.commandPrefix, nil, test.argv)
			if got != "" || err == nil || !containsErrorCode(err, test.code) {
				t.Fatalf("resourceSignature()=%q, %v; want empty/%s", got, err, test.code)
			}
		})
	}
}

func TestEstimateReserveAllBranches(t *testing.T) {
	tests := []struct {
		name     string
		stats    runner.PeakRSSStats
		headroom int64
		reserve  int64
		override bool
		basis    string
	}{
		{name: "no history", stats: runner.PeakRSSStats{}, headroom: 100, basis: "fallback:no-history"},
		{name: "capture unavailable", stats: runner.PeakRSSStats{TotalCount: 4}, headroom: 100, basis: "fallback:capture-unavailable"},
		{name: "insufficient samples", stats: runner.PeakRSSStats{TotalCount: 4, SampleCount: 2, PeakMax: 100}, headroom: 100, basis: "fallback:insufficient-samples:n=2"},
		{name: "malformed zero peak", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3}, headroom: 100, basis: "fallback:malformed"},
		{name: "malformed negative peak", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: -1}, headroom: 100, basis: "fallback:malformed"},
		{name: "ordinary exact", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 100}, headroom: 500, reserve: 115, override: true, basis: "estimate:max=100,n=3,f=115"},
		{name: "integer rounding", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 101}, headroom: 500, reserve: 116, override: true, basis: "estimate:max=101,n=3,f=115"},
		{name: "peak exactly cap", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: maxEstimateReserve}, headroom: 1, reserve: maxEstimateReserve, override: true, basis: "estimate:capped"},
		{name: "peak above cap", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: maxEstimateReserve + 1}, headroom: 1, reserve: maxEstimateReserve, override: true, basis: "estimate:capped"},
		{name: "growth crosses cap", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: maxEstimateReserve - 1}, headroom: 1, reserve: maxEstimateReserve, override: true, basis: "estimate:capped"},
		{name: "oom floors at headroom", stats: runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 100, OOMCount: 1}, headroom: 500, reserve: 500, override: true, basis: "estimate:oom:max=100,n=4,oom=1,f=115"},
		{name: "oom estimate above headroom", stats: runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 1000, OOMCount: 2}, headroom: 500, reserve: 1150, override: true, basis: "estimate:oom:max=1000,n=4,oom=2,f=115"},
		{name: "oom headroom exactly cap", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 1, OOMCount: 1}, headroom: maxEstimateReserve, reserve: maxEstimateReserve, override: true, basis: "estimate:oom:max=1,n=3,oom=1,f=115"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reserve, override, basis := estimateReserve(test.stats, test.headroom)
			if reserve != test.reserve || override != test.override || basis != test.basis {
				t.Fatalf("estimateReserve(%+v,%d)=(%d,%v,%q), want (%d,%v,%q)", test.stats, test.headroom, reserve, override, basis, test.reserve, test.override, test.basis)
			}
			if override && (reserve <= 0 || reserve > maxEstimateReserve) {
				t.Fatalf("override reserve outside defended range: %d", reserve)
			}
		})
	}
}

func containsErrorCode(err error, code string) bool {
	return err != nil && strings.HasPrefix(err.Error(), code)
}
