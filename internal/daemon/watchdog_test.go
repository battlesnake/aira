package daemon

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/store"
	"golang.org/x/sys/unix"
)

func TestParseHostPressureFull(t *testing.T) {
	avg, total, ok, reason := parseHostPressureFull([]byte("some avg10=0.00 avg60=0.01 avg300=0.02 total=9\nfull avg10=12.34 avg60=2.00 avg300=1.00 total=987654\n"))
	if !ok || reason != "" || avg != 12.34 || total != 987654 {
		t.Fatalf("avg=%v total=%d ok=%v reason=%q", avg, total, ok, reason)
	}
	for _, data := range [][]byte{
		[]byte("some avg10=1 total=2\n"),
		[]byte("full avg10=nope avg60=2 avg300=3 total=4\n"),
		[]byte("full avg10=2 avg60=2 avg300=3\n"),
		[]byte("full avg10=2 total=-1\n"),
		[]byte("full avg10=NaN total=4\n"),
		[]byte("full avg10=+Inf total=4\n"),
	} {
		if _, _, ok, reason := parseHostPressureFull(data); ok || reason == "" {
			t.Fatalf("%q parsed ok=%v reason=%q", data, ok, reason)
		}
	}
}

func TestParseMemAvailable(t *testing.T) {
	if got, ok, reason := parseMemAvailable([]byte("MemTotal: 100 kB\nMemAvailable: 8192 kB\n")); !ok || reason != "" || got != 8192<<10 {
		t.Fatalf("got=%d ok=%v reason=%q", got, ok, reason)
	}
	for _, data := range [][]byte{[]byte("MemTotal: 1 kB\n"), []byte("MemAvailable: nope kB\n"), []byte("MemAvailable: 1 MB\n")} {
		if _, ok, reason := parseMemAvailable(data); ok || reason == "" {
			t.Fatalf("%q parsed ok=%v reason=%q", data, ok, reason)
		}
	}
}

func TestParseProcStartTimeAfterLastParen(t *testing.T) {
	line := "123 (odd ) name (still)) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 4242 20\n"
	if got, ok := parseProcStartTime([]byte(line)); !ok || got != 4242 {
		t.Fatalf("got=%d ok=%v", got, ok)
	}
}

