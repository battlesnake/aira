package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/core"
)

// verifies: AIRA-210 -- the CLI refuses an option its operation does not declare,
// by the SAME table MCP validates against (OperationSpec.Args intersected with
// the verb's ArgSpec list), so an enum-bearing option that a subverb's builder
// does not forward can no longer bypass Do's enum check and exit 0.

func buildFromArgv(argv ...string) (core.Request, error) {
	positional, options, err := parseArgs(argv[0], argv[1:])
	if err != nil {
		return core.Request{}, err
	}
	return buildRequest(argv[0], positional, options)
}

// mcpDeclares reports whether MCP's aira_<tool> would accept `argument` on
// `operation`, read from the same helper decodeMCPRequest validates against.
func mcpDeclares(t *testing.T, verb, operation, argument string) bool {
	t.Helper()
	descriptor, ok := descriptorNamed(core.New(nil).DispatchDescriptors(), verb)
	if !ok {
		t.Fatalf("no descriptor for %s", verb)
	}
	for _, arg := range makeMCPOperation(descriptor, operation).declaredArgs {
		if arg.Name == argument {
			return true
		}
	}
	return false
}

// Every counterexample reported against b48adc5: an option the operation does
// not declare is accepted-and-discarded on the base, exit 0.
func TestCLIRefusesOptionsTheOperationDoesNotDeclare(t *testing.T) {
	cases := []struct {
		argv      []string
		verb, op  string
		option    string
		mcpAbsent string // the MCP argument name that is not declared
	}{
		{[]string{"rant", "ls", "--severity", "P1"}, "rant", "ls", "severity", "severity"},
		{[]string{"rant", "ls", "--severity", "annoyance"}, "rant", "ls", "severity", "severity"},
		{[]string{"rant", "ls", "--outcome", "bogus"}, "rant", "ls", "outcome", "outcome"},
		{[]string{"rant", "get", "RANT-1", "--by", "bogus"}, "rant", "get", "by", "by"},
		{[]string{"rant", "another rant", "--outcome", "bogus"}, "rant", "capture", "outcome", "outcome"},
		{[]string{"rant", "capture", "another rant", "--note", "x"}, "rant", "capture", "note", "note"},
		{[]string{"rant", "review", "RANT-1", "--severity", "blocker"}, "rant", "review", "severity", "severity"},
		{[]string{"find", "ls", "--severity", "P9"}, "find", "ls", "severity", "severity"},
		{[]string{"find", "ls", "--verdict", "nope"}, "find", "ls", "verdict", "verdict"},
		{[]string{"find", "ls", "--disposition", "nope"}, "find", "ls", "disposition", "disposition"},
		{[]string{"find", "add", "AIRA-1", "--disposition", "waived"}, "find", "add", "disposition", "disposition"},
		{[]string{"find", "set", "F-1", "--severity", "P1"}, "find", "set", "severity", "severity"},
		{[]string{"gate", "ls", "--verdict", "bogus"}, "gate", "ls", "verdict", "verdict"},
		// A VALID value is refused too: accepted-and-discarded reads as applied.
		{[]string{"gate", "ls", "--checker", "command"}, "gate", "ls", "checker", "checker"},
		{[]string{"gate", "ls", "--checker", "bogus"}, "gate", "ls", "checker", "checker"},
		{[]string{"gate", "show", "G", "--verdict", "pass"}, "gate", "show", "verdict", "verdict"},
		{[]string{"req", "ls", "--status", "built"}, "req", "ls", "status", "status"},
		{[]string{"req", "show", "AR-1", "--fields", "id"}, "req", "show", "fields", "fields"},
		{[]string{"spend", "ls", "--provider", "openai"}, "spend", "ls", "provider", "provider"},
		{[]string{"quota", "ls", "--provider", "openai"}, "quota", "ls", "provider", "provider"},
		{[]string{"test-report", "ls", "--format", "go-json"}, "test-report", "ls", "format", "format"},
		{[]string{"test-report", "show", "TR-1", "--ticket", "AIRA-1"}, "test-report", "show", "ticket", "ticket"},
		{[]string{"test-report", "flaky", "--config-env", "A=B"}, "test-report", "flaky", "config-env", "env_digest"},
		// A BARE FLAG is an undeclared option too (it carries "true", not a value).
		{[]string{"test-report", "ls", "--all"}, "test-report", "ls", "all", "all"},
		{[]string{"rant", "capture", "text", "--unreviewed"}, "rant", "capture", "unreviewed", "unreviewed"},
		{[]string{"spend", "ls", "--reasoning-subset"}, "spend", "ls", "reasoning-subset", "reasoning_subset"},
		// The undeclared option among declared ones, in each SORTED position
		// (declared: by, fields; options sort as the loop visits them).
		{[]string{"find", "ls", "--actor", "a", "--by", "source", "--fields", "id"}, "find", "ls", "actor", "actor"},
		{[]string{"find", "ls", "--by", "source", "--category", "bug", "--fields", "id"}, "find", "ls", "category", "category"},
		{[]string{"find", "ls", "--by", "source", "--fields", "id", "--verdict", "pass"}, "find", "ls", "verdict", "verdict"},
		// Verbs where the verb's allowed set is shared by sibling subverbs.
		{[]string{"gate", "show", "G", "--canary_id", "C"}, "gate", "show", "canary_id", "canary_id"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			_, err := buildFromArgv(tc.argv...)
			if err == nil {
				t.Fatalf("%v: accepted; want a refusal naming --%s for %s %s", tc.argv, tc.option, tc.verb, tc.op)
			}
			want := "E_SELECTOR_INVALID: option --" + tc.option + " is not valid for " + tc.verb + " " + tc.op
			if !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("%v: error %q, want prefix %q", tc.argv, err.Error(), want)
			}
			// The refusal is MCP's: the same table does not declare the argument.
			if mcpDeclares(t, tc.verb, tc.op, tc.mcpAbsent) {
				t.Fatalf("MCP declares %s on %s %s, so the CLI refusal is stricter than MCP", tc.mcpAbsent, tc.verb, tc.op)
			}
		})
	}
}

