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
	reader := bufio.NewReader(r)

	type validRecord struct {
		input   domain.ReviewFindingInput
		line    int
		existed bool
	}
	var (
		valid []validRecord
		skips []ImportSkip
		total int
	)

	// Pass 1: read every line (bufio.Reader has no fixed line-length limit, so a
	// long line is never silently dropped), parse + validate, and probe the
	// target with the NORMALISED key so imported-vs-updated is accurate and a
	// pre-existing corrupt target is caught here — before any write — rather than
	// mid-pass-2 (which would leave partial state in strict mode).
	lineNum := 0
	for {
		lineNum++
		line, readErr := reader.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			total++
			input, key, err := parseAndValidate(line, lineNum)
			if err != nil {
				skips = append(skips, ImportSkip{Line: lineNum, Error: err.Error()})
			} else if _, gerr := s.GetFinding(key); gerr != nil && ErrorCode(gerr) != "E_NOT_FOUND" {
				skips = append(skips, ImportSkip{Line: lineNum, Error: fmt.Sprintf("E_IMPORT_INVALID: line %d: existing finding at target is unreadable: %v", lineNum, gerr)})
			} else {
				valid = append(valid, validRecord{input: input, line: lineNum, existed: gerr == nil})
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return ImportSummary{}, fmt.Errorf("E_IMPORT_INVALID: read error near line %d: %v", lineNum, readErr)
		}
	}

	// Strict mode: any skip means import nothing. The skip error already carries
	// the E_IMPORT_INVALID code + line number.
	if strict && len(skips) > 0 {
		return ImportSummary{}, errors.New(skips[0].Error)
	}

	// Pass 2: import the validated records. A pre-existing corrupt target was
	// already caught in pass 1, so a failure here is a fresh runtime/IO error.
	imported, updated := 0, 0
	for _, rec := range valid {
		if _, _, err := s.AddFinding(ctx, rec.input); err != nil {
			if strict {
				return ImportSummary{}, fmt.Errorf("E_IMPORT_INVALID: import failed on validated record line %d: %w", rec.line, err)
			}
			skips = append(skips, ImportSkip{Line: rec.line, Error: fmt.Sprintf("E_IMPORT_INVALID: line %d: %v", rec.line, err)})
			continue
		}
		if rec.existed {
			updated++
		} else {
			imported++
		}
	}

	if skips == nil {
		skips = []ImportSkip{}
	}
	return ImportSummary{Imported: imported, Updated: updated, Skipped: skips, Total: total}, nil
}

// parseAndValidate decodes a line into a domain.ReviewFindingInput, validates it,
// and returns the NORMALISED content key (what AddFinding will write under).
func parseAndValidate(line string, lineNum int) (domain.ReviewFindingInput, string, error) {
	var raw rawFinding
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return domain.ReviewFindingInput{}, "", fmt.Errorf("E_IMPORT_INVALID: line %d: JSON decode error: %w", lineNum, err)
	}
	// Ensure no additional data follows the JSON object.
	_, err := dec.Token()
	if err == nil {
		return domain.ReviewFindingInput{}, "", fmt.Errorf("E_IMPORT_INVALID: line %d: trailing content after JSON object", lineNum)
	}
	if err != io.EOF {
		return domain.ReviewFindingInput{}, "", fmt.Errorf("E_IMPORT_INVALID: line %d: %w", lineNum, err)
	}

	if raw.Subtype != "" && raw.Subtype != "review" {
		return domain.ReviewFindingInput{}, "", fmt.Errorf("E_IMPORT_INVALID: line %d: only subtype review may be imported, got %q", lineNum, raw.Subtype)
	}

	disposition := raw.Disposition
	if disposition == "" {
		disposition = string(domain.DispositionOpen)
	}

	input := domain.ReviewFindingInput{
		TicketID:      raw.Ticket,
		Category:      raw.Category,
		Severity:      domain.Severity(raw.Severity),
		Verdict:       domain.Verdict(raw.Verdict),
		Source:        raw.Source,
		Message:       raw.Message,
		File:          raw.File,
		Line:          raw.Line,
		RequirementID: raw.Requirement,
		Disposition:   domain.Disposition(disposition),
		WaiverReason:  raw.WaiverReason,
		WaiverActor:   raw.WaiverActor,
	}

	finding, err := domain.NewReviewFinding(input)
	if err != nil {
		return domain.ReviewFindingInput{}, "", fmt.Errorf("E_IMPORT_INVALID: line %d: %w", lineNum, err)
	}

	return input, finding.Key, nil
}
