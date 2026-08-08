package store

import (
	"database/sql"
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
	Warnings   []string      `json:"-"`
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
		for pos < len(raw) && raw[pos] != ' ' && raw[pos] != ':' {
			pos++
		}
		if pos == start {
			return nil, errors.New("missing query field")
		}
		field := strings.ToLower(raw[start:pos])
		if pos >= len(raw) || raw[pos] != ':' {
			return nil, errors.New("query term requires ':'")
		}
		pos++
		quotedText := field == "text" && pos < len(raw) && raw[pos] == '"'
		value, next, err := queryValue(raw, pos)
		if err != nil {
			return nil, err
		}
		pos = next
		if value == "" {
			return nil, errors.New("query value is empty")
		}
		if field == "text" {
			if !quotedText {
				return nil, errors.New("text query value must be quoted")
			}
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
	case "id", "status", "kind", "severity", "assignee", "milestone", "label", "project":
		return true
	default:
		return false
	}
}

// Get resolves only an exact ID or exact file anchor. Plural query grammar is
// deliberately not accepted by singular commands.
func (s *Store) Get(selector string) (TicketRecord, error) {
	if strings.TrimSpace(selector) == "" {
		return TicketRecord{}, errors.New("E_SELECTOR_INVALID: singular selector is empty")
	}
	sel, err := parseSelector(selector)
	if err != nil {
		return TicketRecord{}, err
	}
	if sel.ExactID == "" {
		return TicketRecord{}, errors.New("E_SELECTOR_INVALID: singular selector must be an exact ID or file anchor")
	}
	return s.exactRecord(sel.ExactID, sel.ExactPath)
}

// List returns all current-worktree matches. It does not apply the output cap;
// the core applies that cap after it has computed the total and distribution.
func (s *Store) List(selector string) ([]TicketRecord, error) {
	sel, err := parseSelector(selector)
	if err != nil {
		return nil, err
	}
	if sel.ExactID != "" {
		record, err := s.exactRecord(sel.ExactID, sel.ExactPath)
		if ErrorCode(err) == "E_NOT_FOUND" {
			return []TicketRecord{}, nil
		}
		if err != nil {
			return nil, err
		}
		return []TicketRecord{record}, nil
	}
	return s.records(selectorFilter{Terms: sel.Terms})
}

func (s *Store) Find(selector string) ([]TicketRecord, error) { return s.List(selector) }

type selectorFilter struct {
	Terms []queryTerm
}

func (s *Store) records(filter selectorFilter) ([]TicketRecord, error) {
	indexed, err := s.indexedDigests()
	if err != nil {
		return nil, err
	}
	tickets, _, err := scanTickets(s.root, s.worktreeID)
	if err != nil {
		return nil, err
	}
	result := make([]TicketRecord, 0, len(tickets))
	for _, scanned := range tickets {
		record := TicketRecord{Ticket: scanned.Ticket, Body: scanned.Body, Path: repoPath(s.root, scanned.Path), WorktreeID: s.worktreeID, Digest: scanned.Digest}
		if indexedDigest, ok := indexed[scanned.Ticket.ID]; ok && indexedDigest != scanned.Digest {
			record.Warnings = []string{"W_STALE_INDEX"}
		}
		if matchesTerms(record, filter.Terms) {
			result = append(result, record)
		}
	}
	sortRecords(result)
	return result, nil
}

func (s *Store) exactRecord(id, anchor string) (TicketRecord, error) {
	path := filepath.Join(s.root, ".aira", "tickets", id+".md")
	if anchor != "" {
		path = filepath.Join(s.root, filepath.FromSlash(anchor))
	}
	data, err := readRegularTicket(path)
	if errors.Is(err, os.ErrNotExist) {
		return TicketRecord{}, errors.New("E_NOT_FOUND: selector matched no tickets")
	}
	if err != nil {
		return TicketRecord{}, err
	}
	ticket, body, err := domain.ParseTicket(data)
	if err != nil {
		return TicketRecord{}, err
	}
	if ticket.ID != id || filepath.Base(path) != id+".md" {
		return TicketRecord{}, fmt.Errorf("E_CONFIG_INVALID: filename/frontmatter mismatch %s", repoPath(s.root, path))
	}
	record := TicketRecord{Ticket: ticket, Body: body, Path: repoPath(s.root, path), WorktreeID: s.worktreeID, Digest: digestBytes(data)}
	var indexedDigest string
	err = s.db.QueryRow(`SELECT digest FROM tickets WHERE project_id=? AND worktree_id=? AND id=?`, s.projectID, s.worktreeID, id).Scan(&indexedDigest)
	if err == nil && indexedDigest != record.Digest {
		record.Warnings = []string{"W_STALE_INDEX"}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TicketRecord{}, err
	}
	return record, nil
}

