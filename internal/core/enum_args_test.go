package core

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"aira/internal/domain"
)

// verifies: AIRA-210 -- every declared closed enum is enforced where every
// core-routed request passes (Do), before any handler runs.

// enumBearingArgs lists every (verb, arg) that declares a closed enum, in a
// stable order.
func enumBearingArgs(c *Core) []struct {
	spec verbSpec
	arg  ArgSpec
} {
	names := make([]string, 0, len(c.verbs))
	for name := range c.verbs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []struct {
		spec verbSpec
		arg  ArgSpec
	}
	for _, name := range names {
		for _, arg := range c.verbs[name].Args {
			if len(arg.Enum) > 0 {
				out = append(out, struct {
					spec verbSpec
					arg  ArgSpec
				}{c.verbs[name], arg})
			}
		}
	}
	return out
}

func TestEveryDeclaredEnumIsEnforcedAtDispatch(t *testing.T) {
	c := New(nil)
	cases := enumBearingArgs(c)
	if len(cases) == 0 {
		t.Fatal("no enum-bearing dispatch args found; the walk proves nothing")
	}
	t.Logf("enum-bearing dispatch args walked: %d", len(cases))
	const bad = "__not-a-member__"
	for _, tc := range cases {
		tc := tc
		t.Run(tc.spec.Name+"/"+tc.arg.Name, func(t *testing.T) {
			var response Response
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				response = c.Do(context.Background(), Request{Verb: tc.spec.Name, Args: map[string]any{tc.arg.Name: bad}})
			}()
			if panicked {
				t.Fatalf("handler ran (panicked) instead of the dispatch enum check refusing %s %s", tc.spec.Name, tc.arg.Name)
			}
			if response.OK || response.Code != "E_ARGUMENT_INVALID" || response.Exit != 2 {
				t.Fatalf("%s %s=%q: code=%q exit=%d ok=%v error=%q, want E_ARGUMENT_INVALID exit 2", tc.spec.Name, tc.arg.Name, bad, response.Code, response.Exit, response.OK, response.Error)
			}
			for _, want := range append([]string{tc.spec.Name, tc.arg.Name, `"` + bad + `"`}, tc.arg.Enum...) {
				if !strings.Contains(response.Error, want) {
					t.Fatalf("refusal %q does not name %q", response.Error, want)
				}
			}
		})
	}
}

func TestEnumValuesCanonicalisedAtDispatch(t *testing.T) {
	c := New(nil)
	for _, tc := range enumBearingArgs(c) {
		for _, v := range tc.arg.Enum {
			for _, typed := range []string{strings.ToUpper(v), " " + v + " ", v} {
				in := map[string]any{tc.arg.Name: typed, "untouched": "x"}
				snapshot := map[string]any{tc.arg.Name: typed, "untouched": "x"}
				out, err := canonicalEnumArgs(tc.spec, in)
				if err != nil {
					t.Fatalf("%s %s=%q refused: %v", tc.spec.Name, tc.arg.Name, typed, err)
				}
				if got := out[tc.arg.Name]; got != v {
					t.Fatalf("%s %s=%q canonicalised to %#v, want %q", tc.spec.Name, tc.arg.Name, typed, got, v)
				}
				if out["untouched"] != "x" {
					t.Fatalf("unrelated key lost: %#v", out)
				}
				if !reflect.DeepEqual(in, snapshot) {
					t.Fatalf("caller's map was mutated: %#v want %#v", in, snapshot)
				}
			}
		}
	}
}

func TestEnumCanonicalisationEdges(t *testing.T) {
	c := New(nil)
	mv, rant := c.verbs["mv"], c.verbs["rant"]
	// An optional empty (or whitespace) value is the absent value: untouched.
	for _, empty := range []string{"", "   "} {
		out, err := canonicalEnumArgs(rant, map[string]any{"severity": empty})
		if err != nil || out["severity"] != empty {
			t.Fatalf("optional empty %q: out=%#v err=%v", empty, out, err)
		}
	}
	// A nil value is absent.
	if out, err := canonicalEnumArgs(rant, map[string]any{"severity": nil}); err != nil || out["severity"] != nil {
		t.Fatalf("nil value: out=%#v err=%v", out, err)
	}
	// A required empty value is refused (mv status).
	if _, err := canonicalEnumArgs(mv, map[string]any{"status": ""}); err == nil || !strings.Contains(err.Error(), "E_ARGUMENT_INVALID") {
		t.Fatalf("required empty: err=%v", err)
	}
	// A non-string value is refused.
	if _, err := canonicalEnumArgs(rant, map[string]any{"severity": 7}); err == nil || !strings.Contains(err.Error(), `"7"`) {
		t.Fatalf("non-string: err=%v", err)
	}
	// The refusal echoes the value exactly as typed, not folded or trimmed.
	_, err := canonicalEnumArgs(rant, map[string]any{"severity": "BLOCKERX"})
	if err == nil || !strings.Contains(err.Error(), `"BLOCKERX"`) || strings.Contains(err.Error(), `"blockerx"`) {
		t.Fatalf("raw echo: err=%v", err)
	}
	want := `E_ARGUMENT_INVALID: rant severity "P1" is invalid; allowed: papercut, annoyance, blocker`
	if _, err := canonicalEnumArgs(rant, map[string]any{"severity": "P1"}); err == nil || err.Error() != want {
		t.Fatalf("refusal text = %v, want %q", err, want)
	}
	// A verb without enum args is untouched and the very same map comes back.
	in := map[string]any{"prefix": "X"}
	out, err := canonicalEnumArgs(c.verbs["id"], in)
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("no-enum verb: out=%#v err=%v", out, err)
	}
	// Nil map is fine.
	if out, err := canonicalEnumArgs(mv, nil); err != nil || len(out) != 0 {
		t.Fatalf("nil map: out=%#v err=%v", out, err)
	}
}

