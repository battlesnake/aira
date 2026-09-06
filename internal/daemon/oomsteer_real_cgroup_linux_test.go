//go:build linux

package daemon

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// AIRA-113 anti-INERT tier, and the test the ticket actually asks for: a
// multi-scope contention scenario proving the steering changes WHICH scope the
// kernel would favour as its OOM victim.
//
// Every other AIRA-113 test drives evaluateOOMSteer through oomSteerDeps, so all
// of them would keep passing against a build whose production walker could never
// find a pid on a real cgroup tree, or whose production readers could never
// establish a reading — which is exactly how the AIRA-59 watchdog shipped inert.
// So this one builds a real cgroup-v2 slice with a real memory.max, puts real
// allocating processes into two real .aira-CONFINE-* scopes of DIFFERENT AIRA-27
// classes, and runs the production readers, the production ledger snapshot and
// the production /proc walker.
//
// The shape is the exact failure AIRA-113 exists for, and it is deliberately
// stacked AGAINST the fix: the compliant neighbour is the BIGGER scope. Under
// AIRA-27's static bias alone the kernel prefers it (delegate 800 outweighs
// non-delegate 500 by a fifth of machine total), so a compliant job dies for a
// neighbour's over-commit. That is asserted as the BEFORE state, from the adj
// values actually read out of /proc, so the AFTER assertion cannot be a
// tautology.
//
// verifies: evaluateOOMSteer and runner.SetSubtreeOOMScoreAdj against a real
// cgroup tree.

// steerRealScope creates <parent>/.aira-<scopeID> for a scope id of the
// requested AIRA-27 class.
func steerRealScope(t *testing.T, parent, name string, delegate bool) (scopePath, scopeID string) {
	t.Helper()
	stamp := strconv.FormatInt(time.Now().UnixNano()%(1<<40), 36)
	scopeID = "CONFINE-" + name + "-" + strconv.Itoa(os.Getpid()) + "-" + stamp
	if delegate {
		scopeID = "CONFINE-@dr-" + name + "-" + strconv.Itoa(os.Getpid()) + "-" + stamp
	}
	if runner.IsDelegateRAMScopeID(scopeID) != delegate {
		t.Fatalf("scope id %q does not carry the class this test needs (delegate=%v)", scopeID, delegate)
	}
	scopePath = filepath.Join(parent, confineScopeDirName(scopeID))
	if err := os.Mkdir(scopePath, 0o755); err != nil {
		t.Fatal(err)
	}
	return scopePath, scopeID
}

// steerRealJob starts a process, places it in the scope BEFORE it allocates
// anything, sets the AIRA-27 class baseline the confined child would have
// written to its own /proc/self/oom_score_adj at exec, then lets it allocate and
// waits until it says it has FINISHED allocating.
//
// Two things here are load-bearing rather than tidy:
//
//   - The placement must precede the allocation. cgroup-v2 charges a page to the
//     cgroup that faulted it and migrating a process does not recharge what it
//     already holds, so a process that allocated before the move would leave the
//     scope's memory.current near zero and this test would silently prove
//     nothing.
//   - The wait must be for the job's OWN "done" signal, not for memory.current
//     to cross the requested figure. A shell's command substitution holds
//     several copies of the string while it builds it, so a scope crosses the
//     requested number long before it settles — and a test that measured there
//     would compare two scopes at unrelated points on their growth curves,
//     making "which one is bigger" a race.
func steerRealJob(t *testing.T, scopePath string, allocateBytes int64, baseline int) int {
	t.Helper()
	// The allocated string stays live in the shell's own variable for the life of
	// the process, which is what keeps the pages charged to the scope. No
	// parameter expansion is performed on it: ${A%?} on a multi-megabyte string
	// takes minutes in dash, which is what made an earlier version of this
	// helper time out rather than allocate.
	script := "read go; A=$(tr '\\0' 'x' < /dev/zero | head -c " + strconv.FormatInt(allocateBytes, 10) + "); " +
		"echo ready; sleep 300"
	job := exec.Command("sh", "-c", script)
	stdin, err := job.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := job.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = job.Process.Kill()
		_, _ = job.Process.Wait()
	})
	pid := job.Process.Pid
	if err := os.WriteFile(filepath.Join(scopePath, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place a process into %s: %v", scopePath, err)
	}
	if err := os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte(strconv.Itoa(baseline)+"\n"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set the AIRA-27 class baseline on pid %d: %v", pid, err)
	}
	if _, err := io.WriteString(stdin, "go\n"); err != nil {
		t.Fatalf("cannot release the allocator: %v", err)
	}
	ready := make(chan error, 1)
	go func() {
		buffer := make([]byte, len("ready\n"))
		_, err := io.ReadFull(stdout, buffer)
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "the allocator in %s never reported ready: %v", scopePath, err)
		}
	case <-time.After(60 * time.Second):
		current, _ := readChargeCgroupInt(scopePath, "memory.current")
		cgrouptest.SkipOrFailRealCgroup(t,
			"the allocator in %s never finished (memory.current %d, wanted %d)", scopePath, current, allocateBytes)
	}
	if current, ok := readChargeCgroupInt(scopePath, "memory.current"); !ok || current < allocateBytes {
		cgrouptest.SkipOrFailRealCgroup(t,
			"scope %s holds %d bytes after allocating %d; the tree is not what this test assumes", scopePath, current, allocateBytes)
	}
	return pid
}

