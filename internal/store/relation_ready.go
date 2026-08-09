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

	"aira/internal/domain"
)

type storedRelation struct {
	Owner    string
	Path     string
	Relation domain.Relation
}

type ReadyRecord struct {
	Ticket   TicketRecord          `json:"ticket"`
	Ready    bool                  `json:"ready"`
	Blockers []domain.RelationView `json:"blockers,omitempty"`
	Verdict  string                `json:"verdict"`
	Findings []CheckFinding        `json:"findings,omitempty"`
}

func relationSubject(r domain.Relation) string {
	return r.From + ":" + string(r.Kind) + ":" + r.To
}

func relationEndpointKey(project, id string) string { return project + "\x00" + id }

func scanStoredRelationsAt(root, worktreeID string) ([]storedRelation, map[string]domain.Ticket, []CheckFinding, error) {
	tickets, scanFindings, err := scanTickets(root, worktreeID)
	if err != nil {
		return nil, nil, nil, err
	}
	byID := make(map[string]domain.Ticket, len(tickets))
	for _, ticket := range tickets {
		byID[relationEndpointKey(ticket.Ticket.Project, ticket.Ticket.ID)] = ticket.Ticket
	}
	findings := append([]CheckFinding(nil), scanFindings...)
	var relations []storedRelation
	for _, ticket := range tickets {
		for _, relation := range ticket.Ticket.Relations {
			if !domain.ValidRelationKind(relation.Kind) || relation.From == relation.To {
				findings = append(findings, CheckFinding{Code: "E_RELATION_INVALID", Subject: relationSubject(relation), Message: "relation kind or endpoints are invalid", Kind: "fail"})
				continue
			}
			fromKey := relationEndpointKey(ticket.Ticket.Project, relation.From)
			toKey := relationEndpointKey(ticket.Ticket.Project, relation.To)
			if _, ok := byID[fromKey]; !ok {
				if endpointHasOtherProject(byID, relation.From, ticket.Ticket.Project) {
					findings = append(findings, CheckFinding{Code: "E_CROSS_PROJECT_RELATION", Subject: relationSubject(relation), Message: "relation source ticket belongs to another project", Kind: "fail"})
				} else {
					findings = append(findings, CheckFinding{Code: "E_RELATION_TARGET_MISSING", Subject: relation.From, Message: "relation source ticket is missing", Kind: "fail"})
				}
			}
			if _, ok := byID[toKey]; !ok {
				if endpointHasOtherProject(byID, relation.To, ticket.Ticket.Project) {
					findings = append(findings, CheckFinding{Code: "E_CROSS_PROJECT_RELATION", Subject: relationSubject(relation), Message: "relation target ticket belongs to another project", Kind: "fail"})
				} else {
					findings = append(findings, CheckFinding{Code: "E_RELATION_TARGET_MISSING", Subject: relation.To, Message: "relation target ticket is missing", Kind: "fail"})
				}
			}
			if domain.CanonicalRelationOwner(relation.From, relation.To) != ticket.Ticket.ID {
				findings = append(findings, CheckFinding{Code: "E_RELATION_INVALID", Subject: relationSubject(relation), Message: "relation is not stored on its canonical lower-ID ticket", Kind: "fail"})
			}
			relations = append(relations, storedRelation{Owner: ticket.Ticket.ID, Path: ticket.Path, Relation: relation})
		}
	}
	return relations, byID, findings, nil
}

func endpointHasOtherProject(byID map[string]domain.Ticket, id, project string) bool {
	for key, ticket := range byID {
		if ticket.ID == id && key != relationEndpointKey(project, id) {
			return true
		}
	}
	return false
}

func (s *Store) scanStoredRelations() ([]storedRelation, map[string]domain.Ticket, []CheckFinding, error) {
	return scanStoredRelationsAt(s.root, s.worktreeID)
}

func relationIntegrityError(findings []CheckFinding) error {
	for _, finding := range findings {
		if finding.Code == "E_RELATION_TARGET_MISSING" || finding.Code == "E_RELATION_INVALID" || finding.Code == "E_CROSS_PROJECT_RELATION" || finding.Code == "E_RELATION_UNOBSERVABLE" {
			return fmt.Errorf("%s: %s", finding.Code, finding.Message)
		}
	}
	return nil
}

