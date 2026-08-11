package runner

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
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
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusExited, ScopeIntegrity: ScopeContained, TerminalComplete: true}
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
	for _, mutate := range []func(*RunRecord){func(r *RunRecord) { r.Status = StatusKilled }, func(r *RunRecord) { r.CaptureComplete = false }, func(r *RunRecord) { r.TerminalComplete = false }, func(r *RunRecord) { r.ScopeIntegrity = ScopeHandoffUnverified }, func(r *RunRecord) { r.ErrorCodes = []string{"E_RUN_CAPTURE_FAILED"} }} {
		copy := r
		mutate(&copy)
		if copy.CleanSuccess() {
			t.Fatalf("incomplete record earned success: %+v", copy)
		}
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
	base := t.TempDir()
	r, err := New(Config{CommonDir: base, Grace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	backend := r.backend
	if err := backend.Probe(context.Background()); err != nil {
		t.Skipf("real cgroup-v2 delegation unavailable: %v", err)
	}
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

func TestCaptureCodeDistinguishesENOSPC(t *testing.T) {
	if got := captureCode(syscall.ENOSPC); got != "E_RUN_OUTPUT_DISK_FULL" {
		t.Fatal(got)
	}
}
