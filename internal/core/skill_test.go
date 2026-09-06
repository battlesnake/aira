package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"aira/internal/codes"
	"aira/internal/store"
)

func TestSkillMetadataNormalisesEveryIncludedAction(t *testing.T) {
	descriptors := New(nil).DispatchDescriptors()
	artifacts, err := GenerateSkillArtifacts(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts.Actions) != 72 {
		t.Fatalf("actions=%d, want 72", len(artifacts.Actions))
	}
	for _, action := range artifacts.Actions {
		if action.Summary == "" || !action.Safety.Valid() || !strings.HasPrefix(action.Command, "aira ") {
			t.Fatalf("incomplete action: %#v", action)
		}
	}
	if !strings.Contains(string(artifacts.SkillMD), "`line`") || !strings.Contains(string(artifacts.SkillMD), "there is no `--line` flag") {
		t.Fatal("generated Skill does not document find add file/line encoding")
	}
	for _, action := range artifacts.Actions {
		for _, arg := range action.Args {
			if arg.Name == "subverb" || arg.Name == "list" {
				t.Fatalf("discriminator leaked into action %s/%s", action.Verb, action.Operation)
			}
		}
	}
}

func TestSkillMandatesConfineAndFramesCoordinationOptIn(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	skill := string(artifacts.SkillMD)
	guide := string(artifacts.Guide)
	// The mandatory-confine directive + the opt-in coordination framing must appear
	// in BOTH the installed SKILL.md and the agent guide (they share renderMarkdownBody).
	for _, want := range []string{
		"Confining heavy commands (mandatory)",
		"MUST be run under `aira confine",
		"project-less and needs no `.aira/config`",
		"Coordination is opt-in per project",
		"return `E_CONFIG_MISSING`",
		"`aira confine --list`",
		"`aira confine --kill <name|supervisor-pid|scope-id>`",
		"Kill the scope, not a bash wrapper",
		"Never `kill -9` the supervisor",
		"`export AIRA_CONFINE_OWNER=<stable-session-id>`",
		// AIRA-22. The guide must teach the detached form AND its exit-code trap:
		// `--detach` exits 0 when the supervisor started, which an agent reading
		// only `$?` would otherwise take as the job having succeeded.
		"aira confine --detach -- <cmd>",
		"NOT when the job succeeded",
		"aira confine --status",
		"outcome-unknown",
	} {
		if !strings.Contains(skill, want) {
			t.Fatalf("SKILL.md missing mandate/opt-in prose: %q", want)
		}
		if !strings.Contains(guide, want) {
			t.Fatalf("guide missing mandate/opt-in prose: %q", want)
		}
	}
	// The pre-AIRA-22 claim is now FALSE and must not survive anywhere: an agent
	// told confine has no --detach will keep using the fragile backgrounding
	// workaround the ticket exists to replace.
	for _, stale := range []string{
		"has no native `--detach` of its own yet",
		"backgrounding via the calling harness is the current workaround",
	} {
		if strings.Contains(skill, stale) || strings.Contains(guide, stale) {
			t.Fatalf("stale pre-AIRA-22 detach guidance remains: %q", stale)
		}
	}
	if strings.Contains(skill, "whale-run") || strings.Contains(guide, "whale-run") {
		t.Fatal("retired whale-run guidance remains")
	}
	if !strings.Contains(skill, "allowed-tools: Bash(aira *)") {
		t.Fatal("SKILL.md frontmatter missing allowed-tools scope")
	}
	// confine stays CLI-only: mandated in prose, never a generated action (Include=false)
	// so it is also never an MCP tool.
	for _, action := range artifacts.Actions {
		// confine-status is CLI-only for the same reason confine is, plus one of
		// its own: it must keep working when the daemon does not.
		if action.Verb == "confine" || action.Verb == "confine-status" {
			t.Fatalf("%s leaked into generated actions; it must stay a prose-only CLI verb", action.Verb)
		}
	}
}

