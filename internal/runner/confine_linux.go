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
	"net"
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
	confineHandshakeSchema  = 1
	confineHandshakeMaxSize = 4096
	confineSetupFD          = 3
	confineReleaseFD        = 4
	confineNice             = 19
	confineIOPriorityClass  = 2 // best-effort / IOPRIO_CLASS_BE
	confineIOPriorityData   = 7 // Best-effort priority: 0 is highest, 7 is lowest.
	defaultCPUWeightStart   = int64(100)
	defaultCPUWeightFloor   = int64(10)
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
	// AIRA-121. group is set only on the ci-shim launch path, where the child is
	// made the leader of its own process group (Setpgid) so a forwarded signal
	// can reach DESCENDANTS the real path reaches with cgroup.kill. reaped is the
	// pgid-recycling cut-off: once cmd.Wait() has returned, the leader has been
	// reaped and its pid may be reissued to an unrelated process, so no group
	// signal may EVER be sent past that point.
	group  bool
	reaped bool
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

// markReaped closes the group-signal cut-off. Called exactly once, immediately
// after cmd.Wait() returns.
func (command *confineCommand) markReaped() {
	command.mu.Lock()
	defer command.mu.Unlock()
	command.reaped = true
}

// signal delivers sig to the confined job. On the real path that is the direct
// child, byte-identical to what confine has always done; the scope's own
// cgroup.kill is what reaches descendants there.
//
// On the ci-shim path there is no cgroup.kill, so delivery is to the child's
// process GROUP (requirement 8): kill(-pgid, sig) reaches every descendant that
// has not deliberately setsid'd or double-forked out of the group. A descendant
// that HAS is unreachable by any non-cgroup mechanism; that gap is documented
// rather than papered over, and asserted by a negative test.
func (command *confineCommand) signal(sig os.Signal) error {
	command.mu.Lock()
	defer command.mu.Unlock()
	if command.process == nil {
		return nil
	}
	if !command.group {
		return command.process.Signal(sig)
	}
	if command.reaped {
		// The leader has been reaped; its pid (and therefore this pgid) can have
		// been reissued. Signalling now could hit an unrelated process group.
		return nil
	}
	number, ok := sig.(syscall.Signal)
	if !ok {
		return command.process.Signal(sig)
	}
	return unix.Kill(-command.process.Pid, number)
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
	writeScopeSwapCap     func(Scope) (string, error)
	writeScopeMemoryCap   func(Scope, int64, int64, bool) error
	writeScopeCPUWeight   func(Scope, int64) bool
	start                 func(*confineCommand) error
	readHandshake         func(*os.File, time.Duration) ([]byte, error)
	readCap               func(string) (int64, bool)
	signalSource          func() (<-chan os.Signal, func())
	readUsage             func(string) cgroupUsage
	reportPeak            func(context.Context, ConfineRequest, string, *int64, bool) error
	queuePosition         func(context.Context, ConfineRequest, string) (confineQueuePosition, bool)
	// resolveMode is the AIRA-121 confinement-mode seam. Production is
	// ResolveConfineMode, which reads the durable install-mode record; tests
	// substitute a constant so the shim path can be exercised without an
	// installed record. It is READ ONCE per launch, before any slice work.
	resolveMode           func() string
	admitWaitDiagInterval time.Duration
	// admitQueueProbeTimeout bounds ONE queue-position probe. Tests raise it
	// well above their own patience so that "the grant cancelled the probe" is
	// distinguishable from "the probe's own timeout expired"; production uses
	// confineQueueProbeTimeout.
	admitQueueProbeTimeout time.Duration
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
		writeScopeSwapCap:     writeScopeSwapCap,
		writeScopeMemoryCap:   writeScopeMemoryCap,
		writeScopeCPUWeight:   writeScopeCPUWeightFailOpen,
		start:                 func(command *confineCommand) error { return command.Start() },
		readHandshake:         readConfineHandshake,
		readCap:               effectiveConfineCap,
		signalSource:          confineSignalSource,
		readUsage:             readCgroupUsage,
		reportPeak:            reportConfinePeak,
		queuePosition:         confineQueuePositionFromDaemon,
		resolveMode:           ResolveConfineMode,
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
	if deps.writeScopeSwapCap == nil {
		deps.writeScopeSwapCap = defaults.writeScopeSwapCap
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
	if deps.queuePosition == nil {
		deps.queuePosition = defaults.queuePosition
	}
	if deps.resolveMode == nil {
		deps.resolveMode = defaults.resolveMode
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
	// One normaliser, shared with MintConfineScopeID (AIRA-22): a detached
	// supervisor mints its own scope id before calling this, and two independent
	// copies of "empty name means job, empty owner means unknown" would be free to
	// drift apart between the mint and the bind.
	normalizedName, normalizedOwner, identityErr := normalizeConfineIdentity(request)
	if identityErr != nil {
		result.Status.Slice = attemptedSlice
		return result, identityErr
	}
	request.Name, request.Owner = normalizedName, normalizedOwner
	// AIRA-121. The ci-shim branch, taken HERE: after every argument, cap,
	// reserve-bound and identity check (all of which are mode-independent and
	// must refuse identically in both modes), and BEFORE the first line that
	// touches a cgroup. Requirement 2 asks for the cgroup work to be skipped
	// entirely rather than attempted and failed, and this placement is what makes
	// that structural -- there is no cgroup syscall above the branch to issue.
	if deps.resolveMode() == ConfineModeShim {
		return confineShim(ctx, request, deps, result)
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
	// AIRA-102. A caller-declared CONTAINER limit may change what this job charges
	// the shared ledger, but only for docker and only when nothing was declared on
	// the confine side. Deliberately applied AFTER ResolveConfineReserve so the
	// declaredReserve/declaredReserveBytes provenance captured above is untouched:
	// those two decide the scope memory.max further down, and a container-derived
	// number must never masquerade as a caller-declared cap.
	//
	// Podman is skipped entirely. Its container is nested INSIDE this job's own
	// scope (--cgroups=split), so its memory is already inside whatever the job
	// reserved; raising would over-book past a binding cap or, unpinned, replace
	// the daemon's history estimate with a client-pinned guess.
	containerPlan := PlanContainerIntegration(request.Argv)
	var containerReserveSkip string
	reserve, pinned, containerReserveSkip = containerPlan.ResolveReserve(reserve, pinned, request.DelegateRAM, maximum)
	signature := request.ResourceSignature
	if signature == "" {
		if computed, signatureErr := ResourceSignature(nil, nil, request.Argv); signatureErr == nil {
			signature = computed
		}
	}
	request.ResourceSignature = signature
	request.MemoryReserve = reserve
	request.MemoryReservePinned = pinned
	// AIRA-22: a DETACHED supervisor mints its own scope id before calling here,
	// because the pid embedded in the cgroup directory name is what
	// `confine --kill <pid>`, `--list`'s SupervisorPID column, and the orphan
	// reaper's liveness predicate all read back — and for a detached job the
	// process that must be named there is the supervisor, not the launcher that
	// exits seconds later. The field is unexported, so only package runner can
	// supply one, and bindConfineScopeID additionally refuses any id that does not
	// describe THIS process running THIS request: syntax alone accepts a foreign
	// pid, a foreign owner, or the wrong delegate class. Never silently re-mint —
	// that would run the job in a scope the durable record does not name.
	scopeID := request.presetScopeID
	if scopeID == "" {
		scopeID = confineScopeID(request.Name, request.Owner, request.DelegateRAM)
	} else if bindErr := bindConfineScopeID(scopeID, request.Name, request.Owner, request.DelegateRAM); bindErr != nil {
		return result, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %w", bindErr)
	}
	request.ScopeID = scopeID
	// The launch gate (AIRA-22). Every precondition the foreground form reports
	// synchronously — argv, reserve bounds, owner, slice resolution, the backend
	// probe, the finite-cap refusal, memory delegation — has now passed, and the
	// only unbounded wait in this function is the admission immediately below. A
	// detached supervisor announces readiness from here, so `--detach` cannot turn
	// an exit-2/exit-4 launch failure into a premature exit 0. A non-nil return
	// aborts before admission: nothing is charged, no scope exists, no child ran.
	if request.BeforeAdmit != nil {
		if gateErr := request.BeforeAdmit(confineLaunchInfo(scopeID, sliceName, maximum)); gateErr != nil {
			return result, gateErr
		}
	}
	// Admission can legitimately block: a reserve-contended slice queues this job
	// behind other sessions' in-flight jobs under the shared cap, and the client
	// waits on a single socket read for up to its maxWait. Emit a periodic progress
	// line so a bounded wait is never mistaken for a hang. Nothing prints when
	// admission returns promptly; the goroutine is stopped and joined first.
	//
	// AIRA-24: each line also carries this job's own place in the queue, which
	// is what turns "still alive" into a decision an operator can act on. The
	// position is fetched over a SEPARATE, short-lived daemon connection per
	// tick — the blocked admission socket is this job's lease and the daemon
	// reads its next byte as "the client went away", so it must never be
	// multiplexed. See confineQueuePositionFromDaemon.
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
				queueNote := confineQueueNote(ctx, deps, request, path, admitWaitDone)
				// Sampled AFTER the probe, which can take up to its own
				// timeout: reading the clock first would under-report the wait
				// by however long the daemon took to answer.
				waited := int64(time.Since(start).Seconds())
				// The probe can take up to its own timeout, and the grant can
				// arrive inside that window. Re-check before printing: a
				// "still waiting" line emitted after admission was granted
				// states something that stopped being true while the line was
				// being composed. Build review (DeepSeek) raised this; the
				// window is pre-existing but the probe widens it.
				select {
				case <-admitWaitDone:
					return
				default:
				}
				if pinned {
					fmt.Fprintf(admitDiag, "confine: waiting for memory admission on %s (reserve %s, waited %ds%s)\n",
						sliceName, FormatConfineBytes(reserve), waited, queueNote)
				} else {
					fmt.Fprintf(admitDiag, "confine: waiting for memory admission on %s (requested reserve %s, unpinned — the daemon resolves the actual grant, which may differ; waited %ds%s)\n",
						sliceName, FormatConfineBytes(reserve), waited, queueNote)
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
	// AIRA-101. teardownStarted gates the exclusivity watcher below. It MUST be
	// set before the lease is closed on the ordinary exit path, or the watcher's
	// read fails with "use of closed network connection" on every CLEAN run and
	// the facet reports `exclusive=lost` every single time — inverting an honesty
	// signal into a permanent false alarm.
	var teardownStarted atomic.Bool
	releaseAdmission := func() {
		releaseAdmissionOnce.Do(func() {
			teardownStarted.Store(true)
			admission.releaseAdmission()
		})
	}
	defer releaseAdmission()
	switch admission.state {
	case "immediate", "waited":
		result.Status.Admission = ConfineAdmissionAdmitted
	case "timeout":
		result.Status.Admission = ConfineAdmissionTimeout
	default:
		result.Status.Admission = ConfineAdmissionUnevaluated
	}
	// AIRA-101. Reaching here under --exclusive means a REAL grant: admit()
	// refuses rather than degrades (see exclusiveRefusal), so there is no path on
	// which this records exclusivity that was not actually obtained. The facet is
	// finalised on the completion path beside TerminatedBy, where a mid-run loss
	// can still downgrade it.
	var exclusiveLost atomic.Bool
	if request.Exclusive {
		result.Status.Exclusive = ConfineExclusiveGranted
		result.Status.ExclusiveDrainedMS = admission.waitedMS
	}

	// Resolved BEFORE the signal handler is installed below, not at the launch
	// site further down where it used to live: the handler now writes AIRA-70's
	// missing "received SIGTERM" line, and a handler that closed over a writer
	// assigned later would dereference nil on a path no ordinary test run
	// exercises. Content is unchanged, and no early return is crossed by the
	// move.
	diagnostics := request.Stderr
	if diagnostics == nil {
		diagnostics = os.Stderr
	}
	diagnostics = &confineLockedWriter{w: diagnostics}

	// AIRA-101. Watch the admission lease. Its closure BEFORE teardown means the
	// daemon restarted or stopped, so the exclusive hold is gone and the rest of
	// this run was contended. The job is deliberately NOT killed — killing a
	// benchmark because the daemon restarted is worse than reporting it honestly.
	//
	// Both sides already read one byte from this socket purely as a liveness
	// signal, and neither writes after the grant frame, so reading it here is
	// consistent with the existing protocol rather than a new use of the channel.
	//
	// TWO stated limits of this watcher, neither fixed:
	//   - It sees the lease CLOSING. A ledger-only release (the stale-lease sweep,
	//     or confine --kill's daemon-side discharge) leaves the socket open and the
	//     handler parked, so exclusivity can end without this firing. The honest
	//     claim is "never silently downgraded WHEN THE LEASE CLOSES".
	//   - A close in the last few milliseconds before teardown is masked by the
	//     guard below and reported as a clean run. The window is the teardown
	//     itself, and erring that way keeps a clean run from being libelled.
	//
	// The teardownStarted guard is load-bearing: releaseAdmission closes the lease
	// on the ordinary exit path, so without it this read would fail with "use of
	// closed network connection" on EVERY clean run and report exclusive=lost
	// every time, inverting the honesty signal into a permanent false alarm.
	if request.Exclusive {
		lease, watchable := admission.release.(net.Conn)
		if !watchable {
			// The grant is real (admit refuses anything else), but without a lease
			// connection to watch, a mid-run loss could not be detected — so the run's
			// outcome is UNEVALUATED rather than granted. Claiming `granted` here
			// would assert something this process cannot observe, which is the exact
			// fabrication the facet exists to prevent, and it is also what made
			// `unevaluated` unreachable in an earlier revision: a value nothing can
			// emit is a reader trap (found by build review).
			result.Status.Exclusive = ConfineExclusiveUnevaluated
		}
		if watchable {
			go func() {
				var one [1]byte
				_, readErr := lease.Read(one[:])
				if readErr == nil || teardownStarted.Load() {
					return
				}
				exclusiveLost.Store(true)
				fmt.Fprint(diagnostics, "aira: warning: exclusivity lost (admission lease closed) — this run was no longer scheduled alone; treat any measurement from it as contended\n")
			}()
		}
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
	deliver := func(sig os.Signal) error {
		commandMu.RLock()
		defer commandMu.RUnlock()
		if command == nil {
			return nil
		}
		return command.signal(sig)
	}
	// AIRA-70 finding #1: a signal delivered to the supervisor itself used to
	// leave no trace anywhere -- no log line, and a trailer identical to a clean
	// run's. It is now witnessed here and named on the trailer.
	//
	// The witness is recorded, and the line written, BEFORE interrupted/cleanup.
	// Both orderings matter: cleanup() blocks for up to 2s in waitEmpty, and it
	// is the step that actually kills the job, so recording afterwards would
	// race the very wait this witness is read against. Recording first gives the
	// happens-before edge the trailer relies on -- supervisorSignalMu unlock ->
	// cleanup -> child dies -> cmd.Wait returns -> the snapshot's Lock. Only the
	// FIRST signal is kept: it is the one that caused the teardown, and a
	// follow-up Ctrl-C on an already-dying job is noise.
	//
	// runEnded is the CUT-OFF (build-review round 3, P1). The handler stays live
	// through readUsage, reportPeak and the teardown attestation -- seconds,
	// after the child is already dead. A signal arriving there terminated
	// nothing, so past the cut-off the handler records no witness and, crucially,
	// does NOT tear the scope down: doing so would remove the cgroup out from
	// under readUsage and silently degrade an honest verdict to `unevaluated`.
	// The deferred cleanup() still runs moments later, so the scope is torn down
	// either way; only the timing changes. The signal is still forwarded, and
	// still logged -- with wording that says what actually happened.
	var supervisorSignalMu sync.Mutex
	var supervisorSignal os.Signal
	runEnded := false
	signalEvents, stopSignalSource := deps.signalSource()
	stopSignalHandler := forwardConfineSignals(signalEvents, deliver, func(received os.Signal) {
		supervisorSignalMu.Lock()
		late := runEnded
		first := !late && supervisorSignal == nil
		if first {
			supervisorSignal = received
		}
		supervisorSignalMu.Unlock()
		if late {
			_, _ = fmt.Fprintf(diagnostics, "confine: received %s after the job had already ended; scope %s is being torn down anyway\n",
				confineSignalName(received), scopeID)
			return
		}
		// TEARDOWN FIRST, log second (build-review round 4). `diagnostics` is the
		// confineLockedWriter shared with the child's stderr pump, so on a chatty
		// job writing to a flow-controlled terminal this Fprintf can block on the
		// mutex for as long as the reader is stalled. Gating the kill behind it
		// would make Ctrl-C responsiveness depend on whoever is reading the
		// output. The mutex-guarded witness above still precedes cleanup(), which
		// is what the trailer's happens-before argument actually needs; the log
		// line does not, and it still lands before the trailer either way.
		interrupted.Store(true)
		cleanup()
		if first {
			// "killed" is past tense because cleanup() has already run above;
			// "forwarding" is present tense because forwardConfineSignals does
			// that after this callback returns. Neither tense is decorative --
			// the line must not claim an action that has not happened.
			_, _ = fmt.Fprintf(diagnostics, "confine: received %s; killed scope %s on %s, forwarding to the confined job\n",
				confineSignalName(received), scopeID, sliceName)
		}
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
	// AIRA-121. Enforced containment is claimed HERE and nowhere earlier: the
	// finite-cap gate passed, the scope exists, and memory.oom.group is set, so
	// all three legs of the claim ("a bounded per-job cgroup with a group kill")
	// are established facts. A launch that failed before this point leaves the
	// facet unevaluated rather than asserting a containment it never obtained.
	result.Status.Containment = ConfineContainmentEnforced
	// AIRA-110. memory.swap.max=0, UNCONDITIONALLY and before any cap decision
	// below, because cgroup-v2's memory.max bounds memory and not memory+swap: a
	// scope with only a memory.max is reclaimed into swap instead of being killed
	// (measured: 512 MiB allocated inside a 32 MiB cap, exit status 0, ~520 MiB
	// paged out). AIRA's whole job is protecting a shared machine from
	// uncontrolled memory pressure, and letting a confined job swap defeats it --
	// swap thrash degrades the WHOLE box worse than a clean in-scope OOM-kill,
	// and it is invisible to the reserve ledger and to the peak-RSS history that
	// sizes future reserves. There is deliberately no opt-out flag: a swapping
	// confined job is never the behaviour this primitive promises.
	//
	// Unconditional, rather than paired with the memory.max write further down,
	// because the cap is conditional (an unpinned, non-daemon-admitted launch is
	// deliberately left uncapped) while the swap bound is policy for EVERY
	// confine scope -- an uncapped scope that swaps still evades the slice
	// accounting and still records a deflated peak.
	//
	// Placed immediately after the oom.group write for the ordering reason
	// writeScopeSwapCap documents: that successful memory.* write is the proof
	// that this cgroup HAS the memory controller, so a subsequent ENOENT can only
	// mean the kernel has no swap control rather than no controller at all.
	swapCap, err := deps.writeScopeSwapCap(scope)
	if err != nil {
		return result, confineUnavailable(sliceName, fmt.Errorf("set memory.swap.max: %w", err))
	}
	result.Status.ScopeSwapCap = swapCap
	scopeMemoryMax := request.ScopeMemoryMax
	// AIRA-133. capSource is written by the SAME branch that chooses the cap, so
	// the provenance and the number can never disagree: there is no second
	// derivation to keep in step, and nothing downstream re-infers the source by
	// decoding reserve-basis or by pattern-matching a byte count. Assigned here
	// (rather than left to a switch after the fact) precisely because the branch
	// order below encodes real precedence — --memory-max wins over a declared
	// reserve, which wins over the daemon grant, and delegate-ram's ceiling only
	// fills a gap none of those filled.
	capSource := ""
	if scopeMemoryMax > 0 {
		capSource = ConfineCapSourceMemoryMax
	}
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
		capSource = ConfineCapSourceMemoryReserve
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
		capSource = ConfineCapSourceDaemonReserve
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
		capSource = ConfineCapSourceDelegateRAM
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
		// AIRA-133. Recorded only where a cap was actually WRITTEN, so the facet
		// can never describe a cap that does not exist. An enforced cap whose
		// source somehow went unrecorded renders as unevaluated rather than
		// defaulting to either party.
		result.Status.ScopeMemoryCapSource = capSource
		if result.Status.ScopeMemoryCapSource == "" {
			result.Status.ScopeMemoryCapSource = ConfineCapSourceUnevaluated
		}
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
	// AIRA-102. Container integration happens HERE, at the launch site, for two
	// reasons: scopeMemoryMax is only final by this point (the daemon-estimate cap
	// is assigned above), and the ResourceSignature was already computed from the
	// ORIGINAL argv, so peak-RSS history keys do not fork the day AIRA starts
	// injecting a flag. request.Argv itself is never rewritten -- the durable
	// detached-job record keeps what the caller actually typed.
	// The DECLARED cap, never the resolved scope cap (build review, Fable P0).
	// On an unpinned daemon grant scopeMemoryMax is a peak-RSS estimate, and for
	// docker that estimate tracks the CLI, not the container -- injecting it caps
	// the user's container at tens of megabytes, forever and unrecoverably. Only
	// a number the caller chose may be imposed on their container.
	declaredContainerCap := request.ScopeMemoryMax
	// The !DelegateRAM guard mirrors the scope-cap assignment above, and for the
	// same reason (build review, Fable): under --delegate-ram a declared
	// --memory-reserve is the pinned FRAMEWORK OVERHEAD, not a cap -- the scope's
	// own memory.max is the much larger delegate ceiling. Without this guard,
	// `--delegate-ram --memory-reserve 512M -- podman run img pytest ...` (the
	// SKILL's own recommended pytest idiom) would inject `--memory=536870912`
	// into a container whose scope allows 16G, and OOM-kill it at 512M. Under
	// delegate-ram only an explicit --memory-max is a declared cap.
	if declaredContainerCap <= 0 && declaredReserve && !request.DelegateRAM {
		declaredContainerCap = declaredReserveBytes
	}
	containerInjection := containerPlan.Inject(request.Argv, declaredContainerCap)
	if containerPlan.Detected() {
		result.Status.Container = containerInjection.Placement
		// The ledger charge is real only on a daemon grant. This is the SAME
		// predicate the scope-cap assignment above uses; the flock fallback
		// reports state "immediate"/"waited" WITH a lock and is a slice
		// free-memory check, so a facet keyed on admission.state would claim a
		// charge that never happened.
		ledgerCharged := admitted && admission.lock == nil && admission.release != nil
		result.Status.ContainerMemory = ContainerMemoryFacet(containerPlan, containerInjection, containerReserveSkip, ledgerCharged)
		for _, advisory := range ContainerAdvisories(containerPlan, containerInjection, scopeMemoryMax, result.Status.ReserveBasis) {
			_, _ = fmt.Fprintln(diagnostics, advisory)
		}
	}
	setupArgv, err := confineSetupArgv(containerInjection.Argv, request.DelegateRAM)
	if err != nil {
		return result, err
	}
	stdin, stdout := request.Stdin, request.Stdout
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	cmd := exec.CommandContext(ctx, self, setupArgv...)
	// The one resolved self binary the child's AIRA verbs are invoked through:
	// worker-admit and aitest-bootstrap are both verbs on it.
	reserveCommand := ""
	if request.DelegateRAM {
		reserveCommand = self
		if executable, executableErr := filepath.EvalSymlinks(self); executableErr == nil {
			reserveCommand = executable
		}
	}
	// AIRA-115. `path`, not `sliceName`: it is the RESOLVED slice cgroup path this
	// job was admitted against (admitConfine passes exactly this value as its
	// memorySlice), so a confine-reserve sub-reservation taken inside the job
	// lands in its parent's OWN daemon queue. A bare slice name would instead be
	// re-resolved from the DAEMON's cgroup ancestry, which for a slice that is not
	// on the daemon's own path resolves elsewhere or not at all.
	//
	// It is published under pylib.ConfineParentSliceEnv, NOT AIRA_CONFINE_SLICE:
	// this absolute path is an emitted coordinate, and putting it in the
	// operator's explicit-slice input would make every nested `aira confine` treat
	// it as a declared --slice. See pylib.ConfineParentSliceEnv.
	cmd.Env = pylib.AppendConfineChildEnvironment(confineEnvironment(request.Env), scopeID, path)
	// AIRA-121 requirement 7, as AIRA-123 settled it: AitestBackendCanFunction is
	// THE one gate on publishing the AIRA_AITEST_* coordinates, and it now also
	// names the backend. It is trivially true on this (real) path -- the shim
	// path never reaches here -- and is called anyway so that the gate has
	// exactly one name and one home.
	if _, backendOK := AitestBackendCanFunction(ConfineModeReal); request.DelegateRAM && backendOK {
		// aitest is only meaningful for a delegate-RAM launch (worker-admit
		// grants nested sub-scopes under THIS job's own outer scope); every
		// other launch gets no aitest coordinates at all, mirroring
		// the delegate-RAM gate on reserveCommand immediately above, which is
		// the SAME resolved self binary — both worker-admit and
		// aitest-bootstrap are verbs on that one aira binary.
		// scope.Reference() is THIS job's real outer scope, handed down rather
		// than rediscovered by the bootstrap verb from its own current cgroup
		// (AIRA-44) — which is wrong for a second aitest-enabled pytest run in
		// the same job, because the first run's bootstrap has by then relocated
		// the whole tree into <outer>/.aira-supervisor.
		cmd.Env = pylib.AppendAitestChildEnvironment(cmd.Env, request.RuntimeDir, diagnostics, reserveCommand, scope.Reference())
	} else {
		// Strip unconditionally, not just skip appending (Fable build-review,
		// final gate): AppendAitestChildEnvironment was previously called
		// ONLY inside the branch above, so a non-delegate launch's cmd.Env
		// kept whatever AIRA_AITEST_* coordinates it inherited from ITS OWN
		// parent process untouched -- e.g. a shell or test inside a
		// delegate-RAM aitest job launching `aira confine -- ...` without
		// --delegate-ram would hand its child stale coordinates pointing at
		// the outer job's (possibly since-deleted) extraction dir and relay
		// binary. Mirrors StripCoordinationEnvironment's own unconditional
		// strip on every confine launch (runner_linux.go).
		cmd.Env = pylib.StripAitestEnvironment(cmd.Env)
	}
	// AIRA-101. An exclusive launch STAMPS its own scope id. A non-exclusive one
	// leaves whatever it inherited INTACT: the entire process tree below a holder
	// must carry the token.
	//
	// Set UNCONDITIONALLY here rather than through AppendConfineChildEnvironment:
	// exclusivity is a property of THIS launch, not of the coordination
	// coordinates that helper owns, and an attestation that silently vanished
	// under some configurations would be worse than none at all, because a
	// benchmark checking for it would then refuse to run for entirely the wrong
	// reason.
	//
	// An earlier revision STRIPPED an inherited token on a non-exclusive launch,
	// reasoning that the attestation would be "false" for such a job. That
	// deadlocked the holder against its own hold at depth three: H --exclusive →
	// N1 `aira confine -- make X` inherits the token and is admitted, but its
	// CHILD environment had the token removed, so N2 `aira confine -- pytest` from
	// that Makefile sent no token, was blocked by H's own hold, waited its full
	// max_wait, and H stalled on N2 while the slice stayed held against every
	// other session on the machine. CLAUDE.md's own "confine every heavy command"
	// rule makes that depth entirely ordinary (found by build review).
	//
	// The reasoning it replaces was also just wrong: a job running inside a live
	// hold IS running exclusively. And a STALE token is harmless by construction —
	// belongsToHolder matches only the LIVE holder's unique scope id, so a token
	// from a finished holder matches nothing and grants nothing.
	if request.Exclusive {
		cmd.Env = upsertConfineEnv(cmd.Env, ExclusiveHolderEnv, scopeID)
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
	// Placement is PROVEN here — the child started and was verified to be a member
	// of the scope — so this is the first instant at which "running" is a fact
	// rather than a guess. AIRA-22's detached status distinguishes `admitting`
	// from `running` on exactly this observation, because a live supervisor can
	// legitimately sit in the admission queue for hours and reporting that as
	// "running" would tell an operator something false. No error return: nothing
	// remains to abort.
	if request.OnPlaced != nil {
		request.OnPlaced(confineLaunchInfo(scopeID, sliceName, maximum))
	}
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
	exitCode, termination := waitConfineCommand(cmd)
	result.Exit = exitCode
	// Snapshot on the very next statement, and close the cut-off in the same
	// critical section: from here on the handler records nothing and tears
	// nothing down, so neither the verdict nor the counters readUsage is about
	// to read can be changed by a signal that terminated nothing.
	//
	// One window is irreducible and is stated rather than papered over: a signal
	// delivered between the child's own exit and this Lock -- a few instructions
	// -- is still recorded, so an operator's Ctrl-C landing in that instant reads
	// as `supervisor-signal:`. Closing it would need the wait and the signal to
	// share one select, i.e. a rewrite of the wait path, for a window no operator
	// can observe. Recorded as a deferral in the plan, section 6.
	supervisorSignalMu.Lock()
	terminatedBySignal := supervisorSignal
	runEnded = true
	supervisorSignalMu.Unlock()
	usage := deps.readUsage(scope.Reference())
	if usage.PeakRSS != nil && *usage.PeakRSS <= 0 {
		usage.PeakRSS = nil
	}
	result.Status.PeakRSS = usage.PeakRSS
	// AIRA-104: usage already carries CPUUser/CPUSys from this SAME read
	// (readCgroupUsage parses cpu.stat alongside memory.peak in one teardown
	// read) -- no second read, no new timing. Assigned directly, without
	// PeakRSS's <=0 clamp: a genuinely idle-but-scheduled subtree can read as
	// zero user or system usec, and that is a real observation, not a bad one.
	result.Status.CPUUser = usage.CPUUser
	result.Status.CPUSys = usage.CPUSys
	// The HIERARCHICAL counter, kept deliberately for reportPeak below: an OOM
	// anywhere in the job is a signal that the estimate was low, which is an
	// estimate-quality input rather than a claim made to an operator. AIRA-102
	// records the consequence rather than hiding it (plan L6): a container OOM at
	// a caller's small `--memory` marks this signature's history `oom`, and
	// EstimateMemoryReserve then bumps future estimates to headroom.
	oom := usage.OOMKill != nil && *usage.OOMKill > 0
	// The OPERATOR-facing attribution is a different question and is classified,
	// never assumed from the hierarchical counter (AIRA-102).
	oomAttribution := classifyConfineOOM(usage)
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
	result.Status.TerminatedBy = classifyConfineTermination(termination, usage, terminatedBySignal)
	// AIRA-101, finalised beside its sibling facet and on the same completion
	// path. A lease that closed mid-run downgrades granted -> lost, so a
	// measurement taken while the hold was gone is never reported as clean.
	if request.Exclusive && exclusiveLost.Load() {
		result.Status.Exclusive = ConfineExclusiveLost
	}
	_, _ = fmt.Fprintln(diagnostics, FormatConfineStatus(result.Status))
	// Only an OWN-limit OOM may reach the "job OOM-killed at its memory cap" line
	// (AIRA-102). Its other branch -- peak RSS near the cap -- is unaffected.
	if advisory := formatConfineReserveAdvisory(result.Status.ScopeMemoryMax, result.Status.PeakRSS, confineOwnCapAdviceWarranted(usage), result.Status.ScopeMemoryCapSource); advisory != "" {
		_, _ = fmt.Fprintln(diagnostics, advisory)
	}
	// The remaining attributions get their OWN line, deliberately NOT gated on
	// scopeMemoryMax: formatConfineReserveAdvisory returns "" for an uncapped
	// scope, so inheriting its gate would make a container OOM inside an uncapped
	// job silent again -- exactly the silence this classification exists to end.
	if advisory := formatConfineOOMAttributionAdvisory(result.Status.TerminatedBy, oomAttribution, usage); advisory != "" {
		_, _ = fmt.Fprintln(diagnostics, advisory)
	}
	// A separate line from the reserve advisory above, which stays cap-gated and
	// OOM-worded exactly as it was: this one speaks only for the verdict the
	// reserve advisory has never been able to see (AIRA-91 Part A).
	if advisory := formatConfineTerminationAdvisory(result.Status.TerminatedBy, usage); advisory != "" {
		_, _ = fmt.Fprintln(diagnostics, advisory)
	}
	if advisory := formatConfineOOMLimitAdvisory(result.Status.TerminatedBy, usage); advisory != "" {
		_, _ = fmt.Fprintln(diagnostics, advisory)
	}
	return result, nil
}

// confineOwnCapAdviceWarranted reports whether "job OOM-killed at its memory
// cap" is a claim this scope's counters actually support (AIRA-102).
//
// It fires only when the OOM killer demonstrably killed OUR processes, and is
// suppressed only when the counters POSITIVELY establish that an ancestor's
// limit fired instead (the AIRA-27 slice-collateral shape), where "raise your
// cap" is the wrong advice. An unreadable own-limit counter still warrants the
// advice: we know this job was OOM-killed, and withholding useful guidance over
// an unknown refinement would trade a false claim for an unhelpful silence.
//
// The case it newly EXCLUDES is the one this ticket is about: a DESCENDANT
// (a container at its own --memory) OOM-killed while nothing of ours died.
func confineOwnCapAdviceWarranted(usage cgroupUsage) bool {
	killed, evaluated := usage.LocalOOM()
	if !killed || !evaluated {
		return false
	}
	own, ownEvaluated := usage.OwnLimitOOM()
	// A POSITIVE own-limit declaration is required, not merely the absence of a
	// negative one (build review, Sol P1). An unreadable `memory.events.local`
	// `oom` counter cannot establish that OUR cap was the binding one, and the
	// phrase "OOM-killed at its memory cap <cap>" asserts exactly that. Such a
	// run is NOT left silent: formatConfineOOMAttributionAdvisory speaks for it
	// instead, with the same actionable advice minus the unsupported claim.
	return ownEvaluated && own
}

// formatConfineOOMAttributionAdvisory speaks for every OOM attribution EXCEPT
// own-limit, which formatConfineReserveAdvisory already owns and whose wording
// is unchanged. Each line states exactly what the counters establish and no
// more; none of them claims a cap value, so none is gated on having one.
//
// covers: AIRA-102
func formatConfineOOMAttributionAdvisory(verdict string, attribution ConfineOOMAttribution, usage cgroupUsage) string {
	// Deliberately silent where the existing advisories already speak, so an
	// operator never gets two overlapping explanations of one event:
	//   - the `oom` verdict is owned by formatConfineOOMLimitAdvisory, which
	//     already names own-limit versus ancestor and is already honestly silent
	//     when the local counter is unreadable;
	//   - the unattributed-SIGKILL verdict is owned by
	//     formatConfineTerminationAdvisory, which already notes a descendant OOM.
	// The gap they both leave -- and the one AIRA-102 makes routine -- is an OOM
	// BENEATH this scope on a job that ended any other way, which is exactly what a
	// container hitting its own --memory looks like: podman exits 0, so the job's
	// verdict is `normal` and every existing line stays quiet.
	// The `oom` verdict's own-limit and ancestor attributions are already owned
	// by formatConfineOOMLimitAdvisory, and the unattributed-SIGKILL verdict by
	// formatConfineTerminationAdvisory -- speaking here too would duplicate them.
	// The ONE exception is an `oom` verdict whose attribution is unestablished:
	// formatConfineOOMLimitAdvisory is deliberately silent there, and since
	// confineOwnCapAdviceWarranted now also declines that case, staying quiet
	// would leave a genuine OOM with no explanation at all.
	if verdict == ConfineTerminatedUnattributedSIGKILL {
		return ""
	}
	if verdict == ConfineTerminatedOOM && attribution != ConfineOOMUnestablished {
		return ""
	}
	kills := int64(0)
	if usage.OOMKill != nil {
		kills = *usage.OOMKill
	}
	switch attribution {
	case ConfineOOMDescendant:
		// Deliberately worded as what the counters establish. "this scope's own
		// limit did not fire" would be a stronger claim than the evidence
		// supports: a max-breach can be DECLARED while killing nothing.
		return fmt.Sprintf("confine: memory.events records %d OOM kill(s) beneath this scope (a descendant's own limit, e.g. a container's --memory); the OOM killer killed nothing belonging to this scope itself", kills)
	case ConfineOOMAncestor:
		return fmt.Sprintf("confine: %d OOM kill(s) took this job's processes, but this scope's own limit did not declare the breach — an ancestor's cap fired (e.g. the slice) and this job was the collateral; raising this job's own cap would not have prevented it", kills)
	case ConfineOOMUnestablished:
		return fmt.Sprintf("confine: %d OOM kill(s) occurred under this scope; whose limit fired could not be "+
			"established from this scope's local counters, so this was either this scope's own cap or an "+
			"ancestor's (the shared slice). If it was this job's own, raise --memory-max (or --memory-reserve); "+
			"if an ancestor's, the slice was over-committed and raising this job's cap will not help.", kills)
	default:
		return ""
	}
}

// formatConfineReserveAdvisory names the next step an OOM at this scope's own
// cap actually warrants. AIRA-133 made that step depend on capSource, because
// the generic "raise the cap" wording it used to emit was wrong for the common
// case: most confine jobs are killed at a cap AIRA ESTIMATED, and for those the
// correct action is to re-run the identical command — the OOM has by now been
// recorded against the command's own signature and the next admission is sized
// higher with no operator action at all. Telling an operator to raise a flag
// they never set sent a real session hunting a limit it could not find.
//
// The source is the recorded provenance of the cap that was written, never a
// guess: an unevaluated source gets the original source-agnostic wording, which
// names both possibilities rather than picking one.
func formatConfineReserveAdvisory(scopeMemoryMax int64, peakRSS *int64, oom bool, capSource string) string {
	if scopeMemoryMax <= 0 {
		return ""
	}
	peak := "unknown"
	if peakRSS != nil {
		peak = FormatConfineBytes(*peakRSS)
	}
	if oom {
		head := fmt.Sprintf("confine: job OOM-killed at its memory cap %s (peak RSS %s)", FormatConfineBytes(scopeMemoryMax), peak)
		switch capSource {
		case ConfineCapSourceMemoryMax:
			// The operator's own number. Re-running changes nothing here, and
			// saying so is the whole point: the auto case below tells the reader
			// to re-run, and an operator who applies that advice to a limit they
			// set themselves just repeats the kill.
			return head + "; cap-source=" + capSource + " — this cap is YOUR OWN --memory-max, not an AIRA estimate, " +
				"so re-running the identical command will not change it. Raise that flag, or split heavy work."
		case ConfineCapSourceMemoryReserve:
			return head + "; cap-source=" + capSource + " — this cap is YOUR OWN --memory-reserve, not an AIRA estimate, " +
				"so re-running the identical command will not change it. Raise that flag, or split heavy work."
		case ConfineCapSourceDaemonReserve:
			return head + "; cap-source=" + capSource + " — AIRA chose this cap from this command's peak-RSS history, you did not. " +
				"The kill has now been recorded against this command's signature, so RE-RUN THE IDENTICAL COMMAND and the next " +
				"admission is sized higher on its own. If an identical re-run is killed at the same cap again, that is a genuine " +
				"bug worth reporting. Pass --memory-reserve/--memory-max to skip the cycle."
		case ConfineCapSourceDelegateRAM:
			return head + "; cap-source=" + capSource + " — this is --delegate-ram's whole-scope ceiling, chosen by AIRA rather than " +
				"by you, and it climbs with this signature's recorded peaks: RE-RUN THE IDENTICAL COMMAND before changing anything. " +
				"Pass --memory-max to set the ceiling yourself."
		default:
			return head + "; cap-source=" + ConfineCapSourceUnevaluated + " — where this cap came from could not be established. " +
				"If you set --memory-max/--memory-reserve yourself, raise it; if AIRA estimated it, re-running the identical " +
				"command admits at a higher reserve. Either way, splitting heavy work also helps."
		}
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
		Exclusive:            request.Exclusive,
		// AIRA-101. A confine launched from INSIDE an exclusive job inherits that
		// job's holder token through the environment and forwards it here, so the
		// daemon exempts it from the hold it would otherwise deadlock against. An
		// absent, stale or foreign token matches nothing and changes nothing.
		ExclusiveHolder: InheritedExclusiveHolder(),
	})
}

// upsertConfineEnv sets key=value in env, replacing any existing entry so a
// child can never see two values for the same variable (the last would win in
// some readers and the first in others).
func upsertConfineEnv(env []string, key, value string) []string {
	result := removeConfineEnv(env, key)
	return append(result, key+"="+value)
}

// removeConfineEnv drops every entry for key.
func removeConfineEnv(env []string, key string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

// ExclusiveHolderEnv carries the scope id of the exclusive job a process is
// running inside (AIRA-101). It has two jobs, complementary rather than
// conflated:
//
//  1. The NESTING TOKEN. CLAUDE.md requires every heavy command be prefixed
//     `aira confine`, and a nested confine resolves to the SAME slice and creates
//     a SIBLING scope — which its own parent's exclusive hold would block,
//     deadlocking the benchmark against itself. The token exempts it.
//  2. The job-visible ATTESTATION, so a benchmark can confirm it really got
//     exclusivity before recording a number:
//     `[ -n "$AIRA_CONFINE_EXCLUSIVE" ] || exit 1`.
//
// Precisely: it attests ACQUISITION, not the whole run — an environment variable
// cannot be unset inside an already-running process if exclusivity is lost later.
// The confine trailer's `exclusive=` facet attests the run.
//
// It is deliberately NOT a member of pylib.coordinationEnvironmentKeys:
// StripCoordinationEnvironment runs on every nested launch, so listing it there
// would drop the token and make two-level nesting deadlock.
const ExclusiveHolderEnv = "AIRA_CONFINE_EXCLUSIVE"

// InheritedConfineScopeID reads the scope id of the confine job this process is
// running inside, as exported by AppendConfineChildEnvironment. It is what lets
// `aira confine-reserve` declare itself a sub-reservation of an already-running
// job rather than new work entering the slice.
//
// A malformed or absent value yields "": the daemon refuses a non-canonical
// scope id, and forwarding a stray one would fail an unrelated job's admission.
func InheritedConfineScopeID() string {
	scopeID := strings.TrimSpace(os.Getenv("AIRA_CONFINE_SCOPE_ID"))
	if scopeID == "" {
		return ""
	}
	if _, _, _, _, ok := parseConfineScopeID(scopeID); !ok {
		return ""
	}
	return scopeID
}

// InheritedConfineSlice reads the RESOLVED slice of the confine job this process
// is running inside, as exported by AppendConfineChildEnvironment under
// pylib.ConfineParentSliceEnv. It is the slice a reservation taken from inside
// the job must be charged to.
//
// AIRA-115: without it `aira confine-reserve` defaulted its slice to
// DefaultConfineSlice independently of the scope id it inherited, so a job
// confined to any other slice had its per-test sub-reservations charged against
// aira.slice — over-charging a slice that does not host the memory, while the
// slice that does host it under-counted.
//
// The key is pylib.ConfineParentSliceEnv and NOT AIRA_CONFINE_SLICE, which is
// load-bearing rather than cosmetic. AIRA_CONFINE_SLICE is the OPERATOR's
// explicit-slice input, consumed by ResolveConfineSlice under "an explicit value
// never falls back". A job that published its own resolved cgroup path there
// would hand every nested `aira confine` a forged operator override — bypassing
// default resolution's managed-unit guard and whale fallback, and reaching the
// daemon's management-slice resolution too. This function reads what AIRA
// EMITTED; ResolveConfineSlice reads what the operator DECLARED. Never the same
// variable.
//
// A value containing a ".." component yields "" rather than being forwarded:
// both slice resolvers reject one, so passing it on would only turn a stray
// environment variable into an unevaluated admission.
func InheritedConfineSlice() string {
	slice := strings.TrimSpace(os.Getenv(pylib.ConfineParentSliceEnv))
	if slice == "" || hasParentComponent(slice) {
		return ""
	}
	return slice
}

// InheritedExclusiveHolder reads the token this process inherited, if any.
//
// A malformed value is DISCARDED rather than forwarded: the daemon refuses a
// non-canonical scope id, which would turn somebody's stray environment variable
// into a launch failure for an unrelated job.
func InheritedExclusiveHolder() string {
	holder := strings.TrimSpace(os.Getenv(ExclusiveHolderEnv))
	if holder == "" {
		return ""
	}
	if _, _, _, _, ok := parseConfineScopeID(holder); !ok {
		return ""
	}
	return holder
}

// confineLaunchInfo carries RESOLVED facts to the AIRA-22 launch callbacks. The
// slice is the one actually resolved (which may be the whale.slice compatibility
// path rather than what the caller typed) and the cap is the effective ceiling
// read out of the cgroup ancestry, so a detached launcher reports what is true
// rather than what was requested.
func confineLaunchInfo(scopeID, sliceName string, capBytes int64) ConfineLaunchInfo {
	return ConfineLaunchInfo{ScopeID: scopeID, Slice: sliceName, CapBytes: capBytes, SupervisorPID: os.Getpid()}
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

// confineScopeID mints the scope directory name. The owner is appended after an
// '@' delimiter (AIRA-52) for the same reason the delegate-RAM marker lives here
// (see IsDelegateRAMScopeID): the cgroup directory name is the ONLY carrier that
// survives a daemon restart. Owner used to live exclusively on the in-memory
// admitWaiter, and the daemon's restart-adoption scan rebuilds aggregate reserve
// scalars from a live cgroup scan without recreating per-job waiters — so a job
// whose lifetime spanned a restart lost its owner permanently and degraded to
// "unknown", forcing an unnecessary --steal to kill your own job.
//
// '@' is unambiguous: neither a --name nor a caller-supplied owner may contain
// it (validateConfineName / ValidateConfineIdentity), and the only other '@' in
// the id is the fixed "@dr" marker immediately after the "CONFINE-" prefix,
// which parseConfineScopeID strips before looking for this delimiter. An
// INFERRED owner carries its own leading '@' (ConfineInferredOwnerPrefix) and
// survives verbatim, because the split takes everything after the first
// delimiter rather than splitting on every '@'.
func confineScopeID(name, owner string, delegateRAM bool) string {
	if name == "" {
		name = "job"
	}
	id := "CONFINE-"
	if delegateRAM {
		id += delegateRAMScopeIDMarker + "-"
	}
	id += name + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// An unknown owner is encoded as the ABSENCE of a suffix, never as
	// "@unknown": a reader must not be able to confuse "nobody claimed this" with
	// a claim, and an id minted before this change parses identically.
	if owner != "" && owner != ConfineUnknownOwner && ValidateConfineOwner(owner) == nil {
		id += "@" + owner
	}
	return id
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

// forwardConfineSignals returns a stop function that closes the handler down
// and then JOINS its goroutine. The join is load-bearing since AIRA-70 gave the
// handler an observable side effect -- it writes the "received SIGTERM" line to
// the shared diagnostics writer and records the witness the trailer classifies
// from. Without the join, a signal landing at the moment of return could write
// that line after confineWithDeps had already returned, i.e. after its caller
// (or a test) had read the diagnostics it produced.
//
// The join cannot deadlock and adds no new worst case: the loop either sits in
// the select (and returns the instant done closes) or is inside onSignal, whose
// cleanup() the caller's own `defer cleanup()` already waits on through its
// sync.Once. Callers stop the signal SOURCE before calling this, so no further
// signal can be delivered while the join is in progress.
//
// AIRA-121: the delivery step is a `deliver` FUNCTION rather than a
// `func() *os.Process` the forwarder signals itself. Shim mode signals the
// child's process GROUP (requirement 8), and expressing that as a mode branch
// INSIDE this loop would put the real path one careless edit away from
// regressing. One seam, two implementations (confineCommand.signal), no branch
// here at all.
func forwardConfineSignals(forward <-chan os.Signal, deliver func(os.Signal) error, onSignal func(os.Signal)) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case received, ok := <-forward:
				if !ok {
					return
				}
				// Go's select picks at random among ready cases, so a still-hot
				// signal channel could otherwise keep winning over a closed
				// `done` and stretch the join out indefinitely. Checking `done`
				// first bounds it: once stop has been called, at most this one
				// already-dequeued signal is handled.
				select {
				case <-done:
					return
				default:
				}
				if onSignal != nil {
					onSignal(received)
				}
				if deliver != nil {
					_ = deliver(received)
				}
			case <-done:
				return
			}
		}
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() { close(done) })
		<-finished
	}
}

// confineTermination is the decoded wait status, kept separate from the exit
// code because 128+signal is lossy in exactly the direction this fix cares
// about: it cannot distinguish a signalled child from one that chose to exit
// 137. Decoded is false when the wait status could not be established at all,
// which is unevaluated evidence, not evidence of a clean exit.
type confineTermination struct {
	Decoded  bool
	Signaled bool
	Signal   syscall.Signal
}

func waitConfineCommand(cmd *exec.Cmd) (int, confineTermination) {
	err := cmd.Wait()
	if err == nil {
		return 0, confineTermination{Decoded: true}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if wait, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if wait.Signaled() {
				return 128 + int(wait.Signal()), confineTermination{Decoded: true, Signaled: true, Signal: wait.Signal()}
			}
			return wait.ExitStatus(), confineTermination{Decoded: true}
		}
	}
	return 3, confineTermination{}
}

// classifyConfineTermination answers AIRA-70's question -- what killed this job?
// -- from the three pieces of evidence the supervisor actually holds, and
// refuses to answer further than they reach. The order is load-bearing and each
// step is justified in
// docs/superpowers/plans/2026-09-05-aira70-91a-terminated-by-trailer-plan.md
// section 3.2; the short form:
//
//  1. no decoded wait status                        -> unevaluated
//  2. SIGKILLed and LOCAL oom_kill readable and >0   -> oom
//  3. this supervisor was signalled                  -> supervisor-signal:<NAME>
//  4. not signalled                                  -> normal
//  5. signal is not SIGKILL                          -> child-signal:<NAME>
//  6. local oom_kill not readable                    -> unevaluated
//  7. SIGKILL with local oom_kill == 0               -> unattributed-sigkill
//
// Step 2 rests on TWO independent guards, both added by build review, because
// memcg events propagate UPWARD and this project's own aitest worker scopes are
// exactly the descendant cgroups that exploit that:
//
//   - It reads memory.events.LOCAL, not the hierarchical memory.events, via
//     usage.LocalOOM(). A worker sub-cgroup OOM-killed at its own cap raises the
//     hierarchical counter on this scope while this scope's own processes are
//     untouched; the local counters stay at zero for that. They rise when the
//     OOM killer actually killed something OF OURS -- including when the leader
//     has been drained into a `.aira-supervisor` sub-cgroup, which is why
//     LocalOOM is a disjunction over oom_kill and oom_group_kill rather than a
//     single counter. Measured, not assumed --
//     TestMemoryEventsLocalDistinguishesOwnLimitFromDescendantOOM pins every
//     shape against real cgroups.
//
//     Note the exact claim, which is narrower than it first reads: the verdict
//     says THE OOM KILLER KILLED THIS JOB. It deliberately does NOT say whose
//     limit fired. An aira.slice-level OOM caused by a NEIGHBOUR's usage reaches
//     this branch the same way our own cap would, because our memory.oom.group
//     is honoured for it and raises our local oom_group_kill (measured: our
//     local reads oom 0, oom_kill 0, oom_group_kill 1). That is still an honest
//     `oom` -- the kernel really did OOM-kill this job -- but "raise your cap"
//     would be the wrong conclusion to draw from it. Separating the two needs
//     the local `oom` declaration counter, which IS keyed on the cgroup whose
//     limit fired; it is a fidelity improvement rather than an honesty fix and
//     is deferred (plan section 6), not built here.
//
//   - It requires SIGKILL specifically. The OOM killer, and memory.oom.group
//     with it, deliver SIGKILL and nothing else, so a leader that died of
//     SIGTERM was not OOM-killed whatever any counter says.
//
// Step 2 precedes step 3 because cgroup.kill -- which is how our own cleanup()
// tears a job down -- never increments oom_kill, so a positive counter can
// never be our doing. Step 3 precedes step 4 because a child may CATCH the
// forwarded SIGTERM and exit non-signalled; the operator's Ctrl-C still ended
// it. Steps 5 and 6 precede step 7 so that a crashing child and an unreadable
// counter are never swept into AIRA-91's bucket.
//
// The HIERARCHICAL counter is deliberately left to formatConfineReserveAdvisory
// and the peak-RSS report, which ask a different question -- "did anything
// under this scope hit a memory wall" -- and are unchanged by this fix.
//
// supervisorSignal must be a snapshot taken immediately after the wait returned
// (see confineWithDeps): the handler stays live through the post-run teardown,
// and a signal arriving there terminated nothing.
//
// covers: AIRA-70, AIRA-91 Part A
func classifyConfineTermination(term confineTermination, usage cgroupUsage, supervisorSignal os.Signal) string {
	if !term.Decoded {
		return ConfineTerminatedUnevaluated
	}
	localOOM, oomEvaluated := usage.LocalOOM()
	killed := term.Signaled && term.Signal == syscall.SIGKILL
	if killed && localOOM {
		return ConfineTerminatedOOM
	}
	if supervisorSignal != nil {
		return ConfineTerminatedSupervisorSignalPrefix + confineSignalName(supervisorSignal)
	}
	if !term.Signaled {
		return ConfineTerminatedNormal
	}
	if !killed {
		return ConfineTerminatedChildSignalPrefix + confineSignalName(term.Signal)
	}
	if !oomEvaluated {
		return ConfineTerminatedUnevaluated
	}
	return ConfineTerminatedUnattributedSIGKILL
}

// confineSignalName renders the kernel's own mnemonic ("SIGTERM"). Note that
// syscall.Signal.String() gives the human phrase ("terminated"), not the
// mnemonic, which is why unix.SignalName is used instead. A signal number the
// table does not know falls back to its number rather than to an empty or
// invented label -- the facet must never read as though no signal was involved.
func confineSignalName(sig os.Signal) string {
	if sig == nil {
		return "unknown"
	}
	unixSignal, ok := sig.(syscall.Signal)
	if !ok {
		return sig.String()
	}
	if name := unix.SignalName(unixSignal); name != "" {
		return name
	}
	return "signal-" + strconv.Itoa(int(unixSignal))
}

// formatConfineTerminationAdvisory is the "name the realistic sources" half of
// AIRA-91 Part A. It fires only for the unattributed verdict, and it lists the
// candidate mechanisms AS candidates: the classifier positively established
// that the kill was a SIGKILL this supervisor did not send and that this scope's
// OWN memory.events.local records no OOM, and nothing beyond that.
//
// It names the LOCAL counter explicitly, and adds a clause when the hierarchical
// counter disagrees (build-review round 3, P1). Saying "this scope's
// memory.events records no OOM kill" would be false in exactly the shape this
// verdict exists to serve -- a worker sub-cgroup OOM raises the hierarchical
// counter -- and would contradict formatConfineReserveAdvisory, which reads that
// same hierarchical counter and may be printing an OOM line directly above.
func formatConfineTerminationAdvisory(verdict string, usage cgroupUsage) string {
	if verdict != ConfineTerminatedUnattributedSIGKILL {
		return ""
	}
	// The ancestor-cgroup OOM that earlier revisions listed here has been REMOVED
	// (build-review round 4): oom_kill is keyed on the VICTIM's cgroup, so an
	// ancestor OOM that killed our processes raises OUR local counter and lands
	// on the `oom` verdict, never here. Listing it sent the operator after a
	// cause this verdict has already ruled out.
	line := "confine: the job was SIGKILLed by something this supervisor cannot attribute: it sent no signal itself, " +
		"and this scope's own memory.events.local records no OOM kill. Candidates: an external whole-cgroup kill " +
		"(systemd-oomd under PSI pressure, another session's `aira confine --kill`, a direct cgroup.kill write), " +
		"an external kill -9, or the job killing itself."
	if usage.OOMKill != nil && *usage.OOMKill > 0 {
		line += " Note that memory.events (which counts this scope AND everything below it) does record " +
			strconv.FormatInt(*usage.OOMKill, 10) + " OOM kill(s): something in a cgroup BENEATH this scope was " +
			"OOM-killed at its own limit, which is a separate event from whatever killed this job."
	}
	return line
}

// formatConfineOOMLimitAdvisory names WHOSE limit fired, for the `oom` verdict
// only. The facet itself deliberately claims no more than "the OOM killer killed
// this job" (see classifyConfineTermination), because `oom_kill` is keyed on the
// VICTIM's cgroup: a slice-level OOM caused by a NEIGHBOUR's usage looks
// identical there. The local `oom` declaration counter is keyed on the cgroup
// whose limit actually fired, so it separates the two -- and the distinction is
// the difference between "raise your own cap" and "you were AIRA-27 collateral,
// your own cap was never the problem". Returns "" when the counter is
// unreadable: silence, not a guess.
func formatConfineOOMLimitAdvisory(verdict string, usage cgroupUsage) string {
	if verdict != ConfineTerminatedOOM {
		return ""
	}
	own, evaluated := usage.OwnLimitOOM()
	if !evaluated {
		return ""
	}
	if own {
		return "confine: the OOM fired at this scope's OWN memory limit."
	}
	return "confine: the OOM did NOT fire at this scope's own limit — an ancestor cgroup's limit " +
		"(the shared slice) was breached and this job's processes were the collateral. Raising this job's " +
		"own cap will not help; the slice was over-committed."
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
	// AIRA-121. In ci-shim mode there is no scope to verify: no memory.oom.group
	// was written and no finite memory.max exists anywhere in the ancestry, by
	// design. Gated on the durable install-mode RECORD, not on a flag the parent
	// could pass, so a forged-fd standalone invocation cannot use this to run a
	// heavy job in an oom.group-but-uncapped cgroup on a REAL install -- the check
	// this branch skips is the defence-in-depth mirror of confineWithDeps' own
	// finite-cap refusal, and it stays armed wherever that refusal is armed.
	//
	// Everything AFTER this point still runs: oom_score_adj, nice and ionice are
	// real kernel facts that work without cgroups, so the priorities facet stays a
	// genuine observation rather than being degraded along with the scope.
	shimSetup := ResolveConfineMode() == ConfineModeShim
	if verifyErr := verifyConfineSetupScope(); verifyErr != nil && !shimSetup {
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
