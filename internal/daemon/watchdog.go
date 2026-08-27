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

	"golang.org/x/sys/unix"
)

const (
	watchdogLowMemAvailable     = int64(8 << 30)
	watchdogRecoverMemAvailable = int64(16 << 30)
	watchdogMinVictimRSS        = int64(2 << 30)
	watchdogDebounce            = 3
	watchdogGrace               = 5 * time.Second
	watchdogPostKillSettle      = time.Second
	watchdogEmitTimeout         = 250 * time.Millisecond
	// Approximately ceil(postKillSettle/interval). This is 1 because the
	// one-second settle is no longer than the minimum legal watchdog interval.
	watchdogReArmCooldown = max(1, int((watchdogPostKillSettle+defaultWatchdogInterval-1)/defaultWatchdogInterval))
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
	PSIAvg10     *float64           `json:"psi_full_avg10,omitempty"`
	PSITotal     *uint64            `json:"psi_full_total_us,omitempty"`
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
	logf                func(format string, args ...any)
	sleep               func(context.Context, time.Duration) bool
	now                 func() time.Time
	minVictimRSS        int64
	lowMemAvailable     int64
	recoverMemAvailable int64
	debounce            int
	grace               time.Duration
	postKillSettle      time.Duration
	daemonPID           int
	daemonCgroup        string
}

