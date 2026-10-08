package install

import (
	"os"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-283. `aira install --cpu-slots-per-core=R`.

// verifies: AIRA-283 §3.1 — the flag is an integer in 1..64, given as =R or as a
// separate argument. 0, negative, > 64, fractional and non-numeric values are
// E_INSTALL_ARGUMENT_INVALID with the accepted range in the message.
func TestParseCPUSlotsPerCoreFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--cpu-slots-per-core=3"},
		{"--cpu-slots-per-core", "3"},
	} {
		opts, err := parseInstallArgs(args)
		if err != nil || opts.cpuSlotsPerCore != 3 {
			t.Fatalf("%q: opts.cpuSlotsPerCore=%d err=%v, want 3", args, opts.cpuSlotsPerCore, err)
		}
	}
	for _, value := range []string{"1", "64"} {
		if opts, err := parseInstallArgs([]string{"--cpu-slots-per-core=" + value}); err != nil || opts.cpuSlotsPerCore == 0 {
			t.Fatalf("boundary %s refused: %v", value, err)
		}
	}
	for _, value := range []string{"0", "-1", "65", "1.5", "x"} {
		_, err := parseInstallArgs([]string{"--cpu-slots-per-core=" + value})
		if err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
			t.Fatalf("--cpu-slots-per-core=%s: err=%v, want %s", value, err, CodeArgumentInvalid)
		}
		if !strings.Contains(err.Error(), "1 to 64") {
			t.Fatalf("--cpu-slots-per-core=%s: the refusal does not state the accepted range: %v", value, err)
		}
	}
	// A separate "-1" is a VALUE, not an option, so it reaches the range check.
	if _, err := parseInstallArgs([]string{"--cpu-slots-per-core", "-1"}); err == nil || !strings.Contains(err.Error(), "1 to 64") {
		t.Fatalf("--cpu-slots-per-core -1: err=%v, want the range refusal", err)
	}
}

// verifies: AIRA-283 §3.1 (E2, E8) — the flag is REFUSED on every path that does
// not write the record (--status, --stage=start: a silently ignored flag is a
// lie) and accepted where the record is written (real install, --ci=shim at
// --stage=build or both stages, --ci=auto).
func TestCPUSlotsPerCoreFlagIsRefusedWhereNoRecordIsWritten(t *testing.T) {
	for _, args := range [][]string{
		{"--status", "--cpu-slots-per-core=3"},
		{"--stage=start", "--cpu-slots-per-core=3"},
		{"--ci=shim", "--stage=start", "--cpu-slots-per-core=3"},
	} {
		if _, err := parseInstallArgs(args); err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
			t.Fatalf("%q: err=%v, want %s", args, err, CodeArgumentInvalid)
		}
	}
	for _, args := range [][]string{
		{"--cpu-slots-per-core=3"},
		{"--memory-max=16G", "--cpu-slots-per-core=3"},
		{"--stage=build", "--cpu-slots-per-core=3"},
		{"--ci=shim", "--stage=build", "--cpu-slots-per-core=3"},
		{"--ci=shim", "--memory-max=8G", "--cpu-slots-per-core=3"},
		{"--ci=auto", "--cpu-slots-per-core=3"},
	} {
		if _, err := parseInstallArgs(args); err != nil {
			t.Fatalf("%q refused: %v", args, err)
		}
	}
}

// recordBytes reads the install-mode record exactly as written.
func recordBytes(t *testing.T, d installDeps) string {
	t.Helper()
	paths, err := d.daemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(runner.InstallModePathFor(paths.StateHome))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// verifies: AIRA-283 §3.1 persistence, real-slice — the flag is recorded; a
// reinstall WITHOUT it preserves the stored ratio; an explicit =2 is recorded
// and is distinct on disk from omission.
func TestRealInstallRecordsAndPreservesTheRatio(t *testing.T) {
	d, _ := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recordBytes(t, d), "cpu_slots_per_core") {
		t.Fatalf("an install with no flag recorded a ratio:\n%s", recordBytes(t, d))
	}
	if err := runInstall(d, installOpts{memoryMax: "16G", cpuSlotsPerCore: 3}); err != nil {
		t.Fatal(err)
	}
	if got := readShimRecord(t, d).CPUSlotsPerCore; got != 3 {
		t.Fatalf("recorded ratio=%d after --cpu-slots-per-core=3", got)
	}
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if got := readShimRecord(t, d).CPUSlotsPerCore; got != 3 {
		t.Fatalf("a reinstall without the flag changed the recorded ratio to %d, want 3 preserved", got)
	}
	if err := runInstall(d, installOpts{memoryMax: "16G", cpuSlotsPerCore: 2}); err != nil {
		t.Fatal(err)
	}
	if content := recordBytes(t, d); !strings.Contains(content, `"cpu_slots_per_core": 2`) {
		t.Fatalf("an explicit =2 was not recorded distinctly from omission:\n%s", content)
	}
}

