// internal/store/import.go
package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"aira/internal/domain"
)

// ImportSkip represents a line that could not be processed.
type ImportSkip struct {
	Line  int    `json:"line"`
	Error string `json:"error"`
}

// ImportSummary holds the result of a findings import.
type ImportSummary struct {
	Imported int          `json:"imported"`
	Updated  int          `json:"updated"`
	Skipped  []ImportSkip `json:"skipped"`
	Total    int          `json:"total"`
}

const maxLineLength = 8 * 1024 * 1024

type rawFinding struct {
	Subtype      string `json:"subtype"`
	Ticket       string `json:"ticket"`
	Category     string `json:"category"`
	Severity     string `json:"severity"`
	Verdict      string `json:"verdict"`
	Source       string `json:"source"`
	Message      string `json:"message"`
	File         string `json:"file"`
	Line         int    `json:"line"`
	Requirement  string `json:"requirement"`
	Disposition  string `json:"disposition"`
	WaiverReason string `json:"waiver_reason"`
	WaiverActor  string `json:"waiver_actor"`
}

// ImportFindingsFile opens a JSONL file and imports its review findings. A
// missing file is a stable E_NOT_FOUND; the parse/import semantics are
// ImportFindings.
func (s *Store) ImportFindingsFile(ctx context.Context, path string, strict bool) (ImportSummary, error) {
	if strings.TrimSpace(path) == "" {
		return ImportSummary{}, errors.New("E_NOT_FOUND: import requires a file path")
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ImportSummary{}, fmt.Errorf("E_NOT_FOUND: import file %q does not exist", path)
	}
	if err != nil {
		return ImportSummary{}, fmt.Errorf("E_IMPORT_INVALID: cannot read import file %q: %w", path, err)
	}
	defer f.Close()
	return s.ImportFindings(ctx, f, strict)
}

func (s *Store) ImportFindings(ctx context.Context, r io.Reader, strict bool) (ImportSummary, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, bufio.MaxScanTokenSize)
	scanner.Buffer(buf, maxLineLength)

	var (
		validInputs []struct {
			input domain.ReviewFindingInput
			line  int
		}
		skips []ImportSkip
		total int
	)

	// Pass 1: parse and validate all non-blank lines.
	for lineNum := 1; scanner.Scan(); lineNum++ {
		line := scanner.Text()
		// Trim whitespace; if empty, skip (not a record).
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
		input, err := parseAndValidate(line, lineNum)
		if err != nil {
			skips = append(skips, ImportSkip{Line: lineNum, Error: err.Error()})
			continue
		}
		validInputs = append(validInputs, struct {
			input domain.ReviewFindingInput
			line  int
		}{input: input, line: lineNum})
	}

	if err := scanner.Err(); err != nil {
		return ImportSummary{}, fmt.Errorf("scanning input: %w", err)
	}

	// In strict mode any skip is an error – import nothing. The skip error
	// already carries the E_IMPORT_INVALID code + line number.
	if strict && len(skips) > 0 {
		return ImportSummary{}, errors.New(skips[0].Error)
	}

	// Pass 2: import valid records.
	imported := 0
	updated := 0
	for _, rec := range validInputs {
		key := domain.ReviewFindingKey(rec.input)
		_, err := s.GetFinding(key)
		existed := err == nil
		if err != nil && ErrorCode(err) != "E_NOT_FOUND" {
			return ImportSummary{}, fmt.Errorf("unexpected store error: %w", err)
		}

		if _, _, err := s.AddFinding(ctx, rec.input); err != nil {
			if strict {
				return ImportSummary{}, fmt.Errorf("import error on validated record line %d: %w", rec.line, err)
			}
			skips = append(skips, ImportSkip{
				Line:  rec.line,
				Error: fmt.Sprintf("E_ADD_FAILED: line %d: %v", rec.line, err.Error()),
			})
			continue
		}

		if existed {
			updated++
		} else {
			imported++
		}
	}

	if skips == nil {
		skips = []ImportSkip{}
	}

	return ImportSummary{
		Imported: imported,
		Updated:  updated,
		Skipped:  skips,
		Total:    total,
	}, nil
}

// parseAndValidate decodes a line into a domain.ReviewFindingInput and validates it.
func parseAndValidate(line string, lineNum int) (domain.ReviewFindingInput, error) {
	var raw rawFinding
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return domain.ReviewFindingInput{}, fmt.Errorf("E_IMPORT_INVALID: line %d: JSON decode error: %w", lineNum, err)
	}
	// Ensure no additional data follows the JSON object.
	_, err := dec.Token()
	if err == nil {
		return domain.ReviewFindingInput{}, fmt.Errorf("E_IMPORT_INVALID: line %d: trailing content after JSON object", lineNum)
	}
	if err != io.EOF {
		return domain.ReviewFindingInput{}, fmt.Errorf("E_IMPORT_INVALID: line %d: %w", lineNum, err)
	}

	if raw.Subtype != "" && raw.Subtype != "review" {
		return domain.ReviewFindingInput{}, fmt.Errorf("E_IMPORT_INVALID: line %d: only subtype review may be imported, got %q", lineNum, raw.Subtype)
	}

	disposition := raw.Disposition
	if disposition == "" {
		disposition = string(domain.DispositionOpen)
	}

	input := domain.ReviewFindingInput{
		TicketID:     raw.Ticket,
		Category:     raw.Category,
		Severity:     domain.Severity(raw.Severity),
		Verdict:      domain.Verdict(raw.Verdict),
		Source:       raw.Source,
		Message:      raw.Message,
		File:         raw.File,
		Line:         raw.Line,
		RequirementID: raw.Requirement,
		Disposition:  domain.Disposition(disposition),
		WaiverReason: raw.WaiverReason,
		WaiverActor:  raw.WaiverActor,
	}

	if _, err := domain.NewReviewFinding(input); err != nil {
		return domain.ReviewFindingInput{}, fmt.Errorf("E_IMPORT_INVALID: line %d: %w", lineNum, err)
	}

	return input, nil
}
