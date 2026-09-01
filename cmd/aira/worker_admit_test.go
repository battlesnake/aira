package main

import "testing"

func TestParseWorkerAdmitArgsRequiresJobIDOuterScopeAndEstimatedBytes(t *testing.T) {
	if _, _, err := parseWorkerAdmitArgs(nil); err == nil {
		t.Fatal("missing required options must error")
	}
	_, options, err := parseWorkerAdmitArgs([]string{"--job-id", "j1", "--outer-scope", "/outer", "--estimated-bytes", "400M"})
	if err != nil || options["job-id"] != "j1" || options["outer-scope"] != "/outer" || options["estimated-bytes"] != "400M" {
		t.Fatalf("options=%v err=%v", options, err)
	}
}