// The false-refuse direction: every option a subverb really uses, in its real
// spelling (kebab, aliased or snake), still builds a request. Without this the
// stricter check could silently disable a working flag.
func TestCLIStillAcceptsEveryDeclaredOption(t *testing.T) {
	valid := [][]string{
		{"rant", "text", "--tag", "t", "--severity", "blocker", "--ref", "r", "--idem", "k", "--project", "p"},
		{"rant", "capture", "text", "--tag", "t", "--severity", "papercut", "--ref", "r", "--idem", "k", "--prefix", "AIRA"},
		{"rant", "ls", "--by", "tag", "--unreviewed", "--since", "5", "--tag", "t", "--prefix", "AIRA"},
		{"rant", "list", "--project", "p"},
		{"rant", "get", "RANT-1", "--project", "p"},
		{"rant", "review", "RANT-1", "--outcome", "planned", "--note", "n", "--resolved-by", "ticket:AIRA-1", "--prefix", "AIRA"},
		{"rant", "redact", "RANT-1", "--prefix", "AIRA"},
		{"find", "add", "AIRA-1", "--category", "bug", "--severity", "P1", "--verdict", "confirmed", "--source", "s", "--message", "m", "--file", "x.go:3", "--requirement", "AR-1"},
		{"find", "ls", "query", "--by", "source", "--fields", "id"},
		{"find", "list", "--by", "source"},
		{"find", "show", "F-1"},
		{"find", "set", "F-1", "--disposition", "waived", "--reason", "r", "--actor", "a"},
		{"req", "add", "The system must X", "--status", "planned"},
		{"req", "ls", "--fields", "id"},
		{"req", "show", "AR-1"},
		{"req", "set", "AR-1", "--status", "built"},
		{"req", "import", "REQUIREMENTS.md"},
		{"spend", "add", "--provider", "openai", "--model", "m", "--source", "manual", "--ticket", "AIRA-1", "--phase", "plan", "--at", "2026-01-01T00:00:00Z", "--session", "s", "--agent", "a", "--turn-id", "t1", "--total", "5", "--cost-usd", "0.1", "--usage-file", "u.json", "--bucket", "output=5", "--reasoning-subset", "--resolve-ticket"},
		{"spend", "ls", "provider:openai", "--by", "phase", "--session", "s"},
		{"quota", "add", "--provider", "openai", "--source", "manual", "--at", "2026-01-01T00:00:00Z", "--window", "5h", "--used", "1", "--limit", "2", "--remaining", "1", "--reset-at", "2026-01-01T05:00:00Z"},
		{"quota", "ls", "provider:openai"},
		{"test-report", "add", "r.json", "--format", "go-json", "--ticket", "AIRA-1", "--phase", "p", "--commit", "c", "--branch", "b", "--suite", "s", "--config", "c", "--config-env", "A=B", "--shard", "1/2", "--retry", "1"},
		{"test-report", "ls"},
		{"test-report", "show", "TR-1"},
		{"test-report", "flaky", "--explain", "TestX"},
		{"test-report", "flaky", "--all"},
		{"gate", "add", "G", "--checker", "command", "--predicate", "p", "--argv", "a", "--cwd", "root", "--env-allow", "PATH", "--timeout-ms", "5", "--output-cap-bytes", "5", "--parser", "go-test-json-v1"},
		{"gate", "set", "G", "--timeout-ms", "9"},
		{"gate", "show", "G"},
		{"gate", "run", "G"},
		{"gate", "ls"},
		{"gate", "check"},
		{"gate", "attest", "G", "--verdict", "pass", "--actor", "human"},
		{"gate", "prove", "G"},
		{"gate", "review", "G"},
		{"gate", "canary-run", "C"},
		{"gate", "canary-show", "C"},
		{"gate", "add", "G", "--checker", "check-dimension", "--dimension", "traceability"},
		{"commands", "ls", "--by", "key"},
		{"commands", "count", "--by", "key"},
	}
	for _, argv := range valid {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			if _, err := buildFromArgv(argv...); err != nil {
				t.Fatalf("%v: refused: %v", argv, err)
			}
		})
	}
}

