package runner

import (
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
