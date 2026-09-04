//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/bits"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"aira/internal/pylib"
	"golang.org/x/sys/unix"
)

const (
	confineHandshakeSchema     = 1
	confineHandshakeMaxSize    = 4096
	confineSetupFD             = 3
	confineReleaseFD           = 4
	confineOOMScoreAdj         = 500
	confineDelegateOOMScoreAdj = 800
	confineNice                = 19
	confineIOPriorityClass     = 2 // best-effort / IOPRIO_CLASS_BE
	confineIOPriorityData      = 7 // Best-effort priority: 0 is highest, 7 is lowest.
	defaultCPUWeightStart      = int64(100)
	defaultCPUWeightFloor      = int64(10)
)

type confineDelegation struct {
	cpuWeight bool
}

// confineControllerOps keeps the cgroup controller files behind the smallest
// useful seam: delegation policy remains in ensureConfineDelegation, while its
// file protocol can be exercised without a privileged cgroup fixture.
type confineControllerOps struct {
	readFile  func(string) ([]byte, error)
	writeFile func(string, []byte) error
}

// cpuWeightStep is elapsed time from scope creation, rather than a delay from
// the preceding step. The default leaves a just-created scope at 100 and then
// decays it over the long-running contention window observed in AIRA-14.
type cpuWeightStep struct {
	After  time.Duration
	Weight int64
}

type cpuWeightConfig struct {
	Start int64
	Floor int64
	Steps []cpuWeightStep
}

var defaultCPUWeightSteps = []cpuWeightStep{
	{After: 10 * time.Second, Weight: 100},
	{After: 30 * time.Second, Weight: 70},
	{After: time.Minute, Weight: 50},
	{After: 5 * time.Minute, Weight: 30},
	{After: 10 * time.Minute, Weight: 20},
	{After: 30 * time.Minute, Weight: 10},
}

type confineHandshake struct {
	Schema      int  `json:"schema"`
	OOMScoreAdj bool `json:"oom_score_adj"`
	Nice        bool `json:"nice"`
	IONice      bool `json:"ionice"`
}

func (result confineHandshake) applied() bool {
	return result.Schema == confineHandshakeSchema && result.OOMScoreAdj && result.Nice && result.IONice
}

func parseConfineHandshake(payload []byte) (confineHandshake, bool) {
	var result confineHandshake
	if len(payload) == 0 || len(payload) > confineHandshakeMaxSize || payload[len(payload)-1] != '\n' || bytes.Count(payload, []byte{'\n'}) != 1 {
		return result, false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload[:len(payload)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || !result.applied() {
		return result, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return confineHandshake{}, false
	}
	return result, true
}

type confineCommand struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	process *os.Process
}

func (command *confineCommand) Start() error {
	command.mu.Lock()
	defer command.mu.Unlock()
	err := command.cmd.Start()
	command.process = command.cmd.Process
	return err
}

func (command *confineCommand) Process() *os.Process {
	command.mu.Lock()
	defer command.mu.Unlock()
	return command.process
}

type confineLockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (writer *confineLockedWriter) Write(payload []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.w.Write(payload)
}

type confineDeps struct {
	resolveSlicePath      func(string) (string, bool, string)
	resolveSlicePathExact func(string) (string, error)
	managedUnitPresent    func(string) (bool, error)
	ensureDelegation      func(string) (confineDelegation, error)
	newBackend            func(string) ScopeBackend
	admit                 func(context.Context, string, ConfineRequest, int64) (admissionResult, error)
	writeOOMGroup         func(Scope) error
	writeScopeMemoryCap   func(Scope, int64, int64, bool) error
	writeScopeCPUWeight   func(Scope, int64) bool
	start                 func(*confineCommand) error
	readHandshake         func(*os.File, time.Duration) ([]byte, error)
	readCap               func(string) (int64, bool)
	signalSource          func() (<-chan os.Signal, func())
	readUsage             func(string) cgroupUsage
	reportPeak            func(context.Context, ConfineRequest, string, *int64, bool) error
	admitWaitDiagInterval time.Duration
}

// defaultAdmitWaitDiagInterval is how often a blocked admission prints a
// "waiting for memory admission" progress line. Admission can legitimately wait
// (a reserve-contended slice queues a job behind other sessions' in-flight jobs
// under the shared cap), and the client blocks on a single socket read for up to
// its maxWait; without a periodic line a bounded wait is indistinguishable from a
// hang. Tests override the interval.
const defaultAdmitWaitDiagInterval = 15 * time.Second

func defaultConfineDeps() confineDeps {
	return confineDeps{
		resolveSlicePath:      resolveSlicePath,
		resolveSlicePathExact: resolveSlicePathExact,
		managedUnitPresent:    managedConfineUnitPresent,
		ensureDelegation:      ensureConfineDelegation,
		newBackend:            newDefaultBackend,
		admit:                 admitConfine,
		writeOOMGroup:         writeConfineOOMGroup,
		writeScopeMemoryCap:   writeScopeMemoryCap,
		writeScopeCPUWeight:   writeScopeCPUWeightFailOpen,
		start:                 func(command *confineCommand) error { return command.Start() },
		readHandshake:         readConfineHandshake,
		readCap:               effectiveConfineCap,
		signalSource:          confineSignalSource,
		readUsage:             readCgroupUsage,
		reportPeak:            reportConfinePeak,
	}
}

func fillConfineDeps(deps confineDeps) confineDeps {
	defaults := defaultConfineDeps()
	if deps.resolveSlicePath == nil {
		deps.resolveSlicePath = defaults.resolveSlicePath
	}
	if deps.resolveSlicePathExact == nil {
		deps.resolveSlicePathExact = defaults.resolveSlicePathExact
	}
	if deps.managedUnitPresent == nil {
		deps.managedUnitPresent = defaults.managedUnitPresent
	}
	if deps.ensureDelegation == nil {
		deps.ensureDelegation = defaults.ensureDelegation
	}
	if deps.newBackend == nil {
		deps.newBackend = defaults.newBackend
	}
	if deps.admit == nil {
		deps.admit = defaults.admit
	}
	if deps.writeOOMGroup == nil {
		deps.writeOOMGroup = defaults.writeOOMGroup
	}
	if deps.writeScopeMemoryCap == nil {
		deps.writeScopeMemoryCap = defaults.writeScopeMemoryCap
	}
	if deps.writeScopeCPUWeight == nil {
		deps.writeScopeCPUWeight = defaults.writeScopeCPUWeight
	}
	if deps.start == nil {
		deps.start = defaults.start
	}
	if deps.readHandshake == nil {
		deps.readHandshake = defaults.readHandshake
	}
	if deps.readCap == nil {
		deps.readCap = defaults.readCap
	}
	if deps.signalSource == nil {
		deps.signalSource = defaults.signalSource
	}
	if deps.readUsage == nil {
		deps.readUsage = defaults.readUsage
	}
	if deps.reportPeak == nil {
		deps.reportPeak = defaults.reportPeak
	}
	return deps
}

func confine(ctx context.Context, request ConfineRequest) (ConfineResult, error) {
	return confineWithDeps(ctx, request, defaultConfineDeps())
}

func resolveDefaultConfineSlice(deps confineDeps) (string, string, error) {
	managed, unitErr := deps.managedUnitPresent(DefaultConfineSlice)
	if unitErr != nil {
		return DefaultConfineSlice, "", fmt.Errorf("cannot evaluate aira.slice unit: %w", unitErr)
	}
	airaPath, airaErr := deps.resolveSlicePathExact(DefaultConfineSlice)
	if airaErr == nil {
		if !managed {
			return DefaultConfineSlice, "", errors.New("aira.slice cgroup is active but its aira-managed unit is absent; re-run aira install")
		}
		return DefaultConfineSlice, airaPath, nil
	}
	if managed {
		return DefaultConfineSlice, "", errors.New("aira.slice installed but not active — anchor dead? re-run aira install")
	}
	if !errors.Is(airaErr, fs.ErrNotExist) {
		return DefaultConfineSlice, "", fmt.Errorf("cannot evaluate aira.slice: %w", airaErr)
	}
	whalePath, whaleErr := deps.resolveSlicePathExact("whale.slice")
	if whaleErr == nil {
		return "whale.slice", whalePath, nil
	}
	if !errors.Is(whaleErr, fs.ErrNotExist) {
		return DefaultConfineSlice, "", fmt.Errorf("aira.slice is absent and whale.slice cannot be evaluated: %w", whaleErr)
	}
	return DefaultConfineSlice, "", errors.New("aira.slice not found (run 'aira install')")
}

func managedConfineUnitPresent(unit string) (bool, error) {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return false, err
		}
	}
	path := filepath.Join(home, ".config", "systemd", "user", unit)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular unit file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return false, fmt.Errorf("%s is not owned by the invoking user", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	first := content
	if index := bytes.IndexByte(first, '\n'); index >= 0 {
		first = first[:index]
	}
	return string(first) == "# aira-managed: "+unit, nil
}

