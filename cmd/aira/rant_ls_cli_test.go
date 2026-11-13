package main

import (
	"reflect"
	"testing"
)

// verifies: AIRA-327 — the CLI forwards every rant ls option verbatim to the
// core (which owns parsing and refusals), with --severity reaching the ls
// filter argument rather than capture's severity.
func TestCLIRantLsForwardsEveryOption(t *testing.T) {
	request, err := buildFromArgv("rant", "ls", "--status", "all", "--severity", "unset", "--tag", "Slow Tests", "--actor", "opus",
		"--grep", "50%", "--since", "7", "--limit", "20", "--newest", "--full")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"subverb": "ls", "by": "", "status": "all", "severity_filter": "unset", "tags": []string{"Slow Tests"}, "actor": "opus",
		"grep": "50%", "since": "7", "limit": "20", "newest": true, "full": true, "project": "", "prefix": ""}
	if !reflect.DeepEqual(request.Args, want) {
		t.Fatalf("args = %#v\nwant %#v", request.Args, want)
	}
	// Bare: the work-queue defaults are the core's, so nothing is invented here.
	bare, err := buildFromArgv("rant", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if bare.Args["newest"] != false || bare.Args["full"] != false || bare.Args["status"] != "" || bare.Args["limit"] != "" {
		t.Fatalf("bare ls args = %#v", bare.Args)
	}
	// The retired boolean is refused, not silently ignored.
	if _, err := buildFromArgv("rant", "ls", "--unreviewed"); err == nil {
		t.Fatal("rant ls --unreviewed accepted; it was replaced by --status")
	}
}
