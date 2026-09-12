//go:build linux

// package pylib_test (external), NOT package pylib: this file imports
// aira/internal/daemon, and internal/daemon transitively imports internal/core
// -> internal/pylib, so an internal `package pylib` file importing daemon hits
// an import cycle. The S16 e2e (pytest_aitest_e2e_test.go) solved this exact
// situation the same way; this file reuses that file's requireRealPytest and
// shortE2ERuntimeDir helpers directly (same package).
package pylib_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/daemon"
	"aira/internal/pylib"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// S18 — the WORKER-PATH restart merge gate (the branch-exit gate for the
// 18-slice admission-counter rebuild). It is the worker-lease twin of the S13
// confine merge gate (internal/daemon/restart_merge_gate_real_cgroup_linux_test.go)
// and closes the one gap that gate cannot: the AIRA-41 EOF-release + the Go
// relay keeper's reconnect/re-declare + the aitest supervisor, exercised as a
// whole across a REAL daemon restart, against REAL cgroups.
//
// The NO-DUMP model (design §4): a daemon restart drops every connection but
// leaves jobs holding their RAM/CPU. The new daemon starts EMPTY with a 2 s
// new-admission freeze. Each surviving worker's `aira worker-admit` relay keeper
// re-dials (2/sec) and re-declares its frozen ARDR frame (freeze-EXEMPT),
// re-anchoring the lease in the new ledger. There is no dump, no reload, no
// kill-probe. A worker's relay closing stdin (retirement) releases its lease
// IMMEDIATELY on the daemon's EOF (the AIRA-41 reversal).
//
// DETERMINISM. Like the S13 confine gate — which charges a FIXTURE slice, not
// the shared real aira.slice — this gate pins the LEDGER slice + a fixed,
// headroom-ample memory reading via the daemon's test seams. The shared real
// aira.slice's free space depends on every other session on the box, which would
// make the pool-fill and the exact-Σ assertions flaky. The worker cgroup
// SUB-SCOPES stay REAL (runner.CreateWorkerScope under the real outer scope); only
// the slice the leases charge against is pinned. The 2 s restart freeze is left
// at its real default (real time, not a fake clock — the S11 trap).
//
// THE THREE FALSE-PASS TRAPS this gate is written to avoid (all three flagged in
// review):
//
//  1. POSITIVE, EXACT re-anchor — NOT "<=" and NOT "no over-admit". An UNDER-count
//     (a survivor that failed to re-anchor) trivially passes "no over-admit", so
//     the pool-reanchor test asserts B's ledger key-set == EXACTLY the survivors'
//     verbatim scope-ids with Σ == exactly their reserves, mirroring the S13
//     confine gate's exact-key-set assertion for the worker path.
//  2. RESTART ONLY AFTER THE POOL IS FULL + STABLE, and daemon_available STAYS
//     TRUE across it. The S16 residual is that an EMPTY-pool claim landing in the
//     restart window disables the daemon -> the supervisor runs UNCONFINED and the
//     suite completes GREEN — a false pass measuring nothing. Restarting only once
//     N worker leases are present avoids that window; asserting the run never
//     printed the _disable_daemon fallback/UNCONFINED notice proves containment was
//     not stripped (the same class as the shipped `--admit-timeout` false-pass).
//  3. RETIRED-WORKER via the REAL EOF-release path — stronger than "absent in B"
//     (which is trivially true since B starts empty). The EOF-release test retires
//     a worker WHILE daemon A is still running by closing its REAL `aira
//     worker-admit` relay subprocess's stdin, and asserts A's ledger DROPS it
//     IMMEDIATELY (S15's unit test used a net.Pipe; this closes a real relay's
//     stdin), THEN restarts and asserts B holds exactly the remaining N-1.
//
// MUTATIONS that must RED this gate (verified by hand and reverted):
//   (a) disable the worker relay keeper's reconnect (internal/runner/
//       lease_keeper_linux.go — reconnectAndReDeclare returns false, or
//       newLeaseKeeperFrame does not start the goroutine): a survivor's lease is
//       ABSENT in B -> the exact-key-set re-anchor assertion (trap 1) reds in BOTH
//       worker-path tests. Requires rebuilding the aira binary (the relay is that
//       binary).
//   (b) re-add a supervisor fail-open / _disable_daemon on the restart-window
//       Unavailable (internal/pylib/aitest/supervisor.py — _try_grow_one's
//       WorkerAdmitUnavailable branch calls _disable_daemon): the pool run prints
//       the fallback/UNCONFINED notice -> the daemon-available-across-restart
//       property (trap 2) reds. Takes effect on `go test` recompile (supervisor.py
//       is go:embed'd and re-extracted by ExtractAitest).