func TestSkillGeneratorFailsClosedForIncompleteMetadata(t *testing.T) {
	base := New(nil).DispatchDescriptors()
	var create DispatchDescriptor
	for _, descriptor := range base {
		if descriptor.Name == "create" {
			create = descriptor
		}
	}
	for name, mutate := range map[string]func(*DispatchDescriptor){
		"summary": func(descriptor *DispatchDescriptor) { descriptor.Summary = "" },
		"safety":  func(descriptor *DispatchDescriptor) { descriptor.Safety = SafetyClass("unsafe") },
		"example": func(descriptor *DispatchDescriptor) { descriptor.Example = nil },
	} {
		caseDescriptor := create
		mutate(&caseDescriptor)
		if _, err := GenerateSkillArtifacts([]DispatchDescriptor{caseDescriptor}); err == nil {
			t.Fatalf("injected empty %s metadata was accepted", name)
		}
	}
	var find DispatchDescriptor
	for _, descriptor := range base {
		if descriptor.Name == "find" {
			find = descriptor
		}
	}
	find.Operations = find.Operations[:len(find.Operations)-1]
	if _, err := GenerateSkillArtifacts([]DispatchDescriptor{find}); err == nil {
		t.Fatal("missing grouped operation was accepted")
	}
}

func TestSkillSafetyGolden(t *testing.T) {
	want := map[string]SafetyClass{
		"init": SafetyReconcile, "id": SafetyMutate, "create": SafetyMutate, "show": SafetyRead, "review": SafetyRead,
		"grep": SafetyRead, "import": SafetyMutate, "claim": SafetyLease, "release": SafetyLease,
		"heartbeat": SafetyLease, "touch": SafetyMutate, "unlink": SafetyMutate, "ready": SafetyRead,
		"list": SafetyRead, "count": SafetyRead, "set": SafetyMutate, "mv": SafetyMutate,
		"reconcile": SafetyReconcile, "check": SafetyReconcile, "eject": SafetyReconcile,
		"intent-retire": SafetyReconcile,
		"insights/ls":   SafetyRead, "insights/show": SafetyRead,
		"test-report/add": SafetyMutate, "test-report/ls": SafetyRead, "test-report/show": SafetyRead, "test-report/flaky": SafetyRead,
		"run": SafetyExecute, "run-input": SafetyExecute, "run-kill": SafetyExecute, "run-log": SafetyRead,
		"confine-list": SafetyRead, "confine-kill": SafetyExecute,
		"time": SafetyExecute, "commands/ls": SafetyRead, "commands/count": SafetyRead,
		"git/clone": SafetyExecute, "git/fetch": SafetyExecute, "git/push": SafetyExecute, "git/ls-remote": SafetyExecute,
		"find/add": SafetyMutate, "find/ls": SafetyRead, "find/show": SafetyRead, "find/set": SafetyMutate,
		"req/add": SafetyMutate, "req/ls": SafetyRead, "req/show": SafetyRead, "req/set": SafetyMutate, "req/import": SafetyMutate,
		"link/link": SafetyMutate, "link/list": SafetyRead,
		"spend/add": SafetyMutate, "spend/ls": SafetyRead,
		"quota/add": SafetyMutate, "quota/ls": SafetyRead,
		"rant/capture": SafetyMutate, "rant/ls": SafetyRead, "rant/get": SafetyRead, "rant/review": SafetyMutate, "rant/redact": SafetyMutate,
		"gate/add": SafetyMutate, "gate/ls": SafetyRead, "gate/show": SafetyRead,
		"gate/set": SafetyMutate, "gate/run": SafetyReconcile, "gate/check": SafetyRead,
		"gate/attest": SafetyMutate, "gate/prove": SafetyMutate, "gate/review": SafetyMutate,
		"gate/canary-run": SafetyReconcile, "gate/canary-show": SafetyRead,
		"lease/ls": SafetyRead,
	}
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SafetyClass{}
	for _, action := range artifacts.Actions {
		key := action.Verb
		if action.Verb == "find" || action.Verb == "req" || action.Verb == "link" || action.Verb == "gate" || action.Verb == "test-report" || action.Verb == "spend" || action.Verb == "quota" || action.Verb == "insights" || action.Verb == "git" || action.Verb == "rant" || action.Verb == "commands" || action.Verb == "lease" {
			key += "/" + action.Operation
		}
		got[key] = action.Safety
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("safety=%v, want=%v", got, want)
	}
}

