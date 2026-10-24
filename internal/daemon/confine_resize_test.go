package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// AIRA-312. `aira confine --resize <selector> --memory-reserve SZ`: lower the
// admission-ledger reserve of one RUNNING job. The evaluator tests drive passes
// directly on hand-built queues (no evaluator goroutine), and the job's live
// usage comes from the confineScopeUsage seam, so every decision is exact.

// runningJob is a granted, accounted, anchored confine job with a decodable
// scope id whose embedded owner is owner ("" for none).
func runningJob(seq int64, name, owner string, reserve int64) *admitWaiter {
	job := frontWaiter(seq, name, owner)
	job.state, job.accounted, job.reserve, job.cpu, job.vram = admitGranted, true, reserve, 1, 0
	job.grantedCh = closedCh()
	job.anchor = testAnchorConn()
	return job
}

// usageSeam installs a confineScopeUsage seam answering from usage, keyed by
// scope id under the "/slice" fixture path. A scope id absent from usage reads
// as unreadable, with the reason the default reader would give a missing scope.
func usageSeam(server *Server, usage map[string]int64) {
	server.confineScopeUsage = func(dir string) (int64, bool, string) {
		for scopeID, value := range usage {
			if dir == filepath.Join("/slice", confineScopeDirName(scopeID)) {
				return value, true, ""
			}
		}
		return 0, false, "the job's scope cgroup is not present (still launching, or already gone)"
	}
}

// ledgerFingerprint is every field a resize could touch, for every waiter, plus
// the queue's four derived totals and its sequence: "byte-identical" for a
// refusal means this string does not change.
func ledgerFingerprint(queue *sliceQueue) string {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	var out strings.Builder
	fmt.Fprintf(&out, "out=%d cpu=%d vram=%d jobs=%d seq=%d|", queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs, queue.seq)
	for _, waiter := range queue.waiters {
		fmt.Fprintf(&out, "%s:%d:%d:%d:%d:%v:%d:%d;", waiter.scopeID, waiter.state, waiter.reserve, waiter.cpu, waiter.vram, waiter.accounted, waiter.pushSeq, waiter.bypassed)
	}
	return out.String()
}

func errorCode(err error) string {
	if err == nil {
		return "OK"
	}
	return store.ErrorCode(err)
}

func drainKick(queue *sliceQueue) bool {
	select {
	case <-queue.kick:
		return true
	default:
		return false
	}
}

