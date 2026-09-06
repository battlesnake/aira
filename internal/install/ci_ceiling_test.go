package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AIRA-120. `aira install --ci` sizes the STATIC slice ceiling from a one-time
// MemAvailable snapshot with zero headroom subtracted, for a worker box
// dedicated entirely to AIRA-confined jobs.
//
// Every test here injects the MemAvailable reader. Asserting against the real
// machine's free RAM would be untestable (it moves between two reads) and would
// make the assertions vacuous on any host — the ticket asks for a mocked reader
// precisely so the measured value can be pinned exactly.

// ciMeasured is deliberately NOT a whole number of GiB: 37 GiB + 700 MiB. It
// pins two things at once — that the rendered cap is floor-to-GiB format
// quantisation (37G) and not a rounded-up 38G the machine cannot honour, and
// that the recorded provenance keeps the EXACT measured byte count rather than
// the quantised cap.
const (
	ciMeasured    = int64(37)<<30 + int64(700)<<20
	ciMeasuredCap = "37G"
)

var ciMeasuredAt = time.Date(2026, 9, 6, 11, 22, 33, 0, time.UTC)

// withCIReader injects a fixed MemAvailable snapshot and a fixed clock, and
// counts reads. The count is load-bearing: --ci is specified as a ONE-TIME
// install-time snapshot, so an implementation that polled or re-measured would
// be a different (and, on a CI box, non-reproducible) feature.
func withCIReader(d installDeps, available int64, ok bool, reason string, calls *int) installDeps {
	d.readMemAvailable = func() (int64, bool, string) {
		*calls++
		return available, ok, reason
	}
	d.now = func() time.Time { return ciMeasuredAt }
	return d
}

func sliceUnitContent(t *testing.T, state *fakeInstallState) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(state.unitDir(), "aira.slice"))
	if err != nil {
		t.Fatalf("read published aira.slice: %v", err)
	}
	return string(content)
}

func TestInstallCIArgumentShapeAndMutualExclusion(t *testing.T) {
	opts, err := parseInstallArgs([]string{"--ci"})
	if err != nil || !opts.ci || opts.memoryMax != "" {
		t.Fatalf("--ci: opts=%+v err=%v", opts, err)
	}
	// Both orders: neither flag may silently win.
	for _, args := range [][]string{
		{"--ci", "--memory-max", "16G"},
		{"--memory-max=16G", "--ci"},
	} {
		_, err := parseInstallArgs(args)
		if err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("%q must be refused as mutually exclusive, got %v", args, err)
		}
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--ci=yes"}, "does not take a value"},
		{[]string{"--ci", "--ci"}, "may occur once"},
		{[]string{"--status", "--ci"}, "--status cannot be combined with mutation options"},
	} {
		_, err := parseInstallArgs(test.args)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%q: err=%v, want %q", test.args, err, test.want)
		}
	}
}

