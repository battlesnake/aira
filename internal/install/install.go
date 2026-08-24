package install

import (
	"bufio"
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aira/internal/daemon"
	"golang.org/x/sys/unix"
)

const (
	CodeUnavailable     = "E_INSTALL_UNAVAILABLE"
	CodeArgumentInvalid = "E_INSTALL_ARGUMENT_INVALID"
	CodeDelegation      = "E_INSTALL_DELEGATION"
	CodeOvercommit      = "E_INSTALL_OVERCOMMIT"

	defaultSliceUnit  = "aira.slice"
	defaultAnchorUnit = "aira-slice-keepalive.service"
	defaultDaemonUnit = daemon.DefaultServiceUnit
	gibPerKiB         = int64(1024 * 1024)
	cgroupRoot        = "/sys/fs/cgroup"
)

var (
	memorySizeRE          = regexp.MustCompile(`^[0-9]+G$`)
	installedMemoryMaxRE  = regexp.MustCompile(`(?m)^MemoryMax=(.*)$`)
	installedMemoryHighRE = regexp.MustCompile(`(?m)^MemoryHigh=(.*)$`)
)

//go:embed assets
var assets embed.FS

type installOpts struct {
	memoryMax        string
	memoryHigh       string
	allowOvercommit  bool
	dryRun           bool
	status           bool
	watchdog         string
	watchdogInterval time.Duration
}

// installDeps is intentionally exhaustive: install-time identity, process,
// filesystem, descriptor, clock, and output effects all cross this seam.
type installDeps struct {
	geteuid      func() int
	getenv       func(string) string
	executable   func() (string, error)
	abs          func(string) (string, error)
	getpid       func() int
	now          func() time.Time
	run          func([]string, []byte) ([]byte, error)
	stat         func(string) (os.FileInfo, error)
	lstat        func(string) (os.FileInfo, error)
	readFile     func(string) ([]byte, error)
	writeFile    func(string, []byte, os.FileMode) error
	mkdirAll     func(string, os.FileMode) error
	mkdirTemp    func(string, string) (string, error)
	remove       func(string) error
	rename       func(string, string) error
	openat       func(int, string, int, uint32) (int, error)
	fstat        func(int, *unix.Stat_t) error
	close        func(int) error
	readFD       func(int) ([]byte, error)
	writeFD      func(int, []byte) error
	fsync        func(int) error
	fchmod       func(int, uint32) error
	renameat     func(int, string, int, string) error
	unlinkat     func(int, string, int) error
	flock        func(int, int) error
	logf         func(string, ...any)
	daemonPaths  func() (daemon.Paths, error)
	daemonStatus func(daemon.Paths) daemon.StatusInfo
	daemonStop   func(daemon.Paths) error
	sleep        func(time.Duration)

	sliceUnit  string
	anchorUnit string
	daemonUnit string
	// daemonRuntimeDir is test-only: production inherits XDG_RUNTIME_DIR from
	// the user manager; real-systemd tests bake an isolated runtime explicitly.
	daemonRuntimeDir string
	cgroupRoot       string
}

func realInstallDeps() installDeps {
	return installDeps{
		geteuid:    os.Geteuid,
		getenv:     os.Getenv,
		executable: os.Executable,
		abs:        filepath.Abs,
		getpid:     os.Getpid,
		now:        time.Now,
		run: func(argv []string, stdin []byte) ([]byte, error) {
			if len(argv) == 0 {
				return nil, errors.New("empty command")
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			if stdin != nil {
				cmd.Stdin = bytes.NewReader(stdin)
			}
			cmd.Stderr = os.Stderr
			return cmd.Output()
		},
		stat: os.Stat, lstat: os.Lstat, readFile: os.ReadFile, writeFile: os.WriteFile,
		mkdirAll: os.MkdirAll, mkdirTemp: os.MkdirTemp, remove: os.Remove, rename: os.Rename,
		openat: unix.Openat, fstat: unix.Fstat, close: unix.Close,
		readFD: func(fd int) ([]byte, error) {
			var out []byte
			buf := make([]byte, 4096)
			for {
				n, err := unix.Read(fd, buf)
				if n > 0 {
					out = append(out, buf[:n]...)
				}
				if err != nil {
					return nil, err
				}
				if n == 0 {
					return out, nil
				}
			}
		},
		writeFD: func(fd int, data []byte) error {
			for len(data) > 0 {
				n, err := unix.Write(fd, data)
				if err != nil {
					return err
				}
				if n == 0 {
					return io.ErrShortWrite
				}
				data = data[n:]
			}
			return nil
		},
		fsync: unix.Fsync, fchmod: unix.Fchmod, renameat: unix.Renameat,
		unlinkat: unix.Unlinkat, flock: unix.Flock,
		logf:        func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
		daemonPaths: daemon.PathsFromEnv, daemonStatus: daemon.Status, daemonStop: daemon.Stop, sleep: time.Sleep,
		sliceUnit: defaultSliceUnit, anchorUnit: defaultAnchorUnit, daemonUnit: defaultDaemonUnit, cgroupRoot: cgroupRoot,
	}
}

// Run is the standalone CLI face. It performs no project discovery and does
// not contact the AIRA daemon.
func Run(args []string, stdout io.Writer) error {
	opts, err := parseInstallArgs(args)
	if err != nil {
		return err
	}
	d := realInstallDeps()
	d.logf = func(format string, values ...any) { _, _ = fmt.Fprintf(stdout, format+"\n", values...) }
	if opts.status {
		return runStatus(d)
	}
	return runInstall(d, opts)
}

// RunSliceAnchor blocks until systemd asks the mandatory delegation anchor to
// stop. It deliberately owns no work beyond remaining a memory-accounted member.
func RunSliceAnchor() int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	signal.Stop(signals)
	return 0
}