func ensureConfineDelegation(parent string) (confineDelegation, error) {
	return ensureConfineDelegationWithOps(parent, confineControllerOps{
		readFile:  os.ReadFile,
		writeFile: writeConfineSubtreeControl,
	})
}

func ensureConfineDelegationWithOps(parent string, ops confineControllerOps) (confineDelegation, error) {
	controllers, err := ops.readFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return confineDelegation{}, fmt.Errorf("read cgroup.controllers: %w", err)
	}
	if !confineHasToken(controllers, "memory") {
		return confineDelegation{}, errors.New("memory is absent from cgroup.controllers")
	}
	if err := enableConfineControllerWithOps(parent, "memory", ops); err != nil {
		return confineDelegation{}, err
	}
	// CPU aging is intentionally best effort. Some delegated parents do not
	// expose cpu, and systemd can reset subtree_control between launches; either
	// case means only that aging is unavailable, never that memory confinement
	// should refuse an otherwise safe job.
	delegation := confineDelegation{}
	if confineHasToken(controllers, "cpu") {
		delegation.cpuWeight = enableConfineControllerWithOps(parent, "cpu", ops) == nil
	}
	return delegation, nil
}

func writeConfineSubtreeControl(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
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

func enableConfineControllerWithOps(parent, controller string, ops confineControllerOps) error {
	subtree := filepath.Join(parent, "cgroup.subtree_control")
	current, err := ops.readFile(subtree)
	if err != nil {
		return fmt.Errorf("read cgroup.subtree_control: %w", err)
	}
	if !confineHasToken(current, controller) {
		if err := ops.writeFile(subtree, []byte("+"+controller+"\n")); err != nil {
			return fmt.Errorf("write +%s to cgroup.subtree_control: %w", controller, err)
		}
	}
	verified, err := ops.readFile(subtree)
	if err != nil {
		return fmt.Errorf("verify cgroup.subtree_control: %w", err)
	}
	if !confineHasToken(verified, controller) {
		return fmt.Errorf("%s missing from cgroup.subtree_control after enable", controller)
	}
	return nil
}

func confineHasToken(data []byte, wanted string) bool {
	for _, token := range strings.Fields(string(data)) {
		if token == wanted {
			return true
		}
	}
	return false
}

func confineWithDeps(ctx context.Context, request ConfineRequest, deps confineDeps) (ConfineResult, error) {
	deps = fillConfineDeps(deps)
	explicitSlice := ResolveConfineSlice(request.Slice)
	attemptedSlice := explicitSlice
	if attemptedSlice == "" {
		attemptedSlice = DefaultConfineSlice
	}
	result := ConfineResult{Status: ConfineStatus{
		Cap: ConfineCapUnevaluated, Admission: ConfineAdmissionUnevaluated,
		Scope: ConfineScopeUnverified, OOMGroup: ConfineOOMGroupUnverified,
		Priorities: ConfinePrioritiesUnverified, CPUWeight: ConfineCPUWeightUnavailable,
	}}
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		result.Status.Slice = attemptedSlice
		return result, errors.New("E_CONFINE_ARGUMENT_INVALID: target argv is empty")
	}
	if err := validateScopeMemoryCap(request.ScopeMemoryMax, request.ScopeMemoryHigh); err != nil {
		result.Status.Slice = attemptedSlice
		return result, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %w", err)
	}
	// A DECLARED reserve below the minimum the CLI itself accepts is refused here
	// rather than silently launched uncapped. Both are enforced on this value —
	// the daemon-grant path makes a declared reserve the scope memory.max — so
	// quietly dropping the cap for a sub-minimum value would recreate the very
	// "same request, different containment" divergence this change closes, and
	// would be another silent substitution of the kind AIRA-58 exists to remove.
	// Symmetric with the CLI (main.go rejects below the same bound) and with the
	// daemon's own behaviour.
	if request.MemoryReservePinned && request.MemoryReserve > 0 && request.MemoryReserve < MinPinnedScopeCap {
		result.Status.Slice = attemptedSlice
		return result, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --memory-reserve: declared reserve %d is below the %d-byte minimum", request.MemoryReserve, MinPinnedScopeCap)
	}
	if err := validateConfineName(request.Name); err != nil {
		result.Status.Slice = attemptedSlice
		return result, err
	}
	if request.Owner == "" {
		request.Owner = ConfineUnknownOwner
	}
	if err := ValidateConfineIdentity(request.Owner); err != nil {
		result.Status.Slice = attemptedSlice
		return result, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --owner: %w", err)
	}
	sliceName, path := explicitSlice, ""
	if explicitSlice == "" {
		var resolveErr error
		sliceName, path, resolveErr = resolveDefaultConfineSlice(deps)
		result.Status.Slice = sliceName
		if resolveErr != nil {
			return result, confineUnavailable(sliceName, resolveErr)
		}
	} else {
		var ok bool
		var reason string
		path, ok, reason = deps.resolveSlicePath(sliceName)
		result.Status.Slice = sliceName
		if !ok {
			if reason == "" {
				reason = "slice-not-found"
			}
			return result, confineUnavailable(sliceName, errors.New(reason))
		}
	}
	backend := deps.newBackend(path)
	if err := backend.Probe(ctx); err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("probe: %w", err))
	}

	// A finite effective cap is a precondition, not a facet: without one the only
	// backstop is a global (host) OOM, which is exactly what confinement must
	// prevent. Refuse before admitting or launching so an uncapped slice never runs
	// a heavy job that could starve the machine (the child self-check in
	// RunConfineSetup is the defence-in-depth mirror of this gate).
	maximum, finite := deps.readCap(path)
	if !finite {
		return result, confineUnavailable(sliceName, fmt.Errorf(
			"slice %s has no finite memory.max in its cgroup ancestry (uncapped); refusing to launch", sliceName))
	}
	result.Status.Cap = ConfineCapEnforced
	result.Status.CapBytes = maximum
	delegation, err := deps.ensureDelegation(path)
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("ensure memory delegation: %w", err))
	}

	// declaredReserve records PROVENANCE and must be read before ResolveConfineReserve
	// widens anything: `pinned` becomes true for ANY positive reserve, so
	// afterwards it can no longer distinguish "the caller declared this number" from
	// "the caller passed some number". Only a declared value may become a hard
	// scope memory.max on the non-daemon paths — a token reserve enforced as a cap
	// contains nothing and merely OOM-kills the job at launch (three real-cgroup
	// tests passing MemoryReserve: 1 caught exactly that during this build).
	// Both facts are read from the ORIGINAL request, before any resolution below:
	//   - provenance: MemoryReservePinned is about to be widened to true for ANY
	//     positive reserve, after which it can no longer distinguish "the caller
	//     declared this" from "the caller passed something". Callers that pass a
	//     token reserve without declaring it (several tests pass 1) must never
	//     have it enforced as a cap — that contains nothing and kills the job.
	//   - validity: a caller that pins WITHOUT a usable number has its reserve
	//     replaced by the 4GiB default inside ResolveConfineReserve, and capping at
	//     that default would be capping at a guess while calling it declared —
	//     exactly what the unpinned branch deliberately refuses to do.
	// A declared-but-too-small reserve was already refused above, so a positive
	// declared value here is usable. MemoryReserve is overwritten a few lines below,
	// so the byte count is captured now rather than re-read later.
	declaredReserve := request.MemoryReservePinned && request.MemoryReserve > 0
	declaredReserveBytes := request.MemoryReserve
	// AIRA-62: the ledger charge is decided HERE and only here — never upstream in a
	// face. Both request fields are overwritten with the result a few lines below,
	// and MemoryReservePinned is the one admitConfine puts on the admit wire, so
	// admission always sees the RESOLVED pair rather than what a caller typed.
	reserve, pinned := ResolveConfineReserve(request)
	signature := request.ResourceSignature
	if signature == "" {
		if computed, signatureErr := ResourceSignature(nil, nil, request.Argv); signatureErr == nil {
			signature = computed
		}
	}
	request.ResourceSignature = signature
	request.MemoryReserve = reserve
	request.MemoryReservePinned = pinned
	if request.Name == "" {
		request.Name = "job"
	}
	scopeID := confineScopeID(request.Name, request.DelegateRAM)
	request.ScopeID = scopeID
	// Admission can legitimately block: a reserve-contended slice queues this job
	// behind other sessions' in-flight jobs under the shared cap, and the client
	// waits on a single socket read for up to its maxWait. Emit a periodic progress
	// line so a bounded wait is never mistaken for a hang. Nothing prints when
	// admission returns promptly; the goroutine is stopped and joined first.
	// deferred: AIRA-24 visibility needs a separate daemon --list connection per
	// tick; do not multiplex this blocked admission socket.
	admitDiag := request.Stderr
	if admitDiag == nil {
		admitDiag = os.Stderr
	}
	diagInterval := deps.admitWaitDiagInterval
	if diagInterval <= 0 {
		diagInterval = defaultAdmitWaitDiagInterval
	}
	admitWaitDone := make(chan struct{})
	admitWaitStopped := make(chan struct{})
	// AIRA-51: an unpinned request's `reserve` here is only the client's
	// no-history fallback hint (DefaultConfineMemoryReserve, or the delegate-ram
	// overhead) sent to the daemon as a starting point — resolveAdmitReserve
	// (internal/daemon/admit.go) resolves the real, admission-gating reserve from
	// peak-RSS history or a machine-wide p90 prior before this job is even
	// queued, and that resolved figure is what the daemon actually grants. The
	// client learns it only on the single blocking admit response (deferred
	// AIRA-24: no mid-wait channel exists to poll it), so while unpinned this
	// progress line must not present the hint as the number the slice is
	// contending over; a pinned request (explicit --memory-reserve or an implied
	// pin) IS honored verbatim by the daemon, so that figure is accurate as-is.
	go func() {
		defer close(admitWaitStopped)
		start := time.Now()
		ticker := time.NewTicker(diagInterval)
		defer ticker.Stop()
		for {
			select {
			case <-admitWaitDone:
				return
			case <-ticker.C:
				waited := int64(time.Since(start).Seconds())
				if pinned {
					fmt.Fprintf(admitDiag, "confine: waiting for memory admission on %s (reserve %s, waited %ds)\n",
						sliceName, FormatConfineBytes(reserve), waited)
				} else {
					fmt.Fprintf(admitDiag, "confine: waiting for memory admission on %s (requested reserve %s, unpinned — the daemon resolves the actual grant, which may differ; waited %ds)\n",
						sliceName, FormatConfineBytes(reserve), waited)
				}
			}
		}
	}()
	admission, err := deps.admit(ctx, path, request, reserve)
	close(admitWaitDone)
	<-admitWaitStopped
	resolvedReserve := reserve
	if admission.reserve > 0 {
		resolvedReserve = admission.reserve
	}
	result.Status.ReserveBytes = resolvedReserve
	result.Status.ReserveBasis = admission.basis
	result.Status.AdmissionState = admission.state
	result.Status.AdmissionWaitedMS = admission.waitedMS
	if err != nil {
		return result, err
	}
	var releaseAdmissionOnce sync.Once
	releaseAdmission := func() { releaseAdmissionOnce.Do(admission.releaseAdmission) }
	defer releaseAdmission()
	switch admission.state {
	case "immediate", "waited":
		result.Status.Admission = ConfineAdmissionAdmitted
	case "timeout":
		result.Status.Admission = ConfineAdmissionTimeout
	default:
		result.Status.Admission = ConfineAdmissionUnevaluated
	}

	scope, err := backend.Create(ctx, scopeID)
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("create scope: %w", err))
	}
	var started atomic.Bool
	var interrupted atomic.Bool
	var cleanupOnce sync.Once
	cpuWeightStop := func() {}
	cleanup := func() {
		cleanupOnce.Do(func() {
			cpuWeightStop()
			cleanupConfineScope(scope, started.Load() || interrupted.Load())
		})
	}
	defer cleanup()
	if delegation.cpuWeight {
		config := confineCPUWeightConfig()
		if deps.writeScopeCPUWeight(scope, config.Start) {
			result.Status.CPUWeight = ConfineCPUWeightAging
			cpuWeightStop = startCPUWeightDecay(scope, config.Steps, deps.writeScopeCPUWeight)
		}
	}

	var commandMu sync.RWMutex
	var command *confineCommand
	process := func() *os.Process {
		commandMu.RLock()
		defer commandMu.RUnlock()
		if command == nil {
			return nil
		}
		return command.Process()
	}
	signalEvents, stopSignalSource := deps.signalSource()
	stopSignalHandler := forwardConfineSignals(signalEvents, process, func() {
		interrupted.Store(true)
		cleanup()
	})
	defer func() {
		stopSignalSource()
		stopSignalHandler()
	}()
	if interrupted.Load() {
		return result, confineUnavailable(sliceName, errors.New("interrupted before confined target start"))
	}
	if err := deps.writeOOMGroup(scope); err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("set memory.oom.group: %w", err))
	}
	result.Status.OOMGroup = ConfineOOMGroupSet
	scopeMemoryMax := request.ScopeMemoryMax
	admitted := admission.state == "immediate" || admission.state == "waited"
	// A PINNED reserve is the user's own declared number (--memory-reserve, or
	// --memory-max, which already set ScopeMemoryMax above) — not an estimate — so
	// it is enforced as the scope cap regardless of how admission resolved: daemon
	// grant, flock fallback, timeout, or unevaluated.
	//
	// This condition used to also require `admission.lock == nil`, which holds
	// ONLY for a daemon grant (admitWithFlock always returns a lock). So the very
	// same command produced a capped scope when the daemon answered and an
	// UNCAPPED one when it did not — daemon restart, transport failure, or the
	// client's own premature deadline. That divergence, not the fallback itself,
	// was the containment gap: an uncapped scope can consume the entire slice and
	// OOM well-behaved neighbours, bounded only by aira.slice's own cap.
	// Deliberately keyed on pinned-ness rather than on admission state, because
	// several launchable outcomes (fallback timeout/unevaluated, or a daemon
	// `unevaluated`) hold no lock yet still create the scope.
	// The MinPinnedScopeCap floor is load-bearing, not defensive padding: the
	// pinned flag is set for ANY positive reserve (see :447), so it does not by
	// itself prove a deliberate cap request, and a token reserve enforced as
	// memory.max contains nothing while killing the job at launch. The existing
	// runner suite caught exactly that — three real-cgroup tests pass
	// MemoryReserve: 1 to enable admission and were killed with "pid absent".
	// Keyed on declaredReserve — provenance AND validity captured from the
	// original request — never on request.MemoryReservePinned, which by here is
	// true for any positive reserve and cannot tell a deliberate cap from a token
	// value, and never on request.MemoryReserve, which by here may be a resolved
	// default rather than anything the caller said. A declared reserve too small
	// to be a real cap was refused up front, so there is no silently-uncapped
	// case left for this branch to hide.
	if !request.DelegateRAM && scopeMemoryMax <= 0 && declaredReserve {
		scopeMemoryMax = declaredReserveBytes
	}
	// An UNPINNED reserve stays restricted to an ADMITTED (accounted) daemon
	// grant: only that carries a history-derived estimate the daemon has itself
	// validated against Σ(reserve) ≤ cap-headroom. The client-side fallback
	// default is explicitly a guess (DefaultConfineMemoryReserve), and enforcing a
	// guess as a hard cap would OOM-kill jobs that succeed today — which is why
	// the unpinned fallback is deliberately left uncapped rather than
	// conservatively capped. Delegate-ram never takes either branch: its pinned
	// reserve is framework overhead, and it gets a finite cap below.
	if !request.DelegateRAM && scopeMemoryMax <= 0 && admitted && admission.lock == nil && admission.release != nil && admission.reserve > 0 {
		scopeMemoryMax = admission.reserve
	}
	// Delegate-ram: an explicit --memory-max (scopeMemoryMax > 0) is the user's
	// informed, still-finite-and-contained choice and WINS — it is never lowered by
	// the learned ceiling, which would false-kill a suite the user deliberately sized
	// larger (and which is exactly the interim --memory-max mitigation others rely on).
	// The ceiling only supplies a finite cap when there is no explicit one; a compiled-in
	// fallback backs it when the daemon provides none, so the scope is never uncapped.
	if request.DelegateRAM && scopeMemoryMax <= 0 {
		scopeMemoryMax = admission.scopeCeiling
		if scopeMemoryMax <= 0 {
			scopeMemoryMax = delegateRAMScopeFallback()
		}
	}
	if request.DelegateRAM && scopeMemoryMax <= 0 {
		return result, confineUnavailable(sliceName, errors.New("delegate-ram scope has no finite memory.max"))
	}
	if scopeMemoryMax > 0 {
		if err := deps.writeScopeMemoryCap(scope, scopeMemoryMax, request.ScopeMemoryHigh, false); err != nil {
			return result, confineUnavailable(sliceName, fmt.Errorf("set scope memory cap: %w", err))
		}
		result.Status.ScopeMemoryMax = floorMemoryPage(scopeMemoryMax)
		result.Status.ScopeMemoryHigh = floorMemoryPage(request.ScopeMemoryHigh)
		result.Status.ScopeMemoryBinding = "ancestor-limited"
		result.Status.ScopeMemoryEffective = maximum
		if result.Status.ScopeMemoryMax < maximum {
			result.Status.ScopeMemoryBinding = "scope-limited"
			result.Status.ScopeMemoryEffective = result.Status.ScopeMemoryMax
		}
	}
	if interrupted.Load() {
		return result, confineUnavailable(sliceName, errors.New("interrupted before confined target start"))
	}

	handshakeRead, handshakeWrite, err := os.Pipe()
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("create setup handshake: %w", err))
	}
	defer handshakeRead.Close()
	defer handshakeWrite.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("create setup release gate: %w", err))
	}
	defer releaseRead.Close()
	defer releaseWrite.Close()
	self := strings.TrimSpace(request.SelfPath)
	if self == "" {
		self = "/proc/self/exe"
	}
	setupArgv, err := confineSetupArgv(request.Argv, request.DelegateRAM)
	if err != nil {
		return result, err
	}
	diagnostics := request.Stderr
	if diagnostics == nil {
		diagnostics = os.Stderr
	}
	diagnostics = &confineLockedWriter{w: diagnostics}
	stdin, stdout := request.Stdin, request.Stdout
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	cmd := exec.CommandContext(ctx, self, setupArgv...)
	reserveCommand := ""
	memoryDefault := ""
	if request.DelegateRAM {
		reserveCommand = self
		if executable, executableErr := filepath.EvalSymlinks(self); executableErr == nil {
			reserveCommand = executable
		}
		memoryDefault = strings.TrimSpace(os.Getenv("AIRA_TEST_MEM_DEFAULT"))
		if parsed, parseErr := ParseMemorySize(memoryDefault); parseErr != nil || parsed <= 0 {
			memoryDefault = pylib.DefaultTestMemoryReserve
		}
	}
	cmd.Env = pylib.AppendConfineChildEnvironment(confineEnvironment(request.Env), request.RuntimeDir, diagnostics, request.DelegateRAM, reserveCommand, memoryDefault, scopeID, sliceName)
	if request.DelegateRAM {
		// aitest is only meaningful for a delegate-RAM launch (worker-admit
		// grants nested sub-scopes under THIS job's own outer scope); every
		// other launch gets no aitest coordinates at all, mirroring
		// AppendConfineChildEnvironment's own delegateRAM gate immediately
		// above. reserveCommand is the SAME resolved self binary already
		// computed for the RAM governor a few lines up — both worker-admit
		// and aitest-bootstrap are verbs on that one aira binary.
		cmd.Env = pylib.AppendAitestChildEnvironment(cmd.Env, request.RuntimeDir, diagnostics, reserveCommand)
	} else {
		// Strip unconditionally, not just skip appending (Fable build-review,
		// final gate): AppendAitestChildEnvironment was previously called
		// ONLY inside the branch above, so a non-delegate launch's cmd.Env
		// kept whatever AIRA_AITEST_* coordinates it inherited from ITS OWN
		// parent process untouched -- e.g. a shell or test inside a
		// delegate-RAM aitest job launching `aira confine -- ...` without
		// --delegate-ram would hand its child stale coordinates pointing at
		// the outer job's (possibly since-deleted) extraction dir and relay
		// binary. Mirrors StripGovernorEnvironment's own unconditional
		// strip on every confine launch (runner_linux.go).
		cmd.Env = pylib.StripAitestEnvironment(cmd.Env)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, diagnostics
	cmd.ExtraFiles = []*os.File{handshakeWrite, releaseRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}
	commandMu.Lock()
	command = &confineCommand{cmd: cmd}
	commandMu.Unlock()
	if interrupted.Load() {
		return result, confineUnavailable(sliceName, errors.New("interrupted before confined target start"))
	}
	if err := deps.start(command); err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("start in scope: %w", err))
	}
	started.Store(true)
	_ = handshakeWrite.Close()
	_ = releaseRead.Close()
	if admission.lock != nil {
		releaseAdmission()
	}

	abortStarted := func(cause error) (ConfineResult, error) {
		_ = releaseWrite.Close()
		_ = scope.Kill()
		if child := command.Process(); child != nil {
			_ = child.Kill()
		}
		_ = command.cmd.Wait()
		return result, confineUnavailable(sliceName, cause)
	}
	if interrupted.Load() {
		return abortStarted(errors.New("interrupted before confined target release"))
	}

	handshakeTimeout := request.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = time.Second
	}
	if payload, readErr := deps.readHandshake(handshakeRead, handshakeTimeout); readErr == nil {
		if handshake, verified := parseConfineHandshake(payload); verified && handshake.applied() {
			result.Status.Priorities = ConfinePrioritiesApplied
		}
	}
	pid := command.Process().Pid
	members, memberErr := scope.Members()
	if memberErr != nil {
		return abortStarted(fmt.Errorf("verify scope membership: %w", memberErr))
	}
	if !containsPID(members, pid) {
		return abortStarted(fmt.Errorf("verify scope membership: pid %d absent", pid))
	}
	bootID, bootErr := currentBootID()
	identity := PIDIdentity{PID: pid, StartTick: processStartTick(pid), BootID: bootID}
	if bootErr != nil || identity.StartTick == 0 {
		if bootErr == nil {
			bootErr = errors.New("process start tick unavailable")
		}
		return abortStarted(fmt.Errorf("verify process identity: %w", bootErr))
	}
	result.Status.Scope = ConfineScopePlaced
	monitorStop := make(chan struct{})
	monitorResult := make(chan scopeMonitorResult, 1)
	go monitorScopeMembership(scope, identity, members, monitorStop, monitorResult)
	if interrupted.Load() {
		close(monitorStop)
		<-monitorResult
		return abortStarted(errors.New("interrupted before confined target release"))
	}
	if n, writeErr := releaseWrite.Write([]byte{1}); writeErr != nil || n != 1 {
		close(monitorStop)
		<-monitorResult
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return abortStarted(fmt.Errorf("release confined target: %w", writeErr))
	}
	_ = releaseWrite.Close()
	result.Exit = waitConfineCommand(cmd)
	usage := deps.readUsage(scope.Reference())
	if usage.PeakRSS != nil && *usage.PeakRSS <= 0 {
		usage.PeakRSS = nil
	}
	result.Status.PeakRSS = usage.PeakRSS
	oom := usage.OOMKill != nil && *usage.OOMKill > 0
	if signature != "" {
		reportCtx, cancelReport := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_ = deps.reportPeak(reportCtx, request, signature, usage.PeakRSS, oom)
		cancelReport()
	}
	close(monitorStop)
	monitorSummary := <-monitorResult
	teardown := attestScopeTeardown(ctx, scope, pid, 2*time.Second)
	integrity, _, _ := classifyLaunchScopeIntegrity(launchScopeFacts{
		ScopeVerified: true, PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		ScopePath: scope.Reference(), Monitor: monitorSummary, Teardown: teardown,
	})
	result.Status.ScopeIntegrity = integrity
	if observation := escapedObservation(scope.Reference(), monitorSummary.Escape, teardown.Escape); observation != nil {
		result.Status.DescendantEscape = &DescendantEscapeEvidence{PIDIdentity: observation.Identity, Cgroup: observation.Cgroup}
	}
	_, _ = fmt.Fprintln(diagnostics, FormatConfineStatus(result.Status))
	if advisory := formatConfineReserveAdvisory(result.Status.ScopeMemoryMax, result.Status.PeakRSS, oom); advisory != "" {
		_, _ = fmt.Fprintln(diagnostics, advisory)
	}
	return result, nil
}

