package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"aira/internal/domain"
)

type TestReportAddResult struct {
	Report       domain.TestReport `json:"report"`
	ID           string            `json:"id"`
	EvictedCount int               `json:"evicted_count"`
	Remaining    int               `json:"remaining"`
	Idempotent   bool              `json:"idempotent,omitempty"`
}

type TestReportContext struct {
	Commit     string
	Branch     string
	WorktreeID string
}

// TestReportContext returns the same git/worktree identity AddTestReport uses
// for omitted provenance, allowing run wiring to construct a complete input.
func (s *Store) TestReportContext(ctx context.Context) TestReportContext {
	return TestReportContext{Commit: s.gitValue(ctx, "HEAD"), Branch: s.gitValue(ctx, "--abbrev-ref", "HEAD"), WorktreeID: s.worktreeID}
}

// FlakyCellSummary is the uncapped cell-state aggregate behind both the
// flaky face and the flaky-rate gauge. Denominator is Flaky+Clean; cells with
// insufficient evidence remain visible in Unevaluated but are excluded.
type FlakyCellSummary struct {
	Total       int   `json:"total_cells"`
	Flaky       int   `json:"flaky_cells"`
	Clean       int   `json:"clean_cells"`
	Unevaluated int   `json:"unevaluated_cells"`
	Denominator int   `json:"denominator"`
	AtSeq       int64 `json:"at_seq"`
}

// FlakyCells reads report headers and results in one SQLite transaction and
// then reuses computeFlakyTests, the same cell evaluator as FlakyTests.
func (s *Store) FlakyCellSummary(ctx context.Context) (FlakyCellSummary, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FlakyCellSummary{}, err
	}
	defer tx.Rollback()
	reports, err := s.loadAllTestReports(ctx, tx)
	if err != nil {
		return FlakyCellSummary{}, err
	}
	var summary FlakyCellSummary
	for _, report := range reports {
		if report.AtSeq > summary.AtSeq {
			summary.AtSeq = report.AtSeq
		}
	}
	for _, test := range computeFlakyTests(reports) {
		for _, cell := range test.Cells {
			summary.Total++
			switch cell.State {
			case domain.FlakyStateFlaky:
				summary.Flaky++
			case domain.FlakyStateClean:
				summary.Clean++
			case domain.FlakyStateUnevaluated:
				summary.Unevaluated++
			}
		}
	}
	summary.Denominator = summary.Flaky + summary.Clean
	if err := tx.Commit(); err != nil {
		return FlakyCellSummary{}, err
	}
	return summary, nil
}

// FlakyCellStateSummary is a concise alias for the --all read API.
func (s *Store) FlakyCellStateSummary(ctx context.Context) (FlakyCellSummary, error) {
	return s.FlakyCellSummary(ctx)
}

