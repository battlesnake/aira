package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-121. `aira install --ci=shim` for a container with no systemd.

// shimProbeDeps makes the systemd probe FAIL the way a container does, and
// records every command that was run so a test can assert that no systemd work
// was attempted beyond the probe itself.
func shimProbeDeps(t *testing.T, d installDeps, state *fakeInstallState, present map[string]bool) installDeps {
	t.Helper()
	d.lookPath = func(name string) (string, error) {
		if present[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	previous := d.run
	d.run = func(argv []string, stdin []byte) ([]byte, error) {
		state.commands = append(state.commands, append([]string(nil), argv...))
		if strings.Contains(strings.Join(argv, " "), "is-system-running") {
			return nil, errors.New("Failed to connect to bus: No such file or directory")
		}
		return previous(argv, stdin)
	}
	return d
}

// verifies: AIRA-121 gate condition C7
//
// The three probe outcomes an operator must be able to tell apart, established
// through the lookPath seam BEFORE any command runs.
//
// The counterexample is the shape the plan proposed: classifying on the RUN's
// error. timeoutArgv wraps everything as `timeout 10s <cmd>`, so a missing
// systemctl surfaces as `timeout` exiting 127 — never as
// `exec: "systemctl": executable file not found` — and in a distroless image
// `timeout` itself is gone. Neither is distinguishable from a live systemd that
// answered badly, which is what the third case pins.
func TestCapabilityProbeDistinguishesAbsentFromUnreachable(t *testing.T) {
	for _, test := range []struct {
		name    string
		present map[string]bool
		output  string
		runErr  error
		want    string
	}{
		{
			name: "systemctl absent", present: map[string]bool{"timeout": true},
			want: systemdAbsent,
		},
		{
			name: "timeout absent", present: map[string]bool{"systemctl": true},
			want: systemdUnevaluated + "timeout(1) is absent",
		},
		{
			name: "systemctl present but no user manager", present: map[string]bool{"timeout": true, "systemctl": true},
			runErr: errors.New("exit status 1"), want: systemdUnreachable,
		},
		{
			name: "reachable", present: map[string]bool{"timeout": true, "systemctl": true},
			output: "running\n", want: systemdReachable,
		},
		{
			// `degraded` exits NON-ZERO and is a live manager. Classifying on the
			// exit status alone would call this unreachable and install a shim on
			// a perfectly good desktop.
			name: "degraded still counts as reachable", present: map[string]bool{"timeout": true, "systemctl": true},
			output: "degraded\n", runErr: errors.New("exit status 1"), want: systemdReachable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ran := 0
			// Built from realInstallDeps rather than through fillInstallDeps: the
			// reflection filler cannot Set an unexported func field obtained from
			// another struct, so a partial installDeps literal panics there. Every
			// other install test starts the same way.
			d := realInstallDeps()
			d.lookPath = func(name string) (string, error) {
				if test.present[name] {
					return "/usr/bin/" + name, nil
				}
				return "", exec.ErrNotFound
			}
			d.run = func([]string, []byte) ([]byte, error) {
				ran++
				return []byte(test.output), test.runErr
			}
			d.readFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
			got := probeSystemdUserManager(d)
			if !strings.HasPrefix(got, test.want) {
				t.Fatalf("SystemdUserManager=%q, want prefix %q", got, test.want)
			}
			if !test.present["timeout"] && ran != 0 {
				t.Fatal("the probe ran a command with no timeout(1) available: a hung D-Bus must never hang a docker build")
			}
		})
	}
}

// verifies: AIRA-121 requirement 1, ticket test (a)
//
// A failing probe under --ci=auto resolves to shim mode, records it durably, and
// runs NO systemd unit work at all — no daemon-reload, no enable, no linger.
func TestInstallAutoResolvesToShimWhenTheProbeFails(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true, "systemctl": true})
	d.spawnShimDaemon = func(shimDaemonSpec) error { state.daemonRunning = true; return nil }
	d.readFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/meminfo":
			return []byte("MemTotal:       16777216 kB\nMemAvailable:    8388608 kB\n"), nil
		case "/proc/self/cgroup":
			return []byte("0::/\n"), nil
		}
		return nil, os.ErrNotExist
	}
	if err := runInstall(d, installOpts{ciValue: "auto"}); err != nil {
		t.Fatalf("shim install: %v", err)
	}

	record := readShimRecord(t, d)
	if record.Mode != runner.ConfineModeShim {
		t.Fatalf("recorded mode=%q, want ci-shim", record.Mode)
	}
	if !strings.Contains(record.ResolvedBy, "--ci=auto") {
		t.Fatalf("resolved_by=%q must name the flag that decided", record.ResolvedBy)
	}
	if record.ShimBudgetSource != runner.ShimBudgetSourceMemTotal || record.ShimBudgetBytes != 16<<30 {
		t.Fatalf("budget=%d source=%q, want the MemTotal fallback", record.ShimBudgetBytes, record.ShimBudgetSource)
	}
	for _, argv := range state.commands {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "daemon-reload") || strings.Contains(joined, "enable --now") ||
			strings.Contains(joined, "enable-linger") {
			t.Fatalf("shim install ran systemd work: %q", joined)
		}
	}
	if _, err := os.Stat(filepath.Join(state.unitDir(), "aira.slice")); err == nil {
		t.Fatal("shim install published a systemd unit")
	}
	// The resolved mode is ALWAYS reported: requirement 1's "never let two boxes
	// end up silently different" is satisfied structurally, not by convention.
	joinedLogs := strings.Join(state.logs, "\n")
	if !strings.Contains(joinedLogs, "install mode: ci-shim") || !strings.Contains(joinedLogs, "containment: advisory") {
		t.Fatalf("install output never states the resolved mode and its containment:\n%s", joinedLogs)
	}
}

