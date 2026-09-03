package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gate"
)

const ratchetComparatorVersion = "m13b-v1"

type RatchetSnapshot struct {
	FailingSet      []string         `json:"failing_set"`
	DiscoveredCount int              `json:"discovered_count"`
	Coverage        *domain.Coverage `json:"coverage,omitempty"`
}

type GateBaseline struct {
	GateID            string             `json:"gate_id"`
	Seq               uint64             `json:"seq"`
	Comparator        string             `json:"comparator"`
	ComparatorVersion string             `json:"comparator_version"`
	Lane              string             `json:"lane"`
	ComparisonKey     gate.ComparisonKey `json:"comparison_key"`
	SourceReportIDs   []string           `json:"source_report_ids"`
	SourceCommit      string             `json:"source_commit"`
	EvidenceDigest    string             `json:"evidence_digest"`
	SnapshotDigest    string             `json:"snapshot_digest"`
	Snapshot          RatchetSnapshot    `json:"snapshot"`
	PinActor          string             `json:"pin_actor"`
	PinAt             string             `json:"pin_at"`
	PinReason         string             `json:"pin_reason,omitempty"`
}

type RatchetComparison struct {
	Predicate      gate.PredicateState `json:"predicate"`
	Code           string              `json:"code,omitempty"`
	CurrentFailing []string            `json:"current_failing,omitempty"`
	NewFailures    []string            `json:"new_failures,omitempty"`
	ExcludedFlaky  []string            `json:"excluded_flaky,omitempty"`
	BaselinePct    *float64            `json:"baseline_pct,omitempty"`
	CurrentPct     *float64            `json:"current_pct,omitempty"`
	Delta          *float64            `json:"delta,omitempty"`
}

func compareNoNewFailures(snapshot RatchetSnapshot, currentFailing []string, excluded map[string]struct{}) RatchetComparison {
	baseline := make(map[string]struct{}, len(snapshot.FailingSet))
	for _, name := range snapshot.FailingSet {
		baseline[name] = struct{}{}
	}
	currentSet := make(map[string]struct{}, len(currentFailing))
	for _, name := range currentFailing {
		currentSet[name] = struct{}{}
	}
	newFailures := make([]string, 0)
	excludedNames := make([]string, 0)
	for name := range currentSet {
		if _, exists := baseline[name]; exists {
			continue
		}
		if _, flaky := excluded[name]; flaky {
			excludedNames = append(excludedNames, name)
			continue
		}
		newFailures = append(newFailures, name)
	}
	sort.Strings(newFailures)
	sort.Strings(excludedNames)
	current := sortedSet(currentSet)
	comparison := RatchetComparison{Predicate: gate.PredicatePass, CurrentFailing: current, NewFailures: newFailures, ExcludedFlaky: excludedNames}
	if len(newFailures) > 0 {
		comparison.Predicate = gate.PredicateFail
		comparison.Code = "E_GATE_RATCHET_REGRESSED"
	}
	return comparison
}