func formatConfineReserveAdvisory(scopeMemoryMax int64, peakRSS *int64, oom bool) string {
	if scopeMemoryMax <= 0 {
		return ""
	}
	peak := "unknown"
	if peakRSS != nil {
		peak = FormatConfineBytes(*peakRSS)
	}
	if oom {
		return fmt.Sprintf("confine: job OOM-killed at its memory cap %s (peak RSS %s); raise the cap with --memory-max (or --memory-reserve for a whole-job reserve), or split heavy work", FormatConfineBytes(scopeMemoryMax), peak)
	}
	// Overflow-safe threshold: never multiply a byte count that the input domain
	// allows to approach MaxInt64. `scopeMemoryMax - scopeMemoryMax/10` is a
	// conservative >=90% mark that never fires below 90% for a non-round cap.
	if peakRSS != nil && *peakRSS >= scopeMemoryMax-scopeMemoryMax/10 {
		return fmt.Sprintf("confine: peak RSS %s reached %d%% of the reserved cap %s; consider a higher --memory-reserve or --delegate-ram for suites", peak, confinePercentOfCap(*peakRSS, scopeMemoryMax), FormatConfineBytes(scopeMemoryMax))
	}
	return ""
}

// confinePercentOfCap returns floor(peak*100/cap) exactly, for any peak and cap
// in [1, MaxInt64], by doing the multiply in 128 bits so peak*100 never wraps.
// cap is > 0 and peak > 0 at every call site; peak <= cap (a peak above the
// enforced cap is an OOM, handled before this is reached), so the quotient is
// <= 100 and bits.Div64 (which requires the quotient to fit in 64 bits) is safe.
func confinePercentOfCap(peak, capBytes int64) int {
	hi, lo := bits.Mul64(uint64(peak), 100)
	quotient, _ := bits.Div64(hi, lo, uint64(capBytes))
	return int(quotient)
}