const (
	// The direct-relay test's per-worker reserve == the real worker-scope
	// memory.max. 32 MiB matches the proven S16 e2e value (a blocking, near-noop
	// worker is lighter than S16's real test workers, which run under this cap fine).
	restartGateWorkerReserve = 32 << 20
	// The PYTEST test's per-worker reserve. Sized together with the ledger maximum
	// below so EXACTLY TWO workers fit and a third never does — see startServer's
	// memory reader and the pool test's worker_count=3. Under a 256 MiB memory.max a
	// blocking pytest worker is comfortable (it uses tens of MiB).
	restartGatePytestReserve = 256 << 20
	// The pinned fixture ledger MAXIMUM. With the daemon's default headroom
	// (2 GiB + 64 MiB/job) and checkedAvailable = maximum − headroom − outstanding:
	//   1 worker held (256 MiB): available = 2816 − (2048+128) − 256 = 384 MiB >= 256 -> a 2nd FITS
	//   2 workers held (512 MiB): available = 2816 − (2048+192) − 512 =  64 MiB <  256 -> a 3rd does NOT fit
	// so a pool asked for 3 (--aitest-workers=3) stabilises at exactly 2 and the
	// supervisor keeps PROBING for the 3rd across the restart — which is what makes
	// the daemon-available-stays-true property (trap 2) mutation-catchable rather
	// than vacuous (a full pool at worker_count would never touch the daemon during
	// the restart at all). Margins are ~128/192 MiB, not knife-edge.
	restartGateFixtureMaximum = 2<<30 + 768<<20
	// The pinned fixture ledger slice path. Any stable string works: the resolve
	// AND read seams below both ignore/answer it uniformly, so it need not be a
	// real cgroup — the leases charge it, the worker sub-scopes are created
	// elsewhere (under the real outer scope).
	restartGateFixtureSlice = "/aira-mergegate-worker-fixture-slice"
)

type restartGateHarness struct {
	pytest    string // "" when pytest is not needed (the EOF-release test)
	binary    string
	outer     string
	outerFile *os.File
	aitestDir string
	pythonDir string
	paths     daemon.Paths
}

func newRestartGateHarness(t *testing.T, needPytest bool) *restartGateHarness {
	t.Helper()
	pytest := ""
	if needPytest {
		pytest = requireRealPytest(t)
	}
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-restartgate")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real, finite numeric cap (readSliceMemory treats a bare "max" as unbounded),
	// and the finite-memory.max the aitest bootstrap requires as proof this is a
	// real confine scope. Bounds ACTUAL Σ(worker usage) via the outer scope's
	// oom.group; 768 MiB comfortably holds the supervisor plus two workers whose real
	// RSS is tens of MiB each (their 256 MiB memory.max is a cap, not an allocation).
	if err := os.WriteFile(filepath.Join(outer, "memory.max"), []byte("805306368"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set outer memory.max: %v", err)
	}
	if !needPytest {
		// The DIRECT-RELAY path has no aitest bootstrap to delegate the memory
		// controller onto the outer scope, so the daemon's CreateWorkerScope would
		// find no memory.* files under a worker sub-scope. Do it here — safe on this
		// path precisely because NO process is ever placed directly into the outer
		// scope (the relays are the test's own subprocesses, in the test's cgroup),
		// so cgroup-v2's no-internal-process rule is not violated. The PYTEST path
		// must NOT pre-delegate: its supervisor is placed directly into outer via
		// CgroupFD and the bootstrap delegates only AFTER draining it out, so
		// pre-enabling here would EBUSY that placement.
		if err := os.WriteFile(filepath.Join(outer, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate +memory onto the outer scope: %v", err)
		}
	}

	binary := filepath.Join(t.TempDir(), "aira")
	// aira confine -- wraps the build per the project's global constraints; it
	// nests under the outer `aira confine -- go test ...` this whole binary runs
	// in. The machine's installed `aira` on PATH builds a fresh one into a temp dir.
	build := exec.Command("aira", "confine", "--", "go", "build", "-o", binary, "aira/cmd/aira")
	if buildOutput, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, buildOutput)
	}

	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortE2ERuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	pythonDir, err := pylib.ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	outerFile, err := os.Open(outer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outerFile.Close() })
	return &restartGateHarness{
		pytest: pytest, binary: binary, outer: outer, outerFile: outerFile,
		aitestDir: aitestDir, pythonDir: pythonDir, paths: paths,
	}
}

