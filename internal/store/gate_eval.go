package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gate"
)

type EvaluationRoot struct {
	Path   string
	Digest string
}
type DimensionEvaluation struct {
	Predicate gate.PredicateState
	Findings  []CheckFinding
	Evidence  bool
	Root      EvaluationRoot
	Code      string
	EnvDigest string
	RunID     string
}

func gateResultFields(def gate.GateDefinition, definitionDigest, declarationDigest, subjectScope, canaryTreeDigest, proofSeq string) map[string]string {
	return map[string]string{
		"definition_digest":  definitionDigest,
		"declaration_digest": declarationDigest,
		"canary_tree_digest": canaryTreeDigest,
		"subject_scope":      subjectScope,
		"lane":               def.Lane.Name,
		"evaluator_version":  def.Lane.EvaluatorVersion,
		"proof_seq":          proofSeq,
	}
}

func appendCanaryUnevaluated(audit *GateAudit, def gate.GateDefinition, definitionDigest, declarationDigest, subject string, runErr error) (GateAuditRecord, error) {
	fields := gateResultFields(def, definitionDigest, declarationDigest, subject, "", "")
	fields["gate_id"], fields["subject"] = def.ID, subject
	code := "U_GATE_CANARY_UNEVALUATED"
	if candidate := ErrorCode(runErr); strings.HasPrefix(candidate, "U_GATE_") {
		code = candidate
	}
	fields["verdict"], fields["code"], fields["trusted"], fields["suspect"] = gate.VerdictUnevaluated, code, "false", "true"
	fields["at"], fields["error"] = time.Now().UTC().Format(time.RFC3339Nano), runErr.Error()
	return audit.Append("result", fields)
}

func digestEvaluationRoot(root string) (string, error) {
	paths, err := trackedTracePaths(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(path + "\x00"))
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func EvaluateDimension(root, dimension string) (DimensionEvaluation, error) {
	if strings.TrimSpace(root) == "" {
		return DimensionEvaluation{}, errors.New("U_GATE_EVIDENCE_UNAVAILABLE: evaluation root is empty")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return DimensionEvaluation{}, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: evaluation root: %w", err)
	}
	digest, err := digestEvaluationRoot(root)
	if err != nil {
		return DimensionEvaluation{}, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: %w", err)
	}
	evaluationRoot := EvaluationRoot{Path: root, Digest: digest}
	if dimension != "traceability" {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Evidence: false, Root: evaluationRoot}, fmt.Errorf("U_GATE_EVIDENCE_UNAVAILABLE: unsupported check dimension %q", dimension)
	}
	snapshot, err := captureTraceSnapshot(root, nil)
	if err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Root: evaluationRoot}, err
	}
	scan, err := parseTraceabilitySnapshot(root, snapshot)
	if err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Root: evaluationRoot}, err
	}
	requirements := map[string]traceRequirement{}
	malformed := map[string]string{}
	for _, file := range snapshot.Files {
		if !file.Requirement {
			continue
		}
		requirement, parseErr := domain.ParseRequirement(file.Data)
		id := strings.TrimSuffix(filepath.Base(filepath.FromSlash(file.Path)), ".md")
		if parseErr != nil || requirement.ID != id {
			malformed[id] = file.Path
			continue
		}
		requirements[requirement.ID] = traceRequirement{status: requirement.Status, path: file.Path}
	}
	report := CheckReport{Verdict: "pass", Dimensions: map[string]string{"traceability": "pass"}}
	if len(requirements) == 0 {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Findings: []CheckFinding{{Code: "U_TRACE_EMPTY", Subject: "traceability", Message: "requirement registry is empty", Kind: "unevaluated"}}, Evidence: true, Root: evaluationRoot}, nil
	}
	if err := resolveTraceabilityEdges(&report, scan.Edges, requirements, malformed); err != nil {
		return DimensionEvaluation{}, err
	}
	predicate := gate.PredicatePass
	if len(report.Findings) > 0 {
		predicate = gate.PredicateFail
	} else if report.Unevaluated {
		predicate = gate.PredicateUnevaluated
	}
	findings := append([]CheckFinding{}, report.Findings...)
	findings = append(findings, report.UnevaluatedFindings...)
	findings = append(findings, report.Warnings...)
	return DimensionEvaluation{Predicate: predicate, Findings: findings, Evidence: true, Root: evaluationRoot}, nil
}

