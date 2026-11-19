package install

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/runner"
)

// verifies: AIRA-350 §2.5 — the small-job lane's install flags, on the AIRA-324
// pin-charge pattern: absent flags parse to the zero value; with nothing
// installed the ship default renders 8G for jobs booking at most 1G; an omitted
// flag keeps a valid installed value and ignores an invalid one; explicit flags
// win and are forwarded by the sudo re-exec only when given; bad values, an
// x < y pair and --status with either flag are refused with
// E_INSTALL_ARGUMENT_INVALID; the unit carries both variables; a CI (ci-shim)
// daemon gets the lane off. Mutations: default off; the re-exec forwarding an
// ungiven flag; an invalid installed value propagated; a pair with y > x
// accepted; the shim env dropping off.
func TestInstallSmallLaneFlags(t *testing.T) {
	opts, err := parseInstallArgs(nil)
	if err != nil || opts.smallLane != "" || opts.smallLaneJobMax != "" {
		t.Fatalf("absent options must parse to the zero value: %+v %v", opts, err)
	}
	opts, err = parseInstallArgs([]string{"--small-lane", "4G", "--small-lane-job-max=512M"})
	if err != nil || opts.smallLane != "4G" || opts.smallLaneJobMax != "512M" {
		t.Fatalf("explicit=%+v err=%v", opts, err)
	}
	if opts, err = parseInstallArgs([]string{"--small-lane=off"}); err != nil || opts.smallLane != "off" {
		t.Fatalf("off=%+v err=%v", opts, err)
	}
	for _, args := range [][]string{
		{"--small-lane=abc"}, {"--small-lane=0"}, {"--small-lane"}, {"--small-lane-job-max=0"},
		{"--small-lane-job-max=512K"}, {"--small-lane-job-max=lots"},
		{"--status", "--small-lane=8G"}, {"--status", "--small-lane-job-max=1G"},
		// Explicit empty and whitespace-only values, attached and separate: each
		// trims to "", which the lane parser reads as "not given".
		{"--small-lane="}, {"--small-lane= "}, {"--small-lane", ""}, {"--small-lane", " \t"},
		{"--small-lane-job-max="}, {"--small-lane-job-max= "}, {"--small-lane-job-max", ""}, {"--small-lane-job-max", " "},
	} {
		if _, err := parseInstallArgs(args); err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
			t.Fatalf("args=%q err=%v", args, err)
		}
	}

	resolved, err := resolveDaemonModes(installOpts{}, "")
	if err != nil || resolved.smallLane != "8G" || resolved.smallLaneJobMax != "1G" {
		t.Fatalf("first install: lane %q job max %q, want 8G 1G: %v", resolved.smallLane, resolved.smallLaneJobMax, err)
	}
	installed := "# aira-managed: aira-daemon.service\n[Service]\n" +
		"Environment=AIRA_DAEMON_SMALL_LANE=4G\n" +
		"Environment=AIRA_DAEMON_SMALL_LANE_JOB_MAX=512M\n"
	resolved, err = resolveDaemonModes(installOpts{}, installed)
	if err != nil || resolved.smallLane != "4G" || resolved.smallLaneJobMax != "512M" {
		t.Fatalf("omitted flags did not keep the installed 4G/512M: %q %q %v", resolved.smallLane, resolved.smallLaneJobMax, err)
	}
	resolved, err = resolveDaemonModes(installOpts{smallLane: "off"}, installed)
	if err != nil || resolved.smallLane != "off" || resolved.smallLaneJobMax != "512M" {
		t.Fatalf("explicit off lost to the installed unit: %q %q %v", resolved.smallLane, resolved.smallLaneJobMax, err)
	}
	garbled := "Environment=AIRA_DAEMON_SMALL_LANE=lots\nEnvironment=AIRA_DAEMON_SMALL_LANE_JOB_MAX=0\n"
	resolved, err = resolveDaemonModes(installOpts{}, garbled)
	if err != nil || resolved.smallLane != "8G" || resolved.smallLaneJobMax != "1G" {
		t.Fatalf("an invalid installed value was propagated: %q %q %v", resolved.smallLane, resolved.smallLaneJobMax, err)
	}
	// A pair invalid only together, from the installed unit: a flagless
	// re-install falls back to the defaults rather than failing.
	mismatched := "Environment=AIRA_DAEMON_SMALL_LANE=1G\nEnvironment=AIRA_DAEMON_SMALL_LANE_JOB_MAX=2G\n"
	resolved, err = resolveDaemonModes(installOpts{}, mismatched)
	if err != nil || resolved.smallLane != "8G" || resolved.smallLaneJobMax != "1G" {
		t.Fatalf("an installed pair with y > x: %q %q %v, want the defaults 8G 1G", resolved.smallLane, resolved.smallLaneJobMax, err)
	}
	if _, err := resolveDaemonModes(installOpts{smallLane: "512M"}, ""); err == nil {
		t.Fatal("a lane smaller than the default job maximum was accepted")
	}
	if _, err := resolveDaemonModes(installOpts{smallLane: "4G", smallLaneJobMax: "8G"}, ""); err == nil {
		t.Fatal("a job maximum above the lane was accepted")
	}

	target := installTarget{uid: 1000, gid: 1000, home: "/home/someone", username: "someone"}
	args := strings.Join(reexecRequestFor("/usr/bin/aira", target, installOpts{memoryMax: "16G"}).args, " ")
	if strings.Contains(args, "--small-lane") {
		t.Fatalf("re-exec forwards an ungiven --small-lane flag: %q", args)
	}
	args = strings.Join(reexecRequestFor("/usr/bin/aira", target, installOpts{smallLane: "4G", smallLaneJobMax: "512M"}).args, " ")
	for _, want := range []string{"--small-lane 4G", "--small-lane-job-max 512M"} {
		if !strings.Contains(args, want) {
			t.Fatalf("re-exec argv %q drops %q", args, want)
		}
	}

	unit, err := renderDaemonUnit("d.service", "/opt/aira", "/var/lib/aira", "observe", 2*time.Second, "observe", "observe", "0.8", "8G", "1G", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Environment=AIRA_DAEMON_SMALL_LANE=8G\n", "Environment=AIRA_DAEMON_SMALL_LANE_JOB_MAX=1G\n"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
	if _, err := renderDaemonUnit("d.service", "/opt/aira", "/var/lib/aira", "observe", 2*time.Second, "observe", "observe", "0.8", "4G", "8G", ""); err == nil {
		t.Fatal("rendered a unit with a job maximum above the lane")
	}

	d, _ := newFakeInstall(t)
	env := strings.Join(shimDaemonEnvironment(d, runner.InstallModeRecord{
		Home: "/home/x", ShimBudgetBytes: 1 << 30, ShimBudgetSource: runner.ShimBudgetSourceDeclared,
	}), " ")
	if !strings.Contains(env, "AIRA_DAEMON_SMALL_LANE=off") {
		t.Fatalf("a CI (ci-shim) daemon does not get the lane off:\n%s", env)
	}
}