// verifies: AIRA-283 §3.1 persistence, ci-shim — the build stage records the
// flag, and a later build without it keeps the stored ratio.
func TestShimBuildRecordsAndPreservesTheRatio(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(nil)
	d.spawnShimDaemon = func(shimDaemonSpec) error { t.Fatal("the build stage started a daemon"); return nil }
	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild, cpuSlotsPerCore: 3}); err != nil {
		t.Fatal(err)
	}
	if got := readShimRecord(t, d).CPUSlotsPerCore; got != 3 {
		t.Fatalf("shim record ratio=%d, want 3", got)
	}
	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild}); err != nil {
		t.Fatal(err)
	}
	if got := readShimRecord(t, d).CPUSlotsPerCore; got != 3 {
		t.Fatalf("a shim rebuild without the flag changed the ratio to %d, want 3 preserved", got)
	}
}

// countDaemonRestarts counts `systemctl --user restart aira-daemon.service`.
func countDaemonRestarts(state *fakeInstallState) int {
	restarts := 0
	for _, argv := range state.commands {
		if strings.Join(argv, " ") == "systemctl --user restart "+defaultDaemonUnit {
			restarts++
		}
	}
	return restarts
}

// verifies: AIRA-283 E2 — real-slice install restarts a present daemon only
// when its unit bytes change, so a changed R would otherwise leave the live
// daemon on the OLD ratio behind a green install. A changed recorded R on a
// present daemon restarts it (and the fake daemon, like Serve, re-reads the
// record on restart); a byte-identical convergence run with the SAME R must
// still not bounce it.
func TestRealInstallRestartsAPresentDaemonWhenTheRatioChanged(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if state.daemonSlotsPerCore != 2 {
		t.Fatalf("fixture: first install's daemon adopted R=%d, want 2", state.daemonSlotsPerCore)
	}
	before := countDaemonRestarts(state)
	state.logs = nil
	if err := runInstall(d, installOpts{memoryMax: "16G", cpuSlotsPerCore: 3}); err != nil {
		t.Fatal(err)
	}
	if countDaemonRestarts(state) != before+1 {
		t.Fatalf("a changed ratio on a present daemon did not restart it (restarts %d -> %d)\nlogs:\n%s",
			before, countDaemonRestarts(state), strings.Join(state.logs, "\n"))
	}
	if state.daemonSlotsPerCore != 3 {
		t.Fatalf("live ratio after the install = %d, want 3", state.daemonSlotsPerCore)
	}
	if !strings.Contains(strings.Join(state.logs, "\n"), "cpu slots per core") {
		t.Fatalf("the restart is not explained:\n%s", strings.Join(state.logs, "\n"))
	}
	before = countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if countDaemonRestarts(state) != before {
		t.Fatal("a convergence run with an unchanged ratio bounced the live daemon")
	}
}

// verifies: AIRA-283 E2 — a running ci-shim daemon is left alone by the start
// stage (it holds the in-memory ledger), so a recorded ratio that differs from
// the one the live daemon adopted is reported as `restart required`, naming
// both numbers, rather than passing silently.
func TestShimStartReportsRestartRequiredForALiveDaemonOnAnotherRatio(t *testing.T) {
	d, state := newFakeInstall(t)
	d = shimProbeDeps(t, d, state, map[string]bool{"timeout": true})
	d.readFile = shimProcReader(nil)
	d.spawnShimDaemon = func(shimDaemonSpec) error { t.Fatal("a running daemon was replaced"); return nil }
	if err := runInstall(d, installOpts{ciValue: "shim", memoryMax: "8G", stage: installStageBuild, cpuSlotsPerCore: 3}); err != nil {
		t.Fatal(err)
	}
	state.daemonRunning, state.daemonSlotsPerCore = true, 2
	state.logs = nil
	if err := runInstall(d, installOpts{ciValue: "shim", stage: installStageStart}); err != nil {
		t.Fatal(err)
	}
	if logs := strings.Join(state.logs, "\n"); !strings.Contains(logs, "restart required: recorded R=3, live R=2") {
		t.Fatalf("no restart-required line for a live daemon on R=2 with R=3 recorded:\n%s", logs)
	}

	state.daemonSlotsPerCore = 3
	state.logs = nil
	if err := runInstall(d, installOpts{ciValue: "shim", stage: installStageStart}); err != nil {
		t.Fatal(err)
	}
	if logs := strings.Join(state.logs, "\n"); strings.Contains(logs, "restart required") {
		t.Fatalf("a live daemon already on the recorded ratio was reported as needing a restart:\n%s", logs)
	}
}