func baseWatchdogDeps(readings []pressureSample, available int64, procs map[int]watchdogProc) (watchdogDeps, *[]watchdogEvent, *[]unix.Signal) {
	index := 0
	events := []watchdogEvent{}
	signals := []unix.Signal{}
	d := watchdogDeps{
		readPressure: func() (float64, uint64, bool, string) {
			if index >= len(readings) {
				r := readings[len(readings)-1]
				return r.avg10, r.total, r.ok, r.reason
			}
			r := readings[index]
			index++
			return r.avg10, r.total, r.ok, r.reason
		},
		readMemAvailable: func() (int64, bool, string) { return available, true, "" },
		snapshotProcs:    func() (map[int]watchdogProc, error) { return procs, nil },
		pidfdOpen:        func(pid int) (int, error) { return pid + 1000, nil },
		pidfdSignal: func(_ int, sig unix.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		closeFD: func(int) error { return nil },
		startTime: func(pid int) (uint64, bool, string) {
			p, ok := procs[pid]
			if !ok {
				return 0, false, "exited"
			}
			return p.startTime, true, ""
		},
		cgroupOf: func(pid int) (watchdogCgroup, bool, string) {
			p, ok := procs[pid]
			if !ok {
				return watchdogCgroup{}, false, "exited"
			}
			return p.cgroup, true, ""
		},
		interlockOK: func() (func(), bool, string) { return func() {}, true, "" },
		emitEvent: func(_ context.Context, event watchdogEvent) error {
			events = append(events, event)
			return nil
		},
		sleep:               func(context.Context, time.Duration) bool { return true },
		now:                 time.Now,
		minVictimRSS:        100,
		tripPSIFullAvg10:    10,
		recoverPSIFullAvg10: 1,
		lowMemAvailable:     1000,
		debounce:            3,
		grace:               time.Millisecond,
		postKillSettle:      time.Millisecond,
		daemonPID:           999,
		daemonCgroup:        "/user.slice/aira.service",
	}
	return d, &events, &signals
}

func eligibleTree() map[int]watchdogProc {
	return map[int]watchdogProc{
		10: {pid: 10, ppid: 1, comm: "claude", rss: 50, startTime: 10, cgroup: watchdogCgroup{path: "/user.slice/session.scope", uncapped: true}},
		20: {pid: 20, ppid: 10, comm: "worker", rss: 500, startTime: 20, cgroup: watchdogCgroup{path: "/user.slice/session.scope", uncapped: true}},
		21: {pid: 21, ppid: 20, comm: "helper", rss: 200, startTime: 21, cgroup: watchdogCgroup{path: "/user.slice/session.scope", uncapped: true}},
	}
}

func TestTriggerRequiresDeltaAverageLowMemoryAndDebounce(t *testing.T) {
	cases := []struct {
		name      string
		readings  []pressureSample
		available int64
		want      int
	}{
		{"sustained", []pressureSample{{0, 100, true, ""}, {11, 101, true, ""}, {12, 102, true, ""}, {13, 103, true, ""}}, 10, 1},
		{"no delta", []pressureSample{{0, 100, true, ""}, {11, 100, true, ""}, {12, 100, true, ""}, {13, 100, true, ""}}, 10, 0},
		{"average low", []pressureSample{{0, 100, true, ""}, {9, 101, true, ""}, {9, 102, true, ""}, {9, 103, true, ""}}, 10, 0},
		{"memory healthy", []pressureSample{{0, 100, true, ""}, {11, 101, true, ""}, {12, 102, true, ""}, {13, 103, true, ""}}, 5000, 0},
		{"only two", []pressureSample{{0, 100, true, ""}, {11, 101, true, ""}, {12, 102, true, ""}}, 10, 0},
		{"in band holds", []pressureSample{{0, 100, true, ""}, {11, 101, true, ""}, {12, 102, true, ""}, {5, 103, true, ""}, {13, 104, true, ""}}, 10, 1},
		{"unreadable resets", []pressureSample{{0, 100, true, ""}, {11, 101, true, ""}, {12, 102, true, ""}, {0, 0, false, "read-error"}, {13, 103, true, ""}, {13, 104, true, ""}}, 10, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, events, _ := baseWatchdogDeps(tc.readings, tc.available, eligibleTree())
			state := watchdogState{}
			for range tc.readings {
				evaluateWatchdog(context.Background(), watchdogObserve, &state, d)
			}
			got := 0
			for _, event := range *events {
				if event.Decision == "would_signal" {
					got++
				}
			}
			if got != tc.want {
				t.Fatalf("would_signal=%d events=%+v", got, *events)
			}
		})
	}
}