func (s *Store) Link(ctx context.Context, from string, kind domain.RelationKind, to string) (EventKey, error) {
	if err := domain.ValidateID(from); err != nil {
		return EventKey{}, err
	}
	if err := domain.ValidateID(to); err != nil {
		return EventKey{}, err
	}
	if !kind.IsForward() {
		return EventKey{}, errors.New("E_RELATION_INVALID: inverse relation kinds are query-only")
	}
	fromTicket, err := s.Get(from)
	if err != nil {
		return EventKey{}, err
	}
	toTicket, err := s.Get(to)
	if err != nil {
		if ErrorCode(err) == "E_NOT_FOUND" {
			return EventKey{}, errors.New("E_RELATION_TARGET_MISSING: relation target ticket does not exist")
		}
		return EventKey{}, err
	}
	if fromTicket.Ticket.Project != s.projectSlug || toTicket.Ticket.Project != s.projectSlug {
		return EventKey{}, errors.New("E_CROSS_PROJECT_RELATION: relation endpoints are not in this project")
	}
	if from == to {
		return EventKey{}, errors.New("E_RELATION_INVALID: relation endpoints must differ")
	}
	relations, _, findings, err := s.scanStoredRelations()
	if err != nil {
		return EventKey{}, err
	}
	for _, finding := range findings {
		if finding.Code == "E_RELATION_INVALID" && strings.Contains(finding.Message, "canonical") {
			// A pre-existing wrong-side copy is an integrity failure; link never
			// repairs it implicitly.
			for _, relation := range relations {
				if relation.Relation.From == from && relation.Relation.To == to && relation.Relation.Kind == kind {
					return EventKey{}, errors.New("E_RELATION_INVALID: existing relation is stored on the wrong side")
				}
			}
		}
	}
	for _, relation := range relations {
		if relation.Relation.From == from && relation.Relation.To == to && relation.Relation.Kind == kind {
			if relation.Owner == domain.CanonicalRelationOwner(from, to) {
				return EventKey{}, errors.New("E_RELATION_EXISTS: relation already exists")
			}
			return EventKey{}, errors.New("E_RELATION_INVALID: existing relation is stored on the wrong side")
		}
	}
	owner := domain.CanonicalRelationOwner(from, to)
	path := s.ticketPath(owner)
	data, err := readRegularTicket(path)
	if err != nil {
		return EventKey{}, err
	}
	ticket, body, err := domain.ParseTicket(data)
	if err != nil {
		return EventKey{}, err
	}
	ticket.Relations = append(ticket.Relations, domain.Relation{Kind: kind, From: from, To: to})
	newData, err := domain.RenderTicket(ticket, body)
	if err != nil {
		return EventKey{}, err
	}
	target := relationSubject(domain.Relation{Kind: kind, From: from, To: to})
	intent, err := s.preparePathMutationEvent(ctx, path, digestBytes(data), newData, "relation.add", target)
	if err != nil {
		return EventKey{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return EventKey{}, err
	}
	return EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
}

func (s *Store) Unlink(ctx context.Context, from string, kind domain.RelationKind, to string) (EventKey, error) {
	if err := domain.ValidateID(from); err != nil {
		return EventKey{}, err
	}
	if err := domain.ValidateID(to); err != nil {
		return EventKey{}, err
	}
	if !kind.IsForward() {
		return EventKey{}, errors.New("E_RELATION_INVALID: inverse relation kinds are query-only")
	}
	if _, err := s.Get(from); err != nil {
		return EventKey{}, err
	}
	if _, err := s.Get(to); err != nil {
		return EventKey{}, errors.New("E_RELATION_TARGET_MISSING: relation target ticket does not exist")
	}
	owner := domain.CanonicalRelationOwner(from, to)
	path := s.ticketPath(owner)
	data, err := readRegularTicket(path)
	if err != nil {
		return EventKey{}, err
	}
	ticket, body, err := domain.ParseTicket(data)
	if err != nil {
		return EventKey{}, err
	}
	found := false
	filtered := ticket.Relations[:0]
	for _, relation := range ticket.Relations {
		if relation.Kind == kind && relation.From == from && relation.To == to {
			found = true
			continue
		}
		filtered = append(filtered, relation)
	}
	if !found {
		return EventKey{}, errors.New("E_RELATION_INVALID: relation does not exist on canonical ticket")
	}
	ticket.Relations = filtered
	newData, err := domain.RenderTicket(ticket, body)
	if err != nil {
		return EventKey{}, err
	}
	target := relationSubject(domain.Relation{Kind: kind, From: from, To: to})
	intent, err := s.preparePathMutationEvent(ctx, path, digestBytes(data), newData, "relation.remove", target)
	if err != nil {
		return EventKey{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return EventKey{}, err
	}
	return EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
}

func (s *Store) Relations(id string) ([]domain.RelationView, error) {
	if err := domain.ValidateID(id); err != nil {
		return nil, err
	}
	if _, err := readRegularTicket(s.ticketPath(id)); err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("E_NOT_FOUND: selector matched no tickets")
		}
		return nil, err
	}
	return s.derivedRelationViews(id)
}