// verifies: AIRA-121 gate condition C3
//
// The ROOT-in-a-container case, which is the `docker build` default. runInstall
// must NOT take the runRootInstall path in shim mode: that path demands
// SUDO_USER, an owned /run/user/<uid> session directory, /etc drop-ins and
// loginctl, none of which exists in a build layer.
func TestShimInstallAsRootNeedsNoSudoIdentityOrSession(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.geteuid = func() int { return 0 }
	d.getenv = func(name string) string {
		if name == "SUDO_USER" || name == "SUDO_UID" || name == "SUDO_GID" {
			return ""
		}
		return installTestEnv(state, name)
	}
	d.stat = func(path string) (os.FileInfo, error) {
		if strings.HasPrefix(path, "/run/user/") {
			return nil, errors.New("no such session directory in a docker build layer")
		}
		return os.Stat(path)
	}
	d.reexec = func(reexecRequest) error {
		t.Fatal("shim install re-executed itself; there is no second user to install on behalf of")
		return nil
	}
	d.readFile = shimProcReader(nil)
	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild}); err != nil {
		t.Fatalf("root shim build stage: %v", err)
	}
	record := readShimRecord(t, d)
	if record.UID != 0 || record.Home != state.home {
		t.Fatalf("record=%+v; a root shim install records the CURRENT user's own home", record)
	}
}

// verifies: AIRA-121 requirement 9, ticket test (h)
//
// The build stage places bytes and starts NOTHING. The half that fails against a
// build stage which quietly starts things is the spawn seam, which fails the
// test if it is ever called.
func TestShimBuildStageStartsNothingAndStartStageIsWhatLaunches(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(nil)
	spawned := 0
	d.spawnShimDaemon = func(shimDaemonSpec) error {
		spawned++
		state.daemonRunning = true
		return nil
	}

	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild}); err != nil {
		t.Fatalf("build stage: %v", err)
	}
	if spawned != 0 {
		t.Fatal("the build stage started a daemon; a `docker build` RUN layer must place bytes only")
	}
	if state.daemonRunning {
		t.Fatal("the build stage left a daemon running")
	}

	if err := runInstall(d, installOpts{ciValue: "shim", stage: installStageStart}); err != nil {
		t.Fatalf("start stage: %v", err)
	}
	if spawned != 1 {
		t.Fatalf("the start stage spawned %d daemons, want exactly 1", spawned)
	}
}