// startServer starts one daemon on the harness's (fixed) socket, with the fixture
// ledger slice + a fixed headroom-ample memory reading pinned. The caller controls
// the A->B restart by cancelling A itself. The real 2 s freeze is left armed.
func (h *restartGateHarness) startServer(t *testing.T) (*daemon.Server, context.CancelFunc, <-chan error) {
	t.Helper()
	server := daemon.NewServer(h.paths)
	server.SetAdmitResolveSliceForTest(func(string) (string, bool, string) {
		return restartGateFixtureSlice, true, ""
	})
	server.SetAdmitReadMemoryForTest(func(string) (int64, int64, int64, bool, string) {
		// current=0, reclaimable=0, maximum=restartGateFixtureMaximum — sized so the
		// pytest pool stabilises at exactly 2 workers (see the constant). Independent
		// of whatever else is on the shared box, which is what keeps the exact-Σ and
		// exact-key-set assertions deterministic. The direct-relay test's 32 MiB
		// workers fit trivially under the same maximum.
		return 0, restartGateFixtureMaximum, 0, true, ""
	})
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("daemon exited before ready: %v", err)
	case <-testdeadline.After(5 * time.Second):
		cancel()
		t.Fatal("daemon did not become ready")
	}
	return server, cancel, done
}

func awaitServerShutdown(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

// waitLeaseCount polls the server's granted-lease ledger until it holds exactly
// `want` leases or the timeout elapses, returning the last snapshot. When runCh
// is non-nil it also watches for the pytest run ending early (which would mean the
// suite died before the pool reached `want`) and fails loudly with its output
// rather than silently timing out.
func waitLeaseCount(t *testing.T, server *daemon.Server, want int, timeout time.Duration, runCh <-chan runOutcome) map[string]int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		leases := server.GrantedLeasesForTest()
		if len(leases) == want {
			return leases
		}
		if runCh != nil {
			select {
			case o := <-runCh:
				t.Fatalf("aitest run ended before the pool reached %d leases (had %d): err=%v\n%s",
					want, len(leases), o.err, o.output)
			default:
			}
		}
		if time.Now().After(deadline) {
			return leases
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type runOutcome struct {
	output []byte
	err    error
}

// TestRestartMergeGateAitestPoolReanchorsAcrossRestart is the headline gate: a
// REAL aitest suite (>=2 workers) under a REAL daemon, the daemon restarted MID-RUN
// once the pool is full and stable, asserting the whole worker-lease path
// reconciles — positive-exact re-anchor (trap 1), daemon-available-stays-true
// (trap 2), actual pool growth to N, and the freeze holding a fresh admit.
//
// The suite asks for 3 workers but the pinned ledger fits exactly 2 (see
// restartGateFixtureMaximum), so the stable pool is 2 AND the supervisor keeps
// probing the daemon for a 3rd throughout — including across the restart. That
// continued probing is deliberate: it is what makes trap 2 real. A pool sitting
// at worker_count would never touch the daemon during the restart, so a re-added
// _disable_daemon fault would have nothing to fire on and the assertion would be
// vacuous; here each restart-window / freeze-window growth probe exercises exactly
// the branch mutation (b) corrupts.
//
// verifies: S18 worker-path restart merge gate (design §4).
func TestRestartMergeGateAitestPoolReanchorsAcrossRestart(t *testing.T) {
	h := newRestartGateHarness(t, true)
	serverA, cancelA, doneA := h.startServer(t)

	// The release sentinel: its ABSENCE blocks every fixture test; we touch it only
	// after B has re-anchored the survivors, so the pool stays alive across the
	// restart and then completes cleanly.
	sentinel := filepath.Join(t.TempDir(), "release")

	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Minute))
	defer cancelRun()
	command := exec.CommandContext(runCtx, h.pytest, "-q", "--aitest-workers=3")
	command.Dir = filepath.Join(h.aitestDir, "restart_gate_testdata")
	// WaitDelay bounds the post-kill wait on the output pipes (a grandchild holding
	// a duplicate write end keeps them open) — load-bearing on a real-cgroup run,
	// per the S16 e2e's own note.
	command.WaitDelay = 15 * time.Second
	// Place the supervisor directly into the real outer scope at creation, the same
	// clone3(CLONE_INTO_CGROUP) mechanism the S16 e2e uses.
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(h.outerFile.Fd())}
	command.Env = append(environForRealDaemonAitest(),
		"PYTHONPATH="+filepath.Dir(h.aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+h.pythonDir,
		"AIRA_AITEST_BOOTSTRAP_CMD="+h.binary,
		"AIRA_AITEST_WORKER_ADMIT_CMD="+h.binary,
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(restartGatePytestReserve),
		"AIRA_REAL_CGROUP=1",
		"AIRA_AITEST_RESTART_SENTINEL="+sentinel,
		"AIRA_AITEST_RESTART_BLOCK_TIMEOUT=100",
	)
	runCh := make(chan runOutcome, 1)
	go func() {
		out, err := command.CombinedOutput()
		runCh <- runOutcome{out, err}
	}()

	// 1. POOL GROWTH TO N (design assertion #4): wait for EXACTLY 2 stable worker
	//    leases on A — proof the pool grew to 2 via the probe->claim path, not that
	//    "the suite ran". A confine/cgroup that cannot be established here would
	//    surface as the run ending early (waitLeaseCount reports it with output).
	leasesA := waitLeaseCount(t, serverA, 2, testdeadline.Wait(45*time.Second), runCh)
	if len(leasesA) != 2 {
		cancelRun()
		o := <-runCh
		t.Fatalf("pool did not fill to 2 worker leases on A (got %d: %v)\n%s", len(leasesA), leasesA, o.output)
	}
	// Capture the exact scope-id key-set + reserves the survivors must re-anchor to.
	wantKeys := make(map[string]int64, len(leasesA))
	for id, reserve := range leasesA {
		wantKeys[id] = reserve
	}

	// 2. Restart A -> B (MID-RUN, pool full + stable), with a deliberate daemon-DOWN
	//    GAP between them. A real restart is not instantaneous, and the gap is
	//    load-bearing for trap 2: during it the supervisor's growth probes (it is
	//    still chasing the 3rd worker) DIAL a dead socket and get class=admission-
	//    unusable — exactly the transient the S16 fix must ride out (return False,
	//    daemon left available) and exactly what the pre-S16 fail-open (mutation b)
	//    turned into a _disable_daemon. The two surviving workers ride the gap out
	//    too: their relay keepers reconnect at 2/sec and re-anchor once B is up.
	cancelA()
	awaitServerShutdown(t, doneA)
	time.Sleep(testdeadline.Wait(3 * time.Second))
	serverB, cancelB, doneB := h.startServer(t)
	defer func() { cancelB(); awaitServerShutdown(t, doneB) }()

	if pre := serverB.GrantedLeasesForTest(); len(pre) != 0 {
		cancelRun()
		t.Fatalf("B started with a non-empty ledger %v; the no-dump restart must open empty", pre)
	}

	// 3. FREEZE holds a fresh admit (design assertion #5). A non-blocking worker-admit
	//    PROBE dialled into B during the 2 s freeze reports state=unevaluated
	//    reason=snapshot (the slice is FROZEN, not saturated — the S11 honesty pin),
	//    never a real snapshot or a grant. Done FIRST, before the 2 s can elapse
	//    (a localhost round trip is ~ms). Only re-declares are freeze-exempt.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Second))
	probe := runner.RequestWorkerAdmit(probeCtx, runner.WorkerAdmitClientRequest{
		SocketPath: h.paths.SocketPath, JobID: "freeze-probe", OuterScope: h.outer,
		ParentScopeID: e2eConfineScopeID, EstimatedBytes: restartGatePytestReserve, MaxWait: 0,
	})
	probeCancel()
	if probe.State != runner.WorkerAdmitStateUnevaluated || probe.Reason != runner.WorkerAdmitReasonSnapshot {
		cancelRun()
		t.Fatalf("fresh worker-admit probe during B's restart freeze reported state=%q reason=%q, want state=%q reason=%q "+
			"(the freeze must hold a fresh admit; only re-declares are exempt)",
			probe.State, probe.Reason, runner.WorkerAdmitStateUnevaluated, runner.WorkerAdmitReasonSnapshot)
	}

	// 4. POSITIVE, EXACT re-anchor (trap 1): the survivors' relay keepers reconnect
	//    and re-declare -> B's ledger holds EXACTLY the same 2 verbatim scope-ids,
	//    each charged EXACTLY its reserve, Σ exact. NOT "<=", NOT "no over-admit".
	leasesB := waitLeaseCount(t, serverB, 2, testdeadline.Wait(30*time.Second), runCh)
	if len(leasesB) != 2 {
		cancelRun()
		o := <-runCh
		t.Fatalf("B holds %d worker leases after the restart, want exactly the 2 survivors re-anchored: %v\n%s", len(leasesB), leasesB, o.output)
	}
	var sumWant, sumGot int64
	for id, reserve := range wantKeys {
		sumWant += reserve
		got, present := leasesB[id]
		if !present {
			cancelRun()
			t.Fatalf("B's ledger key-set %v is missing the survivor's VERBATIM scope-id %q — a survivor that failed to re-anchor (or re-declared under a transformed key) is a FALSE PASS of any '<=' / 'no over-admit' check", leasesB, id)
		}
		if got != reserve {
			cancelRun()
			t.Fatalf("survivor %q re-anchored with reserve %d in B, want exactly %d (no drift, no double-count)", id, got, reserve)
		}
	}
	for _, reserve := range leasesB {
		sumGot += reserve
	}
	if sumGot != sumWant {
		cancelRun()
		t.Fatalf("Σleases(B) = %d, want exactly the survivors' %d — every survivor re-anchored once, none double-counted, nothing extra admitted", sumGot, sumWant)
	}

	// 5. Release the pool: the fixture tests unblock and the suite completes.
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		cancelRun()
		t.Fatalf("write release sentinel: %v", err)
	}

	// 6. The run completes GREEN and daemon_available STAYED TRUE across the restart
	//    (trap 2): the supervisor never printed the _disable_daemon fallback/UNCONFINED
	//    notice, so containment was not stripped; every fixture test passed; exit 0.
	var outcome runOutcome
	select {
	case outcome = <-runCh:
	case <-testdeadline.After(45 * time.Second):
		cancelRun()
		t.Fatal("aitest run did not complete after the release sentinel was written")
	}
	text := string(outcome.output)
	if bytes.Contains(outcome.output, []byte("falling back to")) || bytes.Contains(outcome.output, []byte("UNCONFINED")) {
		t.Fatalf("the daemon-backed admission was disabled across the restart (containment stripped) — trap 2:\n%s", text)
	}
	for _, want := range []string{
		"test_restart_block.py::test_block_one passed",
		"test_restart_block.py::test_block_two passed",
		"test_restart_block.py::test_block_three passed",
	} {
		if !bytes.Contains(outcome.output, []byte(want)) {
			t.Fatalf("aitest run missing expected pass line %q:\n%s", want, text)
		}
	}
	if outcome.err != nil {
		t.Fatalf("aitest run exited nonzero after a clean restart+re-anchor: %v\n%s", outcome.err, text)
	}
}