// (a) --ci resolves to the actual measured MemAvailable at the moment of
// install — and overrides an already-installed MemoryMax exactly as an explicit
// --memory-max does.
func TestInstallCISizesTheCeilingFromTheMeasuredMemAvailable(t *testing.T) {
	d, state := newFakeInstall(t)
	calls := 0
	d = withCIReader(d, ciMeasured, true, "", &calls)

	// Install a DIFFERENT cap first. Without this the test could pass against an
	// implementation that merely preserved whatever was already on disk.
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	if got := sliceUnitContent(t, state); !strings.Contains(got, "MemoryMax=16G") {
		t.Fatalf("seed install did not publish 16G: %q", got)
	}
	if calls != 0 {
		t.Fatalf("a non-ci install read MemAvailable %d time(s); it must not read it at all", calls)
	}

	state.logs = nil
	if err := runInstall(d, installOpts{ci: true}); err != nil {
		t.Fatalf("--ci install: %v", err)
	}
	content := sliceUnitContent(t, state)
	if !strings.Contains(content, "MemoryMax="+ciMeasuredCap+"\n") {
		t.Fatalf("--ci did not size MemoryMax to the measured snapshot (want %s): %q", ciMeasuredCap, content)
	}
	// The exact byte count and the exact measurement time, not a paraphrase: a
	// dynamic or headroom-subtracting implementation cannot produce these.
	wantMarker := fmt.Sprintf("# aira-ceiling-source: ci-memavailable bytes=%d at=%s\n", ciMeasured, ciMeasuredAt.Format(time.RFC3339))
	if !strings.Contains(content, wantMarker) {
		t.Fatalf("ceiling provenance marker missing (want %q): %q", wantMarker, content)
	}
	if calls != 1 {
		t.Fatalf("--ci read MemAvailable %d time(s); it is a ONE-TIME install-time snapshot", calls)
	}
	// The cap the kernel is actually programmed with (the fake models systemd
	// applying the unit, and runInstall's own verifyLiveLimits already compared
	// the two before returning).
	if state.liveMax != int64(37)<<30 {
		t.Fatalf("live memory.max=%d, want %d", state.liveMax, int64(37)<<30)
	}
	report := strings.Join(state.logs, "\n")
	if !strings.Contains(report, "slice ceiling source: --ci MemAvailable snapshot") ||
		!strings.Contains(report, fmt.Sprint(ciMeasured)) ||
		!strings.Contains(report, "zero headroom subtracted") {
		t.Fatalf("install report does not identify the --ci snapshot: %q", report)
	}
}

// --ci refuses rather than guessing when the snapshot cannot be established, or
// cannot be expressed as a legal cap. Both are environment facts, so both are
// E_INSTALL_UNAVAILABLE, and neither may leave a mutation behind.
func TestInstallCIRefusesAnUnusableSnapshotWithoutMutating(t *testing.T) {
	for _, test := range []struct {
		name      string
		available int64
		ok        bool
		reason    string
		want      string
	}{
		{name: "unevaluated", ok: false, reason: "read-error", want: "MemAvailable is unevaluated (read-error)"},
		{name: "below the floor", available: int64(3)<<30 + int64(900)<<20, ok: true, want: "below the 4G MemoryMax floor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, state := newFakeInstall(t)
			calls := 0
			d = withCIReader(d, test.available, test.ok, test.reason, &calls)
			err := runInstall(d, installOpts{ci: true})
			if err == nil || !strings.Contains(err.Error(), CodeUnavailable) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %s containing %q", err, CodeUnavailable, test.want)
			}
			if state.writes != 0 {
				t.Fatalf("a refused --ci install performed %d write(s)", state.writes)
			}
		})
	}
}