// verifies: AIRA-283 E2 — `aira install --status` prints the recorded ratio, the
// effective (normalised) one, and the live one the daemon reports, and says
// "restart required" when live and effective differ. A hand-edited
// out-of-range value prints as recorded with the default as effective.
func TestStatusReportsRecordedEffectiveAndLiveRatio(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", cpuSlotsPerCore: 3}); err != nil {
		t.Fatal(err)
	}
	state.daemonSlotsPerCore = 2
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	logs := strings.Join(state.logs, "\n")
	if !strings.Contains(logs, "cpu slots per core: recorded 3, effective 3, live 2 (restart required") {
		t.Fatalf("status lacks the recorded/effective/live ratio line:\n%s", logs)
	}

	paths, _ := d.daemonPaths()
	path := runner.InstallModePathFor(paths.StateHome)
	content := strings.Replace(recordBytes(t, d), `"cpu_slots_per_core": 3`, `"cpu_slots_per_core": 100`, 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	state.daemonRunning = false
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	logs = strings.Join(state.logs, "\n")
	if !strings.Contains(logs, "cpu slots per core: recorded 100, effective 2 (") || !strings.Contains(logs, "live unevaluated") {
		t.Fatalf("status misreports an out-of-range recorded ratio or an absent daemon:\n%s", logs)
	}
}

// verifies: AIRA-283 — `sudo aira install --cpu-slots-per-core=R` forwards the
// flag to the unprivileged leg that writes the record; dropping it would record
// nothing and report success.
func TestReexecForwardsTheCPUSlotsPerCoreFlag(t *testing.T) {
	target := installTarget{uid: 1000, gid: 1000, home: "/home/x", username: "x"}
	args := strings.Join(reexecRequestFor("/opt/aira", target, installOpts{cpuSlotsPerCore: 5}).args, " ")
	if !strings.Contains(args, "--cpu-slots-per-core=5") {
		t.Fatalf("re-exec args %q drop --cpu-slots-per-core", args)
	}
	if args := strings.Join(reexecRequestFor("/opt/aira", target, installOpts{}).args, " "); strings.Contains(args, "cpu-slots-per-core") {
		t.Fatalf("re-exec args %q forward a ratio that was not given (it would stop preservation working)", args)
	}
}

// verifies: AIRA-283 review fix -- a running daemon that predates the setting
// reports no ratio in its lock; it can only be running the historical R=2, so a
// real install that sets R=3 must restart it rather than stay green with the old
// ceiling live.
func TestRealInstallRestartsALegacyDaemonThatReportsNoRatio(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	state.daemonSlotsPerCore = 0 // a pre-AIRA-283 daemon reports no ratio
	before := countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G", cpuSlotsPerCore: 3}); err != nil {
		t.Fatal(err)
	}
	if countDaemonRestarts(state) != before+1 {
		t.Fatalf("a legacy daemon (no reported ratio) was left on R=2 after recording R=3 (restarts %d -> %d)",
			before, countDaemonRestarts(state))
	}
	// And a recorded R of 2 (the legacy value) must NOT bounce it.
	state.daemonSlotsPerCore = 0
	before = countDaemonRestarts(state)
	if err := runInstall(d, installOpts{memoryMax: "16G", cpuSlotsPerCore: 2}); err != nil {
		t.Fatal(err)
	}
	if countDaemonRestarts(state) != before {
		t.Fatal("recording the default R=2 restarted a legacy daemon that is already on R=2")
	}
}
