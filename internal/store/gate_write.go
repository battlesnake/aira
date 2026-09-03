package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aira/internal/gate"
)

// renderCanary emits the plain-JSON declaration form that canaryFor decodes
// with DisallowUnknownFields. It validates before rendering.
func renderCanary(declaration gate.CanaryDeclaration) ([]byte, error) {
	if err := gate.ValidateCanary(declaration); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(declaration, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("E_GATE_CANARY_INVALID: %w", err)
	}
	return append(data, '\n'), nil
}

// GateWriteResult reports what `gate add` / `gate set` actually materialized.
// CanaryStatus and IndexStatus are deliberately explicit: a gate written
// without a canary declaration cannot be proven, and a definition whose index
// refresh failed is not silently reported as refreshed.
type GateWriteResult struct {
	GateID       string
	Operation    string
	Path         string
	Definition   gate.GateDefinition
	CanaryID     string
	CanaryPath   string
	CanaryStatus string
	IndexStatus  string
	Warnings     []string
}

const (
	gateOperationCreated = "created"
	gateOperationUpdated = "updated"

	gateCanaryMaterialized = "materialized"
	gateCanaryAbsent       = "absent"

	gateIndexRefreshed = "refreshed"
	gateIndexStale     = "stale"

	// gateDefaultMaxAgeSecs matches the established in-repo proof policy rather
	// than 0, which would let a proof be reused indefinitely.
	gateDefaultMaxAgeSecs = 604800
	// gateDefaultEvaluatorVersion gives the proof binding an evaluator identity.
	gateDefaultEvaluatorVersion = "1"
	// gateDefaultCwd is the materialized evaluation root.
	gateDefaultCwd = "root"
)

// gateCommandFieldNames are the input fields that only a command payload can
// accept. Naming them lets `set` refuse them against a non-command gate
// instead of quietly dropping them.
var gateCommandFieldNames = []string{"predicate", "argv", "cwd", "env_allow", "timeout_ms", "output_cap_bytes", "parser"}

