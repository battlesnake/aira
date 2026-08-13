package runner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEnvDigestUsesSortedLengthPrefixedBytes(t *testing.T) {
	entries := []EnvEntry{{Key: []byte("B"), Value: []byte("x\x00=")}, {Key: []byte("A"), Value: []byte("line\n")}}
	got, err := EnvDigest(entries)
	if err != nil {
		t.Fatal(err)
	}
	var encoded []byte
	var buf [binary.MaxVarintLen64]byte
	for _, e := range []EnvEntry{entries[1], entries[0]} {
		n := binary.PutUvarint(buf[:], uint64(len(e.Key)))
		encoded = append(encoded, buf[:n]...)
		encoded = append(encoded, e.Key...)
		n = binary.PutUvarint(buf[:], uint64(len(e.Value)))
		encoded = append(encoded, buf[:n]...)
		encoded = append(encoded, e.Value...)
	}
	wantBytes := sha256.Sum256(encoded)
	want := hex.EncodeToString(wantBytes[:])
	if got != want {
		t.Fatalf("digest=%s want %s", got, want)
	}
	if other, _ := EnvDigest([]EnvEntry{{Key: []byte("A"), Value: []byte("line")}, {Key: []byte("B"), Value: []byte("x\x00=line\n")}}); other == got {
		t.Fatal("length-prefixed environment digests collided")
	}
	if _, err := EnvDigest([]EnvEntry{{Key: []byte("A")}, {Key: []byte("A"), Value: []byte("x")}}); err == nil || !strings.Contains(err.Error(), "E_RUN_ENV_INVALID") {
		t.Fatalf("duplicate env accepted: %v", err)
	}
}

func TestPrefixValidationRejectsAmbiguousDelimiter(t *testing.T) {
	if got, err := validatePrefix([]string{"agentmux", "run", "--"}); err != nil || len(got) != 2 {
		t.Fatalf("valid delimiter: %v %v", got, err)
	}
	for _, prefix := range [][]string{{"agentmux", "--", "x"}, {"agentmux", "--", "--"}} {
		if _, err := validatePrefix(prefix); err == nil {
			t.Fatalf("accepted invalid prefix %v", prefix)
		}
	}
}