func parseInstallArgs(args []string) (installOpts, error) {
	opts := installOpts{watchdog: "observe", watchdogInterval: 2 * time.Second}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !strings.HasPrefix(arg, "--") || name == "" {
			return opts, argumentInvalid(fmt.Sprintf("unexpected argument %q", arg))
		}
		if seen[name] {
			return opts, argumentInvalid(fmt.Sprintf("option --%s may occur once", name))
		}
		seen[name] = true
		switch name {
		case "allow-overcommit", "dry-run", "status":
			if hasValue {
				return opts, argumentInvalid(fmt.Sprintf("option --%s does not take a value", name))
			}
			switch name {
			case "allow-overcommit":
				opts.allowOvercommit = true
			case "dry-run":
				opts.dryRun = true
			case "status":
				opts.status = true
			}
		case "memory-max", "memory-high", "watchdog", "watchdog-interval":
			if !hasValue {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
					return opts, argumentInvalid(fmt.Sprintf("option --%s requires a value", name))
				}
				i++
				value = args[i]
			}
			if value == "" {
				return opts, argumentInvalid(fmt.Sprintf("option --%s requires a value", name))
			}
			if name == "memory-max" {
				opts.memoryMax = value
			} else if name == "memory-high" {
				opts.memoryHigh = value
			} else if name == "watchdog" {
				if value != "off" && value != "observe" && value != "enforce" {
					return opts, argumentInvalid("--watchdog must be off, observe, or enforce")
				}
				opts.watchdog = value
			} else {
				interval, parseErr := time.ParseDuration(value)
				if parseErr != nil || interval < time.Second || interval >= 30*time.Second {
					return opts, argumentInvalid("--watchdog-interval must be a Go duration in [1s,30s)")
				}
				opts.watchdogInterval = interval
			}
		default:
			return opts, argumentInvalid(fmt.Sprintf("unknown option --%s", name))
		}
	}
	if opts.status && (opts.memoryMax != "" || opts.memoryHigh != "" || opts.allowOvercommit || opts.dryRun || seen["watchdog"] || seen["watchdog-interval"]) {
		return opts, argumentInvalid("--status cannot be combined with mutation options")
	}
	return opts, nil
}

