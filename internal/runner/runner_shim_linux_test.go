//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shimPanicBackend is the ci-shim counterpart of unavailableBackend, and the
// difference is the point of it: EVERY cgroup seam PANICS.
//
// unavailableBackend returns an ERROR from Probe, so an implementation that
// probed the backend "and handled the failure" would still pass a test built on
// it. AIRA-129 requirement 1 asks for the cgroup path to be skipped ENTIRELY up
// front, and only a panic can tell "skipped" from "attempted and recovered".
type shimPanicBackend struct{}

func (shimPanicBackend) Probe(context.Context) error {
	panic("ci-shim launch reached the cgroup seam Probe")
}

func (shimPanicBackend) Create(context.Context, string) (Scope, error) {
	panic("ci-shim launch reached the cgroup seam Create")
}

func (shimPanicBackend) Open(context.Context, string) (Scope, error) {
	panic("ci-shim launch reached the cgroup seam Open")
}

func shimRunner(t *testing.T, cfg Config) *Runner {
	t.Helper()
	if cfg.CommonDir == "" {
		cfg.CommonDir = t.TempDir()
	}
	if cfg.Backend == nil {
		cfg.Backend = shimPanicBackend{}
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The durable install-mode record is process-global and cached behind a
	// sync.Once, so the seam is used rather than AIRA_INSTALL_MODE_FILE: two
	// tests could not share the env-var route.
	r.resolveModeFn = func() string { return ConfineModeShim }
	return r
}

func shimLedgerKinds(t *testing.T, r *Runner) []string {
	t.Helper()
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// verifies: AIRA-129 requirement 1 and 4 (the newBackend-panics harness), and
// requirement 2's established unevaluated facets.
//
// Counterexample: the pre-AIRA-129 implementation refused outright, and any
// implementation that probes, creates or opens a scope panics on one of
// shimPanicBackend's seams. Only a launch that skips every one of them up front
// AND still produces a real captured run passes.
func TestShimRunLaunchesWithoutTouchingAnyCgroupSeam(t *testing.T) {
	var diagnostics bytes.Buffer
	r := shimRunner(t, Config{Diagnostics: &diagnostics})
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "printf out; printf err >&2"}})
	if err != nil {
		t.Fatalf("shim launch err=%v diagnostics=%s", err, diagnostics.String())
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("record=%+v", record)
	}
	if record.Containment != ConfineContainmentAdvisory {
		t.Fatalf("containment=%q, want %q", record.Containment, ConfineContainmentAdvisory)
	}
	if record.ScopeIntegrity != ScopeAdvisory {
		t.Fatalf("scope-integrity=%q, want %q", record.ScopeIntegrity, ScopeAdvisory)
	}
	if record.CgroupScope != "" {
		t.Fatalf("cgroup_scope=%q; a ci-shim run names no cgroup, and Reconcile/Kill key off exactly that emptiness", record.CgroupScope)
	}
	// Established unevaluated, never a measured zero: nothing may be fed back to
	// the AIRA-67 per-signature estimator from a mode with no memory.peak.
	if record.PeakRSS != nil || record.CPUUser != nil || record.CPUSys != nil {
		t.Fatalf("usage must be unevaluated in ci-shim mode: peak=%v user=%v sys=%v", record.PeakRSS, record.CPUUser, record.CPUSys)
	}
	if record.DescendantEscape != nil || record.ScopeKill.Requested || record.KillIntent.Present {
		t.Fatalf("no kill or escape evidence may be fabricated: %+v", record)
	}
	if !record.CaptureComplete || record.CaptureForcedClosed || len(record.ErrorCodes) != 0 {
		t.Fatalf("capture/errors=%+v", record)
	}
	if !record.CleanSuccess() {
		t.Fatalf("a clean ci-shim run must read as a clean success, not a failed one: %+v", record)
	}
	if data, readErr := os.ReadFile(record.OutputRefs["out"].Path); readErr != nil || string(data) != "out" {
		t.Fatalf("stdout=%q err=%v", data, readErr)
	}
	if data, readErr := os.ReadFile(record.OutputRefs["err"].Path); readErr != nil || string(data) != "err" {
		t.Fatalf("stderr=%q err=%v", data, readErr)
	}
	if !strings.Contains(diagnostics.String(), string(ConfineContainmentAdvisory)) {
		t.Fatalf("the launch must SAY it is advisory; diagnostics=%q", diagnostics.String())
	}
}