func (s *Store) AddTestReport(ctx context.Context, rawInput domain.TestReportInput) (TestReportAddResult, error) {
	input := rawInput.Normalized()
	if input.Shard == "" {
		input.Shard = "1/1"
	}
	if input.Raw != nil {
		parsed, err := parseTestReport(input.Format, input.Raw)
		if err != nil {
			return TestReportAddResult{}, err
		}
		input.Results = parsed.Results
		input.ParserComplete = parsed.Complete
	}
	if input.ForceParserIncomplete {
		input.ParserComplete = false
	}
	if input.SourceDigest == "" {
		payload := input.Raw
		if payload == nil {
			payload, _ = json.Marshal(struct {
				Format                                    string
				Commit, SuiteID, Config, EnvDigest, Shard string
				RetryIndex                                int
				Results                                   []domain.TestResult
			}{input.Format, input.Commit, input.SuiteID, input.Config, input.EnvDigest, input.Shard, input.RetryIndex, input.Results})
		}
		if input.ForceParserIncomplete {
			payload = append([]byte("aira:test-report:forced-incomplete\x00"), payload...)
		}
		digest := sha256.Sum256(payload)
		input.SourceDigest = hex.EncodeToString(digest[:])
	}
	if input.At == "" {
		input.At = timeNow()
	}
	identity := TestReportContext{}
	if input.Commit == "" || input.Branch == "" || input.WorktreeID == "" {
		identity = s.TestReportContext(ctx)
	}
	if input.Commit == "" {
		input.Commit = identity.Commit
	}
	if input.Branch == "" {
		input.Branch = identity.Branch
	}
	if input.WorktreeID == "" {
		input.WorktreeID = identity.WorktreeID
	}
	if err := input.Validate(); err != nil {
		return TestReportAddResult{}, err
	}
	seen := map[string]struct{}{}
	for _, result := range input.Results {
		if _, exists := seen[result.Name]; exists {
			return TestReportAddResult{}, errors.New("E_TESTREPORT_INVALID: report contains duplicate test names")
		}
		seen[result.Name] = struct{}{}
	}
	var result TestReportAddResult
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		if existing, found, err := s.findIdempotentReport(ctx, conn, input); err != nil {
			return err
		} else if found {
			result = TestReportAddResult{Report: existing, ID: existing.ID, Remaining: 0, Idempotent: true}
			return conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM test_reports WHERE project_id=?`, s.projectID).Scan(&result.Remaining)
		}
		number, sequence, err := nextTestReportNumbers(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		report := domain.TestReport{
			ID: fmt.Sprintf("TR-%d", number), TicketID: input.TicketID, Phase: input.Phase,
			Commit: input.Commit, Branch: input.Branch, WorktreeID: input.WorktreeID,
			Agent: input.Agent, Session: input.Session, At: input.At, RunRef: input.RunRef,
			SuiteID: input.SuiteID, Runner: input.Runner, Config: input.Config, EnvDigest: input.EnvDigest,
			Shard: input.Shard, RetryIndex: input.RetryIndex, ParserComplete: input.ParserComplete,
			Coverage: input.Coverage, Format: input.Format, SourceDigest: input.SourceDigest, AtSeq: sequence,
			Results: append([]domain.TestResult(nil), input.Results...),
		}
		var pct any
		var linesCovered any
		var linesTotal any
		if input.Coverage != nil {
			if input.Coverage.Pct != nil {
				pct = *input.Coverage.Pct
			}
			if input.Coverage.LinesCovered != nil {
				linesCovered = *input.Coverage.LinesCovered
			}
			if input.Coverage.LinesTotal != nil {
				linesTotal = *input.Coverage.LinesTotal
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO test_reports(project_id,id,ticket_id,phase,"commit",branch,worktree_id,agent,session,at,run_ref,suite_id,runner,config,env_digest,shard,retry_index,parser_complete,coverage_pct,lines_covered,lines_total,format,source_digest,at_seq,pinned)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			s.projectID, report.ID, report.TicketID, report.Phase, report.Commit, report.Branch, report.WorktreeID,
			report.Agent, report.Session, report.At, report.RunRef, report.SuiteID, report.Runner, report.Config,
			report.EnvDigest, report.Shard, report.RetryIndex, boolInt(report.ParserComplete), pct, linesCovered, linesTotal,
			report.Format, report.SourceDigest, report.AtSeq); err != nil {
			return err
		}
		for index, test := range report.Results {
			if s.testReportInsertHook != nil {
				if err := s.testReportInsertHook(index); err != nil {
					return err
				}
			}
			var duration any
			if test.DurationNS != nil {
				duration = *test.DurationNS
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO test_report_results(project_id,report_id,name,outcome,duration_ns,message) VALUES(?,?,?,?,?,?)`,
				s.projectID, report.ID, test.Name, test.Outcome, duration, test.Message); err != nil {
				return err
			}
		}
		evicted, err := s.evictTestReports(ctx, conn)
		if err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM test_reports WHERE project_id=?`, s.projectID).Scan(&result.Remaining); err != nil {
			return err
		}
		if err := s.reconcileFlakyConn(ctx, conn); err != nil {
			return err
		}
		result = TestReportAddResult{Report: report, ID: report.ID, EvictedCount: evicted, Remaining: result.Remaining}
		return nil
	})
	return result, err
}

func (s *Store) gitValue(ctx context.Context, args ...string) string {
	commandArgs := append([]string{"-C", s.root, "rev-parse"}, args...)
	output, err := exec.CommandContext(ctx, "git", commandArgs...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func nextTestReportNumbers(ctx context.Context, conn *sql.Conn, project string) (int64, int64, error) {
	var number, sequence int64
	err := conn.QueryRowContext(ctx, `SELECT next_number,next_seq FROM test_report_counter WHERE project_id=?`, project).Scan(&number, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO test_report_counter(project_id,next_number,next_seq) VALUES(?,?,?)`, project, 2, 2); err != nil {
			return 0, 0, err
		}
		return 1, 1, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if number < 1 || sequence < 1 {
		return 0, 0, errors.New("E_TESTREPORT_INVALID: report counter is invalid")
	}
	if _, err := conn.ExecContext(ctx, `UPDATE test_report_counter SET next_number=?,next_seq=? WHERE project_id=?`, number+1, sequence+1, project); err != nil {
		return 0, 0, err
	}
	return number, sequence, nil
}