func runInstall(d installDeps, opts installOpts) error {
	d = fillInstallDeps(d)
	if opts.watchdog == "" {
		opts.watchdog = "observe"
	}
	if opts.watchdogInterval == 0 {
		opts.watchdogInterval = 2 * time.Second
	}
	home, err := installHome(d)
	if err != nil {
		return err
	}
	uid := d.geteuid()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	installed, whaleContent := []byte(nil), []byte(nil)
	daemonPresent := false
	if existingFD, present, openErr := openExistingUnitDirectory(d, unitDir, uid); openErr != nil {
		return unavailable(openErr)
	} else if present {
		var readErr error
		installed, _, readErr = readRegularUnitAt(d, existingFD, uid, d.sliceUnit, true)
		if readErr == nil {
			_, _, readErr = readRegularUnitAt(d, existingFD, uid, d.anchorUnit, true)
		}
		if readErr == nil {
			_, daemonPresent, readErr = readRegularUnitAt(d, existingFD, uid, d.daemonUnit, true)
		}
		if readErr == nil {
			whaleContent, _, readErr = readRegularUnitAt(d, existingFD, uid, "whale.slice", false)
		}
		_ = d.close(existingFD)
		if readErr != nil {
			return unavailable(readErr)
		}
	}

	meminfo, memErr := d.readFile("/proc/meminfo")
	// The ⅔-MemTotal default is only consulted when no --memory-max was given and
	// no value is already on disk. In that case an unreadable OR malformed
	// (MemTotal-less) meminfo is an environment failure, not a bad user argument:
	// surface it as E_INSTALL_UNAVAILABLE rather than letting a 0 MemTotal fall
	// through to a "0G" cap that fails the floor and is misreported as argument-invalid.
	if opts.memoryMax == "" && parseInstalledValue(string(installed), installedMemoryMaxRE) == "" {
		if memErr != nil {
			return unavailable(fmt.Errorf("read /proc/meminfo for default memory limit: %w", memErr))
		}
		if parseMemTotalKB(meminfo) == 0 {
			return unavailable(errors.New("/proc/meminfo has no usable MemTotal for the default memory limit"))
		}
	}
	maximum, high, err := computeMemoryLimits(opts.memoryMax, opts.memoryHigh, string(installed), parseMemTotalKB(meminfo))
	if err != nil {
		return argumentInvalid(err.Error())
	}

	if err := requireUserMemoryDelegation(d, uid); err != nil {
		return err
	}
	executable, err := d.executable()
	if err != nil {
		return unavailable(fmt.Errorf("resolve running executable: %w", err))
	}
	executable, err = d.abs(executable)
	if err != nil {
		return unavailable(fmt.Errorf("make executable path absolute: %w", err))
	}

	whaleCapped := cappedWhaleContent(whaleContent)
	recorded := overcommitRecorded(installed)
	if whaleCapped && !opts.allowOvercommit && !recorded {
		return overcommitError()
	}
	accepted := opts.allowOvercommit || recorded
	slice, anchor, err := renderUnits(d.sliceUnit, d.anchorUnit, executable, maximum, high, accepted)
	if err != nil {
		return argumentInvalid(err.Error())
	}
	paths, err := d.daemonPaths()
	if err != nil {
		return unavailable(fmt.Errorf("resolve daemon paths: %w", err))
	}
	daemonUnit, err := renderDaemonUnit(d.daemonUnit, executable, paths.StateHome, opts.watchdog, opts.watchdogInterval, d.daemonRuntimeDir)
	if err != nil {
		return argumentInvalid(err.Error())
	}

	if opts.dryRun {
		d.logf("--- %s ---\n%s", d.sliceUnit, strings.TrimSuffix(slice, "\n"))
		d.logf("--- %s ---\n%s", d.anchorUnit, strings.TrimSuffix(anchor, "\n"))
		d.logf("--- %s ---\n%s", d.daemonUnit, strings.TrimSuffix(daemonUnit, "\n"))
		d.logf("planned: create %s; atomically update managed units", unitDir)
		d.logf("planned: systemctl --user daemon-reload")
		d.logf("planned: systemctl --user enable --now %s", d.anchorUnit)
		d.logf("planned: enable and verify +memory delegation in %s", d.sliceUnit)
		d.logf("planned: stop incumbent daemon and wait; publish and enable --now %s", d.daemonUnit)
		d.logf("planned: loginctl enable-linger %d; verify active/running MainPID equals daemon lock PID", uid)
		return nil
	}

	if err := d.mkdirAll(unitDir, 0o755); err != nil {
		return unavailable(fmt.Errorf("create user unit directory: %w", err))
	}
	dirfd, err := d.openat(unix.AT_FDCWD, unitDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return unavailable(fmt.Errorf("open user unit directory: %w", err))
	}
	defer d.close(dirfd)
	if err := validateDirectoryFD(d, dirfd, uid); err != nil {
		return unavailable(err)
	}
	lockfd, err := d.openat(dirfd, ".aira-install.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return unavailable(fmt.Errorf("open install lock: %w", err))
	}
	defer d.close(lockfd)
	if err := validateRegularFD(d, lockfd, uid, ".aira-install.lock"); err != nil {
		return unavailable(err)
	}
	if err := d.flock(lockfd, unix.LOCK_EX); err != nil {
		return unavailable(fmt.Errorf("lock install: %w", err))
	}

	// Re-read beneath the lock, through O_NOFOLLOW descriptors, before replacing.
	sliceChanged, err := publishManagedUnit(d, dirfd, uid, d.sliceUnit, []byte(slice))
	if err != nil {
		return unavailable(err)
	}
	anchorChanged, err := publishManagedUnit(d, dirfd, uid, d.anchorUnit, []byte(anchor))
	if err != nil {
		return unavailable(err)
	}
	if sliceChanged {
		d.logf("%s: updated", d.sliceUnit)
	} else {
		d.logf("%s: up to date", d.sliceUnit)
	}
	if anchorChanged {
		d.logf("%s: updated", d.anchorUnit)
	} else {
		d.logf("%s: up to date", d.anchorUnit)
	}

	// Always reload. Content equality must never make a prior failed reload permanent.
	if _, err := d.run([]string{"systemctl", "--user", "daemon-reload"}, nil); err != nil {
		return unavailable(fmt.Errorf("systemctl --user daemon-reload: %w", err))
	}
	if _, err := d.run([]string{"systemctl", "--user", "enable", "--now", d.anchorUnit}, nil); err != nil {
		return unavailable(fmt.Errorf("start mandatory anchor %s: %w", d.anchorUnit, err))
	}
	if err := verifyActive(d, d.sliceUnit); err != nil {
		return unavailable(err)
	}
	if err := verifyActive(d, d.anchorUnit); err != nil {
		return unavailable(err)
	}
	sliceCgroup, err := controlGroupPath(d, d.sliceUnit)
	if err != nil {
		return unavailable(err)
	}
	if err := enableMemoryDelegation(d, sliceCgroup); err != nil {
		return unavailable(fmt.Errorf("delegate memory in %s: %w", d.sliceUnit, err))
	}
	if err := verifyLiveLimits(d, sliceCgroup, maximum, high); err != nil {
		return unavailable(err)
	}
	anchorCgroup, err := controlGroupPath(d, d.anchorUnit)
	if err != nil {
		return unavailable(err)
	}
	if _, err := d.readFile(filepath.Join(anchorCgroup, "memory.max")); err != nil {
		return unavailable(fmt.Errorf("anchor child lacks delegated memory controller: %w", err))
	}
	if err := verifyDaemonSocketIdentity(d, paths); err != nil {
		return unavailable(err)
	}
	// Displace an incumbent ONLY on a first install (no managed unit yet): the only
	// daemon to displace predates the install (a client-forked, watchdog-off stray).
	// On a re-install the managed unit already exists and — because clients defer to
	// the service — any running daemon IS the managed service; stopping it would
	// bounce the live machine daemon (drop the admission queue, reset the watchdog
	// latch, interrupt the reconciler) on a no-op convergence run. So skip the whole
	// stop+wait when daemonPresent.
	if !daemonPresent {
		if status := d.daemonStatus(paths); status.Running {
			if err := d.daemonStop(paths); err != nil {
				// A racing shutdown can make Status say running immediately before
				// Stop observes no holder. That is the intended end state; only fail
				// if a holder remains after the stop error.
				if d.daemonStatus(paths).Running {
					return unavailable(fmt.Errorf("stop incumbent daemon: %w", err))
				}
			}
		}
		stopDeadline := d.now().Add(12 * time.Second)
		for d.daemonStatus(paths).Running {
			if !d.now().Before(stopDeadline) {
				return unavailable(errors.New("incumbent daemon did not stop before deadline"))
			}
			d.sleep(25 * time.Millisecond)
		}
	}
	daemonChanged, err := publishManagedUnit(d, dirfd, uid, d.daemonUnit, []byte(daemonUnit))
	if err != nil {
		return unavailable(err)
	}
	if daemonChanged {
		d.logf("%s: updated", d.daemonUnit)
	} else {
		d.logf("%s: up to date", d.daemonUnit)
	}
	if _, err := d.run([]string{"systemctl", "--user", "daemon-reload"}, nil); err != nil {
		return unavailable(fmt.Errorf("reload daemon unit: %w", err))
	}
	if _, err := d.run([]string{"systemctl", "--user", "enable", "--now", d.daemonUnit}, nil); err != nil {
		return unavailable(fmt.Errorf("enable and start %s: %w", d.daemonUnit, err))
	}
	// Restart only when a pre-existing managed service's unit content actually
	// changed (e.g. a changed --watchdog mode) — `enable --now` above does NOT pick
	// up a changed Environment on an already-running service, but a byte-identical
	// convergence re-run must NOT bounce the live daemon. On a first install
	// (!daemonPresent) `enable --now` already started it fresh with the new content.
	if daemonPresent && daemonChanged {
		if _, err := d.run([]string{"systemctl", "--user", "restart", d.daemonUnit}, nil); err != nil {
			return unavailable(fmt.Errorf("restart %s to apply changed unit: %w", d.daemonUnit, err))
		}
	}
	if _, err := d.run([]string{"loginctl", "enable-linger", strconv.Itoa(uid)}, nil); err != nil {
		d.logf("warning: loginctl enable-linger %d failed; daemon persistence is limited to active login sessions: %v", uid, err)
	}
	if err := waitDaemonReachable(d, paths); err != nil {
		return unavailable(err)
	}
	if accepted && whaleCapped {
		d.logf("overcommit: accepted and recorded (capped whale.slice coexists)")
	} else if accepted {
		d.logf("overcommit opt-in: recorded (no capped whale.slice detected)")
	}
	d.logf("installed: %s MemoryMax=%s MemoryHigh=%s; anchor active; memory delegated; %s active, running, and MainPID-tied", d.sliceUnit, maximum, high, d.daemonUnit)
	return nil
}

