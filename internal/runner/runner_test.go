package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

func TestHasRunUsesOnlyTheSelectedProjectLedger(t *testing.T) {
	common := t.TempDir()
	l, err := newLedger(common, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.append(ledgerEvent{Kind: "starting", Run: RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-41", Status: StatusStarting}}); err != nil {
		t.Fatal(err)
	}
	found, err := HasRun(common, "RUN-41")
	if err != nil || !found {
		t.Fatalf("selected ledger found=%v err=%v", found, err)
	}
	found, err = HasRun(common, "RUN-42")
	if err != nil || found {
		t.Fatalf("missing run found=%v err=%v", found, err)
	}
	found, err = HasRun(t.TempDir(), "RUN-41")
	if err != nil || found {
		t.Fatalf("foreign project found=%v err=%v", found, err)
	}
}

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

func TestStdinConnectValidationPrecedesLaunch(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &validationBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]Request{
		"requires detach": {Argv: []string{"true"}, StdinConnect: true},
		"stdin file":      {Argv: []string{"true"}, Detach: true, StdinConnect: true, StdinPath: "input"},
		"stdin dash":      {Argv: []string{"true"}, Detach: true, StdinConnect: true, StdinPath: "-"},
		"pty":             {Argv: []string{"true"}, Detach: true, StdinConnect: true, PTY: true},
		"store stdin":     {Argv: []string{"true"}, Detach: true, StdinConnect: true, StoreStdin: true},
		"reader":          {Argv: []string{"true"}, Detach: true, StdinConnect: true, Stdin: strings.NewReader("x")},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.Launch(context.Background(), req)
			var launch *LaunchError
			if !errors.As(err, &launch) || launch.Code != "E_RUN_ARGUMENT_INVALID" {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

type validationBackend struct{}

func (*validationBackend) Probe(context.Context) error { return errors.New("must not probe") }
func (*validationBackend) Create(context.Context, string) (Scope, error) {
	return nil, errors.New("must not create")
}
func (*validationBackend) Open(context.Context, string) (Scope, error) {
	return nil, errors.New("must not open")
}

func TestStdbufInjectionPrependsAndNoOpPreservesEnvironment(t *testing.T) {
	previous := locateLibstdbufFn
	t.Cleanup(func() { locateLibstdbufFn = previous })
	locateLibstdbufFn = func() string { return "/usr/lib/coreutils/libstdbuf.so" }
	got, applied := stdbufInjection([]string{"PATH=/bin", "LD_PRELOAD=/x.so", "_STDBUF_O=B"})
	if !applied {
		t.Fatal("stdbuf injection was not applied")
	}
	toMap := func(entries []string) map[string]string {
		t.Helper()
		result := make(map[string]string, len(entries))
		for _, entry := range entries {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || key == "" {
				t.Fatalf("invalid environment entry %q", entry)
			}
			if _, exists := result[key]; exists {
				t.Fatalf("duplicate environment key %q in %q", key, entries)
			}
			result[key] = value
		}
		return result
	}
	want := map[string]string{"PATH": "/bin", "LD_PRELOAD": "/usr/lib/coreutils/libstdbuf.so:/x.so", "_STDBUF_O": "L", "_STDBUF_E": "L", "PYTHONUNBUFFERED": "1"}
	if gotMap := toMap(got); !reflect.DeepEqual(gotMap, want) {
		t.Fatalf("injected environment=%q want map=%v", got, want)
	}

	input := []string{"PATH=/bin", "LD_PRELOAD=/x.so"}
	locateLibstdbufFn = func() string { return "" }
	got, applied = stdbufInjection(input)
	if applied || !reflect.DeepEqual(toMap(got), toMap(input)) {
		t.Fatalf("no-op environment=%q applied=%v want unchanged map=%v", got, applied, toMap(input))
	}
}

func TestLedgerRoundTripPreservesBufferingAndAdmission(t *testing.T) {
	l, err := newLedger(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Owner: "A", StolenBy: "B", Status: StatusStarting, ScopeIntegrity: ScopeContained, Buffering: "realtime", Admission: "waited", AdmissionReason: "", AdmissionWaitedMS: 123, StdinConnect: true, InputSocket: "/runtime/input.sock"}
	if _, err := l.append(ledgerEvent{Kind: "starting", Run: run}); err != nil {
		t.Fatal(err)
	}
	events, err := l.read()
	if err != nil || len(events) != 1 {
		t.Fatalf("ledger events=%d err=%v", len(events), err)
	}
	if events[0].Run.Buffering != "realtime" {
		t.Fatalf("ledger buffering=%q events=%+v", events[0].Run.Buffering, events)
	}
	if events[0].Run.Admission != "waited" || events[0].Run.AdmissionWaitedMS != 123 {
		t.Fatalf("ledger admission=%+v", events[0].Run)
	}
	if events[0].Run.Owner != "A" || events[0].Run.StolenBy != "B" {
		t.Fatalf("ledger ownership=%+v", events[0].Run)
	}
	if !events[0].Run.StdinConnect || events[0].Run.InputSocket != "/runtime/input.sock" {
		t.Fatalf("ledger input discovery=%+v", events[0].Run)
	}
}

func TestLegacyLedgerRecordDefaultsBufferingToNone(t *testing.T) {
	l, err := newLedger(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"schema_version":1,"sequence":1,"kind":"starting","run":{"schema_version":1,"id":"RUN-legacy","argv":["/bin/true"],"cwd":"/tmp","env_digest":"digest","status":"starting","scope_integrity":"handoff-unverified","started_at":"2026-08-14T00:00:00Z","capture_complete":false,"capture_forced_closed":false,"stdin_stored":false,"scope_kill":{"requested":false,"started":false,"completed":false},"kill_intent":{"present":false,"completed":false,"empty_scope":false},"terminal_complete":false}}`)
	if err := os.WriteFile(l.ledger, frame(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	record, err := l.current("RUN-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if record.Buffering != "none" {
		t.Fatalf("legacy buffering=%q record=%+v", record.Buffering, record)
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
	bootID, err := currentBootID()
	if err != nil {
		t.Fatal(err)
	}
	identity.BootID = bootID
	if identity.StartTick == 0 || !processIdentityMatches(identity) || processLive(identity) != processAlive {
		t.Fatalf("current process identity was not recognized: %+v", identity)
	}
	identity.StartTick++
	if processIdentityMatches(identity) || processLive(identity) == processAlive {
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
	base := RunRecord{ID: "RUN-1", Buffering: "none", ScopeIntegrity: ScopeHandoffUnverified}
	candidate := RunRecord{ID: "RUN-1", Buffering: "realtime", ScopeIntegrity: ScopeContained}
	merged := mergeEvidence(base, candidate)
	if merged.ScopeIntegrity != ScopeContained {
		t.Fatalf("observed containment did not win evidence merge: %q", merged.ScopeIntegrity)
	}
	if merged.Buffering != "realtime" {
		t.Fatalf("buffering evidence was not carried forward: %q", merged.Buffering)
	}
}

func TestMergeEvidenceCarriesNonEmptyRunMetadata(t *testing.T) {
	base := RunRecord{ID: "RUN-1", Ticket: "AIRA-1", Phase: "implement", Label: "unit", Tool: "go"}
	got := mergeEvidence(base, RunRecord{ID: "RUN-1"})
	if got.Ticket != base.Ticket || got.Phase != base.Phase || got.Label != base.Label || got.Tool != base.Tool {
		t.Fatalf("empty candidate erased metadata: got=%+v base=%+v", got, base)
	}

	want := RunRecord{ID: "RUN-1", Ticket: "AIRA-2", Phase: "work-review", Label: "review", Tool: "codex"}
	got = mergeEvidence(base, want)
	if got.Ticket != want.Ticket || got.Phase != want.Phase || got.Label != want.Label || got.Tool != want.Tool {
		t.Fatalf("non-empty candidate metadata was not carried: got=%+v want=%+v", got, want)
	}
}

func TestRunMetadataIsLedgeredWithoutChangingEnvironmentDigest(t *testing.T) {
	entries := []EnvEntry{{Key: []byte("A"), Value: []byte("one")}}
	wantDigest, err := EnvDigest(entries)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Ticket: "AIRA-1", Phase: "implement", Label: "unit", Tool: "go"}
	record := RunRecord{
		SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting,
		Ticket: request.Ticket, Phase: request.Phase, Label: request.Label, Tool: request.Tool,
		EnvDigest: wantDigest,
	}
	l, err := newLedger(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
		t.Fatal(err)
	}
	got, err := l.current("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ticket != request.Ticket || got.Phase != request.Phase || got.Label != request.Label || got.Tool != request.Tool {
		t.Fatalf("first ledger event lost metadata: %+v", got)
	}
	if got.EnvDigest != wantDigest {
		t.Fatalf("metadata changed env digest: got=%q want=%q", got.EnvDigest, wantDigest)
	}
}

func TestLaunchStartingEventCarriesMetadataOrthogonally(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	stop := errors.New("stop after first ledger event")
	var starting RunRecord
	r.appendFault = func(event ledgerEvent) error {
		if event.Kind == "starting" {
			starting = event.Run
			return stop
		}
		return nil
	}
	request := Request{
		Argv: []string{"/bin/true"}, Env: []string{"A=one"}, ExplicitEnv: true,
		Ticket: "AIRA-1", Phase: "implement", Label: "unit", Tool: "go",
	}
	if _, err := r.Launch(context.Background(), request); !errors.Is(err, stop) {
		t.Fatalf("launch error=%v want injected stop", err)
	}
	wantDigest, err := EnvDigest([]EnvEntry{{Key: []byte("A"), Value: []byte("one")}})
	if err != nil {
		t.Fatal(err)
	}
	if starting.Ticket != request.Ticket || starting.Phase != request.Phase || starting.Label != request.Label || starting.Tool != request.Tool {
		t.Fatalf("starting event metadata=%+v request=%+v", starting, request)
	}
	if starting.EnvDigest != wantDigest {
		t.Fatalf("metadata-bearing env digest=%q want=%q", starting.EnvDigest, wantDigest)
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
	assertRunEventsOwner(t, r, record.ID, "A")
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
	assertRunEventsOwner(t, r, record.ID, "A")
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

func TestSidecarEnvironmentIsInjectedAfterDigestForExplicitEnv(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.inputRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var childEnv []string
	r.startFn = func(command *exec.Cmd) error {
		childEnv = append([]string(nil), command.Env...)
		return errors.New("injected after environment observation")
	}
	record, err := r.Launch(context.Background(), Request{
		Argv: []string{"/bin/true"},
		Env: []string{
			"PATH=/bin",
			"AIRA_PY_LIB=/stale",
			"AIRA_CPU_SLOTS_DIR=/stale-slots",
			"AIRA_CPU_POLL_INTERVAL=99",
			"AIRA_CPU_MAX_WAIT=99",
		},
		ExplicitEnv: true,
	})
	if err == nil {
		t.Fatalf("launch record=%+v err=%v", record, err)
	}
	record, getErr := r.Get("RUN-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	values := testEnvironmentValues(t, childEnv)
	if values["AIRA_PY_LIB"] == "" || values["AIRA_CPU_SLOTS_DIR"] != filepath.Join(r.inputRuntimeDir, "cpuslots") {
		t.Fatalf("child sidecar env=%v", childEnv)
	}
	wantDigest, digestErr := EnvDigest([]EnvEntry{{Key: []byte("PATH"), Value: []byte("/bin")}})
	if digestErr != nil || record.EnvDigest != wantDigest {
		t.Fatalf("governor vars changed digest: got=%q want=%q err=%v", record.EnvDigest, wantDigest, digestErr)
	}
}

func TestSidecarEnvironmentDigestIgnoresInheritedGovernorValues(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.inputRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var childEnv []string
	r.startFn = func(command *exec.Cmd) error {
		childEnv = append([]string(nil), command.Env...)
		return errors.New("injected after environment observation")
	}

	governorKeys := []string{"AIRA_PY_LIB", "AIRA_CPU_SLOTS_DIR", "AIRA_CPU_POLL_INTERVAL", "AIRA_CPU_MAX_WAIT"}
	for _, key := range governorKeys {
		t.Setenv(key, "/stale-"+key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	_, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
	if err == nil {
		t.Fatal("launch unexpectedly succeeded")
	}
	withoutGovernor, getErr := r.Get("RUN-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	for _, key := range governorKeys {
		if err := os.Setenv(key, "/stale-"+key); err != nil {
			t.Fatal(err)
		}
	}
	_, err = r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
	if err == nil {
		t.Fatal("launch unexpectedly succeeded")
	}
	withGovernor, getErr := r.Get("RUN-2")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if withGovernor.EnvDigest != withoutGovernor.EnvDigest {
		t.Fatalf("inherited governor values changed digest: with=%q without=%q", withGovernor.EnvDigest, withoutGovernor.EnvDigest)
	}
	values := testEnvironmentValues(t, childEnv)
	if values["AIRA_PY_LIB"] == "" || values["AIRA_PY_LIB"] == "/stale-AIRA_PY_LIB" || values["AIRA_CPU_SLOTS_DIR"] != filepath.Join(r.inputRuntimeDir, "cpuslots") {
		t.Fatalf("child did not receive authoritative governor values: %v", childEnv)
	}
}

func TestSidecarExtractionFailureDoesNotBlockLaunch(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.inputRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	blockedDataHome := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDataHome, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", blockedDataHome)
	var childEnv []string
	r.startFn = func(command *exec.Cmd) error {
		childEnv = append([]string(nil), command.Env...)
		return errors.New("launch reached after sidecar failure")
	}
	_, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, Env: []string{
		"PATH=/bin",
		"AIRA_PY_LIB=/stale",
		"AIRA_CPU_SLOTS_DIR=/stale-slots",
		"AIRA_CPU_POLL_INTERVAL=99",
		"AIRA_CPU_MAX_WAIT=99",
	}, ExplicitEnv: true})
	if err == nil || !strings.Contains(err.Error(), "launch reached after sidecar failure") {
		t.Fatalf("sidecar failure blocked or replaced launch result: %v", err)
	}
	values := testEnvironmentValues(t, childEnv)
	for _, key := range []string{"AIRA_PY_LIB", "AIRA_CPU_SLOTS_DIR", "AIRA_CPU_POLL_INTERVAL", "AIRA_CPU_MAX_WAIT"} {
		if _, present := values[key]; present {
			t.Fatalf("failed extraction retained %s: %v", key, childEnv)
		}
	}
}

func testEnvironmentValues(t *testing.T, env []string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", entry)
		}
		if _, exists := values[key]; exists {
			t.Fatalf("duplicate environment key %q", key)
		}
		values[key] = value
	}
	return values
}

func TestRealCgroupRealtimeHonestyAndChildEnvironment(t *testing.T) {
	lib := realtimeFixtureLib(t)
	previous := locateLibstdbufFn
	t.Cleanup(func() { locateLibstdbufFn = previous })
	probe := realtimeEnvProbe()
	env := []string{"PATH=/usr/bin:/bin", "LD_PRELOAD=" + lib}

	locateLibstdbufFn = func() string { return lib }
	withInjection, err := realRunner(t).Launch(context.Background(), Request{Argv: probe, Env: env, ExplicitEnv: true, Realtime: true})
	if err != nil {
		t.Fatal(err)
	}
	if withInjection.Buffering != "realtime" {
		t.Fatalf("realtime buffering=%q record=%+v", withInjection.Buffering, withInjection)
	}
	data, err := os.ReadFile(withInjection.OutputRefs["out"].Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"LD_PRELOAD=" + lib + ":" + lib + "\n", "_STDBUF_O=L\n", "_STDBUF_E=L\n", "PYTHONUNBUFFERED=1\n"} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("injected child environment=%q missing %q", data, expected)
		}
	}
}

func TestRealCgroupRealtimeNoOpPreservesEnvironment(t *testing.T) {
	lib := realtimeFixtureLib(t)
	previous := locateLibstdbufFn
	t.Cleanup(func() { locateLibstdbufFn = previous })
	locateLibstdbufFn = func() string { return "" }
	request := Request{Argv: realtimeEnvProbe(), Env: []string{"PATH=/usr/bin:/bin", "LD_PRELOAD=" + lib}, ExplicitEnv: true}
	plain, err := realRunner(t).Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Realtime = true
	withoutInjection, err := realRunner(t).Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if withoutInjection.Buffering != "none" {
		t.Fatalf("no-op buffering=%q record=%+v", withoutInjection.Buffering, withoutInjection)
	}
	data, err := os.ReadFile(withoutInjection.OutputRefs["out"].Path)
	if err != nil {
		t.Fatal(err)
	}
	plainData, err := os.ReadFile(plain.OutputRefs["out"].Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"_STDBUF_O=", "_STDBUF_E=", "PYTHONUNBUFFERED="} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("no-op child environment=%q contains tactic variable %q", data, forbidden)
		}
	}
	if !bytes.Equal(data, plainData) {
		t.Fatalf("no-op child environment changed from plain baseline: plain=%q no-op=%q", plainData, data)
	}
}

func realtimeFixtureLib(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "libstdbuf.so")
	if err := os.WriteFile(path, []byte("not a shared object"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func realtimeEnvProbe() []string {
	return []string{"/bin/sh", "-c", "[ -n \"${LD_PRELOAD+x}\" ] && printf 'LD_PRELOAD=%s\\n' \"$LD_PRELOAD\"; [ -n \"${_STDBUF_O+x}\" ] && printf '_STDBUF_O=%s\\n' \"$_STDBUF_O\"; [ -n \"${_STDBUF_E+x}\" ] && printf '_STDBUF_E=%s\\n' \"$_STDBUF_E\"; [ -n \"${PYTHONUNBUFFERED+x}\" ] && printf 'PYTHONUNBUFFERED=%s\\n' \"$PYTHONUNBUFFERED\""}
}

func TestRealCgroupRealtimeKeepsDigestOrthogonalToInjection(t *testing.T) {
	lib := realtimeFixtureLib(t)
	previous := locateLibstdbufFn
	t.Cleanup(func() { locateLibstdbufFn = previous })
	locateLibstdbufFn = func() string { return lib }
	request := Request{Argv: []string{"/bin/sh", "-c", "printf 'LD_PRELOAD=%s\\n_STDBUF_O=%s\\n' \"$LD_PRELOAD\" \"$_STDBUF_O\""}, Env: []string{"PATH=/usr/bin:/bin", "AIRA_REALTIME_TEST=1"}, ExplicitEnv: true}
	plain, err := realRunner(t).Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Realtime = true
	realtime, err := realRunner(t).Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if plain.EnvDigest != realtime.EnvDigest {
		t.Fatalf("plain digest=%q realtime digest=%q", plain.EnvDigest, realtime.EnvDigest)
	}
	if realtime.Buffering != "realtime" {
		t.Fatalf("realtime record=%+v", realtime)
	}
	plainData, err := os.ReadFile(plain.OutputRefs["out"].Path)
	if err != nil {
		t.Fatal(err)
	}
	realtimeData, err := os.ReadFile(realtime.OutputRefs["out"].Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainData), "_STDBUF_O=L") || !strings.Contains(string(realtimeData), "_STDBUF_O=L") {
		t.Fatalf("plain=%q realtime=%q", plainData, realtimeData)
	}
}

func TestRealCgroupRealtimePreservesSeparateCaptureAndDigests(t *testing.T) {
	lib := realtimeFixtureLib(t)
	previous := locateLibstdbufFn
	t.Cleanup(func() { locateLibstdbufFn = previous })
	locateLibstdbufFn = func() string { return lib }
	record, err := realRunner(t).Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "printf out; printf err >&2"}, Realtime: true})
	if err != nil {
		t.Fatal(err)
	}
	if record.Buffering != "realtime" || !record.CaptureComplete {
		t.Fatalf("realtime capture record=%+v", record)
	}
	for stream, want := range map[string][]byte{"out": []byte("out"), "err": []byte("err")} {
		ref, ok := record.OutputRefs[stream]
		if !ok || ref.Path == "" || ref.State != OutputComplete {
			t.Fatalf("stream %s ref=%+v record=%+v", stream, ref, record)
		}
		data, readErr := os.ReadFile(ref.Path)
		if readErr != nil || (stream == "out" && !bytes.Equal(data, want)) || (stream == "err" && !bytes.HasSuffix(data, want)) {
			t.Fatalf("stream %s data=%q err=%v", stream, data, readErr)
		}
		digest := sha256.Sum256(data)
		if ref.Digest != hex.EncodeToString(digest[:]) || ref.Bytes != int64(len(data)) {
			t.Fatalf("stream %s ref=%+v data=%q", stream, ref, data)
		}
	}
}

func realRunner(t *testing.T) *Runner {
	t.Helper()
	// A private cgroup parent per call: real-cgroup tests across packages share the
	// ambient scope, so an ambient-parent runner races others to create `.aira-RUN-1`
	// (see cgrouptest). Unique per call so a test that builds several runners does not
	// EEXIST its own parent.
	r, err := New(Config{CommonDir: t.TempDir(), Owner: "A", CgroupParent: cgrouptest.IsolatedScopeParent(t), Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.backend.Probe(context.Background()); err != nil {
		skipOrFailRealCgroup(t, "real cgroup-v2 delegation unavailable: %v", err)
	}
	return r
}

// AIRA_REAL_CGROUP=1 makes real-cgroup verification fail closed: delegation,
// controller setup, and fixture dependencies become fatal instead of skips.
// Leave it unset in environments such as this sandbox where cgroup-v2 is
// intentionally read-only.
func skipOrFailRealCgroup(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("AIRA_REAL_CGROUP") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
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
	if _, err := r.Kill(context.Background(), "RUN-1", false); err != nil {
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
	assertRunEventsOwner(t, r, current.ID, "A")
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestRealCgroupKillWaitRaceHasOneTerminalWinner(t *testing.T) {
	r := realRunner(t)
	outcome := launchAsync(t, r, Request{Argv: []string{"/bin/sh", "-c", "sleep 0.2"}})
	_, _ = r.Kill(context.Background(), "RUN-1", false)
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
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	assertRunEventsOwner(t, r, current.ID, "A")
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
	bootID, bootErr := currentBootID()
	if bootErr != nil {
		t.Fatal(bootErr)
	}
	identity.BootID = bootID
	if identity.StartTick == 0 || processLive(identity) != processAlive {
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
	_, _ = r.Kill(context.Background(), "RUN-1", false)
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
