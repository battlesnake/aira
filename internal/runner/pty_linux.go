//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type ptyReader struct {
	io.ReadCloser
}

func (r *ptyReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, unix.EIO) {
		return n, io.EOF
	}
	return n, err
}

type captureCompleteness struct {
	incomplete atomic.Bool
}

func (c *captureCompleteness) markIncomplete() { c.incomplete.Store(true) }
func (c *captureCompleteness) complete() bool  { return !c.incomplete.Load() }
func (c *captureCompleteness) observe(result captureResult) {
	if result.Err != nil || result.State != OutputComplete {
		c.markIncomplete()
	}
}

// collectPTYCapture bounds the master join. The incomplete transition occurs
// before Close unblocks Read, so a racing late EIO can never restore complete.
func collectPTYCapture(ctx context.Context, ch <-chan captureResult, count int, grace time.Duration, readers []io.Closer, completeness *captureCompleteness) ([]captureResult, bool) {
	result := make([]captureResult, 0, count)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	forced := false
	for len(result) < count && !forced {
		select {
		case item := <-ch:
			if !completeness.complete() {
				item.State = OutputPartial
			}
			result = append(result, item)
			completeness.observe(item)
		case <-timer.C:
			forced = true
		case <-ctx.Done():
			forced = true
		}
	}
	if !forced {
		return result, false
	}
	completeness.markIncomplete()
	for _, reader := range readers {
		_ = reader.Close()
	}
	// Closing the pty master interrupts a blocking master.Read — but the drain
	// goroutine could instead be blocked WRITING the capture file (a stuck/full
	// sink), which the reader close does not unblock. So this SECOND join is ALSO
	// bounded: on its deadline we stop waiting rather than hang Launch forever. A
	// drain still blocked is left to finish on its own (captureCh is buffered, so
	// its eventual send never blocks); its stream's OutputRef keeps the initialised
	// OutputPartial state, which is honest.
	secondary := time.NewTimer(grace)
	defer secondary.Stop()
	for len(result) < count {
		select {
		case item := <-ch:
			item.State = OutputPartial
			result = append(result, item)
			completeness.observe(item)
		case <-secondary.C:
			return result, true
		}
	}
	return result, true
}

var (
	ioctlGetPTPeerFn  = ioctlGetPTPeer
	ioctlGetTermiosFn = unix.IoctlGetTermios
	ioctlSetTermiosFn = unix.IoctlSetTermios
)

func ioctlGetPTPeer(fd int, flags int) (int, error) {
	peer, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCGPTPEER), uintptr(flags))
	if errno != 0 {
		return 0, errno
	}
	return int(peer), nil
}

// allocatePTY creates a race-free ptmx/pts pair. TIOCGPTPEER is deliberately
// the only slave lookup: a path lookup by pts index can cross devpts mounts or
// race index reuse and attach the wrong terminal.
func allocatePTY() (master, slave *os.File, err error) {
	const flags = unix.O_RDWR | unix.O_NOCTTY | unix.O_CLOEXEC
	masterFD, err := unix.Open("/dev/ptmx", flags, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	masterOwned := true
	defer func() {
		if masterOwned {
			_ = unix.Close(masterFD)
		}
	}()

	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}
	slaveFD, err := ioctlGetPTPeerFn(masterFD, flags)
	if err != nil {
		return nil, nil, fmt.Errorf("TIOCGPTPEER: %w", err)
	}
	slaveOwned := true
	defer func() {
		if slaveOwned {
			_ = unix.Close(slaveFD)
		}
	}()

	termios, err := ioctlGetTermiosFn(slaveFD, unix.TCGETS)
	if err != nil {
		return nil, nil, fmt.Errorf("TCGETS: %w", err)
	}
	termios.Oflag &^= unix.OPOST
	if err := ioctlSetTermiosFn(slaveFD, unix.TCSETS, termios); err != nil {
		return nil, nil, fmt.Errorf("TCSETS: %w", err)
	}

	master = os.NewFile(uintptr(masterFD), "/dev/ptmx")
	slave = os.NewFile(uintptr(slaveFD), "pts")
	if master == nil || slave == nil {
		return nil, nil, fmt.Errorf("wrap pty descriptors")
	}
	masterOwned, slaveOwned = false, false
	return master, slave, nil
}