func TestGateReviewIsMutation(t *testing.T) {
	descriptor, ok := descriptorByName(New(nil).DispatchDescriptors(), "gate")
	if !ok {
		t.Fatal("gate descriptor missing")
	}
	for _, operation := range descriptor.Operations {
		if operation.Name == "review" {
			if operation.Safety != SafetyMutate {
				t.Fatalf("gate review safety=%s, want %s", operation.Safety, SafetyMutate)
			}
			return
		}
	}
	t.Fatal("gate review operation missing")
}

func TestSkillActionSetIsCanonicalAndDealiased(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, action := range artifacts.Actions {
		got[action.Verb+"/"+action.Operation] = true
	}
	for _, name := range []string{"help", "new", "get", "ls"} {
		for action := range got {
			if strings.HasPrefix(action, name+"/") {
				t.Fatalf("alias/help action leaked: %q", action)
			}
		}
	}
	for _, name := range []string{"find/add", "find/ls", "find/show", "find/set", "req/add", "req/ls", "req/show", "req/set", "req/import", "link/link", "link/list", "unlink/unlink", "git/clone", "git/fetch", "git/push", "git/ls-remote"} {
		if !got[name] {
			t.Fatalf("missing action %q", name)
		}
	}
}

func TestSkillManifestAndVersionAreDeterministic(t *testing.T) {
	descriptors := New(nil).DispatchDescriptors()
	first, err := GenerateSkillArtifacts(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateSkillArtifacts(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.Version == "" || first.Manifest.Version != second.Manifest.Version || !reflect.DeepEqual(first.SkillMD, second.SkillMD) || !reflect.DeepEqual(first.ManifestJSON, second.ManifestJSON) {
		t.Fatal("generation is not deterministic")
	}
	var manifest SkillManifest
	if err := json.Unmarshal(first.ManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Entrypoint.Type != "cli" || manifest.Entrypoint.Command != "aira" || manifest.Discovery.SkillFile != "SKILL.md" || len(manifest.Actions) != len(first.Actions) {
		t.Fatalf("host contract=%#v", manifest)
	}
	changed := append([]DispatchDescriptor(nil), descriptors...)
	for i := range changed {
		if changed[i].Name == "create" {
			changed[i].Summary += " changed"
		}
	}
	third, err := GenerateSkillArtifacts(changed)
	if err != nil {
		t.Fatal(err)
	}
	if third.Manifest.Version == first.Manifest.Version {
		t.Fatal("metadata change did not change version")
	}
}

func TestM19RunArgumentsReachGeneratedSkillAction(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"ticket": true, "phase": true, "label": true, "tool": true, "report": true, "suite": true, "config_env": true, "shard": true, "retry": true, "usage": true, "provider": true, "strict_wiring": true}
	for _, action := range artifacts.Actions {
		if action.Verb != "run" {
			continue
		}
		for _, arg := range action.Args {
			delete(want, arg.Name)
		}
		if len(want) != 0 {
			t.Fatalf("generated run action is missing M19 args: %v", want)
		}
		return
	}
	t.Fatal("generated skill has no run action")
}

func TestResponseContractMatchesUnevaluatedDo(t *testing.T) {
	contract := ResponseContract()
	if contract.UnevaluatedIsPass || !reflect.DeepEqual(contract.Verdicts, []string{"pass", "fail", "unevaluated"}) || contract.ExitCodes["UNEVALUATED"] != 3 {
		t.Fatalf("contract=%#v", contract)
	}
	response := New(unevaluatedContractStore{}).Do(context.Background(), Request{Verb: "check"})
	if response.Code != "UNEVALUATED" || response.Exit != contract.ExitCodes["UNEVALUATED"] || response.Code == "PASS" {
		t.Fatalf("response=%#v contract=%#v", response, contract)
	}
}

type unevaluatedContractStore struct{ metadataProbeStore }

func (unevaluatedContractStore) ImportRequirements(context.Context, string) (store.ImportRequirementsSummary, error) {
	return store.ImportRequirementsSummary{}, nil
}

func (unevaluatedContractStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{Verdict: "unevaluated", Unevaluated: true}, nil
}

type verdictStore struct {
	metadataProbeStore
	verdict     string
	unevaluated bool
}

func (verdictStore) ImportRequirements(context.Context, string) (store.ImportRequirementsSummary, error) {
	return store.ImportRequirementsSummary{}, nil
}

func (v verdictStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{Verdict: v.verdict, Unevaluated: v.unevaluated}, nil
}

// TestResponseContractExitsMatchRealDo cross-checks every verdict exit against
// real Do() dispatch, not only unevaluated — so a change to verdictExit that
// left the contract's literals stale would be caught (Sol/Fable finding).
func TestResponseContractExitsMatchRealDo(t *testing.T) {
	contract := ResponseContract()
	cases := []struct {
		verdict     string
		unevaluated bool
		wantCode    string
	}{
		{"pass", false, "PASS"},
		{"fail", false, "FAIL"},
		{"unevaluated", true, "UNEVALUATED"},
	}
	for _, tc := range cases {
		resp := New(verdictStore{verdict: tc.verdict, unevaluated: tc.unevaluated}).Do(context.Background(), Request{Verb: "check"})
		if resp.Code != tc.wantCode {
			t.Fatalf("verdict %q -> code %q, want %q", tc.verdict, resp.Code, tc.wantCode)
		}
		want, ok := contract.ExitCodes[tc.wantCode]
		if !ok || resp.Exit != want {
			t.Fatalf("verdict %q real exit=%d, contract exit=%d (present=%v)", tc.verdict, resp.Exit, want, ok)
		}
	}
}

// TestResponseContractDocumentsDomainCodesAndDefault guards against the
// contract presenting an incomplete vocabulary: emittable domain codes must be
// listed, and the non-exhaustive default-exit rule must be stated (Fable P2).
func TestResponseContractDocumentsDomainCodesAndDefault(t *testing.T) {
	contract := ResponseContract()
	if contract.DefaultExit != codes.ExitForCode("E_UNREGISTERED_SENTINEL") {
		t.Fatalf("contract default exit=%d, want %d", contract.DefaultExit, codes.ExitForCode("E_UNREGISTERED_SENTINEL"))
	}
	listed := map[string]bool{}
	for _, code := range contract.StableCodes {
		listed[code] = true
	}
	for _, required := range []string{
		"E_LEASE_TOKEN", "E_LEASE_HELD", "E_LEASE_EXPIRED", "E_TRANSITION_INVALID",
		"E_RELATION_INVALID", "E_RELATION_EXISTS", "E_WRITE_CONFLICT", "E_PROJECT_MISMATCH",
	} {
		if !listed[required] {
			t.Fatalf("contract omits emittable code %q", required)
		}
	}
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifacts.SkillMD), "not exhaustive") {
		t.Fatal("SKILL.md does not state the code list is non-exhaustive")
	}
}