// verifies: AIRA-350 §2.5 (review fix) — an explicit flag never repairs a pair
// it made invalid with an inherited value: installed 512M/256M, then
// --small-lane-job-max=1G (or --small-lane=128M) is refused, never resolved to the 8G/1G defaults that silently change the value
// the operator did not type. Through runInstall the refusal carries
// E_INSTALL_ARGUMENT_INVALID and leaves the installed unit as it was. Mutation:
// the pre-fix recovery (any conflict with an inherited value falls back).
func TestInstallSmallLaneExplicitFlagNeverRepairsTheInstalledPair(t *testing.T) {
	installed := "Environment=AIRA_DAEMON_SMALL_LANE=512M\nEnvironment=AIRA_DAEMON_SMALL_LANE_JOB_MAX=256M\n"
	resolved, err := resolveDaemonModes(installOpts{}, installed)
	if err != nil || resolved.smallLane != "512M" || resolved.smallLaneJobMax != "256M" {
		t.Fatalf("flagless re-install: %q %q %v, want the installed 512M 256M", resolved.smallLane, resolved.smallLaneJobMax, err)
	}
	for _, opts := range []installOpts{{smallLaneJobMax: "1G"}, {smallLane: "128M"}} {
		if resolved, err := resolveDaemonModes(opts, installed); err == nil {
			t.Fatalf("%+v against installed 512M/256M resolved to %q %q, want a refusal", opts, resolved.smallLane, resolved.smallLaneJobMax)
		}
	}

	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", smallLane: "512M", smallLaneJobMax: "256M"}); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	unitPath := filepath.Join(state.home, ".config", "systemd", "user", d.daemonUnit)
	before, err := os.ReadFile(unitPath)
	if err != nil || !strings.Contains(string(before), "AIRA_DAEMON_SMALL_LANE=512M\n") {
		t.Fatalf("seed unit: %v\n%s", err, before)
	}
	err = runInstall(d, installOpts{smallLaneJobMax: "1G"})
	if err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
		t.Fatalf("--small-lane-job-max=1G against installed 512M/256M: err=%v, want %s", err, CodeArgumentInvalid)
	}
	if after, _ := os.ReadFile(unitPath); string(after) != string(before) {
		t.Fatalf("a refused install changed the unit:\n%s", after)
	}
}