func steerReadAdj(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/oom_score_adj")
	if err != nil {
		t.Fatalf("read oom_score_adj for pid %d: %v", pid, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse oom_score_adj for pid %d: %v", pid, err)
	}
	return value
}

// oomBadness mirrors the kernel's oom_badness(): a task's score is its
// RSS-and-friends plus adj * totalpages / 1000, and the OOM killer picks the
// highest. Both terms are in bytes here, which is a monotone rescale of the
// kernel's page units and therefore ranks identically. It is deliberately in the
// test rather than in production: AIRA never computes this, it only biases one
// input to it, and a production copy would be an unverifiable claim about a
// kernel internal.
func oomBadness(rss int64, adj int, memTotal int64) int64 {
	return rss + int64(adj)*memTotal/1000
}

func TestRealCgroupOOMSteerFlipsTheFavouredVictimToTheOffender(t *testing.T) {
	const (
		sliceMax = 1 << 30
		// The COMPLIANT scope is deliberately the bigger of the two, so the static
		// AIRA-27 bias and the raw RSS ordering BOTH favour it and the flip cannot
		// come from anywhere but the steering.
		compliantAllocate = 48 << 20
		offenderAllocate  = 16 << 20
	)

	memTotal, ok := readMemTotal()
	if !ok {
		cgrouptest.SkipOrFailRealCgroup(t, "MemTotal is unestablished, so the kernel's own victim ranking cannot be computed")
	}

	slice := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(slice, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", slice, err)
	}
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(strconv.Itoa(sliceMax)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set memory.max on %s: %v", slice, err)
	}

	offenderPath, offenderScope := steerRealScope(t, slice, "offender", false)
	compliantPath, compliantScope := steerRealScope(t, slice, "suite", true)
	offenderPID := steerRealJob(t, offenderPath, offenderAllocate, runner.ConfineOOMScoreAdj)
	compliantPID := steerRealJob(t, compliantPath, compliantAllocate, runner.ConfineDelegateOOMScoreAdj)

	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }

	// PRODUCTION deps throughout. Only the three policy NUMBERS are moved, so a
	// test slice sized in megabytes crosses the same fullness gate a 64 GiB one
	// does; every reader, the ledger snapshot, the class policy and the /proc
	// walker are the ones the daemon runs.
	deps := realOOMSteerDeps(server)
	deps.enterPct, deps.exitPct, deps.overrunFloor = 5, 1, 4<<20

	if _, _, _, ok, reason := deps.readSliceParts(slice); !ok {
		cgrouptest.SkipOrFailRealCgroup(t, "the production slice reader cannot read %s: %s", slice, reason)
	}
	offenderRSS, ok := deps.readScopeCurrent(offenderPath)
	if !ok {
		t.Fatalf("the production scope reader cannot read %s", offenderPath)
	}
	compliantRSS, ok := deps.readScopeCurrent(compliantPath)
	if !ok {
		t.Fatalf("the production scope reader cannot read %s", compliantPath)
	}
	if compliantRSS <= offenderRSS {
		t.Fatalf("test arithmetic: the compliant scope holds %d bytes and the offender %d; this test only proves anything while the COMPLIANT one is bigger",
			compliantRSS, offenderRSS)
	}

	// The ledger, derived from what the kernel actually reports rather than from
	// a guess about how much a shell's command substitution costs: the compliant
	// scope's charge covers its usage with room to spare, the offender's is a
	// small fraction of its own — over budget by far more than the overrun floor.
	offender := &admitWaiter{
		seq: 1, state: admitGranted, accounted: true, scopeID: offenderScope,
		reserve: offenderRSS / 8, effectiveCharge: offenderRSS / 8, chargeTracked: true,
	}
	compliant := &admitWaiter{
		seq: 2, state: admitGranted, accounted: true, scopeID: compliantScope,
		reserve: compliantRSS + (64 << 20), effectiveCharge: compliantRSS + (64 << 20), chargeTracked: true,
	}
	server.admitQueues[slice] = &sliceQueue{path: slice, server: server, waiters: []*admitWaiter{offender, compliant}}

	// THE BEFORE STATE, read out of /proc rather than assumed: AIRA-27's static
	// class bias alone picks the compliant neighbour.
	beforeOffenderAdj, beforeCompliantAdj := steerReadAdj(t, offenderPID), steerReadAdj(t, compliantPID)
	if beforeOffenderAdj != runner.ConfineOOMScoreAdj || beforeCompliantAdj != runner.ConfineDelegateOOMScoreAdj {
		t.Fatalf("the class baselines did not take: offender=%d compliant=%d", beforeOffenderAdj, beforeCompliantAdj)
	}
	beforeOffender := oomBadness(offenderRSS, beforeOffenderAdj, memTotal)
	beforeCompliant := oomBadness(compliantRSS, beforeCompliantAdj, memTotal)
	if beforeOffender >= beforeCompliant {
		t.Fatalf("test arithmetic: the static bias already favoured the offender (%d vs %d), so this test could not show a flip",
			beforeOffender, beforeCompliant)
	}

	state := newOOMSteerState("")
	evaluateOOMSteer(oomSteerEnforce, &state, deps)

	afterOffenderAdj, afterCompliantAdj := steerReadAdj(t, offenderPID), steerReadAdj(t, compliantPID)
	if afterOffenderAdj != runner.ConfineMaxOOMScoreAdj {
		t.Fatalf("the offender's real process still carries oom_score_adj %d; the steering is INERT against a real tree", afterOffenderAdj)
	}
	if afterCompliantAdj != runner.ConfineDelegateOOMScoreAdj {
		t.Fatalf("the compliant neighbour was moved to %d; a uniform raise is no bias at all", afterCompliantAdj)
	}
	afterOffender := oomBadness(offenderRSS, afterOffenderAdj, memTotal)
	afterCompliant := oomBadness(compliantRSS, afterCompliantAdj, memTotal)
	if afterOffender <= afterCompliant {
		t.Fatalf("the kernel would still prefer the compliant neighbour (%d vs %d) after steering; the whole point is the flip",
			afterCompliant, afterOffender)
	}

	// RESTORE-DOWN, against the same real processes: once admission's charge
	// covers the usage, the offender goes back to its own class baseline — not to
	// the other class's, and not part-way.
	offender.effectiveCharge = offenderRSS + (64 << 20)
	evaluateOOMSteer(oomSteerEnforce, &state, deps)
	if got := steerReadAdj(t, offenderPID); got != runner.ConfineOOMScoreAdj {
		t.Fatalf("after the ledger caught up the offender carries oom_score_adj %d, want its class baseline %d", got, runner.ConfineOOMScoreAdj)
	}
	if got := steerReadAdj(t, compliantPID); got != runner.ConfineDelegateOOMScoreAdj {
		t.Fatalf("the compliant neighbour drifted to %d", got)
	}
	if len(state.applied) != 0 {
		t.Fatalf("state still records %v after every scope returned to its baseline", state.applied)
	}
}