// TestSkillVersionCoversArtifactBytes proves the version hash covers the SKILL.md
// AND guide bytes and ignores the manifest Version field (Sol #4, Fable #7).
func TestSkillVersionCoversArtifactBytes(t *testing.T) {
	m := SkillManifest{Name: "aira"}
	base, err := skillVersion(m, []byte("SKILL"), []byte("GUIDE"))
	if err != nil {
		t.Fatal(err)
	}
	guideChanged, _ := skillVersion(m, []byte("SKILL"), []byte("GUIDE-2"))
	if guideChanged == base {
		t.Fatal("version does not cover guide bytes")
	}
	skillChanged, _ := skillVersion(m, []byte("SKILL-2"), []byte("GUIDE"))
	if skillChanged == base {
		t.Fatal("version does not cover SKILL.md bytes")
	}
	withVersion := m
	withVersion.Version = "preexisting"
	ignoresVersion, _ := skillVersion(withVersion, []byte("SKILL"), []byte("GUIDE"))
	if ignoresVersion != base {
		t.Fatal("version hash must ignore the manifest Version field")
	}
}

// TestGroupedVerbLevelSafetyGolden pins the grouped verbs' verb-level Safety in
// DispatchDescriptors so a change that makes the descriptor lie (e.g. find->read)
// is caught even though Skill actions use per-operation safety (Sol #5).
func TestGroupedVerbLevelSafetyGolden(t *testing.T) {
	descriptors := New(nil).DispatchDescriptors()
	for name, want := range map[string]SafetyClass{"find": SafetyMutate, "link": SafetyMutate} {
		descriptor, ok := descriptorByName(descriptors, name)
		if !ok || descriptor.Safety != want {
			t.Fatalf("descriptor %q safety=%v, want %v (found=%v)", name, descriptor.Safety, want, ok)
		}
	}
}