// verifies: AIRA-121 requirement 9
//
// A start stage with no recorded plan REFUSES, and a plan recorded for a
// different home or uid is refused too. A start stage must never RE-RESOLVE a
// mode the build stage already resolved: that is the "two boxes silently
// different" failure with the two boxes being the same box at two moments.
func TestShimStartStageRefusesAnAbsentOrForeignPlan(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(nil)
	d.spawnShimDaemon = func(shimDaemonSpec) error { t.Fatal("started a daemon with no plan"); return nil }

	err := runInstall(d, installOpts{ciValue: "shim", stage: installStageStart})
	if err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("err=%v, want %s for a missing plan", err, CodeUnavailable)
	}

	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild}); err != nil {
		t.Fatal(err)
	}
	d.geteuid = func() int { return state.uid + 1 }
	err = runInstall(d, installOpts{ciValue: "shim", stage: installStageStart})
	if err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("err=%v, want a refusal for a plan recorded under a different uid", err)
	}
}

// verifies: AIRA-121 gate condition C8
//
// --memory-max IS accepted under --ci=shim, as the DECLARED ledger budget, and
// is recorded and printed as declared. It is refused under --ci=auto, which may
// resolve to the real path where it genuinely conflicts.
func TestShimAcceptsDeclaredMemoryMaxAsTheLedgerBudget(t *testing.T) {
	if _, err := parseInstallArgs([]string{"--ci=shim", "--memory-max", "32G"}); err != nil {
		t.Fatalf("--ci=shim with --memory-max was refused: %v", err)
	}
	if _, err := parseInstallArgs([]string{"--ci=auto", "--memory-max", "32G"}); err == nil {
		t.Fatal("--ci=auto with --memory-max must be refused: auto may resolve to the real path, where the two genuinely conflict")
	}
	if _, err := parseInstallArgs([]string{"--ci", "--memory-max", "32G"}); err == nil {
		t.Fatal("bare --ci with --memory-max must stay refused (AIRA-120)")
	}

	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	// A readable container memory.max is present and DELIBERATELY different from
	// the declared value, so this test fails if the declaration is ignored in
	// favour of the probe.
	d.readFile = shimProcReader(map[string][]byte{
		filepath.Join(cgroupRoot, "memory.max"): []byte("17179869184\n"),
	})
	d.spawnShimDaemon = func(shimDaemonSpec) error { return nil }
	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "32G", stage: installStageBuild}); err != nil {
		t.Fatal(err)
	}
	record := readShimRecord(t, d)
	if record.ShimBudgetSource != runner.ShimBudgetSourceDeclared {
		t.Fatalf("budget source=%q, want declared", record.ShimBudgetSource)
	}
	if record.ShimBudgetBytes != 32<<30 {
		t.Fatalf("budget=%d, want the declared 32G", record.ShimBudgetBytes)
	}
	if !strings.Contains(strings.Join(state.logs, "\n"), "declared with --memory-max") {
		t.Fatalf("the declared provenance is not printed:\n%s", strings.Join(state.logs, "\n"))
	}
}

// verifies: AIRA-121 requirement 4, finding F1
//
// A DECLARED budget over a container whose own cgroup has memory.max = `max` —
// the multi-task-per-node case --memory-max exists for — still records the
// container's OWN cgroup path. That path is the daemon's only source of a
// namespaced live `current`; without it readShimMemory falls to host-wide
// meminfo, which on a big node exceeds the declared budget outright and wedges
// every job at E_ADMIT_TOO_LARGE.
func TestShimRecordsTheOwnCgroupPathForADeclaredBudgetOverAnUnboundedContainer(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(map[string][]byte{
		filepath.Join(cgroupRoot, "memory.max"): []byte("max\n"),
	})
	d.spawnShimDaemon = func(shimDaemonSpec) error { return nil }
	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "4G", stage: installStageBuild}); err != nil {
		t.Fatal(err)
	}
	record := readShimRecord(t, d)
	if record.ShimBudgetSource != runner.ShimBudgetSourceDeclared || record.ShimBudgetBytes != 4<<30 {
		t.Fatalf("budget=%d source=%q, want the declared 4GiB", record.ShimBudgetBytes, record.ShimBudgetSource)
	}
	if record.ShimCgroupPath != cgroupRoot {
		t.Fatalf("recorded cgroup path=%q, want %s: an unbounded memory.max does not make the container's own memory.current unreadable",
			record.ShimCgroupPath, cgroupRoot)
	}
	if logs := strings.Join(state.logs, "\n"); strings.Contains(logs, "bytes) (") {
		t.Fatalf("the budget line prints its byte count twice:\n%s", logs)
	}
}