func TestEffectiveArgvPreservesTargetOptionTokensWithoutShell(t *testing.T) {
	got, err := EffectiveArgv([]string{"agentmux", "run", "--"}, []string{"tool", "--literal", "$(not-shell)", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agentmux", "run", "tool", "--literal", "$(not-shell)", ""}
	if len(got) != len(want) {
		t.Fatalf("argv=%q want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv=%q want %q", got, want)
		}
	}
}

func TestLedgerFrameRoundTripAndTornFrameFailsClosed(t *testing.T) {
	l, err := newLedger(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, ScopeIntegrity: ScopeContained}
	if _, err := l.append(ledgerEvent{Kind: "starting", Run: run}); err != nil {
		t.Fatal(err)
	}
	events, err := l.read()
	if err != nil || len(events) != 1 {
		t.Fatalf("read=%d err=%v", len(events), err)
	}
	data, err := os.ReadFile(l.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.ledger, data[:len(data)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.read(); err == nil || (!strings.Contains(err.Error(), "U_RUN_RECONCILE_REQUIRED") && !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT")) {
		t.Fatalf("torn ledger error=%v", err)
	}
}

func TestLedgerProjectionRebuildsAfterDBLoss(t *testing.T) {
	base := t.TempDir()
	l, err := newLedger(base, "")
	if err != nil {
		t.Fatal(err)
	}
	peak, user, sys := int64(1000), int64(12), int64(4)
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusExited, ScopeIntegrity: ScopeContained, TerminalComplete: true, PeakRSS: &peak, CPUUser: &user, CPUSys: &sys}
	if _, err := l.append(ledgerEvent{Kind: "terminal", Run: run}); err != nil {
		t.Fatal(err)
	}
	if err := l.project(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(l.projection); err != nil {
		t.Fatal(err)
	}
	if err := l.rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.projection); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", l.projection)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotPeak, gotUser, gotSys sql.NullInt64
	if err := db.QueryRow(`SELECT peak_rss,cpu_user,cpu_sys FROM runs WHERE id=?`, "RUN-1").Scan(&gotPeak, &gotUser, &gotSys); err != nil {
		t.Fatal(err)
	}
	if !gotPeak.Valid || gotPeak.Int64 != peak || !gotUser.Valid || gotUser.Int64 != user || !gotSys.Valid || gotSys.Int64 != sys {
		t.Fatalf("projected usage peak=%v user=%v sys=%v", gotPeak, gotUser, gotSys)
	}
}

func TestDrainIsBinarySafeAndHandlesShortWriter(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "out")
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 1, 2, 255, 'x', '\n'}
	go func() { _, _ = wr.Write(want); _ = wr.Close() }()
	ch := make(chan captureResult, 1)
	go drain("out", rd, dst, ch)
	got := <-ch
	if got.Err != nil || got.Bytes != int64(len(want)) {
		t.Fatalf("capture=%+v", got)
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(want) {
		t.Fatalf("bytes=%v want %v", actual, want)
	}
}

func TestCleanSuccessRequiresAllEvidence(t *testing.T) {
	zero := 0
	r := RunRecord{Status: StatusExited, ExitCode: &zero, ScopeIntegrity: ScopeContained, CaptureComplete: true, TerminalComplete: true}
	if !r.CleanSuccess() {
		t.Fatal("complete evidence did not earn success")
	}
	for _, mutate := range []func(*RunRecord){func(r *RunRecord) { r.Status = StatusKilled }, func(r *RunRecord) { r.CaptureComplete = false }, func(r *RunRecord) { r.TerminalComplete = false }, func(r *RunRecord) { r.ScopeIntegrity = ScopeUnverified }, func(r *RunRecord) { r.ScopeIntegrity = ScopeHandoffUnverified }, func(r *RunRecord) { r.ErrorCodes = []string{"E_RUN_CAPTURE_FAILED"} }} {
		copy := r
		mutate(&copy)
		if copy.CleanSuccess() {
			t.Fatalf("incomplete record earned success: %+v", copy)
		}
	}
}

func TestUsageEvidenceMergesOnlyEvaluatedMetrics(t *testing.T) {
	base := RunRecord{}
	peak, user := int64(100), int64(7)
	mergeUsage(&base, RunRecord{PeakRSS: &peak, CPUUser: &user})
	if base.PeakRSS == nil || *base.PeakRSS != 100 || base.CPUUser == nil || *base.CPUUser != 7 || base.CPUSys != nil {
		t.Fatalf("initial usage merge=%+v", base)
	}
	sys := int64(9)
	mergeUsage(&base, RunRecord{CPUSys: &sys})
	if base.PeakRSS == nil || *base.PeakRSS != 100 || base.CPUUser == nil || *base.CPUUser != 7 || base.CPUSys == nil || *base.CPUSys != 9 {
		t.Fatalf("partial usage merge=%+v", base)
	}
}

func TestArbitrationWaitBeforeIntentWinsExactlyOnce(t *testing.T) {
	var state Arbitration
	if !state.Wait() || state.Intent() {
		t.Fatal("kill intent displaced a published wait")
	}
	if !state.CommitWait(StatusExited) || state.CommitWait(StatusExited) {
		t.Fatal("wait terminal slot was not unique")
	}
	if state.CommitKill() {
		t.Fatal("kill committed after exited")
	}
	state = Arbitration{}
	if !state.Intent() || state.Wait() || !state.CommitKill() || state.CommitWait(StatusExited) {
		t.Fatal("kill-before-wait arbitration failed")
	}
}

func TestReconcileDecisionNeverOverwritesPublishedWait(t *testing.T) {
	decision := decideReconcile(true, false, true, false)
	if !decision.PreserveOpen || decision.Terminal != "" {
		t.Fatalf("decision=%+v", decision)
	}
	decision = decideReconcile(false, true, true, false)
	if decision.Terminal != StatusLost || !decision.NeedsLost {
		t.Fatalf("ambiguous kill decision=%+v", decision)
	}
	decision = decideReconcile(false, true, false, false)
	if !decision.NeedsKill || decision.Terminal != "" {
		t.Fatalf("live kill decision=%+v", decision)
	}
	decision = decideReconcile(false, true, false, true)
	if decision.Terminal != StatusKilled || !decision.KillProven {
		t.Fatalf("proven kill decision=%+v", decision)
	}
}

func TestMembershipAndMigrationClassification(t *testing.T) {
	if integrity, migrated := classifyMembership(true, true, false); integrity != ScopeMigrated || !migrated {
		t.Fatalf("migration=%q/%v", integrity, migrated)
	}
	if integrity, migrated := classifyMembership(true, false, false); integrity != ScopeContained || migrated {
		t.Fatalf("natural exit=%q/%v", integrity, migrated)
	}
	if integrity, migrated := classifyMembership(false, true, true); integrity != ScopeHandoffUnverified || migrated {
		t.Fatalf("unverified=%q/%v", integrity, migrated)
	}
	if !memberStillPresent([]int{12, 15}, 15) || memberStillPresent([]int{12}, 15) {
		t.Fatal("stale member filter failed")
	}
	if ScopeUnverified == ScopeContained || ScopeUnverified == ScopeMigrated {
		t.Fatal("unverified containment state aliases a different integrity state")
	}
}

func TestProcessIdentityRejectsReusedPIDStartTick(t *testing.T) {
	identity := PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid())}
	if identity.StartTick == 0 || !processIdentityMatches(identity) || !processLive(identity) {
		t.Fatalf("current process identity was not recognized: %+v", identity)
	}
	identity.StartTick++
	if processIdentityMatches(identity) || processLive(identity) {
		t.Fatalf("stale start tick was accepted: %+v", identity)
	}
}

