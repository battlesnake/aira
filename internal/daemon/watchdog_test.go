package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
		lowMemAvailable:     1000,
		recoverMemAvailable: 2000,
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

func TestMemLowAloneTripsAfterDebounce(t *testing.T) {
	readings := []pressureSample{{0, 100, true, ""}, {0, 100, true, ""}, {0, 100, true, ""}}
	d, events, _ := baseWatchdogDeps(readings, 10, eligibleTree())
	state := watchdogState{}
	for range readings {
		evaluateWatchdog(context.Background(), watchdogObserve, &state, d)
	}
	for _, event := range *events {
		if event.Decision == "would_signal" {
			return
		}
	}
	t.Fatalf("mem-low-only polls did not trip after debounce: events=%+v state=%+v", *events, state)
}

func TestTriggerUsesOnlyConsecutiveLowMemory(t *testing.T) {
	cases := []struct {
		name      string
		readings  []pressureSample
		available []struct {
			value  int64
			ok     bool
			reason string
		}
		want            int
		wantUnevaluated int
	}{
		{"calm PSI", []pressureSample{{0, 100, true, ""}}, []struct {
			value  int64
			ok     bool
			reason string
		}{{10, true, ""}, {10, true, ""}, {10, true, ""}}, 1, 0},
		{"unchanged PSI total", []pressureSample{{20, 100, true, ""}}, []struct {
			value  int64
			ok     bool
			reason string
		}{{10, true, ""}, {10, true, ""}, {10, true, ""}}, 1, 0},
		{"PSI unavailable", []pressureSample{{0, 0, false, "read-error"}}, []struct {
			value  int64
			ok     bool
			reason string
		}{{10, true, ""}, {10, true, ""}, {10, true, ""}}, 1, 0},
		{"boundary is healthy", []pressureSample{{20, 101, true, ""}}, []struct {
			value  int64
			ok     bool
			reason string
		}{{1000, true, ""}, {1000, true, ""}, {1000, true, ""}}, 0, 0},
		{"single dip", []pressureSample{{20, 101, true, ""}}, []struct {
			value  int64
			ok     bool
			reason string
		}{{10, true, ""}, {5000, true, ""}, {5000, true, ""}}, 0, 0},
		{"mem failure resets", []pressureSample{{20, 101, true, ""}}, []struct {
			value  int64
			ok     bool
			reason string
		}{{10, true, ""}, {10, true, ""}, {0, false, "read-error"}, {10, true, ""}, {10, true, ""}}, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, events, _ := baseWatchdogDeps(tc.readings, 0, eligibleTree())
			memIndex := 0
			d.readMemAvailable = func() (int64, bool, string) {
				reading := tc.available[memIndex]
				memIndex++
				return reading.value, reading.ok, reading.reason
			}
			state := watchdogState{}
			for range tc.available {
				evaluateWatchdog(context.Background(), watchdogObserve, &state, d)
			}
			got, unevaluated := 0, 0
			for _, event := range *events {
				if event.Decision == "would_signal" {
					got++
				}
				if event.Decision == "unevaluated" {
					unevaluated++
				}
			}
			if got != tc.want || unevaluated != tc.wantUnevaluated {
				t.Fatalf("would_signal=%d unevaluated=%d events=%+v", got, unevaluated, *events)
			}
		})
	}
}

func TestTriggerLatchesUntilMemoryRecoveryThreshold(t *testing.T) {
	available := []int64{10, 10, 10, 10, 1999, 2000, 10, 10, 10}
	d, events, _ := baseWatchdogDeps([]pressureSample{{0, 100, true, ""}}, 0, eligibleTree())
	index := 0
	d.readMemAvailable = func() (int64, bool, string) {
		value := available[index]
		index++
		return value, true, ""
	}
	state := watchdogState{}
	for range available {
		evaluateWatchdog(context.Background(), watchdogObserve, &state, d)
	}
	wouldSignal, recovered := 0, 0
	for _, event := range *events {
		if event.Decision == "would_signal" {
			wouldSignal++
		}
		if event.Decision == "recovered" {
			recovered++
		}
	}
	if wouldSignal != 2 || recovered != 1 {
		t.Fatalf("would_signal=%d recovered=%d events=%+v", wouldSignal, recovered, *events)
	}
}

