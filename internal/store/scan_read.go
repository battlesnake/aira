package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// scanReadOutcome describes whether a live-file read established a coherent
// value. It is deliberately separate from error: an external writer can make
// a scan inconclusive without making the store unhealthy.
type scanReadOutcome uint8

const (
	scanReadStable scanReadOutcome = iota
	scanReadInconclusive
)

// scanReadHook is a test-only seam. It runs after the first read and before
// the second read of each stability attempt, which lets tests model a writer
// changing the file between the two observations.
var scanReadHook func()
var scanTicketsHook func()

func indexUnestablishedError() error {
	return errors.New("U_INDEX_UNESTABLISHED: working-tree scan was inconclusive; retry")
}

func relationGraphUnestablishedError() error {
	return errors.New("U_RELATION_GRAPH_UNESTABLISHED: working-tree relation graph was inconclusive; retry")
}

const (
	stableReadAttempts = 4
	stableReadBackoff  = time.Millisecond
)

func scanEntityNames(entries []os.DirEntry) map[string]struct{} {
	result := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".md") {
			result[entry.Name()] = struct{}{}
		}
	}
	return result
}

func sameScanEntityNames(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for name := range left {
		if _, ok := right[name]; !ok {
			return false
		}
	}
	return true
}

// stableReadFile reads path twice and accepts the bytes only when both reads
// are identical. A transient mismatch is retried a bounded number of times;
// a vanished file is inconclusive rather than an I/O failure. O_NOFOLLOW is
// used for both opens so a path swapped to a symlink after Lstat is never
// followed.
func stableReadFile(path string) ([]byte, scanReadOutcome, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, scanReadInconclusive, nil
		}
		return nil, scanReadStable, err
	}

	for attempt := 0; attempt < stableReadAttempts; attempt++ {
		first, err := readScanFileOnce(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, scanReadInconclusive, nil
			}
			return nil, scanReadStable, err
		}
		if scanReadHook != nil {
			scanReadHook()
		}
		second, err := readScanFileOnce(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, scanReadInconclusive, nil
			}
			return nil, scanReadStable, err
		}
		if bytes.Equal(first, second) {
			return first, scanReadStable, nil
		}
		if attempt+1 < stableReadAttempts {
			time.Sleep(stableReadBackoff)
		}
	}
	return nil, scanReadInconclusive, nil
}

func readScanFileOnce(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("scan read: could not create file handle")
	}
	defer file.Close()
	return io.ReadAll(file)
}