// verifies: AIRA-121 requirement 4, finding F1
//
// The OTHER direction: when the probe could establish NO cgroup of its own —
// no /proc/self/cgroup, or a cgroup-v1-only host with no unified entry — the
// record carries NO path, rather than the cgroup mount root as a guess. The
// daemon then honestly uses meminfo instead of reading a cgroup that is not
// known to be this container's.
func TestShimRecordsNoCgroupPathWhenTheProbeEstablishedNone(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = func(path string) ([]byte, error) {
		if path == "/proc/meminfo" {
			return []byte("MemTotal:      268435456 kB\n"), nil
		}
		return nil, os.ErrNotExist
	}
	d.spawnShimDaemon = func(shimDaemonSpec) error { return nil }
	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "4G", stage: installStageBuild}); err != nil {
		t.Fatal(err)
	}
	if record := readShimRecord(t, d); record.ShimCgroupPath != "" {
		t.Fatalf("recorded cgroup path=%q with no probed cgroup: the mount root is a guess, not this container's cgroup", record.ShimCgroupPath)
	}
}

// verifies: AIRA-121 requirement 4
//
// A container's own memory.max is preferred over host-wide MemTotal, because
// /proc/meminfo is not namespaced: inside a container it reports the HOST's
// memory, and booking against that over-books the container.
func TestShimPrefersTheContainersOwnMemoryMaxOverMemTotal(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(map[string][]byte{
		filepath.Join(cgroupRoot, "memory.max"): []byte("17179869184\n"),
	})
	d.spawnShimDaemon = func(shimDaemonSpec) error { return nil }
	if err := runInstall(d, installOpts{ciValue: "shim", stage: installStageBuild}); err != nil {
		t.Fatal(err)
	}
	record := readShimRecord(t, d)
	if record.ShimBudgetSource != runner.ShimBudgetSourceCgroupMemoryMax || record.ShimBudgetBytes != 16<<30 {
		t.Fatalf("budget=%d source=%q, want the container's own 16GiB memory.max, not the host's MemTotal",
			record.ShimBudgetBytes, record.ShimBudgetSource)
	}
}

// verifies: AIRA-121 requirement 3
//
// A shim install that can establish NO budget FAILS. A silently ungated shim
// must never exist: one loud failure at install beats a per-job wedge later.
func TestShimInstallFailsWhenNoBudgetCanBeEstablished(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = func(path string) ([]byte, error) {
		if path == "/proc/self/cgroup" {
			return []byte("0::/\n"), nil
		}
		return nil, os.ErrNotExist
	}
	d.spawnShimDaemon = func(shimDaemonSpec) error { t.Fatal("started an ungated shim daemon"); return nil }
	err := runInstall(d, installOpts{ciValue: "shim", stage: installStageBuild})
	if err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("err=%v, want %s", err, CodeUnavailable)
	}
	if !strings.Contains(err.Error(), "--memory-max") {
		t.Fatalf("the refusal %q does not tell the operator how to fix it", err)
	}
}