func TestUnavailablePSIIsAbsentFromEventJSON(t *testing.T) {
	d, events, _ := baseWatchdogDeps([]pressureSample{{0, 0, false, "read-error"}}, 0, eligibleTree())
	var logs []string
	d.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	d.readMemAvailable = func() (int64, bool, string) { return 0, false, "read-error" }
	evaluateWatchdog(context.Background(), watchdogObserve, &watchdogState{}, d)
	if len(*events) != 1 {
		t.Fatalf("events=%+v", *events)
	}
	event := (*events)[0]
	if event.PSIAvg10 != nil || event.PSITotal != nil {
		t.Fatalf("unavailable PSI was represented as measured: %+v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"psi_full_avg10", "psi_full_total_us", "psi_full_delta_us"} {
		if _, exists := fields[field]; exists {
			t.Fatalf("%s must be absent when PSI is unavailable: %s", field, encoded)
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "psi_avg10=?") {
		t.Fatalf("unavailable PSI log must use ?: %v", logs)
	}
}

func TestDeferDoesNotLatchAndRearms(t *testing.T) {
	d, events, _ := baseWatchdogDeps([]pressureSample{{0, 100, true, ""}}, 10, nil)
	state := watchdogState{}
	for range 6 {
		evaluateWatchdog(context.Background(), watchdogObserve, &state, d)
	}
	deferred := 0
	for _, event := range *events {
		if event.Decision == "defer" {
			deferred++
		}
	}
	if deferred != 2 || state.latched {
		t.Fatalf("deferred=%d state=%+v events=%+v", deferred, state, *events)
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
	handleArmed(context.Background(), watchdogEnforce, d, pressureSample{avg10: 20, total: 104, ok: true}, 10, procs)
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
	handleArmed(context.Background(), watchdogEnforce, d, pressureSample{avg10: 20, total: 104, ok: true}, 10, p)
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
			handleArmed(context.Background(), watchdogEnforce, d, pressureSample{avg10: 20, total: 104, ok: true}, 10, p)
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
			handleArmed(context.Background(), watchdogEnforce, d, pressureSample{avg10: 20, total: 104, ok: true}, 10, p)
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

func TestEnforceReadsPSIOnlyAfterFirstSignal(t *testing.T) {
	p := eligibleTree()
	delete(p, 21)
	d, events, _ := baseWatchdogDeps([]pressureSample{{20, 104, true, ""}}, 10, p)
	var order []string
	psiReads := 0
	d.readPressure = func() (float64, uint64, bool, string) {
		psiReads++
		order = append(order, "psi")
		return 20, 104, true, ""
	}
	d.pidfdSignal = func(int, unix.Signal) error {
		order = append(order, "signal")
		return nil
	}
	state := watchdogState{}
	for range d.debounce {
		evaluateWatchdog(context.Background(), watchdogEnforce, &state, d)
	}
	if len(order) == 0 || order[0] != "signal" {
		t.Fatalf("operation order=%v; /proc PSI read must not precede signal", order)
	}
	if !slicesContain(order, "psi") {
		t.Fatalf("post-signal outcome did not collect observational PSI: order=%v", order)
	}
	if psiReads != 1 {
		t.Fatalf("PSI reads=%d want exactly one per emitting poll", psiReads)
	}
	observedOutcome := false
	for _, event := range *events {
		if (event.Decision == "trip" || event.Decision == "intent") && (event.PSIAvg10 != nil || event.PSITotal != nil) {
			t.Fatalf("pre-signal event carried PSI: %+v", event)
		}
		if event.Decision == "outcome" && event.PSIAvg10 != nil && event.PSITotal != nil {
			observedOutcome = true
		}
	}
	if !observedOutcome {
		t.Fatalf("post-signal outcome lacks PSI: events=%+v", *events)
	}
}

func TestHandleArmedLatchOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		mode      watchdogMode
		signalErr error
		want      bool
	}{
		{"retryable failure", watchdogEnforce, unix.EPERM, false},
		{"observe would signal", watchdogObserve, nil, true},
		{"ENOSYS degrade", watchdogEnforce, unix.ENOSYS, true},
		{"all terminal", watchdogEnforce, unix.ESRCH, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := eligibleTree()
			delete(p, 21)
			d, _, _ := baseWatchdogDeps([]pressureSample{{20, 104, true, ""}}, 10, p)
			d.pidfdSignal = func(int, unix.Signal) error { return tc.signalErr }
			if got := handleArmed(context.Background(), tc.mode, d, pressureSample{avg10: 20, total: 104, ok: true}, 10, p); got != tc.want {
				t.Fatalf("acted=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestRetryableEnforceFailureRearmsNextCycle(t *testing.T) {
	p := eligibleTree()
	delete(p, 21)
	d, _, _ := baseWatchdogDeps([]pressureSample{{20, 104, true, ""}}, 10, p)
	signalCalls := 0
	d.pidfdSignal = func(int, unix.Signal) error {
		signalCalls++
		return unix.EPERM
	}
	state := watchdogState{}
	for range 2 * d.debounce {
		evaluateWatchdog(context.Background(), watchdogEnforce, &state, d)
	}
	if signalCalls != 4 || state.latched {
		t.Fatalf("signalCalls=%d state=%+v; retryable failures must re-arm", signalCalls, state)
	}
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestPressureStillTrippedUsesOnlyMemory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available int64
		ok        bool
		want      bool
	}{
		{"low", 999, true, true},
		{"boundary", 1000, true, false},
		{"read failure", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := baseWatchdogDeps([]pressureSample{{0, 0, false, "must not read"}}, tc.available, nil)
			d.readMemAvailable = func() (int64, bool, string) { return tc.available, tc.ok, "read-error" }
			d.readPressure = func() (float64, uint64, bool, string) {
				t.Fatal("escalation gate read PSI")
				return 0, 0, false, ""
			}
			if got := pressureStillTripped(d); got != tc.want {
				t.Fatalf("pressureStillTripped=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestRunWatchdogOffParksAndDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	go func() { runWatchdog(ctx, watchdogOff, time.Second, watchdogDeps{}); close(done) }()
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
	if logs.Len() != 0 {
		t.Fatalf("off mode logged invariant error: %q", logs.String())
	}
}

func TestRunWatchdogInvalidDepsFailsLoudWithoutActing(t *testing.T) {
	d, events, signals := baseWatchdogDeps([]pressureSample{{20, 101, true, ""}}, 10, eligibleTree())
	d.debounce = 0
	d.sleep = func(context.Context, time.Duration) bool { return false }
	memReads := 0
	d.readMemAvailable = func() (int64, bool, string) {
		memReads++
		return 10, true, ""
	}
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	runWatchdog(context.Background(), watchdogObserve, time.Second, d)
	if memReads != 0 || len(*events) != 0 || len(*signals) != 0 {
		t.Fatalf("invalid watchdog acted: memReads=%d events=%+v signals=%v", memReads, *events, *signals)
	}
	if !strings.Contains(logs.String(), "invalid dependencies") {
		t.Fatalf("missing loud invariant log: %q", logs.String())
	}
}

func TestWatchdogDepsInvariantRequiresEveryKillPathDependency(t *testing.T) {
	base, _, _ := baseWatchdogDeps([]pressureSample{{20, 101, true, ""}}, 10, eligibleTree())
	if !validWatchdogDeps(base) {
		t.Fatal("base test dependencies unexpectedly invalid")
	}
	cases := []struct {
		name   string
		breaks func(*watchdogDeps)
	}{
		{"readMemAvailable", func(d *watchdogDeps) { d.readMemAvailable = nil }},
		{"readPressure", func(d *watchdogDeps) { d.readPressure = nil }},
		{"snapshotProcs", func(d *watchdogDeps) { d.snapshotProcs = nil }},
		{"emitEvent", func(d *watchdogDeps) { d.emitEvent = nil }},
		{"pidfdOpen", func(d *watchdogDeps) { d.pidfdOpen = nil }},
		{"pidfdSignal", func(d *watchdogDeps) { d.pidfdSignal = nil }},
		{"closeFD", func(d *watchdogDeps) { d.closeFD = nil }},
		{"startTime", func(d *watchdogDeps) { d.startTime = nil }},
		{"cgroupOf", func(d *watchdogDeps) { d.cgroupOf = nil }},
		{"interlockOK", func(d *watchdogDeps) { d.interlockOK = nil }},
		{"now", func(d *watchdogDeps) { d.now = nil }},
		{"sleep", func(d *watchdogDeps) { d.sleep = nil }},
		{"low threshold", func(d *watchdogDeps) { d.lowMemAvailable = 0 }},
		{"recovery threshold", func(d *watchdogDeps) { d.recoverMemAvailable = d.lowMemAvailable }},
		{"debounce", func(d *watchdogDeps) { d.debounce = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.breaks(&d)
			if validWatchdogDeps(d) {
				t.Fatal("invalid dependencies accepted")
			}
		})
	}
}

func TestRealWatchdogDepsThresholdsAndInvariantWiring(t *testing.T) {
	d := realWatchdogDeps(&Server{})
	if d.lowMemAvailable != watchdogLowMemAvailable || d.recoverMemAvailable != watchdogRecoverMemAvailable || d.debounce != watchdogDebounce {
		t.Fatalf("threshold wiring: low=%d recover=%d debounce=%d", d.lowMemAvailable, d.recoverMemAvailable, d.debounce)
	}
	if d.lowMemAvailable <= 0 || d.recoverMemAvailable <= d.lowMemAvailable || d.debounce < 1 || d.logf == nil {
		t.Fatalf("invalid production watchdog deps: %+v", d)
	}
}

func TestWatchdogDecisionLoggingAndIdleDoesNotReadPSI(t *testing.T) {
	d, events, _ := baseWatchdogDeps([]pressureSample{{12.5, 123, true, ""}}, 5000, eligibleTree())
	var logs []string
	d.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	psiReads := 0
	d.readPressure = func() (float64, uint64, bool, string) {
		psiReads++
		return 12.5, 123, true, ""
	}
	evaluateWatchdog(context.Background(), watchdogObserve, &watchdogState{}, d)
	if len(*events) != 0 || len(logs) != 0 || psiReads != 0 {
		t.Fatalf("idle poll emitted or read PSI: events=%+v logs=%v psiReads=%d", *events, logs, psiReads)
	}
	d.readMemAvailable = func() (int64, bool, string) { return 0, false, "read-error" }
	evaluateWatchdog(context.Background(), watchdogObserve, &watchdogState{}, d)
	if len(*events) != 1 || len(logs) != 1 || psiReads != 1 {
		t.Fatalf("decision observability: events=%+v logs=%v psiReads=%d", *events, logs, psiReads)
	}
	if !strings.Contains(logs[0], "watchdog unevaluated") || !strings.Contains(logs[0], "mem_avail=") || !strings.Contains(logs[0], "psi_avg10=12.5") {
		t.Fatalf("unexpected watchdog log: %q", logs[0])
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
	// A leaf WITHOUT memory.max (the memory controller isn't limiting there) is
	// "no cap at the leaf" — the walk must CONTINUE and still find the finite
	// ancestor cap (user.slice=4096), NOT abort to unevaluated. Aborting on an
	// absent memory.max made the watchdog inert on every real host, where the
	// cgroup2 mount root itself has no memory.max (build-review P1).
	if cap, finite, evaluated := effectiveWatchdogCapEvaluated(root, leaf); !evaluated || !finite || cap != 4096 {
		t.Fatalf("leaf without memory.max: cap=%d finite=%v evaluated=%v; want cap=4096 finite=true evaluated=true (bounded by the ancestor slice)", cap, finite, evaluated)
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
	if !handleArmed(context.Background(), watchdogObserve, deps, pressureSample{avg10: 20, total: 2, ok: true}, 10, procs) {
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
	handleArmed(context.Background(), watchdogEnforce, d, pressureSample{avg10: 20, total: 104, ok: true}, 10, p)
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