func fillInstallDeps(d installDeps) installDeps {
	defaults := realInstallDeps()
	dv, sv := reflectInstallDeps(&d), reflectInstallDeps(&defaults)
	for i := 0; i < dv.NumField(); i++ {
		if dv.Field(i).Kind() == reflect.Func && dv.Field(i).IsNil() {
			dv.Field(i).Set(sv.Field(i))
		}
	}
	if d.sliceUnit == "" {
		d.sliceUnit = defaultSliceUnit
	}
	if d.anchorUnit == "" {
		d.anchorUnit = defaultAnchorUnit
	}
	if d.daemonUnit == "" {
		d.daemonUnit = defaultDaemonUnit
	}
	if d.cgroupRoot == "" {
		d.cgroupRoot = cgroupRoot
	}
	return d
}

func reflectInstallDeps(d *installDeps) reflect.Value { return reflect.ValueOf(d).Elem() }

func installHome(d installDeps) (string, error) {
	home := strings.TrimSpace(d.getenv("HOME"))
	if home == "" {
		return "", unavailable(errors.New("HOME is unset; cannot locate the user unit directory"))
	}
	absolute, err := d.abs(home)
	if err != nil {
		return "", unavailable(fmt.Errorf("resolve HOME: %w", err))
	}
	return absolute, nil
}

func computeMemoryLimits(maximumArg, highArg, installed string, memTotalKB int64) (string, string, error) {
	maximum := maximumArg
	if maximum == "" {
		maximum = parseInstalledValue(installed, installedMemoryMaxRE)
	}
	if maximum == "" {
		gib := float64(memTotalKB) / float64(gibPerKiB)
		maximum = fmt.Sprintf("%dG", int64(math.Floor(gib*2/3)))
	}
	maxN, err := validateSize(maximum, "MemoryMax", true)
	if err != nil {
		return "", "", err
	}
	high := highArg
	if high == "" {
		high = fmt.Sprintf("%dG", maxN-2)
	}
	highN, err := validateSize(high, "MemoryHigh", false)
	if err != nil {
		return "", "", err
	}
	if highN > maxN {
		return "", "", fmt.Errorf("MemoryHigh %s exceeds MemoryMax %s", high, maximum)
	}
	return maximum, high, nil
}

func validateSize(value, label string, floor bool) (int64, error) {
	if !memorySizeRE.MatchString(value) {
		return 0, fmt.Errorf("%s must match ^[0-9]+G$, got %q", label, value)
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(value, "G"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", label, value, err)
	}
	if floor && n < 4 {
		return 0, fmt.Errorf("%s %s is below the 4G floor", label, value)
	}
	return n, nil
}

func sizeBytes(value string) (int64, error) {
	n, err := validateSize(value, "memory size", false)
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt64>>30 {
		return 0, errors.New("memory size overflows int64")
	}
	return n << 30, nil
}

func parseInstalledValue(content string, expression *regexp.Regexp) string {
	match := expression.FindStringSubmatch(content)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func parseInstalledMemoryMax(content string) (string, bool) {
	value := parseInstalledValue(content, installedMemoryMaxRE)
	return value, value != ""
}

func parseInstalledMemoryHigh(content string) (string, bool) {
	value := parseInstalledValue(content, installedMemoryHighRE)
	return value, value != ""
}

func parseMemTotalKB(data []byte) int64 {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value
		}
	}
	return 0
}

func renderUnits(sliceUnit, anchorUnit, executable, maximum, high string, accepted bool) (string, string, error) {
	if strings.ContainsAny(sliceUnit+anchorUnit, "\r\n/") {
		return "", "", errors.New("unit names contain an invalid character")
	}
	if !strings.HasSuffix(sliceUnit, ".slice") || !strings.HasSuffix(anchorUnit, ".service") {
		return "", "", errors.New("unit names must end in .slice and .service")
	}
	if strings.ContainsAny(executable, "\r\n") || !filepath.IsAbs(executable) {
		return "", "", errors.New("anchor executable must be an absolute single-line path")
	}
	sliceTemplate, err := assets.ReadFile("assets/aira.slice.in")
	if err != nil {
		return "", "", err
	}
	anchorTemplate, err := assets.ReadFile("assets/aira-slice-keepalive.service.in")
	if err != nil {
		return "", "", err
	}
	acceptance := "no"
	if accepted {
		acceptance = "yes"
	}
	slice := string(sliceTemplate)
	slice = strings.ReplaceAll(slice, "@SLICEUNIT@", sliceUnit)
	slice = strings.ReplaceAll(slice, "@MEMMAX@", maximum)
	slice = strings.ReplaceAll(slice, "@MEMHIGH@", high)
	slice = strings.ReplaceAll(slice, "@OVERCOMMIT@", acceptance)
	anchor := string(anchorTemplate)
	anchor = strings.ReplaceAll(anchor, "@ANCHORUNIT@", anchorUnit)
	anchor = strings.ReplaceAll(anchor, "@SLICEUNIT@", sliceUnit)
	anchor = strings.ReplaceAll(anchor, "@AIRABIN@", systemdExecPath(executable))
	return slice, anchor, nil
}