// verifies: AIRA-121 review round 3, finding F4
//
// A DECLARED --memory-max budget below the 4G floor is REFUSED at install,
// mirroring resolveCIMemoryMax's real-path refusal (install.go:1215-1218). The
// daemon's admission headroom is 2GiB base + 64MiB/job
// (internal/daemon/admit.go); a budget at or below roughly that headroom makes
// checkedAvailable answer 0 for EVERY job, forever -- an entirely ordinary
// CI/k8s-Job container size (2GiB), not an exotic misconfiguration. Failing
// loudly here beats a shim that installs cleanly, prints a healthy-looking
// "advisory admission ledger active" message, and then wedges every job with
// E_ADMIT_TOO_LARGE cap_minus_headroom=0 for its whole life.
//
// Counterexample this fails against: the pre-fix resolveShimBudget, which
// applied sizeBytes' floor=false path and accepted any positive declared size.
func TestShimInstallRefusesADeclaredBudgetBelowTheFloor(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(nil)
	d.spawnShimDaemon = func(shimDaemonSpec) error {
		t.Fatal("started an ungated shim daemon under a sub-floor budget")
		return nil
	}

	err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "2G", stage: installStageBuild})
	if err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("err=%v, want a %s refusal for a 2G declared budget", err, CodeUnavailable)
	}
	if !strings.Contains(err.Error(), "2.00GiB") || !strings.Contains(err.Error(), "4G") {
		t.Fatalf("refusal %q does not name both the offending value and the floor", err)
	}
}

// verifies: AIRA-121 review round 3, finding F4
//
// The SAME floor applies to a budget read from the container's OWN cgroup
// memory.max (source=cgroup-memory-max), not only to a declared --memory-max --
// the ceiling is unusable either way. The message names the container's own
// limit and points at --memory-max as the workaround, since an operator may
// not directly control how the surrounding container runtime sized this
// container's own cgroup.
func TestShimInstallRefusesACgroupDerivedBudgetBelowTheFloor(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(map[string][]byte{
		filepath.Join(cgroupRoot, "memory.max"): []byte("2147483648\n"), // exactly 2GiB
	})
	d.spawnShimDaemon = func(shimDaemonSpec) error {
		t.Fatal("started an ungated shim daemon under a sub-floor budget")
		return nil
	}

	err := runInstall(d, installOpts{ciValue: "shim", stage: installStageBuild})
	if err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
		t.Fatalf("err=%v, want a %s refusal for a 2GiB container memory.max", err, CodeUnavailable)
	}
	if !strings.Contains(err.Error(), "2.00GiB") || !strings.Contains(err.Error(), "4G") || !strings.Contains(err.Error(), "--memory-max") {
		t.Fatalf("refusal %q does not name the container's own limit, the floor, and the --memory-max workaround", err)
	}
}

// verifies: AIRA-121 review round 3, finding F4
//
// A budget AT the floor (exactly 4G) is accepted from either source: the fix
// must refuse below the floor, never AT it.
func TestShimInstallAcceptsABudgetExactlyAtTheFloor(t *testing.T) {
	t.Run("declared", func(t *testing.T) {
		d, state := newFakeInstall(t)
		d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
		d.readFile = shimProcReader(nil)
		d.spawnShimDaemon = func(shimDaemonSpec) error { return nil }
		if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "4G", stage: installStageBuild}); err != nil {
			t.Fatalf("a 4G declared budget was refused: %v", err)
		}
		if record := readShimRecord(t, d); record.ShimBudgetBytes != 4<<30 {
			t.Fatalf("budget=%d, want exactly the 4G floor", record.ShimBudgetBytes)
		}
	})
	t.Run("cgroup-derived", func(t *testing.T) {
		d, state := newFakeInstall(t)
		d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
		d.readFile = shimProcReader(map[string][]byte{
			filepath.Join(cgroupRoot, "memory.max"): []byte("4294967296\n"), // exactly 4GiB
		})
		d.spawnShimDaemon = func(shimDaemonSpec) error { return nil }
		if err := runInstall(d, installOpts{ciValue: "shim", stage: installStageBuild}); err != nil {
			t.Fatalf("a 4GiB container memory.max was refused: %v", err)
		}
		if record := readShimRecord(t, d); record.ShimBudgetBytes != 4<<30 {
			t.Fatalf("budget=%d, want exactly the 4GiB floor", record.ShimBudgetBytes)
		}
	})
}