func admitConfine(ctx context.Context, path string, request ConfineRequest, reserve int64) (admissionResult, error) {
	maxWait := request.AdmissionMaxWait
	if maxWait <= 0 {
		maxWait = 30 * time.Minute
	}
	poll := request.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	admitter := &Runner{
		memorySlice: path, memoryReserve: reserve, admissionMaxWait: maxWait,
		pollInterval: poll, clock: systemClock{}, sliceMemory: readSliceMemory,
		diagnostics: request.Stderr, admitSocketPath: request.AdmitSocketPath,
	}
	return admitter.admit(ctx, Request{
		ResourceSignature:    request.ResourceSignature,
		MemoryReservePinned:  request.MemoryReservePinned,
		DaemonEstimateMemory: true,
		DelegateRAM:          request.DelegateRAM,
		ConfineScopeID:       request.ScopeID,
		ConfineName:          request.Name,
		ConfineOwner:         request.Owner,
	})
}

func readConfineCap(path string) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(path, "memory.max"))
	if err != nil || strings.TrimSpace(string(data)) == "max" {
		return 0, false
	}
	return parseAdmissionMemory(data)
}

func confineEnvironment(env []string) []string {
	if env == nil {
		return os.Environ()
	}
	return append([]string(nil), env...)
}

func validateConfineName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 100 {
		return errors.New("E_CONFINE_ARGUMENT_INVALID: --name is too long")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r) {
			continue
		}
		return errors.New("E_CONFINE_ARGUMENT_INVALID: --name requires letters, digits, '.', '_', or '-'")
	}
	return nil
}