func renderDaemonUnit(unit, executable, stateHome, watchdog string, interval time.Duration, runtimeDir string) (string, error) {
	if strings.ContainsAny(unit, "\r\n/") || !strings.HasSuffix(unit, ".service") {
		return "", errors.New("daemon unit name must end in .service and contain no path separators")
	}
	if strings.ContainsAny(executable+stateHome+runtimeDir, "\r\n") || !filepath.IsAbs(executable) || !filepath.IsAbs(stateHome) {
		return "", errors.New("daemon executable and state home must be absolute single-line paths")
	}
	if watchdog != "off" && watchdog != "observe" && watchdog != "enforce" {
		return "", errors.New("watchdog mode must be off, observe, or enforce")
	}
	if interval < time.Second || interval >= 30*time.Second {
		return "", errors.New("watchdog interval must be in [1s,30s)")
	}
	template, err := assets.ReadFile("assets/aira-daemon.service.in")
	if err != nil {
		return "", err
	}
	runtimeEnvironment := ""
	if runtimeDir != "" {
		if !filepath.IsAbs(runtimeDir) {
			return "", errors.New("daemon runtime directory must be absolute")
		}
		runtimeEnvironment = "Environment=" + systemdEnvironmentAssignment("XDG_RUNTIME_DIR", runtimeDir) + "\n"
	}
	unitContent := string(template)
	replacements := map[string]string{
		"@DAEMONUNIT@": unit, "@AIRABIN@": systemdExecPath(executable), "@STATEHOME@": systemdExecPath(stateHome),
		"@WATCHDOG_MODE@": watchdog, "@WATCHDOG_INTERVAL@": interval.String(), "@RUNTIME_ENV@": runtimeEnvironment,
	}
	for placeholder, value := range replacements {
		unitContent = strings.ReplaceAll(unitContent, placeholder, value)
	}
	return unitContent, nil
}

func systemdEnvironmentAssignment(name, value string) string {
	assignment := name + "=" + value
	if !strings.ContainsAny(assignment, " \t\\\"") {
		return assignment
	}
	return strconv.Quote(assignment)
}

func systemdExecPath(path string) string {
	if !strings.ContainsAny(path, " \t\\\"") {
		return path
	}
	return strconv.Quote(path)
}

func marker(unit string) string { return "# aira-managed: " + unit }

func hasMarker(content []byte, unit string) bool {
	line := content
	if index := bytes.IndexByte(line, '\n'); index >= 0 {
		line = line[:index]
	}
	return string(line) == marker(unit)
}

func overcommitRecorded(content []byte) bool {
	return bytes.Contains(content, []byte("\n# aira-overcommit-accepted: yes\n"))
}

func cappedWhaleOnDisk(d installDeps, path string) (bool, error) {
	info, err := d.lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	content, err := d.readFile(path)
	if err != nil {
		return false, err
	}
	return cappedWhaleContent(content), nil
}

func cappedWhaleContent(content []byte) bool {
	value := strings.TrimSpace(parseInstalledValue(string(content), installedMemoryMaxRE))
	return value != "" && value != "max" && value != "infinity"
}

func requireUserMemoryDelegation(d installDeps, uid int) error {
	path := filepath.Join(d.cgroupRoot, "user.slice", fmt.Sprintf("user-%d.slice", uid), fmt.Sprintf("user@%d.service", uid), "cgroup.controllers")
	data, err := d.readFile(path)
	if err != nil {
		return delegationError(fmt.Errorf("cannot read %s: %w", path, err))
	}
	if !hasToken(data, "memory") {
		return delegationError(errors.New("memory controller not delegated to your user manager; enable it — e.g. run `agentmux install`, or add a `Delegate=yes` drop-in on user@.service — and re-login, then re-run"))
	}
	return nil
}

func hasToken(data []byte, wanted string) bool {
	for _, token := range strings.Fields(string(data)) {
		if token == wanted {
			return true
		}
	}
	return false
}

func inspectExistingPath(d installDeps, path string, uid int, unit string, requireMarker bool) ([]byte, error) {
	info, err := d.lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlink unit %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular unit %s", path)
	}
	if owner, ok := fileOwner(info); !ok || owner != uid {
		return nil, fmt.Errorf("refusing unit not owned by uid %d: %s", uid, path)
	}
	content, err := d.readFile(path)
	if err != nil {
		return nil, err
	}
	if requireMarker && !hasMarker(content, unit) {
		return nil, fmt.Errorf("refusing foreign marker-less unit %s", path)
	}
	return content, nil
}

func fileOwner(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

func validateDirectoryFD(d installDeps, fd, uid int) error {
	var stat unix.Stat_t
	if err := d.fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat user unit directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("user unit path is not a directory")
	}
	if int(stat.Uid) != uid {
		return fmt.Errorf("user unit directory is owned by uid %d, want %d", stat.Uid, uid)
	}
	return nil
}

func validateRegularFD(d installDeps, fd, uid int, name string) error {
	var stat unix.Stat_t
	if err := d.fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("refusing non-regular %s", name)
	}
	if int(stat.Uid) != uid {
		return fmt.Errorf("refusing %s owned by uid %d, want %d", name, stat.Uid, uid)
	}
	return nil
}

func openExistingUnitDirectory(d installDeps, unitDir string, uid int) (int, bool, error) {
	fd, err := d.openat(unix.AT_FDCWD, unitDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return -1, false, nil
	}
	if err != nil {
		return -1, false, fmt.Errorf("open user unit directory: %w", err)
	}
	if err := validateDirectoryFD(d, fd, uid); err != nil {
		_ = d.close(fd)
		return -1, false, err
	}
	return fd, true, nil
}

