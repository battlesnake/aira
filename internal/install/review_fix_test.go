//go:build linux

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestReadRegularUnitAtRefusesFIFO guards the build-review P2: a FIFO planted at
// a unit name must be REFUSED as non-regular, not hang the installer. Before
// O_NONBLOCK was added, openat(O_RDONLY|O_NOFOLLOW) blocked forever on a
// writerless FIFO (O_NOFOLLOW rejects symlinks but not FIFOs), so
// validateRegularFD was never reached. RED (times out) against the pre-fix impl.
func TestReadRegularUnitAtRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "aira.slice"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	dirfd, err := unix.Openat(unix.AT_FDCWD, dir,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer unix.Close(dirfd)

	d := realInstallDeps() // real unix.Openat/Fstat/Read
	uid := os.Geteuid()

	ch := make(chan error, 1)
	go func() {
		_, _, e := readRegularUnitAt(d, dirfd, uid, "aira.slice", false)
		ch <- e
	}()

	select {
	case e := <-ch:
		if e == nil || !strings.Contains(e.Error(), "refusing non-regular") {
			t.Fatalf("want non-regular refusal, got err=%v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readRegularUnitAt BLOCKED on a writerless FIFO (missing O_NONBLOCK)")
	}
}

// TestInstallMalformedMeminfoIsUnavailableNotArgumentInvalid guards the
// build-review P2: when the ⅔-MemTotal default is needed but /proc/meminfo is
// readable-yet-malformed (no MemTotal line), the failure is an environment
// problem (E_INSTALL_UNAVAILABLE), not a bad user argument
// (E_INSTALL_ARGUMENT_INVALID). RED against the pre-fix impl which let a 0
// MemTotal fall through to a "0G" cap that failed the floor and was misreported.
func TestInstallMalformedMeminfoIsUnavailableNotArgumentInvalid(t *testing.T) {
	d, _ := newFakeInstall(t)
	prev := d.readFile
	d.readFile = func(path string) ([]byte, error) {
		if path == "/proc/meminfo" {
			return []byte("MemFree:  1024 kB\nCached:  2048 kB\n"), nil // no MemTotal
		}
		return prev(path)
	}
	// No --memory-max and a fresh install (no installed aira.slice) → the
	// ⅔-default is needed and depends solely on MemTotal.
	err := runInstall(d, installOpts{})
	if err == nil {
		t.Fatal("expected an error from an unusable /proc/meminfo, got nil")
	}
	if strings.Contains(err.Error(), CodeArgumentInvalid) {
		t.Fatalf("malformed environment misattributed to a bad user argument: %v", err)
	}
	if !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("want %s for an unusable /proc/meminfo, got %v", CodeUnavailable, err)
	}
}
