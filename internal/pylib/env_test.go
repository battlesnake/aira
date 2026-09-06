package pylib

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// legacyGovernorEnvironmentKeys are the coordinates AIRA-33 retired with the
// aira_xdist_governor plugin. AIRA still STRIPS every one of them; it must never
// SET one again.
var legacyGovernorEnvironmentKeys = []string{
	"AIRA_PY_LIB",
	"AIRA_GOVERNOR",
	"AIRA_GOVERNOR_CMD",
	"AIRA_GOVERNOR_MAX_WAIT",
	"AIRA_GOVERNOR_SLICE",
	"AIRA_TEST_MEM_GOVERNOR",
	"AIRA_TEST_MEM_DEFAULT",
	"AIRA_TEST_MEM_GROWTH_HEADROOM",
	"AIRA_CONFINE_RESERVE_CMD",
}

// TestAppendConfineChildEnvironmentStripsTheRetiredGovernorCoordinates is the
// unit-level half of AIRA-33's env guard (the behaviour-level half, through a
// real confine launch, is TestConfineDelegateRAMDeliversNoLegacyGovernorCoordinates
// in internal/runner).
//
// The two halves of the assertion are load-bearing TOGETHER. Absence alone would
// be satisfied by a function that returned an empty slice, or by one that never
// stripped because nothing ever set the keys; presence of AIRA_CONFINE_SCOPE_ID
// -- overwritten from a STALE inherited value, so it must be a real upsert --
// is what makes the absences mean something. The scope id itself is not
// decorative: runner.InheritedConfineScopeID reads it, and confine-reserve uses
// it as ParentScopeID, which is what stops a sub-reservation being charged to
// the slice as new work.
//
// verifies: AIRA-33
func TestAppendConfineChildEnvironmentStripsTheRetiredGovernorCoordinates(t *testing.T) {
	// Every retired key present in the input, so absence must be a strip.
	inherited := []string{"PATH=/bin", "AIRA_CONFINE_SCOPE_ID=stale-scope", ConfineParentSliceEnv + "=/sys/fs/cgroup/stale.slice"}
	for _, key := range legacyGovernorEnvironmentKeys {
		inherited = append(inherited, key+"=/stale-"+key)
	}
	got := childEnvValues(t, AppendConfineChildEnvironment(inherited, "scope-123", "/sys/fs/cgroup/custom.slice"))
	for _, key := range legacyGovernorEnvironmentKeys {
		if value, present := got[key]; present {
			t.Errorf("retired coordinate %s=%q survived; AIRA-33 deleted the plugin that read it", key, value)
		}
	}
	if got["AIRA_CONFINE_SCOPE_ID"] != "scope-123" {
		t.Fatalf("AIRA_CONFINE_SCOPE_ID=%q, want the launch scope id overwriting the stale inherited one (its absence would also make every assertion above vacuous)", got["AIRA_CONFINE_SCOPE_ID"])
	}
	// AIRA-115: the slice is the scope id's other half and must upsert the same
	// way — a stale inherited slice reaching a child would charge a grandparent's
	// slice for this job's sub-reservations.
	if got[ConfineParentSliceEnv] != "/sys/fs/cgroup/custom.slice" {
		t.Fatalf("%s=%q, want this launch's resolved slice overwriting the stale inherited one", ConfineParentSliceEnv, got[ConfineParentSliceEnv])
	}
	if got["PATH"] != "/bin" {
		t.Fatalf("the strip took an unrelated variable with it: %v", got)
	}
}

// TestAppendConfineChildEnvironmentStripsWithNoScopeID pins the "" case, which
// is the one an ordinary (non-confine) launch and a scope-id-less confine both
// take: strip, publish nothing, and in particular do not publish an empty
// AIRA_CONFINE_SCOPE_ID= that InheritedConfineScopeID would then have to reject.
//
// verifies: AIRA-33
func TestAppendConfineChildEnvironmentStripsWithNoScopeID(t *testing.T) {
	got := AppendConfineChildEnvironment([]string{"PATH=/bin", "AIRA_PY_LIB=/stale", "AIRA_CONFINE_SCOPE_ID=stale-scope", ConfineParentSliceEnv + "=/stale.slice"}, "", "")
	if strings.Join(got, "\x00") != "PATH=/bin" {
		t.Fatalf("scope-id-less launch published or retained coordinates: %v", got)
	}
}

