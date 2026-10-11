package gate

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
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
//
// Content belongs to the inject-file kind alone. It is literal file bytes the
// evaluator writes verbatim into an isolated snapshot — never a patch, script,
// or command the evaluator interprets — which keeps the language-agnostic kind
// inside the same invariant as the two Go kinds.
type MutationSeed struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	Seed           uint64 `json:"seed"`
	File           string `json:"file,omitempty"`
	Test           string `json:"test,omitempty"`
	Occurrence     int    `json:"occurrence,omitempty"`
	PkgDir         string `json:"pkgdir,omitempty"`
	TestName       string `json:"testname,omitempty"`
	Content        string `json:"content,omitempty"`
	ExpectedResult string `json:"expected_result"`
}

// maxMutationContentBytes bounds an inject-file body. A canary declaration is a
// committed, digested file, not a payload channel; a small cap keeps the closed
// union cheap to authenticate and cheap to read in review.
const maxMutationContentBytes = 64 * 1024

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
	// AIRA-60: the same normalizing predicate evaluation applies, applied here so
	// the refusal lands at declaration time where a gate author sees it. The
	// superseded check was a literal prefix test that accepted -- and digested --
	// .git/config, .git/hooks/pre-commit, sub/.git/config, a/../../etc/passwd and
	// ./../x, every one of which runCanary then refused. Seed.Path's shape was
	// not checked at all, so any consumer that reasonably assumed a validated
	// declaration was safe inherited an unvalidated path; the blast radius is
	// command execution, because runCanary's unconditional `git add -A` would
	// execute a seeded .git/config carrying core.fsmonitor.
	if c.Seed.Path != "" && !SafeRelativePath(c.Seed.Path) {
		return fmt.Errorf("E_GATE_CANARY_INVALID: seed path escapes fixture root: %q", c.Seed.Path)
	}
	for path := range c.Seed.Files {
		if !SafeRelativePath(path) {
			return fmt.Errorf("E_GATE_CANARY_INVALID: seed path escapes fixture root: %q", path)
		}
	}
	return refuseBookkeepingTargets(c)
}

// refuseBookkeepingTargets refuses a canary that writes into, or seeds from,
// AIRA's own ticket and finding files (AIRA-285). Those paths are left out of
// every gate subject's capture, so a mutation landing there is discarded by the
// re-capture and a seed file there is dropped from the fixture tree: the canary
// could then only "fire" for some other reason, and RunGate reads any failure
// on the canary tree as a fire and mints proof-of-fire from it. A Seed.Path
// there is also a live-worktree read (runCanary copies it from the caller's
// root) that no subject digest covers any more. Directory-valued fields are
// tested with a trailing slash, so the directory itself is caught as well as
// anything below it.
func refuseBookkeepingTargets(c CanaryDeclaration) error {
	refuse := func(field string) error {
		return fmt.Errorf("E_GATE_CANARY_INVALID: %s targets AIRA bookkeeping, which is not part of any gate subject (AIRA-285)", field)
	}
	if m := c.Mutation; m != nil {
		if m.File != "" && ExcludedFromSubject(cleanSlashPath(m.File)) {
			return refuse("mutation file")
		}
		if m.PkgDir != "" && ExcludedFromSubject(cleanSlashPath(m.PkgDir)+"/") {
			return refuse("mutation pkgdir")
		}
	}
	for _, path := range c.SortedSeedFiles() {
		if ExcludedFromSubject(cleanSlashPath(path)) {
			return refuse("seed file")
		}
	}
	if c.Seed.Path != "" && ExcludedFromSubject(cleanSlashPath(c.Seed.Path)+"/") {
		return refuse("seed path")
	}
	return nil
}

func cleanSlashPath(path string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
}

// ExcludedFromSubject reports whether a root-relative slash path is AIRA's own
// workflow bookkeeping (AIRA-285), which is left out of every gate subject's
// CAPTURE: absent from the digest and from every tree a checker is given,
// alike.
//
// Exactly two directories, matched as literal prefixes anchored at the root
// and ending in a slash: .aira/tickets/ and .aira/findings/. They are what the
// routine workflow verbs (create, set, mv, link, claim, find) write, and no
// AIRA checker reads them. Everything else under .aira/ stays in, deliberately:
// requirements are read by the traceability checker, so a requirement edit must
// still invalidate, and config, gate definitions, canaries and any .aira/ file
// added later default to "in", the safe direction. So .aira/tickets-archive/x,
// a tracked file named .aira/tickets, and docs/.aira/tickets/x all stay in.
func ExcludedFromSubject(path string) bool {
	return strings.HasPrefix(path, ".aira/tickets/") || strings.HasPrefix(path, ".aira/findings/")
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
		if !SafeRelativePath(m.File) || m.Test == "" || m.Occurrence <= 0 || m.PkgDir != "" || m.TestName != "" || m.Content != "" {
			return errors.New("E_GATE_CANARY_INVALID: invalid go-negate-assertion seed")
		}
	case "go-inject-failing-test":
		if !safeMutationPackagePath(m.PkgDir) || !strings.HasPrefix(m.TestName, "Test") || len(m.TestName) <= len("Test") || m.File != "" || m.Test != "" || m.Occurrence != 0 || m.Content != "" {
			return errors.New("E_GATE_CANARY_INVALID: invalid go-inject-failing-test seed")
		}
	// inject-file is the language-agnostic kind: it names a target path and the
	// literal body to write there, and carries none of the Go kinds' fields.
	case "inject-file":
		if !SafeRelativePath(m.File) || m.Content == "" || len(m.Content) > maxMutationContentBytes || !utf8.ValidString(m.Content) || m.Test != "" || m.Occurrence != 0 || m.PkgDir != "" || m.TestName != "" {
			return errors.New("E_GATE_CANARY_INVALID: invalid inject-file seed")
		}
	default:
		return fmt.Errorf("E_GATE_CANARY_INVALID: unsupported mutation kind %q", m.Kind)
	}
	return nil
}

func safeMutationPackagePath(path string) bool {
	return path == "." || SafeRelativePath(path)
}

// SafeRelativePath is the gate subsystem's one normalizing relative-path
// predicate, applied at declaration time and at evaluation time (AIRA-60).
//
// It replaces three near-identical copies -- store.safeFixturePath,
// store.safeSnapshotPath and gate.safeMutationPath -- that had drifted apart in
// exactly the way three copies of a safety check do, and a fourth, weaker,
// non-normalizing test inside ValidateCanary that let a declaration be accepted
// and digested with a path evaluation would then refuse.
//
// It is the CONJUNCTION of every rejection the three made, never a disjunction
// of their acceptances, which would loosen it: a path is safe only if all three
// would have called it safe. The one rejection none of the store-side copies
// made is the NUL check, which is unreachable from `git ls-files -z` (NUL is the
// separator) but perfectly reachable from a hand-authored canary declaration.
//
// The .git segment matters most: a seed or mutation writing into a snapshot's
// own .git -- a config carrying core.fsmonitor, say -- would be executed by the
// `git add` that stages that snapshot.
func SafeRelativePath(path string) bool {
	if path == "" || path[0] == '/' || filepath.IsAbs(filepath.FromSlash(path)) || strings.ContainsRune(path, '\x00') {
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