// verifies: AIRA-121 gate condition C3
//
// The privileged leg forwards the --ci VALUE and the --stage value. Forwarding a
// bare --ci for a --ci=auto invocation would make the unprivileged leg — the one
// that actually renders and publishes — take a different decision path from the
// root leg, and forwarding nothing for --stage would make `sudo aira install
// --stage=build` run a full start under the covers.
func TestReexecForwardsTheCIValueAndTheStage(t *testing.T) {
	target := installTarget{uid: 1000, gid: 1000, home: "/home/x", username: "x"}
	args := strings.Join(reexecRequestFor("/opt/aira", target, installOpts{ciValue: "auto", stage: installStageBuild}).args, " ")
	if !strings.Contains(args, "--ci=auto") {
		t.Fatalf("re-exec args %q drop the --ci value", args)
	}
	if !strings.Contains(args, "--stage=build") {
		t.Fatalf("re-exec args %q drop the --stage value", args)
	}
	// Bare --ci keeps its exact AIRA-120 spelling.
	bare := strings.Join(reexecRequestFor("/opt/aira", target, installOpts{ci: true}).args, " ")
	if !strings.Contains(bare, "--ci") || strings.Contains(bare, "--ci=") {
		t.Fatalf("bare --ci was rewritten: %q", bare)
	}
}

// verifies: AIRA-121 gate condition C12
//
// The daemon child is told to run with every cgroup-walking subsystem off, and
// with the mode it was installed in.
func TestShimDaemonEnvironmentDisablesEveryCgroupSubsystem(t *testing.T) {
	d, _ := newFakeInstall(t)
	env := shimDaemonEnvironment(d, runner.InstallModeRecord{
		Home: "/home/x", ShimBudgetBytes: 1 << 30, ShimBudgetSource: runner.ShimBudgetSourceDeclared,
	})
	joined := strings.Join(env, " ")
	for _, want := range []string{
		"AIRA_DAEMON_MANAGED=1",
		"AIRA_DAEMON_CONFINE_MODE=" + runner.ConfineModeShim,
		"AIRA_DAEMON_WATCHDOG_MODE=off",
		"AIRA_DAEMON_SLICE_CEILING_MODE=off",
		"AIRA_DAEMON_OOM_STEER_MODE=off",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("shim daemon environment is missing %q:\n%s", want, joined)
		}
	}
}

// installTestEnv mirrors newFakeInstall's getenv for tests that need to override
// only part of it.
func installTestEnv(state *fakeInstallState, name string) string {
	switch name {
	case "HOME":
		return state.home
	case "XDG_STATE_HOME":
		return filepath.Join(state.home, "state")
	case "XDG_RUNTIME_DIR":
		return filepath.Join(state.home, "runtime")
	}
	return ""
}

// shimProcReader stands in for the container's /proc and /sys/fs/cgroup. The
// MemTotal it reports is deliberately LARGER than any container memory.max a
// test supplies, so "the container's own limit won" is a real observation.
func shimProcReader(extra map[string][]byte) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if value, ok := extra[path]; ok {
			return append([]byte(nil), value...), nil
		}
		switch path {
		case "/proc/meminfo":
			return []byte("MemTotal:      268435456 kB\nMemAvailable:  134217728 kB\n"), nil
		case "/proc/self/cgroup":
			return []byte("0::/\n"), nil
		}
		return nil, os.ErrNotExist
	}
}

func readShimRecord(t *testing.T, d installDeps) runner.InstallModeRecord {
	t.Helper()
	paths, err := d.daemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	path := runner.InstallModePathFor(paths.StateHome)
	record, ok := runner.ReadInstallModeRecord(path)
	if !ok {
		data, readErr := os.ReadFile(path)
		t.Fatalf("no usable install-mode record at %s (read err=%v, content=%s)", path, readErr, data)
	}
	return record
}

var _ = fmt.Sprintf