func TestTriggerLatchesUntilGenuineRecovery(t *testing.T) {
	readings := []pressureSample{{0, 100, true, ""}, {11, 101, true, ""}, {11, 102, true, ""}, {11, 103, true, ""}, {11, 104, true, ""}, {11, 105, true, ""}, {11, 106, true, ""}, {11, 107, true, ""}, {0.5, 107, true, ""}, {11, 108, true, ""}, {11, 109, true, ""}, {11, 110, true, ""}}
	d, events, _ := baseWatchdogDeps(readings, 10, eligibleTree())
	state := watchdogState{}
	for range readings {
		evaluateWatchdog(context.Background(), watchdogObserve, &state, d)
	}
	got := 0
	for _, event := range *events {
		if event.Decision == "would_signal" {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("would_signal=%d events=%+v", got, *events)
	}
}

func TestSelectOffenderEveryPredicateGates(t *testing.T) {
	base := eligibleTree()
	if got, _ := selectOffender(base, 100, 999, "/user.slice/aira.service"); got == nil || got.pid != 20 {
		t.Fatalf("got=%+v", got)
	}
	mutations := []struct {
		name   string
		mutate func(map[int]watchdogProc)
	}{
		{"aira capped", func(p map[int]watchdogProc) {
			q := p[20]
			q.cgroup = watchdogCgroup{path: "/user.slice/.aira-job.scope", uncapped: true}
			p[20] = q
			q = p[21]
			q.rss = 0
			p[21] = q
		}},
		{"finite ancestor", func(p map[int]watchdogProc) {
			q := p[20]
			q.cgroup.uncapped = false
			p[20] = q
			q = p[21]
			q.rss = 0
			p[21] = q
		}},
		{"non agent", func(p map[int]watchdogProc) { delete(p, 10) }},
		{"light", func(p map[int]watchdogProc) {
			q := p[20]
			q.rss = 99
			p[20] = q
			q = p[21]
			q.rss = 99
			p[21] = q
		}},
		{"protected", func(p map[int]watchdogProc) {
			q := p[20]
			q.cgroup.path = "/system.slice/db.service"
			p[20] = q
			q = p[21]
			q.rss = 0
			p[21] = q
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			p := eligibleTree()
			tc.mutate(p)
			if got, _ := selectOffender(p, 100, 999, "/user.slice/aira.service"); got != nil {
				t.Fatalf("selected %+v", got)
			}
		})
	}
}

func TestSelectOffenderBiggestRSSWithPIDAscendingTieBreakAndCriticalProtection(t *testing.T) {
	p := eligibleTree()
	for _, pid := range []int{30, 19} {
		p[pid] = watchdogProc{pid: pid, ppid: 10, comm: "worker", rss: 700, startTime: uint64(pid), cgroup: watchdogCgroup{path: "/user.slice/session.scope", uncapped: true}}
	}
	selected, _ := selectOffender(p, 100, 999, "/user.slice/aira.service")
	if selected == nil || selected.pid != 19 {
		t.Fatalf("selected=%+v want pid 19", selected)
	}
	for _, proc := range []watchdogProc{
		{pid: 1, ppid: 10, comm: "worker", rss: 900, startTime: 1, cgroup: watchdogCgroup{path: "/user.slice/session.scope", uncapped: true}},
		{pid: 999, ppid: 10, comm: "worker", rss: 900, startTime: 999, cgroup: watchdogCgroup{path: "/user.slice/session.scope", uncapped: true}},
		{pid: 31, ppid: 10, comm: "worker", rss: 900, startTime: 31, cgroup: watchdogCgroup{path: "/user.slice/aira.service", uncapped: true}},
	} {
		p := eligibleTree()
		p[proc.pid] = proc
		selected, _ := selectOffender(p, 100, 999, "/user.slice/aira.service")
		if selected == nil || selected.pid != 20 {
			t.Fatalf("critical candidate %+v displaced safe selection: %+v", proc, selected)
		}
	}
}

func TestObserveAndInterlockedEnforceNeverSignal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      watchdogMode
		interlock bool
	}{
		{"observe", watchdogObserve, true}, {"active whale", watchdogEnforce, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := []pressureSample{{0, 100, true, ""}, {11, 101, true, ""}, {11, 102, true, ""}, {11, 103, true, ""}}
			d, events, signals := baseWatchdogDeps(r, 10, eligibleTree())
			d.interlockOK = func() (func(), bool, string) { return func() {}, tc.interlock, "whale-watchdog active" }
			state := watchdogState{}
			for range r {
				evaluateWatchdog(context.Background(), tc.mode, &state, d)
			}
			if len(*signals) != 0 {
				t.Fatalf("signals=%v", *signals)
			}
			found := false
			for _, e := range *events {
				if e.Decision == "would_signal" {
					found = true
				}
			}
			if !found {
				t.Fatalf("events=%+v", *events)
			}
		})
	}
}

