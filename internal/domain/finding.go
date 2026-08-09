package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Finding is a sum type represented by Subtype. Review findings are
// authoritative in a git file; reconciliation findings are DB-resident.
type Finding struct {
	Subtype       FindingSubtype `json:"subtype"`
	Key           string         `json:"id"`
	TicketID      string         `json:"ticket_id,omitempty"`
	Category      string         `json:"category,omitempty"`
	Severity      Severity       `json:"severity,omitempty"`
	Verdict       Verdict        `json:"verdict,omitempty"`
	Source        string         `json:"source,omitempty"`
	Message       string         `json:"message,omitempty"`
	RequirementID string         `json:"requirement_id,omitempty"`
	File          string         `json:"file,omitempty"`
	Line          int            `json:"line,omitempty"`
	Disposition   Disposition    `json:"disposition,omitempty"`
	WaiverReason  string         `json:"waiver_reason,omitempty"`
	WaiverActor   string         `json:"waiver_actor,omitempty"`
	Code          string         `json:"code,omitempty"`
	Subject       string         `json:"subject,omitempty"`
	Details       string         `json:"details,omitempty"`
}

type FindingSubtype string

const (
	FindingSubtypeReview         FindingSubtype = "review"
	FindingSubtypeReconciliation FindingSubtype = "reconciliation"
	FindingSubtypeAny            FindingSubtype = "any"
)

type Verdict string

const (
	VerdictConfirmed Verdict = "confirmed"
	VerdictRefuted   Verdict = "refuted"
	VerdictPlausible Verdict = "plausible"
)

type Disposition string

const (
	DispositionOpen   Disposition = "open"
	DispositionFixed  Disposition = "fixed"
	DispositionWaived Disposition = "waived"
)

type ReviewFindingInput struct {
	TicketID      string
	Category      string
	Severity      Severity
	Verdict       Verdict
	Source        string
	Message       string
	RequirementID string
	File          string
	Line          int
	Disposition   Disposition
	WaiverReason  string
	WaiverActor   string
}

type ReconciliationFindingInput struct {
	Code     string
	Subject  string
	Details  string
	TicketID string
}

var findingTokenPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func NewReviewFinding(input ReviewFindingInput) (Finding, error) {
	input.TicketID = strings.TrimSpace(input.TicketID)
	input.Category = strings.ToLower(strings.TrimSpace(input.Category))
	input.Source = strings.ToLower(strings.TrimSpace(input.Source))
	input.RequirementID = strings.TrimSpace(input.RequirementID)
	input.File = strings.TrimSpace(input.File)
	input.Message = strings.TrimSpace(input.Message)
	input.WaiverReason = strings.TrimSpace(input.WaiverReason)
	input.WaiverActor = strings.TrimSpace(input.WaiverActor)
	if input.Disposition == "" {
		input.Disposition = DispositionOpen
	}
	if err := validateReviewInput(input); err != nil {
		return Finding{}, err
	}
	canonicalFile, err := CanonicalFindingFile(input.File)
	if err != nil {
		return Finding{}, err
	}
	input.File = canonicalFile
	finding := Finding{
		Subtype: FindingSubtypeReview, Key: ReviewFindingKey(input),
		TicketID: input.TicketID, Category: input.Category, Severity: input.Severity,
		Verdict: input.Verdict, Source: input.Source, Message: input.Message,
		RequirementID: input.RequirementID, File: input.File, Line: input.Line,
		Disposition: input.Disposition, WaiverReason: input.WaiverReason, WaiverActor: input.WaiverActor,
	}
	return finding, nil
}

func NewReconciliationFinding(input ReconciliationFindingInput) (Finding, error) {
	input.Code = strings.TrimSpace(input.Code)
	input.Subject = strings.TrimSpace(input.Subject)
	input.Details = strings.TrimSpace(input.Details)
	input.TicketID = strings.TrimSpace(input.TicketID)
	if input.Code == "" || !strings.HasPrefix(input.Code, "E_") {
		return Finding{}, errors.New("E_FINDING_INVALID: reconciliation code is invalid")
	}
	if input.Subject == "" || input.Details == "" {
		return Finding{}, errors.New("E_FINDING_INVALID: reconciliation subject and details are required")
	}
	if input.TicketID != "" {
		if err := ValidateID(input.TicketID); err != nil {
			return Finding{}, errors.New("E_FINDING_INVALID: reconciliation ticket is invalid")
		}
	}
	hash := sha256.Sum256([]byte(input.Code + "\x00" + input.Subject + "\x00" + input.TicketID))
	return Finding{
		Subtype: FindingSubtypeReconciliation,
		Key:     "r-" + hex.EncodeToString(hash[:]),
		Code:    input.Code, Subject: input.Subject, Details: input.Details, Message: input.Details,
		TicketID: input.TicketID, Disposition: DispositionOpen,
	}, nil
}