func TestDispatchRefusalTextForMvAndInverseLinkKinds(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	ctx := context.Background()
	created := c.Do(ctx, Request{Verb: "create", Args: map[string]any{"title": "enum target", "kind": "feature", "severity": "P2"}})
	if !created.OK {
		t.Fatalf("create: %#v", created)
	}
	id := fmt.Sprint(created.Data.(map[string]any)["id"])
	got := c.Do(ctx, Request{Verb: "mv", Args: map[string]any{"selector": id, "status": "in_progress"}})
	want := `E_ARGUMENT_INVALID: mv status "in_progress" is invalid; allowed: draft, planned, in-progress, in-review, done, retired, superseded`
	if got.OK || got.Code != "E_ARGUMENT_INVALID" || got.Exit != 2 || got.Error != want {
		t.Fatalf("mv in_progress: %#v, want %q exit 2", got, want)
	}
	// Case-insensitive match is widened, not refused.
	moved := c.Do(ctx, Request{Verb: "mv", Args: map[string]any{"selector": id, "status": "IN-PROGRESS"}})
	if !moved.OK {
		t.Fatalf("mv IN-PROGRESS: %#v", moved)
	}
	// An unknown relation kind is refused at dispatch.
	other := c.Do(ctx, Request{Verb: "create", Args: map[string]any{"title": "other", "kind": "feature", "severity": "P2"}})
	otherID := fmt.Sprint(other.Data.(map[string]any)["id"])
	bad := c.Do(ctx, Request{Verb: "link", Args: map[string]any{"selector": id, "kind": "blockz", "to": otherID}})
	if bad.OK || bad.Code != "E_ARGUMENT_INVALID" || bad.Exit != 2 {
		t.Fatalf("link blockz: %#v", bad)
	}
	// Twin: an inverse kind is a MEMBER of the declared enum, so the dispatch
	// check must let it through to the handler (AIRA-291's forward-command
	// refusal), not call it an invalid enum value.
	inverse := c.Do(ctx, Request{Verb: "link", Args: map[string]any{"selector": id, "kind": "blocked-by", "to": otherID}})
	if inverse.Code == "E_ARGUMENT_INVALID" {
		t.Fatalf("inverse kind was refused by the dispatch enum check: %#v", inverse)
	}
}

func TestRantSeverityOneList(t *testing.T) {
	c := New(nil)
	var declared []string
	for _, arg := range c.verbs["rant"].Args {
		if arg.Name == "severity" {
			declared = arg.Enum
		}
	}
	want := []string{string(domain.RantSeverityPapercut), string(domain.RantSeverityAnnoyance), string(domain.RantSeverityBlocker)}
	if !reflect.DeepEqual(declared, want) {
		t.Fatalf("rant severity declaration %v != domain constants %v", declared, want)
	}
}

// verifies: AIRA-208 -- the generated Skill documents the trailer's cpu-weight
// facet as the schedule ARMED (never the weight reached), including the
// rejected-schedule suffix and the flat-curve kill switch.
func TestSkillDocumentsTheArmedCPUWeightCurve(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	skill := string(artifacts.SkillMD)
	for _, want := range []string{
		"`cpu-weight=aging(100->10@30m0s)`", ",schedule-rejected", "AIRA_CONFINE_CPUWEIGHT_SCHEDULE",
		"`aging(unevaluated)`", "ARMED, not what was reached", "`aging(100->100@30m0s)`",
	} {
		if !strings.Contains(skill, want) {
			t.Fatalf("generated Skill does not document %q", want)
		}
	}
}

// verifies: AIRA-210 -- the audit rule: a declared enum must accept every value
// the system itself prints for that argument. A merged run's only output ref is
// named "log" (runner openOutputs), `run-log --stream merged` answers with
// "stream":"log" and `show RUN-n` lists output_refs key "log", and the run-log
// handler looks that name up directly, so "log" was accepted at the CLI before the
// dispatch check existed and must still be.
func TestRunLogStreamEnumAcceptsTheNameTheSystemPrints(t *testing.T) {
	c := New(nil)
	spec := c.verbs["run-log"]
	for _, stream := range []string{"out", "err", "merged", "log", "LOG"} {
		out, err := canonicalEnumArgs(spec, map[string]any{"stream": stream})
		if err != nil {
			t.Fatalf("run-log --stream %s refused: %v", stream, err)
		}
		if want := strings.ToLower(stream); out["stream"] != want {
			t.Fatalf("run-log --stream %s canonicalised to %#v, want %q", stream, out["stream"], want)
		}
	}
	if _, err := canonicalEnumArgs(spec, map[string]any{"stream": "stdout"}); err == nil {
		t.Fatal("run-log --stream stdout must stay refused")
	}
}

// verifies: AIRA-299 -- the generated Skill tells a gate author that the
// command gate's tree holds the gate's own definition and canary files (now
// written with `&<>` literally), so a whole-tree marker scan sees its own argv.
func TestSkillDocumentsTheGateTreeHoldsItsOwnDefinitionFiles(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	skill := string(artifacts.SkillMD)
	for _, want := range []string{"gate's own definition and canary files", "Point the scan at a source directory, never at `.`"} {
		if !strings.Contains(skill, want) {
			t.Fatalf("generated Skill does not document %q", want)
		}
	}
}
