package gate

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type CanaryMode string

const (
	CanaryFixture              CanaryMode = "fixture"
	CanaryAttestationChallenge CanaryMode = "attestation-challenge"
	CanaryMutation             CanaryMode = "mutation"
	CanarySyntheticRatchet     CanaryMode = "synthetic-ratchet"
)

type Cadence string

const (
	CadenceOnDemand        Cadence = "on-demand"
	CadenceEveryEvaluation Cadence = "every-evaluation"
)

type Isolation string

const IsolationTempGit Isolation = "isolated-temp-git"

type Seed struct {
	Path   string            `json:"path,omitempty"`
	Files  map[string]string `json:"files,omitempty"`
	Digest string            `json:"digest,omitempty"`
}
type CanaryDeclaration struct {
	SchemaVersion      int           `json:"schema_version"`
	ID                 string        `json:"id"`
	GateID             string        `json:"gate_id"`
	Mode               CanaryMode    `json:"mode"`
	Seed               Seed          `json:"seed"`
	Mutation           *MutationSeed `json:"mutation,omitempty"`
	ExpectedGateResult string        `json:"expected_gate_result"`
	Expected           string        `json:"expected,omitempty"`
	BaselineFailing    []string      `json:"baseline_failing,omitempty"`
	CurrentFailing     []string      `json:"current_failing,omitempty"`
	LaneBinding        string        `json:"lane_binding"`
	Isolation          Isolation     `json:"isolation"`
	Cadence            Cadence       `json:"cadence"`
	Description        string        `json:"description,omitempty"`
}

// MutationSeed is a closed tagged union. The evaluator interprets only these
// typed fields; it never executes seed content as a patch or command.
type MutationSeed struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	Seed           uint64 `json:"seed"`
	File           string `json:"file,omitempty"`
	Test           string `json:"test,omitempty"`
	Occurrence     int    `json:"occurrence,omitempty"`
	PkgDir         string `json:"pkgdir,omitempty"`
	TestName       string `json:"testname,omitempty"`
	ExpectedResult string `json:"expected_result"`
}

func ValidateCanary(c CanaryDeclaration) error {
	if c.SchemaVersion != 1 && c.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported schema_version %d", c.SchemaVersion)
	}
	if !slugPattern.MatchString(c.ID) || !slugPattern.MatchString(c.GateID) {
		return errors.New("E_GATE_CANARY_INVALID: invalid id")
	}
	if c.Mode != CanaryFixture && c.Mode != CanaryAttestationChallenge && c.Mode != CanaryMutation && c.Mode != CanarySyntheticRatchet {
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported mode %q", c.Mode)
	}
	if c.Mode == CanarySyntheticRatchet {
		if c.Expected != "regressed" && c.ExpectedGateResult != "regressed" {
			return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet expected must be regressed")
		}
		if len(c.CurrentFailing) == 0 {
			return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet current_failing is empty")
		}
		baseline := make(map[string]struct{}, len(c.BaselineFailing))
		for _, name := range c.BaselineFailing {
			if strings.TrimSpace(name) == "" {
				return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet baseline name is empty")
			}
			if _, exists := baseline[name]; exists {
				return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet duplicate baseline name")
			}
			baseline[name] = struct{}{}
		}
		seenCurrent := map[string]struct{}{}
		introduced := false
		for _, name := range c.CurrentFailing {
			if strings.TrimSpace(name) == "" {
				return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet current name is empty")
			}
			if _, exists := seenCurrent[name]; exists {
				return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet duplicate current name")
			}
			seenCurrent[name] = struct{}{}
			if _, exists := baseline[name]; !exists {
				introduced = true
			}
		}
		if !introduced {
			return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet current_failing introduces no new name")
		}
		if c.ExpectedGateResult != "" && c.ExpectedGateResult != VerdictFail && c.ExpectedGateResult != "regressed" {
			return errors.New("E_GATE_CANARY_INVALID: synthetic-ratchet expected_gate_result must be fail when present")
		}
	} else if c.ExpectedGateResult != VerdictFail {
		return errors.New("E_GATE_CANARY_INVALID: proof-eligible canary must expect fail")
	}
	if c.LaneBinding == "" {
		return errors.New("E_GATE_CANARY_INVALID: lane_binding is required")
	}
	if c.Isolation != IsolationTempGit {
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported isolation %q", c.Isolation)
	}
	if c.Cadence != CadenceOnDemand && c.Cadence != CadenceEveryEvaluation {
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported cadence %q", c.Cadence)
	}
	if c.Mode == CanaryFixture && len(c.Seed.Files) == 0 && c.Seed.Path == "" {
		return errors.New("E_GATE_CANARY_INVALID: fixture seed is empty")
	}
	if c.Mode == CanaryMutation {
		if c.Mutation == nil {
			return errors.New("E_GATE_CANARY_INVALID: mutation seed is required")
		}
		if err := validateMutation(*c.Mutation); err != nil {
			return err
		}
	}
	for path := range c.Seed.Files {
		if path == "" || path[0] == '/' || path == ".git" || len(path) >= 3 && path[:3] == "../" || path == ".." {
			return fmt.Errorf("E_GATE_CANARY_INVALID: seed path escapes fixture root: %q", path)
		}
	}
	return nil
}

func validateMutation(m MutationSeed) error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported mutation schema_version %d", m.SchemaVersion)
	}
	if m.ExpectedResult != VerdictFail {
		return errors.New("E_GATE_CANARY_INVALID: mutation expected_result must be fail")
	}
	switch m.Kind {
	case "go-negate-assertion":
		if !safeMutationPath(m.File) || m.Test == "" || m.Occurrence <= 0 || m.PkgDir != "" || m.TestName != "" {
			return errors.New("E_GATE_CANARY_INVALID: invalid go-negate-assertion seed")
		}
	case "go-inject-failing-test":
		if !safeMutationPackagePath(m.PkgDir) || !strings.HasPrefix(m.TestName, "Test") || len(m.TestName) <= len("Test") || m.File != "" || m.Test != "" || m.Occurrence != 0 {
			return errors.New("E_GATE_CANARY_INVALID: invalid go-inject-failing-test seed")
		}
	default:
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported mutation kind %q", m.Kind)
	}
	return nil
}

func safeMutationPackagePath(path string) bool {
	return path == "." || safeMutationPath(path)
}

func safeMutationPath(path string) bool {
	if path == "" || path[0] == '/' || strings.ContainsRune(path, '\x00') {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" || part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}

func (c CanaryDeclaration) Validate() error                    { return ValidateCanary(c) }
func (c CanaryDeclaration) DeclarationDigest() (string, error) { return DigestCanary(c) }

// SortedSeedFiles returns a detached, deterministic view of the seed.
func (c CanaryDeclaration) SortedSeedFiles() []string {
	keys := make([]string, 0, len(c.Seed.Files))
	for k := range c.Seed.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
