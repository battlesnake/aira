//go:build linux

package install

import (
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
//   - drop the age margin (young dead dir swept);
//   - treat an unreadable start time of an existing pid as gone (live dir
//     swept under a failing cat);
//   - remove with a plain rm -rf, skip restoring permissions, or skip the
//     left-over check (owner file lost, or a mode-000 subdir left behind);
//   - follow a symlink, or sweep a dir without a valid owner file;
//   - lose the command's exit status, or leave the run dir behind;
//   - use tmpfs under CI, or when the root is missing or too small.
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
	}
	kept := map[string]string{
		"live":            plantRunDir(t, root, "t.live", ownerLine(me, myStart, ns), old),
		"young":           plantRunDir(t, root, "t.youn", ownerLine(dead, deadStart, ns), time.Minute),
		"other-namespace": plantRunDir(t, root, "t.othn", ownerLine(dead, deadStart, "pid:[1]"), old),
		"no-owner":        plantRunDir(t, root, "t.noow", "", old),
		"wrong-version":   plantRunDir(t, root, "t.badv", "aira-test-tmpdir v0\n"+strconv.Itoa(dead)+" "+deadStart+" "+ns+"\n", old),
		"bad-pid":         plantRunDir(t, root, "t.badp", testTmpdirOwnerVersion+"\nx1 "+deadStart+" "+ns+"\n", old),
		"other-name":      plantRunDir(t, root, "u.dead", ownerLine(dead, deadStart, ns), old),
		"long-name":       plantRunDir(t, root, "t.dead0", ownerLine(dead, deadStart, ns), old),
	}
	// A symlink named like a run dir, pointing at a dead run's dir elsewhere.
	elsewhere := plantRunDir(t, t.TempDir(), "t.targ", ownerLine(dead, deadStart, ns), old)
	link := filepath.Join(root, "t.link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	kept["symlink"] = link
	kept["symlink-target"] = elsewhere

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
		script := `printf '%s\n%s\n%s\n' "$TMPDIR" "$AIRA_TEST_DISK_TMPDIR" "$GOTMPDIR" > "$1"; cat "$TMPDIR/.aira-test-owner" >> "$1"; exit "$2"`
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
		if len(lines) != 5 {
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
	cases := []struct {
		name string
		env  []string
	}{
		{"ci", []string{"CI=true", "AIRA_TEST_TMPFS_ROOT=" + root, "AIRA_TEST_TMPFS_MIN_KB=0"}},
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
// once it is past the age margin the next run removes it.
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
	// While it runs, a sweep (even with the dir aged past the margin) keeps it.
	owner := filepath.Join(run, ".aira-test-owner")
	aged := time.Now().Add(-time.Hour)
	if err := os.Chtimes(owner, aged, aged); err != nil {
		t.Fatal(err)
	}
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
// the owner file stays, and once past the age margin a later sweep removes it.
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
		aged := time.Now().Add(-time.Hour)
		if err := os.Chtimes(owner, aged, aged); err != nil {
			t.Fatal(err)
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