func (f Finding) Validate() error {
	switch f.Subtype {
	case FindingSubtypeReview:
		input := ReviewFindingInput{TicketID: f.TicketID, Category: f.Category, Severity: f.Severity,
			Verdict: f.Verdict, Source: f.Source, Message: f.Message, RequirementID: f.RequirementID,
			File: f.File, Line: f.Line, Disposition: f.Disposition, WaiverReason: f.WaiverReason, WaiverActor: f.WaiverActor}
		validated, err := NewReviewFinding(input)
		if err != nil {
			return err
		}
		if f.Key != validated.Key {
			return errors.New("E_FINDING_INVALID: review finding key does not match identity")
		}
		return nil
	case FindingSubtypeReconciliation:
		_, err := NewReconciliationFinding(ReconciliationFindingInput{Code: f.Code, Subject: f.Subject, Details: f.Details, TicketID: f.TicketID})
		if err != nil {
			return err
		}
		if strings.TrimSpace(f.Key) == "" || f.Disposition != DispositionOpen || f.Category != "" || f.Source != "" || f.Verdict != "" || f.Severity != "" || f.RequirementID != "" || f.File != "" || f.Line != 0 || f.WaiverReason != "" || f.WaiverActor != "" {
			return errors.New("E_FINDING_INVALID: reconciliation finding contains review fields")
		}
		return nil
	default:
		return errors.New("E_FINDING_INVALID: unknown finding subtype")
	}
}

func validateReviewInput(input ReviewFindingInput) error {
	if err := ValidateID(input.TicketID); err != nil {
		return errors.New("E_FINDING_INVALID: finding ticket is invalid")
	}
	if !findingTokenPattern.MatchString(input.Category) || !findingTokenPattern.MatchString(input.Source) {
		return errors.New("E_FINDING_INVALID: category and source must be lowercase kebab tokens")
	}
	if !validSeverity(input.Severity) || (input.Verdict != VerdictConfirmed && input.Verdict != VerdictRefuted && input.Verdict != VerdictPlausible) {
		return errors.New("E_FINDING_INVALID: finding enum is invalid")
	}
	if input.Message == "" {
		return errors.New("E_FINDING_INVALID: finding message is empty")
	}
	if input.RequirementID != "" {
		if err := ValidateID(input.RequirementID); err != nil {
			return errors.New("E_FINDING_INVALID: finding requirement is invalid")
		}
	}
	if input.File == "" && input.Line != 0 {
		return errors.New("E_FINDING_INVALID: finding line requires a file")
	}
	if input.File != "" && input.Line <= 0 {
		return errors.New("E_FINDING_INVALID: finding line must be positive")
	}
	switch input.Disposition {
	case DispositionOpen, DispositionFixed:
		if input.WaiverReason != "" || input.WaiverActor != "" {
			return errors.New("E_FINDING_INVALID: waiver fields require waived disposition")
		}
	case DispositionWaived:
		if input.WaiverReason == "" || input.WaiverActor == "" {
			return errors.New("E_FINDING_INVALID: waived finding requires reason and actor")
		}
	default:
		return errors.New("E_FINDING_INVALID: finding disposition is invalid")
	}
	return nil
}

// CanonicalFindingFile normalises a repository-relative finding locus.
func CanonicalFindingFile(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return "", errors.New("E_FINDING_INVALID: finding file contains NUL")
	}
	if strings.Contains(raw, "\\") {
		return "", errors.New("E_FINDING_INVALID: finding file must use forward slashes")
	}
	if strings.HasPrefix(raw, "/") || (len(raw) >= 2 && raw[1] == ':') {
		return "", errors.New("E_FINDING_INVALID: finding file must be repository-relative")
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("E_FINDING_INVALID: finding file escapes repository")
	}
	return clean, nil
}

