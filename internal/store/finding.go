package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aira/internal/domain"
)

var ErrWaiverReasonRequired = errors.New("E_WAIVER_REASON_REQUIRED: waived finding requires --reason")

type FindingRecord struct {
	Finding     domain.Finding `json:"finding"`
	Path        string         `json:"path,omitempty"`
	WorktreeID  string         `json:"worktree_id"`
	Digest      string         `json:"digest,omitempty"`
	Warnings    []string       `json:"-"`
	Unevaluated bool           `json:"unevaluated,omitempty"`
	Error       string         `json:"error,omitempty"`
}

type FindingSetInput struct {
	Key         string
	Disposition domain.Disposition
	Reason      string
	Actor       string
}

type scannedFinding struct {
	WorktreeID string
	Root       string
	Path       string
	Finding    domain.Finding
	Digest     string
}

func (s *Store) acquireFindingMutationLock() (*os.File, error) {
	return acquireLock(filepath.Join(s.commonDir, "aira", "locks", "finding-rebuild.lock"))
}

func (s *Store) markFindingMaterialised(ctx context.Context, intent Intent) error {
	finding, err := domain.ParseFinding(intent.Intended)
	if err != nil {
		return err
	}
	if filepath.Base(intent.Path) != finding.Key+".md" {
		return errors.New("E_FINDING_INVALID: finding path/frontmatter mismatch")
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE outbox SET materialised=1 WHERE project_id=? AND seq=? AND materialised=0 AND resolution IS NULL`, intent.ProjectID, intent.Seq); err != nil {
			return err
		}
		return upsertReviewFinding(ctx, conn, intent.ProjectID, intent.WorktreeID, intent.Path, finding, digestBytes(intent.Intended))
	})
}

func (s *Store) findingPath(key string) string {
	return filepath.Join(s.root, ".aira", "findings", key+".md")
}

func readRegularFinding(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("E_FINDING_INVALID: finding path is not a regular file")
	}
	return os.ReadFile(path)
}

func (s *Store) prepareFindingMutation(ctx context.Context, path, precondition string, intended []byte, verb string, finding domain.Finding) (Intent, error) {
	intent, err := s.preparePathMutationEventKind(ctx, path, precondition, intended, verb, finding.Key, IntentKindFindingFile)
	if err != nil {
		return Intent{}, err
	}
	intent.Finding = finding
	return intent, nil
}

func (s *Store) AddFinding(ctx context.Context, input domain.ReviewFindingInput) (domain.Finding, EventKey, error) {
	findingLock, err := s.acquireFindingMutationLock()
	if err != nil {
		return domain.Finding{}, EventKey{}, err
	}
	defer unlockFile(findingLock)
	finding, err := domain.NewReviewFinding(input)
	if err != nil {
		return domain.Finding{}, EventKey{}, err
	}
	path := s.findingPath(finding.Key)
	var oldDigest string
	if data, readErr := readRegularFinding(path); readErr == nil {
		old, parseErr := domain.ParseFinding(data)
		if parseErr != nil {
			return domain.Finding{}, EventKey{}, parseErr
		}
		if old.Key != finding.Key {
			return domain.Finding{}, EventKey{}, errors.New("E_FINDING_INVALID: finding identity changed at existing path")
		}
		oldDigest = digestBytes(data)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return domain.Finding{}, EventKey{}, readErr
	}
	data, err := domain.RenderFinding(finding)
	if err != nil {
		return domain.Finding{}, EventKey{}, err
	}
	intent, err := s.prepareFindingMutation(ctx, path, oldDigest, data, "finding.add", finding)
	if err != nil {
		return domain.Finding{}, EventKey{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return domain.Finding{}, EventKey{}, err
	}
	return finding, EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
}

func (s *Store) SetFinding(ctx context.Context, key string, disposition domain.Disposition, reason, actor string) (EventKey, error) {
	findingLock, err := s.acquireFindingMutationLock()
	if err != nil {
		return EventKey{}, err
	}
	defer unlockFile(findingLock)
	record, err := s.GetFinding(key)
	if err != nil {
		return EventKey{}, err
	}
	if record.Finding.Subtype != domain.FindingSubtypeReview {
		return EventKey{}, errors.New("E_FINDING_INVALID: reconciliation findings cannot be set")
	}
	if disposition == domain.DispositionWaived && strings.TrimSpace(reason) == "" {
		return EventKey{}, ErrWaiverReasonRequired
	}
	updated, err := domain.NewReviewFinding(domain.ReviewFindingInput{
		TicketID: record.Finding.TicketID, Category: record.Finding.Category, Severity: record.Finding.Severity,
		Verdict: record.Finding.Verdict, Source: record.Finding.Source, Message: record.Finding.Message,
		RequirementID: record.Finding.RequirementID, File: record.Finding.File, Line: record.Finding.Line,
		Disposition: disposition, WaiverReason: reason, WaiverActor: actor,
	})
	if err != nil {
		return EventKey{}, err
	}
	path := s.findingPath(updated.Key)
	oldData, err := readRegularFinding(path)
	if err != nil {
		return EventKey{}, err
	}
	newData, err := domain.RenderFinding(updated)
	if err != nil {
		return EventKey{}, err
	}
	intent, err := s.prepareFindingMutation(ctx, path, digestBytes(oldData), newData, "finding.set", updated)
	if err != nil {
		return EventKey{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return EventKey{}, err
	}
	return EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
}

func (s *Store) GetFinding(key string) (FindingRecord, error) {
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, `/\\`) || filepath.Base(key) != key {
		return FindingRecord{}, errors.New("E_SELECTOR_INVALID: finding selector is invalid")
	}
	if strings.HasPrefix(key, "f-") {
		path := s.findingPath(key)
		data, err := readRegularFinding(path)
		if errors.Is(err, os.ErrNotExist) {
			return FindingRecord{}, errors.New("E_NOT_FOUND: selector matched no findings")
		}
		if err != nil {
			return FindingRecord{}, err
		}
		finding, err := domain.ParseFinding(data)
		if err != nil {
			return FindingRecord{}, err
		}
		if finding.Key != key {
			return FindingRecord{}, errors.New("E_FINDING_INVALID: filename/frontmatter finding ID mismatch")
		}
		record := FindingRecord{Finding: finding, Path: repoPath(s.root, path), WorktreeID: s.worktreeID, Digest: digestBytes(data)}
		indexed, indexErr := s.indexedFinding(key, s.worktreeID)
		if errors.Is(indexErr, sql.ErrNoRows) || indexErr == nil && !sameFindingContent(indexed, finding) {
			record.Warnings = []string{"W_STALE_INDEX"}
		} else if indexErr != nil {
			return FindingRecord{}, indexErr
		}
		return record, nil
	}
	return s.dbFinding(key)
}

func (s *Store) indexedFinding(key, worktree string) (domain.Finding, error) {
	row := s.db.QueryRow(`SELECT subtype,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,finding_key,code,subject,details,canonical_file,message FROM findings WHERE project_id=? AND worktree_id=? AND finding_key=?`, s.projectID, worktree, key)
	return scanFindingRow(row)
}

func (s *Store) dbFinding(key string) (FindingRecord, error) {
	row := s.db.QueryRow(`SELECT worktree_id,subtype,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,finding_key,code,subject,details,canonical_file,message FROM findings WHERE project_id=? AND finding_key=? ORDER BY CASE WHEN subtype='reconciliation' THEN 0 ELSE 1 END, worktree_id LIMIT 1`, s.projectID, key)
	var worktree string
	finding, err := scanFindingRowWithWorktree(row, &worktree)
	if errors.Is(err, sql.ErrNoRows) {
		return FindingRecord{}, errors.New("E_NOT_FOUND: selector matched no findings")
	}
	if err != nil {
		return FindingRecord{}, err
	}
	if finding.Subtype == domain.FindingSubtypeReview {
		return FindingRecord{}, errors.New("E_NOT_FOUND: review finding has no authoritative file")
	}
	return FindingRecord{Finding: finding, WorktreeID: worktree}, nil
}

func (s *Store) ListFindings(query string) ([]FindingRecord, error) {
	query = strings.TrimSpace(query)
	if strings.HasPrefix(query, "f-") || strings.HasPrefix(query, "r-") {
		record, err := s.GetFinding(query)
		if errors.Is(err, sql.ErrNoRows) || ErrorCode(err) == "E_NOT_FOUND" {
			return []FindingRecord{}, nil
		}
		if err != nil {
			return nil, err
		}
		return []FindingRecord{record}, nil
	}
	terms := []queryTerm{}
	if query != "" {
		var err error
		terms, err = parseTermsWithValidator(query, validFindingField)
		if err != nil {
			return nil, fmt.Errorf("E_SELECTOR_INVALID: %w", err)
		}
	}
	subtype := domain.FindingSubtypeReview
	for _, term := range terms {
		if term.Field == "subtype" {
			subtype = domain.FindingSubtype(term.Value)
		}
	}
	if subtype != domain.FindingSubtypeAny && subtype != domain.FindingSubtypeReview && subtype != domain.FindingSubtypeReconciliation {
		return nil, errors.New("E_SELECTOR_INVALID: invalid finding subtype")
	}
	var result []FindingRecord
	if subtype == domain.FindingSubtypeReview || subtype == domain.FindingSubtypeAny {
		findings, err := s.scanFindingFiles(s.root, s.worktreeID)
		if err != nil {
			return nil, err
		}
		indexed := s.indexedFindings(s.worktreeID)
		for _, scanned := range findings.valid {
			record := FindingRecord{Finding: scanned.Finding, Path: repoPath(s.root, scanned.Path), WorktreeID: scanned.WorktreeID, Digest: scanned.Digest}
			if indexedFinding, ok := indexed[scanned.Finding.Key]; !ok || !sameFindingContent(indexedFinding, scanned.Finding) {
				record.Warnings = []string{"W_STALE_INDEX"}
			}
			if matchesFindingTerms(record, terms) {
				result = append(result, record)
			}
		}
		for _, invalid := range findings.invalid {
			if subtype == domain.FindingSubtypeReview || subtype == domain.FindingSubtypeAny {
				record := FindingRecord{Path: repoPath(s.root, invalid.Subject), WorktreeID: s.worktreeID, Unevaluated: true, Error: invalid.Message, Warnings: []string{"E_FINDING_INVALID"}}
				if matchesFindingTerms(record, terms) {
					result = append(result, record)
				}
			}
		}
	}
	if subtype == domain.FindingSubtypeReconciliation || subtype == domain.FindingSubtypeAny {
		rows, err := s.db.Query(`SELECT worktree_id,subtype,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,finding_key,code,subject,details,canonical_file,message FROM findings WHERE project_id=? AND subtype='reconciliation'`, s.projectID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var worktree string
			finding, scanErr := scanFindingRowWithWorktree(rows, &worktree)
			if scanErr != nil {
				_ = rows.Close()
				return nil, scanErr
			}
			record := FindingRecord{Finding: finding, WorktreeID: worktree}
			if matchesFindingTerms(record, terms) {
				result = append(result, record)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Finding.Key < result[j].Finding.Key })
	return result, nil
}

func (s *Store) indexedFindings(worktree string) map[string]domain.Finding {
	rows, err := s.db.Query(`SELECT subtype,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,finding_key,code,subject,details,canonical_file,message FROM findings WHERE project_id=? AND worktree_id=? AND subtype='review'`, s.projectID, worktree)
	if err != nil {
		return map[string]domain.Finding{}
	}
	defer rows.Close()
	result := map[string]domain.Finding{}
	for rows.Next() {
		if finding, scanErr := scanFindingRow(rows); scanErr == nil {
			result[finding.Key] = finding
		}
	}
	return result
}

func matchesFindingTerms(record FindingRecord, terms []queryTerm) bool {
	for _, term := range terms {
		var matched bool
		switch term.Field {
		case "subtype":
			matched = record.Unevaluated && term.Value == string(domain.FindingSubtypeReview) || term.Value == string(domain.FindingSubtypeAny) || string(record.Finding.Subtype) == term.Value
		case "ticket":
			matched = record.Finding.TicketID == term.Value
		case "category":
			matched = record.Finding.Category == term.Value
		case "source":
			matched = record.Finding.Source == term.Value
		case "verdict":
			matched = string(record.Finding.Verdict) == term.Value
		case "disposition":
			matched = string(record.Finding.Disposition) == term.Value
		case "severity":
			matched = string(record.Finding.Severity) == term.Value
		case "requirement":
			matched = record.Finding.RequirementID == term.Value
		case "text":
			matched = strings.Contains(strings.ToLower(record.Finding.Message), strings.ToLower(term.Value))
		}
		if !matched {
			return false
		}
	}
	return true
}

func validFindingField(field string) bool {
	switch field {
	case "subtype", "ticket", "category", "source", "verdict", "disposition", "severity", "requirement", "text":
		return true
	default:
		return false
	}
}

func validFindingDistributionField(field string) bool {
	switch field {
	case "subtype", "category", "source", "verdict", "disposition", "severity", "ticket":
		return true
	default:
		return false
	}
}

// ValidFindingDistributionField is the core-facing validation seam for
// finding distributions.
func ValidFindingDistributionField(field string) bool { return validFindingDistributionField(field) }

func findingDistributionValues(finding domain.Finding, by string) []string {
	value := ""
	switch by {
	case "subtype":
		value = string(finding.Subtype)
	case "category":
		value = finding.Category
	case "source":
		value = finding.Source
	case "verdict":
		value = string(finding.Verdict)
	case "disposition":
		value = string(finding.Disposition)
	case "severity":
		value = string(finding.Severity)
	case "ticket":
		value = finding.TicketID
	}
	if value == "" {
		value = "(none)"
	}
	return []string{value}
}

func sameFindingContent(left, right domain.Finding) bool {
	return left.Subtype == right.Subtype && left.Key == right.Key && left.TicketID == right.TicketID && left.Category == right.Category && left.Severity == right.Severity && left.Verdict == right.Verdict && left.Source == right.Source && left.Message == right.Message && left.RequirementID == right.RequirementID && left.File == right.File && left.Line == right.Line && left.Disposition == right.Disposition && left.WaiverReason == right.WaiverReason && left.WaiverActor == right.WaiverActor
}

type findingScanResult struct {
	valid   []scannedFinding
	invalid []CheckFinding
}

func (s *Store) scanFindingFiles(root, worktree string) (findingScanResult, error) {
	return scanFindingFiles(root, worktree)
}

func scanFindingFiles(root, worktree string) (findingScanResult, error) {
	dir := filepath.Join(root, ".aira", "findings")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return findingScanResult{}, nil
	}
	if err != nil {
		return findingScanResult{}, err
	}
	result := findingScanResult{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, readErr := readRegularFinding(path)
		if readErr != nil {
			result.invalid = append(result.invalid, CheckFinding{Code: "E_FINDING_INVALID", Subject: repoPath(root, path), Message: readErr.Error(), Kind: "unevaluated"})
			continue
		}
		finding, parseErr := domain.ParseFinding(data)
		if parseErr != nil || finding.Key+".md" != entry.Name() {
			message := "E_FINDING_INVALID: filename/frontmatter mismatch"
			if parseErr != nil {
				message = parseErr.Error()
			}
			result.invalid = append(result.invalid, CheckFinding{Code: "E_FINDING_INVALID", Subject: repoPath(root, path), Message: message, Kind: "unevaluated"})
			continue
		}
		result.valid = append(result.valid, scannedFinding{WorktreeID: worktree, Root: root, Path: path, Finding: finding, Digest: digestBytes(data)})
	}
	sort.Slice(result.valid, func(i, j int) bool { return result.valid[i].Finding.Key < result.valid[j].Finding.Key })
	sort.Slice(result.invalid, func(i, j int) bool { return result.invalid[i].Subject < result.invalid[j].Subject })
	return result, nil
}

func scanFindingRow(row interface{ Scan(...any) error }) (domain.Finding, error) {
	return scanFindingRowWithWorktree(row, nil)
}

func scanFindingRowWithWorktree(row interface{ Scan(...any) error }, worktree *string) (domain.Finding, error) {
	var subtype, ticket, category, severity, verdict, disposition, source, file, requirement, reason, actor, key, code, subject, details, canonicalFile, message string
	var line int
	args := []any{}
	if worktree != nil {
		args = append(args, worktree)
	}
	args = append(args, &subtype, &ticket, &category, &severity, &verdict, &disposition, &source, &file, &line, &requirement, &reason, &actor, &key, &code, &subject, &details, &canonicalFile, &message)
	if err := row.Scan(args...); err != nil {
		return domain.Finding{}, err
	}
	if subtype == string(domain.FindingSubtypeReview) {
		if code != "" || subject != "" || details != "" {
			return domain.Finding{}, errors.New("E_FINDING_INVALID: review index row contains reconciliation fields")
		}
		finding, err := domain.NewReviewFinding(domain.ReviewFindingInput{TicketID: ticket, Category: category, Severity: domain.Severity(severity), Verdict: domain.Verdict(verdict), Source: source, Message: message, RequirementID: requirement, File: file, Line: line, Disposition: domain.Disposition(disposition), WaiverReason: reason, WaiverActor: actor})
		if err != nil {
			return domain.Finding{}, err
		}
		if finding.Key != key {
			return domain.Finding{}, errors.New("E_FINDING_INVALID: review index key does not match identity")
		}
		return finding, nil
	}
	if subtype == string(domain.FindingSubtypeReconciliation) {
		finding, err := domain.NewReconciliationFinding(domain.ReconciliationFindingInput{Code: code, Subject: subject, Details: details, TicketID: ticket})
		if err != nil {
			return domain.Finding{}, err
		}
		finding.Key = key
		if finding.Disposition != domain.DispositionOpen || message != details || category != "" || source != "" || verdict != "" || severity != "" || file != "" || line != 0 || requirement != "" || reason != "" || actor != "" || canonicalFile != "" {
			return domain.Finding{}, errors.New("E_FINDING_INVALID: reconciliation index row contains review fields")
		}
		return finding, nil
	}
	return domain.Finding{}, errors.New("E_FINDING_INVALID: unknown finding subtype")
}

func upsertReviewFinding(ctx context.Context, conn *sql.Conn, project, worktree, path string, finding domain.Finding, digest string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO findings(project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,worktree_id,finding_key) DO UPDATE SET subtype=excluded.subtype, ticket_id=excluded.ticket_id, category=excluded.category, severity=excluded.severity, verdict=excluded.verdict, disposition=excluded.disposition, source=excluded.source, file=excluded.file, line=excluded.line, requirement_id=excluded.requirement_id, waiver_reason=excluded.waiver_reason, waiver_actor=excluded.waiver_actor, canonical_file=excluded.canonical_file, message=excluded.message, details=excluded.details`,
		project, worktree, finding.Key, finding.Subtype, "", "", "", timeNow(), finding.TicketID, finding.Category, finding.Severity, finding.Verdict, finding.Disposition, finding.Source, finding.File, finding.Line, finding.RequirementID, finding.WaiverReason, finding.WaiverActor, finding.File, finding.Message)
	return err
}