// workerRelay is one live `aira worker-admit` relay SUBPROCESS holding a real
// daemon lease. Closing stdin releases the lease (the AIRA-41 EOF path).
type workerRelay struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
	name   string
}

func (h *restartGateHarness) startWorkerRelay(t *testing.T, name string) *workerRelay {
	t.Helper()
	cmd := exec.Command(h.binary, "worker-admit",
		"--job-id", name, "--outer-scope", h.outer,
		// S2a §16d: worker-admit requires an explicit, canonical parent_scope_id (the
		// daemon refuses an empty one). A direct-relay harness has no supervisor to
		// source AIRA_CONFINE_SCOPE_ID from, so it passes the canonical id directly.
		"--parent-scope-id", e2eConfineScopeID,
		"--estimated-bytes", strconv.Itoa(restartGateWorkerReserve))
	cmd.Env = os.Environ() // carries the harness's XDG_RUNTIME_DIR -> the test socket
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker-admit relay %q: %v", name, err)
	}
	// Drain stdout so the relay (which writes one grant line then blocks on stdin)
	// never stalls on a full pipe. We identify leases from the daemon ledger, not by
	// parsing this line, so the content is discarded.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	relay := &workerRelay{cmd: cmd, stdin: stdin, stderr: stderr, name: name}
	t.Cleanup(func() { relay.cleanup() })
	return relay
}