// verifies: AIRA-350 §2.5 (review fixes) — "default mode only, not CI mode":
// a CI install (bare --ci, --ci=auto, --ci=shim) renders the lane off, over an
// installed lane AND over explicit --small-lane / --small-lane-job-max flags (CI
// renders off, no exception for a typed flag). End to end, dry runs of bare
// --ci and of --ci=auto resolved to the real path, each with --small-lane=4G,
// render AIRA_DAEMON_SMALL_LANE=off into the daemon unit and never =4G.
// Mutations: the CI default deleted (the lane renders 8G); the CI override made
// conditional on --small-lane being absent (round-1's exception: renders 4G).
func TestInstallSmallLaneOffInCIMode(t *testing.T) {
	installed := "Environment=AIRA_DAEMON_SMALL_LANE=4G\nEnvironment=AIRA_DAEMON_SMALL_LANE_JOB_MAX=512M\n"
	for _, ci := range []installOpts{{ci: true}, {ciValue: "auto"}, {ciValue: "shim"}} {
		for _, flags := range []installOpts{{}, {smallLane: "4G"}, {smallLane: "4G", smallLaneJobMax: "512M"}, {smallLaneJobMax: "2G"}} {
			opts := ci
			opts.smallLane, opts.smallLaneJobMax = flags.smallLane, flags.smallLaneJobMax
			for _, unit := range []string{"", installed} {
				resolved, err := resolveDaemonModes(opts, unit)
				if err != nil || resolved.smallLane != runner.SmallLaneOff {
					t.Fatalf("ci=%v ciValue=%q lane=%q jobMax=%q unit=%q: lane %q %v, want off",
						opts.ci, opts.ciValue, opts.smallLane, opts.smallLaneJobMax, unit, resolved.smallLane, err)
				}
			}
		}
	}

	for _, test := range []struct {
		name string
		opts installOpts
		auto bool
	}{
		{"bare --ci", installOpts{ci: true, smallLane: "4G", dryRun: true}, false},
		{"--ci=auto resolved to real", installOpts{ciValue: "auto", smallLane: "4G", smallLaneJobMax: "512M", dryRun: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, state := newFakeInstall(t)
			calls := 0
			d = withCIReader(d, ciMeasured, true, "", &calls)
			probed := false
			d.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
			d.run = func(argv []string, _ []byte) ([]byte, error) {
				if test.auto && strings.Contains(strings.Join(argv, " "), "is-system-running") {
					probed = true
					return []byte("running\n"), nil
				}
				t.Fatalf("dry-run invoked a command: %q", argv)
				return nil, nil
			}
			if err := runInstall(d, test.opts); err != nil {
				t.Fatal(err)
			}
			if test.auto && !probed {
				t.Fatal("--ci=auto never probed the systemd user manager")
			}
			report := strings.Join(state.logs, "\n")
			if !strings.Contains(report, "Environment=AIRA_DAEMON_SMALL_LANE=off\n") ||
				strings.Contains(report, "AIRA_DAEMON_SMALL_LANE=4G") {
				t.Fatalf("%s --small-lane=4G dry run does not render the lane off:\n%s", test.name, report)
			}
			if test.auto && !strings.Contains(report, "aira.slice") {
				t.Fatalf("--ci=auto did not take the real path:\n%s", report)
			}
		})
	}
}

// verifies: AIRA-350 §6 "--status" — `aira install --status` reports the lane's
// size and job maximum, live from the running daemon's environment when it is
// readable, else as declared by the unit. Mutation: either reportDaemonMode call
// deleted.
func TestInstallStatusReportsTheSmallLane(t *testing.T) {
	unit := []byte("# aira-managed: aira-daemon.service\n[Service]\n" +
		"Environment=AIRA_DAEMON_SMALL_LANE=8G\nEnvironment=AIRA_DAEMON_SMALL_LANE_JOB_MAX=1G\n")
	for _, test := range []struct {
		name    string
		liveErr error
		want    []string
	}{
		{"live", nil, []string{"daemon small-job lane: 4G\n", "daemon small-job lane job max: 512M\n"}},
		{"declared", errors.New("no bus"), []string{
			"daemon small-job lane: 8G (declared; live unevaluated)\n",
			"daemon small-job lane job max: 1G (declared; live unevaluated)\n",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, _ := newFakeInstall(t)
			var out bytes.Buffer
			d.logf = func(format string, args ...any) { fmt.Fprintf(&out, format+"\n", args...) }
			d.run = func(argv []string, _ []byte) ([]byte, error) {
				if strings.Join(argv, " ") == "systemctl --user show -p Environment --value "+d.daemonUnit {
					if test.liveErr != nil {
						return nil, test.liveErr
					}
					return []byte("AIRA_DAEMON_SMALL_LANE=4G AIRA_DAEMON_SMALL_LANE_JOB_MAX=512M\n"), nil
				}
				return nil, errors.New("not modelled")
			}
			d.daemonPaths = func() (daemon.Paths, error) { return daemon.Paths{}, errors.New("not modelled") }
			statusDaemonFacet(d, 1000, unit, nil)
			for _, want := range test.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("status lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}
