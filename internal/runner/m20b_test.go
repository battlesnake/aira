//go:build linux

package runner

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func telemetryRunnerForTest(t *testing.T) *Runner {
	t.Helper()
	base := t.TempDir()
	ledger, err := newLedger(base, filepath.Join(base, "output"))
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{ledger: ledger, runLockTimeout: defaultRunLockTimeout}
}

func appendPendingTerminal(t *testing.T, r *Runner) {
	t.Helper()
	zero := 0
	starting := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, Telemetry: "opaque-pending"}
	terminal := starting
	terminal.Status, terminal.ExitCode, terminal.TerminalComplete = StatusExited, &zero, true
	if _, err := r.append(ledgerEvent{Kind: "starting", Run: starting}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.append(ledgerEvent{Kind: "terminal", Run: terminal}); err != nil {
		t.Fatal(err)
	}
}

func TestM20bReplayAuthorizesOnlyOneTelemetryEventAfterTerminal(t *testing.T) {
	zero, seven := 0, 7
	starting := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, Telemetry: "opaque-pending"}
	terminal := starting
	terminal.Status, terminal.ExitCode, terminal.TerminalComplete = StatusExited, &zero, true
	valid := []ledgerEvent{
		{Sequence: 1, Kind: "starting", Run: starting},
		{Sequence: 2, Kind: "terminal", Run: terminal},
		{Sequence: 3, Kind: "telemetry", Run: RunRecord{ID: "RUN-1", Telemetry: "opaque-complete", TelemetryRefs: []string{"TR-1"}}},
	}
	runs, err := replay(valid)
	if err != nil {
		t.Fatal(err)
	}
	got := runs["RUN-1"]
	if got.Telemetry != "opaque-complete" || !reflect.DeepEqual(got.TelemetryRefs, []string{"TR-1"}) || got.Status != StatusExited || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("telemetry replay changed lifecycle evidence: %+v", got)
	}

	mutating := append([]ledgerEvent(nil), valid[:2]...)
	mutating = append(mutating, ledgerEvent{Sequence: 3, Kind: "telemetry", Run: RunRecord{ID: "RUN-1", Status: StatusRunning, ExitCode: &seven, Telemetry: "opaque-complete"}})
	if _, err := replay(mutating); err == nil {
		t.Fatal("post-terminal telemetry payload mutated lifecycle evidence")
	}
	duplicate := append(append([]ledgerEvent(nil), valid...), ledgerEvent{Sequence: 4, Kind: "telemetry", Run: RunRecord{ID: "RUN-1", Telemetry: "opaque-complete", TelemetryRefs: []string{"TR-1"}}})
	if _, err := replay(duplicate); err == nil {
		t.Fatal("replay accepted a second post-terminal telemetry event")
	}
	other := append(append([]ledgerEvent(nil), valid[:2]...), ledgerEvent{Sequence: 3, Kind: "running", Run: terminal})
	if _, err := replay(other); err == nil {
		t.Fatal("replay accepted a non-telemetry event after terminal")
	}
}

func TestM20bRecordAuxTelemetryCASRacesAndIsIdempotent(t *testing.T) {
	r := telemetryRunnerForTest(t)
	appendPendingTerminal(t, r)
	writers := []*Runner{r, {ledger: r.ledger, runLockTimeout: defaultRunLockTimeout}}
	states := []string{"opaque-complete", "opaque-incomplete"}
	errs := make([]error, len(states))
	var wg sync.WaitGroup
	for i := range states {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = writers[i].RecordAuxTelemetry(context.Background(), "RUN-1", states[i], []string{states[i]})
		}(i)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one conflicting CAS must win: %v", errs)
	}
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	winner := current.Telemetry
	if winner != "opaque-complete" && winner != "opaque-incomplete" {
		t.Fatalf("unexpected settled state: %+v", current)
	}
	eventsBefore, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordAuxTelemetry(context.Background(), "RUN-1", winner, append([]string(nil), current.TelemetryRefs...)); err != nil {
		t.Fatalf("identical retry was not idempotent: %v", err)
	}
	eventsAfter, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("idempotent retry appended an event: before=%d after=%d", len(eventsBefore), len(eventsAfter))
	}
}

func TestM20bIdenticalConcurrentSettlementAppendsOnce(t *testing.T) {
	r := telemetryRunnerForTest(t)
	appendPendingTerminal(t, r)
	writers := []*Runner{r, {ledger: r.ledger, runLockTimeout: defaultRunLockTimeout}}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = writers[i].RecordAuxTelemetry(context.Background(), "RUN-1", "opaque-incomplete", []string{"U_REASON"})
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("identical settlements were not idempotent: %v", errs)
	}
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "telemetry" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("telemetry events=%d", count)
	}
}

func TestM20bReconcileNeverWritesTelemetry(t *testing.T) {
	r := telemetryRunnerForTest(t)
	appendPendingTerminal(t, r)
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "telemetry" {
			t.Fatal("reconcile settled opaque telemetry")
		}
	}
}

func TestM20bRecordAuxTelemetryRequiresTerminalPendingEnvelope(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *Runner){
		"non-terminal": func(t *testing.T, r *Runner) {
			_, err := r.append(ledgerEvent{Kind: "starting", Run: RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, Telemetry: "opaque-pending"}})
			if err != nil {
				t.Fatal(err)
			}
		},
		"no-envelope": func(t *testing.T, r *Runner) {
			zero := 0
			_, err := r.append(ledgerEvent{Kind: "terminal", Run: RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusExited, ExitCode: &zero, TerminalComplete: true}})
			if err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := telemetryRunnerForTest(t)
			setup(t, r)
			if _, err := r.RecordAuxTelemetry(context.Background(), "RUN-1", "settled", nil); err == nil {
				t.Fatal("CAS accepted a run without a terminal pending envelope")
			}
		})
	}
}

func TestM20bMergeEvidenceCarriesOpaqueTelemetryDefensively(t *testing.T) {
	refs := []string{"TR-1"}
	got := mergeEvidence(RunRecord{ID: "RUN-1"}, RunRecord{ID: "RUN-1", Telemetry: "opaque", TelemetryRefs: refs})
	refs[0] = "mutated"
	if got.Telemetry != "opaque" || !reflect.DeepEqual(got.TelemetryRefs, []string{"TR-1"}) {
		t.Fatalf("opaque telemetry was not carried defensively: %+v", got)
	}
}

func TestM20bSupervisorLivenessIsGeneric(t *testing.T) {
	priorBoot := readBootIDFn
	priorStat := readProcStatFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn = priorBoot, priorStat })
	readBootIDFn = func() (string, error) { return "boot-a", nil }
	record := RunRecord{SupervisorPID: PIDIdentity{PID: 41, StartTick: 99, BootID: "boot-a"}}
	readProcStatFn = func(int) ([]byte, error) { return nil, errors.New("opaque") }
	if got := (&Runner{}).SupervisorLiveness(record); got != SupervisorUnknown {
		t.Fatalf("unknown=%q", got)
	}
	readProcStatFn = func(int) ([]byte, error) { return nil, errors.ErrUnsupported }
	readBootIDFn = func() (string, error) { return "boot-b", nil }
	if got := (&Runner{}).SupervisorLiveness(record); got != SupervisorDead {
		t.Fatalf("dead=%q", got)
	}
}
