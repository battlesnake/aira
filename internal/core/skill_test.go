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
	if len(artifacts.Actions) != 77 {
		t.Fatalf("actions=%d, want 77", len(artifacts.Actions))
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
		// AIRA-196. An agent that does not know these exist goes back to reading
		// the capture file by path, which is the token-expensive thing the verbs
		// were added to replace -- and the stdin sentence must keep saying
		// /dev/null is the DEFAULT, not the only option.
		"aira confine-log",
		"--grep PATTERN",
		"`/dev/null` BY DEFAULT",
		"aira confine --detach --stdin-connect",
		"aira confine-input",
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
		//
		// AIRA-185: drain is CLI-only for a third reason — it is a FOREGROUND,
		// connection-bound hold, so a request/response tool form could only return
		// before the hold began (a fabricated success) or block a dispatcher
		// indefinitely (S13: the admission wait is no longer client-bounded).
		if action.Verb == "confine" || action.Verb == "confine-status" || action.Verb == "drain" {
			t.Fatalf("%s leaked into generated actions; it must stay a prose-only CLI verb", action.Verb)
		}
	}
	// AIRA-185. The drain guidance is prose, exactly like confine's, so its
	// load-bearing sentences are pinned here rather than left to drift: an agent
	// that reads --timeout as an overall deadline wastes a whole deploy window,
	// and one that reads a drain as job-safety draws a conclusion the feature
	// cannot support.
	for _, want := range []string{
		"aira drain wait",
		"bounds the HELD duration only",
		"NOT client-bounded",
		"NOT job-safety",
		"best-effort contention reduction",
	} {
		if !strings.Contains(skill, want) || !strings.Contains(guide, want) {
			t.Fatalf("drain guidance missing %q from SKILL.md or the agent guide", want)
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
		"confine-list": SafetyRead, "confine-kill": SafetyExecute, "confine-budget": SafetyRead,
		// AIRA-176. The asymmetry is the point: register is the only writer of a
		// binding, audit writes nothing at all and must stay SafetyRead.
		"worktree-register": SafetyMutate, "worktree-audit": SafetyRead,
		// AIRA-196. Unlike confine/confine-status these ARE generated actions:
		// reading a captured file and writing to a job's stdin socket both have
		// honest request/response forms.
		"confine-log": SafetyRead, "confine-input": SafetyExecute,
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
			// AIRA_AITEST_ESTIMATED_BYTES is parsed by _parse_estimated_bytes
			// (internal/pylib/aitest/__init__.py), which accepts a byte count OR
			// a 1024-based size suffix (4G/512M/1GiB) matching Go
			// runner.parseMemorySize, and WARNS on a malformed value instead of
			// silently defaulting to 512M (AIRA-223, which fixed the earlier
			// silent-fallback footgun the old "4G is silently ignored" wording
			// documented). Naming the variable without its units, or claiming a
			// suffix is ignored, would reproduce this ticket's own defect class,
			// so the units and the suffix support are pinned.
			"1024-based size suffix",
			"4G",
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
			{"silently ignored", "reproduces the AIRA-223 footgun wording: a size suffix is now accepted, not silently ignored"},
			{"PLAIN INTEGER BYTE COUNT", "the env var now accepts a 1024-based size suffix, so the plain-integer-only claim is stale (AIRA-223)"},
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

// TestSkillNamesNothingFromTheCollapsedDelegateRAMModel is the same anti-stale-prose
// guard for the S2a collapse of --delegate-ram into an ordinary confine job
// (T07): the deleted aitest-bootstrap verb and the deleted delegate-ram cap-source
// token must not survive in AIRA's own instructions to other agents, the same
// fabricated-fact class as an invented zero.
//
// verifies: aitest v0.7 S2a T07
func TestSkillNamesNothingFromTheCollapsedDelegateRAMModel(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	retired := []string{
		"aitest-bootstrap",  // the deleted subprocess/verb; coordinates now come from the launch env
		"auto:delegate-ram", // the deleted cap-source token; a delegate scope is now cap-source=auto:daemon-reserve
	}
	for _, document := range []struct{ name, body string }{
		{"SKILL.md", string(artifacts.SkillMD)},
		{"guide", string(artifacts.Guide)},
	} {
		for _, phrase := range retired {
			if strings.Contains(document.body, phrase) {
				t.Errorf("%s still describes the collapsed --delegate-ram model: %q (S2a T07 deleted the mechanism; the prose must go with it)", document.name, phrase)
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
		{"estimate:p90-prior", "the basis a never-seen command's first run is capped at WHEN this box has a machine-wide p90 (AIRA-166 adds the case where it does not)"},
		{"estimate:oom-escalated", "the basis when 1.5x the OOM peak IS the reserve"},
		{",oom-on-record", "the token that proves the OOM was attributed to this signature even when another term set the number"},
		{",ceiling-clamped", "the token that says the slice ceiling cut the reserve down"},
		{",ceiling-fitted", "AIRA-153: the token that says an unpinned PRIOR was sized to the largest reserve this slice can grant — a job that RAN, not an error"},
		{"refused immediately with `E_ADMIT_TOO_LARGE` naming `required` and `cap_minus_headroom` instead of waiting: pin `--memory-reserve` (or `--memory-max`) at or below the printed `cap_minus_headroom`",
			"AIRA-151: the clamp applies only to the escalated value, so an over-ceiling reserve from any other term is refused terminally, and this names what to pass instead. AIRA-153 narrows its population — a PRIOR can no longer be over the ceiling — but the sentence stays true and stays the advice"},
		{"On a small slice the RE-RUN advice below DOES self-heal, so long as the job fits under that fitted cap",
			"AIRA-153: the sentence this REPLACES said the opposite, and would now tell an agent to give up on a case that works"},
		{"The one case it cannot help is a job OOM-killed AT that cap",
			"AIRA-153: the carve-out. An unqualified 'RE-RUN works on a small slice too' would be the opposite honesty error — a job killed AT the fitted cap is refused on re-run, not retried"},
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

// TestSkillTeachesBothColdStartBases is AIRA-166.
//
// The guide taught ONE cold-start basis — "a FIRST run ... is capped at a
// machine-wide prior (`reserve-basis=estimate:p90-prior`)" — which is true only
// where a machine-wide p90 EXISTS. It exists only once some signature in this
// box's one shared state.db has three or more recorded peaks, so on a fresh box
// or a fresh state.db resolveAdmitReserve falls through to one of its four
// post-block fallbacks and the real basis is `fallback:no-history` (or
// `:no-signature` / `:history-unavailable` / `:insufficient-samples`).
//
// An agent reading only the old sentence and then seeing `fallback:no-history`
// on a genuine first run had no way to tell the documented cold start from
// something wrong — the same class of mistake AIRA-128 filed against a
// truncated OOM tally, which is why this is a test and not a review note.
//
// verifies: AIRA-166
func TestSkillTeachesBothColdStartBases(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ text, why string }{
		{"fallback:no-history", "the basis a genuine first run gets when this box has no machine-wide p90 at all"},
		{"fallback:no-signature", "the sibling for a launch that sent no signature"},
		{"fallback:history-unavailable", "the sibling for a history read that could not be established"},
		{"fallback:insufficient-samples", "the sibling for a signature with fewer than three usable samples"},
		{"three or more recorded peaks", "WHEN the p90 exists at all — the condition the old sentence assumed silently"},
		{"is NOT a sign that something is wrong", "the whole point: a fallback basis on a first run is the documented cold start"},
		{"unpinned default (4 GiB)", "AIRA-166: what the fallback NUMBER actually is, not just what it is called"},
		{"`,ceiling-fitted` form of it described below on a slice too small to grant the whole default",
			"AIRA-153 made the fitted form the ordinary basis to see on a small slice, so the cold-start clause must name it"},
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
	// The RED direction: the exact claim AIRA-166 was filed against. A revert to
	// the unconditional sentence fails here rather than silently returning an
	// agent to the state where a fallback basis looks like a defect.
	for _, document := range []struct{ name, body string }{
		{"SKILL.md", string(artifacts.SkillMD)},
		{"guide", string(artifacts.Guide)},
	} {
		if strings.Contains(document.body, "is capped at a machine-wide prior (`reserve-basis=estimate:p90-prior`)") {
			t.Errorf("%s again claims every first run is capped at the machine-wide p90; that is true only when one EXISTS", document.name)
		}
	}
}

// TestSkillTeachesTheNeverRanEnvelope is the documentation half of AIRA-147.
//
// AIRA-128 (above) taught agents to read `terminated-by=` before a job's own
// output. That guidance has a blind spot the never-admitted case falls straight
// into: a job that NEVER RAN has no termination to attribute and emits no
// trailer at all, so an agent following the OOM lesson to the letter finds
// nothing and falls back on the exit code — which cannot answer the question.
// AIRA-138 §5.4 passes a real job's own status through verbatim, so
// E_ADMIT_SATURATED's exit 4 is byte-identical to an ordinary command that ran
// and exited 4, and 1/2/3 collide the same way.
//
// Enforced by a test for the same reason the OOM leg is: the failure mode is an
// agent confidently reporting a contended box as a broken build, and the only
// thing standing between it and that mistake is whether AIRA's own instructions
// name the signal.
//
// verifies: AIRA-147
func TestSkillTeachesTheNeverRanEnvelope(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ text, why string }{
		{"ran=no", "the token that says the command never executed, matchable literally"},
		{"The `admission=` facet is what to act on", "the facet that says WHICH never-ran case this is"},
		{"saturated", "the retry-when-the-box-frees-up case, distinct from a bad request"},
		{"`code=E_CONFINE_UNAVAILABLE`", "the host/install case that retrying will not fix"},
		{"exit `4`", "the collision that makes the exit code unable to answer this"},
		{"passed through verbatim and unmodified", "why the exit code cannot be reserved: a real job's status passes through"},
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

// TestSkillTeachesTheWaitIdiomAndThePipelineStatusTrap is the documentation half
// of AIRA-142.
//
// AIRA-142 was reported with measured production evidence: in one day on this
// box, three `pgrep -f` patterns with ZERO real jobs running matched two or
// three processes each — six live waiters, every match a waiter seeing its own
// argv or a sibling's. `until ! pgrep -f <pattern>` deadlocks by construction,
// and the deadlock is indistinguishable from a slow job, so the agent gives up
// and launches ANOTHER copy of the work. Six distinct actors wrote that shape in
// a day and two of them had an explicit warning against it in their own
// briefing, which is why this lands as an idiom to COPY (the file sentinel the
// job writes itself, which worked first time unprompted elsewhere) rather than
// as one more rule to obey.
//
// The ticket's second trap rides with it because the two compose into a false
// green: a waiter that finally exits, then reads the job's status through
// `| tail`, gets tail's `0` rather than the job's failure.
//
// This ticket was resolved as documentation only — no `aira confine --wait`
// verb — so the generated Skill and guide ARE the entire deliverable, and a
// test is the only thing standing between the guidance and a silent deletion.
// Same reason as TestSkillTeachesTheOOMVerdictAndTheColdStartSelfHeal and
// TestSkillTeachesTheNeverRanEnvelope above.
//
// verifies: AIRA-142
func TestSkillTeachesTheWaitIdiomAndThePipelineStatusTrap(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ text, why string }{
		{"never poll `pgrep`", "the heading that names the broken shape, so it is findable"},
		{"matches the full command line of EVERY process, the waiter's own included", "the mechanism: why the loop deadlocks against itself"},
		{"it matches ITSELF", "the self-match, stated so it cannot be read as a flaky-tool story"},
		{"write no waiter at all", "the common case: the harness already backgrounds and reports completion"},
		{"until grep -q '^GATE_EXIT=' ~/tmp/some-gate.log", "the file-sentinel idiom to copy for another agent's or a detached job"},
		{"never on a process name", "the rule that generalises the idiom beyond the one example"},
		{"reports `tail`'s exit code", "the pipeline trap that turns a failed job into a false green"},
		{"${PIPESTATUS[0]}", "the bash-native fix for a status already sent through a pipe"},
		{"echo \"GATE_EXIT=$?\" >> ~/tmp/some-gate.log", "the better fix: the sentinel carries the true status, so nothing can swallow it"},
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

// TestGuideDoesNotBlameTheOperatorForAClientPinnedReserve is AIRA-169.
//
// The cold-start paragraph told every agent that a reserve above the ceiling
// with no OOM escalation behind it came from "this command's own measured
// peak-history estimate, or a reserve you pinned yourself". The second half is
// an over-claim the generated documents cannot support, and it is FALSE on
// three live paths where `pinned=true` reaches the daemon with no operator flag
// anywhere:
//
//   - every `aira run` admission (admission_linux.go sends
//     `!req.DaemonEstimateMemory || req.MemoryReservePinned`, and only confine's
//     launch path sets DaemonEstimateMemory, so `aira run` — which has no
//     --memory-reserve flag at all — is ALWAYS pinned:client);
//   - `aira confine -- docker run --memory=X`, where ContainerPlan.ResolveReserve
//     charges the container limit and re-marks the request pinned;
//   - `aira confine-reserve`, the pytest governor's own reservation.
//
// AIRA-165 (PR #104) already corrected the identical over-claim in the daemon's
// own advice, which now says the reserve was "PINNED on the client side" and
// hedges the cause explicitly. This is the same sentence surviving in the one
// place every agent reads, so it takes the same wording — and the negative is
// the load-bearing half: a revert to blaming the operator fails here rather
// than teaching thousands of sessions to go looking for a flag nobody passed.
//
// verifies: AIRA-169
func TestGuideDoesNotBlameTheOperatorForAClientPinnedReserve(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	documents := []struct{ name, body string }{
		{"SKILL.md", string(artifacts.SkillMD)},
		{"guide", string(artifacts.Guide)},
	}
	for _, document := range documents {
		// RED: the exact over-claim, in both generated documents.
		if strings.Contains(document.body, "you pinned yourself") {
			t.Errorf("%s still attributes a client-pinned reserve to the operator (\"you pinned yourself\"); "+
				"pinned=true reaches the daemon with no operator flag on aira run, a charged docker --memory limit, and confine-reserve", document.name)
		}
		// Nor any near-miss rephrasing of the same blame.
		for _, blame := range []string{"a reserve you pinned", "you pinned this reserve", "the reserve you pinned"} {
			if strings.Contains(document.body, blame) {
				t.Errorf("%s asserts an operator cause the document cannot establish: %q", document.name, blame)
			}
		}
		// GREEN: the wire fact, phrased as the daemon's own advice phrases it.
		if !strings.Contains(document.body, "a reserve pinned on the client side") {
			t.Errorf("%s no longer states the fact that IS established — the reserve was pinned client-side", document.name)
		}
		// The clause must stay attached to the E_ADMIT_TOO_LARGE case it
		// explains, so this cannot pass on a stray mention elsewhere.
		if !strings.Contains(document.body, "or a reserve pinned on the client side -- the run is refused immediately with `E_ADMIT_TOO_LARGE`") {
			t.Errorf("%s: the client-pinned clause is no longer the one explaining E_ADMIT_TOO_LARGE", document.name)
		}
	}
}

// TestSkillTeachesTheWorktreeRitualAndHowToReadItsUnevaluatedFacts is AIRA-176's
// documentation half.
//
// It is a test rather than a review note for the same reason
// TestSkillTeachesTheOOMVerdictAndTheColdStartSelfHeal is: the generated
// documents are AIRA's own instructions to other agents, and the failure modes
// here are silent. An agent that never registers leaves every audit guessing
// from branch names; an agent that reads `unevaluated` as "not merged, fine"
// deletes a worktree holding work; an agent that treats an `inferred` binding
// as a declaration attaches the wrong ticket's status to a checkout. The five
// legs below are exactly those things.
//
// verifies: AIRA-176
func TestSkillTeachesTheWorktreeRitualAndHowToReadItsUnevaluatedFacts(t *testing.T) {
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ text, why string }{
		{"`aira claim <id>` and `aira worktree register <id>` together",
			"the start-of-work ritual: the lease says who is working it now, the binding says what this checkout is FOR and outlives the lease"},
		{"Before removing ANY worktree, run `aira worktree audit`",
			"the habit the feature exists to create — the owner's reported pain was hand-auditing worktrees with git status and git log"},
		{"holds work nothing else has a copy of — recover before removing",
			"the one bucket whose being wrong actually loses work; an agent must recognise it verbatim in the output"},
		{"a fact reported `unevaluated` is not a pass and not a zero",
			"the honesty framing, without which an unevaluated merge check reads as permission to delete"},
		{"never consults `@{upstream}`",
			"the measured trap: on a feature branch branch.<name>.merge usually points at ITSELF, which would report every pushed branch as merged and safe to delete"},
		{"is never promoted to the same confidence as one you registered",
			"an inferred binding is weaker evidence and must not be read as a declaration"},
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