func (s *Store) findIdempotentReport(ctx context.Context, conn *sql.Conn, input domain.TestReportInput) (domain.TestReport, bool, error) {
	row := conn.QueryRowContext(ctx, `SELECT id,ticket_id,phase,"commit",branch,worktree_id,agent,session,at,run_ref,suite_id,runner,config,env_digest,shard,retry_index,parser_complete,coverage_pct,lines_covered,lines_total,format,source_digest,at_seq,pinned FROM test_reports WHERE project_id=? AND source_digest=? AND format=? AND "commit"=? AND suite_id=? AND config=? AND env_digest=? AND shard=? AND retry_index=?`,
		s.projectID, input.SourceDigest, input.Format, input.Commit, input.SuiteID, input.Config, input.EnvDigest, input.Shard, input.RetryIndex)
	report, err := scanTestReportHeader(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TestReport{}, false, nil
	}
	if err != nil {
		return domain.TestReport{}, false, err
	}
	results, err := s.loadTestResults(ctx, conn, report.ID)
	if err != nil {
		return domain.TestReport{}, false, err
	}
	report.Results = results
	return report, true, nil
}

func (s *Store) evictTestReports(ctx context.Context, conn *sql.Conn) (int, error) {
	evicted := 0
	result, err := conn.ExecContext(ctx, `DELETE FROM test_reports WHERE project_id=? AND pinned=0 AND at_seq NOT IN (SELECT at_seq FROM test_reports WHERE project_id=? AND pinned=0 ORDER BY at_seq DESC LIMIT ?)`, s.projectID, s.projectID, s.maxReports)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	evicted += int(count)
	if s.maxAgeDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.maxAgeDays).Format(time.RFC3339Nano)
		result, err = conn.ExecContext(ctx, `DELETE FROM test_reports WHERE project_id=? AND pinned=0 AND at < ?`, s.projectID, cutoff)
		if err != nil {
			return 0, err
		}
		count, err = result.RowsAffected()
		if err != nil {
			return 0, err
		}
		evicted += int(count)
	}
	return evicted, nil
}