// (c) The installed ceiling is honoured exactly the way an equivalent manual
// --memory-max value would be, because there is nothing for a downstream
// consumer to branch on: the two installs publish byte-identical units apart
// from ONE systemd COMMENT line, and program the same live memory.max. The
// daemon's admission reads that live memory.max — it never reads the unit file
// — so a difference that exists only inside a `#` comment cannot reach it.
func TestInstallCIAndEquivalentMemoryMaxAreIndistinguishableDownstream(t *testing.T) {
	ciDeps, ciState := newFakeInstall(t)
	calls := 0
	ciDeps = withCIReader(ciDeps, ciMeasured, true, "", &calls)
	if err := runInstall(ciDeps, installOpts{ci: true}); err != nil {
		t.Fatalf("--ci install: %v", err)
	}

	manualDeps, manualState := newFakeInstall(t)
	manualDeps = withCIReader(manualDeps, ciMeasured, true, "", &calls)
	if err := runInstall(manualDeps, installOpts{memoryMax: ciMeasuredCap}); err != nil {
		t.Fatalf("--memory-max install: %v", err)
	}

	ciLines := strings.Split(sliceUnitContent(t, ciState), "\n")
	manualLines := strings.Split(sliceUnitContent(t, manualState), "\n")
	if len(ciLines) != len(manualLines) {
		t.Fatalf("units differ in line count: %d vs %d", len(ciLines), len(manualLines))
	}
	differing := []int{}
	for i := range ciLines {
		if ciLines[i] != manualLines[i] {
			differing = append(differing, i)
		}
	}
	if len(differing) != 1 {
		t.Fatalf("units differ on %d line(s) %v; only the ceiling-source comment may differ", len(differing), differing)
	}
	line := ciLines[differing[0]]
	if !strings.HasPrefix(line, "# aira-ceiling-source: ") || !strings.HasPrefix(manualLines[differing[0]], "# ") {
		t.Fatalf("the differing line is not a comment: %q vs %q", line, manualLines[differing[0]])
	}
	if manualLines[differing[0]] != "# aira-ceiling-source: "+ceilingSourceStatic {
		t.Fatalf("manual install recorded %q, want the static marker", manualLines[differing[0]])
	}
	if ciState.liveMax != manualState.liveMax || ciState.liveMax != int64(37)<<30 {
		t.Fatalf("live memory.max differs: ci=%d manual=%d, want both %d", ciState.liveMax, manualState.liveMax, int64(37)<<30)
	}
	if ciState.liveHigh != manualState.liveHigh {
		t.Fatalf("live memory.high differs: ci=%d manual=%d", ciState.liveHigh, manualState.liveHigh)
	}
	// The value the daemon's admission actually reads.
	ciMax := string(ciState.cgroup["/sys/fs/cgroup/fake/aira.slice/memory.max"])
	manualMax := string(manualState.cgroup["/sys/fs/cgroup/fake/aira.slice/memory.max"])
	if ciMax != manualMax || strings.TrimSpace(ciMax) != fmt.Sprint(int64(37)<<30) {
		t.Fatalf("cgroup memory.max: ci=%q manual=%q", ciMax, manualMax)
	}
}

// An install that does NOT re-decide the cap keeps the cap (computeMemoryLimits
// reads it back) and must therefore keep its provenance too — otherwise a bare
// convergence run relabels a --ci snapshot as a hand-chosen number, which is the
// indistinguishable bare number this ticket exists to remove. An explicit
// --memory-max does re-decide it, and resets the marker.
func TestInstallCeilingSourceIsPreservedAndResetWithTheCapItDescribes(t *testing.T) {
	d, state := newFakeInstall(t)
	calls := 0
	d = withCIReader(d, ciMeasured, true, "", &calls)
	if err := runInstall(d, installOpts{ci: true}); err != nil {
		t.Fatalf("--ci install: %v", err)
	}
	wantMarker := fmt.Sprintf("# aira-ceiling-source: ci-memavailable bytes=%d at=%s\n", ciMeasured, ciMeasuredAt.Format(time.RFC3339))

	if err := runInstall(d, installOpts{}); err != nil {
		t.Fatalf("plain re-install: %v", err)
	}
	content := sliceUnitContent(t, state)
	if !strings.Contains(content, "MemoryMax="+ciMeasuredCap+"\n") || !strings.Contains(content, wantMarker) {
		t.Fatalf("a plain re-install lost the --ci cap or its provenance: %q", content)
	}
	if calls != 1 {
		t.Fatalf("MemAvailable read %d time(s); a re-install without --ci must not re-measure", calls)
	}

	if err := runInstall(d, installOpts{memoryMax: "20G"}); err != nil {
		t.Fatalf("--memory-max re-install: %v", err)
	}
	content = sliceUnitContent(t, state)
	if !strings.Contains(content, "MemoryMax=20G\n") {
		t.Fatalf("--memory-max did not re-decide the cap: %q", content)
	}
	if strings.Contains(content, "ci-memavailable") || !strings.Contains(content, "# aira-ceiling-source: "+ceilingSourceStatic+"\n") {
		t.Fatalf("a re-decided cap kept a stale --ci provenance: %q", content)
	}
}

