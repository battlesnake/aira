package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aira/internal/app"
	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/runner"
	"aira/internal/store"

	"golang.org/x/sys/unix"
)

// supervisorReportJSON is one complete, parseable `go test -json` package run.
const supervisorReportJSON = `{"Action":"start","Package":"pkg"}
{"Action":"run","Package":"pkg","Test":"TestOne"}
{"Action":"pass","Package":"pkg","Test":"TestOne","Elapsed":0.1}
{"Action":"pass","Package":"pkg","Elapsed":0.1}
`

// supervisorTelemetryRunner is the detached supervisor's runner as seen by the
// wiring path: it replays the terminal run's captured output and accepts the one
// post-terminal telemetry settlement.
type supervisorTelemetryRunner struct {
	relayRecordingRunner
	settledState string
	settledRefs  []string
}

func (*supervisorTelemetryRunner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return &runner.OutputChunk{RunID: "RUN-relay", Bytes: []byte(supervisorReportJSON), Complete: true}, nil
}

func (r *supervisorTelemetryRunner) RecordAuxTelemetry(_ context.Context, _, state string, refs []string) (*runner.RunRecord, error) {
	r.settledState, r.settledRefs = state, append([]string(nil), refs...)
	return &runner.RunRecord{ID: "RUN-relay", Status: runner.StatusExited}, nil
}

