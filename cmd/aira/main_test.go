package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckInvalidInvocationUsesExitTwoWithoutOpeningStore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Run([]string{"check", "unexpected", "--json"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"code":"E_SELECTOR_INVALID"`) {
		t.Fatalf("invalid invocation response = %q", stdout.String())
	}
}