const delegateRAMScopeIDMarker = "@dr"

func confineScopeID(name string, delegateRAM bool) string {
	if name == "" {
		name = "job"
	}
	if delegateRAM {
		return "CONFINE-" + delegateRAMScopeIDMarker + "-" + name + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "CONFINE-" + name + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func delegateRAMScopeFallback() int64 {
	value := strings.TrimSpace(os.Getenv("AIRA_DELEGATE_RAM_SCOPE_DEFAULT"))
	if parsed, err := ParseMemorySize(value); err == nil && parsed > 0 {
		return parsed
	}
	return DefaultDelegateRAMScopeCeiling
}

func confineUnavailable(slice string, err error) error {
	return fmt.Errorf("E_CONFINE_UNAVAILABLE: slice %s: %w", slice, err)
}

func writeConfineOOMGroup(scope Scope) error {
	fd, err := unix.Openat(scope.FD(), "memory.oom.group", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "memory.oom.group")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("open memory.oom.group")
	}
	if _, err := file.WriteString("1\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	readFD, err := unix.Openat(scope.FD(), "memory.oom.group", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	readFile := os.NewFile(uintptr(readFD), "memory.oom.group")
	if readFile == nil {
		_ = unix.Close(readFD)
		return errors.New("open memory.oom.group for verification")
	}
	defer readFile.Close()
	data, err := io.ReadAll(io.LimitReader(readFile, 32))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "1" {
		return errors.New("memory.oom.group did not read back as 1")
	}
	return nil
}

func writeScopeMemoryCap(scope Scope, maximum, high int64, setOOMGroup bool) error {
	if setOOMGroup {
		if err := writeConfineOOMGroup(scope); err != nil {
			return fmt.Errorf("set memory.oom.group: %w", err)
		}
	}
	if err := writeScopeMemoryValue(scope, "memory.max", maximum); err != nil {
		return err
	}
	if high > 0 {
		if err := writeScopeMemoryValue(scope, "memory.high", high); err != nil {
			return err
		}
	}
	if err := verifyScopeMemoryValue(scope, "memory.max", floorMemoryPage(maximum)); err != nil {
		return err
	}
	if high > 0 {
		if err := verifyScopeMemoryValue(scope, "memory.high", floorMemoryPage(high)); err != nil {
			return err
		}
	}
	return nil
}

func writeScopeMemoryValue(scope Scope, name string, value int64) error {
	fd, err := unix.Openat(scope.FD(), name, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open %s", name)
	}
	if _, err := file.WriteString(strconv.FormatInt(value, 10) + "\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return nil
}

// writeScopeCPUWeightFailOpen is deliberately unlike writeScopeMemoryValue:
// CPU aging is an optional contention mitigation, so a vanished or
// undelegated controller must never abort the foreground launch. Openat keeps
// the write anchored to the already-open scope and cannot follow a recreated
// scope name after teardown.
func writeScopeCPUWeightFailOpen(scope Scope, weight int64) bool {
	fd, err := unix.Openat(scope.FD(), "cpu.weight", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), "cpu.weight")
	if file == nil {
		_ = unix.Close(fd)
		return false
	}
	_, writeErr := file.WriteString(strconv.FormatInt(weight, 10) + "\n")
	closeErr := file.Close()
	return writeErr == nil && closeErr == nil
}

func confineCPUWeightConfig() cpuWeightConfig {
	start := parseCPUWeightEnv("AIRA_CONFINE_CPUWEIGHT_START", defaultCPUWeightStart)
	floor := parseCPUWeightEnv("AIRA_CONFINE_CPUWEIGHT_FLOOR", defaultCPUWeightFloor)
	if floor > start {
		floor = start
	}
	steps := append([]cpuWeightStep(nil), defaultCPUWeightSteps...)
	if raw := strings.TrimSpace(os.Getenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE")); raw != "" {
		if parsed, err := parseCPUWeightSchedule(raw); err == nil {
			steps = parsed
		}
	}
	previous := start
	for index := range steps {
		steps[index].Weight = clampCPUWeight(steps[index].Weight)
		if steps[index].Weight < floor {
			steps[index].Weight = floor
		}
		if steps[index].Weight > previous {
			steps[index].Weight = previous
		}
		previous = steps[index].Weight
	}
	// Every valid schedule reaches the configured positive floor. This is a
	// proportional-share mitigation for a finite runnable set, not an
	// anti-starvation guarantee for an unbounded stream of fresh scopes.
	steps[len(steps)-1].Weight = floor
	return cpuWeightConfig{Start: start, Floor: floor, Steps: steps}
}

func parseCPUWeightEnv(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return clampCPUWeight(value)
}

// parseCPUWeightSchedule accepts elapsed-time steps such as
// "10s:100,30s:70,1m:50,5m:30,10m:20,30m:10". '=' is also accepted so an
// environment value is convenient to write in shells and service files.
func parseCPUWeightSchedule(raw string) ([]cpuWeightStep, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 {
		return nil, errors.New("cpu weight schedule is empty")
	}
	steps := make([]cpuWeightStep, 0, len(parts))
	var previous time.Duration
	var previousWeight int64 = 10000
	for _, part := range parts {
		part = strings.TrimSpace(part)
		separator := ":"
		if !strings.Contains(part, separator) {
			separator = "="
		}
		delayText, weightText, ok := strings.Cut(part, separator)
		if !ok || strings.TrimSpace(delayText) == "" || strings.TrimSpace(weightText) == "" {
			return nil, errors.New("invalid cpu weight schedule step")
		}
		delay, err := time.ParseDuration(strings.TrimSpace(delayText))
		if err != nil || delay <= 0 || delay <= previous {
			return nil, errors.New("cpu weight schedule delays must increase")
		}
		weight, err := strconv.ParseInt(strings.TrimSpace(weightText), 10, 64)
		if err != nil {
			return nil, errors.New("invalid cpu weight schedule weight")
		}
		weight = clampCPUWeight(weight)
		if weight > previousWeight {
			return nil, errors.New("cpu weight schedule must be monotone")
		}
		steps = append(steps, cpuWeightStep{After: delay, Weight: weight})
		previous, previousWeight = delay, weight
	}
	return steps, nil
}

func clampCPUWeight(weight int64) int64 {
	if weight < 1 {
		return 1
	}
	if weight > 10000 {
		return 10000
	}
	return weight
}

// startCPUWeightDecay owns a single timer goroutine for this foreground
// supervisor. Its returned stopper closes and joins it before scope removal,
// preventing a late write to a deleted scope.
func startCPUWeightDecay(scope Scope, steps []cpuWeightStep, write func(Scope, int64) bool) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(done)
		started := time.Now()
		for _, step := range steps {
			delay := time.Until(started.Add(step.After))
			if delay < 0 {
				delay = 0
			}
			timer := time.NewTimer(delay)
			select {
			case <-stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
				_ = write(scope, step.Weight)
			}
		}
	}()
	return func() {
		stopOnce.Do(func() { close(stop) })
		<-done
	}
}