// TestIncludedDescriptorsHaveMCPTool forbids an included verb that the Skill
// would surface but MCP would silently omit (Include really is the single
// predicate only if every included verb also has an MCP tool) (Fable #6).
func TestIncludedDescriptorsHaveMCPTool(t *testing.T) {
	for _, descriptor := range New(nil).DispatchDescriptors() {
		if descriptor.Include && descriptor.Name != "help" && descriptor.MCPTool == "" {
			t.Fatalf("included descriptor %q has empty MCPTool: would appear in the Skill but not MCP", descriptor.Name)
		}
	}
}

// TestGroupedCoverageTiedToDiscriminatorEnum proves that growing a grouped
// verb's discriminator enum without a matching OperationSpec, or declaring a
// duplicate operation, fails closed (Sol #2, Fable #5).
func TestGroupedCoverageTiedToDiscriminatorEnum(t *testing.T) {
	base := New(nil).DispatchDescriptors()
	find, ok := descriptorByName(base, "find")
	if !ok {
		t.Fatal("no find descriptor")
	}
	grown := find
	grown.Args = append([]ArgSpec(nil), find.Args...)
	for i := range grown.Args {
		if grown.Args[i].Name == "subverb" {
			grown.Args[i].Enum = append(append([]string(nil), grown.Args[i].Enum...), "future")
		}
	}
	if _, err := GenerateSkillArtifacts([]DispatchDescriptor{grown}); err == nil {
		t.Fatal("growing the subverb enum without an OperationSpec was accepted")
	}
	dup := find
	dup.Operations = append(append([]OperationSpec(nil), find.Operations...), find.Operations[0])
	if _, err := GenerateSkillArtifacts([]DispatchDescriptor{dup}); err == nil {
		t.Fatal("duplicate grouped operation was accepted")
	}
}

// TestSkillCommandShellQuotesMetacharacters proves a glob example is passed
// literally in the rendered command (quoted) while Argv keeps the raw token,
// so the documented command builds the same request as the tested argv (Fable #4).
func TestSkillCommandShellQuotesMetacharacters(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	var touch SkillAction
	for _, action := range artifacts.Actions {
		if action.Verb == "touch" {
			touch = action
		}
	}
	if touch.Verb == "" {
		t.Fatal("no touch action")
	}
	rawGlob := false
	for _, token := range touch.Argv {
		if token == "**/*.go" {
			rawGlob = true
		}
	}
	if !rawGlob {
		t.Fatalf("touch Argv lost the literal glob: %v", touch.Argv)
	}
	if strings.Contains(touch.Command, " **/*.go ") || !strings.Contains(touch.Command, "'**/*.go'") {
		t.Fatalf("touch Command does not shell-quote the glob: %q", touch.Command)
	}
}

// aitestSkillSection returns the body of the generated aitest section.
//
// It fails closed on a missing or empty heading rather than returning "": a
// renamed heading would otherwise make every negative assertion below
// vacuously true, which is the failure mode this whole test exists to prevent.
func aitestSkillSection(t *testing.T, name, document string) string {
	t.Helper()
	const heading = "## Running pytest suites with aitest"
	start := strings.Index(document, heading)
	if start < 0 {
		t.Fatalf("%s has no %q section", name, heading)
	}
	body := document[start+len(heading):]
	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}
	if strings.TrimSpace(body) == "" {
		t.Fatalf("%s %q section is empty", name, heading)
	}
	return body
}