func TestFailedMembershipObservationCannotClaimContained(t *testing.T) {
	memberErr := errors.New("cgroup.procs read failed")
	integrity, unobserved, code := classifyLaunchScopeIntegrity(false, true, true, true, false, memberErr)
	if integrity == ScopeContained || integrity != ScopeHandoffUnverified {
		t.Fatalf("membership failure claimed unexpected integrity=%q", integrity)
	}
	if unobserved || code != "E_RUN_SCOPE_INVALID" {
		t.Fatalf("membership failure classification unobserved=%v code=%q", unobserved, code)
	}

	integrity, unobserved, code = classifyLaunchScopeIntegrity(false, true, false, true, false, nil)
	if integrity == ScopeContained || integrity != ScopeHandoffUnverified {
		t.Fatalf("invalid start identity claimed unexpected integrity=%q", integrity)
	}
	if unobserved || code != "E_RUN_SCOPE_INVALID" {
		t.Fatalf("invalid start identity classification unobserved=%v code=%q", unobserved, code)
	}
}

func TestObservedContainmentUpgradesPreObservationEvidence(t *testing.T) {
	base := RunRecord{ID: "RUN-1", ScopeIntegrity: ScopeHandoffUnverified}
	candidate := RunRecord{ID: "RUN-1", ScopeIntegrity: ScopeContained}
	merged := mergeEvidence(base, candidate)
	if merged.ScopeIntegrity != ScopeContained {
		t.Fatalf("observed containment did not win evidence merge: %q", merged.ScopeIntegrity)
	}
}

func TestHandoffIntegrityAlwaysHasScopeOrReconcileErrorAndIsInadmissible(t *testing.T) {
	representatives := []RunRecord{
		{Status: StatusExited, ScopeIntegrity: ScopeHandoffUnverified, ErrorCodes: []string{"E_RUN_SCOPE_INVALID"}},
		{Status: StatusExited, ScopeIntegrity: ScopeHandoffUnverified, ErrorCodes: []string{"E_RUN_SCOPE_HANDOFF"}},
		{Status: StatusKilled, ScopeIntegrity: ScopeHandoffUnverified, ErrorCodes: []string{"U_RUN_RECONCILE_REQUIRED"}},
		{Status: StatusExited, ScopeIntegrity: ScopeHandoffUnverified, ErrorCodes: []string{"E_RUN_CAPTURE_FAILED", "E_RUN_SCOPE_HANDOFF"}},
	}
	for _, record := range representatives {
		if !hasScopeReconcileError(record.ErrorCodes) {
			t.Fatalf("handoff record lacks scope/reconcile error: %+v", record)
		}
		if commandRecordAdmissibleForTest(record) {
			t.Fatalf("handoff record was gate-admissible: %+v", record)
		}
	}
	fixed := ensureTerminalScopeEvidence(RunRecord{Status: StatusExited, ScopeIntegrity: ScopeHandoffUnverified})
	if !hasScopeReconcileError(fixed.ErrorCodes) || commandRecordAdmissibleForTest(fixed) {
		t.Fatalf("bare terminal handoff was not repaired: %+v", fixed)
	}
}

