//go:build linux

package install

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// AIRA-357. .githooks/test-tmpdir runs `make test`/`race`/`test-store` with
// TMPDIR on a per-run tmpfs directory, and first sweeps the run directories
// that hard-killed runs left behind. These tests drive the REAL script against
// a private root (AIRA_TEST_TMPFS_ROOT), never the shared /dev/shm.
//
// Mutations each case is built to kill (checked by hand, see the commit):
//   - drop the liveness check, or compare only the pid and not its start time
//     (live-run dir swept / reused-pid dir kept);
//   - drop the pid-namespace check (other-namespace dir swept);
//   - treat an unreadable start time of an existing pid as gone (live dir
//     swept under a failing cat);
//   - remove with a plain rm -rf, skip restoring permissions, or skip the
//     left-over check (owner file lost, or a mode-000 subdir left behind);
//   - follow a symlink (drop the physical-path check), or sweep a dir
//     without a valid owner file;
//   - remove by path after the check instead of from inside the checked dir
//     (a name reused, or swapped for a symlink, mid-removal is hit);
//   - drop the lock, or hold it into the command;
//   - lose the command's exit status, or leave the run dir behind;
//   - use tmpfs under CI, without a usable lock, or when the root is missing
//     or too small;
//   - point GOTMPDIR at an explicit (noexec) AIRA_TEST_DISK_TMPDIR.
//
// verifies: AIRA-357

const testTmpdirOwnerVersion = "aira-test-tmpdir v1"

func testTmpdirScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(hookSourceDir(t), "test-tmpdir")
}

// testTmpdirEnv is the test's environment without CI or any AIRA_TEST_*
// variable (a CI runner sets CI=true, which would bypass the script), plus
// extra.
func testTmpdirEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CI" || strings.HasPrefix(name, "AIRA_TEST_") || name == "TMPDIR" || name == "GOTMPDIR" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// procStart returns field 22 of /proc/<pid>/stat.
func procStart(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatalf("read stat of %d: %v", pid, err)
	}
	s := string(data)
	fields := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	return fields[19]
}

func ourPidNS(t *testing.T) string {
	t.Helper()
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatalf("read pid namespace: %v", err)
	}
	return ns
}

// deadPid returns the pid and start time of a process that has exited and been
// reaped.
func deadPid(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	start := procStart(t, pid)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return pid, start
}