func (r *workerRelay) closeStdin() { _ = r.stdin.Close() }

func (r *workerRelay) cleanup() {
	_ = r.stdin.Close()
	done := make(chan struct{})
	go func() { _, _ = r.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = r.cmd.Process.Kill()
		<-done
	}
}

// TestRestartMergeGateWorkerRelayEOFReleaseThenReanchor is the item-3 gate: the
// REAL EOF-release path (trap 3). Two real `aira worker-admit` relay subprocesses
// hold real leases; closing ONE relay's stdin while daemon A is up drops its lease
// IMMEDIATELY, and after the restart B holds exactly the remaining N-1 (the survivor
// re-anchored; the retired one cannot, its process is gone).
//
// verifies: S18 worker-path restart merge gate, real-relay EOF-release (design §3/§4).
func TestRestartMergeGateWorkerRelayEOFReleaseThenReanchor(t *testing.T) {
	h := newRestartGateHarness(t, false)
	serverA, cancelA, doneA := h.startServer(t)

	relay1 := h.startWorkerRelay(t, "relay-1")
	relay2 := h.startWorkerRelay(t, "relay-2")

	// Both leases established on A. If the real cgroup could not create the worker
	// sub-scopes the leases never appear; the harness already skip-or-failed on a
	// missing controller, so a persistent absence here is a real failure.
	leasesA := waitLeaseCount(t, serverA, 2, testdeadline.Wait(30*time.Second), nil)
	if len(leasesA) != 2 {
		cancelA()
		t.Fatalf("both worker relays did not establish leases on A (got %d: %v)\nrelay-1 stderr: %s\nrelay-2 stderr: %s",
			len(leasesA), leasesA, relay1.stderr.String(), relay2.stderr.String())
	}
	keysBefore := make(map[string]struct{}, len(leasesA))
	for id := range leasesA {
		keysBefore[id] = struct{}{}
	}

	// EOF RELEASE (trap 3): retire relay-1 WHILE A is still running by closing its
	// real relay's stdin. The relay's io.Copy(stdin) returns -> lease.Close ->
	// keeper closes the daemon connection -> the daemon releases relay-1's reserve
	// IMMEDIATELY on that EOF. A's ledger drops from 2 to EXACTLY 1.
	relay1.closeStdin()
	leasesAfterDrop := waitLeaseCount(t, serverA, 1, testdeadline.Wait(15*time.Second), nil)
	if len(leasesAfterDrop) != 1 {
		cancelA()
		t.Fatalf("A holds %d leases after relay-1's stdin closed, want exactly 1 (the EOF must release relay-1's reserve immediately): %v", len(leasesAfterDrop), leasesAfterDrop)
	}
	var survivorKey string
	var survivorReserve int64
	for id, reserve := range leasesAfterDrop {
		survivorKey, survivorReserve = id, reserve
	}
	if _, wasOriginal := keysBefore[survivorKey]; !wasOriginal {
		cancelA()
		t.Fatalf("the surviving lease %q was not one of the two originally established leases %v", survivorKey, keysBefore)
	}

	// Restart A -> B. relay-2's keeper reconnects + re-declares; relay-1's process
	// exited on its stdin EOF, so it cannot re-anchor.
	cancelA()
	awaitServerShutdown(t, doneA)
	serverB, cancelB, doneB := h.startServer(t)
	defer func() { cancelB(); awaitServerShutdown(t, doneB) }()

	if pre := serverB.GrantedLeasesForTest(); len(pre) != 0 {
		t.Fatalf("B started with a non-empty ledger %v; the no-dump restart must open empty", pre)
	}

	// B holds EXACTLY the remaining N-1: the survivor's VERBATIM scope-id, charged
	// exactly its reserve. The retired relay must be absent (it is gone), and no
	// phantom or extra lease may appear.
	leasesB := waitLeaseCount(t, serverB, 1, testdeadline.Wait(20*time.Second), nil)
	if len(leasesB) != 1 {
		t.Fatalf("B holds %d leases, want exactly the 1 surviving worker (N-1): %v", len(leasesB), leasesB)
	}
	bReserve, present := leasesB[survivorKey]
	if !present {
		t.Fatalf("B's ledger key-set %v does not carry the survivor's VERBATIM scope-id %q — the survivor failed to re-anchor, or the retired relay re-anchored instead", leasesB, survivorKey)
	}
	if bReserve != survivorReserve {
		t.Fatalf("survivor %q re-anchored with reserve %d in B, want exactly %d", survivorKey, bReserve, survivorReserve)
	}
}
