package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"aira/internal/domain"
)

// ListLimit is the bounded row count used by every row-returning face.
const ListLimit = 50

// TicketRecord combines the authoritative ticket file with its derived index
// metadata. Body is intentionally kept out of domain.Ticket because it is file
// content, not frontmatter state.
type TicketRecord struct {
	Ticket     domain.Ticket `json:"ticket"`
	Body       string        `json:"body,omitempty"`
	Path       string        `json:"path"`
	WorktreeID string        `json:"worktree_id"`
	Digest     string        `json:"digest,omitempty"`
}

type selector struct {
	ExactID   string
	ExactPath string
	Terms     []queryTerm
}

type queryTerm struct {
	Field string
	Value string
	Text  bool
}

// ParseSelector implements the Phase-1 exact selector and plural query grammar.
// Both ':' (the design grammar) and '=' (the CLI's field=value spelling) are
// accepted for field terms.
func ParseSelector(raw string) (string, error) {
	sel, err := parseSelector(raw)
	if err != nil {
		return "", err
	}
	if sel.ExactID != "" {
		return sel.ExactID, nil
	}
	return raw, nil
}

func parseSelector(raw string) (selector, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return selector{}, nil
	}
	if err := domain.ValidateID(raw); err == nil {
		return selector{ExactID: raw}, nil
	}
	if id, ok := anchorID(raw); ok {
		return selector{ExactID: id, ExactPath: filepath.ToSlash(filepath.Clean(raw))}, nil
	}
	terms, err := parseTerms(raw)
	if err != nil {
		return selector{}, fmt.Errorf("E_SELECTOR_INVALID: %w", err)
	}
	return selector{Terms: terms}, nil
}

func anchorID(raw string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(raw))
	const prefix = ".aira/tickets/"
	if !strings.HasPrefix(clean, prefix) || !strings.HasSuffix(clean, ".md") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(clean, prefix), ".md")
	if strings.Contains(id, "/") || domain.ValidateID(id) != nil {
		return "", false
	}
	return id, true
}

func parseTerms(raw string) ([]queryTerm, error) {
	var terms []queryTerm
	for pos := 0; pos < len(raw); {
		for pos < len(raw) && raw[pos] == ' ' {
			pos++
		}
		if pos == len(raw) {
			break
		}
		start := pos
		for pos < len(raw) && raw[pos] != ' ' && raw[pos] != ':' && raw[pos] != '=' {
			pos++
		}
		if pos == start {
			return nil, errors.New("missing query field")
		}
		field := strings.ToLower(raw[start:pos])
		if pos >= len(raw) || (raw[pos] != ':' && raw[pos] != '=') {
			return nil, errors.New("query term requires ':' or '='")
		}
		pos++
		value, next, err := queryValue(raw, pos)
		if err != nil {
			return nil, err
		}
		pos = next
		if value == "" {
			return nil, errors.New("query value is empty")
		}
		if field == "text" {
			terms = append(terms, queryTerm{Field: "text", Value: value, Text: true})
			continue
		}
		if !validQueryField(field) {
			return nil, fmt.Errorf("unknown query field %q", field)
		}
		terms = append(terms, queryTerm{Field: field, Value: value})
	}
	if len(terms) == 0 {
		return nil, errors.New("query has no terms")
	}
	return terms, nil
}