func readRegularUnitAt(d installDeps, dirfd, uid int, unit string, requireMarker bool) ([]byte, bool, error) {
	// O_NONBLOCK so a planted FIFO/device at a unit name returns a fd immediately
	// (then rejected by validateRegularFD) instead of blocking open() forever;
	// O_NOFOLLOW rejects symlinks but not FIFOs. Harmless on a regular file.
	fd, err := d.openat(dirfd, unit, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open %s without following links: %w", unit, err)
	}
	defer d.close(fd)
	if err := validateRegularFD(d, fd, uid, unit); err != nil {
		return nil, false, err
	}
	content, err := d.readFD(fd)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", unit, err)
	}
	if requireMarker && !hasMarker(content, unit) {
		return nil, false, fmt.Errorf("refusing foreign marker-less unit %s", unit)
	}
	return content, true, nil
}

func readManagedUnitAt(d installDeps, dirfd, uid int, unit string) ([]byte, bool, error) {
	return readRegularUnitAt(d, dirfd, uid, unit, true)
}

func publishManagedUnit(d installDeps, dirfd, uid int, unit string, wanted []byte) (bool, error) {
	existing, present, err := readManagedUnitAt(d, dirfd, uid, unit)
	if err != nil {
		return false, err
	}
	if present && bytes.Equal(existing, wanted) {
		return false, nil
	}
	temp := fmt.Sprintf(".%s.tmp-%d-%d", unit, d.getpid(), d.now().UnixNano())
	fd, err := d.openat(dirfd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return false, fmt.Errorf("create temporary %s: %w", unit, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = d.unlinkat(dirfd, temp, 0)
		}
	}()
	closed := false
	defer func() {
		if !closed {
			_ = d.close(fd)
		}
	}()
	if err := validateRegularFD(d, fd, uid, temp); err != nil {
		return false, err
	}
	if err := d.fchmod(fd, 0o644); err != nil {
		return false, fmt.Errorf("chmod temporary %s: %w", unit, err)
	}
	if err := d.writeFD(fd, wanted); err != nil {
		return false, fmt.Errorf("write temporary %s: %w", unit, err)
	}
	if err := d.fsync(fd); err != nil {
		return false, fmt.Errorf("fsync temporary %s: %w", unit, err)
	}
	if err := d.close(fd); err != nil {
		return false, fmt.Errorf("close temporary %s: %w", unit, err)
	}
	closed = true
	if err := d.renameat(dirfd, temp, dirfd, unit); err != nil {
		return false, fmt.Errorf("atomically publish %s: %w", unit, err)
	}
	cleanup = false
	if err := d.fsync(dirfd); err != nil {
		return false, fmt.Errorf("fsync user unit directory after %s: %w", unit, err)
	}
	return true, nil
}