// verifies: AIRA-129 requirement 2 — a requested per-run cap is reported, never
// silently dropped and never echoed back as though a kernel had written it.
func TestShimRunRecordsARequestedMemoryCapAsUnenforced(t *testing.T) {
	r := shimRunner(t, Config{})
	record, err := r.Launch(context.Background(), Request{
		Argv: []string{"/bin/true"}, ScopeMemoryMax: 4 << 30, ScopeMemoryHigh: 2 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ScopeMemoryMax != nil || record.ScopeMemoryHigh != nil {
		t.Fatalf("an unenforced cap must stay unevaluated, not be echoed back: max=%v high=%v", record.ScopeMemoryMax, record.ScopeMemoryHigh)
	}
	if !containsPrefix(record.ErrorCodes, "U_RUN_SCOPE_CAP_UNENFORCED") {
		t.Fatalf("error codes=%v, want U_RUN_SCOPE_CAP_UNENFORCED", record.ErrorCodes)
	}
	// The request must still RUN. AIRA-121 requirement 6: resource flags parse
	// and run in shim mode rather than rejecting the container's invocation.
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("record=%+v", record)
	}
}

// verifies: AIRA-129 requirement 2 — a --cpu-timeout is a cgroup cpu.stat budget
// and there is no cpu.stat, so it reaches the "NEVER MEASURED" state
// U_RUN_CPU_BUDGET_UNENFORCED was catalogued for rather than silently passing.
func TestShimRunRecordsARequestedCPUBudgetAsUnenforced(t *testing.T) {
	r := shimRunner(t, Config{})
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, CPUTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPrefix(record.ErrorCodes, "U_RUN_CPU_BUDGET_UNENFORCED") {
		t.Fatalf("error codes=%v, want U_RUN_CPU_BUDGET_UNENFORCED", record.ErrorCodes)
	}
}

// verifies: AIRA-129 requirement 3 — the written --detach decision, and that it
// is taken AT THE DOOR.
//
// The second assertion is the one that matters: the ticket's own reason for
// refusing rather than half-supporting is that failing deep inside the launch
// "would leave a half-written run record behind". A refusal that had already
// reserved an ID or appended a `starting` event would be exactly that.
func TestShimRunRefusesDetachBeforeWritingAnythingDurable(t *testing.T) {
	r := shimRunner(t, Config{})
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, Detach: true})
	if err == nil || !strings.Contains(err.Error(), "E_RUN_SCOPE_UNAVAILABLE") {
		t.Fatalf("detach err=%v record=%+v", err, record)
	}
	if !strings.Contains(err.Error(), "aira confine --detach") {
		t.Fatalf("the refusal must name the shim-capable alternative: %v", err)
	}
	if kinds := shimLedgerKinds(t, r); len(kinds) != 0 {
		t.Fatalf("a refused detach wrote %v to the ledger; it must leave no half-written record", kinds)
	}
}

