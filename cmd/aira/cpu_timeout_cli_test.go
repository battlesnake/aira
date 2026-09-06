package main

import (
	"strings"
	"testing"
)

// verifies: AIRA-136 — the CLI face accepts --cpu-timeout on `run` exactly as it
// accepts --timeout, once, and refuses it on `confine`, which has no job
// deadline of any kind (plan §3.2).
func TestAIRA136CLIAcceptsCPUTimeoutOnce(t *testing.T) {
	t.Parallel()
	argv, options, err := parseArgs("run", []string{"--cpu-timeout", "10m", "--timeout", "30m", "--", "suite"})
	if err != nil {
		t.Fatal(err)
	}
	if options["cpu-timeout"] != "10m" || options["timeout"] != "30m" {
		t.Fatalf("options=%#v", options)
	}
	request, err := buildRequest("run", argv, options)
	if err != nil {
		t.Fatal(err)
	}
	// The CLI's hyphenated flag must reach core's underscored argument name, or
	// the value is silently dropped and the operator's bound never applied.
	if request.Args["cpu_timeout"] != "10m" || request.Args["timeout"] != "30m" {
		t.Fatalf("request args=%#v", request.Args)
	}

	if _, _, err := parseArgs("run", []string{"--cpu-timeout", "1m", "--cpu-timeout", "2m", "--", "suite"}); err == nil ||
		!strings.HasPrefix(err.Error(), "E_RUN_ARGUMENT_INVALID:") {
		t.Fatalf("a duplicate --cpu-timeout was accepted: err=%v", err)
	}
	if _, _, err := parseArgs("run", []string{"--cpu-timeout", "--", "suite"}); err == nil {
		t.Fatal("--cpu-timeout was accepted without a value")
	}
	if _, _, err := parseArgs("confine", []string{"--cpu-timeout", "1m", "--", "suite"}); err == nil {
		t.Fatal("confine accepted --cpu-timeout, which it has no deadline path to honour")
	}
}