func TestEnforceRevalidatesEveryTargetAndReportsHonestFacets(t *testing.T) {
	r := []pressureSample{{20, 105, true, ""}}
	procs := eligibleTree()
	d, events, signals := baseWatchdogDeps(r, 10, procs)
	d.startTime = func(pid int) (uint64, bool, string) {
		if pid == 20 {
			return 999, true, ""
		}
		return procs[pid].startTime, true, ""
	}
	handleArmed(context.Background(), watchdogEnforce, d, 20, 104, 4, 10, procs)
	if len(*signals) != 2 || (*signals)[0] != unix.SIGTERM || (*signals)[1] != unix.SIGKILL {
		t.Fatalf("signals=%v", *signals)
	}
	for _, e := range *events {
		if strings.Contains(e.Outcome, "killed") {
			t.Fatalf("nil signal syscall falsely claimed killed: %+v", e)
		}
	}
	seenIntent, seenDelivered := false, false
	seenUnresolved := false
	for _, e := range *events {
		seenIntent = seenIntent || e.Decision == "intent"
		seenDelivered = seenDelivered || strings.Contains(e.Outcome, "delivered")
		seenUnresolved = seenUnresolved || e.Outcome == "unresolved"
	}
	if !seenIntent || !seenDelivered || !seenUnresolved {
		t.Fatalf("events=%+v", *events)
	}
}

func TestEnforceOrdersTermGraceKillAndSettle(t *testing.T) {
	p := eligibleTree()
	delete(p, 21)
	d, _, signals := baseWatchdogDeps([]pressureSample{{20, 105, true, ""}}, 10, p)
	d.grace, d.postKillSettle = 5*time.Second, time.Second
	var sleeps []time.Duration
	d.sleep = func(_ context.Context, duration time.Duration) bool {
		sleeps = append(sleeps, duration)
		return true
	}
	handleArmed(context.Background(), watchdogEnforce, d, 20, 104, 4, 10, p)
	if len(*signals) != 2 || (*signals)[0] != unix.SIGTERM || (*signals)[1] != unix.SIGKILL {
		t.Fatalf("signals=%v", *signals)
	}
	if len(sleeps) != 2 || sleeps[0] != 5*time.Second || sleeps[1] != time.Second {
		t.Fatalf("sleeps=%v", sleeps)
	}
}

func TestEnforceRejectsChangedCgroupAndProtectedSubtreeMember(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(int, watchdogCgroup) watchdogCgroup
	}{
		{"became capped", func(pid int, c watchdogCgroup) watchdogCgroup {
			if pid == 20 {
				c.uncapped = false
			}
			return c
		}},
		{"became protected", func(pid int, c watchdogCgroup) watchdogCgroup {
			if pid == 20 {
				c.path = "/init.scope"
			}
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := eligibleTree()
			d, _, signals := baseWatchdogDeps([]pressureSample{{20, 105, true, ""}}, 10, p)
			d.cgroupOf = func(pid int) (watchdogCgroup, bool, string) { return tc.change(pid, p[pid].cgroup), true, "" }
			handleArmed(context.Background(), watchdogEnforce, d, 20, 104, 4, 10, p)
			if len(*signals) != 2 {
				t.Fatalf("signals=%v want only child pid 21 TERM+KILL", *signals)
			}
		})
	}
}

func TestSignalErrorsAreHonest(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{{"exited", unix.ESRCH, "exited"}, {"failure", unix.EPERM, "failure"}, {"unsupported", unix.ENOSYS, "degraded"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := eligibleTree()
			delete(p, 21)
			d, events, _ := baseWatchdogDeps([]pressureSample{{20, 104, true, ""}}, 10, p)
			d.pidfdSignal = func(int, unix.Signal) error { return tc.err }
			handleArmed(context.Background(), watchdogEnforce, d, 20, 104, 4, 10, p)
			joined := ""
			for _, e := range *events {
				joined += e.Outcome + " " + e.Reason
			}
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("events=%+v want %q", *events, tc.want)
			}
		})
	}
}

func TestRunWatchdogOffParksAndDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d, _, _ := baseWatchdogDeps([]pressureSample{{}}, 10, nil)
	go func() { runWatchdog(ctx, watchdogOff, time.Second, d); close(done) }()
	select {
	case <-done:
		t.Fatal("off watchdog returned before cancel")
	case <-time.After(10 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("off watchdog did not drain")
	}
}

func TestFiniteMemoryMaxAncestorAndAIRAComponent(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "user.slice", "job.scope")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, filepath.Join(root, "user.slice"), leaf} {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, finite := effectiveWatchdogCapFrom(root, leaf); finite {
		t.Fatal("unexpected finite cap")
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "memory.max"), []byte("4096\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cap, finite := effectiveWatchdogCapFrom(root, leaf); !finite || cap != 4096 {
		t.Fatalf("cap=%d finite=%v", cap, finite)
	}
	if err := os.Remove(filepath.Join(leaf, "memory.max")); err != nil {
		t.Fatal(err)
	}
	if _, _, evaluated := effectiveWatchdogCapEvaluated(root, leaf); evaluated {
		t.Fatal("missing memory.max was classified uncapped instead of unevaluated")
	}
	if !hasAIRAComponent("/user.slice/.aira-job.scope") || hasAIRAComponent("/user.slice/not.aira-job.scope") {
		t.Fatal("aira component classification wrong")
	}
}