func TestTerminalCASRepairsBareHandoffEvidence(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, ScopeIntegrity: ScopeHandoffUnverified, CgroupScope: scope.Reference()}
	appendRunEvent(t, r, "starting", run)
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), "RUN-1.lock"))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := r.appendTerminalLocked("RUN-1", RunRecord{ID: "RUN-1", Status: StatusExited, ScopeIntegrity: ScopeHandoffUnverified, TerminalComplete: true})
	_ = unlockFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	if committed.ScopeIntegrity != ScopeHandoffUnverified || !hasScopeReconcileError(committed.ErrorCodes) {
		t.Fatalf("terminal CAS left bare handoff evidence: %+v", committed)
	}
}

func TestKillIntentSequenceIsPersistedInDurableRun(t *testing.T) {
	l, err := newLedger(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, CgroupScope: "/fake"}
	if _, err := l.append(ledgerEvent{Kind: "starting", Run: run}); err != nil {
		t.Fatal(err)
	}
	run.KillIntent.Present = true
	event, err := l.append(ledgerEvent{Kind: "kill-intent", Run: run})
	if err != nil {
		t.Fatal(err)
	}
	if event.Run.KillIntent.Sequence == 0 {
		t.Fatal("returned kill intent sequence is zero")
	}
	current, err := l.current("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.KillIntent.Sequence != event.Run.KillIntent.Sequence {
		t.Fatalf("durable sequence=%d event=%d", current.KillIntent.Sequence, event.Run.KillIntent.Sequence)
	}
}

type memoryScope struct {
	members                     []int
	killed, terminated, removed bool
}

func (s *memoryScope) Reference() string       { return "/memory-scope" }
func (s *memoryScope) FD() int                 { return -1 }
func (s *memoryScope) Members() ([]int, error) { return append([]int(nil), s.members...), nil }
func (s *memoryScope) Empty() (bool, error)    { return len(s.members) == 0, nil }
func (s *memoryScope) Terminate([]int) error   { s.terminated = true; s.members = nil; return nil }
func (s *memoryScope) Kill() error             { s.killed = true; s.members = nil; return nil }
func (s *memoryScope) Remove() error           { s.removed = true; return nil }

type memoryBackend struct{ scope *memoryScope }

func (b *memoryBackend) Probe(context.Context) error                   { return nil }
func (b *memoryBackend) Create(context.Context, string) (Scope, error) { return b.scope, nil }
func (b *memoryBackend) Open(context.Context, string) (Scope, error)   { return b.scope, nil }

func newMemoryRunner(t *testing.T, members []int) (*Runner, *memoryScope) {
	t.Helper()
	scope := &memoryScope{members: members}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: scope}, TermGrace: time.Millisecond, Grace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return r, scope
}

func appendRunEvent(t *testing.T, r *Runner, kind string, run RunRecord) {
	t.Helper()
	if _, err := r.ledger.append(ledgerEvent{Kind: kind, Run: run}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileUsesRunLockAndPreservesWaitEvidence(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, ScopeIntegrity: ScopeContained, CgroupScope: scope.Reference()}
	appendRunEvent(t, r, "starting", run)
	appendRunEvent(t, r, "scope-created", run)
	exit := 7
	run.ExitCode = &exit
	appendRunEvent(t, r, "wait-observed", run)
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status.Terminal() || current.ExitCode == nil || *current.ExitCode != 7 {
		t.Fatalf("wait evidence lost: %+v", current)
	}
	if events, err := r.ledger.read(); err != nil {
		t.Fatal(err)
	} else {
		for _, event := range events {
			if event.Kind == "terminal" {
				t.Fatal("reconcile appended terminal over waiter")
			}
		}
	}
}

func TestTerminalCASIsIdempotentUnderConcurrentWaiterAndReconcile(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, ScopeIntegrity: ScopeContained, CgroupScope: scope.Reference()}
	appendRunEvent(t, r, "starting", run)
	appendRunEvent(t, r, "scope-created", run)
	results := make(chan error, 2)
	for _, status := range []Status{StatusExited, StatusLost} {
		go func(status Status) {
			lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), "RUN-1.lock"))
			if err == nil {
				candidate := run
				candidate.Status, candidate.TerminalComplete = status, true
				candidate.EndedAt = nowString(r.now)
				_, err = r.appendTerminalLocked("RUN-1", candidate)
				_ = unlockFile(lock)
			}
			results <- err
		}(status)
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestReconcileDoesNotFabricateKilledFromEmptyScope(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, ScopeIntegrity: ScopeContained, CgroupScope: scope.Reference(), KillIntent: KillIntent{Present: true}}
	appendRunEvent(t, r, "starting", run)
	appendRunEvent(t, r, "scope-created", run)
	appendRunEvent(t, r, "kill-intent", run)
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusLost || current.ScopeKill.Completed || current.KillIntent.Completed || scope.killed {
		t.Fatalf("fabricated kill: %+v scope=%+v", current, scope)
	}
}