func compareCoverage(snapshot RatchetSnapshot, currentPcts []float64) RatchetComparison {
	comparison := RatchetComparison{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_INCOMPARABLE"}
	if snapshot.Coverage == nil || snapshot.Coverage.Pct == nil || len(currentPcts) == 0 {
		return comparison
	}
	baseline := *snapshot.Coverage.Pct
	for _, pct := range currentPcts {
		if pct != currentPcts[0] {
			return comparison
		}
	}
	current := currentPcts[0]
	delta := current - baseline
	comparison.BaselinePct, comparison.CurrentPct, comparison.Delta = &baseline, &current, &delta
	comparison.Predicate = gate.PredicatePass
	comparison.Code = ""
	if current < baseline {
		comparison.Predicate = gate.PredicateFail
		comparison.Code = "E_GATE_RATCHET_REGRESSED"
	}
	return comparison
}

func (s *Store) evaluateRatchet(ctx context.Context, def gate.GateDefinition, root string) (DimensionEvaluation, error) {
	evaluation := DimensionEvaluation{Root: EvaluationRoot{Path: root}}
	if root != "" {
		digest, err := subjectTreeDigest(root)
		if err != nil {
			evaluation.Predicate, evaluation.Code = gate.PredicateUnevaluated, "U_GATE_EVIDENCE_UNAVAILABLE"
			return evaluation, nil
		}
		evaluation.Root.Digest = digest
	}
	baseline, err := s.ResolveGateBaseline(def.ID)
	if err != nil {
		code := ErrorCode(err)
		if code == "U_GATE_BASELINE_MISSING" {
			evaluation.Predicate, evaluation.Code, evaluation.Evidence = gate.PredicateUnevaluated, code, false
			return evaluation, nil
		}
		return evaluation, err
	}
	if def.Ratchet == nil || baseline.ComparisonKey != def.Ratchet.ComparisonKey || baseline.Comparator != def.Ratchet.Comparator || baseline.ComparatorVersion != ratchetComparatorVersion || baseline.Lane != def.Lane.Name {
		evaluation.Predicate, evaluation.Code, evaluation.Evidence = gate.PredicateUnevaluated, "U_GATE_PROOF_STALE", false
		return evaluation, nil
	}
	subjectCommit := s.gitValue(ctx, "HEAD")
	reports, err := s.loadAllTestReports(ctx, s.db)
	if err != nil {
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "U_GATE_INCOMPARABLE", Root: evaluation.Root}, err
	}
	selected := make([]domain.TestReport, 0)
	for _, report := range reports {
		if report.Commit != subjectCommit || report.SuiteID != baseline.ComparisonKey.SuiteID || report.Config != baseline.ComparisonKey.Config || report.EnvDigest != baseline.ComparisonKey.EnvDigest || report.Shard != baseline.ComparisonKey.Shard {
			continue
		}
		if report.RetryIndex != 0 {
			continue
		}
		selected = append(selected, report)
	}
	if len(selected) == 0 {
		evaluation.Predicate, evaluation.Code, evaluation.Evidence = gate.PredicateUnevaluated, "U_GATE_INCOMPARABLE", false
		return evaluation, nil
	}
	currentFailing := map[string]struct{}{}
	type outcomes struct{ pass, fail bool }
	currentOutcomes := map[string]outcomes{}
	currentPcts := make([]float64, 0, len(selected))
	for _, report := range selected {
		if !report.ParserComplete || len(report.Results) == 0 {
			evaluation.Predicate, evaluation.Code, evaluation.Evidence = gate.PredicateUnevaluated, "U_GATE_INCOMPARABLE", false
			return evaluation, nil
		}
		for _, result := range report.Results {
			if result.Name == "" {
				evaluation.Predicate, evaluation.Code, evaluation.Evidence = gate.PredicateUnevaluated, "U_GATE_INCOMPARABLE", false
				return evaluation, nil
			}
			outcome := currentOutcomes[result.Name]
			switch result.Outcome {
			case domain.OutcomePass:
				outcome.pass = true
			case domain.OutcomeFail, domain.OutcomeError:
				outcome.fail = true
			}
			if outcome.pass && outcome.fail {
				evaluation.Predicate, evaluation.Code, evaluation.Evidence = gate.PredicateUnevaluated, "U_GATE_INCOMPARABLE", false
				return evaluation, nil
			}
			currentOutcomes[result.Name] = outcome
			if result.Outcome == domain.OutcomeFail || result.Outcome == domain.OutcomeError {
				currentFailing[result.Name] = struct{}{}
			}
		}
		if def.Ratchet.Comparator == "coverage-drop" {
			if report.Coverage == nil || report.Coverage.Pct == nil {
				evaluation.Predicate, evaluation.Code, evaluation.Evidence = gate.PredicateUnevaluated, "U_GATE_INCOMPARABLE", false
				return evaluation, nil
			}
			currentPcts = append(currentPcts, *report.Coverage.Pct)
		}
	}
	comparison := RatchetComparison{}
	switch def.Ratchet.Comparator {
	case "no-new-failures":
		excluded := s.flakyExclusions(reports, baseline.ComparisonKey, subjectCommit, sortedSet(currentFailing))
		comparison = compareNoNewFailures(baseline.Snapshot, sortedSet(currentFailing), excluded)
	case "coverage-drop":
		comparison = compareCoverage(baseline.Snapshot, currentPcts)
	default:
		return DimensionEvaluation{Predicate: gate.PredicateUnevaluated, Code: "E_GATE_INVALID", Root: evaluation.Root}, errors.New("E_GATE_INVALID: unsupported ratchet comparator")
	}
	evaluation.Predicate, evaluation.Code, evaluation.Evidence = comparison.Predicate, comparison.Code, true
	return evaluation, nil
}