// MCP's own table had the matching hole: an operation that declares NO arguments
// (gate ls, gate check) fell back to the verb's whole argument list, so
// aira_gate {operation: ls, verdict: pass} was accepted and discarded. A declared
// operation now restricts the arguments even when it declares none.
func TestMCPOperationWithNoArgsDeclaresNone(t *testing.T) {
	server := newMCPServer(func(context.Context, core.Request) (*core.Core, func(), error) { return nil, func() {}, nil })
	binding, ok := server.byName["aira_gate"]
	if !ok {
		t.Fatal("no aira_gate binding")
	}
	for _, operation := range []string{"ls", "check"} {
		for _, argument := range []string{"verdict", "checker", "actor"} {
			values := map[string]json.RawMessage{
				"operation": json.RawMessage(`"` + operation + `"`),
				argument:    json.RawMessage(`"pass"`),
			}
			_, _, err := decodeMCPRequest(binding, values)
			if err == nil || !strings.Contains(err.Error(), `unknown argument "`+argument+`"`) {
				t.Fatalf("gate %s %s: err=%v, want unknown argument", operation, argument, err)
			}
		}
		// Twin: the bare operation still decodes.
		if _, _, err := decodeMCPRequest(binding, map[string]json.RawMessage{"operation": json.RawMessage(`"` + operation + `"`)}); err != nil {
			t.Fatalf("gate %s: %v", operation, err)
		}
	}
	// Twin: an operation that declares an argument keeps accepting it.
	if _, _, err := decodeMCPRequest(binding, map[string]json.RawMessage{
		"operation": json.RawMessage(`"attest"`), "gate_id": json.RawMessage(`"G"`),
		"verdict": json.RawMessage(`"pass"`), "actor": json.RawMessage(`"human"`),
	}); err != nil {
		t.Fatalf("gate attest: %v", err)
	}
}