func (s *Store) RunGate(ctx context.Context, id string) (GateCheckResult, error) {
	discovered, err := s.discoverGates()
	if err != nil {
		return GateCheckResult{}, err
	}
	var found *discoveredGate
	for i := range discovered {
		if discovered[i].Definition.ID == id {
			found = &discovered[i]
			break
		}
	}
	if found == nil {
		return GateCheckResult{}, errors.New("E_NOT_FOUND: gate not found")
	}
	def := found.Definition
	if def.Kind == gate.KindManual {
		subjectDigest, digestErr := digestEvaluationRoot(s.root)
		if digestErr != nil {
			return GateCheckResult{}, digestErr
		}
		audit, auditErr := OpenGateAudit(s.commonDir, true)
		if auditErr != nil {
			return GateCheckResult{}, auditErr
		}
		canary, canaryErr := s.canaryFor(def)
		if canaryErr != nil {
			return GateCheckResult{}, canaryErr
		}
		declarationDigest, declarationErr := canary.DeclarationDigest()
		if declarationErr != nil {
			return GateCheckResult{}, declarationErr
		}
		_, auditErr = audit.Append("review", map[string]string{"gate_id": def.ID, "subject": subjectDigest, "challenge": "manual-negative", "required_evidence": "authenticated negative attestation", "definition_digest": found.Digest, "declaration_digest": declarationDigest, "subject_scope": subjectDigest, "lane": def.Lane.Name, "evaluator_version": def.Lane.EvaluatorVersion})
		if auditErr != nil {
			return GateCheckResult{}, auditErr
		}
		fields := gateResultFields(def, found.Digest, declarationDigest, subjectDigest, "attestation-challenge", "")
		fields["gate_id"], fields["subject"] = def.ID, subjectDigest
		fields["verdict"], fields["code"], fields["trusted"], fields["suspect"] = gate.VerdictUnevaluated, "U_GATE_UNPROVEN", "false", "true"
		fields["at"] = time.Now().UTC().Format(time.RFC3339Nano)
		record, auditErr := audit.Append("result", fields)
		if auditErr != nil {
			return GateCheckResult{}, auditErr
		}
		return GateCheckResult{GateID: def.ID, Kind: string(def.Kind), Subject: subjectDigest, Verdict: gate.VerdictUnevaluated, Code: "U_GATE_UNPROVEN", Suspect: true, Seq: record.Seq}, nil
	}
	canary, err := s.canaryFor(def)
	if err != nil {
		return GateCheckResult{}, err
	}
	subjectEval, subjectErr := s.evaluateChecker(ctx, def, s.root)
	if subjectErr != nil && subjectEval.Predicate != gate.PredicateUnevaluated {
		return GateCheckResult{}, subjectErr
	}
	declarationDigest, declarationErr := canary.DeclarationDigest()
	if declarationErr != nil {
		return GateCheckResult{}, declarationErr
	}
	canaryEval, canaryRoot, err := s.runCanary(ctx, canary, def)
	if err != nil {
		subject := subjectEval.Root.Digest
		if subject == "" {
			subject, _ = digestEvaluationRoot(s.root)
		}
		audit, auditErr := OpenGateAudit(s.commonDir, true)
		if auditErr != nil {
			return GateCheckResult{}, auditErr
		}
		record, appendErr := appendCanaryUnevaluated(audit, def, found.Digest, declarationDigest, subject, err)
		if appendErr != nil {
			return GateCheckResult{}, appendErr
		}
		code := "U_GATE_CANARY_UNEVALUATED"
		if candidate := ErrorCode(err); strings.HasPrefix(candidate, "U_GATE_") {
			code = candidate
		}
		return GateCheckResult{GateID: def.ID, Kind: string(def.Kind), Subject: subject, Verdict: gate.VerdictUnevaluated, Code: code, Suspect: true, Seq: record.Seq}, err
	}
	canaryHealth := gate.CanaryUnevaluated
	if canaryEval.Predicate == gate.PredicateFail {
		canaryHealth = gate.CanaryPass
	} else if canaryEval.Predicate == gate.PredicatePass {
		canaryHealth = gate.CanaryFail
	}
	audit, err := OpenGateAudit(s.commonDir, true)
	if err != nil {
		return GateCheckResult{}, err
	}
	proof := gate.ProofMissing
	proofSeq := ""
	if canaryHealth == gate.CanaryPass {
		proofFields := map[string]string{"gate_id": def.ID, "canary_id": canary.ID, "definition_digest": found.Digest, "declaration_digest": declarationDigest, "canary_tree_digest": canaryRoot.Digest, "subject_scope": subjectEval.Root.Digest, "lane": def.Lane.Name, "evaluator_version": def.Lane.EvaluatorVersion}
		if subjectEval.EnvDigest != "" {
			proofFields["env_digest"] = subjectEval.EnvDigest
		}
		proofRecord, appendErr := audit.Append("proof-of-fire", proofFields)
		err = appendErr
		if err != nil {
			return GateCheckResult{}, err
		}
		proofSeq = strconv.FormatUint(proofRecord.Seq, 10)
		proof = gate.ProofValid
	}
	fold := gate.FoldVerdict(subjectEval.Predicate, proof, canaryHealth, gate.EvidenceAvailable)
	if subjectEval.Code != "" && subjectEval.Predicate == gate.PredicateUnevaluated {
		fold.Code = subjectEval.Code
	}
	fields := gateResultFields(def, found.Digest, declarationDigest, subjectEval.Root.Digest, canaryRoot.Digest, proofSeq)
	if subjectEval.EnvDigest != "" {
		fields["env_digest"] = subjectEval.EnvDigest
	}
	fields["gate_id"], fields["subject"] = def.ID, subjectEval.Root.Digest
	fields["verdict"], fields["code"] = fold.Verdict, fold.Code
	fields["trusted"], fields["suspect"] = fmt.Sprintf("%t", fold.Trusted), fmt.Sprintf("%t", fold.Suspect)
	fields["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	record, err := audit.Append("result", fields)
	if err != nil {
		return GateCheckResult{}, err
	}
	return GateCheckResult{GateID: def.ID, Kind: string(def.Kind), Subject: subjectEval.Root.Digest, Verdict: fold.Verdict, Code: fold.Code, Trusted: fold.Trusted, Suspect: fold.Suspect, Seq: record.Seq}, nil
}

// AttestGate consumes an AIRA-issued manual challenge. A negative challenge
// attestation establishes proof-of-fire; a later matching positive attestation
// may then establish a manual pass. A caller cannot set a DB flag or import a
// free-form approval to bypass this sequence.
func (s *Store) AttestGate(ctx context.Context, id, verdict, actor string) (GateCheckResult, error) {
	_ = ctx
	discovered, err := s.discoverGates()
	if err != nil {
		return GateCheckResult{}, err
	}
	var found *discoveredGate
	for i := range discovered {
		if discovered[i].Definition.ID == id {
			found = &discovered[i]
			break
		}
	}
	if found == nil || found.Definition.Kind != gate.KindManual {
		return GateCheckResult{}, errors.New("E_GATE_ATTESTATION_INVALID: manual gate not found")
	}
	if verdict != gate.VerdictPass && verdict != gate.VerdictFail {
		return GateCheckResult{}, errors.New("E_GATE_ATTESTATION_INVALID: verdict must be pass or fail")
	}
	if strings.TrimSpace(actor) == "" {
		return GateCheckResult{}, errors.New("E_GATE_ATTESTATION_INVALID: actor is required")
	}
	subject, err := digestEvaluationRoot(s.root)
	if err != nil {
		return GateCheckResult{}, err
	}
	canary, err := s.canaryFor(found.Definition)
	if err != nil {
		return GateCheckResult{}, err
	}
	declarationDigest, err := canary.DeclarationDigest()
	if err != nil {
		return GateCheckResult{}, err
	}
	audit, err := OpenGateAudit(s.commonDir, true)
	if err != nil {
		return GateCheckResult{}, err
	}
	definitionDigest := found.Digest
	records, err := audit.Read()
	if err != nil {
		return GateCheckResult{}, err
	}
	var challenge GateAuditRecord
	for _, record := range records {
		if record.Type != "review" || record.Fields["gate_id"] != id || record.Fields["subject"] != subject || record.Fields["definition_digest"] != definitionDigest || record.Fields["declaration_digest"] != declarationDigest || record.Fields["lane"] != found.Definition.Lane.Name || record.Fields["evaluator_version"] != found.Definition.Lane.EvaluatorVersion {
			continue
		}
		if challenge.Seq == 0 || record.Seq > challenge.Seq {
			challenge = record
		}
	}
	if challenge.Seq == 0 {
		return GateCheckResult{}, errors.New("U_GATE_UNPROVEN: current manual challenge is missing")
	}
	challengeNonce := challenge.Nonce
	if verdict == gate.VerdictFail {
		record, err := audit.Append("attestation", map[string]string{"gate_id": id, "subject": subject, "actor": actor, "attested_result": "fail", "challenge": "manual-negative", "challenge_nonce": challengeNonce, "definition_digest": definitionDigest, "declaration_digest": declarationDigest, "subject_scope": subject, "lane": found.Definition.Lane.Name, "evaluator_version": found.Definition.Lane.EvaluatorVersion})
		if err != nil {
			return GateCheckResult{}, err
		}
		return GateCheckResult{GateID: id, Kind: string(found.Definition.Kind), Subject: subject, Verdict: gate.VerdictFail, Code: "E_GATE_FAILED", Seq: record.Seq}, nil
	}
	proof := false
	for _, record := range records {
		if record.Type == "attestation" && record.Fields["gate_id"] == id && record.Fields["subject"] == subject && record.Fields["attested_result"] == "fail" && record.Fields["declaration_digest"] == declarationDigest && record.Fields["challenge_nonce"] == challengeNonce {
			proof = true
			break
		}
	}
	if !proof {
		return GateCheckResult{}, errors.New("U_GATE_UNPROVEN: negative manual challenge has not been attested")
	}
	if _, err := audit.Append("attestation", map[string]string{"gate_id": id, "subject": subject, "actor": actor, "attested_result": "pass", "challenge": "manual-positive", "challenge_nonce": challengeNonce, "definition_digest": definitionDigest, "declaration_digest": declarationDigest, "subject_scope": subject, "lane": found.Definition.Lane.Name, "evaluator_version": found.Definition.Lane.EvaluatorVersion}); err != nil {
		return GateCheckResult{}, err
	}
	proofRecord, err := audit.Append("proof-of-fire", map[string]string{"gate_id": id, "canary_id": canary.ID, "definition_digest": definitionDigest, "declaration_digest": declarationDigest, "canary_tree_digest": "attestation-challenge", "subject_scope": subject, "lane": found.Definition.Lane.Name, "evaluator_version": found.Definition.Lane.EvaluatorVersion})
	if err != nil {
		return GateCheckResult{}, err
	}
	fields := gateResultFields(found.Definition, definitionDigest, declarationDigest, subject, "attestation-challenge", strconv.FormatUint(proofRecord.Seq, 10))
	fields["gate_id"], fields["subject"] = id, subject
	fields["verdict"], fields["trusted"], fields["suspect"], fields["code"] = gate.VerdictPass, "true", "false", ""
	fields["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	result, err := audit.Append("result", fields)
	if err != nil {
		return GateCheckResult{}, err
	}
	return GateCheckResult{GateID: id, Kind: string(found.Definition.Kind), Subject: subject, Verdict: gate.VerdictPass, Trusted: true, Seq: result.Seq}, nil
}

// GateAction is the common grouped-face seam for the non-evaluation gate
// operations. Content mutation remains validated by the normal git content
// workflow; these actions never mint a verdict from SQLite.
func (s *Store) GateAction(ctx context.Context, operation, gateID, canaryID string) (any, error) {
	switch operation {
	case "add", "set", "show":
		gates, err := s.ListGates()
		if err != nil {
			return nil, err
		}
		for _, definition := range gates {
			if definition.ID == gateID {
				return definition, nil
			}
		}
		return nil, errors.New("E_NOT_FOUND: gate not found")
	case "prove":
		report, err := s.GateCheck(ctx)
		if err != nil {
			return nil, err
		}
		for _, result := range report.Results {
			if result.GateID == gateID {
				return result, nil
			}
		}
		return nil, errors.New("U_GATE_NO_RESULT: no gate result to prove")
	case "review":
		discovered, err := s.discoverGates()
		if err != nil {
			return nil, err
		}
		for _, definition := range discovered {
			if definition.Definition.ID != gateID {
				continue
			}
			subject, digestErr := digestEvaluationRoot(s.root)
			if digestErr != nil {
				return nil, digestErr
			}
			canary, canaryErr := s.canaryFor(definition.Definition)
			if canaryErr != nil {
				return nil, canaryErr
			}
			declarationDigest, declarationErr := canary.DeclarationDigest()
			if declarationErr != nil {
				return nil, declarationErr
			}
			audit, auditErr := OpenGateAudit(s.commonDir, true)
			if auditErr != nil {
				return nil, auditErr
			}
			record, auditErr := audit.Append("review", map[string]string{"gate_id": gateID, "subject": subject, "challenge": "manual-review", "required_evidence": "gate evidence", "definition_digest": definition.Digest, "declaration_digest": declarationDigest, "subject_scope": subject, "lane": definition.Definition.Lane.Name, "evaluator_version": definition.Definition.Lane.EvaluatorVersion})
			if auditErr != nil {
				return nil, auditErr
			}
			return map[string]any{"gate_id": gateID, "challenge": record.Nonce, "subject": subject}, nil
		}
		return nil, errors.New("E_NOT_FOUND: gate not found")
	case "canary-show":
		gates, err := s.discoverGates()
		if err != nil {
			return nil, err
		}
		for _, definition := range gates {
			for _, id := range definition.Definition.CanaryIDs {
				if id == canaryID {
					return s.canaryFor(definition.Definition)
				}
			}
		}
		return nil, errors.New("E_NOT_FOUND: canary not found")
	case "canary-run":
		gates, err := s.discoverGates()
		if err != nil {
			return nil, err
		}
		for _, definition := range gates {
			for _, id := range definition.Definition.CanaryIDs {
				if id != canaryID {
					continue
				}
				canary, canaryErr := s.canaryFor(definition.Definition)
				if canaryErr != nil {
					return nil, canaryErr
				}
				evaluation, root, runErr := s.runCanary(ctx, canary, definition.Definition)
				if runErr != nil {
					return nil, runErr
				}
				return map[string]any{"canary_id": canaryID, "verdict": evaluation.Predicate, "tree_digest": root.Digest, "findings": evaluation.Findings}, nil
			}
		}
		return nil, errors.New("E_NOT_FOUND: canary not found")
	default:
		return nil, fmt.Errorf("E_GATE_INVALID: unknown gate operation %q", operation)
	}
}

// GateActionWithFields is the transport-parity seam for the extended gate
// descriptors. Gate files remain the authenticated source of truth; fields are
// passed through for callers that materialize content changes and are not used
// to mint a verdict directly.
func (s *Store) GateActionWithFields(ctx context.Context, operation, gateID, canaryID string, fields map[string]any) (any, error) {
	_ = fields
	return s.GateAction(ctx, operation, gateID, canaryID)
}

func (s *Store) runCanary(ctx context.Context, c gate.CanaryDeclaration, def gate.GateDefinition) (DimensionEvaluation, EvaluationRoot, error) {
	_ = ctx
	if c.Mode != gate.CanaryFixture && c.Mode != gate.CanaryMutation {
		return DimensionEvaluation{}, EvaluationRoot{}, errors.New("E_GATE_CANARY_INVALID: M10a evaluation requires fixture canary")
	}
	dir, err := os.MkdirTemp("", "aira-gate-canary-")
	if err != nil {
		return DimensionEvaluation{}, EvaluationRoot{}, err
	}
	defer os.RemoveAll(dir)
	if c.Seed.Path != "" {
		if !safeFixturePath(c.Seed.Path) {
			return DimensionEvaluation{}, EvaluationRoot{}, errors.New("E_GATE_CANARY_INVALID: seed escapes fixture root")
		}
		source := filepath.Join(s.root, filepath.FromSlash(c.Seed.Path))
		if err := copyFixtureSeed(source, dir); err != nil {
			return DimensionEvaluation{}, EvaluationRoot{}, err
		}
	}
	for path, data := range c.Seed.Files {
		if !safeFixturePath(path) {
			return DimensionEvaluation{}, EvaluationRoot{}, errors.New("E_GATE_CANARY_INVALID: seed escapes fixture root")
		}
		clean := filepath.Clean(filepath.FromSlash(path))
		absolute := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			return DimensionEvaluation{}, EvaluationRoot{}, err
		}
		if err := os.WriteFile(absolute, []byte(data), 0o644); err != nil {
			return DimensionEvaluation{}, EvaluationRoot{}, err
		}
	}
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	if err := cmd.Run(); err != nil {
		return DimensionEvaluation{}, EvaluationRoot{}, fmt.Errorf("U_GATE_CANARY_UNEVALUATED: git init: %w", err)
	}
	cmd = exec.Command("git", "-C", dir, "add", "-A")
	if err := cmd.Run(); err != nil {
		return DimensionEvaluation{}, EvaluationRoot{}, fmt.Errorf("U_GATE_CANARY_UNEVALUATED: git add: %w", err)
	}
	if c.Mode == gate.CanaryMutation {
		if c.Mutation == nil {
			return DimensionEvaluation{}, EvaluationRoot{}, errors.New("U_GATE_MUTATION_APPLY_FAILED: mutation seed is missing")
		}
		mutationRoot, mutationCleanup, materializeErr := materializeTrackedSnapshot(s.root)
		if materializeErr != nil {
			return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_MUTATION_APPLY_FAILED"}, EvaluationRoot{}, fmt.Errorf("U_GATE_MUTATION_APPLY_FAILED: %w", materializeErr)
		}
		defer mutationCleanup()
		if mutationErr := applyMutation(mutationRoot, *c.Mutation); mutationErr != nil {
			return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_MUTATION_APPLY_FAILED"}, EvaluationRoot{Path: mutationRoot}, fmt.Errorf("U_GATE_MUTATION_APPLY_FAILED: %w", mutationErr)
		}
		if _, stderr, gitErr := runGit(mutationRoot, "add", "-A"); gitErr != nil {
			return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_MUTATION_APPLY_FAILED"}, EvaluationRoot{Path: mutationRoot}, fmt.Errorf("U_GATE_MUTATION_APPLY_FAILED: git add: %w: %s", gitErr, stderr)
		}
		evaluation, err := s.evaluateChecker(ctx, def, mutationRoot)
		return evaluation, evaluation.Root, err
	}
	evaluation, err := s.evaluateChecker(ctx, def, dir)
	return evaluation, evaluation.Root, err
}

// runFixtureCanary is retained as a compatibility seam for M10a tests and
// delegates to the mode-dispatched implementation.
func (s *Store) runFixtureCanary(ctx context.Context, c gate.CanaryDeclaration, def gate.GateDefinition) (DimensionEvaluation, EvaluationRoot, error) {
	return s.runCanary(ctx, c, def)
}

func safeFixturePath(path string) bool {
	if path == "" || filepath.IsAbs(filepath.FromSlash(path)) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".git" || part == ".." {
			return false
		}
	}
	return true
}

