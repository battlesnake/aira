package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aira/internal/store"
	"golang.org/x/sys/unix"
)

const (
	watchdogTripPSIFullAvg10    = 10.0
	watchdogRecoverPSIFullAvg10 = 1.0
	watchdogLowMemAvailable     = int64(8 << 30)
	watchdogMinVictimRSS        = int64(2 << 30)
	watchdogDebounce            = 3
	watchdogGrace               = 5 * time.Second
	watchdogPostKillSettle      = time.Second
	watchdogEmitTimeout         = 250 * time.Millisecond
)

type pressureSample struct {
	avg10  float64
	total  uint64
	ok     bool
	reason string
}

type watchdogCgroup struct {
	path     string
	uncapped bool
}

type watchdogProc struct {
	pid       int
	ppid      int
	comm      string
	rss       int64
	startTime uint64
	cgroup    watchdogCgroup
}

type watchdogPredicates struct {
	Uncapped          bool `json:"uncapped"`
	AgentAttributable bool `json:"agent_attributable"`
	Heavy             bool `json:"heavy"`
	NotProtected      bool `json:"not_protected"`
}

type watchdogEvent struct {
	At           time.Time          `json:"at"`
	Mode         watchdogMode       `json:"mode"`
	Decision     string             `json:"decision"`
	Reason       string             `json:"reason,omitempty"`
	PSIAvg10     float64            `json:"psi_full_avg10"`
	PSITotal     uint64             `json:"psi_full_total_us"`
	PSIDelta     uint64             `json:"psi_full_delta_us"`
	MemAvailable int64              `json:"mem_available_bytes,omitempty"`
	PID          int                `json:"pid,omitempty"`
	Comm         string             `json:"comm,omitempty"`
	RSS          int64              `json:"rss_bytes,omitempty"`
	StartTime    uint64             `json:"start_time,omitempty"`
	Predicates   watchdogPredicates `json:"predicates"`
	Outcome      string             `json:"outcome,omitempty"`
}

type watchdogDeps struct {
	readPressure        func() (float64, uint64, bool, string)
	readMemAvailable    func() (int64, bool, string)
	snapshotProcs       func() (map[int]watchdogProc, error)
	pidfdOpen           func(int) (int, error)
	pidfdSignal         func(int, unix.Signal) error
	closeFD             func(int) error
	startTime           func(int) (uint64, bool, string)
	cgroupOf            func(int) (watchdogCgroup, bool, string)
	interlockOK         func() (release func(), ok bool, reason string)
	emitEvent           func(context.Context, watchdogEvent) error
	sleep               func(context.Context, time.Duration) bool
	now                 func() time.Time
	minVictimRSS        int64
	tripPSIFullAvg10    float64
	recoverPSIFullAvg10 float64
	lowMemAvailable     int64
	debounce            int
	grace               time.Duration
	postKillSettle      time.Duration
	daemonPID           int
	daemonCgroup        string
}

type watchdogState struct {
	haveTotal bool
	lastTotal uint64
	armCount  int
	latched   bool
}

func readHostPressureFull() (float64, uint64, bool, string) {
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0, 0, false, "read-error"
	}
	return parseHostPressureFull(data)
}

func parseHostPressureFull(data []byte) (float64, uint64, bool, string) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "full" {
			continue
		}
		values := make(map[string]string, len(fields)-1)
		for _, field := range fields[1:] {
			key, value, found := strings.Cut(field, "=")
			if !found || key == "" || value == "" {
				return 0, 0, false, "parse-error"
			}
			values[key] = value
		}
		avg, avgErr := strconv.ParseFloat(values["avg10"], 64)
		total, totalErr := strconv.ParseUint(values["total"], 10, 64)
		if avgErr != nil || totalErr != nil || avg < 0 || math.IsNaN(avg) || math.IsInf(avg, 0) {
			return 0, 0, false, "parse-error"
		}
		return avg, total, true, ""
	}
	return 0, 0, false, "full-line-absent"
}

