// Package pylib_test (external): internal/runner imports internal/pylib, so a
// test that needs the runner's outcome catalogue cannot live in package pylib
// itself. This is the same layering workaround
// TestRunnerDaemonProtocolVersionMatchesTheDaemon uses.
package pylib_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"aira/internal/runner"
)

func supervisorSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("aitest", "supervisor.py"))
	if err != nil {
		t.Fatalf("read supervisor.py: %v", err)
	}
	return string(data)
}

// pythonSetLiteral extracts the string literals of a `name = frozenset((...))`
// or `name = {...}` block, which is enough structure for a vocabulary pin
// without importing a Python parser.
func pythonSetLiteral(t *testing.T, source, name string) []string {
	t.Helper()
	start := strings.Index(source, "\n"+name+" = ")
	if start < 0 {
		t.Fatalf("supervisor.py has no %s definition", name)
	}
	rest := source[start+1:]
	end := strings.Index(rest, "\n)")
	if closing := strings.Index(rest, "\n}"); closing >= 0 && (end < 0 || closing < end) {
		end = closing
	}
	if end < 0 {
		t.Fatalf("supervisor.py's %s definition is not delimited as expected", name)
	}
	literal := regexp.MustCompile(`"([^"]*)"`)
	var values []string
	for _, match := range literal.FindAllStringSubmatch(rest[:end], -1) {
		values = append(values, match[1])
	}
	if len(values) == 0 {
		t.Fatalf("supervisor.py's %s definition contains no string literals", name)
	}
	slices.Sort(values)
	return values
}

// TestWorkerAdmitOutcomeVocabularyMatchesTheSupervisor holds the Go and Python
// halves of the worker-admit outcome vocabulary equal in BOTH directions.
//
// The vocabulary is necessarily duplicated: the producers are Go and the
// consumer is Python, and there is no shared runtime to define it in. This test
// is what makes that duplication safe — the same prevention shape AIRA-66 used
// for the go:embed manifest. A value added on one side and not the other fails
// the build here rather than becoming a runtime WorkerAdmitContractViolation on
// somebody's test run.
//
// verifies: AIRA-42
func TestWorkerAdmitOutcomeVocabularyMatchesTheSupervisor(t *testing.T) {
	source := supervisorSource(t)

	if marker := fmt.Sprintf("_OUTCOME_MARKER = %q", runner.WorkerAdmitOutcomeMarker); !strings.Contains(source, marker) {
		t.Fatalf("supervisor.py does not declare %s", marker)
	}

	goStates := runner.WorkerAdmitStates()
	pyStates := pythonSetLiteral(t, source, "_OUTCOME_STATES")
	if !slices.Equal(goStates, pyStates) {
		t.Fatalf("state catalogues differ:\n  go:     %v\n  python: %v", goStates, pyStates)
	}

	goClasses := runner.WorkerAdmitClasses()
	pyClasses := pythonSetLiteral(t, source, "_OUTCOME_CLASS_EXCEPTIONS")
	if !slices.Equal(goClasses, pyClasses) {
		t.Fatalf("class catalogues differ:\n  go:     %v\n  python: %v", goClasses, pyClasses)
	}
}

// TestSupervisorClassifiesWorkerAdmitByEnumNotBySubstring is the mechanical
// guard that the AIRA-42 class stays closed.
//
// The defect was not any one substring — it was the pattern: six times, a new
// Go error shape reached `acquire_worker` unrecognised, fell through to
// "the daemon is gone", and the fix each time was to add one more `in message`
// probe. This test fails if any of the retired prose tokens reappears anywhere
// in supervisor.py, or if the classifier grows a message-inspection idiom.
//
// Deliberately NOT flagged: the two `startswith` calls that parse the
// aitest-bootstrap verb's own `outer=`/`supervisor_scope=` line and the worker
// result pipe's `_EVENT_LINE_PREFIX`. Both are framing on structured records
// this fix does not touch, not classification of a human sentence — the
// distinction the assertion below encodes by naming the retired tokens
// explicitly rather than banning `startswith` outright.
//
// verifies: AIRA-42
func TestSupervisorClassifiesWorkerAdmitByEnumNotBySubstring(t *testing.T) {
	source := supervisorSource(t)

	// Every prose token the retired classifier matched on, plus the reason
	// prefixes that carried the disposition before it had a field of its own.
	for _, retired := range []string{
		"worker-admit denied",
		"worker-admit timeout",
		"worker-admit unevaluated",
		"local-placement-failed",
		"reject:",
		"fallback:",
		"E_DAEMON_PROTOCOL",
		"E_CONFINE_ARGUMENT_INVALID",
		"i/o timeout",
		"connection reset",
	} {
		if strings.Contains(source, retired) {
			t.Errorf("supervisor.py still mentions the retired prose token %q — the "+
				"classification must come from the structured outcome line's `class` "+
				"field, and nothing may reintroduce a second, prose channel", retired)
		}
	}

	// The classifier itself must not inspect message text at all.
	classifier := functionBody(t, source, "def acquire_worker")
	for _, idiom := range []string{"in message", "in stderr", "in diagnostic", "in line"} {
		if strings.Contains(classifier, idiom) {
			t.Errorf("acquire_worker inspects message text (%q); classification belongs "+
				"to the relay, and this side may only look up `class` by exact value", idiom)
		}
	}
	if !strings.Contains(classifier, "_OUTCOME_CLASS_EXCEPTIONS[outcome[\"class\"]]") {
		t.Error("acquire_worker must dispatch through the exact-match class table")
	}
}

// functionBody returns the source of the def whose header contains header, up
// to the next top-level-of-class def.
func functionBody(t *testing.T, source, header string) string {
	t.Helper()
	start := strings.Index(source, header)
	if start < 0 {
		t.Fatalf("supervisor.py has no %s", header)
	}
	rest := source[start:]
	if next := strings.Index(rest[len(header):], "\n    def "); next >= 0 {
		return rest[:len(header)+next]
	}
	return rest
}
