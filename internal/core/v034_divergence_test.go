// verifies: AIRA-291

package core

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aira/internal/store"
)

// TestDivergenceAdviceClearsIt runs the command the `ready` divergence warning
// names, through core.Do, and checks that the warning is gone. A message that
// named plain `aira reconcile` (which leaves the relation index alone) would
// read fine and fail here.
func TestDivergenceAdviceClearsIt(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	c := New(s)
	ctx := context.Background()
	create := func(title string) string {
		t.Helper()
		response := c.Do(ctx, Request{Verb: "create", Args: map[string]any{"title": title}})
		if !response.OK {
			t.Fatalf("create %s: %#v", title, response)
		}
		var data map[string]any
		marshalRoundTrip(t, response.Data, &data)
		return data["id"].(string)
	}
	owner, dependent := create("blocker"), create("blocked")
	do := func(verb string, args map[string]any) Response {
		t.Helper()
		response := c.Do(ctx, Request{Verb: verb, Args: args})
		if !response.OK {
			t.Fatalf("%s %v: %#v", verb, args, response)
		}
		return response
	}
	do("link", map[string]any{"from": owner, "kind": "blocks", "to": dependent})
	ownerPath := filepath.Join(base, ".aira", "tickets", owner+".md")
	withRelation, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	do("unlink", map[string]any{"from": owner, "kind": "blocks", "to": dependent})
	// The git-merge shape: the file gains the edge, the derived index does not.
	if err := os.WriteFile(ownerPath, withRelation, 0o644); err != nil {
		t.Fatal(err)
	}

	divergence := func() (string, store.ReadyRecord) {
		t.Helper()
		response := do("ready", map[string]any{"selector": dependent})
		var record store.ReadyRecord
		marshalRoundTrip(t, response.Data, &record)
		for _, finding := range record.Findings {
			if finding.Code == "E_RELATION_INDEX_DIVERGENCE" {
				return finding.Message, record
			}
		}
		return "", record
	}
	message, record := divergence()
	if message == "" {
		t.Fatalf("no divergence finding to act on: %#v", record)
	}
	if record.Ready || len(record.Blockers) != 1 {
		t.Fatalf("a relation the index has not seen must still block: %#v", record)
	}
	command := regexp.MustCompile("`(aira [^`]+)`").FindStringSubmatch(message)
	if command == nil {
		t.Fatalf("the warning names no command in backticks: %q", message)
	}
	fields := strings.Fields(command[1])
	if len(fields) < 2 {
		t.Fatalf("command %q", command[1])
	}
	args := map[string]any{}
	for _, field := range fields[2:] {
		if !strings.HasPrefix(field, "--") {
			t.Fatalf("test only parses flags: %q", command[1])
		}
		args[strings.TrimPrefix(field, "--")] = true
	}
	do(fields[1], args)

	message, record = divergence()
	if message != "" {
		t.Fatalf("`%s` did not clear the divergence warning: %q", command[1], message)
	}
	if record.Ready || len(record.Blockers) != 1 {
		t.Fatalf("clearing the warning must not change readiness: %#v", record)
	}
}