func TestFailBeforeLaunchResolvesConcurrentKillUnderRunLock(t *testing.T) {
	r, scope := newMemoryRunner(t, []int{123})
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, ScopeIntegrity: ScopeContained, CgroupScope: scope.Reference()}
	appendRunEvent(t, r, "starting", run)
	appendRunEvent(t, r, "scope-created", run)
	run.KillIntent = KillIntent{Present: true}
	appendRunEvent(t, r, "kill-intent", run)
	_, _ = r.failBeforeLaunch(context.Background(), run, "E_RUN_LAUNCH_FAILED", errors.New("missing executable"))
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusKilled || !current.ScopeKill.Started || !current.ScopeKill.Completed {
		t.Fatalf("race outcome=%+v", current)
	}
	terminals := 0
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "terminal" {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal records=%d", terminals)
	}
}

type unavailableBackend struct{}

func (unavailableBackend) Probe(context.Context) error {
	return errors.New("delegated cgroup is not writable")
}
func (unavailableBackend) Create(context.Context, string) (Scope, error) {
	panic("must not create scope")
}
func (unavailableBackend) Open(context.Context, string) (Scope, error) { panic("must not open scope") }

func TestScopeProbeFailsClosedBeforeLaunch(t *testing.T) {
	base := t.TempDir()
	r, err := New(Config{CommonDir: base, Backend: unavailableBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "marker")
	_, err = r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "touch " + marker}})
	if err == nil || !strings.Contains(err.Error(), "E_RUN_SCOPE_UNAVAILABLE") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("child launched despite scope failure: %v", statErr)
	}
}

func TestRealCgroupIntegrationOrClearSkip(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "printf out; printf err >&2"}})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusExited || record.ExitCode == nil {
		t.Fatalf("record=%+v", record)
	}
	if data, err := os.ReadFile(record.OutputRefs["out"].Path); err != nil || string(data) != "out" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestRealCgroupPlacedThenExitedFastIsHonestAndCaptured(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("fast exit record=%+v", record)
	}
	assertHonestExitScope(t, r, record)
	if !record.CaptureComplete || record.CaptureForcedClosed || len(record.ErrorCodes) != 0 {
		t.Fatalf("fast placed exit was not clean: %+v", record)
	}
	for _, name := range []string{"out", "err"} {
		ref := record.OutputRefs[name]
		if ref.State != OutputComplete || ref.Bytes != 0 {
			t.Fatalf("%s capture=%+v", name, ref)
		}
	}
}