// TestSkillAitestGuidanceRecommendsAnInvocationThatWorks is the doc half of
// AIRA-71.
//
// The generated aitest section used to instruct every agent to launch as a
// PLAIN `aira confine -- pytest --aitest-workers=auto` with "no
// --delegate-ram". internal/runner/confine_linux.go:757-778 wires aitest's four
// AIRA_AITEST_* coordinates ONLY when DelegateRAM is true and strips them
// otherwise, so the documented form could never reach worker-admit; it
// degraded to a single uncontained worker. The runner half of this contract is
// pinned by TestConfineNonDelegateWithPopulatedRuntimeDirDeliversNoAitestCoordinates
// and TestConfineDelegateRAMDeliversAitestCoordinates.
func TestSkillAitestGuidanceRecommendsAnInvocationThatWorks(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []struct{ name, body string }{
		{"SKILL.md", string(artifacts.SkillMD)},
		{"guide", string(artifacts.Guide)},
	} {
		// Scoped to the aitest section deliberately. The confinement section
		// (skill.go:324) already contains "--delegate-ram" several times, so a
		// whole-document strings.Contains -- the style used by
		// TestSkillMandatesConfineAndFramesCoordinationOptIn above -- would
		// pass regardless of what this section actually recommends.
		section := aitestSkillSection(t, document.name, document.body)
		for _, want := range []string{
			// The exact invocation that actually wires the coordinates.
			"aira confine --delegate-ram -- pytest --aitest-workers=auto",
			// Registration is a second, independent precondition: nothing sets
			// PYTHONPATH, so AIRA_AITEST_LIB alone does not load the plugin.
			"conftest.py",
			"pytest_plugins",
			// The accounting claim must stay scoped to what worker-admit
			// actually does. An earlier draft overstated this as "the slice
			// only ever holds this job's 512M framework overhead", which is
			// false: a delegate scope adopted after a daemon restart is
			// reconstructed at live RSS plus margin. (AIRA-77's companion
			// assertion here, the `-p no:aira_xdist_governor` migration
			// workaround, was removed by AIRA-33 along with the plugin that
			// made it necessary; TestSkillNamesNothingFromTheRetiredXdistGovernor
			// now asserts the opposite -- that the phrase is GONE.)
			"adds no slice-ledger charge",
			// AIRA_AITEST_ESTIMATED_BYTES is parsed with int(raw)
			// (internal/pylib/aitest/__init__.py:141-146): a "4G"-style value
			// raises ValueError and silently falls back to the 512M default,
			// with no warning at all (only out-of-range integers warn).
			// Naming the variable without its units would reproduce this
			// ticket's own defect class, so the units are pinned.
			"PLAIN INTEGER BYTE COUNT",
			"AIRA_AITEST_ESTIMATED_BYTES=4294967296",
		} {
			if !strings.Contains(section, want) {
				t.Fatalf("%s aitest section missing %q", document.name, want)
			}
		}
		// Retracted claims, pinned as the exact strings that were generated.
		//
		// Coverage gap, stated rather than implied: these are substring
		// assertions, so they pin the specific wrong claims this section has
		// actually shipped -- they cannot prove the prose is semantically
		// correct, and a reviewer must still read it. They exist so a KNOWN
		// regression cannot return silently.
		for _, forbidden := range []struct{ text, why string }{
			// Matching a looser phrase here would false-fail on the corrected
			// text's legitimate "WITHOUT `--delegate-ram`" failure-mode note.
			{"no `--delegate-ram`", "tells agents to omit --delegate-ram (the flag aitest requires)"},
			{"only a `--delegate-ram` launch is guaranteed", "claims delegate-ram is the only shape with a finite outer cap; --memory-max and a declared --memory-reserve are finite too, they just never receive the coordinates"},
			{"the slice only ever holds", "overstates slice accounting; see the adds-no-slice-ledger-charge assertion above"},
		} {
			if strings.Contains(section, forbidden.text) {
				t.Fatalf("%s aitest section %s: found %q", document.name, forbidden.why, forbidden.text)
			}
		}
	}
}