func readMemAvailable() (int64, bool, string) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false, "read-error"
	}
	return parseMemAvailable(data)
}

func parseMemAvailable(data []byte) (int64, bool, string) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "MemAvailable:" {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, false, "parse-error"
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb < 0 || kb > (1<<63-1)/1024 {
			return 0, false, "parse-error"
		}
		return kb * 1024, true, ""
	}
	return 0, false, "memavailable-absent"
}

func runWatchdog(ctx context.Context, mode watchdogMode, interval time.Duration, deps watchdogDeps) {
	if mode == watchdogOff || interval == 0 {
		<-ctx.Done()
		return
	}
	state := watchdogState{}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		evaluateWatchdog(ctx, mode, &state, deps)
		if !deps.sleep(ctx, interval) {
			return
		}
	}
}

func (s *Server) runWatchdog(ctx context.Context, mode watchdogMode, interval time.Duration, deps watchdogDeps) {
	runWatchdog(ctx, mode, interval, deps)
}

func evaluateWatchdog(ctx context.Context, mode watchdogMode, state *watchdogState, deps watchdogDeps) {
	avg, total, ok, reason := deps.readPressure()
	base := watchdogEvent{At: deps.now(), Mode: mode, PSIAvg10: avg, PSITotal: total}
	if !ok {
		state.armCount = 0
		state.haveTotal = false
		base.Decision, base.Reason = "unevaluated", "psi:"+reason
		emitWatchdog(ctx, deps, base)
		return
	}
	if !state.haveTotal {
		state.haveTotal, state.lastTotal = true, total
		return
	}
	if total < state.lastTotal {
		state.lastTotal, state.armCount = total, 0
		base.Decision, base.Reason = "unevaluated", "psi:counter-reset"
		emitWatchdog(ctx, deps, base)
		return
	}
	delta := total - state.lastTotal
	state.lastTotal = total
	base.PSIDelta = delta
	available, memOK, memReason := deps.readMemAvailable()
	base.MemAvailable = available
	if !memOK {
		state.armCount = 0
		base.Decision, base.Reason = "unevaluated", "memavailable:"+memReason
		emitWatchdog(ctx, deps, base)
		return
	}
	recovered := avg <= deps.recoverPSIFullAvg10 && delta == 0
	if state.latched {
		if recovered {
			state.latched = false
			state.armCount = 0
			base.Decision = "recovered"
			emitWatchdog(ctx, deps, base)
		}
		return
	}
	trip := avg >= deps.tripPSIFullAvg10 && delta > 0 && available < deps.lowMemAvailable
	if trip {
		state.armCount++
	} else if avg <= deps.recoverPSIFullAvg10 || avg >= deps.tripPSIFullAvg10 {
		state.armCount = 0
	}
	if state.armCount < deps.debounce {
		return
	}
	base.Decision = "trip"
	emitWatchdog(ctx, deps, base)
	procs, err := deps.snapshotProcs()
	if err != nil {
		state.armCount = 0
		base.Decision, base.Reason = "unevaluated", "process-snapshot:"+err.Error()
		emitWatchdog(ctx, deps, base)
		return
	}
	acted := handleArmed(ctx, mode, deps, avg, total, delta, available, procs)
	state.armCount = 0
	state.latched = acted
}

func claudeDescendants(procs map[int]watchdogProc) map[int]bool {
	children := make(map[int][]int)
	queue := make([]int, 0)
	for pid, proc := range procs {
		children[proc.ppid] = append(children[proc.ppid], pid)
		if proc.comm == "claude" {
			queue = append(queue, pid)
		}
	}
	for ppid := range children {
		sort.Ints(children[ppid])
	}
	seen := make(map[int]bool)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}
	for pid, proc := range procs {
		if proc.comm == "claude" {
			delete(seen, pid)
		}
	}
	return seen
}

