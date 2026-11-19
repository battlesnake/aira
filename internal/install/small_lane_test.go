package install

import (
	"strings"
	"testing"
	"time"

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
