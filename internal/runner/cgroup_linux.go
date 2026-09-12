//go:build linux

package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const cloneIntoCgroup = uintptr(1 << 33)

type linuxScopeBackend struct{ parent string }
type linuxScope struct {
	path              string
	fd                *os.File
	events            *os.File
	removedMeansEmpty bool
}

func newDefaultBackend(parent string) ScopeBackend { return &linuxScopeBackend{parent: parent} }

func unifiedMount() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.SplitN(s.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		post := strings.Fields(parts[1])
		pre := strings.Fields(parts[0])
		if len(post) < 1 || len(pre) < 5 || post[0] != "cgroup2" {
			continue
		}
		mountPoint := strings.Join(pre[4:5], " ")
		return unescapeMount(mountPoint), nil
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", errors.New("cgroup-v2 unified mount not found")
}

func unescapeMount(s string) string {
	// mountinfo uses octal escapes for spaces, tabs, backslashes.
	for _, item := range []struct{ from, to string }{{"\\040", " "}, {"\\011", "\t"}, {"\\134", "\\"}} {
		s = strings.ReplaceAll(s, item.from, item.to)
	}
	return s
}

func currentCgroupPath(mount string) (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel := strings.TrimPrefix(line, "0::")
			return filepath.Join(mount, strings.TrimPrefix(rel, "/")), nil
		}
	}
	return "", errors.New("unified cgroup membership not found")
}