func queryValue(raw string, pos int) (string, int, error) {
	if pos >= len(raw) {
		return "", pos, errors.New("missing query value")
	}
	if raw[pos] == '"' {
		start := pos
		pos++
		escaped := false
		for pos < len(raw) {
			if raw[pos] == '"' && !escaped {
				var value string
				if err := json.Unmarshal([]byte(raw[start:pos+1]), &value); err != nil {
					return "", pos, fmt.Errorf("invalid quoted value: %w", err)
				}
				if !utf8.ValidString(value) {
					return "", pos, errors.New("query value is not UTF-8")
				}
				if pos+1 < len(raw) && raw[pos+1] != ' ' {
					return "", pos, errors.New("quoted value must end at a term boundary")
				}
				return value, pos + 1, nil
			}
			if raw[pos] == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
			pos++
		}
		return "", pos, errors.New("unterminated quoted value")
	}
	start := pos
	for pos < len(raw) && raw[pos] != ' ' {
		pos++
	}
	value := raw[start:pos]
	for _, r := range value {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._/-", r)) {
			return "", pos, fmt.Errorf("invalid bare value %q", value)
		}
	}
	return value, pos, nil
}

func validQueryField(field string) bool {
	switch field {
	case "id", "status", "kind", "severity", "assignee", "milestone", "label", "project", "hold":
		return true
	default:
		return false
	}
}

// Get resolves one exact selector. Query-shaped selectors are accepted for
// compatibility with the CLI, but cardinality is always checked explicitly.
func (s *Store) Get(selector string) (TicketRecord, error) {
	sel, err := parseSelector(selector)
	if err != nil {
		return TicketRecord{}, err
	}
	if sel.ExactID != "" {
		rows, err := s.records(selectorFilter{ID: sel.ExactID, Path: sel.ExactPath})
		if err != nil {
			return TicketRecord{}, err
		}
		return singular(rows)
	}
	rows, err := s.records(selectorFilter{Terms: sel.Terms})
	if err != nil {
		return TicketRecord{}, err
	}
	return singular(rows)
}

// List returns all current-worktree matches. It does not apply the output cap;
// the core applies that cap after it has computed the total and distribution.
func (s *Store) List(selector string) ([]TicketRecord, error) {
	sel, err := parseSelector(selector)
	if err != nil {
		return nil, err
	}
	filter := selectorFilter{ID: sel.ExactID, Path: sel.ExactPath, Terms: sel.Terms}
	return s.records(filter)
}

func (s *Store) Find(selector string) ([]TicketRecord, error) { return s.List(selector) }

type selectorFilter struct {
	ID    string
	Path  string
	Terms []queryTerm
}