func copyFixtureSeed(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("U_GATE_CANARY_UNEVALUATED: fixture seed: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("E_GATE_CANARY_INVALID: symlink seed is not allowed")
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || (rel != "." && !safeFixturePath(rel)) {
			return errors.New("E_GATE_CANARY_INVALID: seed escapes fixture root")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("E_GATE_CANARY_INVALID: symlink seed is not allowed")
		}
		target := destination
		if rel != "." {
			target = filepath.Join(destination, rel)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return errors.New("E_GATE_CANARY_INVALID: non-regular seed entry")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func (s *Store) GateCheck(ctx context.Context) (GateCheckReport, error) {
	_ = ctx
	discovered, err := s.discoverGates()
	if err != nil {
		return GateCheckReport{}, err
	}
	report := GateCheckReport{Verdict: gate.VerdictPass, Results: []GateCheckResult{}}
	if len(discovered) == 0 {
		return report, nil
	}
	audit, err := OpenGateAudit(s.commonDir, false)
	if err != nil {
		return GateCheckReport{}, err
	}
	records, err := audit.Read()
	if err != nil {
		for _, d := range discovered {
			code := "U_GATE_NO_RESULT"
			if ErrorCode(err) == "E_JOURNAL_CORRUPT" {
				code = "E_JOURNAL_CORRUPT"
			}
			report.Results = append(report.Results, GateCheckResult{GateID: d.Definition.ID, Kind: string(d.Definition.Kind), Subject: s.root, Verdict: gate.VerdictUnevaluated, Code: code, Suspect: true})
		}
		return finishGateReport(report), nil
	}
	latest := gateProjectionRows(records)
	proofs := make([]GateAuditRecord, 0)
	for _, record := range records {
		if record.Type == "proof-of-fire" {
			proofs = append(proofs, record)
		}
	}
	subjectDigest, err := digestEvaluationRoot(s.root)
	if err != nil {
		return GateCheckReport{}, err
	}
	for _, d := range discovered {
		currentSubjectDigest := subjectDigest
		if d.Definition.Lane.Checker == string(gate.CheckerCommand) {
			currentSubjectDigest, err = digestTrackedRoot(s.root)
			if err != nil {
				return GateCheckReport{}, err
			}
		}
		record, ok := latest[d.Definition.ID+"\x00"+currentSubjectDigest]
		if !ok {
			report.Results = append(report.Results, GateCheckResult{GateID: d.Definition.ID, Kind: string(d.Definition.Kind), Subject: currentSubjectDigest, Verdict: gate.VerdictUnevaluated, Code: "U_GATE_NO_RESULT", Suspect: true})
			continue
		}
		result := GateCheckResult{GateID: d.Definition.ID, Kind: string(d.Definition.Kind), Subject: currentSubjectDigest, Verdict: record.Fields["verdict"], Code: record.Fields["code"], Trusted: record.Fields["trusted"] == "true", Suspect: record.Fields["suspect"] == "true", Seq: record.Seq}
		if result.Verdict == gate.VerdictPass {
			canary, canaryErr := s.canaryFor(d.Definition)
			declarationDigest, digestErr := canary.DeclarationDigest()
			currentDefinitionDigest, definitionErr := gate.DigestGate(d.Definition)
			bindingMismatch := canaryErr != nil || digestErr != nil || definitionErr != nil ||
				currentDefinitionDigest != record.Fields["definition_digest"] ||
				declarationDigest != record.Fields["declaration_digest"] ||
				record.Fields["subject_scope"] != currentSubjectDigest ||
				record.Fields["lane"] != d.Definition.Lane.Name ||
				record.Fields["evaluator_version"] != d.Definition.Lane.EvaluatorVersion ||
				record.Fields["canary_tree_digest"] == ""
			if bindingMismatch {
				result.Verdict, result.Code, result.Trusted, result.Suspect = gate.VerdictUnevaluated, "U_GATE_PROOF_STALE", false, true
			} else {
				proofSeq, parseErr := strconv.ParseUint(record.Fields["proof_seq"], 10, 64)
				if parseErr != nil || proofSeq == 0 {
					result.Verdict, result.Code, result.Trusted, result.Suspect = gate.VerdictUnevaluated, "U_GATE_UNPROVEN", false, true
				} else {
					var linked *GateAuditRecord
					for i := range proofs {
						if proofs[i].Seq == proofSeq {
							linked = &proofs[i]
							break
						}
					}
					if linked == nil {
						result.Verdict, result.Code, result.Trusted, result.Suspect = gate.VerdictUnevaluated, "U_GATE_UNPROVEN", false, true
					} else if linked.Fields["gate_id"] != d.Definition.ID || linked.Fields["canary_id"] != canary.ID || linked.Fields["definition_digest"] != record.Fields["definition_digest"] || linked.Fields["declaration_digest"] != record.Fields["declaration_digest"] || linked.Fields["canary_tree_digest"] != record.Fields["canary_tree_digest"] || linked.Fields["subject_scope"] != currentSubjectDigest || linked.Fields["lane"] != d.Definition.Lane.Name || linked.Fields["evaluator_version"] != d.Definition.Lane.EvaluatorVersion || (d.Definition.Command != nil && linked.Fields["env_digest"] != record.Fields["env_digest"]) {
						result.Verdict, result.Code, result.Trusted, result.Suspect = gate.VerdictUnevaluated, "U_GATE_PROOF_STALE", false, true
					}
				}
				if result.Verdict == gate.VerdictPass && d.Definition.Command != nil {
					currentEnvDigest, envErr := currentCommandEnvDigest(*d.Definition.Command)
					if envErr != nil || currentEnvDigest != record.Fields["env_digest"] {
						result.Verdict, result.Code, result.Trusted, result.Suspect = gate.VerdictUnevaluated, "U_GATE_PROOF_STALE", false, true
					}
				}
			}
			if result.Verdict == gate.VerdictPass && d.Definition.ProofPolicy.MaxAgeSecs > 0 {
				when, parseErr := time.Parse(time.RFC3339Nano, record.Fields["at"])
				if parseErr != nil || time.Since(when) > time.Duration(d.Definition.ProofPolicy.MaxAgeSecs)*time.Second {
					result.Verdict, result.Code, result.Trusted, result.Suspect = gate.VerdictUnevaluated, "U_GATE_PROOF_STALE", false, true
				}
			}
		}
		if result.Verdict == gate.VerdictPass && !result.Trusted {
			result.Verdict = gate.VerdictUnevaluated
			result.Code = "U_GATE_UNPROVEN"
			result.Suspect = true
		}
		report.Results = append(report.Results, result)
	}
	return finishGateReport(report), nil
}

func finishGateReport(report GateCheckReport) GateCheckReport {
	for _, result := range report.Results {
		switch result.Verdict {
		case gate.VerdictFail:
			report.Failed++
		case gate.VerdictUnevaluated:
			report.Unevaluated++
		case gate.VerdictPass:
			report.Passed++
		}
	}
	if report.Failed > 0 {
		report.Verdict = gate.VerdictFail
	} else if report.Unevaluated > 0 {
		report.Verdict = gate.VerdictUnevaluated
	}
	return report
}

func (s *Store) checkGatesReadOnly(report *CheckReport) error {
	gateReport, err := s.GateCheck(context.Background())
	if err != nil {
		return err
	}
	for _, result := range gateReport.Results {
		finding := CheckFinding{Code: result.Code, Subject: result.GateID, Message: result.Verdict, Kind: "unevaluated"}
		if result.Verdict == gate.VerdictFail {
			finding.Kind = "fail"
			if finding.Code == "" {
				finding.Code = "E_GATE_FAILED"
			}
		}
		addFinding(report, finding, "gates")
	}
	return nil
}