// plantRunDir makes root/name with an owner file and ages both by age.
func plantRunDir(t *testing.T, root, name, owner string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if owner != "" {
		path := filepath.Join(dir, ".aira-test-owner")
		if err := os.WriteFile(path, []byte(owner), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ownerLine(pid int, start, ns string) string {
	return testTmpdirOwnerVersion + "\n" + strconv.Itoa(pid) + " " + start + " " + ns + "\n"
}

// runTestTmpdir runs the real script on argv with the given extra environment
// and returns combined output, exit code.
func runTestTmpdir(t *testing.T, extraEnv []string, argv ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{testTmpdirScript(t)}, argv...)...)
	cmd.Env = testTmpdirEnv(extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run test-tmpdir: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// TestTestTmpdirSweepRemovesOnlyDeadRunsDirs plants every kind of directory the
// sweep might meet and checks exactly the dead runs' ones go.
func TestTestTmpdirSweepRemovesOnlyDeadRunsDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ns := ourPidNS(t)
	old := time.Hour
	me := os.Getpid()
	myStart := procStart(t, me)
	dead, deadStart := deadPid(t)

	gone := map[string]string{
		"dead":       plantRunDir(t, root, "t.dead", ownerLine(dead, deadStart, ns), old),
		"reused-pid": plantRunDir(t, root, "t.reus", ownerLine(me, myStart+"1", ns), old),
		// No age margin: the liveness proof alone decides (the lock means no
		// sweep sees a dir before its owner file is written).
		"young": plantRunDir(t, root, "t.youn", ownerLine(dead, deadStart, ns), 0),
	}
	kept := map[string]string{
		"live":            plantRunDir(t, root, "t.live", ownerLine(me, myStart, ns), old),
		"other-namespace": plantRunDir(t, root, "t.othn", ownerLine(dead, deadStart, "pid:[1]"), old),
		"no-owner":        plantRunDir(t, root, "t.noow", "", old),
		"wrong-version":   plantRunDir(t, root, "t.badv", "aira-test-tmpdir v0\n"+strconv.Itoa(dead)+" "+deadStart+" "+ns+"\n", old),
		"bad-pid":         plantRunDir(t, root, "t.badp", testTmpdirOwnerVersion+"\nx1 "+deadStart+" "+ns+"\n", old),
		"other-name":      plantRunDir(t, root, "u.dead", ownerLine(dead, deadStart, ns), old),
		"long-name":       plantRunDir(t, root, "t.dead0", ownerLine(dead, deadStart, ns), old),
	}
	// A symlink named like a run dir, pointing at a dead run's dir elsewhere,
	// whose files have modes a chmod -R u+rwX would change.
	link := filepath.Join(root, "t.link")
	target := plantSymlinkTarget(t, link, ownerLine(dead, deadStart, ns))

	out, code := runTestTmpdir(t, []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for name, dir := range gone {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: %s was not swept (lstat err %v)\n%s", name, dir, err, out)
		}
	}
	for name, dir := range kept {
		if _, err := os.Stat(filepath.Join(dir, "payload")); err != nil {
			t.Errorf("%s: %s was swept or emptied: %v\n%s", name, dir, err, out)
		}
	}
	target.check(t, link, out)
}

// symlinkTarget is a dead run's dir outside the root, reached through a symlink
// named like a run dir, with its state recorded so a check can prove the sweep
// changed nothing there.
type symlinkTarget struct {
	dir, owner string
	modes      map[string]os.FileMode
}

// plantSymlinkTarget makes a dead run's dir in a fresh temp dir holding owner,
// a payload, and modes chmod -R u+rwX would change (owner and payload 0400),
// and points the symlink link at it.
func plantSymlinkTarget(t *testing.T, link, owner string) symlinkTarget {
	t.Helper()
	parent := t.TempDir()
	chmodBackOnCleanup(t, parent)
	dir := plantRunDir(t, parent, "t.targ", owner, time.Hour)
	st := symlinkTarget{dir: dir, owner: owner, modes: map[string]os.FileMode{}}
	for _, name := range []string{".aira-test-owner", "payload"} {
		if err := os.Chmod(filepath.Join(dir, name), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{dir, filepath.Join(dir, ".aira-test-owner"), filepath.Join(dir, "payload")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		st.modes[path] = info.Mode()
	}
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	return st
}

// check asserts link is still a symlink to the target, and the target's owner
// file contents and every recorded mode are unchanged.
func (st symlinkTarget) check(t *testing.T, link, out string) {
	t.Helper()
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink (lstat %v, err %v)\n%s", link, info, err, out)
	} else if dest, err := os.Readlink(link); err != nil || dest != st.dir {
		t.Errorf("%s now points at %q (err %v), want %s\n%s", link, dest, err, st.dir, out)
	}
	if data, err := os.ReadFile(filepath.Join(st.dir, ".aira-test-owner")); err != nil || string(data) != st.owner {
		t.Errorf("the symlink target's owner file changed: %q, err %v\n%s", data, err, out)
	}
	for path, mode := range st.modes {
		info, err := os.Lstat(path)
		if err != nil {
			t.Errorf("the symlink target lost %s: %v\n%s", path, err, out)
			continue
		}
		if info.Mode() != mode {
			t.Errorf("%s mode %v, want %v (chmod went through the symlink)\n%s", path, info.Mode(), mode, out)
		}
	}
}

// TestTestTmpdirRunsOnTmpfsAndCleansUp: the command sees TMPDIR as a fresh
// t.XXXX dir under the root holding a valid owner file for a live process,
// the disk dir is exported, the exit status is the command's, and the dir is
// gone afterwards.
func TestTestTmpdirRunsOnTmpfsAndCleansUp(t *testing.T) {
	t.Parallel()
	for _, status := range []int{0, 3} {
		root := t.TempDir()
		report := filepath.Join(t.TempDir(), "report")
		script := `printf '%s\n%s\n%s\n' "$TMPDIR" "$AIRA_TEST_DISK_TMPDIR" "$GOTMPDIR" > "$1"; cat "$TMPDIR/.aira-test-owner" >> "$1"; if flock -n "$AIRA_TEST_TMPFS_ROOT/.aira-test-lock.$(id -u)" true; then echo lock-free; else echo lock-held; fi >> "$1"; exit "$2"`
		// The last line says whether the lock is free while the command runs
		// (no other run uses this root, so held means this run kept it).
		out, code := runTestTmpdir(t, []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0", "TMPDIR=/var/tmp"},
			"bash", "-c", script, "probe", report, strconv.Itoa(status))
		if code != status {
			t.Fatalf("exit %d, want %d\n%s", code, status, out)
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatalf("the command did not run: %v\n%s", err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 6 {
			t.Fatalf("report %q", data)
		}
		run := lines[0]
		if filepath.Dir(run) != root || !strings.HasPrefix(filepath.Base(run), "t.") || len(filepath.Base(run)) != 6 {
			t.Fatalf("TMPDIR %q is not a t.XXXX dir under %s", run, root)
		}
		if lines[1] != "/var/tmp" || lines[2] != "/var/tmp" {
			t.Fatalf("AIRA_TEST_DISK_TMPDIR=%q GOTMPDIR=%q, want the original TMPDIR /var/tmp", lines[1], lines[2])
		}
		if lines[3] != testTmpdirOwnerVersion {
			t.Fatalf("owner version line %q", lines[3])
		}
		owner := strings.Fields(lines[4])
		if len(owner) != 3 || owner[2] != ourPidNS(t) {
			t.Fatalf("owner line %q", lines[4])
		}
		if lines[5] != "lock-free" {
			t.Fatalf("the lock was %s while the command ran", lines[5])
		}
		if _, err := os.Stat(run); !os.IsNotExist(err) {
			t.Fatalf("run dir %s left behind (stat err %v)", run, err)
		}
	}
}

// TestTestTmpdirFallsBackToTheNormalTmpdir: under CI, with the opt-out, with a
// missing root, or with too little space, the command runs with TMPDIR
// untouched (and still with its own exit status).
func TestTestTmpdirFallsBackToTheNormalTmpdir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A root whose lock path is a symlink (to a file we own): never opened.
	symlinkLockRoot := t.TempDir()
	lockTarget := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(lockTarget, filepath.Join(symlinkLockRoot, ".aira-test-lock."+strconv.Itoa(os.Getuid()))); err != nil {
		t.Fatal(err)
	}
	noFlock := "PATH=" + shimDir(t, map[string]string{"flock": "exit 1"}) + ":" + os.Getenv("PATH")
	cases := []struct {
		name string
		env  []string
	}{
		{"ci", []string{"CI=true", "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}},
		{"flock-fails", []string{noFlock, "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}},
		{"lock-is-a-symlink", []string{"AIRA_TEST_TMPFS_ROOT=" + symlinkLockRoot, "AIRA_TEST_TMPFS_MIN_KB=0"}},
		{"opt-out", []string{"AIRA_TEST_TMPFS=0", "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}},
		{"missing-root", []string{"AIRA_TEST_TMPFS_ROOT=" + filepath.Join(root, "absent"), "AIRA_TEST_TMPFS_MIN_KB=0"}},
		{"too-small", []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=999999999999"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, code := runTestTmpdir(t, append([]string{"TMPDIR=/var/tmp"}, c.env...), "bash", "-c", `[ "$TMPDIR" = /var/tmp ] || { echo "TMPDIR=$TMPDIR"; exit 9; }; exit 4`)
			if code != 4 {
				t.Fatalf("exit %d, want 4 (the command's own, with TMPDIR untouched)\n%s", code, out)
			}
			if c.name == "lock-is-a-symlink" {
				if _, err := os.Lstat(lockTarget); !os.IsNotExist(err) {
					t.Fatalf("the lock was opened through the symlink (lstat %s err %v)", lockTarget, err)
				}
				if entries, err := filepath.Glob(filepath.Join(symlinkLockRoot, "t.*")); err != nil || len(entries) != 0 {
					t.Fatalf("run dirs %v made without the lock (err %v)", entries, err)
				}
			}
		})
	}
	// The negative control: the same root and size floor 0 DO move TMPDIR.
	out, code := runTestTmpdir(t, []string{"TMPDIR=/var/tmp", "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "bash", "-c", `[ "$TMPDIR" = /var/tmp ] && exit 4; exit 0`)
	if code != 0 {
		t.Fatalf("control: TMPDIR was not moved (exit %d)\n%s", code, out)
	}

}

// TestTestTmpdirHardKillLeaksAndTheNextRunSweeps is the hard-kill leak check:
// SIGKILL the whole run (as `aira confine --kill` does) and its dir stays;
// the next run removes it at once (there is no age margin).
func TestTestTmpdirHardKillLeaksAndTheNextRunSweeps(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	report := filepath.Join(t.TempDir(), "report")
	cmd := exec.Command("bash", testTmpdirScript(t), "bash", "-c", `echo "$TMPDIR" > "$1.tmp" && mv "$1.tmp" "$1"; exec sleep 60`, "probe", report)
	cmd.Env = testTmpdirEnv("AIRA_TEST_TMPFS_ROOT="+root, "AIRA_TEST_TMPFS_MIN_KB=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var run string
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(report); err == nil {
			run = strings.TrimSpace(string(data))
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatal("the wrapped command never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// While it runs, a sweep keeps it.
	owner := filepath.Join(run, ".aira-test-owner")
	if out, code := runTestTmpdir(t, []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true"); code != 0 {
		t.Fatalf("sweep run exit %d\n%s", code, out)
	}
	if _, err := os.Stat(owner); err != nil {
		t.Fatalf("a LIVE run's dir was swept: %v", err)
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(owner); err != nil {
		t.Fatalf("after a hard kill the run dir should still be there (that is the leak): %v", err)
	}
	out, code := runTestTmpdir(t, []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
	if code != 0 {
		t.Fatalf("sweep run exit %d\n%s", code, out)
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Fatalf("the killed run's dir %s was not swept (stat err %v)\n%s", run, err, out)
	}
	if !strings.Contains(out, "removed "+run) {
		t.Fatalf("the sweep did not report the removal\n%s", out)
	}
}

// shimDir makes a directory holding executable scripts named after the keys of
// shims (body after the shebang), for prepending to PATH to inject a fault.
func shimDir(t *testing.T, shims map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range shims {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// chmodBackOnCleanup gives the user rwx on everything under root again before
// t.TempDir's own cleanup (registered earlier, so run later) removes it.
func chmodBackOnCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+rwX", root).Run() })
}

// TestTestTmpdirSweepKeepsADirWhoseOwnersLivenessIsUnknown: when a live
// owner's /proc/<pid>/stat cannot be read (cat fails), liveness is unknown and
// the dir is kept. The negative control in the same run: a dead owner (no
// /proc/<pid>) is still swept under the same fault.
func TestTestTmpdirSweepKeepsADirWhoseOwnersLivenessIsUnknown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ns := ourPidNS(t)
	me := os.Getpid()
	dead, deadStart := deadPid(t)
	live := plantRunDir(t, root, "t.live", ownerLine(me, procStart(t, me), ns), time.Hour)
	gone := plantRunDir(t, root, "t.dead", ownerLine(dead, deadStart, ns), time.Hour)

	shims := shimDir(t, map[string]string{"cat": "exit 1"})
	out, code := runTestTmpdir(t, []string{"PATH=" + shims + ":" + os.Getenv("PATH"), "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(live, "payload")); err != nil {
		t.Errorf("a live run's dir was swept when its start time could not be read: %v\n%s", err, out)
	}
	if _, err := os.Lstat(gone); !os.IsNotExist(err) {
		t.Errorf("control: the dead run's dir %s was not swept (lstat err %v)\n%s", gone, err, out)
	}
}

// plantLockedPayload adds dir/locked/file and then makes dir/locked mode 000,
// as TestRetireReportsUnevaluatedDigest does to its fixture dir before a hard
// kill can stop its cleanup from restoring it.
func plantLockedPayload(t *testing.T, dir string) {
	t.Helper()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
}

// TestTestTmpdirSweepKeepsTheOwnerFileUntilThePayloadIsGone: a dead run's dir
// holding a mode-000 subdirectory is removed completely (the removal restores
// permissions first). When the payload still cannot be removed (chmod made a
// no-op), the owner file survives, so the next sweep can still prove the run
// is gone and finish the job.
func TestTestTmpdirSweepKeepsTheOwnerFileUntilThePayloadIsGone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()
	ns := ourPidNS(t)
	dead, deadStart := deadPid(t)

	t.Run("restores-permissions", func(t *testing.T) {
		root := t.TempDir()
		chmodBackOnCleanup(t, root)
		dir := plantRunDir(t, root, "t.dead", ownerLine(dead, deadStart, ns), time.Hour)
		plantLockedPayload(t, dir)
		out, code := runTestTmpdir(t, []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s with a mode-000 subdir was not removed (lstat err %v)\n%s", dir, err, out)
		}
	})

	t.Run("failed-removal-keeps-owner", func(t *testing.T) {
		root := t.TempDir()
		chmodBackOnCleanup(t, root)
		dir := plantRunDir(t, root, "t.dead", ownerLine(dead, deadStart, ns), time.Hour)
		plantLockedPayload(t, dir)
		shims := shimDir(t, map[string]string{"chmod": "exit 0"})
		out, code := runTestTmpdir(t, []string{"PATH=" + shims + ":" + os.Getenv("PATH"), "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if _, err := os.Stat(filepath.Join(dir, ".aira-test-owner")); err != nil {
			t.Fatalf("a failed removal lost the owner file: %v\n%s", err, out)
		}
		if strings.Contains(out, "removed "+dir) {
			t.Fatalf("a failed removal was reported as done\n%s", out)
		}
		out, code = runTestTmpdir(t, []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("the retry sweep did not remove %s (lstat err %v)\n%s", dir, err, out)
		}
	})
}

// TestTestTmpdirExitKeepsTheOwnerFileUntilThePayloadIsGone: the run's own exit
// removes a run dir holding a mode-000 subdir; when that removal cannot finish,
// the owner file stays, and a later sweep removes it.
// The exit status stays the command's either way.
func TestTestTmpdirExitKeepsTheOwnerFileUntilThePayloadIsGone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()
	script := `echo "$TMPDIR" > "$1"; mkdir "$TMPDIR/locked" && head -c 4096 /dev/zero > "$TMPDIR/locked/file" && chmod 000 "$TMPDIR/locked"; exit 5`
	// The command's own chmod must be the real one, so the shim case calls it
	// by absolute path.
	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	script = strings.Replace(script, "chmod 000", realChmod+" 000", 1)

	for _, broken := range []bool{false, true} {
		root := t.TempDir()
		chmodBackOnCleanup(t, root)
		report := filepath.Join(t.TempDir(), "report")
		env := []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}
		if broken {
			env = append(env, "PATH="+shimDir(t, map[string]string{"chmod": "exit 0"})+":"+os.Getenv("PATH"))
		}
		out, code := runTestTmpdir(t, env, "bash", "-c", script, "probe", report)
		if code != 5 {
			t.Fatalf("broken=%v: exit %d, want the command's 5\n%s", broken, code, out)
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatalf("broken=%v: the command did not run: %v\n%s", broken, err, out)
		}
		run := strings.TrimSpace(string(data))
		if !broken {
			if _, err := os.Lstat(run); !os.IsNotExist(err) {
				t.Fatalf("run dir %s with a mode-000 subdir left behind (lstat err %v)\n%s", run, err, out)
			}
			continue
		}
		owner := filepath.Join(run, ".aira-test-owner")
		if _, err := os.Stat(owner); err != nil {
			t.Fatalf("a failed exit removal lost the owner file: %v\n%s", err, out)
		}
		out, code = runTestTmpdir(t, []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
		if code != 0 {
			t.Fatalf("sweep exit %d\n%s", code, out)
		}
		if _, err := os.Lstat(run); !os.IsNotExist(err) {
			t.Fatalf("the later sweep did not remove %s (lstat err %v)\n%s", run, err, out)
		}
	}
}

// TestTestTmpdirFallsBackOnANoexecRoot: a writable, roomy root mounted noexec
// cannot run the programs tests build under t.TempDir(), so the command runs
// with TMPDIR untouched and the probe's run dir is removed. The root is a
// private noexec tmpfs in a user+mount namespace; the negative control mounts
// the same tmpfs without noexec and TMPDIR does move.
func TestTestTmpdirFallsBackOnANoexecRoot(t *testing.T) {
	t.Parallel()
	if err := exec.Command("unshare", "-rm", "true").Run(); err != nil {
		t.Skipf("unprivileged user+mount namespaces unavailable (unshare -rm: %v); cannot mount a private noexec tmpfs", err)
	}
	for _, c := range []struct {
		opts string
		want int
	}{{"noexec", 4}, {"exec", 0}} {
		root := t.TempDir()
		// Inside the namespace: mount the root, run the real script on a
		// command that exits 4 when TMPDIR is untouched and 0 when it moved
		// under the root, and 8 if the fallback left anything in the root but
		// the lock file.
		mount := `mount -t tmpfs -o "$1" none "$2" || exit 77; shift 2; exec bash "$@"`
		cmd := exec.Command("unshare", "-rm", "sh", "-c", mount, "sh", c.opts, root, testTmpdirScript(t),
			"bash", "-c", `if [ "$TMPDIR" = /var/tmp ]; then [ -z "$(ls -A "$AIRA_TEST_TMPFS_ROOT" | grep -v '^\.aira-test-lock\.')" ] || exit 8; exit 4; fi; case "$TMPDIR" in "$AIRA_TEST_TMPFS_ROOT"/t.*) exit 0 ;; esac; exit 9`)
		cmd.Env = testTmpdirEnv("TMPDIR=/var/tmp", "AIRA_TEST_TMPFS_ROOT="+root, "AIRA_TEST_TMPFS_MIN_KB=0")
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("%s: %v\n%s", c.opts, err, out)
			}
			code = exitErr.ExitCode()
		}
		if code == 77 {
			t.Skipf("cannot mount a tmpfs in the namespace\n%s", out)
		}
		if code != c.want {
			t.Fatalf("%s root: exit %d, want %d\n%s", c.opts, code, c.want, out)
		}
	}
}

// TestTestTmpdirDiskDir: the disk directory exported for the SliceCeiling
// fixture keeps an explicit AIRA_TEST_DISK_TMPDIR, is the incoming TMPDIR
// when that is on disk, and is /var/tmp when the incoming TMPDIR is tmpfs.
// GOTMPDIR is chosen the same way but never from the explicit fixture override.
func TestTestTmpdirDiskDir(t *testing.T) {
	t.Parallel()
	if isTmpfs("/var/tmp") {
		t.Skip("/var/tmp is tmpfs here; no disk directory to expect")
	}
	explicit := t.TempDir()
	type diskCase struct {
		name         string
		env          []string
		want, wantGo string
	}
	cases := []diskCase{
		{"explicit-kept", []string{"TMPDIR=/var/tmp", "AIRA_TEST_DISK_TMPDIR=" + explicit}, explicit, "/var/tmp"},
		{"disk-tmpdir", []string{"TMPDIR=/var/tmp"}, "/var/tmp", "/var/tmp"},
	}
	if isTmpfs("/dev/shm") {
		cases = append(cases,
			diskCase{"tmpfs-tmpdir", []string{"TMPDIR=/dev/shm"}, "/var/tmp", "/var/tmp"},
			diskCase{"explicit-kept-over-tmpfs", []string{"TMPDIR=/dev/shm", "AIRA_TEST_DISK_TMPDIR=" + explicit}, explicit, "/var/tmp"},
		)
	} else {
		t.Log("/dev/shm is not tmpfs here; the tmpfs-TMPDIR cases are not run")
	}
	for _, c := range cases {
		root := t.TempDir()
		env := append([]string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, c.env...)
		out, code := runTestTmpdir(t, env, "bash", "-c", `case "$TMPDIR" in "$AIRA_TEST_TMPFS_ROOT"/t.*) ;; *) echo "TMPDIR not moved: $TMPDIR"; exit 9 ;; esac; printf '%s %s\n' "$AIRA_TEST_DISK_TMPDIR" "$GOTMPDIR"`)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", c.name, code, out)
		}
		if got, want := strings.TrimSpace(out), c.want+" "+c.wantGo; got != want {
			t.Errorf("%s: AIRA_TEST_DISK_TMPDIR GOTMPDIR = %q, want %q", c.name, got, want)
		}
	}
}

func isTmpfs(dir string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(dir, &st) == nil && st.Type == 0x01021994
}

// TestTestTmpdirGoTmpdirRunsProgramsUnderANoexecFixtureDir: an explicit
// AIRA_TEST_DISK_TMPDIR mounted noexec is fine for the fixture's page-cache
// files, and GOTMPDIR (where go test puts the binaries it runs) is not it, so
// a program copied there runs. The fixture dir is a private noexec tmpfs in a
// user+mount namespace; the control in the same command shows a program in it
// does NOT run.
func TestTestTmpdirGoTmpdirRunsProgramsUnderANoexecFixtureDir(t *testing.T) {
	t.Parallel()
	if isTmpfs("/var/tmp") {
		t.Skip("/var/tmp is tmpfs here; no disk directory for GOTMPDIR")
	}
	if err := exec.Command("unshare", "-rm", "true").Run(); err != nil {
		t.Skipf("unprivileged user+mount namespaces unavailable (unshare -rm: %v)", err)
	}
	// /var/tmp itself must run programs, or there is nothing to show.
	host := `p="/var/tmp/aira-357-exec.$$"; cp "$(type -P true)" "$p" && "$p"; rc=$?; rm -f "$p"; exit $rc`
	if err := exec.Command("bash", "-c", host).Run(); err != nil {
		t.Skipf("/var/tmp cannot run programs here: %v", err)
	}
	fixture := t.TempDir()
	mount := `mount -t tmpfs -o noexec none "$1" || exit 77; shift; exec bash "$@"`
	probe := `cp "$(type -P true)" "$AIRA_TEST_DISK_TMPDIR/x" || exit 5
"$AIRA_TEST_DISK_TMPDIR/x" 2>/dev/null && exit 6
p="$GOTMPDIR/aira-357-exec.$$"; cp "$(type -P true)" "$p" || exit 7
"$p"; rc=$?; rm -f "$p"; [ "$rc" = 0 ] || exit 8`
	cmd := exec.Command("unshare", "-rm", "sh", "-c", mount, "sh", fixture, testTmpdirScript(t), "bash", "-c", probe)
	cmd.Env = testTmpdirEnv("TMPDIR=/var/tmp", "AIRA_TEST_DISK_TMPDIR="+fixture, "AIRA_TEST_TMPFS_ROOT="+t.TempDir(), "AIRA_TEST_TMPFS_MIN_KB=0")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	switch code {
	case 0:
	case 77:
		t.Skipf("cannot mount a tmpfs in the namespace\n%s", out)
	case 6:
		t.Fatalf("control: the fixture dir ran a program, so it is not noexec\n%s", out)
	case 8:
		t.Fatalf("a program in GOTMPDIR did not run: GOTMPDIR is the noexec fixture dir\n%s", out)
	default:
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// pausingChmod returns a PATH entry whose chmod, on its first call only,
// makes <pause>/paused and waits for <pause>/go before running the real
// chmod. Removal's first command, in this script and its predecessor, is that
// chmod, so the pause falls after a sweep has checked a dir and before it
// deletes anything.
func pausingChmod(t *testing.T) (path, pause string) {
	t.Helper()
	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	pause = t.TempDir()
	body := fmt.Sprintf(`if mkdir '%s/paused' 2>/dev/null; then while [ ! -e '%s/go' ]; do sleep 0.01; done; fi
exec '%s' "$@"`, pause, pause, realChmod)
	return "PATH=" + shimDir(t, map[string]string{"chmod": body}) + ":" + os.Getenv("PATH"), pause
}

// startTestTmpdir starts the real script in its own process group (killed on
// cleanup if still running), output to the returned buffer.
func startTestTmpdir(t *testing.T, extraEnv []string, argv ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cmd := exec.Command("bash", append([]string{testTmpdirScript(t)}, argv...)...)
	cmd.Env = testTmpdirEnv(extraEnv...)
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	return cmd, &buf
}

// waitPaused waits for the pausing chmod to be reached.
func waitPaused(t *testing.T, pause string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(pause, "paused")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep never reached its removal")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// resume lets the paused chmod go on and waits for cmd, which must exit 0.
func resume(t *testing.T, pause string, cmd *exec.Cmd, out *bytes.Buffer) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(pause, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("paused run: %v\n%s", err, out)
	}
}

// TestTestTmpdirSweepNeverRemovesADirReusedMidRemoval: sweep B has checked a
// dead run's dir and is about to delete it when (as sweep A and a new run
// could, without serialisation) the dir is removed and its name reused by a
// LIVE run. B must not delete the live run's dir.
func TestTestTmpdirSweepNeverRemovesADirReusedMidRemoval(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ns := ourPidNS(t)
	me := os.Getpid()
	dead, deadStart := deadPid(t)
	dir := plantRunDir(t, root, "t.reus", ownerLine(dead, deadStart, ns), time.Hour)

	path, pause := pausingChmod(t)
	b, out := startTestTmpdir(t, []string{path, "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
	waitPaused(t, pause)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	live := ownerLine(me, procStart(t, me), ns)
	plantRunDir(t, root, "t.reus", live, 0)
	resume(t, pause, b, out)

	if _, err := os.Stat(filepath.Join(dir, "payload")); err != nil {
		t.Errorf("the live run's reused dir was emptied: %v\n%s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, ".aira-test-owner")); err != nil || string(data) != live {
		t.Errorf("the live run's owner file is %q (err %v)\n%s", data, err, out)
	}
}

// TestTestTmpdirSweepNeverFollowsADirSwappedForASymlinkMidRemoval: sweep B has
// checked a dead run's dir and is about to delete it when the dir is renamed
// away and a symlink to another dir put in its place. B must change nothing
// through the symlink.
func TestTestTmpdirSweepNeverFollowsADirSwappedForASymlinkMidRemoval(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ns := ourPidNS(t)
	dead, deadStart := deadPid(t)
	dir := plantRunDir(t, root, "t.swap", ownerLine(dead, deadStart, ns), time.Hour)

	path, pause := pausingChmod(t)
	b, out := startTestTmpdir(t, []string{path, "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}, "true")
	waitPaused(t, pause)
	if err := os.Rename(dir, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	target := plantSymlinkTarget(t, dir, ownerLine(dead, deadStart, ns))
	resume(t, pause, b, out)

	target.check(t, dir, out.String())
}

// TestTestTmpdirSweepsAreSerialised: while one run is mid-removal, the lock is
// held, so a second run started then waits; both finish, and the dead run's
// dir and both runs' own dirs are gone.
func TestTestTmpdirSweepsAreSerialised(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ns := ourPidNS(t)
	dead, deadStart := deadPid(t)
	plantRunDir(t, root, "t.stal", ownerLine(dead, deadStart, ns), time.Hour)
	lock := filepath.Join(root, ".aira-test-lock."+strconv.Itoa(os.Getuid()))

	path, pause := pausingChmod(t)
	env := []string{"AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}
	b, bOut := startTestTmpdir(t, append([]string{path}, env...), "true")
	waitPaused(t, pause)
	if err := exec.Command("flock", "-n", lock, "true").Run(); err == nil {
		t.Errorf("the lock %s was free while a sweep was mid-removal\n%s", lock, bOut)
	}
	a, aOut := startTestTmpdir(t, env, "true")
	resume(t, pause, b, bOut)
	if err := a.Wait(); err != nil {
		t.Fatalf("second run: %v\n%s", err, aOut)
	}
	if left, err := filepath.Glob(filepath.Join(root, "t.*")); err != nil || len(left) != 0 {
		t.Fatalf("left behind: %v (err %v)\nB:\n%s\nA:\n%s", left, err, bOut, aOut)
	}
}