func verifyScopeMemoryValue(scope Scope, name string, want int64) error {
	fd, err := unix.Openat(scope.FD(), name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s for verification: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open %s for verification", name)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return fmt.Errorf("read %s for verification: %w", name, err)
	}
	got, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || got != want {
		return fmt.Errorf("%s read-back=%q, want %d", name, strings.TrimSpace(string(data)), want)
	}
	return nil
}

func confineSetupArgv(target []string, delegateRAM bool) ([]string, error) {
	nonDelegate, delegate, err := confineOOMScoreAdjValues()
	if err != nil {
		return nil, err
	}
	oomAdj := nonDelegate
	if delegateRAM {
		oomAdj = delegate
	}
	argv := []string{
		"__confine-setup", "--handshake-fd", strconv.Itoa(confineSetupFD),
		"--release-fd", strconv.Itoa(confineReleaseFD),
		"--oom-score-adj", strconv.Itoa(oomAdj), "--nice", strconv.Itoa(confineNice),
		"--ionice-class", strconv.Itoa(confineIOPriorityClass), "--",
	}
	return append(argv, target...), nil
}

func confineOOMScoreAdjValues() (nonDelegate, delegate int, err error) {
	nonDelegate, err = parseConfineOOMScoreAdjEnv("AIRA_CONFINE_OOM_SCORE_ADJ", confineOOMScoreAdj)
	if err != nil {
		return 0, 0, err
	}
	delegate, err = parseConfineOOMScoreAdjEnv("AIRA_CONFINE_OOM_SCORE_ADJ_DELEGATE", confineDelegateOOMScoreAdj)
	if err != nil {
		return 0, 0, err
	}
	if delegate <= nonDelegate {
		return 0, 0, errors.New("E_CONFINE_ARGUMENT_INVALID: AIRA_CONFINE_OOM_SCORE_ADJ_DELEGATE must be greater than AIRA_CONFINE_OOM_SCORE_ADJ")
	}
	return nonDelegate, delegate, nil
}

func parseConfineOOMScoreAdjEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < confineOOMScoreAdj || value > 1000 {
		return 0, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %s must be an integer in [%d, 1000]", name, confineOOMScoreAdj)
	}
	return value, nil
}

func readConfineHandshake(reader *os.File, timeout time.Duration) ([]byte, error) {
	type outcome struct {
		payload []byte
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		payload, err := io.ReadAll(io.LimitReader(reader, confineHandshakeMaxSize+1))
		done <- outcome{payload: payload, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case got := <-done:
		if got.err != nil || len(got.payload) > confineHandshakeMaxSize {
			return nil, errors.New("invalid setup handshake")
		}
		return got.payload, nil
	case <-timer.C:
		_ = reader.Close()
		return nil, errors.New("setup handshake timed out")
	}
}

func confineSignalSource() (<-chan os.Signal, func()) {
	forward := make(chan os.Signal, 2)
	signal.Notify(forward, syscall.SIGINT, syscall.SIGTERM)
	return forward, func() { signal.Stop(forward) }
}

func forwardConfineSignals(forward <-chan os.Signal, process func() *os.Process, onSignal func()) func() {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case received, ok := <-forward:
				if !ok {
					return
				}
				if onSignal != nil {
					onSignal()
				}
				if child := process(); child != nil {
					_ = child.Signal(received)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
	}
}

func waitConfineCommand(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if wait, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if wait.Signaled() {
				return 128 + int(wait.Signal())
			}
			return wait.ExitStatus()
		}
	}
	return 3
}