func TestStatusReportsWhereTheCeilingCameFrom(t *testing.T) {
	d, state := newFakeInstall(t)
	calls := 0
	d = withCIReader(d, ciMeasured, true, "", &calls)
	if err := runInstall(d, installOpts{ci: true}); err != nil {
		t.Fatal(err)
	}
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	report := strings.Join(state.logs, "\n")
	if !strings.Contains(report, "slice ceiling source: --ci MemAvailable snapshot") ||
		!strings.Contains(report, fmt.Sprint(ciMeasured)) ||
		!strings.Contains(report, ciMeasuredAt.Format(time.RFC3339)) {
		t.Fatalf("status does not identify the --ci snapshot: %q", report)
	}

	// A unit written before this marker existed reports UNEVALUATED, never a
	// fabricated "static".
	path := filepath.Join(state.unitDir(), "aira.slice")
	content := sliceUnitContent(t, state)
	legacy := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# aira-ceiling-source:") {
			continue
		}
		legacy += line + "\n"
	}
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(legacy, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	if report := strings.Join(state.logs, "\n"); !strings.Contains(report, "slice ceiling source: unevaluated") {
		t.Fatalf("a marker-less unit must report unevaluated: %q", report)
	}
}

func TestDryRunReportsTheCeilingSource(t *testing.T) {
	d, state := newFakeInstall(t)
	calls := 0
	d = withCIReader(d, ciMeasured, true, "", &calls)
	d.run = func([]string, []byte) ([]byte, error) { t.Fatal("dry-run invoked a command"); return nil, nil }
	if err := runInstall(d, installOpts{ci: true, dryRun: true}); err != nil {
		t.Fatal(err)
	}
	report := strings.Join(state.logs, "\n")
	if !strings.Contains(report, "MemoryMax="+ciMeasuredCap) ||
		!strings.Contains(report, "ceiling source: --ci MemAvailable snapshot") ||
		!strings.Contains(report, fmt.Sprint(ciMeasured)) {
		t.Fatalf("dry-run does not identify the --ci snapshot: %q", report)
	}
	if state.writes != 0 {
		t.Fatalf("dry-run wrote %d time(s)", state.writes)
	}
}

// The sudo leg does no sizing: it forwards the FLAG so the unprivileged re-exec
// — the leg that renders and publishes the unit — takes the one snapshot.
func TestCIIsForwardedAsAFlagThroughTheSudoReexec(t *testing.T) {
	request := reexecRequestFor("/opt/aira", installTarget{uid: 1000, home: "/home/u"}, installOpts{ci: true})
	joined := strings.Join(request.args, " ")
	if joined != "install --ci" {
		t.Fatalf("re-exec args=%q, want %q", joined, "install --ci")
	}
	if strings.Contains(joined, "--memory-max") {
		t.Fatalf("the root leg resolved a value instead of forwarding the flag: %q", joined)
	}
}

// describeCeilingSource never invents a measurement it cannot read back.
func TestDescribeCeilingSourceIsHonestAboutUnreadableMarkers(t *testing.T) {
	for recorded, want := range map[string]string{
		"":                  "unevaluated",
		ceilingSourceStatic: "static (",
		"ci-memavailable bytes=0 at=2026-09-06T11:22:33Z":        "recorded value unevaluated",
		"ci-memavailable bytes=nonsense at=2026-09-06T11:22:33Z": "recorded value unevaluated",
		"ci-memavailable bytes=1 at=yesterday":                   "recorded value unevaluated",
		"something-else":                                         "unrecognised",
	} {
		if got := describeCeilingSource(recorded); !strings.Contains(got, want) {
			t.Fatalf("describeCeilingSource(%q)=%q, want it to contain %q", recorded, got, want)
		}
	}
}

// A hand-edited or newer-vocabulary marker must not be able to corrupt a later
// render (the same rule resolveDaemonModes applies to an unrecognised mode).
func TestCeilingSourceFallsBackToStaticForAnUnsafeInstalledMarker(t *testing.T) {
	installed := "# aira-ceiling-source: evil\"\nMemoryMax=16G\n"
	if got := ceilingSourceFor(installOpts{}, installed); got != ceilingSourceStatic {
		t.Fatalf("ceilingSourceFor=%q, want %q", got, ceilingSourceStatic)
	}
	if _, _, err := renderUnits("t.slice", "t.service", "/opt/aira", "16G", "16G", false, "bad\nline"); err == nil {
		t.Fatal("renderUnits accepted a multi-line ceiling source")
	}
}
