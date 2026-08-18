package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"aira/internal/store"
)

func TestSkillMetadataNormalisesEveryIncludedAction(t *testing.T) {
	descriptors := New(nil).DispatchDescriptors()
	artifacts, err := GenerateSkillArtifacts(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts.Actions) != 69 {
		t.Fatalf("actions=%d, want 69", len(artifacts.Actions))
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
		"reconcile": SafetyReconcile, "check": SafetyReconcile,
		"insights/ls": SafetyRead, "insights/show": SafetyRead,
		"test-report/add": SafetyMutate, "test-report/ls": SafetyRead, "test-report/show": SafetyRead, "test-report/flaky": SafetyRead,
		"run": SafetyExecute, "run-input": SafetyExecute, "run-kill": SafetyExecute, "run-log": SafetyRead,
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
		"gate/attest": SafetyMutate, "gate/prove": SafetyMutate, "gate/review": SafetyRead,
		"gate/canary-run": SafetyReconcile, "gate/canary-show": SafetyRead,
		"gate/baseline-pin": SafetyMutate, "gate/baseline-show": SafetyRead,
	}
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SafetyClass{}
	for _, action := range artifacts.Actions {
		key := action.Verb
		if action.Verb == "find" || action.Verb == "req" || action.Verb == "link" || action.Verb == "gate" || action.Verb == "test-report" || action.Verb == "spend" || action.Verb == "quota" || action.Verb == "insights" || action.Verb == "git" || action.Verb == "rant" || action.Verb == "commands" {
			key += "/" + action.Operation
		}
		got[key] = action.Safety
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("safety=%v, want=%v", got, want)
	}
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
	if contract.DefaultExit != store.ExitForCode("E_UNREGISTERED_SENTINEL") {
		t.Fatalf("contract default exit=%d, want %d", contract.DefaultExit, store.ExitForCode("E_UNREGISTERED_SENTINEL"))
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
