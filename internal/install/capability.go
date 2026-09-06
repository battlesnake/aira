package install

import (
	"path/filepath"
	"strconv"
	"strings"
)

// CapabilityReport records the facts an install-mode decision is made from,
// SEPARATELY. Each field is an established fact or an explicit "unevaluated:
// <reason>"; none is a blended boolean, because the report is printed to an
// operator and recorded durably in install-mode.json, and a blob cannot be
// argued with later.
type CapabilityReport struct {
	// SystemdUserManager is "reachable" | "absent" | "unreachable: <reason>" |
	// "unevaluated: <reason>". It is THE decisive field (see resolveInstallMode)
	// and the only one that is.
	SystemdUserManager string
	CgroupV2Unified    string
	OwnCgroupPath      string
	OwnCgroupMemoryMax string
	MemTotalBytes      int64
}

// The four values SystemdUserManager can take, as prefixes.
const (
	systemdReachable   = "reachable"
	systemdAbsent      = "absent"
	systemdUnreachable = "unreachable: "
	systemdUnevaluated = "unevaluated: "
)

// systemdRunningStates are the answers `systemctl --user is-system-running`
// gives when a user manager ANSWERED. Every one of them means "reachable",
// including the failing ones: `degraded` exits 1 and `starting` exits 1, and
// both are a live manager. The decision is therefore made on the ANSWER, never
// on the exit status.
//
// AIRA-121 gate condition C7: exit status cannot carry this distinction at all,
// because timeoutArgv wraps the probe as `timeout 10s systemctl ...`. A missing
// systemctl surfaces as `timeout` exiting 127, not as an exec lookup error, and
// in a distroless image `timeout` itself is missing. Absence is therefore
// established BEFORE the run, through the lookPath seam, and the run's own
// result only ever distinguishes reachable from unreachable.
var systemdRunningStates = map[string]bool{
	"running": true, "degraded": true, "starting": true, "initializing": true,
	"maintenance": true, "stopping": true, "offline": true,
}

// ProbeCapability answers, without writing anything anywhere, whether this box
// can run the real install.
//
// Deliberately NOT probed: an actual cgroup mkdir. A write probe would be the
// only conclusive test of delegation, but it is intrusive (it creates a
// directory in someone else's cgroup tree), it is redundant behind the systemd
// gate, and it would have to be undone on a path where the undo can fail. So
// cgroup.controllers is READ and recorded; nothing is written. Accepted
// limitation: a box with a working systemd user manager but broken delegation
// still fails the REAL install, loudly, exactly as it does today -- the shim
// does not exist to paper over that.
func ProbeCapability(d installDeps) CapabilityReport {
	report := CapabilityReport{}
	report.SystemdUserManager = probeSystemdUserManager(d)
	report.CgroupV2Unified, report.OwnCgroupPath, report.OwnCgroupMemoryMax = probeOwnCgroup(d)
	if meminfo, err := d.readFile("/proc/meminfo"); err == nil {
		report.MemTotalBytes = parseMemTotalKB(meminfo) * 1024
	}
	return report
}

func probeSystemdUserManager(d installDeps) string {
	// The two lookups run BEFORE the command, and in this order, so each absence
	// is reported as itself rather than as whatever exit status the wrapper
	// happens to produce.
	if _, err := d.lookPath("timeout"); err != nil {
		return systemdUnevaluated + "timeout(1) is absent, so the systemd probe cannot be bounded and is not run (a hung D-Bus must never hang a docker build)"
	}
	if _, err := d.lookPath("systemctl"); err != nil {
		return systemdAbsent
	}
	output, err := d.run(timeoutArgv("systemctl", "--user", "is-system-running"), nil)
	answer := strings.TrimSpace(string(output))
	if systemdRunningStates[answer] {
		return systemdReachable
	}
	if err != nil {
		detail := err.Error()
		if answer != "" {
			detail = answer + " (" + detail + ")"
		}
		return systemdUnreachable + detail
	}
	if answer == "" {
		return systemdUnreachable + "systemctl --user is-system-running produced no answer"
	}
	return systemdUnreachable + "systemctl --user is-system-running answered " + strconv.Quote(answer)
}

// probeOwnCgroup reads this process's own cgroup and its memory.max. Read-only
// throughout. The memory.max value feeds the shim ledger budget (requirement 4);
// the controller list is evidence only.
func probeOwnCgroup(d installDeps) (unified, ownPath, ownMax string) {
	root := d.cgroupRoot
	if root == "" {
		root = cgroupRoot
	}
	controllers, err := d.readFile(filepath.Join(root, "cgroup.controllers"))
	switch {
	case err != nil:
		unified = "absent"
	case strings.TrimSpace(string(controllers)) == "":
		unified = "present"
	default:
		unified = "present"
	}
	self, err := d.readFile("/proc/self/cgroup")
	if err != nil {
		return unified, "", systemdUnevaluated + "read /proc/self/cgroup: " + err.Error()
	}
	relative := parseUnifiedCgroupLine(string(self))
	if relative == "" {
		return unified, "", systemdUnevaluated + "/proc/self/cgroup has no unified (0::) entry"
	}
	ownPath = relative
	data, err := d.readFile(filepath.Join(root, strings.TrimPrefix(relative, "/"), "memory.max"))
	if err != nil {
		return unified, ownPath, systemdUnevaluated + "read memory.max: " + err.Error()
	}
	return unified, ownPath, strings.TrimSpace(string(data))
}

// parseUnifiedCgroupLine returns the cgroup-v2 relative path from
// /proc/self/cgroup, i.e. the part after "0::". Empty when there is no unified
// entry (a cgroup-v1-only host).
func parseUnifiedCgroupLine(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "0::") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			if path == "" {
				return "/"
			}
			return path
		}
	}
	return ""
}

// ownCgroupDir turns a recorded /proc/self/cgroup relative path into the
// absolute cgroupfs directory to read. Under cgroupns=private -- the normal
// container case -- the relative path is "/" and this is the mount root itself.
func ownCgroupDir(root, relative string) string {
	if root == "" {
		root = cgroupRoot
	}
	relative = strings.TrimSpace(relative)
	if relative == "" || relative == "/" {
		return root
	}
	return filepath.Join(root, strings.TrimPrefix(relative, "/"))
}