func (s *Store) records(filter selectorFilter) ([]TicketRecord, error) {
	query := `SELECT id, path, digest, status, hold, title, kind, severity FROM tickets WHERE project_id=? AND worktree_id=?`
	args := []any{s.projectID, s.worktreeID}
	if filter.ID != "" {
		query += ` AND id=?`
		args = append(args, filter.ID)
	}
	if filter.Path != "" {
		query += ` AND path=?`
		args = append(args, filepath.Join(s.root, filepath.FromSlash(filter.Path)))
	}
	for _, term := range filter.Terms {
		switch term.Field {
		case "id", "status", "kind", "severity":
			query += ` AND ` + term.Field + `=?`
			args = append(args, term.Value)
		case "hold":
			value, err := strconv.ParseBool(strings.ToLower(term.Value))
			if err != nil {
				return nil, errors.New("E_SELECTOR_INVALID: hold must be true or false")
			}
			query += ` AND hold=?`
			args = append(args, boolInt(value))
		case "assignee", "milestone":
			// These fields are not in the index table yet; filter after parsing.
		case "label", "project", "text":
			// Filter after parsing to keep the index schema intentionally small.
		}
	}
	query += ` ORDER BY id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TicketRecord
	for rows.Next() {
		var record TicketRecord
		var hold int
		if err := rows.Scan(&record.Ticket.ID, &record.Path, &record.Digest, &record.Ticket.Status, &hold,
			&record.Ticket.Title, &record.Ticket.Kind, &record.Ticket.Severity); err != nil {
			return nil, err
		}
		record.Ticket.Schema = 1
		record.Ticket.Project = s.projectSlug
		record.Ticket.Hold = hold != 0
		record.WorktreeID = s.worktreeID
		data, err := os.ReadFile(record.Path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("E_RECONCILE_REQUIRED: indexed ticket file is missing")
		}
		if err != nil {
			return nil, err
		}
		parsed, body, err := domain.ParseTicket(data)
		if err != nil {
			return nil, err
		}
		record.Ticket = parsed
		record.Body = body
		record.Digest = digestBytes(data)
		if matchesTerms(record, filter.Terms) {
			result = append(result, record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func matchesTerms(record TicketRecord, terms []queryTerm) bool {
	for _, term := range terms {
		var matched bool
		switch term.Field {
		case "id":
			matched = record.Ticket.ID == term.Value
		case "status":
			matched = string(record.Ticket.Status) == term.Value
		case "kind":
			matched = string(record.Ticket.Kind) == term.Value
		case "severity":
			matched = string(record.Ticket.Severity) == term.Value
		case "hold":
			want, err := strconv.ParseBool(strings.ToLower(term.Value))
			matched = err == nil && record.Ticket.Hold == want
		case "project":
			matched = record.Ticket.Project == term.Value
		case "assignee":
			matched = record.Ticket.Assignee != nil && *record.Ticket.Assignee == term.Value
		case "milestone":
			matched = record.Ticket.Milestone != nil && *record.Ticket.Milestone == term.Value
		case "label":
			for _, label := range record.Ticket.Labels {
				if label == term.Value {
					matched = true
					break
				}
			}
		case "text":
			matched = strings.Contains(strings.ToLower(record.Ticket.Title+"\n"+record.Body), strings.ToLower(term.Value))
		}
		if !matched {
			return false
		}
	}
	return true
}

func singular(rows []TicketRecord) (TicketRecord, error) {
	switch len(rows) {
	case 0:
		return TicketRecord{}, errors.New("E_NOT_FOUND: selector matched no tickets")
	case 1:
		return rows[0], nil
	default:
		return TicketRecord{}, errors.New("E_SELECTOR_AMBIGUOUS: selector matched multiple tickets")
	}
}

type CountResult struct {
	Total        int            `json:"total"`
	Distribution map[string]int `json:"distribution,omitempty"`
	By           string         `json:"by"`
}

func (s *Store) Count(selector, by string) (CountResult, error) {
	if !validDistributionField(by) {
		return CountResult{}, fmt.Errorf("E_SELECTOR_INVALID: unsupported distribution field %q", by)
	}
	rows, err := s.List(selector)
	if err != nil {
		return CountResult{}, err
	}
	result := CountResult{Total: len(rows), By: by, Distribution: map[string]int{}}
	for _, row := range rows {
		for _, value := range distributionValues(row, by) {
			result.Distribution[value]++
		}
	}
	return result, nil
}

func validDistributionField(field string) bool {
	switch field {
	case "status", "kind", "severity", "hold", "project", "label", "assignee", "milestone":
		return true
	default:
		return false
	}
}

func distributionValues(row TicketRecord, by string) []string {
	switch by {
	case "status":
		return []string{string(row.Ticket.Status)}
	case "kind":
		return []string{string(row.Ticket.Kind)}
	case "severity":
		return []string{string(row.Ticket.Severity)}
	case "hold":
		return []string{strconv.FormatBool(row.Ticket.Hold)}
	case "project":
		return []string{row.Ticket.Project}
	case "assignee":
		if row.Ticket.Assignee == nil {
			return []string{"(none)"}
		}
		return []string{*row.Ticket.Assignee}
	case "milestone":
		if row.Ticket.Milestone == nil {
			return []string{"(none)"}
		}
		return []string{*row.Ticket.Milestone}
	case "label":
		if len(row.Ticket.Labels) == 0 {
			return []string{"(none)"}
		}
		return row.Ticket.Labels
	default:
		return []string{"(unknown)"}
	}
}

func sortRecords(records []TicketRecord) {
	sort.SliceStable(records, func(i, j int) bool { return records[i].Ticket.ID < records[j].Ticket.ID })
}
