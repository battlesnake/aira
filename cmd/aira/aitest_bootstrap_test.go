package main

import "testing"

func TestParseAitestBootstrapArgsRequiresSupervisorPID(t *testing.T) {
	if _, _, err := parseAitestBootstrapArgs(nil); err == nil {
		t.Fatal("missing --supervisor-pid must error")
	}
	_, options, err := parseAitestBootstrapArgs([]string{"--supervisor-pid", "123"})
	if err != nil || options["supervisor-pid"] != "123" {
		t.Fatalf("options=%v err=%v", options, err)
	}
}