// dupTestFD duplicates a descriptor so a callee that closes what it is given
// cannot close a descriptor this test still owns.
func dupTestFD(t *testing.T, file *os.File) int {
	t.Helper()
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

// supervisorProjectFixture builds a real project rooted in the process working
// directory (runSupervisor resolves "."), with XDG paths pointed at a private
// state home. It does NOT create state.db: callers decide whether a daemon runs.
func supervisorProjectFixture(t *testing.T) daemon.Paths {
	t.Helper()
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func supervisorProject(t *testing.T) app.Project {
	t.Helper()
	project, err := app.OpenWithoutStore(context.Background(), ".", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func terminalDetachedRecord() runner.RunRecord {
	exit, peak := 0, int64(4096)
	return runner.RunRecord{
		ID: "RUN-relay", Status: runner.StatusExited, ExitCode: &exit, Detached: true,
		StartedAt: "2026-09-04T10:00:00Z", EndedAt: "2026-09-04T10:00:01Z",
		CaptureComplete: true, TerminalComplete: true, EnvDigest: "env", PeakRSS: &peak,
		Tool: "gpt", Ticket: "AIRA-85",
	}
}

// verifies: the detached supervisor's store handle relays every telemetry write
// to the daemon and can itself write nothing to state.db (AIRA-85).
func TestSupervisorRelayStoreRelaysTelemetryWritesAndCannotWriteStateDBItself(t *testing.T) {
	paths := supervisorProjectFixture(t)
	server := daemon.NewServer(paths)
	startCommandDaemon(t, server)
	project := supervisorProject(t)

	var frames []daemon.StoreOpFrame
	relayed, err := openSupervisorRelayStore(project, paths, func(ctx context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		frames = append(frames, frame)
		return daemon.ExchangeStoreOp(ctx, paths.SocketPath, frame)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relayed.Close()

	raw := []byte(supervisorReportJSON)
	report, err := relayed.AddTestReport(context.Background(), domain.TestReportInput{
		Format: "go-json", Raw: raw, RunRef: "RUN-relay", SuiteID: "suite", EnvDigest: "env",
		At: "2026-09-04T10:00:01Z", PreserveEmptyProvenance: true,
	})
	if err != nil || strings.TrimSpace(report.ID) == "" {
		t.Fatalf("relayed test report id=%q err=%v", report.ID, err)
	}
	compute, err := relayed.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Model: "gpt", Provider: "openai", At: "2026-09-04T10:00:01Z", Source: "run",
	})
	if err != nil || strings.TrimSpace(compute.ID) == "" {
		t.Fatalf("relayed compute event id=%q err=%v", compute.ID, err)
	}

	wantOps := []string{"add-test-report", "add-compute-event"}
	if len(frames) != len(wantOps) {
		t.Fatalf("frames=%d want=%d (%+v)", len(frames), len(wantOps), frames)
	}
	for index, want := range wantOps {
		if frames[index].Op != want {
			t.Fatalf("frame[%d].Op=%q want %q", index, frames[index].Op, want)
		}
		if frames[index].Scope.ProjectID != project.ProjectID || frames[index].Scope.WorktreeID != project.WorktreeID {
			t.Fatalf("frame[%d] scope=%+v want project=%s worktree=%s", index, frames[index].Scope, project.ProjectID, project.WorktreeID)
		}
		if frames[index].Scope.StateID != paths.StateID {
			t.Fatalf("frame[%d] state id=%q want %q", index, frames[index].Scope.StateID, paths.StateID)
		}
	}

	// The daemon really applied both writes: the supervisor's own connection is
	// mode=ro, so a row that reads back can only have come from the daemon.
	stored, err := relayed.GetTestReport(report.ID)
	if err != nil || stored.ID != report.ID {
		t.Fatalf("daemon did not durably record the relayed report: %+v err=%v", stored, err)
	}

	// Any writer the relay does not override must fail against the read-only
	// embed rather than silently becoming a second writer from this process.
	if _, err := relayed.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual"}); err == nil {
		t.Fatal("an unoverridden writer reached state.db from the supervisor process")
	}
}

// verifies: the production store construction runSupervisor uses
// (supervisorTelemetryStore) carries a real terminal detached-run settlement to
// the real daemon, and the rows are durable — the supervisor's own connection is
// mode=ro, so anything that reads back was written by the daemon.
func TestSupervisorDetachedSettlementWritesThroughTheDaemonRelay(t *testing.T) {
	paths := supervisorProjectFixture(t)
	server := daemon.NewServer(paths)
	startCommandDaemon(t, server)
	project := supervisorProject(t)

	relayed, err := supervisorTelemetryStore(project, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer relayed.Close()

	execution := &supervisorTelemetryRunner{}
	params := core.WiringParams{Report: "go-json", Suite: "suite", Tool: "gpt", Provider: "openai"}
	wiring, settled, settleErr := core.NewWithRunner(relayed, execution).WireAndSettleDetached(
		context.Background(), terminalDetachedRecord(), params, store.TestReportContext{WorktreeID: project.WorktreeID})
	if settleErr != nil || !settled {
		t.Fatalf("settled=%v err=%v", settled, settleErr)
	}
	if !wiring.WiringComplete {
		t.Fatalf("wiring incomplete: %+v", wiring)
	}
	if execution.settledState != core.TelemetryComplete {
		t.Fatalf("settled state=%q want %q", execution.settledState, core.TelemetryComplete)
	}
	// The settlement references the ids the DAEMON minted, not locally invented
	// ones — the run record and the relayed rows agree.
	if len(execution.settledRefs) != 2 || execution.settledRefs[0] != wiring.Report.ID || execution.settledRefs[1] != wiring.Compute.ID {
		t.Fatalf("settled refs=%v want [%s %s]", execution.settledRefs, wiring.Report.ID, wiring.Compute.ID)
	}
	stored, err := relayed.GetTestReport(wiring.Report.ID)
	if err != nil || stored.ID != wiring.Report.ID || stored.RunRef != "RUN-relay" {
		t.Fatalf("settled report not durable: %+v err=%v", stored, err)
	}
	events, err := relayed.ListComputeEvents("")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != wiring.Compute.ID {
		t.Fatalf("settled compute event not durable: %+v err=%v", events, err)
	}
}

// verifies: a SPLIT outcome — the report relayed and committed, the compute event
// refused — keeps the committed row, warns only about the half that failed, and
// still settles incomplete. Neither half is rolled back and neither is re-sent
// (build-review, partial-failure direction).
func TestSupervisorSettlementKeepsTheCommittedHalfOfASplitOutcome(t *testing.T) {
	paths := supervisorProjectFixture(t)
	server := daemon.NewServer(paths)
	startCommandDaemon(t, server)
	project := supervisorProject(t)

	sent := map[string]int{}
	relayed, err := openSupervisorRelayStore(project, paths, func(ctx context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		sent[frame.Op]++
		if frame.Op == "add-compute-event" {
			return daemon.ResponseFrame{Code: daemon.CodeTimeout, Error: daemon.CodeTimeout + ": store operation deadline elapsed; outcome unevaluated"}, nil
		}
		return daemon.ExchangeStoreOp(ctx, paths.SocketPath, frame)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relayed.Close()

	execution := &supervisorTelemetryRunner{}
	params := core.WiringParams{Report: "go-json", Suite: "suite", Tool: "gpt", Provider: "openai"}
	wiring, settled, settleErr := core.NewWithRunner(relayed, execution).WireAndSettleDetached(
		context.Background(), terminalDetachedRecord(), params, store.TestReportContext{WorktreeID: project.WorktreeID})
	if settleErr != nil || !settled {
		t.Fatalf("settled=%v err=%v", settled, settleErr)
	}
	if wiring.WiringComplete || execution.settledState != core.TelemetryIncomplete {
		t.Fatalf("a half-failed settlement must be incomplete: wiring=%+v state=%q", wiring, execution.settledState)
	}
	if sent["add-test-report"] != 1 || sent["add-compute-event"] != 1 {
		t.Fatalf("relayed ops=%v want exactly one of each", sent)
	}
	// The half that committed stays committed, and only the other half warns.
	stored, err := relayed.GetTestReport(wiring.Report.ID)
	if err != nil || stored.ID != wiring.Report.ID {
		t.Fatalf("the committed report half was lost: %+v err=%v", stored, err)
	}
	events, err := relayed.ListComputeEvents("")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused compute event reached state.db: %+v", events)
	}
	warnings := map[string]string{}
	for _, warning := range wiring.Warnings {
		warnings[warning.Action] = warning.Code
	}
	if len(warnings) != 1 || warnings["compute"] != daemon.CodeTimeout {
		t.Fatalf("warnings=%+v want exactly one compute warning with %s", wiring.Warnings, daemon.CodeTimeout)
	}
}

// verifies: a lost store-op reply is settled as OUTCOME-UNKNOWN, distinct from a
// clean failure, and the append is never re-sent — so relaying cannot turn a
// committed report into a silent duplicate (build-review, false-fail direction).
func TestSupervisorSettlementSurfacesOutcomeUnknownAndNeverRetriesTheAppend(t *testing.T) {
	paths := supervisorProjectFixture(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	project := supervisorProject(t)

	appends := 0
	relayed, err := openSupervisorRelayStore(project, paths, func(context.Context, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		appends++
		return daemon.ResponseFrame{}, &daemon.StoreOpOutcomeUnknownError{Err: io.ErrUnexpectedEOF}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relayed.Close()

	execution := &supervisorTelemetryRunner{}
	params := core.WiringParams{Report: "go-json", Suite: "suite", Tool: "gpt", Provider: "openai"}
	wiring, settled, settleErr := core.NewWithRunner(relayed, execution).WireAndSettleDetached(
		context.Background(), terminalDetachedRecord(), params, store.TestReportContext{WorktreeID: project.WorktreeID})
	if settleErr != nil || !settled {
		t.Fatalf("settled=%v err=%v", settled, settleErr)
	}
	if wiring.WiringComplete || execution.settledState != core.TelemetryIncomplete {
		t.Fatalf("wiring=%+v state=%q", wiring, execution.settledState)
	}
	if wiring.Report.Code != "U_DAEMON_OUTCOME_UNKNOWN" || wiring.Compute.Code != "U_DAEMON_OUTCOME_UNKNOWN" {
		t.Fatalf("report=%q compute=%q want U_DAEMON_OUTCOME_UNKNOWN on both", wiring.Report.Code, wiring.Compute.Code)
	}
	// One report append and one compute append. A third would mean the ambiguous
	// append was retried, which risks a duplicate row against a daemon that did
	// commit it.
	if appends != 2 {
		t.Fatalf("relayed appends=%d want 2 (an ambiguous append must never be retried)", appends)
	}
}

// verifies: with no daemon reachable the supervisor records nothing rather than
// falling back to a direct state.db write, and the run settles honestly as
// telemetry-incomplete (AIRA-85).
func TestSupervisorSettlementWithoutADaemonIsIncompleteNotADirectWrite(t *testing.T) {
	paths := supervisorProjectFixture(t)
	// A daemon is started only to create state.db, then stopped: the relay has a
	// real database to read but no socket to reach.
	func() {
		db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	project := supervisorProject(t)

	relayed, err := openSupervisorRelayStore(project, paths, daemonStoreOpRelay(paths.SocketPath))
	if err != nil {
		t.Fatal(err)
	}
	defer relayed.Close()

	execution := &supervisorTelemetryRunner{}
	params := core.WiringParams{Report: "go-json", Suite: "suite", Tool: "gpt", Provider: "openai"}
	wiring, settled, settleErr := core.NewWithRunner(relayed, execution).WireAndSettleDetached(
		context.Background(), terminalDetachedRecord(), params, store.TestReportContext{WorktreeID: project.WorktreeID})
	if settleErr != nil || !settled {
		t.Fatalf("settled=%v err=%v", settled, settleErr)
	}
	if wiring.WiringComplete {
		t.Fatalf("a daemon-less settlement claimed complete wiring: %+v", wiring)
	}
	if execution.settledState != core.TelemetryIncomplete {
		t.Fatalf("settled state=%q want %q", execution.settledState, core.TelemetryIncomplete)
	}
	warnings := map[string]string{}
	for _, warning := range wiring.Warnings {
		warnings[warning.Action] = warning.Code
	}
	if warnings["report"] != daemon.CodeUnavailable || warnings["compute"] != daemon.CodeUnavailable {
		t.Fatalf("warnings=%+v want %s for report and compute", wiring.Warnings, daemon.CodeUnavailable)
	}
	// Nothing may have reached state.db from this process.
	reports, err := relayed.ListTestReports("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 {
		t.Fatalf("a daemon-less supervisor wrote %d report(s) directly to state.db", len(reports))
	}
}

// verifies: a nil relay (no daemon endpoint resolved at all) still refuses the
// write instead of degrading to a direct one.
func TestSupervisorRelayStoreWithoutAnEndpointRefusesWrites(t *testing.T) {
	paths := supervisorProjectFixture(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	project := supervisorProject(t)
	if relay := daemonStoreOpRelay("   "); relay != nil {
		t.Fatal("a blank socket path produced a relay")
	}
	relayed, err := openSupervisorRelayStore(project, paths, daemonStoreOpRelay(""))
	if err != nil {
		t.Fatal(err)
	}
	defer relayed.Close()
	if _, err := relayed.AddTestReport(context.Background(), domain.TestReportInput{Format: "go-json"}); err == nil ||
		store.ErrorCode(err) != daemon.CodeUnavailable {
		t.Fatalf("err=%v want %s", err, daemon.CodeUnavailable)
	}
}

// verifies: runSupervisor never opens a read-write state.db handle — it refuses
// when the daemon-owned database is absent rather than creating one, which is
// exactly what app.OpenWithDiagnostics used to do from this process (AIRA-85).
func TestSupervisorRefusesRatherThanCreatingStateDBItself(t *testing.T) {
	paths := supervisorProjectFixture(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-ran.marker")
	control := filepath.Join(dir, "request.ctrl")
	payload, err := json.Marshal(runner.Request{Argv: []string{"/bin/sh", "-c", "touch " + marker}, Detach: true, NoAdmit: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ackR.Close()
	defer ackW.Close()
	// runSupervisor takes ownership of the descriptors it is handed and closes
	// them; give it duplicates so this test's own *os.File values never close a
	// descriptor number that has already been recycled.
	readyFD, ackFD := dupTestFD(t, readyW), dupTestFD(t, ackR)
	// Pre-acknowledge the (never-reached) detach handshake so a regression that
	// gets past the store open FAILS this test's assertions instead of blocking
	// forever on an ack nobody sends.
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(paths.DBPath); !os.IsNotExist(err) {
		t.Fatalf("fixture already created state.db: %v", err)
	}
	exit := runSupervisor([]string{
		"--control", control,
		"--ready-fd", strconv.Itoa(readyFD),
		"--ack-fd", strconv.Itoa(ackFD),
	}, io.Discard)
	if exit != codes.ExitForCode("E_RUN_DETACH_FAILED") {
		t.Fatalf("exit=%d want %d", exit, codes.ExitForCode("E_RUN_DETACH_FAILED"))
	}
	if _, err := os.Stat(paths.DBPath); !os.IsNotExist(err) {
		t.Fatalf("the supervisor created the daemon-owned state.db itself: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the child launched despite an unopenable store: %v", err)
	}
	var ready map[string]string
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if ready["code"] != "E_RUN_DETACH_FAILED" {
		t.Fatalf("readiness=%v", ready)
	}
}
