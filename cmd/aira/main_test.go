package main

import (
	"bytes"
	"strings"
	"testing"

	"aira/internal/core"
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

func TestParseArgsErrorCarriesExitInJSONResponse(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := Run([]string{"list", "--badopt", "x", "--json"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"exit":2`) {
		t.Fatalf("parse error response missing exit: %q", stdout.String())
	}
}

func TestHumanRenderingIncludesWarnings(t *testing.T) {
	var stdout bytes.Buffer
	renderHuman(core.Response{OK: true, Data: map[string]any{"id": "AIRA-1"}, Warnings: []string{"W_STALE_INDEX"}}, &stdout)
	if !strings.Contains(stdout.String(), "warning: W_STALE_INDEX") {
		t.Fatalf("human warning output = %q", stdout.String())
	}
}

func TestReadyListFlagParsesAsBoolean(t *testing.T) {
	positional, options, err := parseArgs("ready", []string{"--list"})
	if err != nil {
		t.Fatalf("ready --list parse: %v", err)
	}
	if len(positional) != 0 || options["list"] != "true" {
		t.Fatalf("ready --list args = positional=%#v options=%#v", positional, options)
	}
	request, err := buildRequest("ready", positional, options)
	if err != nil || request.Args["selector"] != nil {
		t.Fatalf("ready --list request = %#v err=%v", request, err)
	}
}
