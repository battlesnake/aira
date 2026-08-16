package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// fakeSQLiteError mimics *modernc.org/sqlite.Error (whose fields are unexported and
// so cannot be constructed here) by exposing a numeric SQLite result code. It is
// how the retry tests inject a *typed* disk I/O fault through the openOnceFn seam.
type fakeSQLiteError struct {
	code int
	msg  string
}

func (e fakeSQLiteError) Error() string { return e.msg }
func (e fakeSQLiteError) Code() int     { return e.code }

func diskIOError() error {
	return fakeSQLiteError{code: sqlite3.SQLITE_IOERR_WRITE, msg: "disk I/O error (778)"}
}

// retryTestOptions builds a minimal valid Options rooted at dir for the open-retry
// tests, matching the canonical shape used across the store test suite.
func retryTestOptions(dir string) Options {
	return Options{
		Root:         dir,
		CommonDir:    filepath.Join(dir, "common"),
		DBPath:       filepath.Join(dir, "state", "state.db"),
		RegistryPath: filepath.Join(dir, "state", "registry.jsonl"),
		ProjectID:    "project-aira",
		WorktreeID:   "main",
		ProjectSlug:  "aira",
		Prefixes:     []string{"AIRA"},
	}
}

// verifies: Open retries a transient SQLITE_IOERR (disk I/O error) up to the
// bounded budget and succeeds once the underlying open stops failing.
func TestOpenRetriesTransientDiskIOError(t *testing.T) {
	restore := openOnceFn
	t.Cleanup(func() { openOnceFn = restore })

	var calls int32
	openOnceFn = func(ctx context.Context, dbPath, registryPath string) (*DB, error) {
		if atomic.AddInt32(&calls, 1) < int32(storeOpenRetries) {
			return nil, diskIOError()
		}
		return restore(ctx, dbPath, registryPath)
	}

	dir := t.TempDir()
	s, err := Open(context.Background(), retryTestOptions(dir))
	if err != nil {
		t.Fatalf("Open after transient faults: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := atomic.LoadInt32(&calls); got != int32(storeOpenRetries) {
		t.Fatalf("openOnce called %d times, want %d", got, storeOpenRetries)
	}
}

// verifies: a non-transient error is returned immediately, without consuming the
// retry budget — Open must never mask a persistent (e.g. config) failure.
func TestOpenDoesNotRetryNonTransient(t *testing.T) {
	restore := openOnceFn
	t.Cleanup(func() { openOnceFn = restore })

	var calls int32
	openOnceFn = func(context.Context, string, string) (*DB, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("E_CONFIG_INVALID: nope")
	}

	if _, err := Open(context.Background(), retryTestOptions(t.TempDir())); err == nil {
		t.Fatal("Open: want error, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("openOnce called %d times for a non-transient error, want 1", got)
	}
}

// verifies: a disk I/O error that persists past the whole budget surfaces the last
// error rather than looping forever or reporting success.
func TestOpenExhaustsRetryBudget(t *testing.T) {
	restore := openOnceFn
	t.Cleanup(func() { openOnceFn = restore })

	var calls int32
	openOnceFn = func(context.Context, string, string) (*DB, error) {
		atomic.AddInt32(&calls, 1)
		return nil, diskIOError()
	}

	_, err := Open(context.Background(), retryTestOptions(t.TempDir()))
	if !isTransientDiskIOError(err) {
		t.Fatalf("Open: want persistent disk I/O error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(storeOpenRetries) {
		t.Fatalf("openOnce called %d times, want %d", got, storeOpenRetries)
	}
}

// verifies: a cancelled context stops retrying at once — Open makes a single
// attempt and does not burn the retry budget or block on a backoff.
func TestOpenStopsOnCancelledContext(t *testing.T) {
	restore := openOnceFn
	t.Cleanup(func() { openOnceFn = restore })

	var calls int32
	openOnceFn = func(context.Context, string, string) (*DB, error) {
		atomic.AddInt32(&calls, 1)
		return nil, diskIOError()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, retryTestOptions(t.TempDir())); err == nil {
		t.Fatal("Open: want error, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("openOnce called %d times under a cancelled context, want 1", got)
	}
}

// verifies: cancellation arriving *during* a backoff (not before the first
// attempt) is honoured promptly — the loop breaks out of the interruptible sleep
// instead of exhausting the whole retry budget.
func TestOpenCancelDuringBackoff(t *testing.T) {
	restore := openOnceFn
	t.Cleanup(func() { openOnceFn = restore })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	called := make(chan struct{}, 1)
	var calls int32
	openOnceFn = func(context.Context, string, string) (*DB, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case called <- struct{}{}:
		default:
		}
		return nil, diskIOError()
	}
	go func() {
		<-called // first attempt has failed; we are entering the backoff
		cancel()
	}()

	if _, err := Open(ctx, retryTestOptions(t.TempDir())); err == nil {
		t.Fatal("Open: want error, got nil")
	}
	if got := atomic.LoadInt32(&calls); got >= int32(storeOpenRetries) {
		t.Fatalf("openOnce called %d times; cancellation during backoff should stop before exhausting %d", got, storeOpenRetries)
	}
}

// verifies: the classifier matches only the SQLite disk-I/O family by numeric
// result code, and — crucially — NOT a plain error whose message merely contains
// "disk I/O error", nor configuration/constraint/corruption errors.
func TestIsTransientDiskIOError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ioerr-write", fakeSQLiteError{code: sqlite3.SQLITE_IOERR_WRITE, msg: "disk I/O error"}, true},
		{"ioerr-primary", fakeSQLiteError{code: sqlite3.SQLITE_IOERR, msg: "disk I/O error"}, true},
		{"ioerr-wrapped", fmt.Errorf("open store: %w", fakeSQLiteError{code: sqlite3.SQLITE_IOERR_WRITE}), true},
		{"constraint", fakeSQLiteError{code: sqlite3.SQLITE_CONSTRAINT, msg: "UNIQUE constraint failed"}, false},
		{"corrupt", fakeSQLiteError{code: sqlite3.SQLITE_CORRUPT, msg: "database disk image is malformed"}, false},
		{"text-only-not-typed", errors.New("disk I/O error (778)"), false},
		{"config", errors.New("E_CONFIG_INVALID: store options are incomplete"), false},
	}
	for _, c := range cases {
		if got := isTransientDiskIOError(c.err); got != c.want {
			t.Fatalf("%s: isTransientDiskIOError(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