func (s *Store) flakyExclusions(reports []domain.TestReport, key gate.ComparisonKey, commit string, failing []string) map[string]struct{} {
	requested := make(map[string]struct{}, len(failing))
	for _, name := range failing {
		requested[name] = struct{}{}
	}
	excluded := map[string]struct{}{}
	for _, test := range computeFlakyTests(reports) {
		if _, wanted := requested[test.Name]; !wanted {
			continue
		}
		for _, cell := range test.Cells {
			if cell.State == domain.FlakyStateFlaky && cell.Commit == commit && cell.SuiteID == key.SuiteID && cell.Config == key.Config && cell.EnvDigest == key.EnvDigest && cell.Shard == key.Shard {
				excluded[test.Name] = struct{}{}
			}
		}
	}
	return excluded
}

func (s *Store) PinGateBaseline(ctx context.Context, gateID string, reportIDs []string, actor, reason string) (GateBaseline, error) {
	definition, err := s.ratchetDefinition(gateID)
	if err != nil {
		return GateBaseline{}, err
	}
	reports, err := s.loadReportsByID(ctx, reportIDs)
	if err != nil {
		return GateBaseline{}, err
	}
	baseline, err := deriveRatchetBaseline(definition.Definition, reports, actor, reason)
	if err != nil {
		return GateBaseline{}, err
	}
	snapshotJSON, err := canonicalSnapshotJSON(baseline.Snapshot)
	if err != nil {
		return GateBaseline{}, err
	}
	baseline.SnapshotDigest = ratchetDigestBytes(snapshotJSON)
	baseline.EvidenceDigest = digestEvidence(reports)
	fields := map[string]string{
		"gate_id":            baseline.GateID,
		"comparator":         baseline.Comparator,
		"comparator_version": baseline.ComparatorVersion,
		"lane":               baseline.Lane,
		"comparison_key":     mustJSON(baseline.ComparisonKey),
		"source_commit":      baseline.SourceCommit,
		"source_report_ids":  mustJSON(baseline.SourceReportIDs),
		"evidence_digest":    baseline.EvidenceDigest,
		"snapshot_digest":    baseline.SnapshotDigest,
		"snapshot_json":      string(snapshotJSON),
		"pin_actor":          baseline.PinActor,
		"pin_at":             baseline.PinAt,
		"pin_reason":         baseline.PinReason,
	}
	audit, err := OpenGateAudit(s.commonDir, true)
	if err != nil {
		return GateBaseline{}, err
	}
	record, err := audit.Append("baseline", fields)
	if err != nil {
		return GateBaseline{}, err
	}
	baseline.Seq = record.Seq
	if _, err := audit.Append("baseline-pointer", map[string]string{
		"gate_id": baseline.GateID, "active_baseline_seq": strconv.FormatUint(record.Seq, 10),
	}); err != nil {
		return GateBaseline{}, err
	}
	if err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		for _, id := range baseline.SourceReportIDs {
			if _, err := conn.ExecContext(ctx, `UPDATE test_reports SET pinned=1 WHERE project_id=? AND id=?`, s.projectID, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return GateBaseline{}, err
	}
	return baseline, nil
}

func (s *Store) ShowGateBaseline(gateID string) (GateBaseline, error) {
	return s.ResolveGateBaseline(gateID)
}

func (s *Store) ratchetDefinition(gateID string) (discoveredGate, error) {
	items, err := s.discoverGates()
	if err != nil {
		return discoveredGate{}, err
	}
	for _, item := range items {
		if item.Definition.ID == gateID {
			if item.Definition.Kind != gate.KindRatchet || item.Definition.Ratchet == nil {
				return discoveredGate{}, errors.New("E_GATE_BASELINE_INVALID: gate is not a ratchet gate")
			}
			return item, nil
		}
	}
	return discoveredGate{}, errors.New("E_NOT_FOUND: gate not found")
}

func deriveRatchetBaseline(def gate.GateDefinition, reports []domain.TestReport, actor, reason string) (GateBaseline, error) {
	if len(reports) == 0 {
		return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: at least one report is required")
	}
	key := gate.ComparisonKey{}
	var sourceCommit string
	failing := map[string]struct{}{}
	discovered := map[string]struct{}{}
	var coverage *domain.Coverage
	coverageSet := false
	ids := make([]string, 0, len(reports))
	seenIDs := map[string]struct{}{}
	for _, report := range reports {
		if _, exists := seenIDs[report.ID]; exists {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: duplicate report id")
		}
		seenIDs[report.ID] = struct{}{}
		ids = append(ids, report.ID)
		if report.RetryIndex != 0 {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: retry reports cannot be pinned")
		}
		if !report.ParserComplete {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: parser-incomplete report cannot be pinned")
		}
		if len(report.Results) == 0 {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: zero-discovered report cannot be pinned")
		}
		if report.Commit == "" || report.SuiteID == "" || report.Config == "" || report.EnvDigest == "" || report.Shard == "" {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: report identity is incomplete")
		}
		reportKey := gate.ComparisonKey{SuiteID: report.SuiteID, Config: report.Config, EnvDigest: report.EnvDigest, Shard: report.Shard}
		if key.SuiteID == "" {
			key, sourceCommit = reportKey, report.Commit
		} else if key != reportKey || sourceCommit != report.Commit {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: reports do not share one comparison cell and commit")
		}
		if def.Ratchet != nil && def.Ratchet.ComparisonKey != key {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: reports do not match gate comparison_key")
		}
		currentCoverage := report.Coverage
		if !coverageSet {
			coverage = cloneCoverage(currentCoverage)
			coverageSet = true
		} else if !sameCoverage(coverage, currentCoverage) {
			return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: reports have differing coverage")
		}
		for _, result := range report.Results {
			if result.Name == "" {
				return GateBaseline{}, errors.New("E_GATE_BASELINE_INVALID: report has missing test identity")
			}
			discovered[result.Name] = struct{}{}
			if result.Outcome == domain.OutcomeFail || result.Outcome == domain.OutcomeError {
				failing[result.Name] = struct{}{}
			}
		}
	}
	sort.Strings(ids)
	failingSet := sortedSet(failing)
	snapshot := RatchetSnapshot{FailingSet: failingSet, DiscoveredCount: len(discovered), Coverage: coverage}
	if actor == "" {
		actor = "aira"
	}
	return GateBaseline{GateID: def.ID, Comparator: def.Ratchet.Comparator, ComparatorVersion: ratchetComparatorVersion,
		Lane: def.Lane.Name, ComparisonKey: key, SourceReportIDs: ids, SourceCommit: sourceCommit,
		Snapshot: snapshot, PinActor: actor, PinAt: time.Now().UTC().Format(time.RFC3339Nano), PinReason: reason}, nil
}

func (s *Store) loadReportsByID(ctx context.Context, ids []string) ([]domain.TestReport, error) {
	if len(ids) == 0 {
		return nil, errors.New("E_GATE_BASELINE_INVALID: at least one report is required")
	}
	reports := make([]domain.TestReport, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errors.New("E_GATE_BASELINE_INVALID: report id is empty")
		}
		if _, ok := seen[id]; ok {
			return nil, errors.New("E_GATE_BASELINE_INVALID: duplicate report id")
		}
		seen[id] = struct{}{}
		row := s.db.QueryRowContext(ctx, `SELECT id,ticket_id,phase,"commit",branch,worktree_id,agent,session,at,run_ref,suite_id,runner,config,env_digest,shard,retry_index,parser_complete,coverage_pct,lines_covered,lines_total,format,source_digest,at_seq,pinned FROM test_reports WHERE project_id=? AND id=?`, s.projectID, id)
		report, err := scanTestReportHeader(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("E_GATE_BASELINE_INVALID: report %s not found", id)
		}
		if err != nil {
			return nil, err
		}
		report.Results, err = s.loadTestResults(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func (s *Store) ResolveGateBaseline(gateID string) (GateBaseline, error) {
	audit, err := OpenGateAudit(s.commonDir, false)
	if err != nil {
		return GateBaseline{}, err
	}
	records, err := audit.Read()
	if errors.Is(err, errGateAuditEmpty) {
		return GateBaseline{}, errors.New("U_GATE_BASELINE_MISSING: no active baseline pointer")
	}
	if err != nil {
		return GateBaseline{}, err
	}
	var pointer *GateAuditRecord
	for i := range records {
		if records[i].Type == "baseline-pointer" && records[i].Fields["gate_id"] == gateID {
			candidate := records[i]
			pointer = &candidate
		}
	}
	if pointer == nil {
		return GateBaseline{}, errors.New("U_GATE_BASELINE_MISSING: no active baseline pointer")
	}
	seq, parseErr := strconv.ParseUint(pointer.Fields["active_baseline_seq"], 10, 64)
	if parseErr != nil || seq == 0 {
		return GateBaseline{}, errors.New("U_GATE_BASELINE_MISSING: active baseline pointer is invalid")
	}
	for _, record := range records {
		if record.Type != "baseline" || record.Seq != seq || record.Fields["gate_id"] != gateID {
			continue
		}
		baseline, decodeErr := baselineFromAuditRecord(record)
		if decodeErr != nil {
			return GateBaseline{}, fmt.Errorf("U_GATE_BASELINE_MISSING: %w", decodeErr)
		}
		return baseline, nil
	}
	return GateBaseline{}, errors.New("U_GATE_BASELINE_MISSING: active baseline record is absent")
}

func baselineFromAuditRecord(record GateAuditRecord) (GateBaseline, error) {
	fields := record.Fields
	var key gate.ComparisonKey
	if err := json.Unmarshal([]byte(fields["comparison_key"]), &key); err != nil {
		return GateBaseline{}, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(fields["source_report_ids"]), &ids); err != nil {
		return GateBaseline{}, err
	}
	var snapshot RatchetSnapshot
	if err := json.Unmarshal([]byte(fields["snapshot_json"]), &snapshot); err != nil {
		return GateBaseline{}, err
	}
	snapshotJSON, err := canonicalSnapshotJSON(snapshot)
	if err != nil || ratchetDigestBytes(snapshotJSON) != fields["snapshot_digest"] {
		return GateBaseline{}, errors.New("snapshot digest mismatch")
	}
	return GateBaseline{GateID: fields["gate_id"], Seq: record.Seq, Comparator: fields["comparator"], ComparatorVersion: fields["comparator_version"], Lane: fields["lane"], ComparisonKey: key, SourceReportIDs: ids, SourceCommit: fields["source_commit"], EvidenceDigest: fields["evidence_digest"], SnapshotDigest: fields["snapshot_digest"], Snapshot: snapshot, PinActor: fields["pin_actor"], PinAt: fields["pin_at"], PinReason: fields["pin_reason"]}, nil
}

func canonicalSnapshotJSON(snapshot RatchetSnapshot) ([]byte, error) {
	snapshot.FailingSet = append([]string(nil), snapshot.FailingSet...)
	sort.Strings(snapshot.FailingSet)
	return json.Marshal(snapshot)
}

func ratchetDigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digestEvidence(reports []domain.TestReport) string {
	ids := make([]string, 0, len(reports))
	for _, report := range reports {
		ids = append(ids, report.ID+"\x00"+report.SourceDigest)
	}
	sort.Strings(ids)
	return ratchetDigestBytes([]byte(strings.Join(ids, "\x00")))
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func cloneCoverage(value *domain.Coverage) *domain.Coverage {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func sameCoverage(left, right *domain.Coverage) bool {
	if left == nil || right == nil {
		return left == right
	}
	return optionalFloatKey(left.Pct) == optionalFloatKey(right.Pct) && optionalIntKey(left.LinesCovered) == optionalIntKey(right.LinesCovered) && optionalIntKey(left.LinesTotal) == optionalIntKey(right.LinesTotal)
}

func optionalFloatKey(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}

func optionalIntKey(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
