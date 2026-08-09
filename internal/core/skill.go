package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SkillAction is the operation-level projection shared by the manifest and
// the generated guide.
type SkillAction struct {
	Verb      string      `json:"verb"`
	Operation string      `json:"operation"`
	Summary   string      `json:"summary"`
	Safety    SafetyClass `json:"safety"`
	Args      []ArgSpec   `json:"args"`
	Command   string      `json:"command"`
}

type SkillEntrypoint struct {
	Type       string `json:"type"`
	Command    string `json:"command"`
	Invocation string `json:"invocation"`
}

type SkillDiscovery struct {
	SkillFile     string `json:"skill_file"`
	InstallTarget string `json:"install_target"`
}

type SkillManifest struct {
	Name             string               `json:"name"`
	Version          string               `json:"version"`
	Entrypoint       SkillEntrypoint      `json:"entrypoint"`
	Discovery        SkillDiscovery       `json:"discovery"`
	ResponseContract ResponseContractSpec `json:"response_contract"`
	Actions          []SkillAction        `json:"actions"`
}

type SkillArtifacts struct {
	SkillMD      []byte
	Guide        []byte
	ManifestJSON []byte
	Manifest     SkillManifest
	Actions      []SkillAction
}

// GenerateSkillArtifacts deterministically projects descriptors into the
// installable Skill files. It performs no filesystem or process I/O.
func GenerateSkillArtifacts(descriptors []DispatchDescriptor) (SkillArtifacts, error) {
	actions, err := normaliseSkillActions(descriptors)
	if err != nil {
		return SkillArtifacts{}, err
	}
	contract := ResponseContract()
	manifest := SkillManifest{
		Name: "aira",
		Entrypoint: SkillEntrypoint{
			Type: "cli", Command: "aira", Invocation: "aira <verb> [args]",
		},
		Discovery:        SkillDiscovery{SkillFile: "SKILL.md", InstallTarget: "<dir>"},
		ResponseContract: contract,
		Actions:          actions,
	}
	skillMD := []byte(renderSkillMarkdown(actions, contract))
	versionInput, err := json.Marshal(struct {
		Name             string               `json:"name"`
		Entrypoint       SkillEntrypoint      `json:"entrypoint"`
		Discovery        SkillDiscovery       `json:"discovery"`
		ResponseContract ResponseContractSpec `json:"response_contract"`
		Actions          []SkillAction        `json:"actions"`
	}{
		Name: manifest.Name, Entrypoint: manifest.Entrypoint, Discovery: manifest.Discovery,
		ResponseContract: manifest.ResponseContract, Actions: manifest.Actions,
	})
	if err != nil {
		return SkillArtifacts{}, fmt.Errorf("marshal manifest for version: %w", err)
	}
	hashInput := append(append([]byte(nil), versionInput...), skillMD...)
	digest := sha256.Sum256(hashInput)
	manifest.Version = hex.EncodeToString(digest[:])
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return SkillArtifacts{}, fmt.Errorf("marshal manifest: %w", err)
	}
	manifestJSON = append(manifestJSON, '\n')
	return SkillArtifacts{SkillMD: skillMD, Guide: []byte(renderGuideMarkdown(actions, contract)), ManifestJSON: manifestJSON, Manifest: manifest, Actions: actions}, nil
}

// GenerateSkill is the concise alias used by callers that need the complete
// generated Skill package.
func GenerateSkill(descriptors []DispatchDescriptor) (SkillArtifacts, error) {
	return GenerateSkillArtifacts(descriptors)
}

func normaliseSkillActions(descriptors []DispatchDescriptor) ([]SkillAction, error) {
	actions := make([]SkillAction, 0)
	seen := map[string]bool{}
	for _, descriptor := range descriptors {
		if !descriptor.Include || descriptor.Name == "help" || descriptor.Name == "new" || descriptor.Name == "get" || descriptor.Name == "ls" {
			continue
		}
		if strings.TrimSpace(descriptor.Summary) == "" || !descriptor.Safety.Valid() {
			return nil, fmt.Errorf("descriptor %q has incomplete verb metadata", descriptor.Name)
		}
		if len(descriptor.Operations) == 0 {
			if descriptor.Example == nil {
				return nil, fmt.Errorf("descriptor %q has no verb example", descriptor.Name)
			}
			action := SkillAction{Verb: descriptor.Name, Operation: descriptor.Name, Summary: descriptor.Summary, Safety: descriptor.Safety, Args: copyArgSpecs(descriptor.Args), Command: commandFor(descriptor.Name, descriptor.Example)}
			if err := validateAction(action); err != nil {
				return nil, err
			}
			key := action.Verb + "\x00" + action.Operation
			if !seen[key] {
				actions = append(actions, action)
				seen[key] = true
			}
			continue
		}
		if descriptor.Example != nil {
			return nil, fmt.Errorf("grouped descriptor %q has a verb example", descriptor.Name)
		}
		if expected := groupedOperationNames(descriptor); expected != nil {
			got := make(map[string]bool, len(descriptor.Operations))
			for _, operation := range descriptor.Operations {
				got[operation.Name] = true
			}
			for name := range expected {
				if !got[name] {
					return nil, fmt.Errorf("grouped descriptor %q is missing operation %q", descriptor.Name, name)
				}
			}
			if len(got) != len(expected) {
				return nil, fmt.Errorf("grouped descriptor %q has unexpected operations", descriptor.Name)
			}
		}
		for _, operation := range descriptor.Operations {
			if strings.TrimSpace(operation.Name) == "" || strings.TrimSpace(operation.Summary) == "" || !operation.Safety.Valid() || operation.Example == nil {
				return nil, fmt.Errorf("descriptor %q operation %q has incomplete metadata", descriptor.Name, operation.Name)
			}
			args, err := operationArgs(descriptor, operation)
			if err != nil {
				return nil, err
			}
			action := SkillAction{Verb: descriptor.Name, Operation: operation.Name, Summary: operation.Summary, Safety: operation.Safety, Args: args, Command: commandFor(descriptor.Name, operation.Example)}
			if err := validateAction(action); err != nil {
				return nil, err
			}
			key := action.Verb + "\x00" + action.Operation
			if !seen[key] {
				actions = append(actions, action)
				seen[key] = true
			}
		}
	}
	return actions, nil
}

