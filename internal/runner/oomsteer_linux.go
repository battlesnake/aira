//go:build linux

package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// confineOOMSteerMaxDepth bounds the subtree walk exactly as the reaper bounds
// its own (confineReapMaxDepth). A cgroup tree deeper than this is a tree AIRA
// did not build.
const confineOOMSteerMaxDepth = 32

// oomSteerOpenat is a seam for the O_NOFOLLOW flag unit test. Production always
// uses unix.Openat.
var oomSteerOpenat = unix.Openat

// oomSteerWritePID is a seam so the walker's arithmetic can be tested without a
// real /proc write. Production always uses writePIDOOMScoreAdj.
var oomSteerWritePID = writePIDOOMScoreAdj

// oomSteerReadProcs is a seam for the PID-REUSE guard's own test. That guard
// defends against a STALE cgroup.procs reading — a pid that has exited and been
// recycled between the read and the write — and the kernel will not produce that
// state on demand, so the only honest way to exercise it is to hand the walker
// the stale list the race would have produced. Production always uses
// readOOMSteerProcs.
var oomSteerReadProcs = readOOMSteerProcs

func setSubtreeOOMScoreAdj(scopePath string, adj int) (OOMScoreSteerResult, error) {
	scopePath = filepath.Clean(scopePath)
	scopeDir := filepath.Base(scopePath)
	if scopeDir == "." || scopeDir == string(filepath.Separator) {
		return OOMScoreSteerResult{}, errors.New("E_CONFINE_ARGUMENT_INVALID: scope path has no directory name")
	}
	root, err := openOOMSteerDirectory(unix.AT_FDCWD, scopePath)
	if err != nil {
		// The scope vanished, or was never there. Reported as an error rather
		// than an empty success so the caller can drop its state for the scope
		// instead of believing it steered a tree that is not there.
		return OOMScoreSteerResult{}, err
	}
	defer root.Close()
	result := OOMScoreSteerResult{}
	value := []byte(strconv.Itoa(adj) + "\n")
	walkOOMSteerTree(root, scopeDir, value, 0, &result)
	return result, nil
}

func openOOMSteerDirectory(parentFD int, name string) (*os.File, error) {
	fd, err := oomSteerOpenat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open confine scope directory")
	}
	return file, nil
}

// walkOOMSteerTree is best-effort by construction: an unreadable child cgroup
// or an exited pid must never abandon the rest of the walk, because a partially
// steered scope is strictly better than an unsteered one and the caller is told
// exactly how much was done. It therefore records counts and returns nothing.
func walkOOMSteerTree(dir *os.File, scopeDir string, value []byte, depth int, result *OOMScoreSteerResult) {
	result.Cgroups++
	for _, pid := range oomSteerReadProcs(dir) {
		result.PIDs++
		// PID-REUSE GUARD. cgroup.procs was read a moment ago; by the time the
		// write lands that pid may have exited and been recycled by an unrelated
		// process, which would then be handed a 1000. Re-checking membership from
		// /proc/<pid>/cgroup before writing closes all but a vanishingly small
		// residual window, and every remaining outcome is a refusal rather than a
		// wrong write.
		if !pidInConfineScope(pid, scopeDir) {
			result.Skipped++
			continue
		}
		if err := oomSteerWritePID(pid, value); err != nil {
			result.Failed++
			continue
		}
		result.Written++
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return
	}
	if depth >= confineOOMSteerMaxDepth {
		return
	}
	for _, entry := range entries {
		// A cgroup directory holds many regular interface files; directories are
		// the only child cgroups, and a symlink is never followed.
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		child, err := openOOMSteerDirectory(int(dir.Fd()), entry.Name())
		if err != nil {
			continue
		}
		walkOOMSteerTree(child, scopeDir, value, depth+1, result)
		_ = child.Close()
	}
}

func readOOMSteerProcs(dir *os.File) []int {
	fd, err := oomSteerOpenat(int(dir.Fd()), "cgroup.procs", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	file := os.NewFile(uintptr(fd), "cgroup.procs")
	if file == nil {
		_ = unix.Close(fd)
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// pidInConfineScope reports whether this pid's own cgroup path still passes
// through the named confine scope directory.
//
// It matches a whole PATH ELEMENT, never a substring, so a scope whose name
// merely contains another's id can never be mistaken for it — the same rule
// exclusiveDeniesWorkerAdmit applies for the same reason. Element matching also
// makes the check mount-point agnostic: /proc/<pid>/cgroup reports a path
// relative to the cgroup-v2 root, which is not the filesystem path the caller
// holds, and a confine scope id embeds the minting pid and a nanosecond stamp,
// so one element is already a unique identity.
//
// Fail-CLOSED: an unreadable or unrecognised /proc entry returns false, so the
// write is skipped. The cost of skipping is one unsteered process; the cost of
// writing is a 1000 on a process AIRA does not own.
func pidInConfineScope(pid int, scopeDir string) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !ok {
			continue
		}
		for _, element := range strings.Split(path, "/") {
			if element == scopeDir {
				return true
			}
		}
	}
	return false
}

// writePIDOOMScoreAdj opens without O_CREATE deliberately: the target must be
// an existing procfs file, never something this daemon brings into being.
func writePIDOOMScoreAdj(pid int, value []byte) error {
	file, err := os.OpenFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(value)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