// writeGateDefinition materializes a gate definition, and its canary
// declaration when the input describes one, from parsed input fields. The gate
// file remains the authenticated source of truth; nothing here mints a verdict.
func (s *Store) writeGateDefinition(ctx context.Context, operation, gateID, canaryID string, fields map[string]any) (GateWriteResult, error) {
	if strings.TrimSpace(gateID) == "" {
		return GateWriteResult{}, errors.New("E_GATE_INVALID: gate " + operation + " requires a gate id")
	}
	if !gate.ValidSlug(gateID) {
		return GateWriteResult{}, errors.New("E_GATE_INVALID: id must match ^[a-z0-9][a-z0-9-]{0,63}$")
	}
	gatePath := filepath.Join(s.gateRoot(), gateID+".json")

	var definition gate.GateDefinition
	var loadedDigest string
	var err error
	switch operation {
	case "add":
		definition, err = buildGateDefinition(gateID, gateID+"-canary", fields)
	case "set":
		definition, loadedDigest, err = s.loadGateDefinition(gatePath)
		if err == nil {
			definition, err = applyGateFields(definition, fields)
		}
	default:
		return GateWriteResult{}, fmt.Errorf("E_GATE_INVALID: unknown gate write operation %q", operation)
	}
	if err != nil {
		return GateWriteResult{}, err
	}

	// An explicit --canary-id overrides the derived one for both operations.
	if trimmed := strings.TrimSpace(canaryID); trimmed != "" {
		definition.CanaryIDs = []string{trimmed}
	}
	if len(definition.CanaryIDs) != 1 {
		return GateWriteResult{}, errors.New("E_GATE_INVALID: gate write supports exactly one canary id")
	}
	resolvedCanaryID := definition.CanaryIDs[0]
	if !gate.ValidSlug(resolvedCanaryID) {
		return GateWriteResult{}, fmt.Errorf("E_GATE_INVALID: derived canary id %q must match ^[a-z0-9][a-z0-9-]{0,63}$", resolvedCanaryID)
	}

	// RenderGate validates before it renders, so an invalid definition never
	// reaches the filesystem.
	data, err := gate.RenderGate(definition)
	if err != nil {
		return GateWriteResult{}, err
	}

	declaration, hasCanary, err := buildCanaryDeclaration(definition, resolvedCanaryID, fields)
	if err != nil {
		return GateWriteResult{}, err
	}
	var canaryData []byte
	canaryPath := filepath.Join(s.gateRoot(), "canaries", resolvedCanaryID+".json")
	if hasCanary {
		if canaryData, err = renderCanary(declaration); err != nil {
			return GateWriteResult{}, err
		}
	}

	result := GateWriteResult{GateID: gateID, Path: gatePath, Definition: definition, CanaryID: resolvedCanaryID, CanaryStatus: gateCanaryAbsent}
	if operation == "add" {
		result.Operation = gateOperationCreated
	} else {
		result.Operation = gateOperationUpdated
	}

	gateLock, err := acquireLock(s.pathLockFor(s.worktreeID, gatePath))
	if err != nil {
		return GateWriteResult{}, err
	}
	defer gateLock.Close()

	existing, err := fileDigest(gatePath)
	if err != nil {
		return GateWriteResult{}, err
	}
	if operation == "add" && existing != "" {
		return GateWriteResult{}, fmt.Errorf("E_GATE_EXISTS: gate %s already exists", gateID)
	}
	// set read the definition before taking the lock, so re-check that the file
	// still holds what was read. Without this a concurrent writer's change is
	// silently clobbered by the merged copy.
	if operation == "set" && existing != loadedDigest {
		return GateWriteResult{}, fmt.Errorf("%w: %s", ErrWriteConflict, gatePath)
	}

	if hasCanary {
		canaryLock, lockErr := acquireLock(s.pathLockFor(s.worktreeID, canaryPath))
		if lockErr != nil {
			return GateWriteResult{}, lockErr
		}
		defer canaryLock.Close()
		existingCanary, digestErr := fileDigest(canaryPath)
		if digestErr != nil {
			return GateWriteResult{}, digestErr
		}
		if operation == "add" && existingCanary != "" {
			return GateWriteResult{}, fmt.Errorf("E_GATE_EXISTS: canary %s already exists", resolvedCanaryID)
		}
		// The canary is written first on purpose. Only the gate file is
		// discoverable, so a failure between the two writes can never leave a
		// discoverable gate whose declared canary is missing.
		if writeErr := writeAtomic(canaryPath, canaryData, time.Now().UnixNano()); writeErr != nil {
			return GateWriteResult{}, writeErr
		}
		result.CanaryPath, result.CanaryStatus = canaryPath, gateCanaryMaterialized
	}

	if err := writeAtomic(gatePath, data, time.Now().UnixNano()); err != nil {
		return GateWriteResult{}, err
	}

	// Resolvability is decided by canaryFor, not by statting one path: it also
	// accepts the <gate>.canary.json form and enforces the gate and lane
	// binding, so this cannot claim a canary that would not actually resolve,
	// nor warn about a hand-authored one that does.
	if _, canaryErr := s.canaryFor(definition); canaryErr == nil {
		result.CanaryStatus = gateCanaryMaterialized
	} else {
		result.CanaryStatus = gateCanaryAbsent
		result.CanaryPath = ""
		result.Warnings = append(result.Warnings,
			"gate "+gateID+" has no resolvable canary declaration ("+canaryErr.Error()+"): it cannot be proven, gate run reports E_GATE_CANARY_INVALID, and gate check reports it unevaluated (U_GATE_NO_RESULT) until a canary is authored",
			"an unproven gate holds readiness: ready reports every ticket not-ready until this gate is proven")
	}

	// The gate file is authoritative and every gate read path reads files, but
	// rant validates a --gate reference against the projection, so the index is
	// refreshed here instead of waiting for reconcile. A failed refresh is
	// reported, never silently claimed as refreshed.
	result.IndexStatus = gateIndexRefreshed
	if err := s.rebuildGateProjection(ctx); err != nil {
		result.IndexStatus = gateIndexStale
		result.Warnings = append(result.Warnings, "gate index refresh failed, run aira reconcile: "+err.Error())
	}
	return result, nil
}

// loadGateDefinition returns the parsed definition and the digest of the exact
// bytes it parsed, so the caller can detect a concurrent rewrite under the lock.
func (s *Store) loadGateDefinition(path string) (gate.GateDefinition, string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return gate.GateDefinition{}, "", errors.New("E_NOT_FOUND: gate not found")
	}
	if err != nil {
		return gate.GateDefinition{}, "", err
	}
	definition, err := gate.ParseGate(data, filepath.Base(path))
	if err != nil {
		return gate.GateDefinition{}, "", err
	}
	return definition, digestBytes(data), nil
}