// TestAppendConfineChildEnvironmentPublishesTheResolvedSlice is the unit half of
// AIRA-115's fix: the confined child must be told WHICH slice its job runs in,
// because `aira confine-reserve` inside it has no other way to know and used to
// default to aira.slice regardless.
//
// The stale-value input is what makes this a real upsert assertion rather than a
// pass-through one, and the empty-slice case pins that nothing publishes an
// empty coordinate (which InheritedConfineSlice would then have to reject, and
// which confineReserve would then have to refuse on).
//
// The operator's own AIRA_CONFINE_SLICE is asserted UNTOUCHED, and that is the
// load-bearing half of this test rather than a decoration. The first cut of
// AIRA-115 published the resolved path under that very name, which made every
// nested `aira confine` read its parent's absolute cgroup path as an
// operator-declared explicit --slice (ResolveConfineSlice: explicit never falls
// back), bypassing default resolution's managed-unit guard and whale fallback.
// An emitted coordinate and an operator input must not share a variable.
//
// verifies: AIRA-115
func TestAppendConfineChildEnvironmentPublishesTheResolvedSlice(t *testing.T) {
	got := childEnvValues(t, AppendConfineChildEnvironment(
		[]string{"PATH=/bin", ConfineParentSliceEnv + "=/sys/fs/cgroup/stale.slice", "AIRA_CONFINE_SLICE=operator.slice"},
		"scope-9", "/sys/fs/cgroup/user.slice/custom.slice"))
	if got[ConfineParentSliceEnv] != "/sys/fs/cgroup/user.slice/custom.slice" {
		t.Fatalf("%s=%q, want the resolved launch slice", ConfineParentSliceEnv, got[ConfineParentSliceEnv])
	}
	if got["AIRA_CONFINE_SLICE"] != "operator.slice" {
		t.Fatalf("AIRA_CONFINE_SLICE=%q, want the operator's own explicit-slice input carried through unchanged: overwriting it hands every nested confine a forged --slice", got["AIRA_CONFINE_SLICE"])
	}
	noSlice := AppendConfineChildEnvironment([]string{"PATH=/bin"}, "scope-9", "")
	for _, entry := range noSlice {
		if strings.HasPrefix(entry, ConfineParentSliceEnv+"=") {
			t.Fatalf("published an empty slice coordinate: %v", noSlice)
		}
	}
	if !IsCoordinationEnvironmentKey(ConfineParentSliceEnv) {
		t.Errorf("%s left the strip set: a nested launch would inherit its parent's slice unchanged", ConfineParentSliceEnv)
	}
	if IsCoordinationEnvironmentKey("AIRA_CONFINE_SLICE") {
		t.Error("AIRA_CONFINE_SLICE entered the strip set: the operator's explicit-slice input is theirs, not a coordinate AIRA emits, and a descendant is entitled to see it")
	}
}

// TestStripCoordinationEnvironmentStillCoversEveryRetiredKey is the guard on the
// deliberate asymmetry AIRA-33 leaves behind: nine keys that are stripped but
// never set.
//
// The temptation on a future cleanup is to "tidy" the strip set down to the one
// key still exported. That would be wrong, and silently so: a child launched
// from inside a still-running PRE-deletion delegate job inherits a live
// AIRA_PY_LIB pointing at an extant extraction directory, and carrying it into a
// conftest that guards on it is a stale-plugin import path with no error to
// announce it. Nine map entries buy that away.
//
// verifies: AIRA-33
func TestStripCoordinationEnvironmentStillCoversEveryRetiredKey(t *testing.T) {
	for _, key := range legacyGovernorEnvironmentKeys {
		if !IsCoordinationEnvironmentKey(key) {
			t.Errorf("%s left the strip set; an inherited value would now reach a child", key)
		}
		if got := StripCoordinationEnvironment([]string{"PATH=/bin", key + "=/stale"}); strings.Join(got, "\x00") != "PATH=/bin" {
			t.Errorf("StripCoordinationEnvironment kept %s: %v", key, got)
		}
	}
	if !IsCoordinationEnvironmentKey("AIRA_CONFINE_SCOPE_ID") {
		t.Error("AIRA_CONFINE_SCOPE_ID left the strip set: a nested launch would inherit its parent's scope id unchanged")
	}
}

func childEnvValues(t *testing.T, env []string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			t.Fatalf("invalid child env entry %q", entry)
		}
		if _, duplicate := values[key]; duplicate {
			t.Fatalf("duplicate child env key %q in %v", key, env)
		}
		values[key] = value
	}
	return values
}