func (s *Store) derivedRelationViews(id string) ([]domain.RelationView, error) {
	relations, _, findings, err := s.scanStoredRelations()
	if err != nil {
		return nil, err
	}
	if err := relationIntegrityError(findings); err != nil {
		return nil, err
	}
	views := make([]domain.RelationView, 0)
	for _, relation := range relations {
		if relation.Relation.From == id {
			views = append(views, domain.RelationView{Kind: relation.Relation.Kind, From: id, To: relation.Relation.To})
		} else if relation.Relation.To == id {
			views = append(views, domain.RelationView{Kind: relation.Relation.Kind.Inverse(), From: id, To: relation.Relation.From})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Kind != views[j].Kind {
			return views[i].Kind < views[j].Kind
		}
		if views[i].From != views[j].From {
			return domain.IDLess(views[i].From, views[j].From)
		}
		return domain.IDLess(views[i].To, views[j].To)
	})
	return views, nil
}

func relationIndexKey(relation storedRelation) string {
	return string(relation.Relation.Kind) + "\x00" + relation.Relation.From + "\x00" + relation.Relation.To
}

func relationIndexRecordKey(relation storedRelation) string {
	return relationIndexKey(relation) + "\x00" + relation.Owner + "\x00" + filepath.Clean(relation.Path)
}

