// Package gate contains the side-effect-free gate and canary content model.
package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const CurrentSchemaVersion = 2

type Kind string

const (
	KindCheckable Kind = "checkable"
	KindManual    Kind = "manual"
	KindRatchet   Kind = "ratchet"
)

type Checker string

const CheckerDimension Checker = "check-dimension"
const CheckerManual Checker = "manual-attestation"
const CheckerRatchet Checker = "ratchet"

type ProofMode string

const ProofRequired ProofMode = "required"

type GateDefinition struct {
	SchemaVersion   int         `json:"schema_version"`
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Kind            Kind        `json:"kind"`
	AppliesTo       AppliesTo   `json:"applies_to"`
	Lane            Lane        `json:"lane"`
	ProofPolicy     ProofPolicy `json:"proof_policy"`
	CanaryIDs       []string    `json:"canary_ids"`
	Checkable       *Checkable  `json:"checkable,omitempty"`
	Manual          *Manual     `json:"manual,omitempty"`
	Command         *Command    `json:"command,omitempty"`
	Ratchet         *Ratchet    `json:"ratchet,omitempty"`
	Enabled         bool        `json:"enabled"`
	Advisory        bool        `json:"advisory"`
	AdvisoryInReady bool        `json:"advisory_in_ready"`
	Description     string      `json:"description,omitempty"`
	FailureGuidance string      `json:"failure_guidance,omitempty"`
}