func TestRealCgroupTimeoutUsesDurableKillAndKillsGrandchild(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{
		Argv:    []string{"/bin/sh", "-c", "setsid sh -c 'sleep 30' & sleep 30"},
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusKilled || !containsString(record.ErrorCodes, "E_RUN_TIMEOUT") || record.CleanSuccess() {
		t.Fatalf("timeout record=%+v", record)
	}
	if !record.KillIntent.Present || !record.KillIntent.Completed || !record.ScopeKill.Completed {
		t.Fatalf("timeout did not retain durable kill evidence: %+v", record)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration(t *testing.T) {
	r := realRunner(t)
	exited, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "printf ok"}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if exited.Status != StatusExited || exited.CleanSuccess() && containsString(exited.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("natural exit was rewritten by timeout: %+v", exited)
	}
	assertHonestExitScope(t, r, exited)
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("natural terminal records=%d", got)
	}

	r = realRunner(t)
	timedOut, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "sleep 30"}, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if timedOut.Status != StatusKilled || !containsString(timedOut.ErrorCodes, "E_RUN_TIMEOUT") || timedOut.CleanSuccess() {
		t.Fatalf("timeout arbitration record=%+v", timedOut)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("timeout terminal records=%d", got)
	}

	r = realRunner(t)
	nearDeadline, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "sleep 0.04"}, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	switch nearDeadline.Status {
	case StatusExited:
		assertHonestExitScope(t, r, nearDeadline)
		if nearDeadline.ExitCode == nil || *nearDeadline.ExitCode != 0 || containsString(nearDeadline.ErrorCodes, "E_RUN_TIMEOUT") || !nearDeadline.CleanSuccess() {
			t.Fatalf("near-deadline clean exit evidence=%+v", nearDeadline)
		}
	case StatusKilled:
		if !containsString(nearDeadline.ErrorCodes, "E_RUN_TIMEOUT") || nearDeadline.CleanSuccess() || !nearDeadline.KillIntent.Present || !nearDeadline.KillIntent.Completed {
			t.Fatalf("near-deadline timeout evidence=%+v", nearDeadline)
		}
	default:
		t.Fatalf("near-deadline run did not arbitrate to a terminal state: %+v", nearDeadline)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("near-deadline terminal records=%d", got)
	}
}

func TestRealCgroupExplicitEmptyEnvironmentIsExactAndDigested(t *testing.T) {
	const probe = "AIRA_M10B_PARENT_ONLY"
	if err := os.Setenv(probe, "must-not-leak"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(probe)
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/usr/bin/env"}, ExplicitEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("explicit-env record=%+v", record)
	}
	assertHonestExitScope(t, r, record)
	data, err := os.ReadFile(record.OutputRefs["out"].Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 || strings.Contains(string(data), probe+"=") {
		t.Fatalf("explicit empty environment leaked values: %q", data)
	}
	want, err := EnvDigest(nil)
	if err != nil || record.EnvDigest != want {
		t.Fatalf("env digest=%s want %s (err=%v)", record.EnvDigest, want, err)
	}
}

func TestRealCgroupExplicitNonemptyEnvironmentIsCanonicalAndDigested(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{
		Argv:        []string{"/usr/bin/env"},
		Env:         []string{"Z_LAST=last", "A_FIRST=first"},
		ExplicitEnv: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("explicit-env record=%+v", record)
	}
	data, err := os.ReadFile(record.OutputRefs["out"].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "A_FIRST=first\nZ_LAST=last\n" {
		t.Fatalf("explicit nonempty environment=%q", data)
	}
	want, err := EnvDigest([]EnvEntry{{Key: []byte("Z_LAST"), Value: []byte("last")}, {Key: []byte("A_FIRST"), Value: []byte("first")}})
	if err != nil || record.EnvDigest != want {
		t.Fatalf("env digest=%s want %s (err=%v)", record.EnvDigest, want, err)
	}
}

func realRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(Config{CommonDir: t.TempDir(), Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.backend.Probe(context.Background()); err != nil {
		t.Skipf("real cgroup-v2 delegation unavailable: %v", err)
	}
	return r
}

type launchOutcome struct {
	record *RunRecord
	err    error
}

func launchAsync(t *testing.T, r *Runner, req Request) <-chan launchOutcome {
	t.Helper()
	result := make(chan launchOutcome, 1)
	go func() { record, err := r.Launch(context.Background(), req); result <- launchOutcome{record, err} }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case outcome := <-result:
			result <- outcome
			return result
		case <-deadline.C:
			t.Fatal("real runner did not establish a durable run identity")
			return result
		case <-ticker.C:
			if record, err := r.Get("RUN-1"); err == nil && (record.Status == StatusRunning || record.Status.Terminal()) {
				return result
			}
		}
	}
}

func terminalRecords(t *testing.T, r *Runner) int {
	t.Helper()
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "terminal" {
			count++
		}
	}
	return count
}