func selectOffender(procs map[int]watchdogProc, minRSS int64, daemonPID int, daemonCgroup string) (*watchdogProc, map[int]watchdogPredicates) {
	descendants := claudeDescendants(procs)
	verdicts := make(map[int]watchdogPredicates, len(procs))
	var selected *watchdogProc
	for pid, proc := range procs {
		verdict := watchdogPredicates{
			Uncapped:          proc.cgroup.uncapped && !hasAIRAComponent(proc.cgroup.path),
			AgentAttributable: descendants[pid],
			Heavy:             proc.rss >= minRSS,
			NotProtected:      !watchdogProtected(pid, proc.cgroup.path, daemonPID, daemonCgroup),
		}
		verdicts[pid] = verdict
		if !verdict.Uncapped || !verdict.AgentAttributable || !verdict.Heavy || !verdict.NotProtected {
			continue
		}
		if selected == nil || proc.rss > selected.rss || (proc.rss == selected.rss && pid < selected.pid) {
			copy := proc
			selected = &copy
		}
	}
	return selected, verdicts
}

func handleArmed(ctx context.Context, mode watchdogMode, deps watchdogDeps, avg float64, total, delta uint64, available int64, procs map[int]watchdogProc) bool {
	offender, verdicts := selectOffender(procs, deps.minVictimRSS, deps.daemonPID, deps.daemonCgroup)
	if offender == nil {
		emitWatchdog(ctx, deps, watchdogEvent{At: deps.now(), Mode: mode, Decision: "defer", Reason: "pressure elsewhere; deferring to oomd/kernel", PSIAvg10: avg, PSITotal: total, PSIDelta: delta, MemAvailable: available})
		return false
	}
	base := watchdogEvent{At: deps.now(), Mode: mode, PSIAvg10: avg, PSITotal: total, PSIDelta: delta, MemAvailable: available, PID: offender.pid, Comm: offender.comm, RSS: offender.rss, StartTime: offender.startTime, Predicates: verdicts[offender.pid]}
	if mode == watchdogObserve {
		base.Decision, base.Outcome = "would_signal", "WOULD SIGKILL"
		emitWatchdog(ctx, deps, base)
		return true
	}
	release, allowed, reason := deps.interlockOK()
	if !allowed {
		base.Decision, base.Reason, base.Outcome = "would_signal", "interlock: "+reason, "degraded_to_observe; WOULD SIGKILL"
		emitWatchdog(ctx, deps, base)
		return true
	}
	if release == nil {
		base.Decision, base.Reason, base.Outcome = "would_signal", "interlock: missing authority release", "degraded_to_observe; WOULD SIGKILL"
		emitWatchdog(ctx, deps, base)
		return true
	}
	defer release()
	targets := offenderSubtree(procs, offender.pid)
	type openedTarget struct {
		proc watchdogProc
		fd   int
	}
	opened := make([]openedTarget, 0, len(targets))
	defer func() {
		for _, target := range opened {
			_ = deps.closeFD(target.fd)
		}
	}()
	for _, proc := range targets {
		fd, err := deps.pidfdOpen(proc.pid)
		if errors.Is(err, unix.ENOSYS) {
			base.Decision, base.Reason, base.Outcome = "degraded", "pidfd_open unsupported", "degraded_no_signal"
			emitWatchdog(ctx, deps, base)
			return true
		}
		if errors.Is(err, unix.ESRCH) {
			outcome := base
			outcome.PID, outcome.Decision, outcome.Outcome = proc.pid, "outcome", "exited"
			emitWatchdog(ctx, deps, outcome)
			continue
		}
		if err != nil {
			outcome := base
			outcome.PID, outcome.Decision, outcome.Reason, outcome.Outcome = proc.pid, "outcome", err.Error(), "failure"
			emitWatchdog(ctx, deps, outcome)
			continue
		}
		opened = append(opened, openedTarget{proc: proc, fd: fd})
	}
	if len(opened) == 0 {
		return true
	}
	survivors := make([]openedTarget, 0, len(opened))
	for _, target := range opened {
		if valid, reason := revalidateWatchdogTarget(target.proc, deps); !valid {
			outcome := eventForTarget(base, target.proc)
			outcome.Decision, outcome.Outcome, outcome.Reason = "outcome", "skipped", reason
			emitWatchdog(ctx, deps, outcome)
			continue
		}
		intent := eventForTarget(base, target.proc)
		intent.Decision, intent.Outcome = "intent", "about_to_sigterm"
		emitWatchdog(ctx, deps, intent)
		err := deps.pidfdSignal(target.fd, unix.SIGTERM)
		outcome := eventForTarget(base, target.proc)
		outcome.Decision = "outcome"
		switch {
		case err == nil:
			outcome.Outcome = "signal_sent,delivered"
			survivors = append(survivors, target)
		case errors.Is(err, unix.ESRCH):
			outcome.Outcome = "signal_sent,exited"
		case errors.Is(err, unix.ENOSYS):
			outcome.Outcome, outcome.Reason = "degraded_no_signal", "pidfd_send_signal unsupported"
			emitWatchdog(ctx, deps, outcome)
			return true
		default:
			outcome.Outcome, outcome.Reason = "signal_sent,failure", err.Error()
			survivors = append(survivors, target)
		}
		emitWatchdog(ctx, deps, outcome)
	}
	if len(survivors) == 0 || !deps.sleep(ctx, deps.grace) {
		return true
	}
	stillTripped, currentTotal := pressureStillTripped(deps, total)
	if !stillTripped {
		for _, target := range survivors {
			outcome := eventForTarget(base, target.proc)
			outcome.Decision, outcome.Outcome = "outcome", targetStateAfterGrace(target.proc, deps)
			emitWatchdog(ctx, deps, outcome)
		}
		return true
	}
	escalated := make([]openedTarget, 0, len(survivors))
	for _, target := range survivors {
		if valid, reason := revalidateWatchdogTarget(target.proc, deps); !valid {
			outcome := eventForTarget(base, target.proc)
			outcome.Decision, outcome.Outcome, outcome.Reason = "outcome", targetStateAfterGrace(target.proc, deps), reason
			emitWatchdog(ctx, deps, outcome)
			continue
		}
		intent := eventForTarget(base, target.proc)
		intent.PSITotal, intent.PSIDelta = currentTotal, currentTotal-total
		intent.Decision, intent.Outcome = "intent", "about_to_sigkill"
		emitWatchdog(ctx, deps, intent)
		err := deps.pidfdSignal(target.fd, unix.SIGKILL)
		outcome := eventForTarget(base, target.proc)
		outcome.Decision = "outcome"
		switch {
		case err == nil:
			outcome.Outcome = "signal_sent,delivered,escalated_sigkill"
			escalated = append(escalated, target)
		case errors.Is(err, unix.ESRCH):
			outcome.Outcome = "signal_sent,exited"
		case errors.Is(err, unix.ENOSYS):
			outcome.Outcome, outcome.Reason = "degraded_no_signal", "pidfd_send_signal unsupported"
		default:
			outcome.Outcome, outcome.Reason = "signal_sent,failure", err.Error()
		}
		emitWatchdog(ctx, deps, outcome)
	}
	if len(escalated) > 0 && deps.sleep(ctx, deps.postKillSettle) {
		for _, target := range escalated {
			final := eventForTarget(base, target.proc)
			final.Decision, final.Outcome = "outcome", targetStateAfterGrace(target.proc, deps)
			emitWatchdog(ctx, deps, final)
		}
	}
	return true
}

