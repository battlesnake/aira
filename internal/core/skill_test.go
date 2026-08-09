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
	if len(artifacts.Actions) != 24 {
		t.Fatalf("actions=%d, want 24", len(artifacts.Actions))
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
		"init": SafetyReconcile, "id": SafetyMutate, "create": SafetyMutate, "show": SafetyRead,
		"grep": SafetyRead, "import": SafetyMutate, "claim": SafetyLease, "release": SafetyLease,
		"heartbeat": SafetyLease, "touch": SafetyMutate, "unlink": SafetyMutate, "ready": SafetyRead,
		"list": SafetyRead, "count": SafetyRead, "set": SafetyMutate, "mv": SafetyMutate,
		"reconcile": SafetyReconcile, "check": SafetyReconcile,
		"find/add": SafetyMutate, "find/ls": SafetyRead, "find/show": SafetyRead, "find/set": SafetyMutate,
		"link/link": SafetyMutate, "link/list": SafetyRead,
	}
	artifacts, err := GenerateSkillArtifacts(New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SafetyClass{}
	for _, action := range artifacts.Actions {
		key := action.Verb
		if action.Verb == "find" || action.Verb == "link" {
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
	for _, name := range []string{"find/add", "find/ls", "find/show", "find/set", "link/link", "link/list", "unlink/unlink"} {
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

func (unevaluatedContractStore) Check(context.Context) (store.CheckReport, error) {
	return store.CheckReport{Verdict: "unevaluated", Unevaluated: true}, nil
}