type watchdogState struct {
	armCount    int  // debounce counter for critical-run entry only
	latched     bool // acted and not yet fully recovered; gates recovered emission only
	criticalRun bool // already acted during the current continuous below-low run
	cooldown    int  // settle polls remaining before the next action in a critical run
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
	if !validWatchdogDeps(deps) {
		log.Printf("aira daemon: watchdog disabled: invalid dependencies (low_mem=%d recover_mem=%d debounce=%d); watchdog will not run", deps.lowMemAvailable, deps.recoverMemAvailable, deps.debounce)
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

func validWatchdogDeps(deps watchdogDeps) bool {
	return deps.lowMemAvailable > 0 &&
		deps.recoverMemAvailable > deps.lowMemAvailable &&
		deps.debounce >= 1 &&
		deps.readMemAvailable != nil &&
		deps.readPressure != nil &&
		deps.snapshotProcs != nil &&
		deps.emitEvent != nil &&
		deps.pidfdOpen != nil &&
		deps.pidfdSignal != nil &&
		deps.closeFD != nil &&
		deps.startTime != nil &&
		deps.cgroupOf != nil &&
		deps.interlockOK != nil &&
		deps.now != nil &&
		deps.sleep != nil
}

func (s *Server) runWatchdog(ctx context.Context, mode watchdogMode, interval time.Duration, deps watchdogDeps) {
	runWatchdog(ctx, mode, interval, deps)
}

func evaluateWatchdog(ctx context.Context, mode watchdogMode, state *watchdogState, deps watchdogDeps) {
	available, memOK, memReason := deps.readMemAvailable()
	base := watchdogEvent{At: deps.now(), Mode: mode, MemAvailable: available}
	readPSI := func() pressureSample {
		avg, total, ok, reason := deps.readPressure()
		if !ok && reason == "" {
			reason = "unavailable"
		}
		return pressureSample{avg10: avg, total: total, ok: ok, reason: reason}
	}
	act := func(entry bool) {
		if entry {
			// HOLD or an unreadable memory sample can start another debounced entry
			// while an earlier earned latch is still waiting for full recovery. Those
			// entries each trip, but the latched period closes with one recovered event.
			base.Decision = "trip"
			emitWatchdog(ctx, deps, base)
		}
		// A just-signalled offender can remain in /proc until it is reaped and be
		// selected again. That is benign: per-target revalidation, ESRCH handling,
		// and the cooldown make the retry safe and paced.
		procs, err := deps.snapshotProcs()
		if err != nil {
			state.armCount = 0
			base.Decision, base.Reason = "unevaluated", "process-snapshot:"+err.Error()
			emitWatchdog(ctx, deps, eventWithPSI(base, readPSI()))
			return
		}
		psi := pressureSample{}
		if mode == watchdogObserve {
			psi = readPSI()
		}
		acted := handleArmed(ctx, mode, deps, psi, available, procs)
		if acted {
			state.latched = true
			state.criticalRun = true
			state.cooldown = watchdogReArmCooldown
		} else {
			state.criticalRun = false
			state.cooldown = 0
		}
		state.armCount = 0
	}
	if !memOK {
		state.armCount = 0
		state.criticalRun = false
		state.cooldown = 0
		base.Decision, base.Reason = "unevaluated", "memavailable:"+memReason
		emitWatchdog(ctx, deps, eventWithPSI(base, readPSI()))
		return
	}
	if available >= deps.recoverMemAvailable {
		wasLatched := state.latched
		state.armCount = 0
		state.latched = false
		state.criticalRun = false
		state.cooldown = 0
		if wasLatched {
			base.Decision = "recovered"
			emitWatchdog(ctx, deps, eventWithPSI(base, readPSI()))
		}
		return
	}
	if available >= deps.lowMemAvailable {
		state.armCount = 0
		state.criticalRun = false
		state.cooldown = 0
		return
	}
	if state.criticalRun {
		if state.cooldown > 0 {
			state.cooldown--
			return
		}
		act(false)
		return
	}
	state.armCount++
	if state.armCount < deps.debounce {
		return
	}
	act(true)
}

func eventWithPSI(event watchdogEvent, psi pressureSample) watchdogEvent {
	if !psi.ok {
		return event
	}
	avg, total := psi.avg10, psi.total
	event.PSIAvg10, event.PSITotal = &avg, &total
	return event
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

func handleArmed(ctx context.Context, mode watchdogMode, deps watchdogDeps, psi pressureSample, available int64, procs map[int]watchdogProc) bool {
	psiSampled := psi.ok || psi.reason != ""
	samplePSI := func() pressureSample {
		if !psiSampled {
			psi.avg10, psi.total, psi.ok, psi.reason = deps.readPressure()
			psiSampled = true
		}
		return psi
	}
	offender, verdicts := selectOffender(procs, deps.minVictimRSS, deps.daemonPID, deps.daemonCgroup)
	if offender == nil {
		event := watchdogEvent{At: deps.now(), Mode: mode, Decision: "defer", Reason: "pressure elsewhere; deferring to oomd/kernel", MemAvailable: available}
		emitWatchdog(ctx, deps, eventWithPSI(event, samplePSI()))
		return false
	}
	base := watchdogEvent{At: deps.now(), Mode: mode, MemAvailable: available, PID: offender.pid, Comm: offender.comm, RSS: offender.rss, StartTime: offender.startTime, Predicates: verdicts[offender.pid]}
	if mode == watchdogObserve {
		base.Decision, base.Outcome = "would_signal", "would_signal: SIGTERM; SIGKILL after grace if still alive and MemAvailable remains < 8 GiB"
		emitWatchdog(ctx, deps, eventWithPSI(base, samplePSI()))
		return true
	}
	release, allowed, reason := deps.interlockOK()
	if !allowed {
		base.Decision, base.Reason, base.Outcome = "would_signal", "interlock: "+reason, "degraded_to_observe; would_signal: SIGTERM; SIGKILL after grace if still alive and MemAvailable remains < 8 GiB"
		emitWatchdog(ctx, deps, eventWithPSI(base, samplePSI()))
		return true
	}
	if release == nil {
		base.Decision, base.Reason, base.Outcome = "would_signal", "interlock: missing authority release", "degraded_to_observe; would_signal: SIGTERM; SIGKILL after grace if still alive and MemAvailable remains < 8 GiB"
		emitWatchdog(ctx, deps, eventWithPSI(base, samplePSI()))
		return true
	}
	defer release()
	// offenderSubtree establishes ancestry only. Before every signal below,
	// revalidateWatchdogTarget freshly re-checks the safety predicates (cgroup,
	// AIRA/protected status, and process identity) for each target. The remaining
	// revalidate-to-signal gap is unavoidable and deliberately kept minimal.
	targets := offenderSubtree(procs, offender.pid)
	type openedTarget struct {
		proc watchdogProc
		fd   int
	}
	opened := make([]openedTarget, 0, len(targets))
	terminalTargets := 0
	delivered := false
	unsupported := false
	defer func() {
		for _, target := range opened {
			_ = deps.closeFD(target.fd)
		}
	}()
	for _, proc := range targets {
		fd, err := deps.pidfdOpen(proc.pid)
		if errors.Is(err, unix.ENOSYS) {
			base.Decision, base.Reason, base.Outcome = "degraded", "pidfd_open unsupported", "degraded_no_signal"
			emitWatchdog(ctx, deps, eventWithPSI(base, samplePSI()))
			return true
		}
		if errors.Is(err, unix.ESRCH) {
			terminalTargets++
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
		return terminalTargets == len(targets)
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
			delivered = true
			outcome.Outcome = "signal_sent,delivered"
			survivors = append(survivors, target)
		case errors.Is(err, unix.ESRCH):
			terminalTargets++
			outcome.Outcome = "already_exited"
		case errors.Is(err, unix.ENOSYS):
			outcome.Outcome, outcome.Reason = "degraded_no_signal", "pidfd_send_signal unsupported"
			emitWatchdog(ctx, deps, eventWithPSI(outcome, samplePSI()))
			return true
		default:
			outcome.Outcome, outcome.Reason = "signal_failed", err.Error()
			survivors = append(survivors, target)
		}
		emitWatchdog(ctx, deps, eventWithPSI(outcome, samplePSI()))
	}
	if len(survivors) == 0 || !deps.sleep(ctx, deps.grace) {
		return delivered || terminalTargets == len(targets)
	}
	if !pressureStillTripped(deps) {
		for _, target := range survivors {
			outcome := eventForTarget(base, target.proc)
			outcome.Decision, outcome.Outcome = "outcome", targetStateAfterGrace(target.proc, deps)
			emitWatchdog(ctx, deps, eventWithPSI(outcome, samplePSI()))
		}
		return delivered || terminalTargets == len(targets)
	}
	escalated := make([]openedTarget, 0, len(survivors))
	for _, target := range survivors {
		if valid, reason := revalidateWatchdogTarget(target.proc, deps); !valid {
			outcome := eventForTarget(base, target.proc)
			outcome.Decision, outcome.Outcome, outcome.Reason = "outcome", targetStateAfterGrace(target.proc, deps), reason
			emitWatchdog(ctx, deps, eventWithPSI(outcome, samplePSI()))
			continue
		}
		intent := eventForTarget(base, target.proc)
		intent.Decision, intent.Outcome = "intent", "about_to_sigkill"
		emitWatchdog(ctx, deps, intent)
		err := deps.pidfdSignal(target.fd, unix.SIGKILL)
		outcome := eventForTarget(base, target.proc)
		outcome.Decision = "outcome"
		switch {
		case err == nil:
			delivered = true
			outcome.Outcome = "signal_sent,delivered,escalated_sigkill"
			escalated = append(escalated, target)
		case errors.Is(err, unix.ESRCH):
			terminalTargets++
			outcome.Outcome = "already_exited"
		case errors.Is(err, unix.ENOSYS):
			unsupported = true
			outcome.Outcome, outcome.Reason = "degraded_no_signal", "pidfd_send_signal unsupported"
		default:
			outcome.Outcome, outcome.Reason = "signal_failed", err.Error()
		}
		emitWatchdog(ctx, deps, eventWithPSI(outcome, samplePSI()))
	}
	if len(escalated) > 0 && deps.sleep(ctx, deps.postKillSettle) {
		for _, target := range escalated {
			final := eventForTarget(base, target.proc)
			final.Decision, final.Outcome = "outcome", targetStateAfterGrace(target.proc, deps)
			emitWatchdog(ctx, deps, eventWithPSI(final, samplePSI()))
		}
	}
	return delivered || terminalTargets == len(targets) || unsupported
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

func pressureStillTripped(deps watchdogDeps) bool {
	available, ok, _ := deps.readMemAvailable()
	return ok && available < deps.lowMemAvailable
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
	if deps.logf != nil {
		detail := event.Outcome
		if detail == "" {
			detail = event.Reason
		}
		if detail == "" {
			detail = "-"
		}
		psi := "?"
		if event.PSIAvg10 != nil {
			psi = strconv.FormatFloat(*event.PSIAvg10, 'f', -1, 64)
		}
		// An unestablished MemAvailable (mem-read failure → available==0) must not render
		// as a concrete "0.00GiB": that is a fabricated zero on the operator surface. Show
		// "?" — consistent with the JSON audit, whose mem_available_bytes is omitempty (a
		// live /proc read never returns exactly 0 before the box is dead).
		mem := "?"
		if event.MemAvailable != 0 {
			mem = fmt.Sprintf("%.2fGiB", float64(event.MemAvailable)/(1<<30))
		}
		victim := ""
		if event.PID != 0 {
			victim = fmt.Sprintf(" victim pid=%d comm=%s rss=%d", event.PID, event.Comm, event.RSS)
		}
		deps.logf("aira daemon: watchdog %s: %s mem_avail=%s psi_avg10=%s%s", event.Decision, detail, mem, psi, victim)
	}
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
		logf:                log.Printf,
		sleep:               watchdogSleep,
		now:                 time.Now,
		minVictimRSS:        watchdogMinVictimRSS,
		lowMemAvailable:     watchdogLowMemAvailable,
		recoverMemAvailable: watchdogRecoverMemAvailable,
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
	byProject := s.readyProjectViewsForUse()
	if len(byProject) == 0 {
		log.Printf("aira daemon: watchdog audit unrouted: no ready scope: %s", payload)
		return nil
	}
	var errs []error
	for projectID, view := range byProject {
		func() {
			defer s.endProjectUse(projectID)
			if err := view.AppendWatchdogEvent(ctx, "watchdog."+event.Decision, string(payload)); err != nil {
				errs = append(errs, fmt.Errorf("project %s: %w", projectID, err))
			}
		}()
	}
	return errors.Join(errs...)
}