// verifies: AIRA-129 requirement 1 — the process-GROUP reach, which is the whole
// of requirement 8's behaviour carried over from AIRA-121.
//
// The counterexample is a supervisor that signals only its direct child. Two
// INDEPENDENT things then go wrong and both are asserted: the grandchild
// survives the kill and creates its marker, and it keeps the inherited capture
// pipe open past the grace so the capture is forced closed. Verified by flipping
// confineCommand.group to false: with a single-PID signal the marker appears and
// CaptureComplete is false.
func TestShimRunTimeoutKillReachesTheJobsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")
	r := shimRunner(t, Config{Grace: 400 * time.Millisecond, TermGrace: 300 * time.Millisecond})
	// The `&` grandchild stays in the leader's process group: a non-interactive
	// shell without job control does not put a background job in its own group.
	script := "sh -c 'sleep 1; : > " + marker + "' & sleep 30"
	record, err := r.Launch(context.Background(), Request{
		Argv: []string{"/bin/sh", "-c", script}, Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("err=%v record=%+v", err, record)
	}
	if record.Status != StatusKilled || !containsPrefix(record.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("record=%+v", record)
	}
	if !record.CaptureComplete || record.CaptureForcedClosed {
		t.Fatalf("a grandchild left alive holds the capture pipe open: complete=%v forced=%v codes=%v",
			record.CaptureComplete, record.CaptureForcedClosed, record.ErrorCodes)
	}
	// The kill ran to its terminal state and the leader was proved dead...
	if !record.ScopeKill.Completed || !record.KillIntent.Completed {
		t.Fatalf("scope_kill=%+v kill_intent=%+v", record.ScopeKill, record.KillIntent)
	}
	// ...but nothing here proves a subtree empty, and empty_scope is the field
	// that would claim it. See RunRecord.Containment.
	if record.KillIntent.Empty {
		t.Fatal("ci-shim has no cgroup, so kill_intent.empty_scope is not establishable and must never be set")
	}
	if record.ScopeIntegrity != ScopeAdvisory {
		t.Fatalf("scope-integrity=%q; an unproven kill must not be recorded as a containment failure in a mode that has no containment", record.ScopeIntegrity)
	}
	// The grandchild's own marker lands one second after launch if it survived.
	time.Sleep(1200 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the grandchild survived the process-group kill: %v", statErr)
	}
}

// verifies: AIRA-129 requirement 3's cross-process half — `aira run kill` has no
// safe primitive in ci-shim mode and refuses BEFORE publishing an intent.
//
// The ledger assertion is the load-bearing one. Publishing first and failing to
// execute would leave a durable intent nothing can satisfy and would drive the
// launching supervisor into its U_RUN_RECONCILE_REQUIRED arm for a kill that was
// never attempted.
func TestShimRunKillRefusesWithoutPublishingAKillIntent(t *testing.T) {
	r := shimRunner(t, Config{})
	record := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Containment: ConfineContainmentAdvisory, ScopeIntegrity: ScopeAdvisory}
	if _, err := r.ledger.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
		t.Fatal(err)
	}
	killed, err := r.Kill(context.Background(), "RUN-1", false)
	if err == nil || !strings.Contains(err.Error(), "E_RUN_SCOPE_UNAVAILABLE") {
		t.Fatalf("kill err=%v record=%+v", err, killed)
	}
	for _, kind := range shimLedgerKinds(t, r) {
		if kind == "kill-intent" {
			t.Fatal("a refused kill published a durable intent nothing could ever satisfy")
		}
	}
}

// verifies: AIRA-129 requirement 2 — reconcile reaches no cgroup seam for a
// ci-shim record, and terminalises an abandoned one honestly.
//
// A stranded shim run really is LOST: the supervisor held the only reach
// (kill(-pgid)) and is gone, so no second party can observe or end it. What must
// not happen is a cgroup call against a record that names no cgroup, which
// shimPanicBackend turns into a failure rather than a silent no-op.
func TestShimRunReconcileTerminalisesWithoutTouchingTheBackend(t *testing.T) {
	r := shimRunner(t, Config{})
	record := RunRecord{
		SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning,
		Containment: ConfineContainmentAdvisory, ScopeIntegrity: ScopeAdvisory,
		OutputRefs: map[string]OutputRef{},
	}
	if _, err := r.ledger.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
		t.Fatal(err)
	}
	reconciled, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].Status != StatusLost {
		t.Fatalf("reconciled=%+v", reconciled)
	}
	if !containsPrefix(reconciled[0].ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("codes=%v", reconciled[0].ErrorCodes)
	}
}

