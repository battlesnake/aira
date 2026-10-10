//go:build linux

package runner

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// writeConfineSummaryFn is the single write(2) the summary line is appended with,
// a seam so tests can count writes and inject EIO or a short write.
var writeConfineSummaryFn = func(fd int, line []byte) (int, error) { return unix.Write(fd, line) }

// openConfineSummaryFile opens (creating if absent) the --summary-file target
// for append and returns a RAW fd. It is raw on purpose: os.NewFile would try to
// register a non-blocking fd with Go's poller.
//
//   - O_APPEND makes each write(2) land at end-of-file under the inode lock, so
//     concurrent jobs sharing one file do not interleave their lines (a Linux
//     property of local regular files; it does not hold on NFS).
//   - O_NONBLOCK so a FIFO at the path returns at once (ENXIO, or it opens and is
//     refused below as not regular) instead of blocking the launch waiting for a
//     reader; internal/install uses it for the same reason.
//   - O_CLOEXEC keeps the fd out of the job.
//   - Mode 0o666 under the umask, exactly as the shell's `>>`. Symlinks are
//     followed, as `>>` follows them: the path is the caller's own.
//
// A target that is not a regular file is closed and refused.
func openConfineSummaryFile(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o666)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return -1, errors.New("not a regular file")
	}
	return fd, nil
}

// appendConfineSummary makes exactly ONE write of the whole line and never
// retries: a retried short write could interleave with another writer's line.
func appendConfineSummary(fd int, line []byte) error {
	n, err := writeConfineSummaryFn(fd, line)
	if err != nil {
		return err
	}
	if n != len(line) {
		return fmt.Errorf("short write %d of %d bytes; the file may now hold a truncated line that also damages the next appended line", n, len(line))
	}
	return nil
}

func closeConfineSummaryFile(fd int) { _ = unix.Close(fd) }