func eventForTarget(base watchdogEvent, proc watchdogProc) watchdogEvent {
	base.PID, base.Comm, base.RSS, base.StartTime = proc.pid, proc.comm, proc.rss, proc.startTime
	return base
}

func targetStateAfterGrace(proc watchdogProc, deps watchdogDeps) string {
	start, ok, reason := deps.startTime(proc.pid)
	if !ok && reason == "exited" {
		return "exited"
	}
	if !ok || start != proc.startTime {
		return "unresolved"
	}
	return "unresolved"
}

func pressureStillTripped(deps watchdogDeps, previousTotal uint64) (bool, uint64) {
	avg, total, ok, _ := deps.readPressure()
	if !ok || total <= previousTotal || avg < deps.tripPSIFullAvg10 {
		return false, total
	}
	available, ok, _ := deps.readMemAvailable()
	return ok && available < deps.lowMemAvailable, total
}

func revalidateWatchdogTarget(proc watchdogProc, deps watchdogDeps) (bool, string) {
	start, ok, reason := deps.startTime(proc.pid)
	if !ok {
		return false, "start-time:" + reason
	}
	if start != proc.startTime {
		return false, "start-time-mismatch"
	}
	cgroup, ok, reason := deps.cgroupOf(proc.pid)
	if !ok {
		return false, "cgroup:" + reason
	}
	if !cgroup.uncapped || hasAIRAComponent(cgroup.path) {
		return false, "cgroup-now-capped"
	}
	if watchdogProtected(proc.pid, cgroup.path, deps.daemonPID, deps.daemonCgroup) {
		return false, "target-now-protected"
	}
	return true, ""
}