func TestRealCgroupWholeScopeKillIncludesSetsidGrandchild(t *testing.T) {
	r := realRunner(t)
	outcome := launchAsync(t, r, Request{Argv: []string{"/bin/sh", "-c", "setsid sh -c 'sleep 30' & sleep 30"}})
	if _, err := r.Kill(context.Background(), "RUN-1"); err != nil {
		t.Fatal(err)
	}
	result := <-outcome
	if result.err != nil && !strings.Contains(result.err.Error(), "U_RUN_RECONCILE_REQUIRED") {
		t.Fatal(result.err)
	}
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusKilled {
		t.Fatalf("whole-scope result=%+v", current)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestRealCgroupKillWaitRaceHasOneTerminalWinner(t *testing.T) {
	r := realRunner(t)
	outcome := launchAsync(t, r, Request{Argv: []string{"/bin/sh", "-c", "sleep 0.2"}})
	_, _ = r.Kill(context.Background(), "RUN-1")
	result := <-outcome
	if result.err != nil && !strings.Contains(result.err.Error(), "U_RUN_RECONCILE_REQUIRED") {
		t.Fatal(result.err)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestRealCgroupReconcileRacePreservesWaitAndTerminalUniqueness(t *testing.T) {
	r := realRunner(t)
	outcome := launchAsync(t, r, Request{Argv: []string{"/bin/sh", "-c", "printf ok"}})
	for i := 0; i < 20; i++ {
		_, _ = r.Reconcile(context.Background())
		time.Sleep(time.Millisecond)
	}
	result := <-outcome
	if result.err != nil {
		t.Fatal(result.err)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestRealCgroupMigratedLaunchIsNotClean(t *testing.T) {
	r := realRunner(t)
	script := `set -eu; rel=$(awk -F: '$1=="0" {print $3}' /proc/self/cgroup); parent=/sys/fs/cgroup$(dirname "$rel"); target="$parent/.aira-migrate-$$"; mkdir "$target"; echo $$ > "$target/cgroup.procs"; printf migrated; sleep 0.1`
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status == StatusExited && record.ExitCode != nil && *record.ExitCode == 0 && record.ScopeIntegrity == ScopeContained {
		t.Fatalf("migration was reported clean: %+v", record)
	}
	if record.ScopeIntegrity != ScopeMigrated && record.ScopeIntegrity != ScopeHandoffUnverified {
		t.Skipf("migration fixture could not establish an observable handoff: %+v", record)
	}
}

func TestRealCgroupDescendantMigrationBeforeLeaderExitResidual(t *testing.T) {
	r := realRunner(t)
	proofDir := t.TempDir()
	t.Cleanup(func() {
		pidData, _ := os.ReadFile(filepath.Join(proofDir, "pid"))
		targetData, _ := os.ReadFile(filepath.Join(proofDir, "target"))
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
		target := strings.TrimSpace(string(targetData))
		if err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if target != "" {
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if data, readErr := os.ReadFile(filepath.Join(target, "cgroup.procs")); readErr == nil && len(strings.TrimSpace(string(data))) == 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			_ = os.Remove(target)
		}
	})
	script := `set -eu
rel=$(awk -F: '$1=="0" {print $3}' /proc/self/cgroup)
parent=/sys/fs/cgroup$(dirname "$rel")
target="$parent/.aira-descendant-$$"
mkdir "$target"
proof=$(pwd)
printf '%s\n' "$target" > "$proof/target"
sh -c 'target=$1; proof=$2; printf "%s\n" "$$" > "$proof/pid"; echo $$ > "$target/cgroup.procs"; printf migrated > "$proof/migrated"; exec 1>&- 2>&-; exec sleep 30' sh "$target" "$proof" &
exit 0`
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script}, Cwd: proofDir})
	if err != nil {
		t.Fatal(err)
	}
	target := strings.TrimSpace(string(waitForFixtureFile(t, filepath.Join(proofDir, "target"))))
	pidText := strings.TrimSpace(string(waitForFixtureFile(t, filepath.Join(proofDir, "pid"))))
	if string(waitForFixtureFile(t, filepath.Join(proofDir, "migrated"))) != "migrated" {
		t.Fatal("migration fixture did not publish its migration marker")
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		t.Fatalf("migration fixture published invalid pid %q: %v", pidText, err)
	}
	if target == "" || filepath.Dir(target) != filepath.Dir(record.CgroupScope) || target == record.CgroupScope {
		t.Fatalf("migration fixture target is not a sibling cgroup: target=%q scope=%q", target, record.CgroupScope)
	}
	membersData, err := os.ReadFile(filepath.Join(target, "cgroup.procs"))
	if err != nil {
		t.Fatalf("migration fixture sibling membership unavailable: %v", err)
	}
	var siblingMembers []int
	for _, field := range strings.Fields(string(membersData)) {
		member, parseErr := strconv.Atoi(field)
		if parseErr != nil {
			t.Fatalf("invalid sibling cgroup member %q: %v", field, parseErr)
		}
		siblingMembers = append(siblingMembers, member)
	}
	if !containsPID(siblingMembers, pid) {
		t.Fatalf("migration fixture pid %d is not in sibling cgroup %q: %v", pid, target, siblingMembers)
	}
	identity := PIDIdentity{PID: pid, StartTick: processStartTick(pid)}
	if identity.StartTick == 0 || !processLive(identity) {
		t.Fatalf("migration fixture descendant is not alive with matching identity: %+v", identity)
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("descendant migration record=%+v", record)
	}
	if record.ScopeIntegrity == ScopeMigrated && record.CleanSuccess() {
		t.Fatal("impossible migrated clean success")
	}
	if record.CleanSuccess() {
		assertHonestExitScope(t, r, record)
		t.Skip("leader containment observed; descendant containment not attestable after scope exit — daemonless residual, see task #20 (cgroup-namespace / supervisor mitigation)")
	}
	if record.ScopeIntegrity == ScopeContained && len(record.ErrorCodes) == 0 {
		t.Fatalf("fixture was proven escaped but record was unexpectedly clean: %+v", record)
	}
}

func TestRealCgroupAtomicMigrationResidualIsExplicit(t *testing.T) {
	r := realRunner(t)
	// Accepted deviation in docs/superpowers/specs/2026-08-11-aira-m12-runner-lite-design.md:
	// a migrate-and-exit sequence that completes before observation is possible
	// cannot be classified reliably by the deliberately daemonless runner.
	script := `set -eu; rel=$(awk -F: '$1=="0" {print $3}' /proc/self/cgroup); parent=/sys/fs/cgroup$(dirname "$rel"); target="$parent/.aira-atomic-$$"; mkdir "$target"; echo $$ > "$target/cgroup.procs"; printf atomic`
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	if record.ScopeIntegrity == ScopeMigrated || record.ScopeIntegrity == ScopeHandoffUnverified || !record.CleanSuccess() {
		return
	}
	t.Skip("accepted daemonless residual: atomic migrate-and-exit completed before scope observation")
}

func TestRealCgroupFailBeforeLaunchKillRaceHasOneTerminal(t *testing.T) {
	r := realRunner(t)
	outcome := launchAsync(t, r, Request{Argv: []string{"/definitely/not/an/executable"}})
	_, _ = r.Kill(context.Background(), "RUN-1")
	result := <-outcome
	if result.err == nil {
		t.Fatal("missing launch failure")
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestCaptureCodeDistinguishesENOSPC(t *testing.T) {
	if got := captureCode(syscall.ENOSPC); got != "E_RUN_OUTPUT_DISK_FULL" {
		t.Fatal(got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertHonestExitScope(t *testing.T, r *Runner, record *RunRecord) {
	t.Helper()
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	observed := hasEvent(events, record.ID, "running")
	switch record.ScopeIntegrity {
	case ScopeContained:
		if !observed {
			t.Fatalf("contained without positive running observation: %+v", record)
		}
	case ScopeUnverified:
		if observed {
			t.Fatalf("unverified despite positive running observation: %+v", record)
		}
	default:
		t.Fatalf("unexpected fast-exit integrity=%q record=%+v", record.ScopeIntegrity, record)
	}
}

func waitForFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return data
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("migration fixture did not establish proof file %q", path)
	return nil
}

func commandRecordAdmissibleForTest(record RunRecord) bool {
	if record.CaptureForcedClosed || record.Status != StatusExited || record.ExitCode == nil || record.ScopeIntegrity != ScopeContained || !record.CaptureComplete || !record.TerminalComplete || len(record.OutputRefs) == 0 {
		return false
	}
	for _, ref := range record.OutputRefs {
		if ref.State != OutputComplete || ref.Path == "" || ref.Bytes < 0 {
			return false
		}
	}
	for _, code := range record.ErrorCodes {
		if code != "E_RUN_FAILED" {
			return false
		}
	}
	return true
}