func TestMachineInterlockCannotConfirmAndHeldLock(t *testing.T) {
	runtime := t.TempDir()
	for _, tc := range []struct {
		name, out string
		err       error
		hold      bool
		want      bool
	}{
		{"inactive", "inactive\n", nil, false, true}, {"failed", "failed\n", nil, false, true},
		{"active", "active\n", nil, false, false}, {"empty", "", errors.New("exit status 1"), false, false},
		{"other", "activating\n", nil, false, false}, {"padded", " inactive\n", nil, false, false},
		{"held", "inactive\n", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held *os.File
			if tc.hold {
				held, _ = os.OpenFile(filepath.Join(runtime, "aira-memory-watchdog.lock"), os.O_CREATE|os.O_RDWR, 0o600)
				if err := unix.Flock(int(held.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer held.Close()
			}
			release, ok, _ := machineInterlock(runtime, func(context.Context) (string, error) { return tc.out, tc.err })
			if release != nil {
				defer release()
			}
			if ok != tc.want {
				t.Fatalf("ok=%v want=%v", ok, tc.want)
			}
		})
	}
}

func TestWatchdogProcHelper(t *testing.T) {
	if os.Getenv("AIRA_WATCHDOG_PROC_HELPER") != "1" {
		return
	}
	if err := os.WriteFile(filepath.Join("/proc/self/task", strconv.Itoa(os.Getpid()), "comm"), []byte("claude"), 0o600); err != nil {
		os.Exit(2)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	_, _ = os.Stdout.WriteString(strconv.Itoa(child.Process.Pid) + "\n")
	_ = child.Wait()
	os.Exit(0)
}

func TestRealProcObserveSelectsClaudeChildWithoutSignalling(t *testing.T) {
	if os.Getenv("AIRA_WATCHDOG_REAL_PROC") != "1" {
		t.Skip("set AIRA_WATCHDOG_REAL_PROC=1 to run the real-/proc fixture")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWatchdogProcHelper$")
	cmd.Env = append(os.Environ(), "AIRA_WATCHDOG_PROC_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("helper did not report child: %v", scanner.Err())
	}
	childPID, err := strconv.Atoi(scanner.Text())
	if err != nil {
		t.Fatal(err)
	}
	mount, mountErr := watchdogUnifiedMount()
	var procs map[int]watchdogProc
	foundTree := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		procs, err = snapshotWatchdogProcs(mount, mountErr)
		if err == nil && procs[childPID].ppid == cmd.Process.Pid && procs[cmd.Process.Pid].comm == "claude" {
			foundTree = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !foundTree {
		t.Fatalf("real tree not visible: parent=%+v child=%+v", procs[cmd.Process.Pid], procs[childPID])
	}
	child := procs[childPID]
	child.cgroup.uncapped = true
	child.rss = 4096
	procs[childPID] = child
	if selected, _ := selectOffender(procs, 1024, os.Getpid(), "/not-the-helper"); selected == nil || selected.pid != childPID {
		t.Fatalf("selected=%+v want child pid %d", selected, childPID)
	}
	deps, events, signals := baseWatchdogDeps([]pressureSample{{20, 2, true, ""}}, 10, procs)
	deps.minVictimRSS = 1024
	deps.daemonPID, deps.daemonCgroup = os.Getpid(), "/not-the-helper"
	if !handleArmed(context.Background(), watchdogObserve, deps, 20, 2, 1, 10, procs) {
		t.Fatal("observe did not produce an armed decision")
	}
	if len(*signals) != 0 || len(*events) != 1 || (*events)[0].Decision != "would_signal" || (*events)[0].PID != childPID {
		t.Fatalf("events=%+v signals=%v", *events, *signals)
	}
	child.cgroup.uncapped = false
	procs[childPID] = child
	if selected, _ := selectOffender(procs, 1024, os.Getpid(), "/not-the-helper"); selected != nil && selected.pid == childPID {
		t.Fatalf("capped real child selected: %+v", selected)
	}
}

func TestPidfdOpenENOSYSDegradesWithoutSignal(t *testing.T) {
	p := eligibleTree()
	d, events, signals := baseWatchdogDeps([]pressureSample{{20, 104, true, ""}}, 10, p)
	d.pidfdOpen = func(int) (int, error) { return -1, unix.ENOSYS }
	handleArmed(context.Background(), watchdogEnforce, d, 20, 104, 4, 10, p)
	if len(*signals) != 0 {
		t.Fatalf("signals=%v", *signals)
	}
	joined := ""
	for _, e := range *events {
		joined += e.Outcome + e.Reason
	}
	if !strings.Contains(joined, "degraded") {
		t.Fatalf("events=%+v", *events)
	}
}

func TestWatchdogAuditBroadcastsToReadyProjectsOnly(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	builder := NewServer(paths)
	builder.db = db
	readyOne, _, err := builder.storeForScope(independentScope(t, paths, "watchdog-ready-one", "WDONE"))
	if err != nil {
		t.Fatal(err)
	}
	readyTwo, _, err := builder.storeForScope(independentScope(t, paths, "watchdog-ready-two", "WDTWO"))
	if err != nil {
		t.Fatal(err)
	}
	unready, _, err := builder.storeForScope(independentScope(t, paths, "watchdog-unready", "WDNO"))
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	close(closed)
	server := NewServer(paths)
	server.scopes = map[string]*scopeEntry{
		"one": {view: readyOne, ready: closed},
		"two": {view: readyTwo, ready: closed},
		"no":  {view: unready, ready: make(chan struct{})},
	}
	if err := server.emitWatchdogEvent(context.Background(), watchdogEvent{At: time.Now(), Mode: watchdogObserve, Decision: "would_signal", PID: 42, Outcome: "WOULD SIGKILL"}); err != nil {
		t.Fatal(err)
	}
	for name, view := range map[string]*store.Store{"one": readyOne, "two": readyTwo, "unready": unready} {
		events, _, err := view.EventsSince(context.Background(), 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if name == "unready" {
			want = 0
		}
		if len(events) != want {
			t.Fatalf("%s events=%+v want %d", name, events, want)
		}
	}
}