func offenderSubtree(procs map[int]watchdogProc, root int) []watchdogProc {
	children := make(map[int][]int)
	for pid, proc := range procs {
		children[proc.ppid] = append(children[proc.ppid], pid)
	}
	for ppid := range children {
		sort.Ints(children[ppid])
	}
	queue := []int{root}
	result := make([]watchdogProc, 0)
	seen := make(map[int]bool)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if proc, ok := procs[pid]; ok {
			result = append(result, proc)
			queue = append(queue, children[pid]...)
		}
	}
	return result
}

func watchdogProtected(pid int, cgroup string, daemonPID int, daemonCgroup string) bool {
	if pid == 1 || pid == daemonPID || (daemonCgroup != "" && filepath.Clean(cgroup) == filepath.Clean(daemonCgroup)) {
		return true
	}
	for _, component := range strings.Split(filepath.Clean(cgroup), string(filepath.Separator)) {
		if component == "system.slice" || component == "init.scope" {
			return true
		}
	}
	return false
}

func hasAIRAComponent(cgroup string) bool {
	for _, component := range strings.Split(filepath.Clean(cgroup), string(filepath.Separator)) {
		if strings.HasPrefix(component, ".aira-") {
			return true
		}
	}
	return false
}

func emitWatchdog(ctx context.Context, deps watchdogDeps, event watchdogEvent) {
	if deps.emitEvent == nil {
		return
	}
	emitCtx, cancel := context.WithTimeout(ctx, watchdogEmitTimeout)
	defer cancel()
	if err := deps.emitEvent(emitCtx, event); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("aira daemon: watchdog audit: %v", err)
	}
}

func realWatchdogDeps(s *Server) watchdogDeps {
	mount, mountErr := watchdogUnifiedMount()
	daemonGroup, _, _ := watchdogCgroupForPID(os.Getpid(), mount, mountErr)
	return watchdogDeps{
		readPressure:        readHostPressureFull,
		readMemAvailable:    readMemAvailable,
		snapshotProcs:       func() (map[int]watchdogProc, error) { return snapshotWatchdogProcs(mount, mountErr) },
		pidfdOpen:           func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) },
		pidfdSignal:         func(fd int, signal unix.Signal) error { return unix.PidfdSendSignal(fd, signal, nil, 0) },
		closeFD:             unix.Close,
		startTime:           readProcStartTime,
		cgroupOf:            func(pid int) (watchdogCgroup, bool, string) { return watchdogCgroupForPID(pid, mount, mountErr) },
		interlockOK:         realWatchdogInterlock,
		emitEvent:           s.emitWatchdogEvent,
		sleep:               watchdogSleep,
		now:                 time.Now,
		minVictimRSS:        watchdogMinVictimRSS,
		tripPSIFullAvg10:    watchdogTripPSIFullAvg10,
		recoverPSIFullAvg10: watchdogRecoverPSIFullAvg10,
		lowMemAvailable:     watchdogLowMemAvailable,
		debounce:            watchdogDebounce,
		grace:               watchdogGrace,
		postKillSettle:      watchdogPostKillSettle,
		daemonPID:           os.Getpid(),
		daemonCgroup:        daemonGroup.path,
	}
}

func watchdogSleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func snapshotWatchdogProcs(mount string, mountErr error) (map[int]watchdogProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	procs := make(map[int]watchdogProc)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		comm, ppid, rss, ok := parseProcStatus(status)
		if !ok {
			continue
		}
		start, ok, _ := readProcStartTime(pid)
		if !ok {
			continue
		}
		cgroup, _, _ := watchdogCgroupForPID(pid, mount, mountErr)
		procs[pid] = watchdogProc{pid: pid, ppid: ppid, comm: comm, rss: rss, startTime: start, cgroup: cgroup}
	}
	return procs, nil
}

func parseProcStatus(data []byte) (comm string, ppid int, rss int64, ok bool) {
	haveName, havePPID, haveRSS := false, false, false
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch key {
		case "Name":
			comm, haveName = strings.TrimSpace(value), true
		case "PPid":
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil && parsed >= 0 {
				ppid, havePPID = parsed, true
			}
		case "VmRSS":
			fields := strings.Fields(value)
			if len(fields) == 2 && fields[1] == "kB" {
				parsed, err := strconv.ParseInt(fields[0], 10, 64)
				if err == nil && parsed >= 0 && parsed <= (1<<63-1)/1024 {
					rss, haveRSS = parsed*1024, true
				}
			}
		}
	}
	return comm, ppid, rss, haveName && havePPID && haveRSS
}

func readProcStartTime(pid int) (uint64, bool, string) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, "exited"
	}
	if err != nil {
		return 0, false, "read-error"
	}
	value, ok := parseProcStartTime(data)
	if !ok {
		return 0, false, "parse-error"
	}
	return value, true, ""
}

func parseProcStartTime(data []byte) (uint64, bool) {
	line := strings.TrimSpace(string(data))
	close := strings.LastIndexByte(line, ')')
	if close < 0 || close+1 >= len(line) {
		return 0, false
	}
	// Fields after comm begin at field 3 (state); starttime is field 22,
	// therefore index 19 in this suffix.
	fields := strings.Fields(line[close+1:])
	if len(fields) <= 19 {
		return 0, false
	}
	value, err := strconv.ParseUint(fields[19], 10, 64)
	return value, err == nil
}

func watchdogUnifiedMount() (string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		pre, post := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(pre) < 5 || len(post) < 1 || post[0] != "cgroup2" {
			continue
		}
		mount := pre[4]
		for _, replacement := range []struct{ from, to string }{{"\\040", " "}, {"\\011", "\t"}, {"\\134", "\\"}} {
			mount = strings.ReplaceAll(mount, replacement.from, replacement.to)
		}
		return mount, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("cgroup-v2 unified mount not found")
}

