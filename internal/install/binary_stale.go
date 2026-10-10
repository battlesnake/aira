package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// AIRA-203. Would a restart run different bytes from the ones running now?
//
// `aira install` used to restart a present daemon only when the unit TEXT changed
// (or the CPU ratio did), so a rebuilt binary at the same path left the old
// daemon running behind a green install and every client then failed
// E_DAEMON_PROTOCOL until someone restarted it by hand. The question is asked
// directly: the executable of the process `systemctl restart` would replace (the
// service's MainPID, /proc/<pid>/exe) against the file the unit's ExecStart names.

// liveDaemonBinaryStale reports whether the present daemon service is running a
// binary other than `executable` (the absolute path rendered into ExecStart=),
// with the reason. "Cannot tell" is stale too, and says so: a needless restart is
// a short admission blip with jobs kept, a missed one is an outage.
//
// The inspected process is the service's MainPID, read the way
// verifyDaemonReachable reads it, and NOT the daemon lock's PID: daemon.Status
// reports Running only for a positive lock PID with a matching boot id, so a held
// lock with missing or corrupt metadata would read as "not running" and skip the
// check; and the lock holder need not be the process a restart replaces.
func liveDaemonBinaryStale(d installDeps, executable string) (bool, string) {
	const unevaluated = "the running daemon's binary is unevaluated: "
	out, err := d.run([]string{"systemctl", "--user", "show", "-p", "MainPID", "--value", d.daemonUnit}, nil)
	if err != nil {
		return true, unevaluated + "MainPID is unreadable: " + err.Error()
	}
	value := strings.TrimSpace(string(out))
	pid, err := strconv.Atoi(value)
	if err != nil {
		return true, unevaluated + fmt.Sprintf("MainPID is unreadable: %q is not a process id", value)
	}
	if pid <= 0 {
		return true, unevaluated + "the service reports no main process"
	}
	same, err := d.sameFileContent(fmt.Sprintf("/proc/%d/exe", pid), executable)
	if err != nil {
		return true, unevaluated + err.Error()
	}
	if !same {
		return true, "the running daemon's binary differs from " + executable
	}
	return false, ""
}

// defaultSameFileContent reports whether the files at a and b hold the same
// bytes. The same inode is the fast path only: install.sh copies a NEW inode on
// every run, so identical bytes at a different inode must still compare equal
// (no restart on every convergence run), and an in-place rewrite of one inode is
// never mistaken for "same file" by anything but os.SameFile itself. Any open or
// read error is returned as an error, never as "same".
func defaultSameFileContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	sa, err := fa.Stat()
	if err != nil {
		return false, err
	}
	sb, err := fb.Stat()
	if err != nil {
		return false, err
	}
	if os.SameFile(sa, sb) {
		return true, nil
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}
	const chunk = 64 * 1024
	bufA, bufB := make([]byte, chunk), make([]byte, chunk)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		endA := errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF)
		endB := errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF)
		if errA != nil && !endA {
			return false, errA
		}
		if errB != nil && !endB {
			return false, errB
		}
		if endA || endB {
			return endA == endB, nil
		}
	}
}
