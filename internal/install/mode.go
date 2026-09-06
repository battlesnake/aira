package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aira/internal/daemon"
	"aira/internal/runner"
)

// AIRA-121. Install-mode resolution, the durable record, and the ci-shim
// install itself.

const (
	installStageBoth  = ""
	installStageBuild = "build"
	installStageStart = "start"
)

// shimDaemonReadyTimeout bounds the start stage's wait for the shim daemon's
// socket. It is deliberately its own constant and NOT waitDaemonReachable's
// budget: that function is implemented entirely through `systemctl --user show
// -p ActiveState/SubState/MainPID` and is therefore unusable here (AIRA-121 gate
// condition C4). Ten seconds rather than the real path's five because a cold
// container's first daemon start also opens and migrates the SQLite state.
const shimDaemonReadyTimeout = 10 * time.Second

// resolveInstallMode decides which install this invocation performs, and says
// how it decided.
//
//	--ci             unchanged AIRA-120 behaviour: a REAL install whose
//	                 MemoryMax comes from a MemAvailable snapshot. No probe.
//	                 Fails on a systemd-less box exactly as it does today.
//	--ci=shim        force the shim. The probe still runs and is RECORDED as
//	                 evidence; it just does not decide anything.
//	--ci=auto        run the probe: a reachable systemd user manager means the
//	                 --ci path, anything else means the shim.
//	(absent)         real.
//
// Bare --ci is deliberately NOT made auto-detecting. The ticket's rule is "never
// let two boxes running nominally the same install command end up silently
// different"; a bare flag that changes meaning with the host is precisely that
// shape, and it would also retroactively change a flag that landed three commits
// ago. --ci=auto is the opt-in to host-dependence, asked for by name.
//
// The decision reads exactly ONE field of the report, SystemdUserManager, and
// that is a fact about this codebase rather than a simplification: every step of
// the real install is downstream of a working systemd user manager -- the
// aira.slice unit, daemon-reload, enable --now on the anchor, Delegate=-derived
// memory delegation, and aira-daemon.service itself. The other probed facts are
// recorded because an operator needs them to understand the decision, and
// because two of them feed the budget resolution.
func resolveInstallMode(d installDeps, opts installOpts) (string, CapabilityReport, string, error) {
	switch opts.ciValue {
	case "shim":
		return runner.ConfineModeShim, ProbeCapability(d), "--ci=shim", nil
	case "auto":
		report := ProbeCapability(d)
		if report.SystemdUserManager == systemdReachable {
			return runner.ConfineModeReal, report, "--ci=auto (probe: systemd user manager reachable)", nil
		}
		return runner.ConfineModeShim, report, "--ci=auto (probe: systemd user manager " + report.SystemdUserManager + ")", nil
	case "":
		if opts.ci {
			return runner.ConfineModeReal, CapabilityReport{}, "--ci", nil
		}
		return runner.ConfineModeReal, CapabilityReport{}, "default", nil
	}
	return "", CapabilityReport{}, "", argumentInvalid("--ci must be given bare, or as --ci=shim or --ci=auto")
}

