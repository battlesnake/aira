// verifies: AIRA-73 — the CLI face of intent-retire refuses anything but one
// non-empty selector, and refuses every option.

package main

import (
	"strings"
	"testing"

	"aira/internal/store"
)

// TestIntentRetireCLIArity pins the CLI's own gate. The verb is destructive and
// takes exactly one selector; accepting zero or two would let a mistyped
// invocation reach the store with a selector the caller did not mean.
func TestIntentRetireCLIArity(t *testing.T) {
	request, err := buildRequest("intent-retire", []string{"reconcile:main:7"}, map[string]string{})
	if err != nil {
		t.Fatalf("one selector must be accepted: %v", err)
	}
	if got, _ := request.Args["selector"].(string); got != "reconcile:main:7" {
		t.Fatalf("selector = %q, want reconcile:main:7", got)
	}
	if request.Verb != "intent-retire" {
		t.Fatalf("verb = %q", request.Verb)
	}

	for name, positional := range map[string][]string{
		"no selector":     {},
		"two selectors":   {"7", "8"},
		"blank selector":  {"   "},
		"empty selector":  {""},
		"three selectors": {"a", "b", "c"},
	} {
		if _, err := buildRequest("intent-retire", positional, map[string]string{}); err == nil {
			t.Fatalf("%s was accepted", name)
		} else if store.ErrorCode(err) != "E_SELECTOR_INVALID" {
			t.Fatalf("%s produced %s (%v), want E_SELECTOR_INVALID", name, store.ErrorCode(err), err)
		}
	}
}

// TestIntentRetireRefusesEveryOption: the verb has no options at all, and
// parseArgs must say so rather than silently discarding one.
func TestIntentRetireRefusesEveryOption(t *testing.T) {
	for _, option := range []string{"--force", "--rebuild", "--steal"} {
		_, _, err := parseArgs("intent-retire", []string{option, "7"})
		if err == nil {
			t.Fatalf("option %s was accepted for intent-retire", option)
		}
		if !strings.Contains(err.Error(), "is not valid for intent-retire") {
			t.Fatalf("option %s produced %v", option, err)
		}
	}
}