func verifyActive(d installDeps, unit string) error {
	out, err := d.run([]string{"systemctl", "--user", "is-active", unit}, nil)
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		if err == nil {
			err = fmt.Errorf("state is %q", strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("%s is not active: %w", unit, err)
	}
	return nil
}

func verifyDaemonSocketIdentity(d installDeps, invoking daemon.Paths) error {
	runtimeDir := d.daemonRuntimeDir
	if runtimeDir == "" {
		out, err := d.run([]string{"systemctl", "--user", "show-environment"}, nil)
		if err != nil {
			return fmt.Errorf("read user-manager environment for daemon SocketPath: %w", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "XDG_RUNTIME_DIR=") {
				runtimeDir = strings.TrimPrefix(line, "XDG_RUNTIME_DIR=")
				break
			}
		}
	}
	if runtimeDir == "" {
		return errors.New("user-manager XDG_RUNTIME_DIR is unavailable; cannot verify daemon SocketPath identity")
	}
	home, err := installHome(d)
	if err != nil {
		return err
	}
	servicePaths, err := daemon.PathsFromEnvironment(invoking.StateHome, runtimeDir, home)
	if err != nil {
		return fmt.Errorf("resolve baked daemon SocketPath: %w", err)
	}
	if servicePaths.SocketPath != invoking.SocketPath {
		return fmt.Errorf("daemon SocketPath divergence: invoking client resolves %s but the user service resolves %s", invoking.SocketPath, servicePaths.SocketPath)
	}
	return nil
}

func verifyDaemonReachable(d installDeps, paths daemon.Paths) error {
	property := func(name string) (string, error) {
		out, err := d.run([]string{"systemctl", "--user", "show", "-p", name, "--value", d.daemonUnit}, nil)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	active, err := property("ActiveState")
	if err != nil || active != "active" {
		return fmt.Errorf("%s ActiveState=%q (want active): %v", d.daemonUnit, active, err)
	}
	sub, err := property("SubState")
	if err != nil || sub != "running" {
		return fmt.Errorf("%s SubState=%q (want running): %v", d.daemonUnit, sub, err)
	}
	mainValue, err := property("MainPID")
	mainPID, parseErr := strconv.Atoi(mainValue)
	if err != nil || parseErr != nil || mainPID <= 0 {
		return fmt.Errorf("%s MainPID=%q is unreadable: command=%v parse=%v", d.daemonUnit, mainValue, err, parseErr)
	}
	status := d.daemonStatus(paths)
	if !status.Running || !status.Ready || status.Lock.PID <= 0 {
		return fmt.Errorf("%s MainPID=%d but daemon status is not running and ready (lock PID=%d)", d.daemonUnit, mainPID, status.Lock.PID)
	}
	if mainPID != status.Lock.PID {
		return fmt.Errorf("%s MainPID=%d does not match daemon lock-holder PID=%d", d.daemonUnit, mainPID, status.Lock.PID)
	}
	return nil
}

func waitDaemonReachable(d installDeps, paths daemon.Paths) error {
	deadline := d.now().Add(5 * time.Second)
	var last error
	for {
		last = verifyDaemonReachable(d, paths)
		if last == nil {
			return nil
		}
		if !d.now().Before(deadline) {
			return last
		}
		d.sleep(25 * time.Millisecond)
	}
}

func controlGroupPath(d installDeps, unit string) (string, error) {
	out, err := d.run([]string{"systemctl", "--user", "show", "-p", "ControlGroup", "--value", unit}, nil)
	if err != nil {
		return "", fmt.Errorf("read %s ControlGroup: %w", unit, err)
	}
	relative := strings.TrimSpace(string(out))
	if relative == "" || !filepath.IsAbs(relative) || strings.Contains(relative, "..") {
		return "", fmt.Errorf("%s has invalid ControlGroup %q", unit, relative)
	}
	return filepath.Join(d.cgroupRoot, filepath.Clean(relative)), nil
}

func enableMemoryDelegation(d installDeps, parent string) error {
	controllers, err := d.readFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read cgroup.controllers: %w", err)
	}
	if !hasToken(controllers, "memory") {
		return errors.New("memory is absent from cgroup.controllers")
	}
	subtreePath := filepath.Join(parent, "cgroup.subtree_control")
	before, err := d.readFile(subtreePath)
	if err != nil {
		return fmt.Errorf("read cgroup.subtree_control: %w", err)
	}
	if !hasToken(before, "memory") {
		if err := d.writeFile(subtreePath, []byte("+memory\n"), 0); err != nil {
			return fmt.Errorf("write +memory to cgroup.subtree_control: %w", err)
		}
	}
	after, err := d.readFile(subtreePath)
	if err != nil {
		return fmt.Errorf("verify cgroup.subtree_control: %w", err)
	}
	if !hasToken(after, "memory") {
		return errors.New("memory missing from cgroup.subtree_control after enable")
	}
	return nil
}

func verifyLiveLimits(d installDeps, path, maximum, high string) error {
	wantMax, err := sizeBytes(maximum)
	if err != nil {
		return err
	}
	wantHigh, err := sizeBytes(high)
	if err != nil {
		return err
	}
	for _, item := range []struct {
		name string
		want int64
	}{{"memory.max", wantMax}, {"memory.high", wantHigh}} {
		data, readErr := d.readFile(filepath.Join(path, item.name))
		if readErr != nil {
			return fmt.Errorf("read live %s: %w", item.name, readErr)
		}
		got, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if parseErr != nil || got != item.want {
			return fmt.Errorf("live %s=%q, declared=%d", item.name, strings.TrimSpace(string(data)), item.want)
		}
	}
	return nil
}

func runStatus(d installDeps) error {
	d = fillInstallDeps(d)
	home, err := installHome(d)
	if err != nil {
		return err
	}
	uid := d.geteuid()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	var content, anchorContent, daemonContent, whaleContent []byte
	var unitErr, anchorErr, daemonErr, whaleErr error
	if dirfd, present, openErr := openExistingUnitDirectory(d, unitDir, uid); openErr != nil {
		unitErr, anchorErr, daemonErr, whaleErr = openErr, openErr, openErr, openErr
	} else if !present {
		unitErr, anchorErr, daemonErr, whaleErr = fs.ErrNotExist, fs.ErrNotExist, fs.ErrNotExist, fs.ErrNotExist
	} else {
		var slicePresent, anchorPresent, daemonPresent, whalePresent bool
		content, slicePresent, unitErr = readRegularUnitAt(d, dirfd, uid, d.sliceUnit, true)
		anchorContent, anchorPresent, anchorErr = readRegularUnitAt(d, dirfd, uid, d.anchorUnit, true)
		daemonContent, daemonPresent, daemonErr = readRegularUnitAt(d, dirfd, uid, d.daemonUnit, true)
		whaleContent, whalePresent, whaleErr = readRegularUnitAt(d, dirfd, uid, "whale.slice", false)
		if unitErr == nil && !slicePresent {
			unitErr = fs.ErrNotExist
		}
		if anchorErr == nil && !anchorPresent {
			anchorErr = fs.ErrNotExist
		}
		if daemonErr == nil && !daemonPresent {
			daemonErr = fs.ErrNotExist
		}
		if whaleErr == nil && !whalePresent {
			whaleErr = fs.ErrNotExist
		}
		_ = d.close(dirfd)
	}
	unitPresent := unitErr == nil
	switch {
	case unitErr == nil:
		d.logf("unit: present (managed marker ok)")
	case errors.Is(unitErr, fs.ErrNotExist):
		d.logf("unit: absent")
	case strings.Contains(unitErr.Error(), "marker-less"):
		d.logf("unit: present (foreign/marker invalid)")
	default:
		d.logf("unit: unevaluated (%v)", unitErr)
	}

	sliceActive := statusActive(d, d.sliceUnit, "slice")
	_ = sliceActive
	statusLiveProperty(d, d.sliceUnit, "MemoryMax")
	statusLiveProperty(d, d.sliceUnit, "MemoryHigh")

	managerControllers := filepath.Join(d.cgroupRoot, "user.slice", fmt.Sprintf("user-%d.slice", uid), fmt.Sprintf("user@%d.service", uid), "cgroup.controllers")
	managerData, managerErr := d.readFile(managerControllers)
	if managerErr != nil {
		d.logf("delegation: unevaluated (%v)", managerErr)
	} else if !hasToken(managerData, "memory") {
		d.logf("delegation: memory unavailable")
	} else {
		path, cgErr := controlGroupPath(d, d.sliceUnit)
		if cgErr != nil {
			d.logf("delegation: unevaluated (%v)", cgErr)
		} else if subtree, readErr := d.readFile(filepath.Join(path, "cgroup.subtree_control")); readErr != nil {
			d.logf("delegation: unevaluated (%v)", readErr)
		} else if hasToken(subtree, "memory") {
			d.logf("delegation: memory enabled")
		} else {
			d.logf("delegation: memory not enabled")
		}
	}

	anchorBinary, binaryOK := parseAnchorBinary(anchorContent)
	if errors.Is(anchorErr, fs.ErrNotExist) {
		d.logf("anchor: absent")
	} else if anchorErr != nil {
		d.logf("anchor: unevaluated (%v)", anchorErr)
	} else {
		statusActive(d, d.anchorUnit, "anchor")
		switch {
		case !binaryOK:
			d.logf("anchor binary: unevaluated (ExecStart cannot be parsed)")
		default:
			if _, statErr := d.stat(anchorBinary); errors.Is(statErr, fs.ErrNotExist) {
				d.logf("anchor: points at a missing binary (re-run aira install)")
			} else if statErr != nil {
				d.logf("anchor binary: unevaluated (%v)", statErr)
			} else {
				d.logf("anchor binary: present (%s)", anchorBinary)
			}
		}
	}
	statusDaemonFacet(d, uid, daemonContent, daemonErr)

	whaleCapped := cappedWhaleContent(whaleContent)
	switch {
	case whaleErr != nil && !errors.Is(whaleErr, fs.ErrNotExist):
		d.logf("whale.slice coexistence: unevaluated (%v)", whaleErr)
	case whaleCapped && unitPresent && overcommitRecorded(content):
		d.logf("coexisting capped slices present; overcommit opt-in recorded")
	case whaleCapped && unitPresent:
		d.logf("coexisting capped slices present; overcommit opt-in not recorded")
	case whaleCapped:
		d.logf("whale.slice: capped; aira.slice not established; overcommit opt-in not recorded")
	default:
		if unitPresent && overcommitRecorded(content) {
			d.logf("whale.slice coexistence: none detected; overcommit opt-in recorded")
		} else {
			d.logf("whale.slice coexistence: none detected; overcommit opt-in not recorded")
		}
	}
	return nil
}

func statusActive(d installDeps, unit, label string) bool {
	out, err := d.run([]string{"systemctl", "--user", "is-active", unit}, nil)
	state := strings.TrimSpace(string(out))
	if state == "active" {
		d.logf("%s: active", label)
		return true
	}
	if state != "" {
		d.logf("%s: %s", label, state)
		return false
	}
	d.logf("%s: unevaluated (%v)", label, err)
	return false
}

func statusLiveProperty(d installDeps, unit, property string) {
	out, err := d.run([]string{"systemctl", "--user", "show", "-p", property, "--value", unit}, nil)
	value := strings.TrimSpace(string(out))
	if err != nil || value == "" {
		d.logf("live %s: unevaluated (%v)", property, err)
		return
	}
	d.logf("live %s: %s", property, value)
}

func statusDaemonFacet(d installDeps, uid int, content []byte, unitErr error) {
	switch {
	case unitErr == nil:
		d.logf("daemon unit: present (managed marker ok)")
	case errors.Is(unitErr, fs.ErrNotExist):
		d.logf("daemon unit: absent")
	case strings.Contains(unitErr.Error(), "marker-less"):
		d.logf("daemon unit: present (foreign/marker invalid)")
	default:
		d.logf("daemon unit: unevaluated (%v)", unitErr)
	}
	active := statusProperty(d, d.daemonUnit, "ActiveState")
	sub := statusProperty(d, d.daemonUnit, "SubState")
	if active == "active" && sub == "running" {
		d.logf("daemon: active+running")
	} else {
		d.logf("daemon: ActiveState=%s SubState=%s", valueOrUnevaluated(active), valueOrUnevaluated(sub))
	}
	mode := ""
	liveEnvironment, liveErr := d.run([]string{"systemctl", "--user", "show", "-p", "Environment", "--value", d.daemonUnit}, nil)
	if liveErr == nil {
		for _, field := range strings.Fields(string(liveEnvironment)) {
			if strings.HasPrefix(field, "AIRA_DAEMON_WATCHDOG_MODE=") {
				mode = strings.TrimPrefix(field, "AIRA_DAEMON_WATCHDOG_MODE=")
			}
		}
	}
	if mode == "" {
		for _, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(line, "Environment=AIRA_DAEMON_WATCHDOG_MODE=") {
				mode = strings.TrimPrefix(line, "Environment=AIRA_DAEMON_WATCHDOG_MODE=") + " (declared; live unevaluated)"
			}
		}
	}
	if mode == "" {
		d.logf("daemon watchdog: unevaluated (%v)", liveErr)
	} else {
		d.logf("daemon watchdog: %s", mode)
	}
	paths, pathsErr := d.daemonPaths()
	if pathsErr != nil {
		d.logf("daemon reachable: unevaluated (%v)", pathsErr)
	} else if err := verifyDaemonReachable(d, paths); err != nil {
		d.logf("daemon reachable: no (%v)", err)
	} else {
		d.logf("daemon reachable: yes (MainPID matches lock-holder PID)")
	}
	lingerOut, lingerErr := d.run([]string{"loginctl", "show-user", strconv.Itoa(uid), "-p", "Linger", "--value"}, nil)
	linger := strings.TrimSpace(string(lingerOut))
	if lingerErr != nil || (linger != "yes" && linger != "no") {
		d.logf("linger: unevaluated (%v)", lingerErr)
	} else {
		d.logf("linger: %s", map[bool]string{true: "on", false: "off"}[linger == "yes"])
	}
}