// verifies: AIRA-129 — the real-slice path is untouched by the mode branch.
//
// This is the false-fail direction of the change: with the mode resolving to
// real-slice, Launch must still reach backend.Probe and fail closed there,
// exactly as it did before the shim path existed.
func TestRealModeStillProbesTheBackendBeforeLaunch(t *testing.T) {
	base := t.TempDir()
	r, err := New(Config{CommonDir: base, Backend: unavailableBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	r.resolveModeFn = func() string { return ConfineModeReal }
	marker := filepath.Join(base, "marker")
	if _, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "touch " + marker}}); err == nil ||
		!strings.Contains(err.Error(), "E_RUN_SCOPE_UNAVAILABLE") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("child launched despite scope failure: %v", statErr)
	}
}

// verifies: AIRA-129 — decideShimTimeoutIntentNotExecuted's conjuncts.
//
// Each row drops exactly one and must flip the answer to false. The row that
// matters most is the last: the real path's predicate also requires Kill.Empty,
// which ci-shim can never establish, so this predicate must not be a copy that
// quietly reinterprets that field.
func TestShimTimeoutIntentNotExecutedRequiresEveryConjunct(t *testing.T) {
	published := killAttempt{IntentPublished: true, IntentCreated: true}
	if !decideShimTimeoutIntentNotExecuted(nil, published, processDead) {
		t.Fatal("a published, self-created intent that emitted no signal against a dead leader is not-executed")
	}
	for name, args := range map[string]struct {
		err     error
		attempt killAttempt
		leader  processLiveness
	}{
		"errored kill is unevaluated":  {errors.New("x"), published, processDead},
		"intent not published":         {nil, killAttempt{IntentCreated: true}, processDead},
		"intent adopted, not created":  {nil, killAttempt{IntentPublished: true}, processDead},
		"a signal was actually sent":   {nil, killAttempt{IntentPublished: true, IntentCreated: true, Kill: killResult{Started: true}}, processDead},
		"leader still alive":           {nil, published, processAlive},
		"leader liveness unestablised": {nil, published, processUnknown},
	} {
		if decideShimTimeoutIntentNotExecuted(args.err, args.attempt, args.leader) {
			t.Fatalf("%s: must refuse the disposition", name)
		}
	}
}

// verifies: AIRA-129 requirement 1 — the forwarded-signal ORDER, which is the
// half of requirement 8 that the timeout test cannot reach.
//
// AIRA-121's gate condition C9 is the property under test: the received signal
// must be DELIVERED to the group first and the escalation grace started only
// after, so a job's own handler gets to run. The grandchild's TERM handler sleeps
// a measurable 0.3s before writing its marker, so an implementation that copied
// the real path's shape — kill the scope first, forward afterwards — would
// SIGKILL it mid-handler and the marker would never appear. So would a
// single-PID forward, which never reaches the grandchild at all.
//
// The signal is delivered through the injected source, so nothing is sent to the
// test binary itself.
func TestShimRunForwardsASignalToTheGroupAndLetsHandlersFinish(t *testing.T) {
	dir := t.TempDir()
	script := shimSignalScript(t, dir, false, false)
	events := make(chan os.Signal, 1)
	r := shimRunner(t, Config{})
	r.signalSourceFn = func() (<-chan os.Signal, func()) { return events, func() {} }
	go func() {
		if !waitForFile(t, filepath.Join(dir, "grandchild-ready"), 10*time.Second) {
			t.Errorf("grandchild never reported ready")
			return
		}
		events <- syscall.SIGTERM
	}()
	record, err := r.Launch(context.Background(), Request{Argv: []string{script}})
	if err != nil {
		t.Fatalf("err=%v record=%+v", err, record)
	}
	if record.Status != StatusExited {
		t.Fatalf("record=%+v", record)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "grandchild-term")); statErr != nil {
		t.Fatalf("the grandchild's own SIGTERM handler did not run to completion: %v", statErr)
	}
	if record.Containment != ConfineContainmentAdvisory || record.ScopeIntegrity != ScopeAdvisory {
		t.Fatalf("containment=%q scope-integrity=%q", record.Containment, record.ScopeIntegrity)
	}
}