func (s *Store) ListTestReports(_ string) ([]domain.TestReport, error) {
	rows, err := s.db.Query(`SELECT id,ticket_id,phase,"commit",branch,worktree_id,agent,session,at,run_ref,suite_id,runner,config,env_digest,shard,retry_index,parser_complete,coverage_pct,lines_covered,lines_total,format,source_digest,at_seq,pinned FROM test_reports WHERE project_id=? ORDER BY at_seq DESC`, s.projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []domain.TestReport
	for rows.Next() {
		report, err := scanTestReportHeader(rows)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range reports {
		results, err := s.loadTestResults(context.Background(), s.db, reports[i].ID)
		if err != nil {
			return nil, err
		}
		reports[i].Results = results
	}
	return reports, nil
}

func (s *Store) GetTestReport(id string) (domain.TestReport, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.TestReport{}, errors.New("E_SELECTOR_INVALID: test report selector is empty")
	}
	row := s.db.QueryRow(`SELECT id,ticket_id,phase,"commit",branch,worktree_id,agent,session,at,run_ref,suite_id,runner,config,env_digest,shard,retry_index,parser_complete,coverage_pct,lines_covered,lines_total,format,source_digest,at_seq,pinned FROM test_reports WHERE project_id=? AND id=?`, s.projectID, id)
	report, err := scanTestReportHeader(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TestReport{}, errors.New("E_NOT_FOUND: test report not found")
	}
	if err != nil {
		return domain.TestReport{}, err
	}
	report.Results, err = s.loadTestResults(context.Background(), s.db, id)
	return report, err
}

type rowScanner interface{ Scan(...any) error }

func scanTestReportHeader(row rowScanner) (domain.TestReport, error) {
	var report domain.TestReport
	var complete, pinned int
	var pct sql.NullFloat64
	var covered, total sql.NullInt64
	err := row.Scan(&report.ID, &report.TicketID, &report.Phase, &report.Commit, &report.Branch, &report.WorktreeID, &report.Agent, &report.Session, &report.At, &report.RunRef, &report.SuiteID, &report.Runner, &report.Config, &report.EnvDigest, &report.Shard, &report.RetryIndex, &complete, &pct, &covered, &total, &report.Format, &report.SourceDigest, &report.AtSeq, &pinned)
	if err != nil {
		return domain.TestReport{}, err
	}
	report.ParserComplete, report.Pinned = complete != 0, pinned != 0
	if pct.Valid || covered.Valid || total.Valid {
		report.Coverage = &domain.Coverage{}
		if pct.Valid {
			report.Coverage.Pct = &pct.Float64
		}
		if covered.Valid {
			report.Coverage.LinesCovered = &covered.Int64
		}
		if total.Valid {
			report.Coverage.LinesTotal = &total.Int64
		}
	}
	return report, nil
}

func (s *Store) loadTestResults(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, reportID string) ([]domain.TestResult, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT name,outcome,duration_ns,message FROM test_report_results WHERE project_id=? AND report_id=? ORDER BY name`, s.projectID, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []domain.TestResult
	for rows.Next() {
		var result domain.TestResult
		var duration sql.NullInt64
		if err := rows.Scan(&result.Name, &result.Outcome, &duration, &result.Message); err != nil {
			return nil, err
		}
		if duration.Valid {
			result.DurationNS = &duration.Int64
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func sortReports(reports []domain.TestReport) {
	sort.Slice(reports, func(i, j int) bool { return reports[i].AtSeq > reports[j].AtSeq })
}

type flakyCellKey struct {
	commit, suite, config, env, shard string
}

type flakyCellAccumulator struct {
	key      flakyCellKey
	valid    bool
	reason   string
	evidence []string
	passes   []string
	failures []string
}

func (s *Store) FlakyTests(selector string) ([]domain.FlakyTest, error) {
	reports, err := s.loadAllTestReports(context.Background(), s.db)
	if err != nil {
		return nil, err
	}
	tests := computeFlakyTests(reports)
	if selector != "" {
		for _, test := range tests {
			if test.Name == selector {
				return []domain.FlakyTest{test}, nil
			}
		}
		return []domain.FlakyTest{{Name: selector, State: domain.FlakyStateUnevaluated, Reason: "no evidence for test"}}, nil
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
	return tests, nil
}

func computeFlakyTests(reports []domain.TestReport) []domain.FlakyTest {
	cells := map[string]map[flakyCellKey]*flakyCellAccumulator{}
	for _, report := range reports {
		for _, result := range report.Results {
			byCell := cells[result.Name]
			if byCell == nil {
				byCell = map[flakyCellKey]*flakyCellAccumulator{}
				cells[result.Name] = byCell
			}
			key := flakyCellKey{report.Commit, report.SuiteID, report.Config, report.EnvDigest, report.Shard}
			cell := byCell[key]
			if cell == nil {
				cell = &flakyCellAccumulator{key: key, valid: report.Commit != "" && report.SuiteID != "" && report.Config != "" && report.EnvDigest != "" && report.Shard != ""}
				if !cell.valid {
					cell.reason = "missing commit, suite_id, config, env_digest, or shard"
				}
				byCell[key] = cell
			}
			if !cell.valid {
				continue
			}
			if !report.ParserComplete {
				cell.reason = "parser-incomplete report excluded"
				continue
			}
			if report.RetryIndex != 0 {
				cell.reason = "retry result excluded from first-pass evidence"
				continue
			}
			if result.Outcome == domain.OutcomeSkip {
				cell.reason = "skipped result does not count as evidence"
				continue
			}
			cell.evidence = append(cell.evidence, report.ID)
			switch result.Outcome {
			case domain.OutcomePass:
				cell.passes = append(cell.passes, report.ID)
			case domain.OutcomeFail, domain.OutcomeError:
				cell.failures = append(cell.failures, report.ID)
			}
		}
	}
	result := make([]domain.FlakyTest, 0, len(cells))
	for name, byCell := range cells {
		test := domain.FlakyTest{Name: name, State: domain.FlakyStateClean}
		for _, cell := range byCell {
			state := domain.FlakyStateUnevaluated
			reason := cell.reason
			if !cell.valid {
				state = domain.FlakyStateUnevaluated
			} else if len(cell.evidence) < 2 {
				state = domain.FlakyStateUnevaluated
				if reason == "" {
					reason = fmt.Sprintf("only %d comparable first-pass result(s)", len(cell.evidence))
				}
			} else if len(cell.passes) > 0 && len(cell.failures) > 0 {
				state = domain.FlakyStateFlaky
				reason = "same identity cell has both pass and fail/error first-pass witnesses"
			} else {
				state = domain.FlakyStateClean
				reason = "at least two comparable first-pass results agree"
			}
			flakyCell := domain.FlakyCell{Commit: cell.key.commit, SuiteID: cell.key.suite, Config: cell.key.config, EnvDigest: cell.key.env, Shard: cell.key.shard, State: state, Reason: reason, Evidence: len(cell.evidence), Passes: append([]string(nil), cell.passes...), Failures: append([]string(nil), cell.failures...)}
			test.Cells = append(test.Cells, flakyCell)
			switch {
			case state == domain.FlakyStateFlaky:
				test.State = domain.FlakyStateFlaky
			case state == domain.FlakyStateUnevaluated && test.State != domain.FlakyStateFlaky:
				test.State = domain.FlakyStateUnevaluated
			}
		}
		if test.State == domain.FlakyStateFlaky {
			test.Reason = "one or more identity cells disagree"
		} else if test.State == domain.FlakyStateUnevaluated {
			test.Reason = "one or more identity cells lack two comparable first-pass results"
		} else {
			test.Reason = "all identity cells have at least two agreeing first-pass results"
		}
		sort.Slice(test.Cells, func(i, j int) bool {
			return test.Cells[i].Commit+"\x00"+test.Cells[i].SuiteID+"\x00"+test.Cells[i].Config+"\x00"+test.Cells[i].EnvDigest+"\x00"+test.Cells[i].Shard < test.Cells[j].Commit+"\x00"+test.Cells[j].SuiteID+"\x00"+test.Cells[j].Config+"\x00"+test.Cells[j].EnvDigest+"\x00"+test.Cells[j].Shard
		})
		result = append(result, test)
	}
	return result
}

func (s *Store) ReconcileFlaky(ctx context.Context) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error { return s.reconcileFlakyConn(ctx, conn) })
}

func (s *Store) reconcileFlakyConn(ctx context.Context, conn *sql.Conn) error {
	reports, err := s.loadAllTestReports(ctx, conn)
	if err != nil {
		return err
	}
	tests := computeFlakyTests(reports)
	if _, err := conn.ExecContext(ctx, `DELETE FROM findings WHERE project_id=? AND subtype='reconciliation' AND code=?`, s.projectID, "E_TESTREPORT_FLAKY"); err != nil {
		return err
	}
	for _, test := range tests {
		if test.State != domain.FlakyStateFlaky {
			continue
		}
		for _, cell := range test.Cells {
			if cell.State != domain.FlakyStateFlaky {
				continue
			}
			subject := flakySubject(test.Name, cell)
			details := fmt.Sprintf("test=%s cell=[commit=%s suite=%s config=%s env=%s shard=%s] pass:%s fail:%s", test.Name, cell.Commit, cell.SuiteID, cell.Config, cell.EnvDigest, cell.Shard, strings.Join(cell.Passes, ","), strings.Join(cell.Failures, ","))
			finding, err := domain.NewReconciliationFinding(domain.ReconciliationFindingInput{Code: "E_TESTREPORT_FLAKY", Subject: subject, Details: details})
			if err != nil {
				return err
			}
			if err := upsertReconciliationFinding(ctx, conn, s.projectID, s.worktreeID, finding.Key, finding.Code, finding.Subject, finding.Details); err != nil {
				return err
			}
		}
	}
	return nil
}

func flakySubject(name string, cell domain.FlakyCell) string {
	hash := sha256.New()
	for _, value := range []string{name, cell.Commit, cell.SuiteID, cell.Config, cell.EnvDigest, cell.Shard} {
		var prefix [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(prefix[:], uint64(len(value)))
		_, _ = hash.Write(prefix[:n])
		_, _ = hash.Write([]byte(value))
	}
	return "flaky:" + hex.EncodeToString(hash.Sum(nil))
}

func (s *Store) loadAllTestReports(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]domain.TestReport, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT id,ticket_id,phase,"commit",branch,worktree_id,agent,session,at,run_ref,suite_id,runner,config,env_digest,shard,retry_index,parser_complete,coverage_pct,lines_covered,lines_total,format,source_digest,at_seq,pinned FROM test_reports WHERE project_id=? ORDER BY at_seq`, s.projectID)
	if err != nil {
		return nil, err
	}
	var reports []domain.TestReport
	for rows.Next() {
		report, err := scanTestReportHeader(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range reports {
		reports[i].Results, err = s.loadTestResults(ctx, queryer, reports[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return reports, nil
}
