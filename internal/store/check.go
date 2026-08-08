package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type CheckFinding struct {
	Code    string `json:"code"`
	Subject string `json:"subject,omitempty"`
	Message string `json:"message,omitempty"`
	Kind    string `json:"kind"`
}

type CheckReport struct {
	Verdict     string         `json:"verdict"`
	Findings    []CheckFinding `json:"findings,omitempty"`
	Warnings    []CheckFinding `json:"warnings,omitempty"`
	Unevaluated bool           `json:"unevaluated,omitempty"`
}

// Check runs the explicit full consistency pass. Known integrity findings are
// returned as a fail verdict; unexpected inability to access the store is
// returned as an error so the adapter can use exit 4 rather than claiming a
// verdict it did not establish.
func (s *Store) Check(ctx context.Context) (CheckReport, error) {
	report := CheckReport{Verdict: "pass"}
	if err := s.reconcile(ctx); err != nil {
		if isIntegrityError(err) {
			report.Findings = append(report.Findings, findingFromError(err, "reconcile"))
		} else {
			return CheckReport{}, fmt.Errorf("E_RECONCILE_REQUIRED: %w", err)
		}
	}
	if err := s.Rebuild(ctx); err != nil {
		if isIntegrityError(err) {
			report.Findings = append(report.Findings, findingFromError(err, "rebuild"))
		} else {
			return CheckReport{}, err
		}
	}

	rows, err := s.db.Query(`SELECT prefix, number, path, state FROM allocations WHERE project_id=?`, s.projectID)
	if err != nil {
		return CheckReport{}, err
	}
	for rows.Next() {
		var prefix string
		var number int64
		var path, state string
		if err := rows.Scan(&prefix, &number, &path, &state); err != nil {
			_ = rows.Close()
			return CheckReport{}, err
		}
		if state == "allocated" {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				report.Findings = append(report.Findings, CheckFinding{
					Code: "E_ID_UNRESOLVED", Subject: fmt.Sprintf("%s-%d", prefix, number),
					Message: "allocation has no materialised ticket file", Kind: "fail",
				})
			} else if err != nil {
				_ = rows.Close()
				return CheckReport{}, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return CheckReport{}, err
	}
	_ = rows.Close()

	// A rebuild updates rows it can still see but intentionally leaves stale rows
	// as evidence. Surface those rows with the stable warning rather than letting
	// a read silently pretend the derived index is authoritative.
	indexed, err := s.db.Query(`SELECT id, path, digest FROM tickets WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID)
	if err != nil {
		return CheckReport{}, err
	}
	for indexed.Next() {
		var id, path, digest string
		if err := indexed.Scan(&id, &path, &digest); err != nil {
			_ = indexed.Close()
			return CheckReport{}, err
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			report.Warnings = append(report.Warnings, CheckFinding{Code: "W_STALE_INDEX", Subject: id, Message: "indexed ticket file is missing", Kind: "warning"})
			continue
		}
		if err != nil {
			_ = indexed.Close()
			return CheckReport{}, err
		}
		if digestBytes(data) != digest {
			report.Warnings = append(report.Warnings, CheckFinding{Code: "W_STALE_INDEX", Subject: id, Message: "indexed digest differs from ticket file", Kind: "warning"})
		}
	}
	if err := indexed.Err(); err != nil {
		_ = indexed.Close()
		return CheckReport{}, err
	}
	_ = indexed.Close()

	worktrees, err := s.db.Query(`SELECT worktree_id, root FROM worktrees WHERE project_id=?`, s.projectID)
	if err != nil {
		return CheckReport{}, err
	}
	for worktrees.Next() {
		var id, root string
		if err := worktrees.Scan(&id, &root); err != nil {
			_ = worktrees.Close()
			return CheckReport{}, err
		}
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			warning := CheckFinding{Code: "W_ORPHAN_WORKTREE", Subject: id, Message: root, Kind: "warning"}
			report.Warnings = append(report.Warnings, warning)
			report.Unevaluated = true
		} else if err != nil {
			_ = worktrees.Close()
			return CheckReport{}, err
		}
	}
	if err := worktrees.Err(); err != nil {
		_ = worktrees.Close()
		return CheckReport{}, err
	}
	_ = worktrees.Close()

	findings, err := s.db.Query(`SELECT code, subject, details FROM findings WHERE project_id=? ORDER BY finding_key`, s.projectID)
	if err != nil {
		return CheckReport{}, err
	}
	for findings.Next() {
		var code, subject, details string
		if err := findings.Scan(&code, &subject, &details); err != nil {
			_ = findings.Close()
			return CheckReport{}, err
		}
		if code == "E_GIT_SCAN" {
			report.Unevaluated = true
			report.Warnings = append(report.Warnings, CheckFinding{Code: "W_ORPHAN_WORKTREE", Subject: subject, Message: details, Kind: "warning"})
			continue
		}
		if strings.HasPrefix(code, "W_") {
			report.Warnings = append(report.Warnings, CheckFinding{Code: code, Subject: subject, Message: details, Kind: "warning"})
		} else {
			report.Findings = append(report.Findings, CheckFinding{Code: code, Subject: subject, Message: details, Kind: "fail"})
		}
	}
	if err := findings.Err(); err != nil {
		_ = findings.Close()
		return CheckReport{}, err
	}
	_ = findings.Close()

	if len(report.Findings) > 0 {
		report.Verdict = "fail"
	} else if report.Unevaluated {
		report.Verdict = "unevaluated"
	}
	return report, nil
}

func isIntegrityError(err error) bool {
	code := ErrorCode(err)
	switch code {
	case "E_DUPLICATE_ID", "E_ID_UNRESOLVED", "E_RELATION_TARGET_MISSING", "E_RELATION_INVALID", "E_WRITE_CONFLICT", "E_TRANSITION_INVALID", "E_PATH_INTENT_UNRESOLVED", "E_JOURNAL_CORRUPT", "E_SELECTOR_AMBIGUOUS":
		return true
	default:
		return false
	}
}

func findingFromError(err error, subject string) CheckFinding {
	return CheckFinding{Code: ErrorCode(err), Subject: subject, Message: err.Error(), Kind: "fail"}
}

// ErrorCode extracts the stable catalog prefix from errors returned across the
// store/core boundary. It deliberately does not expose driver error strings as
// protocol codes.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if idx := strings.IndexByte(message, ':'); idx >= 0 {
		message = message[:idx]
	}
	if strings.HasPrefix(message, "E_") || strings.HasPrefix(message, "W_") || strings.HasPrefix(message, "U_") {
		return message
	}
	return "E_INTERNAL"
}
