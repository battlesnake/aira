package gate

import (
	"errors"
	"fmt"
	"sort"
)

type CanaryMode string

const (
	CanaryFixture              CanaryMode = "fixture"
	CanaryAttestationChallenge CanaryMode = "attestation-challenge"
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
	SchemaVersion      int        `json:"schema_version"`
	ID                 string     `json:"id"`
	GateID             string     `json:"gate_id"`
	Mode               CanaryMode `json:"mode"`
	Seed               Seed       `json:"seed"`
	ExpectedGateResult string     `json:"expected_gate_result"`
	LaneBinding        string     `json:"lane_binding"`
	Isolation          Isolation  `json:"isolation"`
	Cadence            Cadence    `json:"cadence"`
	Description        string     `json:"description,omitempty"`
}

func ValidateCanary(c CanaryDeclaration) error {
	if c.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported schema_version %d", c.SchemaVersion)
	}
	if !slugPattern.MatchString(c.ID) || !slugPattern.MatchString(c.GateID) {
		return errors.New("E_GATE_CANARY_INVALID: invalid id")
	}
	if c.Mode != CanaryFixture && c.Mode != CanaryAttestationChallenge {
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported mode %q", c.Mode)
	}
	if c.ExpectedGateResult != VerdictFail {
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
	for path := range c.Seed.Files {
		if path == "" || path[0] == '/' || path == ".git" || len(path) >= 3 && path[:3] == "../" || path == ".." {
			return fmt.Errorf("E_GATE_CANARY_INVALID: seed path escapes fixture root: %q", path)
		}
	}
	return nil
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