// resolveShimBudget resolves the container's RAM budget for the advisory ledger
// (requirement 4, and gate condition C8), in this precedence:
//
//  1. DECLARED -- an operator typed --memory-max. On the real path the slice's
//     MemoryMax IS the admission ceiling the daemon reads, so --memory-max
//     meaning "the ledger ceiling" in shim mode is the SAME meaning, not a
//     second one. It is first because it is the only source that can be right
//     when a node runs several containers with no per-container memory.max --
//     GCP Batch with taskCountPerNode > 1 is exactly that, and there the
//     MemTotal fallback below over-books the node by the task count.
//
//  2. The container's OWN cgroup memory.max, when readable and finite. This is
//     "what am I actually allowed": the number the container runtime will
//     OOM-kill against. Preferred over AIRA-120's MemAvailable because
//     /proc/meminfo is NOT namespaced on the runtimes in question (GCP Batch,
//     AWS Batch, Fargate, k8s, plain containerd) unless something like lxcfs is
//     interposed -- inside a 32 GiB container on a 256 GiB host, MemAvailable
//     reports the HOST's free memory, and booking against that over-books the
//     container by an order of magnitude.
//
//  3. /proc/meminfo MemTotal. Recorded as the distinctly weaker source it is.
//
//  4. Nothing readable -> the install FAILS. A silently ungated shim must never
//     exist: failing here is one loud failure in one place, versus a per-job
//     wedge later.
//
// AIRA-120 uses MemAvailable for the real slice because that slice must coexist
// with the rest of a shared desktop -- a free-RAM snapshot is a politeness
// bound. A batch container is single-tenant, so the ceiling is the container's
// limit and CURRENT usage is subtracted at admission time by the existing
// checkedAvailable rather than baked into the ceiling. Same primitive, different
// environment, different correct answer.
//
// ALL THREE budget sources are floored at minimumCeilingGiB, mirroring the real
// --ci path's own resolveCIMemoryMax refusal (round-3 review finding F4, plus
// AIRA-130 for the MemTotal source round 4 left un-floored). The daemon's
// admission headroom is 2GiB base plus 64MiB per queued job
// (internal/daemon/admit.go); checkedAvailable answers a bare 0 whenever a
// budget is at or below roughly that headroom, so a budget below the floor does
// not merely admit smaller jobs, it admits NOTHING, ever, for the container's
// whole life -- an entirely ordinary CI/k8s-Job container size away. Refusing it
// here is one loud failure in one place, exactly like the "nothing readable"
// case just above, rather than a silent per-job wedge discovered later. The
// MemTotal source is no exception: a 2GiB-class host (e2-small, t3.small) with
// an unbounded container memory.max reaches it and produces exactly the same
// permanently wedged ledger, and the real --ci path floors ITS host-wide source
// (MemAvailable) identically.
func resolveShimBudget(d installDeps, opts installOpts, report CapabilityReport) (int64, string, string, error) {
	if opts.memoryMax != "" {
		bytes, err := sizeBytes(opts.memoryMax)
		if err != nil || bytes <= 0 {
			return 0, "", "", argumentInvalid(fmt.Sprintf("--memory-max %q is not a usable size for the ci-shim ledger budget", opts.memoryMax))
		}
		if bytes < int64(minimumCeilingGiB)<<30 {
			return 0, "", "", unavailable(fmt.Errorf(
				"--ci=shim: --memory-max %s is below the %dG ci-shim ledger floor; the daemon's admission headroom (2GiB base + 64MiB per job) would leave nothing to grant and every job would wedge at E_ADMIT_TOO_LARGE forever -- pass a larger --memory-max",
				formatCeilingBytes(bytes), minimumCeilingGiB))
		}
		return bytes, runner.ShimBudgetSourceDeclared, ownCgroupDir(d.cgroupRoot, report.OwnCgroupPath), nil
	}
	if value := strings.TrimSpace(report.OwnCgroupMemoryMax); value != "" && value != "max" && !strings.HasPrefix(value, systemdUnevaluated) {
		if bytes, err := strconv.ParseInt(value, 10, 64); err == nil && bytes > 0 {
			if bytes < int64(minimumCeilingGiB)<<30 {
				return 0, "", "", unavailable(fmt.Errorf(
					"ci-shim: this container's own cgroup memory.max is %s, below the %dG ci-shim ledger floor; the daemon's admission headroom (2GiB base + 64MiB per job) would leave nothing to grant and every job would wedge at E_ADMIT_TOO_LARGE forever -- this container's own cgroup sizing may not be yours to change, so pass an explicit --memory-max instead",
					formatCeilingBytes(bytes), minimumCeilingGiB))
			}
			return bytes, runner.ShimBudgetSourceCgroupMemoryMax, ownCgroupDir(d.cgroupRoot, report.OwnCgroupPath), nil
		}
	}
	if report.MemTotalBytes > 0 {
		if report.MemTotalBytes < int64(minimumCeilingGiB)<<30 {
			return 0, "", "", unavailable(fmt.Errorf(
				"ci-shim: /proc/meminfo MemTotal is %s, below the %dG ci-shim ledger floor; the daemon's admission headroom (2GiB base + 64MiB per job) would leave nothing to grant and every job would wedge at E_ADMIT_TOO_LARGE forever -- this host is smaller than the floor, so pass an explicit --memory-max only if this container really does have that much to spend",
				formatCeilingBytes(report.MemTotalBytes), minimumCeilingGiB))
		}
		return report.MemTotalBytes, runner.ShimBudgetSourceMemTotal, "", nil
	}
	return 0, "", "", unavailable(errors.New(
		"ci-shim mode needs a RAM budget for its advisory ledger and could establish none: this container declares no readable memory.max and /proc/meminfo has no usable MemTotal; pass --memory-max <N>G to declare one"))
}

