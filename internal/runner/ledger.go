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
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const ledgerSchema = 1

type ledgerEvent struct {
	SchemaVersion int       `json:"schema_version"`
	Sequence      uint64    `json:"sequence"`
	Kind          string    `json:"kind"`
	Run           RunRecord `json:"run"`
	WaitExit      *int      `json:"wait_exit,omitempty"`
	WaitSignal    string    `json:"wait_signal,omitempty"`
	WaitObserved  bool      `json:"wait_observed,omitempty"`
	KillCompleted bool      `json:"kill_completed,omitempty"`
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
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
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
		for _, prior := range events {
			if prior.Run.ID == event.Run.ID && prior.Kind == "terminal" {
				return event, fmt.Errorf("E_JOURNAL_CORRUPT: duplicate terminal record for %s", event.Run.ID)
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
	for _, e := range events {
		if e.Run.ID == "" {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: empty run id")
		}
		if terminals[e.Run.ID] {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: record after terminal run %s", e.Run.ID)
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
		runs[e.Run.ID] = e.Run
	}
	return runs, nil
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
	for _, r := range runs {
		data, _ := json.Marshal(r)
		if _, err = db.ExecContext(ctx, `INSERT INTO runs(id,status,terminal,record_json) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,terminal=excluded.terminal,record_json=excluded.record_json`, r.ID, r.Status, r.Status.Terminal(), data); err != nil {
			return err
		}
	}
	return db.Close()
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