func cleanupConfineScope(scope Scope, started bool) {
	if scope == nil {
		return
	}
	if started {
		_ = scope.Kill()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = waitEmpty(ctx, scope, 2*time.Second)
	_ = scope.Remove()
}

// RunConfineSetup verifies its own cgroup, applies child-local priority knobs,
// reports them to the parent, and waits for the parent's release before exec.
func RunConfineSetup(argv []string, diagnostics io.Writer) int {
	handshakeFD, releaseFD, oomAdj, niceValue, ioClass, target, err := parseConfineSetupArgs(argv)
	if err != nil {
		if diagnostics != nil {
			_, _ = fmt.Fprintln(diagnostics, err)
		}
		return 127
	}
	handshake := os.NewFile(uintptr(handshakeFD), "confine-handshake")
	release := os.NewFile(uintptr(releaseFD), "confine-release")
	if handshake == nil || release == nil {
		return 127
	}
	unix.CloseOnExec(handshakeFD)
	unix.CloseOnExec(releaseFD)
	defer handshake.Close()
	defer release.Close()
	if verifyErr := verifyConfineSetupScope(); verifyErr != nil {
		_ = writeConfineHandshake(handshake, confineHandshake{Schema: confineHandshakeSchema})
		if diagnostics != nil {
			_, _ = fmt.Fprintf(diagnostics, "confine setup: verify scope: %v\n", verifyErr)
		}
		return 127
	}
	result := confineHandshake{Schema: confineHandshakeSchema}
	result.OOMScoreAdj = os.WriteFile("/proc/self/oom_score_adj", []byte(strconv.Itoa(oomAdj)+"\n"), 0o644) == nil
	result.Nice = unix.Setpriority(unix.PRIO_PROCESS, 0, niceValue) == nil
	_, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, uintptr(1), 0, uintptr(confineIOPriority(ioClass)))
	result.IONice = errno == 0
	if err := writeConfineHandshake(handshake, result); err != nil {
		if diagnostics != nil {
			_, _ = fmt.Fprintf(diagnostics, "confine setup: write handshake: %v\n", err)
		}
		return 127
	}
	if err := handshake.Close(); err != nil {
		return 127
	}
	var releaseByte [1]byte
	if _, err := io.ReadFull(release, releaseByte[:]); err != nil {
		if diagnostics != nil {
			_, _ = fmt.Fprintf(diagnostics, "confine setup: wait for release: %v\n", err)
		}
		return 127
	}
	path, lookErr := exec.LookPath(target[0])
	if lookErr != nil {
		if diagnostics != nil {
			_, _ = fmt.Fprintf(diagnostics, "confine setup: %v\n", lookErr)
		}
		return 127
	}
	if err := syscall.Exec(path, target, os.Environ()); err != nil {
		if diagnostics != nil {
			_, _ = fmt.Fprintf(diagnostics, "confine setup: exec: %v\n", err)
		}
		return 127
	}
	return 127
}

func verifyConfineSetupScope() error {
	mount, err := unifiedMount()
	if err != nil {
		return err
	}
	path, err := currentCgroupPath(mount)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(path, "memory.oom.group"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "1" {
		return errors.New("memory.oom.group is not 1")
	}
	// oom.group alone does not bound growth — it only groups the kill once an OOM
	// occurs. A heavy job must never exec in an UNCAPPED cgroup, or it can still
	// drive the host to a global OOM. Require a finite memory.max somewhere in the
	// ancestry (the effective cap), so a standalone/forged-fd invocation cannot run
	// the target in an oom.group-but-uncapped cgroup (Sol confirm P0-a).
	if !hasFiniteCapAncestor(mount, path) {
		return errors.New("no finite memory.max in cgroup ancestry (uncapped)")
	}
	return nil
}

// hasFiniteCapAncestor reports whether the cgroup at path, or any ancestor up to
// the cgroup2 mount root, has a finite memory.max (memory.max is hierarchical, so
// any finite ancestor bounds the subtree).
func hasFiniteCapAncestor(mount, path string) bool {
	_, ok := effectiveCapFrom(mount, path)
	return ok
}

// effectiveConfineCap returns the effective memory ceiling for a slice path: the
// smallest finite memory.max across the path and its ancestors up to the cgroup2
// mount root, and whether any finite cap bounds the subtree. memory.max is
// hierarchical, so the effective ceiling is the minimum finite value in the
// ancestry — an uncapped slice under a capped parent is still bounded.
func effectiveConfineCap(path string) (int64, bool) {
	mount, err := unifiedMount()
	if err != nil {
		return 0, false
	}
	return effectiveCapFrom(mount, path)
}

func effectiveCapFrom(mount, path string) (int64, bool) {
	current := filepath.Clean(path)
	root := filepath.Clean(mount)
	var best int64
	found := false
	for {
		if maximum, finite := readConfineCap(current); finite && maximum > 0 {
			if !found || maximum < best {
				best, found = maximum, true
			}
		}
		if current == root {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || len(parent) < len(root) {
			break
		}
		current = parent
	}
	return best, found
}

func writeConfineHandshake(writer io.Writer, result confineHandshake) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	for len(payload) > 0 {
		n, writeErr := writer.Write(payload)
		if writeErr != nil {
			return writeErr
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

// confineIOPriority encodes the ioprio_set value: the I/O class in the high bits
// with the pinned lowest best-effort priority (confineIOPriorityData). The child
// syscall and its test both call this, so the test pins the real callsite rather
// than recomputing the expression.
func confineIOPriority(ioClass int) int {
	return ioClass<<13 | confineIOPriorityData
}

func parseConfineSetupArgs(argv []string) (handshakeFD, releaseFD, oomAdj, niceValue, ioClass int, target []string, err error) {
	if len(argv) < 11 || argv[0] != "--handshake-fd" || argv[2] != "--release-fd" || argv[4] != "--oom-score-adj" || argv[6] != "--nice" || argv[8] != "--ionice-class" || argv[10] != "--" || len(argv) == 11 || argv[11] == "" {
		return 0, 0, 0, 0, 0, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: malformed confine setup arguments")
	}
	values := []*int{&handshakeFD, &releaseFD, &oomAdj, &niceValue, &ioClass}
	for i, text := range []string{argv[1], argv[3], argv[5], argv[7], argv[9]} {
		value, parseErr := strconv.Atoi(text)
		if parseErr != nil {
			return 0, 0, 0, 0, 0, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: malformed confine setup arguments")
		}
		*values[i] = value
	}
	if handshakeFD < 3 || releaseFD < 3 || handshakeFD == releaseFD || oomAdj < -1000 || oomAdj > 1000 || niceValue < -20 || niceValue > 19 || ioClass != 2 {
		return 0, 0, 0, 0, 0, nil, errors.New("E_CONFINE_ARGUMENT_INVALID: malformed confine setup arguments")
	}
	return handshakeFD, releaseFD, oomAdj, niceValue, ioClass, append([]string(nil), argv[11:]...), nil
}
