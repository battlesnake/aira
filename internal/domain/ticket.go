package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// covers: AR-5, AR-6

type Status string

const (
	StatusDraft      Status = "draft"
	StatusPlanned    Status = "planned"
	StatusInProgress Status = "in-progress"
	StatusInReview   Status = "in-review"
	StatusDone       Status = "done"
	StatusRetired    Status = "retired"
	StatusSuperseded Status = "superseded"
)

type Kind string

const (
	KindFeature         Kind = "feature"
	KindBug             Kind = "bug"
	KindChore           Kind = "chore"
	KindSpike           Kind = "spike"
	KindRequirementWork Kind = "requirement-work"
)

type Severity string

const (
	SeverityP0 Severity = "P0"
	SeverityP1 Severity = "P1"
	SeverityP2 Severity = "P2"
)

type RelationKind string

const (
	RelationBlocks     RelationKind = "blocks"
	RelationParent     RelationKind = "parent"
	RelationRelates    RelationKind = "relates"
	RelationDuplicates RelationKind = "duplicates"
	RelationSupersedes RelationKind = "supersedes"
	RelationResolves   RelationKind = "resolves"
)

type Relation struct {
	Kind RelationKind `json:"kind"`
	From string       `json:"from"`
	To   string       `json:"to"`
}

type Ticket struct {
	Schema    int        `json:"schema"`
	ID        string     `json:"id"`
	Project   string     `json:"project"`
	Title     string     `json:"title"`
	Status    Status     `json:"status"`
	Kind      Kind       `json:"kind"`
	Severity  Severity   `json:"severity"`
	Assignee  *string    `json:"assignee"`
	Milestone *string    `json:"milestone"`
	Labels    []string   `json:"labels"`
	Hold      bool       `json:"hold"`
	Relations []Relation `json:"relations"`
}

type CreateTicketInput struct {
	Title    string
	Kind     Kind
	Severity Severity
	Body     string
	Labels   []string
}

var idPattern = regexp.MustCompile(`^[A-Z]{2,}-[1-9][0-9]*$`)
var projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("E_ID_INVALID: invalid ticket ID %q", id)
	}
	return nil
}

func ValidateProjectSlug(project string) error {
	if !projectPattern.MatchString(project) {
		return fmt.Errorf("E_CONFIG_INVALID: invalid project slug %q", project)
	}
	return nil
}

func ValidateTransition(from, to Status) error {
	allowed := map[Status]map[Status]bool{
		StatusDraft:      {StatusPlanned: true, StatusRetired: true, StatusSuperseded: true},
		StatusPlanned:    {StatusInProgress: true, StatusRetired: true, StatusSuperseded: true},
		StatusInProgress: {StatusInReview: true, StatusPlanned: true, StatusRetired: true, StatusSuperseded: true},
		StatusInReview:   {StatusInProgress: true, StatusDone: true, StatusRetired: true, StatusSuperseded: true},
		StatusDone:       {StatusRetired: true, StatusSuperseded: true},
		StatusRetired:    {},
		StatusSuperseded: {},
	}
	if !allowed[from][to] {
		return fmt.Errorf("E_TRANSITION_INVALID: %s -> %s", from, to)
	}
	return nil
}

func (t Ticket) Validate() error {
	if t.Schema != 1 {
		return errors.New("E_CONFIG_INVALID: unsupported ticket schema")
	}
	if err := ValidateID(t.ID); err != nil {
		return err
	}
	if err := ValidateProjectSlug(t.Project); err != nil {
		return err
	}
	if strings.TrimSpace(t.Title) == "" {
		return errors.New("E_CONFIG_INVALID: ticket title is empty")
	}
	if !validStatus(t.Status) || !validKind(t.Kind) || !validSeverity(t.Severity) {
		return errors.New("E_CONFIG_INVALID: ticket enum is invalid")
	}
	for _, label := range t.Labels {
		if label == "" || label != strings.ToLower(label) {
			return errors.New("E_CONFIG_INVALID: labels must be non-empty lowercase strings")
		}
	}
	for _, r := range t.Relations {
		if !validRelation(r.Kind) || r.From == r.To || ValidateID(r.From) != nil || ValidateID(r.To) != nil {
			return errors.New("E_RELATION_INVALID: invalid relation")
		}
	}
	return nil
}

func validStatus(s Status) bool {
	switch s {
	case StatusDraft, StatusPlanned, StatusInProgress, StatusInReview, StatusDone, StatusRetired, StatusSuperseded:
		return true
	default:
		return false
	}
}

func validKind(k Kind) bool {
	switch k {
	case KindFeature, KindBug, KindChore, KindSpike, KindRequirementWork:
		return true
	default:
		return false
	}
}

func validSeverity(s Severity) bool {
	return s == SeverityP0 || s == SeverityP1 || s == SeverityP2
}

func validRelation(k RelationKind) bool {
	switch k {
	case RelationBlocks, RelationParent, RelationRelates, RelationDuplicates, RelationSupersedes, RelationResolves:
		return true
	default:
		return false
	}
}

func RenderTicket(ticket Ticket, body string) ([]byte, error) {
	ticket.Schema = 1
	ticket.Labels = append([]string(nil), ticket.Labels...)
	if ticket.Labels == nil {
		ticket.Labels = []string{}
	}
	sort.Strings(ticket.Labels)
	ticket.Relations = append([]Relation(nil), ticket.Relations...)
	if ticket.Relations == nil {
		ticket.Relations = []Relation{}
	}
	sort.Slice(ticket.Relations, func(i, j int) bool {
		a, b := ticket.Relations[i], ticket.Relations[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return idLess(a.From, b.From)
		}
		return idLess(a.To, b.To)
	})
	if err := ticket.Validate(); err != nil {
		return nil, err
	}
	if body == "" {
		body = "\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	header, err := json.Marshal(ticket)
	if err != nil {
		return nil, err
	}
	return bytes.Join([][]byte{[]byte("---\n"), header, []byte("\n---\n"), []byte(body)}, nil), nil
}

func ParseTicket(data []byte) (Ticket, string, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return Ticket{}, "", errors.New("E_CONFIG_INVALID: missing ticket frontmatter")
	}
	rest := data[len("---\n"):]
	marker := []byte("\n---\n")
	idx := bytes.Index(rest, marker)
	if idx < 0 {
		return Ticket{}, "", errors.New("E_CONFIG_INVALID: malformed ticket frontmatter")
	}
	var ticket Ticket
	dec := json.NewDecoder(bytes.NewReader(rest[:idx]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ticket); err != nil {
		return Ticket{}, "", fmt.Errorf("E_CONFIG_INVALID: %w", err)
	}
	if err := ticket.Validate(); err != nil {
		return Ticket{}, "", err
	}
	body := string(rest[idx+len(marker):])
	if !strings.HasSuffix(body, "\n") {
		return Ticket{}, "", errors.New("E_CONFIG_INVALID: ticket body must end in newline")
	}
	return ticket, body, nil
}

func idLess(a, b string) bool {
	if a == b {
		return false
	}
	ap, an := splitID(a)
	bp, bn := splitID(b)
	if ap != bp {
		return ap < bp
	}
	an = strings.TrimLeft(an, "0")
	bn = strings.TrimLeft(bn, "0")
	if an == "" {
		an = "0"
	}
	if bn == "" {
		bn = "0"
	}
	if len(an) != len(bn) {
		return len(an) < len(bn)
	}
	return an < bn
}

func splitID(id string) (string, string) {
	idx := strings.LastIndexByte(id, '-')
	if idx < 0 {
		return id, ""
	}
	return id[:idx], id[idx+1:]
}