// AppliesTo is deliberately a closed selector. All means every subject; the
// other fields narrow it without accepting arbitrary expression languages.
type AppliesTo struct {
	All           bool     `json:"all,omitempty"`
	LifecycleStep string   `json:"lifecycle_step,omitempty"`
	Ticket        string   `json:"ticket,omitempty"`
	Milestone     string   `json:"milestone,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	Paths         []string `json:"paths,omitempty"`
}

type Lane struct {
	Name             string `json:"name"`
	Checker          string `json:"checker"`
	EvaluatorVersion string `json:"evaluator_version,omitempty"`
	ConfigDigest     string `json:"config_digest,omitempty"`
}

type ProofPolicy struct {
	Mode                 ProofMode `json:"mode"`
	MaxAgeSecs           int64     `json:"max_age_secs"`
	RequireCurrentCanary bool      `json:"require_current_canary"`
}

type Checkable struct {
	Dimension string `json:"dimension"`
}
type Manual struct {
	Role          string   `json:"role,omitempty"`
	EvidenceKinds []string `json:"evidence_kinds,omitempty"`
	PromptID      string   `json:"prompt_id,omitempty"`
}

// Ratchet is committed policy. The mutable baseline is deliberately kept in
// the authenticated audit class instead of in the gate definition.
type Ratchet struct {
	Metric            string        `json:"metric"`
	Comparator        string        `json:"comparator"`
	ComparisonKey     ComparisonKey `json:"comparison_key"`
	BaselineSelection string        `json:"baseline_selection"`
}

type ComparisonKey struct {
	SuiteID   string `json:"suite_id"`
	Config    string `json:"config"`
	EnvDigest string `json:"env_digest"`
	Shard     string `json:"shard"`
}

var ratchetShardPattern = regexp.MustCompile(`^[1-9][0-9]*/[1-9][0-9]*$`)

func (r Ratchet) Validate() error {
	if strings.TrimSpace(r.Metric) == "" {
		return errors.New("E_GATE_INVALID: ratchet metric is required")
	}
	if strings.TrimSpace(r.Comparator) == "" {
		return errors.New("E_GATE_INVALID: ratchet comparator is required")
	}
	if r.Comparator != "no-new-failures" && r.Comparator != "coverage-drop" {
		return fmt.Errorf("E_GATE_INVALID: unsupported ratchet comparator %q", r.Comparator)
	}
	selection := strings.ToLower(strings.TrimSpace(r.BaselineSelection))
	if !strings.Contains(selection, "active") || !strings.Contains(selection, "pin") {
		return errors.New("E_GATE_INVALID: ratchet baseline_selection must be active-explicitly-pinned")
	}
	key := r.ComparisonKey
	if strings.TrimSpace(key.SuiteID) == "" || strings.TrimSpace(key.Config) == "" || strings.TrimSpace(key.EnvDigest) == "" || strings.TrimSpace(key.Shard) == "" {
		return errors.New("E_GATE_INVALID: ratchet comparison_key requires suite_id, config, env_digest, and shard")
	}
	if !ratchetShardPattern.MatchString(key.Shard) {
		return fmt.Errorf("E_GATE_INVALID: ratchet comparison_key shard %q must be i/n", key.Shard)
	}
	if r.Comparator == "coverage-drop" && r.Metric != "coverage" {
		return errors.New("E_GATE_INVALID: coverage-drop ratchet requires coverage metric")
	}
	if r.Comparator == "no-new-failures" && r.Metric != "tests" && r.Metric != "test-failures" {
		return errors.New("E_GATE_INVALID: no-new-failures ratchet requires tests metric")
	}
	return nil
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func (g GateDefinition) Validate(filename string) error { return ValidateGate(g, filename) }

func ValidateGate(g GateDefinition, filename string) error {
	if g.SchemaVersion != 1 && g.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("E_GATE_INVALID: unsupported schema_version %d", g.SchemaVersion)
	}
	if !slugPattern.MatchString(g.ID) {
		return errors.New("E_GATE_INVALID: id must match ^[a-z0-9][a-z0-9-]{0,63}$")
	}
	if filename != "" && filepath.Base(filename) != g.ID+".json" {
		return fmt.Errorf("E_GATE_INVALID: filename %q does not match id %q", filename, g.ID)
	}
	if strings.TrimSpace(g.Name) == "" {
		return errors.New("E_GATE_INVALID: name is required")
	}
	if g.Kind != KindCheckable && g.Kind != KindManual && g.Kind != KindRatchet {
		return fmt.Errorf("E_GATE_KIND_INVALID: unsupported gate kind %q", g.Kind)
	}
	if !validSelector(g.AppliesTo) {
		return errors.New("E_GATE_INVALID: applies_to selector is empty or invalid")
	}
	if g.Lane.Name == "" || g.Lane.Checker == "" {
		return errors.New("E_GATE_INVALID: lane name and checker are required")
	}
	if g.Lane.Checker != string(CheckerDimension) && g.Lane.Checker != string(CheckerManual) && g.Lane.Checker != string(CheckerCommand) && g.Lane.Checker != string(CheckerRatchet) {
		return fmt.Errorf("E_GATE_INVALID: unknown checker %q", g.Lane.Checker)
	}
	if g.ProofPolicy.Mode != ProofRequired || g.ProofPolicy.MaxAgeSecs < 0 {
		return errors.New("E_GATE_INVALID: invalid proof policy")
	}
	if len(g.CanaryIDs) == 0 {
		return errors.New("E_GATE_CANARY_INVALID: canary_ids must not be empty")
	}
	if hasDuplicate(g.CanaryIDs) {
		return errors.New("E_GATE_CANARY_INVALID: duplicate canary id")
	}
	payloads := 0
	if g.Checkable != nil {
		payloads++
	}
	if g.Manual != nil {
		payloads++
	}
	if g.Command != nil {
		payloads++
	}
	if g.Ratchet != nil {
		payloads++
	}
	if payloads != 1 {
		return errors.New("E_GATE_INVALID: exactly one kind payload is required")
	}
	if g.Kind == KindCheckable && g.Checkable == nil && g.Command == nil {
		return errors.New("E_GATE_INVALID: checkable payload is required")
	}
	if g.Kind == KindManual && g.Manual == nil {
		return errors.New("E_GATE_INVALID: manual payload is required")
	}
	if g.Kind == KindRatchet {
		if g.Ratchet == nil || g.Lane.Checker != string(CheckerRatchet) {
			return errors.New("E_GATE_INVALID: ratchet gates require ratchet checker and payload")
		}
		if err := g.Ratchet.Validate(); err != nil {
			return err
		}
	} else if g.Ratchet != nil || g.Lane.Checker == string(CheckerRatchet) {
		return errors.New("E_GATE_INVALID: ratchet payload requires a ratchet gate")
	}
	if g.Checkable != nil {
		if g.Lane.Checker != string(CheckerDimension) {
			return errors.New("E_GATE_INVALID: checkable gates require check-dimension checker")
		}
		if strings.TrimSpace(g.Checkable.Dimension) == "" {
			return errors.New("E_GATE_INVALID: checkable dimension is required")
		}
		if strings.EqualFold(g.Checkable.Dimension, "gates") || strings.EqualFold(g.Checkable.Dimension, "check") {
			return errors.New("E_GATE_INVALID: check-dimension cannot target gates or aggregate check")
		}
	}
	if g.Manual != nil && g.Lane.Checker != string(CheckerManual) {
		return errors.New("E_GATE_INVALID: manual gates require manual-attestation checker")
	}
	if g.Command != nil {
		if g.SchemaVersion < 2 {
			return errors.New("E_GATE_INVALID: command gates require schema_version 2")
		}
		if g.Kind != KindCheckable || g.Lane.Checker != string(CheckerCommand) {
			return errors.New("E_GATE_INVALID: command payload requires a command checkable gate")
		}
		if err := g.Command.Validate(); err != nil {
			return err
		}
	} else if g.Lane.Checker == string(CheckerCommand) {
		return errors.New("E_GATE_INVALID: command checker requires command payload")
	}
	return nil
}

func validSelector(s AppliesTo) bool {
	if s.All && (strings.TrimSpace(s.LifecycleStep) != "" || strings.TrimSpace(s.Ticket) != "" || strings.TrimSpace(s.Milestone) != "" || len(s.Labels) > 0 || len(s.Paths) > 0) {
		return false
	}
	if s.All {
		return true
	}
	if strings.TrimSpace(s.LifecycleStep) != "" || strings.TrimSpace(s.Ticket) != "" || strings.TrimSpace(s.Milestone) != "" || len(s.Labels) > 0 || len(s.Paths) > 0 {
		if !validStrings(s.Labels) || !validStrings(s.Paths) {
			return false
		}
		for _, path := range s.Paths {
			if filepath.IsAbs(filepath.FromSlash(path)) || path == ".." || strings.HasPrefix(filepath.Clean(filepath.FromSlash(path)), ".."+string(filepath.Separator)) {
				return false
			}
		}
		return true
	}
	return false
}
func validStrings(v []string) bool {
	for _, s := range v {
		if strings.TrimSpace(s) == "" || strings.Contains(s, "\x00") {
			return false
		}
	}
	return true
}
func hasDuplicate(values []string) bool {
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] {
			return true
		}
		seen[v] = true
	}
	return false
}

// RenderGate writes the stable JSON-in-frontmatter representation.
func RenderGate(g GateDefinition) ([]byte, error) {
	if g.Command != nil {
		command := g.Command.Normalized()
		g.Command = &command
	}
	if err := ValidateGate(g, g.ID+".json"); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("E_GATE_INVALID: %w", err)
	}
	return append(append([]byte("---\n"), data...), []byte("\n---\n")...), nil
}

func ParseGate(data []byte, filename string) (GateDefinition, error) {
	body := bytes.TrimSpace(data)
	if !bytes.HasPrefix(body, []byte("---")) {
		return GateDefinition{}, errors.New("E_GATE_INVALID: missing frontmatter")
	}
	lines := bytes.Split(body, []byte("\n"))
	if len(lines) < 3 || string(bytes.TrimSpace(lines[0])) != "---" || string(bytes.TrimSpace(lines[len(lines)-1])) != "---" {
		return GateDefinition{}, errors.New("E_GATE_INVALID: malformed frontmatter")
	}
	jsonData := bytes.Join(lines[1:len(lines)-1], []byte("\n"))
	var g GateDefinition
	dec := json.NewDecoder(bytes.NewReader(jsonData))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return GateDefinition{}, fmt.Errorf("E_GATE_INVALID: %w", err)
	}
	if g.Command != nil {
		command := g.Command.Normalized()
		g.Command = &command
	}
	if err := ValidateGate(g, filename); err != nil {
		return GateDefinition{}, err
	}
	return g, nil
}

// CanonicalSelectorFields is useful to callers constructing stable digests.
func CanonicalSelectorFields(s AppliesTo) []string {
	fields := []string{}
	if s.All {
		fields = append(fields, "all=true")
	}
	fields = append(fields, "lifecycle_step="+s.LifecycleStep, "ticket="+s.Ticket, "milestone="+s.Milestone)
	labels := append([]string{}, s.Labels...)
	paths := append([]string{}, s.Paths...)
	sort.Strings(labels)
	sort.Strings(paths)
	fields = append(fields, "labels="+strings.Join(labels, "\x00"), "paths="+strings.Join(paths, "\x00"))
	return fields
}