func kernelAtLeast514() bool {
	u := unix.Utsname{}
	if err := unix.Uname(&u); err != nil {
		return false
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	parts := strings.Split(string(b), ".")
	if len(parts) < 2 {
		return false
	}
	major, e1 := strconv.Atoi(parts[0])
	minorText := parts[1]
	for i, c := range minorText {
		if c < '0' || c > '9' {
			minorText = minorText[:i]
			break
		}
	}
	minor, e2 := strconv.Atoi(minorText)
	return e1 == nil && e2 == nil && (major > 5 || (major == 5 && minor >= 14))
}

func clone3Available() bool {
	// size=0 is deliberately rejected before clone creation. EINVAL means the
	// syscall is present; ENOSYS/EPERM means the platform or seccomp policy does
	// not provide the required primitive. The real Start call is the final
	// valid CLONE_INTO_CGROUP capability check.
	_, _, errno := unix.RawSyscall6(unix.SYS_CLONE3, 0, 0, 0, 0, 0, 0)
	return errno == unix.EINVAL
}

func (b *linuxScopeBackend) Probe(ctx context.Context) error {
	if !kernelAtLeast514() {
		return errors.New("kernel < 5.14")
	}
	mount, err := unifiedMount()
	if err != nil {
		return err
	}
	parent := b.parent
	if parent == "" {
		parent, err = currentCgroupPath(mount)
		if err != nil {
			return err
		}
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return err
	}
	if filepath.Clean(parent) == filepath.Clean(mount) {
		return errors.New("delegated parent may not be the unified root")
	}
	if err := ensureParent(parent); err != nil {
		return err
	}
	if !clone3Available() {
		return errors.New("clone3 unavailable or denied")
	}
	b.parent = parent
	return nil
}

func ensureParent(parent string) error {
	st, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("cgroup parent is not a directory")
	}
	// A delegated parent must permit private children. Probe creation is
	// removed before returning; no process is ever moved by this probe.
	probe := filepath.Join(parent, ".aira-probe-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Mkdir(probe, 0o755); err != nil {
		return err
	}
	defer os.Remove(probe)
	kill := filepath.Join(probe, "cgroup.kill")
	f, err := os.OpenFile(kill, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_ = f.Close()
	return nil
}

func (b *linuxScopeBackend) Create(ctx context.Context, id string) (Scope, error) {
	mount, err := unifiedMount()
	if err != nil {
		return nil, err
	}
	parent := b.parent
	if parent == "" {
		parent, err = currentCgroupPath(mount)
		if err != nil {
			return nil, err
		}
	}
	if filepath.IsAbs(parent) == false {
		return nil, errors.New("cgroup parent must be absolute")
	}
	cleanParent := filepath.Clean(parent)
	if filepath.Base(cleanParent) == "." || strings.Contains(id, "/") {
		return nil, errors.New("invalid scope id")
	}
	path := filepath.Join(cleanParent, ".aira-"+id)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, err
	}
	fd, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	kill, err := os.OpenFile(filepath.Join(path, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		_ = fd.Close()
		_ = os.Remove(path)
		return nil, err
	}
	_ = kill.Close()
	return &linuxScope{path: path, fd: fd}, nil
}

func (b *linuxScopeBackend) Open(ctx context.Context, reference string) (Scope, error) {
	path, err := filepath.Abs(reference)
	if err != nil {
		return nil, err
	}
	fd, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &linuxScope{path: path, fd: fd}, nil
}

func (s *linuxScope) Reference() string { return s.path }
func (s *linuxScope) FD() int           { return int(s.fd.Fd()) }

// Close releases the scope's directory FD. It is deliberately NOT on the Scope
// interface: long-lived holders (a confine job's own scope) keep the FD for the
// life of the job by design. It exists for callers that only needed the scope in
// order to CREATE and configure it and then hand back a path -- CreateWorkerScope.
// Since AIRA-39 that path runs inside the long-lived DAEMON rather than a
// short-lived CLI process, so the FD is no longer reclaimed by process exit, and
// leaning on the *os.File finalizer would let one FD per aitest worker
// accumulate between GCs (found by Sol build-review).
func (s *linuxScope) Close() error       { return s.fd.Close() }
func (s *linuxScope) EventsPath() string { return filepath.Join(s.path, "cgroup.events") }
func (s *linuxScope) openFile(name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(s.FD(), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open scope control file")
	}
	return file, nil
}
func (s *linuxScope) Members() ([]int, error) {
	file, err := s.openFile("cgroup.procs", unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return nil, err
	}
	var result []int
	for _, line := range strings.Fields(string(data)) {
		pid, e := strconv.Atoi(line)
		if e != nil {
			return nil, e
		}
		result = append(result, pid)
	}
	return result, nil
}
func (s *linuxScope) Empty() (bool, error) {
	file := s.events
	closeAfter := false
	if file == nil {
		var err error
		file, err = s.openFile("cgroup.events", unix.O_RDONLY)
		if err != nil {
			return false, err
		}
		closeAfter = true
	}
	if closeAfter {
		defer file.Close()
	}
	data := make([]byte, 4096)
	n, err := unix.Pread(int(file.Fd()), data, 0)
	if s.removedMeansEmpty && (errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT)) {
		// A cgroup directory can be removed only after the kernel has observed it
		// empty. Management enables this inference only after cgroup.kill itself
		// succeeded, so a supervisor teardown racing the poll is an equally strong
		// empty attestation rather than a fabricated success.
		return true, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	data = data[:n]
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] == "0", nil
		}
	}
	return false, errors.New("cgroup.events lacks populated")
}
func (s *linuxScope) Terminate(pids []int) error {
	// Never trust a stale Members() result. Refresh immediately before each
	// signal so an exited PID cannot be reused as the grace-period target.
	for _, pid := range pids {
		current, err := s.Members()
		if err != nil {
			return err
		}
		if !memberStillPresent(current, pid) {
			continue
		}
		if err := unix.Kill(pid, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	return nil
}
func memberStillPresent(current []int, pid int) bool { return containsPID(current, pid) }
func (s *linuxScope) Kill() error {
	f, err := s.openFile("cgroup.kill", unix.O_WRONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("1"); err != nil {
		return err
	}
	return f.Sync()
}
func (s *linuxScope) Remove() error {
	if empty, err := s.Empty(); err != nil {
		return err
	} else if !empty {
		return errors.New("scope is not empty")
	}
	// AIRA-144. Empty() proves no PROCESS remains anywhere in the subtree
	// (cgroup.events `populated` is subtree-aware); it says nothing about the
	// child cgroup DIRECTORIES the workload created inside this scope, which
	// cgroup.kill does not remove. rmdir refuses a cgroup that still has a child
	// cgroup, even an empty one, so without this the rmdir below fails EBUSY and
	// the whole tree is left on the slice — observed as `.aira-RUN-8` plus
	// `.aira-RUN-8/.aira-nested`, both reading `populated 0`, after AIRA-140's
	// nested kill, and equally after any nested job that exits normally.
	if err := s.removeChildCgroups(); err != nil {
		return err
	}
	if err := s.fd.Close(); err != nil {
		return err
	}
	return os.Remove(s.path)
}

// removeChildCgroups rmdirs every descendant cgroup directory inside the scope,
// deepest-first, leaving the scope itself for Remove's own rmdir.
//
// Deepest-first is the whole point: a child that has children of its own (a
// podman --cgroups=split container, or any workload that nests more than one
// level) cannot be rmdir'd until its
// own children are gone, so a single-level sweep would still leave the tree
// behind. internal/cgrouptest's removeScopeTree makes the same point for tests.
//
// The walk is AIRA-72's reaper walk, reused rather than re-derived: the whole
// post-order plan is read before the first unlink, every Openat and Unlinkat is
// anchored to an already-open O_NOFOLLOW directory fd rather than to a rebuilt
// path, and the depth is bounded. Its helpers carry `confine` in their names
// because the reaper was their first caller; nothing in them is confine-specific.
//
// Removal is not a second emptiness proof and does not weaken the first one: the
// kernel refuses Unlinkat(AT_REMOVEDIR) on anything populated, so a child
// repopulated between Empty() and here fails the unlink and Remove returns that
// error rather than reporting a teardown that did not happen.
func (s *linuxScope) removeChildCgroups() error {
	// A duplicate directory fd: readConfineReapTree closes every fd it owns, and
	// the scope's own fd outlives this call (Remove closes it, and a failure here
	// must leave the scope usable).
	dir, err := openConfineReapDirectory(s.FD(), ".")
	if err != nil {
		return err
	}
	tree, err := readConfineReapTree(dir, ".", 0)
	if err != nil {
		return err // readConfineReapTree closed every owned fd.
	}
	defer tree.close()
	for _, child := range tree.children {
		if err := removeConfineReapTree(int(tree.dir.Fd()), child); err != nil {
			return err
		}
	}
	return nil
}

func waitEmpty(ctx context.Context, scope Scope, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		empty, err := scope.Empty()
		if err != nil {
			return err
		}
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("scope did not become empty")
		case <-tick.C:
		}
	}
}