func buildGateDefinition(gateID, canaryID string, fields map[string]any) (gate.GateDefinition, error) {
	checker, ok := gateFieldString(fields, "checker")
	if !ok {
		return gate.GateDefinition{}, errors.New("E_GATE_INVALID: gate add requires --checker (command, check-dimension, or manual-attestation)")
	}
	definition := gate.GateDefinition{
		SchemaVersion: gate.CurrentSchemaVersion,
		ID:            gateID,
		Name:          gateID,
		AppliesTo:     gate.AppliesTo{All: true},
		Lane:          gate.Lane{Name: gateID, Checker: checker, EvaluatorVersion: gateDefaultEvaluatorVersion},
		ProofPolicy:   gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: gateDefaultMaxAgeSecs, RequireCurrentCanary: true},
		CanaryIDs:     []string{canaryID},
		Enabled:       true,
	}
	switch checker {
	case string(gate.CheckerCommand):
		definition.Kind = gate.KindCheckable
		command, err := buildGateCommand(gate.Command{Cwd: gateDefaultCwd}, fields)
		if err != nil {
			return gate.GateDefinition{}, err
		}
		definition.Command = &command
	case string(gate.CheckerDimension):
		definition.Kind = gate.KindCheckable
		dimension, ok := gateFieldString(fields, "dimension")
		if !ok {
			return gate.GateDefinition{}, errors.New("E_GATE_INVALID: check-dimension gates require --dimension")
		}
		if err := validateGateDimension(dimension); err != nil {
			return gate.GateDefinition{}, err
		}
		definition.Checkable = &gate.Checkable{Dimension: dimension}
	case string(gate.CheckerManual):
		definition.Kind = gate.KindManual
		definition.Manual = &gate.Manual{}
	case string(gate.CheckerRatchet):
		return gate.GateDefinition{}, errors.New("E_GATE_INVALID: ratchet gates have no flag surface for metric, comparator, and comparison_key; author .aira/gates/<id>.json directly")
	default:
		return gate.GateDefinition{}, fmt.Errorf("E_GATE_INVALID: unknown checker %q", checker)
	}
	if err := rejectForeignPayloadFields(definition, fields); err != nil {
		return gate.GateDefinition{}, err
	}
	return definition, nil
}

// applyGateFields is the `set` path. It changes only the fields present in the
// input and deliberately refuses a checker change, which would require a kind
// and payload migration that belongs in the file, not in a flag.
func applyGateFields(definition gate.GateDefinition, fields map[string]any) (gate.GateDefinition, error) {
	if _, ok := gateFieldString(fields, "checker"); ok {
		return gate.GateDefinition{}, errors.New("E_GATE_INVALID: gate set cannot change --checker; edit .aira/gates/<id>.json or re-add the gate")
	}
	if dimension, ok := gateFieldString(fields, "dimension"); ok {
		if definition.Checkable == nil {
			return gate.GateDefinition{}, errors.New("E_GATE_INVALID: --dimension requires a check-dimension gate")
		}
		if err := validateGateDimension(dimension); err != nil {
			return gate.GateDefinition{}, err
		}
		definition.Checkable = &gate.Checkable{Dimension: dimension}
	}
	if err := rejectForeignPayloadFields(definition, fields); err != nil {
		return gate.GateDefinition{}, err
	}
	if definition.Command != nil {
		command, err := buildGateCommand(*definition.Command, fields)
		if err != nil {
			return gate.GateDefinition{}, err
		}
		definition.Command = &command
	}
	return definition, nil
}

// rejectForeignPayloadFields refuses input that the gate's payload cannot hold,
// so a flag is never silently dropped.
func rejectForeignPayloadFields(definition gate.GateDefinition, fields map[string]any) error {
	if definition.Command == nil {
		for _, name := range gateCommandFieldNames {
			if gateFieldPresent(fields, name) {
				return fmt.Errorf("E_GATE_INVALID: --%s requires a command gate", strings.ReplaceAll(name, "_", "-"))
			}
		}
	}
	if definition.Checkable == nil && gateFieldPresent(fields, "dimension") {
		return errors.New("E_GATE_INVALID: --dimension requires a check-dimension gate")
	}
	return nil
}

// gateFieldPresent treats an empty or whitespace-only value as absent, so an
// unset flag never produces a confusing "requires a command gate" refusal.
func gateFieldPresent(fields map[string]any, name string) bool {
	if _, ok := gateFieldString(fields, name); ok {
		return true
	}
	_, ok := gateFieldStrings(fields, name)
	return ok
}

func validateGateDimension(dimension string) error {
	// EvaluateDimension supports traceability only. Refusing anything else at
	// creation avoids minting a gate that can never evaluate.
	if dimension != "traceability" {
		return fmt.Errorf("E_GATE_INVALID: unsupported check dimension %q, only traceability is evaluable", dimension)
	}
	return nil
}