func (s *Store) indexedDigests() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT id, digest FROM tickets WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var id, digest string
		if err := rows.Scan(&id, &digest); err != nil {
			return nil, err
		}
		result[id] = digest
	}
	return result, rows.Err()
}

func repoPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func readRegularTicket(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("E_CONFIG_INVALID: ticket path is not a regular file")
	}
	return os.ReadFile(path)
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

type CountResult struct {
	Total        int            `json:"total"`
	Distribution map[string]int `json:"distribution,omitempty"`
	By           string         `json:"by"`
	Warnings     []string       `json:"-"`
}

func (s *Store) Count(selector, by string) (CountResult, error) {
	if !validDistributionField(by) {
		return CountResult{}, fmt.Errorf("E_SELECTOR_INVALID: unsupported distribution field %q", by)
	}
	sel, err := parseSelector(selector)
	if err != nil {
		return CountResult{}, err
	}
	if canAggregateCount(sel, by) {
		stale, scannedFindings, err := s.indexState()
		if err != nil {
			return CountResult{}, err
		}
		if stale || scannedFindings {
			rows, err := s.List(selector)
			if err != nil {
				return CountResult{}, err
			}
			result := countRows(rows, by)
			if stale {
				result.Warnings = []string{"W_STALE_INDEX"}
			}
			return result, nil
		}
		return s.aggregateCount(sel, by)
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

func countRows(rows []TicketRecord, by string) CountResult {
	result := CountResult{Total: len(rows), By: by, Distribution: map[string]int{}}
	for _, row := range rows {
		for _, value := range distributionValues(row, by) {
			result.Distribution[value]++
		}
	}
	return result
}

func (s *Store) indexState() (stale, scannedFindings bool, err error) {
	indexed, err := s.indexedDigests()
	if err != nil {
		return false, false, err
	}
	tickets, findings, err := scanTickets(s.root, s.worktreeID)
	if err != nil {
		return false, false, err
	}
	scannedFindings = len(findings) > 0
	seen := make(map[string]bool, len(tickets))
	for _, ticket := range tickets {
		seen[ticket.Ticket.ID] = true
		if indexed[ticket.Ticket.ID] != ticket.Digest {
			stale = true
		}
	}
	for id := range indexed {
		if !seen[id] {
			stale = true
		}
	}
	return stale, scannedFindings, nil
}

func canAggregateCount(sel selector, by string) bool {
	if by != "status" && by != "kind" && by != "severity" && by != "hold" {
		return false
	}
	if sel.ExactPath != "" {
		return false
	}
	for _, term := range sel.Terms {
		switch term.Field {
		case "id", "status", "kind", "severity":
		case "hold":
			return false
		default:
			return false
		}
	}
	return true
}

func (s *Store) aggregateCount(sel selector, by string) (CountResult, error) {
	column := by
	selectColumn := column
	if by == "hold" {
		selectColumn = "hold"
	}
	query := "SELECT " + selectColumn + ", COUNT(*) FROM tickets WHERE project_id=? AND worktree_id=?"
	args := []any{s.projectID, s.worktreeID}
	if sel.ExactID != "" {
		query += " AND id=?"
		args = append(args, sel.ExactID)
	}
	for _, term := range sel.Terms {
		if term.Field == "id" || term.Field == "status" || term.Field == "kind" || term.Field == "severity" {
			query += " AND " + term.Field + "=?"
			args = append(args, term.Value)
		}
	}
	query += " GROUP BY " + selectColumn
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return CountResult{}, err
	}
	defer rows.Close()
	result := CountResult{By: by, Distribution: map[string]int{}}
	for rows.Next() {
		var value string
		var count int
		if by == "hold" {
			var hold int
			if err := rows.Scan(&hold, &count); err != nil {
				return CountResult{}, err
			}
			value = strconv.FormatBool(hold != 0)
		} else if err := rows.Scan(&value, &count); err != nil {
			return CountResult{}, err
		}
		result.Distribution[value] = count
		result.Total += count
	}
	return result, rows.Err()
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
	sort.SliceStable(records, func(i, j int) bool {
		leftPrefix, leftNumber := splitTicketID(records[i].Ticket.ID)
		rightPrefix, rightNumber := splitTicketID(records[j].Ticket.ID)
		if leftPrefix != rightPrefix {
			return leftPrefix < rightPrefix
		}
		return leftNumber < rightNumber
	})
}