func watchdogCgroupForPID(pid int, mount string, mountErr error) (watchdogCgroup, bool, string) {
	if mountErr != nil {
		return watchdogCgroup{}, false, "cgroup-mount-unavailable"
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if errors.Is(err, os.ErrNotExist) {
		return watchdogCgroup{}, false, "exited"
	}
	if err != nil {
		return watchdogCgroup{}, false, "read-error"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		if rel == "" || !strings.HasPrefix(rel, "/") {
			return watchdogCgroup{}, false, "parse-error"
		}
		path := filepath.Join(mount, strings.TrimPrefix(rel, "/"))
		_, finite, evaluated := effectiveWatchdogCapEvaluated(mount, path)
		if !evaluated {
			return watchdogCgroup{path: rel}, false, "memory-max-unevaluated"
		}
		return watchdogCgroup{path: rel, uncapped: !finite && !hasAIRAComponent(rel)}, true, ""
	}
	return watchdogCgroup{}, false, "unified-cgroup-absent"
}

func effectiveWatchdogCapFrom(mount, path string) (int64, bool) {
	cap, finite, _ := effectiveWatchdogCapEvaluated(mount, path)
	return cap, finite
}

func effectiveWatchdogCapEvaluated(mount, path string) (int64, bool, bool) {
	current, root := filepath.Clean(path), filepath.Clean(mount)
	if current != root && !strings.HasPrefix(current, root+string(filepath.Separator)) {
		return 0, false, false
	}
	var best int64
	found := false
	for {
		data, err := os.ReadFile(filepath.Join(current, "memory.max"))
		if err != nil {
			if !os.IsNotExist(err) {
				// A genuine read error (permission/IO) — cannot establish the
				// cap. For a KILLER, fail conservative: unevaluated, so the
				// process is never classified uncapped and never killed.
				return 0, false, false
			}
			// memory.max ABSENT at this level: the cgroup2 mount root NEVER has
			// a memory.max (kernel invariant), and a controllerless cgroup
			// likewise — "no cap here", so CONTINUE the ancestry walk. Aborting
			// on this ENOENT made the classifier inert on every real host
			// (build-review P1); confine's readConfineCap tolerates it too.
		} else if value := strings.TrimSpace(string(data)); value != "max" {
			parsed, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil || parsed <= 0 {
				return 0, false, false
			}
			if !found || parsed < best {
				best, found = parsed, true
			}
		}
		if current == root {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return 0, false, false
		}
		current = parent
	}
	return best, found, true
}

func realWatchdogInterlock() (func(), bool, string) {
	runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if runtimeDir == "" {
		return nil, false, "XDG_RUNTIME_DIR unset"
	}
	return machineInterlock(runtimeDir, systemctlWatchdogStatus)
}

func systemctlWatchdogStatus(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "whale-watchdog").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return string(output), err
		}
		// is-active uses non-zero status for inactive/failed. Preserve only an
		// exact, positive state answer; empty bus failures remain errors.
		if _, ok := exactInactiveState(string(output)); !ok {
			return string(output), err
		}
	}
	return string(output), nil
}

func machineInterlock(runtimeDir string, status func(context.Context) (string, error)) (func(), bool, string) {
	if strings.TrimSpace(runtimeDir) == "" {
		return nil, false, "runtime directory unset"
	}
	lockPath := filepath.Join(runtimeDir, "aira-memory-watchdog.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, "lock open: " + err.Error()
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, "watchdog authority lock held"
		}
		return nil, false, "lock acquire: " + err.Error()
	}
	release := func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := status(ctx)
	if err != nil {
		release()
		return nil, false, "cannot confirm whale-watchdog inactive: " + err.Error()
	}
	_, inactive := exactInactiveState(output)
	if !inactive {
		release()
		return nil, false, fmt.Sprintf("whale-watchdog state %q", strings.TrimSpace(output))
	}
	return release, true, ""
}

func exactInactiveState(output string) (string, bool) {
	switch output {
	case "inactive", "inactive\n":
		return "inactive", true
	case "failed", "failed\n":
		return "failed", true
	default:
		return "", false
	}
}

func (s *Server) emitWatchdogEvent(ctx context.Context, event watchdogEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	byProject := make(map[string]*store.Store)
	for _, entry := range s.scopes {
		select {
		case <-entry.ready:
			byProject[entry.view.ProjectID()] = entry.view
		default:
		}
	}
	s.mu.Unlock()
	if len(byProject) == 0 {
		log.Printf("aira daemon: watchdog audit unrouted: no ready scope: %s", payload)
		return nil
	}
	var errs []error
	for projectID, view := range byProject {
		if err := view.AppendWatchdogEvent(ctx, "watchdog."+event.Decision, string(payload)); err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", projectID, err))
		}
	}
	return errors.Join(errs...)
}
