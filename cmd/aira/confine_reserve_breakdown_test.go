package main

import (
	"bytes"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-68. `slice reserve: ... across N admitted jobs` sits directly under a
// table of SCOPES, and the two count different things: a scope-less
// `aira confine-reserve` per-test reservation is an admitted job with no cgroup
// scope and therefore no row. Reading N against the row count is what produced a
// P0 that did not exist, so the breakdown is printed unconditionally and the
// scope-less population is named explicitly whenever it is non-empty.
func TestRenderConfineListReserveBreakdown(t *testing.T) {
	base := runner.ConfineSliceReserve{
		GrantedBytes: 48 << 30, CeilingBytes: 61 << 30, Jobs: 23, GrantedEstablished: true,
		ScopeJobs: 3, ScopeBytes: 24 << 30,
		ReservationJobs: 20, ReservationBytes: 14 << 30,
	}
	render := func(t *testing.T, reserve runner.ConfineSliceReserve) string {
		t.Helper()
		result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}, SliceReserve: &reserve}
		var stdout, stderr bytes.Buffer
		if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
		}
		return stdout.String()
	}

	t.Run("splits-the-populations", func(t *testing.T) {
		out := render(t, base)
		if !strings.Contains(out, "of which: 3 confine scopes 24G, 20 scope-less reservations 14G") {
			t.Fatalf("stdout=%q", out)
		}
		if !strings.Contains(out, "never appears in the table above") {
			t.Fatalf("scope-less population not explained; stdout=%q", out)
		}
		if strings.Contains(out, "LEDGER INCONSISTENCY") {
			t.Fatalf("consistent ledger reported an inconsistency; stdout=%q", out)
		}
	})

	t.Run("singular-forms", func(t *testing.T) {
		reserve := base
		reserve.ScopeJobs, reserve.ReservationJobs = 1, 1
		out := render(t, reserve)
		if !strings.Contains(out, "1 confine scope 24G, 1 scope-less reservation 14G") {
			t.Fatalf("stdout=%q", out)
		}
	})

	t.Run("no-reservations-still-prints-the-zero", func(t *testing.T) {
		reserve := base
		reserve.ReservationJobs, reserve.ReservationBytes = 0, 0
		out := render(t, reserve)
		if !strings.Contains(out, "0 scope-less reservations 0B") {
			t.Fatalf("a zero population must be a stated fact, not an absence; stdout=%q", out)
		}
		if strings.Contains(out, "never appears in the table above") {
			t.Fatalf("explained a population that is empty; stdout=%q", out)
		}
	})

	// (S14 removed the `vanished` line and its VanishedJobs/VanishedBytes wire
	// fields with the cgroup scan that produced them.)

	// The residual is signed and printed for jobs AND bytes independently. The
	// most plausible regression (a lost `outstanding -=` with the job decrement
	// intact) is byte-only, and formatReserveBytes floors a negative to "0B",
	// which would hide exactly half of the defect.
	t.Run("residual-is-signed-and-two-dimensional", func(t *testing.T) {
		reserve := base
		reserve.ResidualJobs, reserve.ResidualBytes = 0, -(1 << 30)
		out := render(t, reserve)
		if !strings.Contains(out, "LEDGER INCONSISTENCY: jobs +0, bytes -1073741824") {
			t.Fatalf("stdout=%q", out)
		}
		reserve.ResidualJobs, reserve.ResidualBytes = 2, 0
		if out := render(t, reserve); !strings.Contains(out, "LEDGER INCONSISTENCY: jobs +2, bytes +0") {
			t.Fatalf("stdout=%q", out)
		}
	})

	// The absent-SliceReserve case keeps its existing contract: no summary at
	// all, and therefore none of the new lines either.
	t.Run("no-reserve-prints-nothing-new", func(t *testing.T) {
		result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}}
		var stdout, stderr bytes.Buffer
		if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 {
			t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
		}
		if strings.Contains(stdout.String(), "of which") || strings.Contains(stdout.String(), "LEDGER INCONSISTENCY") {
			t.Fatalf("stdout=%q", stdout.String())
		}
	})
}