func operationArgs(descriptor DispatchDescriptor, operation OperationSpec) ([]ArgSpec, error) {
	byName := make(map[string]ArgSpec, len(descriptor.Args))
	for _, arg := range descriptor.Args {
		byName[arg.Name] = arg
	}
	args := make([]ArgSpec, 0, len(operation.Args))
	for _, declared := range operation.Args {
		if declared.Name == discriminatorName(descriptor) {
			return nil, fmt.Errorf("descriptor %q operation %q declares discriminator %q", descriptor.Name, operation.Name, declared.Name)
		}
		arg, ok := byName[declared.Name]
		if !ok {
			return nil, fmt.Errorf("descriptor %q operation %q declares unknown arg %q", descriptor.Name, operation.Name, declared.Name)
		}
		arg.Required = declared.Required
		args = append(args, arg)
	}
	return args, nil
}

func discriminatorName(descriptor DispatchDescriptor) string {
	if descriptor.MCPOperation == "subverb" {
		return "subverb"
	}
	if descriptor.Name == "link" {
		return "list"
	}
	return ""
}

func groupedOperationNames(descriptor DispatchDescriptor) map[string]bool {
	switch descriptor.Name {
	case "find":
		return map[string]bool{"add": true, "ls": true, "show": true, "set": true}
	case "link":
		return map[string]bool{"link": true, "list": true}
	default:
		return nil
	}
}

func validateAction(action SkillAction) error {
	if action.Verb == "" || action.Operation == "" || strings.TrimSpace(action.Summary) == "" || !action.Safety.Valid() || !strings.HasPrefix(action.Command, "aira ") {
		return fmt.Errorf("action %q/%q has incomplete metadata", action.Verb, action.Operation)
	}
	seen := map[string]bool{}
	for _, arg := range action.Args {
		if arg.Name == "" || seen[arg.Name] {
			return fmt.Errorf("action %q/%q has invalid args", action.Verb, action.Operation)
		}
		seen[arg.Name] = true
	}
	return nil
}

func copyArgSpecs(args []ArgSpec) []ArgSpec {
	result := make([]ArgSpec, len(args))
	for i, arg := range args {
		result[i] = arg
		result[i].Enum = append([]string(nil), arg.Enum...)
	}
	return result
}

func commandFor(verb string, example []string) string {
	parts := append([]string{"aira", verb}, example...)
	return strings.Join(parts, " ")
}

func renderSkillMarkdown(actions []SkillAction, contract ResponseContractSpec) string {
	var out strings.Builder
	out.WriteString("---\nname: aira\ndescription: Machine-local coordination for AI agents.\n---\n\n")
	out.WriteString(renderMarkdownBody("# AIRA Skill", actions, contract))
	return out.String()
}

func renderGuideMarkdown(actions []SkillAction, contract ResponseContractSpec) string {
	return renderMarkdownBody("# AIRA Agent Guide", actions, contract)
}

func renderMarkdownBody(title string, actions []SkillAction, contract ResponseContractSpec) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\n", title)
	out.WriteString("AIRA is a machine-local coordination tool for AI agents, with one CLI entrypoint over its core operations.\n\n")
	out.WriteString("## Honesty contract\n\n")
	fmt.Fprintf(&out, "Responses carry stable AIRA codes. Verdicts are `%s`, `%s`, and `%s`. `unevaluated` is not a pass and not zero.\n\n", contract.Verdicts[0], contract.Verdicts[1], contract.Verdicts[2])
	out.WriteString("Stable codes: `")
	out.WriteString(strings.Join(contract.StableCodes, "`, `"))
	out.WriteString("`.\n\nExit codes:\n\n")
	for _, code := range sortedContractCodes(contract.ExitCodes) {
		fmt.Fprintf(&out, "- `%s`: `%d`\n", code, contract.ExitCodes[code])
	}
	out.WriteString("\n## Actions\n\n")
	for _, action := range actions {
		fmt.Fprintf(&out, "### `%s %s`\n\n%s\n\nSafety: `%s`\n\nCommand: `%s`\n\n", action.Verb, action.Operation, action.Summary, action.Safety, action.Command)
		if len(action.Args) == 0 {
			out.WriteString("Arguments: none.\n\n")
		} else {
			out.WriteString("Arguments:\n\n")
			for _, arg := range action.Args {
				required := "optional"
				if arg.Required {
					required = "required"
				}
				fmt.Fprintf(&out, "- `%s` (`%s`, %s): %s\n", arg.Name, arg.Kind, required, arg.Description)
			}
			out.WriteString("\n")
		}
		if hasArg(action.Args, "file") && hasArg(action.Args, "line") {
			out.WriteString("For this action, the composite CLI encoding is `--file path:line`; there is no `--line` flag.\n\n")
		}
	}
	return out.String()
}

func hasArg(args []ArgSpec, name string) bool {
	for _, arg := range args {
		if arg.Name == name {
			return true
		}
	}
	return false
}

func sortedContractCodes(codes map[string]int) []string {
	result := make([]string, 0, len(codes))
	for code := range codes {
		result = append(result, code)
	}
	sort.Strings(result)
	return result
}