// ReviewFindingKey is the stable identity derivation. Mutable content is
// deliberately absent from the NUL-separated canonical tuple.
func ReviewFindingKey(input ReviewFindingInput) string {
	file, _ := CanonicalFindingFile(input.File)
	line := ""
	if file != "" {
		line = strconv.Itoa(input.Line)
	}
	canonical := strings.Join([]string{strings.TrimSpace(input.TicketID), strings.ToLower(strings.TrimSpace(input.Source)), strings.ToLower(strings.TrimSpace(input.Category)), file, line, strings.TrimSpace(input.RequirementID)}, "\x00")
	hash := sha256.Sum256([]byte(canonical))
	return "f-" + strings.TrimSpace(input.TicketID) + "-" + strings.ToLower(strings.TrimSpace(input.Source)) + "-" + strings.ToLower(strings.TrimSpace(input.Category)) + "-" + hex.EncodeToString(hash[:])
}

type findingFrontmatter struct {
	Schema        int            `json:"schema"`
	ID            string         `json:"id"`
	Subtype       FindingSubtype `json:"subtype"`
	TicketID      string         `json:"ticket_id"`
	Category      string         `json:"category"`
	Severity      Severity       `json:"severity"`
	Verdict       Verdict        `json:"verdict"`
	Source        string         `json:"source"`
	RequirementID string         `json:"requirement_id"`
	File          string         `json:"file"`
	Line          int            `json:"line"`
	Disposition   Disposition    `json:"disposition"`
	WaiverReason  string         `json:"waiver_reason"`
	WaiverActor   string         `json:"waiver_actor"`
}

func RenderFinding(finding Finding) ([]byte, error) {
	if err := finding.Validate(); err != nil {
		return nil, err
	}
	if finding.Subtype != FindingSubtypeReview {
		return nil, errors.New("E_FINDING_INVALID: only review findings have git files")
	}
	front, err := json.Marshal(findingFrontmatter{Schema: 1, ID: finding.Key, Subtype: finding.Subtype,
		TicketID: finding.TicketID, Category: finding.Category, Severity: finding.Severity,
		Verdict: finding.Verdict, Source: finding.Source, RequirementID: finding.RequirementID,
		File: finding.File, Line: finding.Line, Disposition: finding.Disposition,
		WaiverReason: finding.WaiverReason, WaiverActor: finding.WaiverActor})
	if err != nil {
		return nil, err
	}
	body := finding.Message
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return bytes.Join([][]byte{[]byte("---\n"), front, []byte("\n---\n"), []byte(body)}, nil), nil
}

func ParseFinding(data []byte) (Finding, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return Finding{}, errors.New("E_FINDING_INVALID: missing finding frontmatter")
	}
	rest := data[len("---\n"):]
	marker := []byte("\n---\n")
	idx := bytes.Index(rest, marker)
	if idx < 0 {
		return Finding{}, errors.New("E_FINDING_INVALID: malformed finding frontmatter")
	}
	var front findingFrontmatter
	dec := json.NewDecoder(bytes.NewReader(rest[:idx]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&front); err != nil {
		return Finding{}, fmt.Errorf("E_FINDING_INVALID: %w", err)
	}
	body := string(rest[idx+len(marker):])
	if !strings.HasSuffix(body, "\n") {
		return Finding{}, errors.New("E_FINDING_INVALID: finding body must end in newline")
	}
	if front.Subtype != FindingSubtypeReview || front.Schema != 1 {
		return Finding{}, errors.New("E_FINDING_INVALID: finding file is not a review schema")
	}
	finding, err := NewReviewFinding(ReviewFindingInput{TicketID: front.TicketID, Category: front.Category,
		Severity: front.Severity, Verdict: front.Verdict, Source: front.Source, Message: strings.TrimSuffix(body, "\n"),
		RequirementID: front.RequirementID, File: front.File, Line: front.Line, Disposition: front.Disposition,
		WaiverReason: front.WaiverReason, WaiverActor: front.WaiverActor})
	if err != nil {
		return Finding{}, err
	}
	if finding.Key != front.ID {
		return Finding{}, errors.New("E_FINDING_INVALID: filename/frontmatter finding ID mismatch")
	}
	return finding, nil
}