func upsertReconciliationFinding(ctx context.Context, conn *sql.Conn, project, worktree, key, code, subject, details string) error {
	updated, err := conn.ExecContext(ctx, `UPDATE findings SET worktree_id=?, code=?, subject=?, details=?, message=?, subtype='reconciliation', disposition='open', ticket_id='', category='', severity='', verdict='', source='', file='', line=0, requirement_id='', waiver_reason='', waiver_actor='', canonical_file='' WHERE project_id=? AND finding_key=? AND subtype='reconciliation'`, worktree, code, subject, details, details, project, key)
	if err != nil {
		return err
	}
	if count, countErr := updated.RowsAffected(); countErr == nil && count > 0 {
		return nil
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO findings(project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,worktree_id,finding_key) DO UPDATE SET code=excluded.code,subject=excluded.subject,details=excluded.details,message=excluded.message`,
		project, worktree, key, domain.FindingSubtypeReconciliation, code, subject, details, timeNow(), "", "", "", "", domain.DispositionOpen, "", "", 0, "", "", "", "", details)
	return err
}

func timeNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) findingIndexDivergence() ([]CheckFinding, error) {
	registry, err := readRegistry(s.registryPath)
	if err != nil {
		return nil, err
	}
	entries, err := discoverWorktrees(s.root, s.projectID, registry)
	if err != nil {
		return nil, err
	}
	expected := map[string]domain.Finding{}
	var findings []CheckFinding
	for _, entry := range entries {
		result, scanErr := scanFindingFiles(entry.Root, entry.WorktreeID)
		if scanErr != nil {
			return nil, scanErr
		}
		for _, invalid := range result.invalid {
			invalid.Subject = filepath.ToSlash(filepath.Join(repoPath(s.root, entry.Root), invalid.Subject))
			findings = append(findings, invalid)
		}
		for _, scanned := range result.valid {
			expected[entry.WorktreeID+"\x00"+scanned.Finding.Key] = scanned.Finding
		}
	}
	actual := map[string]domain.Finding{}
	rows, err := s.db.Query(`SELECT worktree_id,subtype,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,finding_key,code,subject,details,canonical_file,message FROM findings WHERE project_id=? AND subtype='review'`, s.projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var worktree string
		finding, scanErr := scanFindingRowWithWorktree(rows, &worktree)
		if scanErr != nil {
			findings = append(findings, CheckFinding{Code: "E_FINDING_INDEX_DIVERGENCE", Subject: "finding-index", Message: scanErr.Error(), Kind: "fail"})
			continue
		}
		actual[worktree+"\x00"+finding.Key] = finding
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for key, finding := range expected {
		if indexed, ok := actual[key]; !ok {
			findings = append(findings, CheckFinding{Code: "E_FINDING_INDEX_DIVERGENCE", Subject: finding.Key, Message: "canonical finding is missing from the derived index", Kind: "fail"})
		} else if !sameFindingContent(indexed, finding) {
			findings = append(findings, CheckFinding{Code: "E_FINDING_INDEX_DIVERGENCE", Subject: finding.Key, Message: "derived finding index differs from canonical file", Kind: "fail"})
		}
	}
	for key, finding := range actual {
		if _, ok := expected[key]; !ok {
			findings = append(findings, CheckFinding{Code: "E_FINDING_INDEX_DIVERGENCE", Subject: finding.Key, Message: "derived finding index row is absent from canonical files", Kind: "fail"})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Subject != findings[j].Subject {
			return findings[i].Subject < findings[j].Subject
		}
		return findings[i].Message < findings[j].Message
	})
	return findings, nil
}