func (s *Store) indexedRelations() ([]storedRelation, error) {
	rows, err := s.db.Query(`SELECT kind, from_id, to_id, canonical_file FROM relations WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []storedRelation
	for rows.Next() {
		var kind, from, to, path string
		if err := rows.Scan(&kind, &from, &to, &path); err != nil {
			return nil, err
		}
		result = append(result, storedRelation{Owner: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), Path: path, Relation: domain.Relation{Kind: domain.RelationKind(kind), From: from, To: to}})
	}
	return result, rows.Err()
}

func (s *Store) relationIndexDivergence() ([]CheckFinding, error) {
	canonical, _, _, err := scanStoredRelationsAt(s.root, s.worktreeID)
	if err != nil {
		return nil, err
	}
	indexed, err := s.indexedRelations()
	if err != nil {
		return nil, err
	}
	expected := make(map[string]storedRelation, len(canonical))
	for _, relation := range canonical {
		expected[relationIndexRecordKey(relation)] = relation
	}
	actual := make(map[string]storedRelation, len(indexed))
	for _, relation := range indexed {
		actual[relationIndexRecordKey(relation)] = relation
	}
	var findings []CheckFinding
	for key, relation := range expected {
		if _, ok := actual[key]; !ok {
			findings = append(findings, CheckFinding{Code: "E_RELATION_INDEX_DIVERGENCE", Subject: relationSubject(relation.Relation),
				Message: "canonical relation is missing from the derived index", Kind: relationIndexFindingKind(relation)})
		}
	}
	for key, relation := range actual {
		if _, ok := expected[key]; !ok {
			findings = append(findings, CheckFinding{Code: "E_RELATION_INDEX_DIVERGENCE", Subject: relationSubject(relation.Relation),
				Message: "derived relation index row is absent from canonical files", Kind: relationIndexFindingKind(relation)})
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

func relationIndexFindingKind(relation storedRelation) string {
	data, err := readRegularTicket(relation.Path)
	if err != nil {
		return "fail"
	}
	if _, _, err := domain.ParseTicket(data); err != nil {
		return "fail"
	}
	return "warning"
}

func workable(status domain.Status) bool {
	return status == domain.StatusPlanned || status == domain.StatusInProgress
}

func satisfied(status domain.Status) bool {
	return status == domain.StatusDone || status == domain.StatusRetired || status == domain.StatusSuperseded
}

func (s *Store) Ready(selector string) ([]ReadyRecord, error) {
	sel, err := parseSelector(selector)
	if err != nil {
		return nil, err
	}
	hasSelector := strings.TrimSpace(selector) != ""
	if hasSelector && sel.ExactID == "" {
		return nil, errors.New("E_SELECTOR_INVALID: ready requires an exact ID or file anchor")
	}

	tickets, scanFindings, err := scanTickets(s.root, s.worktreeID)
	if err != nil {
		return nil, err
	}
	indexed, err := s.indexedDigests()
	if err != nil {
		return nil, err
	}
	rows := make([]TicketRecord, 0, len(tickets))
	for _, scanned := range tickets {
		if hasSelector && (scanned.Ticket.ID != sel.ExactID || (sel.ExactPath != "" && filepath.ToSlash(repoPath(s.root, scanned.Path)) != sel.ExactPath)) {
			continue
		}
		row := TicketRecord{Ticket: scanned.Ticket, Body: scanned.Body, Path: repoPath(s.root, scanned.Path), WorktreeID: s.worktreeID, Digest: scanned.Digest}
		if indexed[scanned.Ticket.ID] != scanned.Digest {
			row.Warnings = []string{"W_STALE_INDEX"}
		}
		rows = append(rows, row)
	}
	if hasSelector && len(rows) == 0 {
		for _, finding := range scanFindings {
			if findingPathMatchesSelector(finding.Subject, sel) {
				rows = append(rows, TicketRecord{Path: finding.Subject, WorktreeID: s.worktreeID})
				break
			}
		}
		if len(rows) == 0 {
			return nil, errors.New("E_NOT_FOUND: selector matched no tickets")
		}
	}

	relations, byID, relationFindings, err := scanStoredRelationsAt(s.root, s.worktreeID)
	if err != nil {
		return nil, err
	}
	indexFindings, err := s.relationIndexDivergence()
	if err != nil {
		return nil, err
	}
	for i := range indexFindings {
		// The canonical files determine readiness. A stale derived row is
		// surfaced here as a warning, but must not make a canonical-ready
		// ticket fail or become blocked.
		if indexFindings[i].Kind != "fail" {
			indexFindings[i].Kind = "warning"
		}
	}
	findings := append(append(append([]CheckFinding(nil), scanFindings...), relationFindings...), indexFindings...)
	result := make([]ReadyRecord, 0, len(rows)+len(findings))
	for _, row := range rows {
		rowFindings := findingsForReadyRow(row, findings, relations)
		hasFailFinding := false
		for _, finding := range rowFindings {
			if finding.Kind == "fail" {
				hasFailFinding = true
				break
			}
		}
		blockers := make([]domain.RelationView, 0)
		if row.Ticket.ID != "" {
			for _, relation := range relations {
				if relation.Relation.Kind != domain.RelationBlocks || relation.Relation.To != row.Ticket.ID {
					continue
				}
				view := domain.RelationView{Kind: domain.RelationBlockedBy, From: row.Ticket.ID, To: relation.Relation.From}
				prerequisite, ok := byID[relationEndpointKey(row.Ticket.Project, relation.Relation.From)]
				if !ok {
					rowFindings = appendUniqueFinding(rowFindings, CheckFinding{Code: "E_RELATION_TARGET_MISSING", Subject: row.Ticket.ID, Message: "blocked-by prerequisite is missing", Kind: "fail"})
				} else if !satisfied(prerequisite.Status) {
					blockers = append(blockers, view)
				}
			}
		}
		isReady := row.Ticket.ID != "" && workable(row.Ticket.Status) && !row.Ticket.Hold && len(blockers) == 0 && !hasFailFinding
		verdict := "pass"
		if hasFailFinding {
			verdict = "fail"
		}
		item := ReadyRecord{Ticket: row, Ready: isReady, Blockers: blockers, Verdict: verdict, Findings: rowFindings}
		if hasSelector || isReady || len(rowFindings) > 0 {
			result = append(result, item)
		}
	}
	if !hasSelector {
		for _, finding := range findings {
			if finding.Code == "E_RELATION_INVALID" || finding.Code == "E_CROSS_PROJECT_RELATION" || finding.Code == "E_RELATION_UNOBSERVABLE" || finding.Code == "E_CONFIG_INVALID" {
				if !findingAlreadyRepresented(result, finding) {
					result = append(result, ReadyRecord{Ticket: TicketRecord{Path: finding.Subject, WorktreeID: s.worktreeID}, Ready: false, Verdict: "fail", Findings: []CheckFinding{finding}})
				}
			}
		}
	}
	sortReadyRecords(result)
	return result, nil
}

func findingPathMatchesSelector(path string, sel selector) bool {
	if sel.ExactPath != "" {
		return filepath.ToSlash(path) == sel.ExactPath
	}
	return filepath.Base(filepath.FromSlash(path)) == sel.ExactID+".md"
}

func findingsForReadyRow(row TicketRecord, findings []CheckFinding, relations []storedRelation) []CheckFinding {
	var result []CheckFinding
	for _, finding := range findings {
		relatedMissingTarget := false
		if finding.Code == "E_RELATION_TARGET_MISSING" && row.Ticket.ID != "" {
			for _, relation := range relations {
				if relation.Owner == row.Ticket.ID && (relation.Relation.From == finding.Subject || relation.Relation.To == finding.Subject) {
					relatedMissingTarget = true
					break
				}
			}
		}
		relatedEndpoint := relationFindingHasEndpoint(finding, row.Ticket.ID)
		if finding.Subject == row.Path || finding.Subject == row.Ticket.ID || relatedMissingTarget || relatedEndpoint {
			result = appendUniqueFinding(result, finding)
		}
	}
	return result
}

func relationFindingHasEndpoint(finding CheckFinding, id string) bool {
	if finding.Code != "E_CROSS_PROJECT_RELATION" && finding.Code != "E_RELATION_INVALID" && finding.Code != "E_RELATION_INDEX_DIVERGENCE" {
		return false
	}
	parts := strings.Split(finding.Subject, ":")
	return len(parts) == 3 && (parts[0] == id || parts[2] == id)
}

func appendUniqueFinding(findings []CheckFinding, finding CheckFinding) []CheckFinding {
	for _, existing := range findings {
		if existing.Code == finding.Code && existing.Subject == finding.Subject && existing.Message == finding.Message {
			return findings
		}
	}
	return append(findings, finding)
}

func findingAlreadyRepresented(rows []ReadyRecord, finding CheckFinding) bool {
	for _, row := range rows {
		for _, existing := range row.Findings {
			if existing.Code == finding.Code && existing.Subject == finding.Subject && existing.Message == finding.Message {
				return true
			}
		}
	}
	return false
}

func sortReadyRecords(rows []ReadyRecord) {
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i].Ticket.Ticket.ID, rows[j].Ticket.Ticket.ID
		if left == "" || right == "" {
			if left == right {
				return rows[i].Ticket.Path < rows[j].Ticket.Path
			}
			return left != ""
		}
		return domain.IDLess(left, right)
	})
}

func (s *Store) relationFindings() ([]CheckFinding, error) {
	registry, err := readRegistry(s.registryPath)
	if err != nil {
		return nil, err
	}
	entries, err := discoverWorktrees(s.root, s.projectID, registry)
	if err != nil {
		return nil, err
	}
	var findings []CheckFinding
	for _, entry := range entries {
		_, _, entryFindings, err := scanStoredRelationsAt(entry.Root, entry.WorktreeID)
		if err != nil {
			return nil, err
		}
		findings = append(findings, entryFindings...)
	}
	return findings, nil
}

func (s *Store) leaseFileOrphanWarnings(ctx context.Context, report *CheckReport) error {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ticket_id, generation, holder_token_hash, boot_id,
        last_heartbeat_mono_ns, ttl_ns, actor, worktree_id FROM leases WHERE project_id=? AND state='held'`, s.projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ticketID string
		var row leaseRow
		if err := rows.Scan(&ticketID, &row.generation, &row.holderTokenHash, &row.bootID, &row.lastHeartbeatMonoNS, &row.ttlNS, &row.actor, &row.worktree); err != nil {
			return err
		}
		lease, err := leaseFromRow(ticketID, row)
		if err != nil {
			return err
		}
		held, _ := lease.Held()
		if !held.IsLive(bootID, monoNS) {
			continue
		}
		var active int
		var root string
		err = s.db.QueryRowContext(ctx, `SELECT active, root FROM worktrees WHERE project_id=? AND worktree_id=?`, s.projectID, held.Worktree).Scan(&active, &root)
		if errors.Is(err, sql.ErrNoRows) || active == 0 || fileMissing(root) {
			addWarning(report, CheckFinding{Code: "W_ORPHAN_WORKTREE", Subject: ticketID, Message: "live lease holder worktree is missing or inactive", Kind: "warning"}, "orphan-worktree")
		} else if err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) relationPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