// TestSkillNamesNothingFromTheRetiredXdistGovernor is AIRA-33's anti-stale-prose
// guard, and it is deliberately a WHOLE-DOCUMENT scan rather than a scoped one.
//
// The generated Skill and agent guide are AIRA's own instructions to other
// agents. Prose describing a subsystem that no longer exists is not a
// documentation nit here: an agent that reads "put an aira_mem() marker on the
// heavy tests" or "block it with -p no:aira_xdist_governor" will act on it and
// get nothing, with no error to tell it so. That is the same fabricated-fact
// class as an invented zero, so it is enforced by a test rather than left to
// review.
//
// This test is FORWARD-LOOKING as much as backward: the deletion is a single
// commit, but the prose it falsified is spread across two multi-kilobyte
// WriteString blocks, and the next person editing them has no way to know which
// phrases were retired. Failing the build is that way.
//
// verifies: AIRA-33
func TestSkillNamesNothingFromTheRetiredXdistGovernor(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	// Every one of these named a real, now-deleted mechanism. Listed with what
	// it was, so a future reader can tell a genuine regression from a coincidence.
	retired := []string{
		"aira_xdist_governor",    // the deleted pytest plugin, by module name
		"AIRA_PY_LIB",            // the env var that published it to a child
		"AIRA_TEST_MEM_GOVERNOR", // armed its per-test RAM reservations
		"AIRA_GOVERNOR",          // armed / disarmed its CPU checkpoint
		"governor-slot",          // the deleted per-worker relay verb
		"aira_mem(",              // its per-test RAM marker
		"per-test gate",          // its fail-open reservation gate
		"per-test reservation",   // what that gate obtained
	}
	for _, document := range []struct{ name, body string }{
		{"SKILL.md", string(artifacts.SkillMD)},
		{"guide", string(artifacts.Guide)},
	} {
		for _, phrase := range retired {
			if strings.Contains(document.body, phrase) {
				t.Errorf("%s still describes the retired xdist governor: %q (AIRA-33 deleted the mechanism; the prose must go with it)", document.name, phrase)
			}
		}
	}
}

// TestSkillTeachesTheOOMVerdictAndTheColdStartSelfHeal is the documentation half
// of AIRA-128.
//
// The incident behind that ticket was NOT a broken estimator: a standalone
// `aira confine -- make test-lite` was capped at the machine-wide p90 prior on
// its first ever run, group-killed, and the CONSUMER's own wrapper reported the
// truncated pytest output as a large failure tally. Every AIRA-side signal that
// would have said otherwise already existed and was correct — `terminated-by=oom`
// on the trailer, exit 137, the OOM advisory — and the very next run of the same
// command was admitted at an escalated reserve with no operator action. What did
// not exist was any instruction telling an agent to LOOK at those signals: the
// generated Skill and guide, which are AIRA's own instructions to other agents,
// named `terminated-by` nowhere at all.
//
// So this is enforced by a test rather than left to review, for the same reason
// TestSkillNamesNothingFromTheRetiredXdistGovernor is: an agent that reads only
// the failure tally will act on it, and a real failure can hide among the fakes.
// The three legs below are the three things it must not lose — the verdict to
// read, the phantom-tally warning, and the self-heal that makes "re-run it" the
// correct response to a first OOM rather than "investigate the failures".
//
// verifies: AIRA-128
func TestSkillTeachesTheOOMVerdictAndTheColdStartSelfHeal(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ text, why string }{
		{"terminated-by=oom", "the trailer field that distinguishes an OOM kill from a real failure"},
		{"exits `137`", "the exit code a consumer's own wrapper sees, so a swallowed status is checkable"},
		{"UNEVALUATED run", "the honesty framing: a killed run has no result, it is not a failing result"},
		{"estimate:p90-prior", "the basis a never-seen command's first run is capped at"},
		{"estimate:oom-escalated", "the basis that proves the next run self-healed"},
		{"RE-RUN the identical command", "the correct response to a first-run OOM"},
		{"pytest -n auto", "the input-nondeterminism case no per-signature estimate can learn"},
	} {
		for _, document := range []struct{ name, body string }{
			{"SKILL.md", string(artifacts.SkillMD)},
			{"guide", string(artifacts.Guide)},
		} {
			if !strings.Contains(document.body, want.text) {
				t.Errorf("%s no longer teaches %q (%s)", document.name, want.text, want.why)
			}
		}
	}
}
