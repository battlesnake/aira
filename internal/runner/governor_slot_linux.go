//go:build linux

package runner

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func setGovernorParentDeathSignal() bool {
	parent := os.Getppid()
	if parent <= 1 {
		return false
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0); err != nil {
		return false
	}
	// The parent could have exited between Getppid and prctl. Kill ourselves so
	// the daemon-held connection cannot outlive the worker in that small race.
	if os.Getppid() != parent {
		_ = unix.Kill(unix.Getpid(), unix.SIGKILL)
	}
	return true
}
