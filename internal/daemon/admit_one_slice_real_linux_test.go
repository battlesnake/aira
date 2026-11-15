//go:build linux

package daemon

import (
	"testing"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// verifies: AIRA-228 — the one-slice guard admits every ordinary sender on a
// real box, through the PRODUCTION slice resolver (no seam). A default
// `aira confine` puts the absolute aira.slice path its own client-side
// resolution found on the wire; `aira run`, a sub-reservation outside a job and
// the plain admit verb put the bare name. Both must resolve, daemon-side, to the
// slice the daemon serves, or every launch on the box would be refused at
// cutover. Skipped (failed under AIRA_REAL_CGROUP=1) where no managed aira.slice
// exists.
func TestAdmitOneSliceGuardAdmitsTheRealDefaultSlice(t *testing.T) {
	t.Setenv("AIRA_CONFINE_SLICE", "")
	_, clientPath, err := runner.ResolveConfineManagementSlice("")
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "the client cannot resolve its default slice on this host: %v", err)
		return
	}
	served, ok, reason := resolveAdmitSlicePath(runner.DefaultConfineSlice)
	if !ok {
		cgrouptest.SkipOrFailRealCgroup(t, "the daemon cannot resolve %s on this host: %s", runner.DefaultConfineSlice, reason)
		return
	}
	for _, wire := range []string{clientPath, runner.DefaultConfineSlice} {
		t.Run(wire, func(t *testing.T) {
			if requested, ok, reason := resolveAdmitSlicePath(wire); !ok || requested != served {
				t.Fatalf("the wire slice %q resolves to %q (ok=%v %s), but the daemon serves %q", wire, requested, ok, reason, served)
			}
			server := NewServer(Paths{})
			server.stopping = make(chan struct{})
			server.admitSliceHeadroomBase = 0
			server.admitSliceHeadroomSupervisor = 0
			// Only the memory read is injected: the ledger must not depend on how
			// loaded the shared slice is right now. Resolution is the real one.
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 1 << 40, 0, true, "" }
			t.Cleanup(func() { retireAdmitQueues(server) })
			args := validAdmitArgs(1<<20, 1000)
			args["slice"] = wire
			if grant := admitGrantData(t, admitOnce(t, server, args)); grant.State != "immediate" {
				t.Fatalf("a request naming %q must be admitted: grant=%+v", wire, grant)
			}
		})
	}
}
