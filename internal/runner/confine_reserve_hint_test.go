package runner

import "testing"

const hintMiB = int64(1) << 20
const hintGiB = int64(1) << 30

func hintPeak(v int64) *int64 { return &v }

// hintStatus is the eligible baseline: a real (enforced) run that ended normally,
// charged exactly the declared 4 GiB, peaking at 310 MiB.
func hintStatus() ConfineStatus {
	return ConfineStatus{
		Containment:  ConfineContainmentEnforced,
		TerminatedBy: ConfineTerminatedNormal,
		ReserveBytes: 4 * hintGiB,
		PeakRSS:      hintPeak(310 * hintMiB),
	}
}

// verifies: AIRA-310 -- the eligible baseline prints the owner's exact line shape.
func TestConfineReserveHintBaselineText(t *testing.T) {
	got := formatConfineReserveHint(hintStatus(), 0, 4*hintGiB, false)
	want := "confine: hint: peak 310M of 4096M reserved (8%); consider --memory-reserve 512M so more jobs fit"
	if got != want {
		t.Fatalf("hint = %q\nwant  %q", got, want)
	}
}

// verifies: AIRA-310 -- every gate, each case flipping exactly one input of the
// eligible baseline.
func TestConfineReserveHintGates(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*ConfineStatus, *int, *int64, *bool)
		wantHint bool
	}{
		{"baseline", func(*ConfineStatus, *int, *int64, *bool) {}, true},
		{"peak exactly 60% never hints", func(s *ConfineStatus, _ *int, d *int64, _ *bool) {
			s.ReserveBytes, *d, s.PeakRSS = 5*hintGiB, 5*hintGiB, hintPeak(3*hintGiB)
		}, false},
		{"peak one byte under 60% hints", func(s *ConfineStatus, _ *int, d *int64, _ *bool) {
			s.ReserveBytes, *d, s.PeakRSS = 5*hintGiB, 5*hintGiB, hintPeak(3*hintGiB-1)
		}, true},
		{"reserve exactly 2 GiB is eligible", func(s *ConfineStatus, _ *int, d *int64, _ *bool) {
			s.ReserveBytes, *d = 2*hintGiB, 2*hintGiB
		}, true},
		{"reserve one byte under 2 GiB never hints", func(s *ConfineStatus, _ *int, d *int64, _ *bool) {
			s.ReserveBytes, *d = 2*hintGiB-1, 2*hintGiB-1
		}, false},
		{"unpinned (no declared reserve) never hints", func(_ *ConfineStatus, _ *int, d *int64, _ *bool) {
			*d = 0
		}, false},
		{"charged reserve differs from the declared one never hints", func(s *ConfineStatus, _ *int, d *int64, _ *bool) {
			// --memory-max 8G --memory-reserve 4G: the ledger charges 8G, so
			// --memory-reserve is not the knob in force.
			s.ReserveBytes, *d = 8*hintGiB, 4*hintGiB
		}, false},
		{"non-zero exit never hints", func(_ *ConfineStatus, e *int, _ *int64, _ *bool) {
			*e = 1
		}, false},
		{"OOM never hints", func(s *ConfineStatus, _ *int, _ *int64, _ *bool) {
			s.TerminatedBy = ConfineTerminatedOOM
		}, false},
		{"supervisor signal never hints", func(s *ConfineStatus, _ *int, _ *int64, _ *bool) {
			s.TerminatedBy = ConfineTerminatedSupervisorSignalPrefix + "SIGTERM"
		}, false},
		{"unevaluated termination never hints", func(s *ConfineStatus, _ *int, _ *int64, _ *bool) {
			s.TerminatedBy = ""
		}, false},
		{"nil peak never hints", func(s *ConfineStatus, _ *int, _ *int64, _ *bool) {
			s.PeakRSS = nil
		}, false},
		{"ci-shim never hints, even with a peak", func(s *ConfineStatus, _ *int, _ *int64, _ *bool) {
			s.Containment = ConfineContainmentAdvisory
		}, false},
		{"unevaluated containment never hints", func(s *ConfineStatus, _ *int, _ *int64, _ *bool) {
			s.Containment = ""
		}, false},
		{"opt-out never hints", func(_ *ConfineStatus, _ *int, _ *int64, o *bool) {
			*o = true
		}, false},
		{"suggestion not below the reserve never hints", func(s *ConfineStatus, _ *int, d *int64, _ *bool) {
			// 59.96% of 2 GiB: 1.5x = 1842M, rounded up to 2048M == the reserve.
			s.ReserveBytes, *d, s.PeakRSS = 2*hintGiB, 2*hintGiB, hintPeak(1228*hintMiB)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, exit, declared, optOut := hintStatus(), 0, int64(4*hintGiB), false
			tc.mutate(&status, &exit, &declared, &optOut)
			got := formatConfineReserveHint(status, exit, declared, optOut)
			if (got != "") != tc.wantHint {
				t.Fatalf("hint = %q, want hint=%v", got, tc.wantHint)
			}
		})
	}
}

// verifies: AIRA-310 -- the suggestion is peak + 50% rounded UP to a 256 MiB
// multiple, never below 256M.
func TestConfineReserveHintSuggestionRounding(t *testing.T) {
	cases := []struct {
		peak int64
		want int64
	}{
		{1, 256 * hintMiB},                // floor
		{100 * hintMiB, 256 * hintMiB},    // 150M -> floor 256M
		{310 * hintMiB, 512 * hintMiB},    // 465M -> 512M
		{512 * hintMiB, 768 * hintMiB},    // 768M exactly, no extra step
		{512*hintMiB + 1, 1024 * hintMiB}, // just over -> next multiple
		{1024 * hintMiB, 1536 * hintMiB},  // exact
		{2000 * hintMiB, 3072 * hintMiB},  // 3000M -> 3072M
	}
	for _, tc := range cases {
		if got := confineReserveHintSuggestion(tc.peak); got != tc.want {
			t.Errorf("suggestion(%d) = %d, want %d", tc.peak, got, tc.want)
		}
	}
}