func capabilityMap(report CapabilityReport) map[string]string {
	return map[string]string{
		"systemd_user_manager":  report.SystemdUserManager,
		"cgroup_v2_unified":     report.CgroupV2Unified,
		"own_cgroup_path":       report.OwnCgroupPath,
		"own_cgroup_memory_max": report.OwnCgroupMemoryMax,
		"mem_total_bytes":       strconv.FormatInt(report.MemTotalBytes, 10),
	}
}

// runShimInstall is the whole ci-shim install: two stages, neither of which ever
// touches systemd, /etc, loginctl, or a cgroup it did not read.
//
// AIRA-121 gate condition C3 -- the ROOT case, which is the docker default.
// runInstall's ordinary euid==0 branch goes to runRootInstall, which requires a
// SUDO_USER identity, a /run/user/<uid> session directory owned by that user,
// /etc drop-ins and loginctl enable-linger. Every one of those fails inside a
// `docker build` RUN layer and in a root-running Batch container. So shim mode
// is decided BEFORE that branch (see runInstall) and installs directly for the
// CURRENT user's own HOME with no session check, no drop-ins and no linger --
// which is correct rather than merely expedient: there is no second user to
// install on behalf of, and nothing here needs privilege at all.
func runShimInstall(d installDeps, opts installOpts, report CapabilityReport, resolvedBy string) error {
	home, err := installHome(d)
	if err != nil {
		return err
	}
	uid := d.geteuid()
	paths, err := d.daemonPaths()
	if err != nil {
		return unavailable(fmt.Errorf("resolve daemon paths: %w", err))
	}
	recordPath := runner.InstallModePathFor(paths.StateHome)

	if opts.stage != installStageStart {
		budget, source, cgroupPath, budgetErr := resolveShimBudget(d, opts, report)
		if budgetErr != nil {
			return budgetErr
		}
		record := runner.InstallModeRecord{
			Schema: 1, Mode: runner.ConfineModeShim,
			RecordedAt: d.now().UTC().Format(time.RFC3339), Home: home, UID: uid,
			ResolvedBy: resolvedBy, Capability: capabilityMap(report),
			ShimBudgetBytes: budget, ShimBudgetSource: source, ShimCgroupPath: cgroupPath,
		}
		reportShimMode(d, record)
		if opts.dryRun {
			d.logf("planned: write %s (build stage: places bytes only)", recordPath)
			d.logf("planned: start `aira daemon serve` detached, with no inherited pipe and no wait (start stage)")
			return nil
		}
		if err := runner.WriteInstallModeRecord(recordPath, record); err != nil {
			return unavailable(fmt.Errorf("record install mode: %w", err))
		}
		d.logf("%s: written", recordPath)
		if opts.stage == installStageBuild {
			d.logf("build stage complete: nothing was started, no systemd was contacted, no /etc file was written")
			return nil
		}
	}

	// --- start stage -------------------------------------------------------
	record, ok := runner.ReadInstallModeRecord(recordPath)
	if !ok {
		return unavailable(fmt.Errorf("no usable install plan at %s; run `aira install --ci=shim --stage=build` first", recordPath))
	}
	if record.Mode != runner.ConfineModeShim {
		return unavailable(fmt.Errorf("recorded install mode is %q, not ci-shim; the start stage must never re-decide a mode the build stage resolved", record.Mode))
	}
	// A start stage must never RE-RESOLVE the mode or the budget the build stage
	// recorded: that is the "two boxes silently different" failure with the two
	// boxes being the same box at two points in time. It may only refuse when the
	// environment it is starting in is not the one the plan was written for.
	if record.Home != home || record.UID != uid {
		return unavailable(fmt.Errorf("recorded install plan is for home=%s uid=%d, but this process has home=%s uid=%d",
			record.Home, record.UID, home, uid))
	}
	if opts.dryRun {
		d.logf("planned: start `aira daemon serve` detached for %s (uid %d)", record.Home, record.UID)
		return nil
	}
	if opts.stage == installStageStart {
		reportShimMode(d, record)
	}
	executable, err := d.executable()
	if err != nil {
		return unavailable(fmt.Errorf("resolve running executable: %w", err))
	}
	if executable, err = d.abs(executable); err != nil {
		return unavailable(fmt.Errorf("make executable path absolute: %w", err))
	}
	if status := d.daemonStatus(paths); status.Running {
		d.logf("aira daemon: already running; leaving it alone")
	} else if err := d.spawnShimDaemon(shimDaemonSpec{
		executable: executable, stateHome: paths.StateHome, record: record,
		environ: shimDaemonEnvironment(d, record),
	}); err != nil {
		return unavailable(fmt.Errorf("start ci-shim daemon: %w", err))
	}
	if err := waitShimDaemonReachable(d, paths); err != nil {
		return unavailable(err)
	}
	d.logf("aira daemon: running (ci-shim); advisory admission ledger active")
	return nil
}