func TestAppendAitestChildEnvironmentInjectsAndStripsStaleKeys(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	inherited := []string{
		"PATH=/bin",
		"AIRA_AITEST_LIB=/stale",
		"AIRA_AITEST_WORKER_ADMIT_CMD=/stale/aira",
		"AIRA_AITEST_BOOTSTRAP_CMD=/stale/aira",
		"AIRA_AITEST_MAX_WORKERS_FALLBACK=999",
		"AIRA_AITEST_OUTER_SCOPE=/stale/scope",
	}
	got := childEnvValues(t, AppendAitestChildEnvironment(inherited, runtimeDir, nil, "/opt/aira", "/sys/fs/cgroup/aira.slice/.aira-CONFINE-x"))
	if got["AIRA_AITEST_LIB"] == "" || got["AIRA_AITEST_LIB"] == "/stale" {
		t.Fatalf("AIRA_AITEST_LIB=%q", got["AIRA_AITEST_LIB"])
	}
	if got["AIRA_AITEST_WORKER_ADMIT_CMD"] != "/opt/aira" || got["AIRA_AITEST_BOOTSTRAP_CMD"] != "/opt/aira" {
		t.Fatalf("worker admit/bootstrap cmd=%v", got)
	}
	if got["AIRA_AITEST_MAX_WORKERS_FALLBACK"] == "999" || got["AIRA_AITEST_MAX_WORKERS_FALLBACK"] == "" {
		t.Fatalf("stale fallback count was not replaced: %q", got["AIRA_AITEST_MAX_WORKERS_FALLBACK"])
	}
	if _, err := os.Stat(filepath.Join(got["AIRA_AITEST_LIB"], "aitest", "__init__.py")); err != nil {
		t.Fatalf("injected aitest lib path is not importable: %v", err)
	}
	// AIRA-44: the launcher's own scope replaces the stale inherited one. A
	// second aitest pytest run in the same job inherits THIS value, which is why
	// bootstrap no longer has to guess the outer scope from its own cgroup.
	if got["AIRA_AITEST_OUTER_SCOPE"] != "/sys/fs/cgroup/aira.slice/.aira-CONFINE-x" {
		t.Fatalf("AIRA_AITEST_OUTER_SCOPE=%q", got["AIRA_AITEST_OUTER_SCOPE"])
	}
}

// TestAppendAitestChildEnvironmentOmitsAnUnknownOuterScope: a blank scope must
// leave no key behind at all — not an empty AIRA_AITEST_OUTER_SCOPE=, which the
// bootstrap verb would have to special-case before falling back to
// self-discovery, and which the guard would otherwise be handed as a path.
//
// verifies: AIRA-44
func TestAppendAitestChildEnvironmentOmitsAnUnknownOuterScope(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	got := childEnvValues(t, AppendAitestChildEnvironment(
		[]string{"PATH=/bin", "AIRA_AITEST_OUTER_SCOPE=/stale/scope"},
		filepath.Join(t.TempDir(), "runtime"), nil, "/opt/aira", "   "))
	if _, present := got["AIRA_AITEST_OUTER_SCOPE"]; present {
		t.Fatalf("blank outer scope was published: %v", got)
	}
	if got["AIRA_AITEST_WORKER_ADMIT_CMD"] != "/opt/aira" {
		t.Fatalf("the rest of the coordinates were dropped too: %v", got)
	}
}

func TestAppendAitestChildEnvironmentEmptyArgsAreSideEffectFree(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("XDG_DATA_HOME", dataHome)
	input := []string{"PATH=/bin", "AIRA_AITEST_WORKER_ADMIT_CMD=/stale/aira"}

	byEmptyRuntimeDir := AppendAitestChildEnvironment(input, "", nil, "/opt/aira", "/scope")
	if strings.Join(byEmptyRuntimeDir, "\x00") != "PATH=/bin" {
		t.Fatalf("empty runtimeDir retained aitest environment: %v", byEmptyRuntimeDir)
	}
	byEmptyCommand := AppendAitestChildEnvironment(input, filepath.Join(t.TempDir(), "runtime"), nil, "", "/scope")
	if strings.Join(byEmptyCommand, "\x00") != "PATH=/bin" {
		t.Fatalf("empty workerAdmitCommand retained aitest environment: %v", byEmptyCommand)
	}
	if _, err := os.Stat(dataHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("side-effect-free call extracted aitest lib: %v", err)
	}
}

func TestAppendAitestChildEnvironmentSkipsEverythingOnExtractionFailure(t *testing.T) {
	previousExtract := extractAitestForChild
	previousOnce := aitestEnvFailureOnce
	extractAitestForChild = func() (string, error) { return "", errors.New("injected aitest extraction failure") }
	aitestEnvFailureOnce = new(sync.Once)
	t.Cleanup(func() {
		extractAitestForChild = previousExtract
		aitestEnvFailureOnce = previousOnce
	})
	input := []string{"PATH=/bin", "AIRA_AITEST_LIB=/stale", "AIRA_AITEST_WORKER_ADMIT_CMD=/stale/aira"}
	var diagnostics bytes.Buffer
	first := AppendAitestChildEnvironment(input, t.TempDir(), &diagnostics, "/opt/aira", "/scope")
	second := AppendAitestChildEnvironment(input, t.TempDir(), &diagnostics, "/opt/aira", "/scope")
	for _, got := range [][]string{first, second} {
		values := childEnvValues(t, got)
		if len(values) != 1 || values["PATH"] != "/bin" {
			t.Fatalf("failure retained aitest environment: %v", got)
		}
	}
	if strings.Count(diagnostics.String(), "injected aitest extraction failure") != 1 {
		t.Fatalf("failure was not logged once: %q", diagnostics.String())
	}
}
