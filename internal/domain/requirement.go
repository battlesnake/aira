package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RequirementStatus is the closed lifecycle of a requirement node.
type RequirementStatus string

const (
	RequirementBuilt      RequirementStatus = "built"
	RequirementPartial    RequirementStatus = "partial"
	RequirementDesigned   RequirementStatus = "designed"
	RequirementPlanned    RequirementStatus = "planned"
	RequirementBoundary   RequirementStatus = "boundary"
	RequirementRetired    RequirementStatus = "retired"
	RequirementSuperseded RequirementStatus = "superseded"
)

func ValidRequirementStatus(status RequirementStatus) bool {
	switch status {
	case RequirementBuilt, RequirementPartial, RequirementDesigned, RequirementPlanned,
		RequirementBoundary, RequirementRetired, RequirementSuperseded:
		return true
	}
	return false
}

// Requirement is a git-durable requirement node. The requirement statement is
// the file body; the frontmatter carries only identity and status. The
// covers/verifies edges are discovered from code annotations (M9c), never stored
// on the node, so there is no second registry to drift.
type Requirement struct {
	Schema int               `json:"schema"`
	ID     string            `json:"id"`
	Status RequirementStatus `json:"status"`
	Text   string            `json:"-"` // the file body, not frontmatter
}

type RequirementInput struct {
	ID     string
	Text   string
	Status RequirementStatus
}

// NewRequirement validates and constructs a requirement, mirroring
// NewReviewFinding: closed status enum, non-empty text, valid ID shape.
func NewRequirement(input RequirementInput) (Requirement, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Text = strings.TrimSpace(input.Text)
	if err := ValidateID(input.ID); err != nil {
		return Requirement{}, fmt.Errorf("E_REQUIREMENT_INVALID: %s", err)
	}
	if input.Text == "" {
		return Requirement{}, errors.New("E_REQUIREMENT_INVALID: requirement text is required")
	}
	if !ValidRequirementStatus(input.Status) {
		return Requirement{}, fmt.Errorf("E_REQUIREMENT_INVALID: invalid status %q", input.Status)
	}
	return Requirement{Schema: 1, ID: input.ID, Status: input.Status, Text: input.Text}, nil
}

func (r Requirement) Validate() error {
	if r.Schema != 1 {
		return errors.New("E_REQUIREMENT_INVALID: unsupported requirement schema")
	}
	if err := ValidateID(r.ID); err != nil {
		return fmt.Errorf("E_REQUIREMENT_INVALID: %s", err)
	}
	if !ValidRequirementStatus(r.Status) {
		return fmt.Errorf("E_REQUIREMENT_INVALID: invalid status %q", r.Status)
	}
	if strings.TrimSpace(r.Text) == "" {
		return errors.New("E_REQUIREMENT_INVALID: requirement text is required")
	}
	return nil
}

// requirementFrontmatter is the JSON frontmatter shape (Text is carried as the
// file body, exactly as a ticket's description is).
type requirementFrontmatter struct {
	Schema int               `json:"schema"`
	ID     string            `json:"id"`
	Status RequirementStatus `json:"status"`
}

func RenderRequirement(requirement Requirement) ([]byte, error) {
	requirement.Schema = 1
	if err := requirement.Validate(); err != nil {
		return nil, err
	}
	body := requirement.Text
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	header, err := json.Marshal(requirementFrontmatter{Schema: 1, ID: requirement.ID, Status: requirement.Status})
	if err != nil {
		return nil, err
	}
	return bytes.Join([][]byte{[]byte("---\n"), header, []byte("\n---\n"), []byte(body)}, nil), nil
}

func ParseRequirement(data []byte) (Requirement, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return Requirement{}, errors.New("E_REQUIREMENT_INVALID: missing requirement frontmatter")
	}
	rest := data[len("---\n"):]
	marker := []byte("\n---\n")
	idx := bytes.Index(rest, marker)
	if idx < 0 {
		return Requirement{}, errors.New("E_REQUIREMENT_INVALID: malformed requirement frontmatter")
	}
	var front requirementFrontmatter
	dec := json.NewDecoder(bytes.NewReader(rest[:idx]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&front); err != nil {
		return Requirement{}, fmt.Errorf("E_REQUIREMENT_INVALID: %s", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Requirement{}, errors.New("E_REQUIREMENT_INVALID: trailing content in requirement frontmatter")
		}
		return Requirement{}, fmt.Errorf("E_REQUIREMENT_INVALID: trailing content in requirement frontmatter: %w", err)
	}
	body := string(rest[idx+len(marker):])
	if !strings.HasSuffix(body, "\n") {
		return Requirement{}, errors.New("E_REQUIREMENT_INVALID: requirement body must end in newline")
	}
	requirement := Requirement{Schema: front.Schema, ID: front.ID, Status: front.Status, Text: strings.TrimRight(body, "\n")}
	if err := requirement.Validate(); err != nil {
		return Requirement{}, err
	}
	return requirement, nil
}