func buildGateCommand(command gate.Command, fields map[string]any) (gate.Command, error) {
	if predicate, ok := gateFieldString(fields, "predicate"); ok {
		command.Predicate = predicate
	}
	if argv, ok := gateFieldStrings(fields, "argv"); ok {
		command.Argv = argv
	}
	if cwd, ok := gateFieldString(fields, "cwd"); ok {
		command.Cwd = cwd
	}
	if envAllow, ok := gateFieldStrings(fields, "env_allow"); ok {
		command.EnvAllow = envAllow
	}
	if parser, ok := gateFieldString(fields, "parser"); ok {
		command.Parser = parser
	}
	timeout, ok, err := gateFieldInt(fields, "timeout_ms")
	if err != nil {
		return gate.Command{}, err
	}
	if ok {
		command.TimeoutMS = timeout
	}
	capBytes, ok, err := gateFieldInt(fields, "output_cap_bytes")
	if err != nil {
		return gate.Command{}, err
	}
	if ok {
		command.OutputCapBytes = capBytes
	}
	return command, nil
}

// buildCanaryDeclaration returns a mutation canary when the input carries a
// mutation seed. Absent mutation input it reports hasCanary false rather than
// inventing a declaration.
func buildCanaryDeclaration(definition gate.GateDefinition, canaryID string, fields map[string]any) (gate.CanaryDeclaration, bool, error) {
	kind, ok := gateFieldString(fields, "mutation_kind")
	if !ok {
		for _, name := range []string{"mutation_file", "mutation_test", "mutation_occurrence", "mutation_pkgdir", "mutation_testname", "mutation_seed", "mutation_expected_result"} {
			if gateFieldPresent(fields, name) {
				return gate.CanaryDeclaration{}, false, errors.New("E_GATE_CANARY_INVALID: mutation fields require --mutation-kind")
			}
		}
		return gate.CanaryDeclaration{}, false, nil
	}
	seed := gate.MutationSeed{SchemaVersion: 1, Kind: kind, ExpectedResult: gate.VerdictFail}
	if expected, present := gateFieldString(fields, "mutation_expected_result"); present {
		seed.ExpectedResult = expected
	}
	seed.File, _ = gateFieldString(fields, "mutation_file")
	seed.Test, _ = gateFieldString(fields, "mutation_test")
	seed.PkgDir, _ = gateFieldString(fields, "mutation_pkgdir")
	seed.TestName, _ = gateFieldString(fields, "mutation_testname")
	occurrence, _, err := gateFieldInt(fields, "mutation_occurrence")
	if err != nil {
		return gate.CanaryDeclaration{}, false, err
	}
	seed.Occurrence = int(occurrence)
	seedValue, _, err := gateFieldInt(fields, "mutation_seed")
	if err != nil {
		return gate.CanaryDeclaration{}, false, err
	}
	if seedValue < 0 {
		return gate.CanaryDeclaration{}, false, errors.New("E_GATE_CANARY_INVALID: --mutation-seed must not be negative")
	}
	seed.Seed = uint64(seedValue)
	declaration := gate.CanaryDeclaration{
		SchemaVersion:      gate.CurrentSchemaVersion,
		ID:                 canaryID,
		GateID:             definition.ID,
		Mode:               gate.CanaryMutation,
		Mutation:           &seed,
		ExpectedGateResult: gate.VerdictFail,
		LaneBinding:        definition.Lane.Name,
		Isolation:          gate.IsolationTempGit,
		Cadence:            gate.CadenceOnDemand,
	}
	if err := gate.ValidateCanary(declaration); err != nil {
		return gate.CanaryDeclaration{}, false, err
	}
	return declaration, true, nil
}

func gateFieldString(fields map[string]any, name string) (string, bool) {
	value, ok := fields[name]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

func gateFieldStrings(fields map[string]any, name string) ([]string, bool) {
	value, ok := fields[name]
	if !ok {
		return nil, false
	}
	switch typed := value.(type) {
	case []string:
		if len(typed) == 0 {
			return nil, false
		}
		return append([]string(nil), typed...), true
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			result = append(result, text)
		}
		if len(result) == 0 {
			return nil, false
		}
		return result, true
	default:
		return nil, false
	}
}

// gateFieldInt parses strictly. A non-numeric value is refused rather than
// coerced to zero, which a validator downstream might silently accept.
func gateFieldInt(fields map[string]any, name string) (int64, bool, error) {
	text, ok := gateFieldString(fields, name)
	if !ok {
		return 0, false, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("E_GATE_INVALID: --%s must be an integer, got %q", strings.ReplaceAll(name, "_", "-"), text)
	}
	return parsed, true, nil
}