func statusProperty(d installDeps, unit, property string) string {
	out, err := d.run([]string{"systemctl", "--user", "show", "-p", property, "--value", unit}, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func valueOrUnevaluated(value string) string {
	if value == "" {
		return "unevaluated"
	}
	return value
}

func parseAnchorBinary(content []byte) (string, bool) {
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		value := strings.TrimPrefix(line, "ExecStart=")
		if strings.HasPrefix(value, "\"") {
			end := strings.Index(value[1:], "\"")
			if end < 0 {
				return "", false
			}
			decoded, err := strconv.Unquote(value[:end+2])
			return decoded, err == nil && filepath.IsAbs(decoded)
		}
		path := strings.Fields(value)
		return first(path), len(path) > 0 && filepath.IsAbs(path[0])
	}
	return "", false
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func argumentInvalid(message string) error { return fmt.Errorf("%s: %s", CodeArgumentInvalid, message) }
func unavailable(err error) error          { return fmt.Errorf("%s: %w", CodeUnavailable, err) }
func delegationError(err error) error      { return fmt.Errorf("%s: %w", CodeDelegation, err) }
func overcommitError() error {
	return fmt.Errorf("%s: capped whale.slice already exists; two independent caps can sum past physical RAM. Migrate whale-run to aira confine for the safe end-state, or re-run with --allow-overcommit to acknowledge the interim risk", CodeOvercommit)
}
