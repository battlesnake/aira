package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifies: AIRA-348 — the confined command never sees AIRA_CONFINE_PRIORITY,
// through a real launch (the ci-shim path; the real-slice path builds the child
// environment with the same pylib.AppendConfineChildEnvironment). A `high`
// that a hook or a caller set for ONE launch therefore cannot reach a nested
// `aira confine`, an aitest worker or a make recursion inside the job, which
// would otherwise all queue ahead of every normal job (AIRA-340's budget is per
// head, not per session). AIRA_CONFINE_OWNER, by contrast, still reaches the
// child. Mutation: drop the key from the strip set -> the child sees `high`.
func TestConfinedChildDoesNotInheritThePriorityVariable(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "child-env")
	request := ConfineRequest{
		Argv:     []string{"/bin/sh", "-c", "env > " + dump},
		SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		RuntimeDir:    t.TempDir(),
		MemoryReserve: 512 << 20, MemoryReservePinned: true,
		HighPriority: true,
		Env:          append(os.Environ(), ConfinePriorityEnv+"=high", "AIRA_CONFINE_OWNER=owner-propagates"),
	}
	if _, err := confineWithDeps(context.Background(), request, shimUnitDeps()); err != nil {
		t.Fatalf("shim confine err=%v", err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read child environment: %v", err)
	}
	owner := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, ConfinePriorityEnv+"=") {
			t.Fatalf("the confined command inherited %q", line)
		}
		owner = owner || line == "AIRA_CONFINE_OWNER=owner-propagates"
	}
	if !owner {
		t.Fatal("AIRA_CONFINE_OWNER did not reach the child: the dump is not the child's real environment, so the absence above proves nothing")
	}
}

// verifies: AIRA-348 — the same through the REAL-slice launch path (fake cgroup
// seams, real child process). Mutation: build the real path's child env
// without AppendConfineChildEnvironment's strip -> the child prints `high`.
func TestRealSliceConfinedChildDoesNotInheritThePriorityVariable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var stdout bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "prio",
		Env:          []string{"PATH=" + os.Getenv("PATH"), ConfinePriorityEnv + "=high", "AIRA_CONFINE_OWNER=owner-propagates"},
		Argv:         reportChildEnv(ConfinePriorityEnv, "AIRA_CONFINE_OWNER"),
		HighPriority: true,
		SelfPath:     os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, confineUnitDeps(&confineFakeScope{}))
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := stdout.String(); got != "|owner-propagates" {
		t.Fatalf("child saw %s|AIRA_CONFINE_OWNER = %q, want \"|owner-propagates\" (priority stripped, owner kept)", ConfinePriorityEnv, got)
	}
}