func reportShimMode(d installDeps, record runner.InstallModeRecord) {
	d.logf("install mode: ci-shim (resolved by %s; systemd user manager: %s)",
		record.ResolvedBy, record.Capability["systemd_user_manager"])
	// formatCeilingBytes already appends "(N bytes)"; printing the count a second
	// time here rendered "4.00GiB (4294967296 bytes) (4294967296 bytes)".
	d.logf("shim ledger budget: %s from %s",
		formatCeilingBytes(record.ShimBudgetBytes),
		runner.DescribeShimBudgetSource(record.ShimBudgetSource))
	d.logf("containment: advisory — no cgroup scope is created and no kill backstop exists; a job that exceeds its booked reserve is NOT killed")
}

type shimDaemonSpec struct {
	executable string
	stateHome  string
	record     runner.InstallModeRecord
	environ    []string
}

// shimDaemonEnvironment builds the daemon child's environment.
//
// The shim coordinates are transcribed here as a convenience and a test seam
// ONLY. The daemon reads the install-mode record itself (AIRA-121 gate condition
// C5), because two other launch paths exist that this function never touches --
// cmd/aira's dispatcher spawns `/proc/self/exe daemon` whenever a daemon-routed
// verb finds no socket, and an operator can run `aira daemon serve` by hand --
// and either would otherwise produce a REAL-mode daemon in a shim-installed
// home, against which a shim client's admission silently falls through to its
// flock fallback and the job LAUNCHES UNGATED.
//
// AIRA_DAEMON_MANAGED stops runDaemonCommand deferring to a systemd service that
// does not exist. The three subsystem modes are forced off here as well as in
// Serve, so the intent is visible in `ps` and in the unit-less process's own
// environment rather than only in code.
func shimDaemonEnvironment(d installDeps, record runner.InstallModeRecord) []string {
	env := []string{
		"HOME=" + record.Home,
		"PATH=" + firstNonEmpty(d.getenv("PATH"), "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"),
		"AIRA_DAEMON_MANAGED=1",
		"AIRA_DAEMON_CONFINE_MODE=" + runner.ConfineModeShim,
		"AIRA_DAEMON_SHIM_BUDGET_BYTES=" + strconv.FormatInt(record.ShimBudgetBytes, 10),
		"AIRA_DAEMON_SHIM_BUDGET_SOURCE=" + record.ShimBudgetSource,
		"AIRA_DAEMON_SHIM_CGROUP_PATH=" + record.ShimCgroupPath,
		"AIRA_DAEMON_WATCHDOG_MODE=off",
		"AIRA_DAEMON_SLICE_CEILING_MODE=off",
		"AIRA_DAEMON_OOM_STEER_MODE=off",
	}
	for _, name := range []string{"XDG_STATE_HOME", "XDG_RUNTIME_DIR", "AIRA_INSTALL_MODE_FILE"} {
		if value := strings.TrimSpace(d.getenv(name)); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// spawnShimDaemonProcess starts the shim daemon so that NOTHING waits on it
// (requirement 9, test i).
//
// Each line is load-bearing:
//
//   - Setsid puts it in a new session, so an entrypoint that signals its own
//     process group does not take the daemon with it, and the daemon is never
//     inside a workload's process group.
//   - stdin is /dev/null and stdout/stderr are an append-opened LOG FILE, never
//     an inherited pipe. A backgrounded process holding the write end of the
//     entrypoint's stdout pipe is the classic reason `docker run` appears to
//     hang after the workload exits: the pipe never reaches EOF. Because both
//     are *os.File, os/exec passes the descriptors straight through and starts
//     no copying goroutine of its own.
//   - Process.Release() and NO Wait(). The installer exits immediately and the
//     daemon is reparented to the container's init. Reaping it when PID 1 exits
//     is init's job and is explicitly out of this ticket's scope; not being
//     waited on while the container runs is in scope, and is exactly what these
//     three properties give.
func spawnShimDaemonProcess(spec shimDaemonSpec) error {
	logPath := filepath.Join(spec.stateHome, "aira", "shim-daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(spec.executable, "daemon", "serve")
	cmd.Env = append([]string(nil), spec.environ...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// waitShimDaemonReachable is the SOCKET-based readiness check (AIRA-121 gate
// condition C4).
//
// waitDaemonReachable cannot be reused: verifyDaemonReachable is implemented
// entirely through `systemctl --user show -p ActiveState/SubState/MainPID`, and
// there is no systemd here. This waits on the daemon's own lock and socket
// through daemon.Status, which is the same evidence every client uses.
//
// It waits on the SOCKET, never on the process. A container whose ledger never
// came up should fail loudly at start rather than silently run every job
// ungated, so this returns an error and the start stage exits non-zero.
func waitShimDaemonReachable(d installDeps, paths daemon.Paths) error {
	deadline := d.now().Add(shimDaemonReadyTimeout)
	for {
		if status := d.daemonStatus(paths); status.Running && status.Ready {
			return nil
		}
		if !d.now().Before(deadline) {
			return fmt.Errorf("ci-shim daemon did not become reachable on %s within %s; see %s",
				paths.SocketPath, shimDaemonReadyTimeout, filepath.Join(paths.StateHome, "aira", "shim-daemon.log"))
		}
		d.sleep(25 * time.Millisecond)
	}
}

// writeRealInstallModeRecord records a REAL install with the same schema the
// shim uses, so that "which mode is this box in" has exactly one answer and one
// place to read it, whichever mode was installed.
func writeRealInstallModeRecord(d installDeps, paths daemon.Paths, home string, uid int, resolvedBy string) error {
	record := runner.InstallModeRecord{
		Schema: 1, Mode: runner.ConfineModeReal,
		RecordedAt: d.now().UTC().Format(time.RFC3339), Home: home, UID: uid, ResolvedBy: resolvedBy,
	}
	if err := runner.WriteInstallModeRecord(runner.InstallModePathFor(paths.StateHome), record); err != nil {
		return unavailable(fmt.Errorf("record install mode: %w", err))
	}
	return nil
}

// reportInstallMode prints the recorded install mode for `aira install --status`.
// An absent record is reported as exactly that -- "unevaluated (no record)" --
// and never as "real", because a box installed before AIRA-121 has no record and
// a box whose record could not be read is a different fact from either.
func reportInstallMode(d installDeps) {
	paths, err := d.daemonPaths()
	if err != nil {
		d.logf("install mode: unevaluated (resolve daemon paths: %v)", err)
		return
	}
	recordPath := runner.InstallModePathFor(paths.StateHome)
	record, ok := runner.ReadInstallModeRecord(recordPath)
	if !ok {
		if _, statErr := d.stat(recordPath); statErr != nil {
			d.logf("install mode: unevaluated (no record at %s; this box predates AIRA-121 or was never installed) — clients therefore take the real-slice path", recordPath)
			return
		}
		d.logf("install mode: unevaluated (record at %s is unreadable or malformed) — clients therefore take the real-slice path", recordPath)
		return
	}
	if record.Mode != runner.ConfineModeShim {
		d.logf("install mode: real-slice (resolved by %s, recorded %s)", record.ResolvedBy, record.RecordedAt)
		d.logf("containment: enforced — per-job cgroup scope under aira.slice")
		return
	}
	reportShimMode(d, record)
	d.logf("ci-shim residual: a daemon restart forgets the in-memory ledger and can over-admit until in-flight jobs finish; there are no cgroup scopes to reconstruct it from")
}