// verifies: AIRA-312 test 1 — the ledger arithmetic and the wake-up. A 48 GiB
// job using 2 GiB leaves a 24 GiB waiter no room on a 64 GiB slice; resizing the
// job to 8 GiB re-derives outstanding to Σ reserve with the new figure, kicks
// the evaluator, and the next pass GRANTS the waiter (the wake-up is real, not
// just a number change). The listing and the OOM-steering budget read the new
// reserve too: the honesty caveat that lowering the reserve lowers the job's
// OOM-steering budget is pinned here.
func TestConfineResizeLowersTheLedgerAndAdmitsAWaiterThatNowFits(t *testing.T) {
	now := time.Unix(60000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	big := runningJob(1, "big", "s-1", 48*qGiB)
	other := runningJob(2, "other", "s-1", 4*qGiB)
	waiting := queuedWaiter(3, 24*qGiB, now)
	queue := queueOf(server, big, other, waiting)
	registerAdmitQueue(server, queue)
	usageSeam(server, map[string]int64{big.scopeID: 2 * qGiB, other.scopeID: qGiB})

	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, waiting) // room 12 GiB: it does not fit
	drainKick(queue)

	result, usage, err := server.resizeRunningConfine("/slice", "big", "8G", "s-1", false)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if result.Status != "resized" || result.ScopeID != big.scopeID || result.Name != "big" || result.Owner != "s-1" ||
		result.OldReserveBytes != 48*qGiB || result.NewReserveBytes != 8*qGiB || usage != 2*qGiB {
		t.Fatalf("result=%+v usage=%d, want big 48G -> 8G at a 2G floor", result, usage)
	}
	queue.mu.Lock()
	outstanding, cpu, vram, jobs := queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs
	wantOut, wantCPU, wantVRAM, wantJobs := rederiveLedgerLocked(queue)
	queue.mu.Unlock()
	if outstanding != 12*qGiB || outstanding != wantOut || cpu != wantCPU || vram != wantVRAM || jobs != wantJobs || jobs != 2 {
		t.Fatalf("ledger out=%d cpu=%d vram=%d jobs=%d, want Σreserve 12 GiB over 2 jobs (rederived %d/%d/%d/%d)",
			outstanding, cpu, vram, jobs, wantOut, wantCPU, wantVRAM, wantJobs)
	}
	if !drainKick(queue) {
		t.Fatal("a resize that freed capacity did not kick the evaluator")
	}
	if got := server.admitSliceSnapshot("/slice").scopeReserves[big.scopeID]; got != 8*qGiB {
		t.Fatalf("listing reserve=%d, want the new 8 GiB", got)
	}
	if got := server.admitScopeBudgets("/slice")[big.scopeID]; got != 8*qGiB {
		t.Fatalf("OOM-steering budget=%d, want it lowered to the new 8 GiB reserve", got)
	}

	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, waiting)
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.outstanding != 36*qGiB || queue.outstandingJobs != 3 {
		t.Fatalf("after the grant outstanding=%d jobs=%d, want 36 GiB over 3 jobs", queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: AIRA-312 — the floor is inclusive and a measured zero is a
// reading: a resize to exactly the live usage is accepted, and a job whose
// memory.current reads 0 can be resized to the 1 MiB minimum.
func TestConfineResizeFloorIsInclusiveAndAcceptsAMeasuredZero(t *testing.T) {
	now := time.Unix(60100, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	exact := runningJob(1, "exact", "s-1", 8*qGiB)
	idle := runningJob(2, "idle", "s-1", 8*qGiB)
	queue := queueOf(server, exact, idle)
	registerAdmitQueue(server, queue)
	usageSeam(server, map[string]int64{exact.scopeID: 3 * qGiB, idle.scopeID: 0})

	if _, _, err := server.resizeRunningConfine("/slice", "exact", strconv.FormatInt(3*qGiB, 10), "s-1", false); err != nil {
		t.Fatalf("resize to exactly the live usage refused: %v", err)
	}
	if _, usage, err := server.resizeRunningConfine("/slice", "idle", "1M", "s-1", false); err != nil || usage != 0 {
		t.Fatalf("resize of a job reading 0 usage: usage=%d err=%v, want accepted", usage, err)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if exact.reserve != 3*qGiB || idle.reserve != 1<<20 || queue.outstanding != 3*qGiB+1<<20 {
		t.Fatalf("reserves %d/%d outstanding=%d", exact.reserve, idle.reserve, queue.outstanding)
	}
}

// verifies: AIRA-312 test 2 — every refusal leaves the ledger byte-identical
// (every waiter's charge fields, the four derived totals and the sequence),
// does not kick the evaluator, and answers its own stable code.
func TestConfineResizeEveryRefusalLeavesTheLedgerByteIdentical(t *testing.T) {
	now := time.Unix(60200, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	mine := runningJob(1, "mine", "s-1", 8*qGiB)
	theirs := runningJob(2, "theirs", "s-2", 8*qGiB)
	twinA, twinB := runningJob(3, "twin", "s-1", 2*qGiB), runningJob(4, "twin", "s-1", 2*qGiB)
	waiting := frontWaiter(5, "waiting", "s-1")
	sub := runningJob(6, "aitest-w1", "s-1", qGiB)
	sub.parentScopeID = mine.scopeID
	looseWorker := runningJob(7, "aitest-w2", "s-1", qGiB) // worker-named, not a sub-reservation
	blind := runningJob(8, "blind", "s-1", 8*qGiB)
	gone := runningJob(9, "gone", "s-1", 4*qGiB)
	// A sub-reservation that is NOT worker-named, so the sub-reservation rule is
	// the only thing that refuses it.
	child := runningJob(10, "child", "s-1", qGiB)
	child.parentScopeID = mine.scopeID
	queue := queueOf(server, mine, theirs, twinA, twinB, waiting, sub, looseWorker, blind, gone, child)
	registerAdmitQueue(server, queue)
	usageSeam(server, map[string]int64{
		mine.scopeID: 2 * qGiB, theirs.scopeID: qGiB, twinA.scopeID: 0, twinB.scopeID: 0,
		sub.scopeID: 0, looseWorker.scopeID: 0, gone.scopeID: 0, child.scopeID: 0,
	})
	server.releaseAdmitWaiter(queue, gone) // a released job: gone from the ledger
	drainKick(queue)

	for _, test := range []struct {
		name, selector, reserve, caller string
		steal                           bool
		want                            string
	}{
		{name: "not found", selector: "nobody", reserve: "1G", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "ambiguous", selector: "twin", reserve: "1G", caller: "s-1", want: "E_SELECTOR_AMBIGUOUS"},
		{name: "queued job", selector: "waiting", reserve: "1G", caller: "s-1", want: runner.CodeConfineNotRunning},
		{name: "released job", selector: "gone", reserve: "1G", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "worker sub-reservation by scope id", selector: sub.scopeID, reserve: "1M", caller: "s-1", steal: true, want: runner.CodeConfineNotRunning},
		{name: "sub-reservation by scope id", selector: child.scopeID, reserve: "1M", caller: "s-1", steal: true, want: runner.CodeConfineNotRunning},
		{name: "sub-reservation by name", selector: "child", reserve: "1M", caller: "s-1", steal: true, want: runner.CodeConfineNotFound},
		{name: "worker by scope id", selector: looseWorker.scopeID, reserve: "1M", caller: "s-1", steal: true, want: runner.CodeConfineNotRunning},
		{name: "worker by name", selector: "aitest-w2", reserve: "1M", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "worker by pid", selector: "6007", reserve: "1M", caller: "s-1", want: runner.CodeConfineNotFound},
		{name: "increase", selector: "mine", reserve: "9G", caller: "s-1", want: runner.CodeConfineResizeNotReduction},
		{name: "equal", selector: "mine", reserve: "8G", caller: "s-1", want: runner.CodeConfineResizeNotReduction},
		{name: "zero", selector: "mine", reserve: "0", caller: "s-1", want: "E_CONFINE_ARGUMENT_INVALID"},
		{name: "below the 1MiB minimum", selector: "mine", reserve: "1023K", caller: "s-1", want: "E_CONFINE_ARGUMENT_INVALID"},
		{name: "malformed size", selector: "mine", reserve: "lots", caller: "s-1", want: "E_CONFINE_ARGUMENT_INVALID"},
		{name: "empty selector", selector: " ", reserve: "1G", caller: "s-1", want: "E_CONFINE_ARGUMENT_INVALID"},
		{name: "below live usage", selector: "mine", reserve: "1G", caller: "s-1", want: runner.CodeConfineResizeBelowUsage},
		{name: "one byte below live usage", selector: "mine", reserve: strconv.FormatInt(2*qGiB-1, 10), caller: "s-1", want: runner.CodeConfineResizeBelowUsage},
		{name: "usage unreadable", selector: "blind", reserve: "1G", caller: "s-1", want: runner.CodeConfineResizeUsageUnevaluated},
		{name: "foreign owner without steal", selector: "theirs", reserve: "2G", caller: "s-1", want: runner.CodeConfineOwnerUnverified},
		{name: "inferred caller on own job", selector: "mine", reserve: "4G", caller: "@cwd-x", want: runner.CodeConfineOwnerUnverified},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := ledgerFingerprint(queue)
			result, _, err := server.resizeRunningConfine("/slice", test.selector, test.reserve, test.caller, test.steal)
			if got := errorCode(err); got != test.want {
				t.Fatalf("code=%s (err=%v result=%+v), want %s", got, err, result, test.want)
			}
			if after := ledgerFingerprint(queue); after != before {
				t.Fatalf("a refused resize changed the ledger:\nbefore %s\nafter  %s", before, after)
			}
			if drainKick(queue) {
				t.Fatal("a refused resize kicked the evaluator")
			}
		})
	}
	_, _, err := server.resizeRunningConfine("/slice", "twin", "1G", "s-1", false)
	if err == nil || !strings.Contains(err.Error(), twinA.scopeID+", "+twinB.scopeID) {
		t.Fatalf("ambiguity refusal %v does not list the sorted scope ids", err)
	}
	_, _, err = server.resizeRunningConfine("/slice", "waiting", "1G", "s-1", false)
	if err == nil || !strings.Contains(err.Error(), "--front") {
		t.Fatalf("not-running refusal %v does not point a queued job at --front", err)
	}
}

// verifies: AIRA-312 test 2 — with no admission queue on the slice at all (a
// fresh daemon, or nothing connection-held) a resize names nothing.
func TestConfineResizeWithNoQueueIsNotFound(t *testing.T) {
	now := time.Unix(60300, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	if _, _, err := server.resizeRunningConfine("/slice", "job", "1G", "s-1", true); errorCode(err) != runner.CodeConfineNotFound {
		t.Fatalf("err=%v, want %s", err, runner.CodeConfineNotFound)
	}
}

// verifies: AIRA-312 test 3 — ownership is the shared CheckConfineOwner guard,
// one test per outcome: the caller's own attested job passes, another session's
// job is refused without --steal, and passes with it.
func TestConfineResizeOwnership(t *testing.T) {
	for _, test := range []struct {
		name, caller string
		steal        bool
		want         string
	}{
		{name: "own attested job", caller: "s-1", want: "OK"},
		{name: "foreign without steal", caller: "s-2", want: runner.CodeConfineOwnerUnverified},
		{name: "foreign with steal", caller: "s-2", steal: true, want: "OK"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(60400, 0)
			server := queueOrderServer(t, 64*qGiB, &now)
			job := runningJob(1, "job", "s-1", 8*qGiB)
			queue := queueOf(server, job)
			registerAdmitQueue(server, queue)
			usageSeam(server, map[string]int64{job.scopeID: qGiB})
			_, _, err := server.resizeRunningConfine("/slice", "job", "2G", test.caller, test.steal)
			if got := errorCode(err); got != test.want {
				t.Fatalf("code=%s (err=%v), want %s", got, err, test.want)
			}
			wantReserve := 8 * qGiB
			if test.want == "OK" {
				wantReserve = 2 * qGiB
			}
			if job.reserve != wantReserve || queue.outstanding != wantReserve {
				t.Fatalf("reserve=%d outstanding=%d, want %d", job.reserve, queue.outstanding, wantReserve)
			}
		})
	}
}

// verifies: AIRA-312 test 2 (ci-shim) — shim mode has no per-job cgroup, so the
// floor is unevaluated and the resize is refused, ledger untouched.
func TestConfineResizeIsRefusedInShimMode(t *testing.T) {
	now := time.Unix(60500, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	server.confineMode = runner.ConfineModeShim
	job := runningJob(1, "job", "s-1", 8*qGiB)
	queue := queueOf(server, job)
	queue.path = runner.ShimConfineSlice
	registerAdmitQueue(server, queue)
	before := ledgerFingerprint(queue)
	_, _, err := server.resizeRunningConfine(runner.ShimConfineSlice, "job", "2G", "s-1", false)
	if errorCode(err) != runner.CodeConfineResizeUsageUnevaluated || !strings.Contains(err.Error(), "ci-shim") {
		t.Fatalf("shim resize err=%v, want %s naming ci-shim", err, runner.CodeConfineResizeUsageUnevaluated)
	}
	if ledgerFingerprint(queue) != before {
		t.Fatal("a refused shim resize changed the ledger")
	}
}

// verifies: AIRA-312 — the PRODUCTION floor reader is wired. Every other resize
// test injects the confineScopeUsage seam, so a slip that made the default reader
// refuse every resize as "ci-shim" (or read the wrong directory) would leave the
// suite green. Here the seam is nil, the mode is real, and the usage is a real
// memory.current file under <slice>/.aira-<scope-id>: a resize at/above it
// succeeds, a malformed file is refused as unevaluated, and a size below it is
// refused as below-usage.
func TestConfineResizeUsesTheRealScopeReaderInRealMode(t *testing.T) {
	now := time.Unix(60600, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	if server.confineScopeUsage != nil || server.shimMode() {
		t.Fatalf("fixture must be real mode with no usage seam (seam=%v shim=%v)", server.confineScopeUsage != nil, server.shimMode())
	}
	job := runningJob(1, "job", "s-1", 8*qGiB)
	queue := queueOf(server, job)
	slice := t.TempDir()
	queue.path = slice
	registerAdmitQueue(server, queue)
	scopeDir := filepath.Join(slice, confineScopeDirName(job.scopeID))
	if err := os.MkdirAll(scopeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(scopeDir, "memory.current")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(current, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("garbage\n")
	before := ledgerFingerprint(queue)
	if _, _, err := server.resizeRunningConfine(slice, "job", "2G", "s-1", false); errorCode(err) != runner.CodeConfineResizeUsageUnevaluated || strings.Contains(err.Error(), "ci-shim") {
		t.Fatalf("malformed memory.current err=%v, want %s (not the shim refusal)", err, runner.CodeConfineResizeUsageUnevaluated)
	}
	if ledgerFingerprint(queue) != before {
		t.Fatal("a refused resize changed the ledger")
	}

	write(fmt.Sprintf("%d\n", 3*qGiB))
	if _, _, err := server.resizeRunningConfine(slice, "job", "2G", "s-1", false); errorCode(err) != runner.CodeConfineResizeBelowUsage {
		t.Fatalf("2G against 3G live err=%v, want %s", err, runner.CodeConfineResizeBelowUsage)
	}
	result, usage, err := server.resizeRunningConfine(slice, "job", "4G", "s-1", false)
	if err != nil || usage != 3*qGiB || result.NewReserveBytes != 4*qGiB {
		t.Fatalf("4G against 3G live: result=%+v usage=%d err=%v", result, usage, err)
	}
}

// verifies: AIRA-312 — the default floor reader. It reads the scope's
// memory.current with the existing reader; a measured 0 is a reading; a missing
// scope, a missing file and a malformed value are each refused with a stated
// reason, never a guessed floor.
func TestReadConfineScopeUsage(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for _, test := range []struct {
		name, dir string
		value     int64
		ok        bool
		reason    string
	}{
		{name: "reading", dir: write("reading", "12345\n"), value: 12345, ok: true},
		{name: "measured zero", dir: write("zero", "0\n"), value: 0, ok: true},
		{name: "missing scope", dir: filepath.Join(root, "absent"), reason: "not present"},
		{name: "no memory.current", dir: write("nofile", ""), reason: "could not be read"},
		{name: "malformed", dir: write("malformed", "12 34\n"), reason: "could not be read"},
		{name: "negative", dir: write("negative", "-5\n"), reason: "could not be read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, ok, reason := readConfineScopeUsage(test.dir)
			if ok != test.ok || value != test.value || !strings.Contains(reason, test.reason) || ok && reason != "" {
				t.Fatalf("value=%d ok=%v reason=%q, want %d/%v containing %q", value, ok, reason, test.value, test.ok, test.reason)
			}
		})
	}
}

// verifies: AIRA-312 test 2 — through the real management handler: a refusal
// writes no audit line and does not kick the evaluator; a success writes the
// line (actor, old, new), kicks the evaluator, and replies with the result.
func TestConfineResizeHandlerAuditsOnlyOnSuccess(t *testing.T) {
	now := time.Unix(60600, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	job := runningJob(1, "target", "s-1", 8*qGiB)
	queue := queueOf(server, job)
	registerAdmitQueue(server, queue)
	usageSeam(server, map[string]int64{job.scopeID: qGiB})
	request := func(caller, reserve string) core.Request {
		return core.Request{Verb: "confine-resize", Args: map[string]any{"slice": "aira.slice", "selector": "target", "memory_reserve": reserve, "owner": caller}}
	}
	// The handler resolves the slice; pin it to the fixture path.
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }

	var refused core.Response
	logged := captureDaemonLog(t, func() { refused = server.confineManagement(t.Context(), request("s-2", "2G")) })
	if refused.OK || refused.Code != runner.CodeConfineOwnerUnverified || refused.Exit == 0 {
		t.Fatalf("foreign resize response=%+v, want %s", refused, runner.CodeConfineOwnerUnverified)
	}
	if strings.Contains(logged, "confine-resize") || drainKick(queue) {
		t.Fatalf("a refused resize left an audit line or kicked the evaluator:\n%s", logged)
	}

	var accepted core.Response
	logged = captureDaemonLog(t, func() { accepted = server.confineManagement(t.Context(), request("s-1", "2G")) })
	result, ok := accepted.Data.(runner.ConfineResizeResult)
	if !accepted.OK || !ok || result.ScopeID != job.scopeID || result.OldReserveBytes != 8*qGiB || result.NewReserveBytes != 2*qGiB {
		t.Fatalf("resize response=%+v, want target 8G -> 2G", accepted)
	}
	want := fmt.Sprintf("confine-resize: actor=s-1 steal=false target-scope=%s target-name=target target-owner=s-1 old=%d new=%d usage=%d", job.scopeID, 8*qGiB, 2*qGiB, qGiB)
	if !strings.Contains(logged, want) {
		t.Fatalf("missing audit line %q in:\n%s", want, logged)
	}
	if !drainKick(queue) {
		t.Fatal("a successful resize did not kick the evaluator")
	}
}

// verifies: AIRA-312 test 4 — a resize racing the release of the SAME job and a
// grant elsewhere on the slice. Under -race: no data race, the ledger is always
// the re-derived Σ, never negative, and the released job is discharged exactly
// once whichever side wins (a resize that loses the race answers not-found).
func TestConfineResizeRacesReleaseAndGrant(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		now := time.Unix(60700, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		target := runningJob(1, "target", "s-1", 16*qGiB)
		keeper := runningJob(2, "keeper", "s-1", 8*qGiB)
		waiting := queuedWaiter(3, 4*qGiB, now) // fits whether or not the resize lands
		queue := queueOf(server, target, keeper, waiting)
		registerAdmitQueue(server, queue)
		usageSeam(server, map[string]int64{target.scopeID: qGiB, keeper.scopeID: qGiB})

		var wg sync.WaitGroup
		var resizeErr error
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _, resizeErr = server.resizeRunningConfine("/slice", "target", "2G", "s-1", false)
		}()
		go func() {
			defer wg.Done()
			server.releaseAdmitWaiter(queue, target)
		}()
		go func() {
			defer wg.Done()
			server.evaluateAdmitQueue(queue)
		}()
		wg.Wait()
		server.evaluateAdmitQueue(queue) // the grant lands even if its pass ran first
		waitAdmitGrant(t, waiting)

		if code := errorCode(resizeErr); code != "OK" && code != runner.CodeConfineNotFound {
			t.Fatalf("iteration %d: resize err=%v, want success or not-found", iteration, resizeErr)
		}
		queue.mu.Lock()
		wantOut, wantCPU, wantVRAM, wantJobs := rederiveLedgerLocked(queue)
		if queue.outstanding != wantOut || queue.cpuOutstanding != wantCPU || queue.vramOutstanding != wantVRAM || queue.outstandingJobs != wantJobs {
			queue.mu.Unlock()
			t.Fatalf("iteration %d: cached ledger %d/%d/%d/%d is not the re-derived %d/%d/%d/%d", iteration,
				queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs, wantOut, wantCPU, wantVRAM, wantJobs)
		}
		if queue.outstanding != 12*qGiB || queue.outstandingJobs != 2 || target.state != admitReleased {
			queue.mu.Unlock()
			t.Fatalf("iteration %d: outstanding=%d jobs=%d target=%v, want keeper+waiter (12 GiB, 2 jobs) and target released once", iteration, queue.outstanding, queue.outstandingJobs, target.state)
		}
		queue.mu.Unlock()
	}
}

// verifies: AIRA-312 test 5 — a reduction is dropped by ANY re-declare, in both
// orderings, and a stale anchor's EOF never discharges the re-anchored lease.
// The client's lease keeper re-declares the reserve it was GRANTED (it froze
// that figure), so a re-declare after a resize restores the original; a resize
// after a re-declare applies to the re-anchored lease, and the next re-declare
// restores the original again. The four derived totals stay the re-derived Σ
// throughout and return to exactly zero on the last release.
func TestConfineResizeIsDroppedByAReDeclare(t *testing.T) {
	const scopeID = "CONFINE-job-6100-ab@s-1"
	check := func(t *testing.T, queue *sliceQueue, lease *admitWaiter, wantReserve int64, wantState admitWaiterState) {
		t.Helper()
		queue.mu.Lock()
		defer queue.mu.Unlock()
		wantOut, wantCPU, wantVRAM, wantJobs := rederiveLedgerLocked(queue)
		if lease.reserve != wantReserve || lease.state != wantState || queue.outstanding != wantOut || queue.cpuOutstanding != wantCPU ||
			queue.vramOutstanding != wantVRAM || queue.outstandingJobs != wantJobs || queue.outstanding < 0 {
			t.Fatalf("lease reserve=%d state=%v ledger %d/%d/%d/%d (rederived %d/%d/%d/%d), want reserve %d state %v",
				lease.reserve, lease.state, queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs,
				wantOut, wantCPU, wantVRAM, wantJobs, wantReserve, wantState)
		}
	}
	reDeclare := func(t *testing.T, server *Server, lease *admitWaiter) {
		t.Helper()
		conn := testAnchorConn()
		_, got, code, err := server.enqueueReDeclare("/slice", 8*gib, "pinned:client", admitRequest{
			scopeID: scopeID, name: "job", owner: "s-1", cpu: 1, peerSameUID: true, conn: conn,
		})
		if err != nil || code != "" || got != lease || lease.anchor != conn {
			t.Fatalf("re-declare: code=%q err=%v re-anchored=%v", code, err, got == lease)
		}
	}
	setup := func(t *testing.T) (*Server, *sliceQueue, *admitWaiter) {
		server := anchorTestServer()
		queue, lease, _ := grantedAnchorLease(server, scopeID, 8*gib)
		usageSeam(server, map[string]int64{scopeID: gib})
		return server, queue, lease
	}

	t.Run("resize then re-declare restores the original", func(t *testing.T) {
		server, queue, lease := setup(t)
		stale := lease.anchor
		if _, _, err := server.resizeRunningConfine("/slice", "job", "2G", "s-1", false); err != nil {
			t.Fatalf("resize: %v", err)
		}
		check(t, queue, lease, 2*gib, admitGranted)
		reDeclare(t, server, lease) // reconnect WITHOUT a daemon restart
		check(t, queue, lease, 8*gib, admitGranted)
		server.releaseAdmitWaiterAnchored(queue, lease, stale) // stale-anchor EOF: a no-op
		check(t, queue, lease, 8*gib, admitGranted)
		server.releaseAdmitWaiterAnchored(queue, lease, lease.anchor)
		check(t, queue, lease, 8*gib, admitReleased)
		if queue.outstanding != 0 || queue.outstandingJobs != 0 {
			t.Fatalf("after the last release outstanding=%d jobs=%d, want exactly zero", queue.outstanding, queue.outstandingJobs)
		}
	})

	t.Run("re-declare then resize, then the next re-declare restores", func(t *testing.T) {
		server, queue, lease := setup(t)
		stale := lease.anchor
		reDeclare(t, server, lease)
		if _, _, err := server.resizeRunningConfine("/slice", scopeID, "2G", "s-1", false); err != nil {
			t.Fatalf("resize of the re-anchored lease: %v", err)
		}
		check(t, queue, lease, 2*gib, admitGranted)
		server.releaseAdmitWaiterAnchored(queue, lease, stale)
		check(t, queue, lease, 2*gib, admitGranted)
		reDeclare(t, server, lease)
		check(t, queue, lease, 8*gib, admitGranted)
		server.releaseAdmitWaiterAnchored(queue, lease, lease.anchor)
		check(t, queue, lease, 8*gib, admitReleased)
		if queue.outstanding != 0 {
			t.Fatalf("outstanding=%d after the last release, want 0 (no double or partial discharge)", queue.outstanding)
		}
	})

	t.Run("a daemon restart drops it", func(t *testing.T) {
		server, _, _ := setup(t)
		if _, _, err := server.resizeRunningConfine("/slice", "job", "2G", "s-1", false); err != nil {
			t.Fatalf("resize: %v", err)
		}
		restarted := anchorTestServer()
		if _, _, err := restarted.resizeRunningConfine("/slice", "job", "2G", "s-1", false); errorCode(err) != runner.CodeConfineNotFound {
			t.Fatalf("resize on a restarted daemon err=%v, want %s (nothing survives a restart)", err, runner.CodeConfineNotFound)
		}
		if _, _, code, err := restarted.enqueueReDeclare("/slice", 8*gib, "pinned:client", admitRequest{
			scopeID: scopeID, name: "job", owner: "s-1", cpu: 1, peerSameUID: true, conn: testAnchorConn(),
		}); err != nil || code != "" {
			t.Fatalf("survivor re-declare: code=%q err=%v", code, err)
		}
		if got := restarted.GrantedLeasesForTest()[scopeID]; got != 8*gib {
			t.Fatalf("re-established reserve=%d, want the original 8 GiB (the reduction is dropped)", got)
		}
	})
}