// verifies: AIRA-210 -- gate's `--canary_id` is the add/set override and is
// FORWARDED (the store binds it into canary_ids); `--gate_id` is not an option
// at all (the gate id is positional), so it is refused rather than accepted and
// dropped. Counterexample on 6fa7b3c9: `gate add G ... --canary_id my-canary`
// exited 0 with the default canary id G-canary.
func TestGateCanaryIDOptionForwardedAndGateIDRefused(t *testing.T) {
	base := []string{"gate", "add", "G", "--checker", "command", "--predicate", "exit-zero", "--argv", "true", "--env-allow", "PATH", "--timeout-ms", "1000", "--output-cap-bytes", "4096"}
	for _, subverb := range []string{"add", "set"} {
		argv := append([]string{"gate", subverb}, base[2:]...)
		without, err := buildFromArgv(argv...)
		if err != nil {
			t.Fatalf("gate %s without --canary_id: %v", subverb, err)
		}
		if _, present := without.Args["canary_id"]; present {
			t.Fatalf("gate %s without --canary_id carries canary_id %v", subverb, without.Args["canary_id"])
		}
		with, err := buildFromArgv(append(append([]string(nil), argv...), "--canary_id", "my-own-canary")...)
		if err != nil {
			t.Fatalf("gate %s --canary_id: %v", subverb, err)
		}
		if got := with.Args["canary_id"]; got != "my-own-canary" {
			t.Fatalf("gate %s --canary_id: request canary_id = %v, want my-own-canary (accepted and discarded)", subverb, got)
		}
	}
	for _, argv := range [][]string{
		append(append([]string(nil), base...), "--gate_id", "other"),
		{"gate", "show", "G", "--gate_id", "other"},
		{"gate", "run", "G", "--gate_id", "other"},
		{"gate", "canary-run", "C", "--gate_id", "other"},
	} {
		_, err := buildFromArgv(argv...)
		if err == nil || !strings.HasPrefix(err.Error(), "E_SELECTOR_INVALID: option --gate_id is not valid for gate") {
			t.Fatalf("%v: err=%v, want E_SELECTOR_INVALID naming --gate_id", argv, err)
		}
	}
	// canary-run / canary-show take the id positionally; a second spelling is
	// refused rather than one of two competing ids silently winning.
	for _, subverb := range []string{"canary-run", "canary-show"} {
		_, err := buildFromArgv("gate", subverb, "C", "--canary_id", "D")
		if err == nil || !strings.HasPrefix(err.Error(), "E_SELECTOR_INVALID: option --canary_id is not valid for gate "+subverb) {
			t.Fatalf("gate %s C --canary_id D: err=%v, want E_SELECTOR_INVALID", subverb, err)
		}
	}
}

// One fault, one code: an undeclared option is reported as itself even when a
// builder value check would also have failed, so the operator is not told one
// code now and E_SELECTOR_INVALID after fixing it.
func TestUndeclaredOptionWinsOverBuilderValueCheck(t *testing.T) {
	// `test-report add` without --format fails a builder check; --all is
	// undeclared on add.
	_, err := buildFromArgv("test-report", "add", "r.json", "--all")
	if err == nil || !strings.HasPrefix(err.Error(), "E_SELECTOR_INVALID: option --all is not valid for test-report add") {
		t.Fatalf("err=%v, want the undeclared-option refusal, not the missing --format check", err)
	}
	// Twin: with no undeclared option the builder's own check still speaks.
	_, err = buildFromArgv("test-report", "add", "r.json")
	if err == nil || !strings.Contains(err.Error(), "requires --format") {
		t.Fatalf("err=%v, want the --format builder check", err)
	}
}

// verifies: AIRA-210 -- canary-run / canary-show used to declare the nine
// mutation_* options and the store discarded them (the canary's own declaration
// is what runs): `gate canary-run C --mutation-file other` exited 0 as if it had
// retargeted the canary. Refused by name now; and `gate add --dimension` (which
// MCP declares) is reachable from the CLI.
func TestCanaryRunShowRefuseDiscardedMutationOptions(t *testing.T) {
	for _, subverb := range []string{"canary-run", "canary-show"} {
		for _, option := range []string{"mutation-kind", "mutation-file", "mutation-test", "mutation-occurrence", "mutation-pkgdir", "mutation-testname", "mutation-content", "mutation-seed", "mutation-expected-result"} {
			_, err := buildFromArgv("gate", subverb, "C", "--"+option, "1")
			if err == nil || !strings.HasPrefix(err.Error(), "E_SELECTOR_INVALID: option --"+option+" is not valid for gate "+subverb) {
				t.Fatalf("gate %s --%s: err=%v, want E_SELECTOR_INVALID", subverb, option, err)
			}
		}
	}
}

func TestGateAddForwardsDimension(t *testing.T) {
	req, err := buildFromArgv("gate", "add", "G", "--checker", "check-dimension", "--dimension", "traceability")
	if err != nil {
		t.Fatalf("gate add --dimension: %v", err)
	}
	if got := req.Args["dimension"]; got != "traceability" {
		t.Fatalf("dimension = %v, want traceability", got)
	}
}
