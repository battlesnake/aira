package runner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const ledgerSchema = 1

type ledgerEvent struct {
	SchemaVersion      int       `json:"schema_version"`
	Sequence           uint64    `json:"sequence"`
	Kind               string    `json:"kind"`
	Run                RunRecord `json:"run"`
	WaitExit           *int      `json:"wait_exit,omitempty"`
	WaitSignal         string    `json:"wait_signal,omitempty"`
	WaitObserved       bool      `json:"wait_observed,omitempty"`
	KillCompleted      bool      `json:"kill_completed,omitempty"`
	LeaderExitObserved bool      `json:"leader_exit_observed,omitempty"`
	ExitCode           *int      `json:"exit_code,omitempty"`
	Signal             string    `json:"signal,omitempty"`
}

type ledger struct {
	root       string
	ledger     string
	counter    string
	lock       string
	projection string
}

func newLedger(common, output string) (*ledger, error) {
	if common == "" {
		return nil, &LaunchError{"E_CONFIG_INVALID", errors.New("common directory is required")}
	}
	common, err := filepath.Abs(common)
	if err != nil {
		return nil, err
	}
	if output == "" {
		output = filepath.Join(common, "aira", "runs", "output")
	}
	if err := os.MkdirAll(filepath.Join(common, "aira", "runs"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return nil, err
	}
	return &ledger{root: common, ledger: filepath.Join(common, "aira", "runs", "ledger.bin"), counter: filepath.Join(common, "aira", "runs", "counter"), lock: filepath.Join(common, "aira", "runs", "ledger.lock"), projection: filepath.Join(common, "aira", "runs", "runs.db")}, nil
}

func lockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(int(f.Fd()))
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func lockFileBounded(path string, timeout time.Duration) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(int(f.Fd()))
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, &LaunchError{Code: "U_RUN_LAUNCH_STALLED", Err: errors.New("timed out acquiring the per-run launch lock")}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func unlockFile(f *os.File) error {
	if f == nil {
		return nil
	}
	err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (l *ledger) reserveID() (string, error) {
	lock, err := lockFile(l.counter + ".lock")
	if err != nil {
		return "", err
	}
	defer unlockFile(lock)
	next := int64(1)
	data, err := os.ReadFile(l.counter)
	if err == nil && len(data) > 0 {
		next, err = strconv.ParseInt(string(data), 10, 64)
		if err != nil || next < 1 {
			return "", fmt.Errorf("E_RUN_RECONCILE_REQUIRED: invalid run counter")
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	f, err := os.OpenFile(l.counter, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if _, err = io.WriteString(f, strconv.FormatInt(next+1, 10)); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(l.counter)); err != nil {
		return "", err
	}
	return fmt.Sprintf("RUN-%d", next), nil
}

func frame(payload []byte) []byte {
	var prefix [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(prefix[:], uint64(len(payload)))
	digest := sha256.Sum256(payload)
	result := make([]byte, 0, n+len(payload)+len(digest))
	result = append(result, prefix[:n]...)
	result = append(result, payload...)
	result = append(result, digest[:]...)
	return result
}

func (l *ledger) append(event ledgerEvent) (ledgerEvent, error) {
	lock, err := lockFile(l.lock)
	if err != nil {
		return event, fmt.Errorf("E_RUN_RECONCILE_REQUIRED: %w", err)
	}
	defer unlockFile(lock)
	if event.Sequence == 0 {
		events, readErr := l.read()
		if readErr != nil {
			return event, readErr
		}
		var terminal, telemetry, envelope bool
		for _, prior := range events {
			if prior.Run.ID != event.Run.ID {
				continue
			}
			if prior.Kind == "terminal" {
				terminal = true
			}
			if prior.Kind == "telemetry" {
				telemetry = true
			}
			if prior.Kind == "starting" && prior.Run.Telemetry != "" {
				envelope = true
			}
		}
		if terminal {
			switch {
			case event.Kind == "terminal":
				return event, fmt.Errorf("E_JOURNAL_CORRUPT: duplicate terminal record for %s", event.Run.ID)
			case event.Kind != "telemetry":
				return event, fmt.Errorf("E_JOURNAL_CORRUPT: record after terminal run %s", event.Run.ID)
			case telemetry:
				return event, fmt.Errorf("E_JOURNAL_CORRUPT: duplicate telemetry record for %s", event.Run.ID)
			case !envelope:
				return event, fmt.Errorf("E_JOURNAL_CORRUPT: telemetry without initial envelope for %s", event.Run.ID)
			case !validAuxTelemetryPayload(event.Run):
				return event, fmt.Errorf("E_JOURNAL_CORRUPT: invalid telemetry payload for %s", event.Run.ID)
			}
		}
		if len(events) != 0 {
			event.Sequence = events[len(events)-1].Sequence + 1
		} else {
			event.Sequence = 1
		}
	}
	event.SchemaVersion = ledgerSchema
	// The event sequence is part of the durable Run identity for kill intent;
	// write it into the payload before marshaling, not only into the returned
	// caller snapshot.
	if event.Kind == "kill-intent" && event.Run.KillIntent.Present && event.Run.KillIntent.Sequence == 0 {
		event.Run.KillIntent.Sequence = event.Sequence
	}
	if event.Kind == "kill-intent" && event.Run.KillIntent.Present {
		event.Run.ScopeKill.Requested = true
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return event, err
	}
	f, err := os.OpenFile(l.ledger, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return event, err
	}
	data := frame(payload)
	for len(data) > 0 {
		n, writeErr := f.Write(data)
		if writeErr != nil {
			_ = f.Close()
			return event, writeErr
		}
		data = data[n:]
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return event, err
	}
	if err := f.Close(); err != nil {
		return event, err
	}
	if err := syncDir(filepath.Dir(l.ledger)); err != nil {
		return event, err
	}
	return event, nil
}

func (l *ledger) read() ([]ledgerEvent, error) {
	f, err := os.Open(l.ledger)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("E_RUN_RECONCILE_REQUIRED: %w", err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var events []ledgerEvent
	var prior uint64
	for {
		n, readErr := binary.ReadUvarint(r)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil || n == 0 || n > 16<<20 {
			return nil, fmt.Errorf("U_RUN_RECONCILE_REQUIRED: torn ledger length")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("U_RUN_RECONCILE_REQUIRED: torn ledger payload: %w", err)
		}
		var want [sha256.Size]byte
		if _, err := io.ReadFull(r, want[:]); err != nil {
			return nil, fmt.Errorf("U_RUN_RECONCILE_REQUIRED: torn ledger checksum: %w", err)
		}
		got := sha256.Sum256(payload)
		if !equalBytes(got[:], want[:]) {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: run ledger checksum mismatch")
		}
		var event ledgerEvent
		dec := json.NewDecoder(bytesReader(payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&event); err != nil || event.SchemaVersion != ledgerSchema || event.Sequence == 0 || event.Sequence <= prior || event.Kind == "" {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: invalid run ledger record")
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: trailing ledger payload")
		}
		if event.Kind != "telemetry" {
			normalizeBuffering(&event.Run)
			normalizeAdmission(&event.Run)
		}
		prior = event.Sequence
		events = append(events, event)
	}
	return events, nil
}

// tiny reader avoids exposing bytes.Buffer in the protocol code.
type byteReader struct {
	data []byte
	off  int
}

func bytesReader(data []byte) io.Reader { return &byteReader{data: data} }
func (r *byteReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}

func replay(events []ledgerEvent) (map[string]RunRecord, error) {
	runs := make(map[string]RunRecord)
	terminals := make(map[string]bool)
	telemetryEvents := make(map[string]bool)
	for _, e := range events {
		if e.Kind != "telemetry" {
			normalizeBuffering(&e.Run)
			normalizeAdmission(&e.Run)
		}
		if e.Run.ID == "" {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: empty run id")
		}
		if terminals[e.Run.ID] {
			if e.Kind != "telemetry" || telemetryEvents[e.Run.ID] {
				return nil, fmt.Errorf("E_JOURNAL_CORRUPT: record after terminal run %s", e.Run.ID)
			}
			prior := runs[e.Run.ID]
			if prior.Telemetry == "" || !validAuxTelemetryPayload(e.Run) {
				return nil, fmt.Errorf("E_JOURNAL_CORRUPT: invalid telemetry payload for %s", e.Run.ID)
			}
			prior.Telemetry = e.Run.Telemetry
			prior.TelemetryRefs = append([]string(nil), e.Run.TelemetryRefs...)
			runs[e.Run.ID] = prior
			telemetryEvents[e.Run.ID] = true
			continue
		}
		if e.Kind == "telemetry" {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: telemetry before terminal run %s", e.Run.ID)
		}
		if e.Kind == "terminal" {
			if e.Run.Status == StatusStarting || e.Run.Status == StatusRunning {
				return nil, fmt.Errorf("E_JOURNAL_CORRUPT: non-terminal state in terminal slot")
			}
			terminals[e.Run.ID] = true
		}
		if prior, ok := runs[e.Run.ID]; ok && e.Run.Status == StatusStarting && prior.Status != StatusStarting {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: lifecycle reordered")
		}
		if e.Kind == "leader-exited" {
			prior := runs[e.Run.ID]
			candidate := prior
			candidate.LeaderExitObserved = e.LeaderExitObserved
			candidate.ExitCode, candidate.Signal = e.ExitCode, e.Signal
			if !prior.LeaderExitObserved && (!candidate.LeaderExitObserved || (candidate.ExitCode == nil && candidate.Signal == "")) {
				return nil, fmt.Errorf("E_JOURNAL_CORRUPT: invalid leader-exited payload for %s", e.Run.ID)
			}
			runs[e.Run.ID] = mergeEvidence(prior, candidate)
			continue
		}
		if e.Kind == "quiesce-forced" {
			prior := runs[e.Run.ID]
			candidate := e.Run
			candidate.QuiesceForced = true
			runs[e.Run.ID] = mergeEvidence(prior, candidate)
			continue
		}
		runs[e.Run.ID] = e.Run
	}
	return runs, nil
}

func validAuxTelemetryPayload(record RunRecord) bool {
	if record.ID == "" || record.Telemetry == "" {
		return false
	}
	expected := RunRecord{ID: record.ID, Telemetry: record.Telemetry, TelemetryRefs: append([]string(nil), record.TelemetryRefs...)}
	return reflect.DeepEqual(record, expected)
}

func normalizeBuffering(record *RunRecord) {
	if record.Buffering == "" {
		record.Buffering = "none"
	}
}

func normalizeAdmission(record *RunRecord) {
	if record.Admission == "" {
		record.Admission = "disabled"
	}
}

func (l *ledger) current(id string) (RunRecord, error) {
	events, err := l.read()
	if err != nil {
		return RunRecord{}, err
	}
	runs, err := replay(events)
	if err != nil {
		return RunRecord{}, err
	}
	r, ok := runs[id]
	if !ok {
		return RunRecord{}, &LaunchError{"E_RUN_NOT_FOUND", errors.New(id)}
	}
	return r, nil
}

func (l *ledger) nextSequence() (uint64, error) {
	events, err := l.read()
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 1, nil
	}
	return events[len(events)-1].Sequence + 1, nil
}

func (l *ledger) project(ctx context.Context) error {
	events, err := l.read()
	if err != nil {
		return err
	}
	runs, err := replay(events)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.projection), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", l.projection+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, status TEXT NOT NULL, terminal INTEGER NOT NULL, record_json BLOB NOT NULL)`); err != nil {
		return err
	}
	for column, kind := range map[string]string{"owner": "TEXT", "stolen_by": "TEXT", "peak_rss": "INTEGER", "cpu_user": "INTEGER", "cpu_sys": "INTEGER", "admission": "TEXT", "admission_reason": "TEXT", "admission_waited_ms": "INTEGER", "telemetry": "TEXT", "telemetry_refs": "BLOB"} {
		if err := ensureRunColumn(ctx, db, column, kind); err != nil {
			return err
		}
	}
	for _, r := range runs {
		data, _ := json.Marshal(r)
		refs, _ := json.Marshal(r.TelemetryRefs)
		if _, err = db.ExecContext(ctx, `INSERT INTO runs(id,status,terminal,record_json,owner,stolen_by,peak_rss,cpu_user,cpu_sys,admission,admission_reason,admission_waited_ms,telemetry,telemetry_refs) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,terminal=excluded.terminal,record_json=excluded.record_json,owner=excluded.owner,stolen_by=excluded.stolen_by,peak_rss=excluded.peak_rss,cpu_user=excluded.cpu_user,cpu_sys=excluded.cpu_sys,admission=excluded.admission,admission_reason=excluded.admission_reason,admission_waited_ms=excluded.admission_waited_ms,telemetry=excluded.telemetry,telemetry_refs=excluded.telemetry_refs`, r.ID, r.Status, r.Status.Terminal(), data, nullableString(r.Owner), nullableString(r.StolenBy), nullableMetric(r.PeakRSS), nullableMetric(r.CPUUser), nullableMetric(r.CPUSys), r.Admission, nullableString(r.AdmissionReason), r.AdmissionWaitedMS, nullableString(r.Telemetry), refs); err != nil {
			return err
		}
	}
	return db.Close()
}

func ensureRunColumn(ctx context.Context, db *sql.DB, name, kind string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(runs)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var column, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if column == name {
			found = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN `+name+` `+kind)
	return err
}

func nullableMetric(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (l *ledger) rebuild(ctx context.Context) error { return l.project(ctx) }

func nowString(now func() time.Time) string {
	if now == nil {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return now().UTC().Format(time.RFC3339Nano)
}

func digestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

var _ = syscall.O_CLOEXEC
