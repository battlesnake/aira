package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"aira/internal/domain"
)

// AIRA-237 Task 2 — id-accepting ticket import.
//
// `aira import --tickets <file.jsonl>` adopts an externally-authored backlog as
// coordination tickets WITHOUT renaming: each bare id (BL-123) is composed with
// the project id_prefix (FEE-BL-123, Task 1) and PRESERVED, never minted. It is
// modelled on ImportRequirements but differs deliberately in three ways the
// plan-review earned:
//   - READ-MERGE, never render-from-row: a re-import overlays only the backlog-
//     owned fields (title/status/severity/body/labels) onto the existing file and
//     digests POST-merge, so aira-owned coordination state (the Relations slice,
//     Hold/Assignee/Milestone, the lease and worktree-binding) survives.
//   - It ACCEPTS a cross-worktree pre-allocated (state='allocated') mint and
//     materialises it here (path rewritten in markTicketMaterialised), rather than
//     refusing on a path mismatch the way import_requirements.go does.
//   - Status is force-set through the file-write path, bypassing the transition
//     graph UpdateTicketContent enforces (the backlog is authoritative for status).

// bareTicketIDPattern is the id shape an import row must carry: a single bare
// <PREFIX>-<number>. It rejects an already-composed id (FEE-BL-1, two hyphens) —
// import is strict, unlike interactive input which canonicaliser-prepends — and
// (until Task 3) a split-suffix tail (BL-10a).
var bareTicketIDPattern = regexp.MustCompile(`^[A-Z]{2,}-[1-9][0-9]*$`)

// ImportTicketError is one row's (or one link's) named refusal, counted in the
// summary and never a silent skip.
type ImportTicketError struct {
	Line  int    `json:"line"`
	ID    string `json:"id,omitempty"`
	Error string `json:"error"`
}

// ImportTicketsSummary reports every row's outcome. Ids are the STORED (compound)
// keys — the summary is an operator-facing record of what was written, not a
// human display projection. AbsentFromBatch is a REPORT of previously-imported
// ids missing from this batch; the importer never auto-retires them.
//
// Unchanged refines the plan's created/refreshed/absent_from_batch/errored
// enumeration: a re-import whose post-merge content is byte-identical writes
// nothing and is reported here, which is the mutation-sensitive proof that the
// digest is computed POST-merge.
type ImportTicketsSummary struct {
	Created         []string            `json:"created"`
	Refreshed       []string            `json:"refreshed"`
	Unchanged       []string            `json:"unchanged"`
	AbsentFromBatch []string            `json:"absent_from_batch"`
	Errored         []ImportTicketError `json:"errored"`
	Total           int                 `json:"total"`
}

type rawTicketRow struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Status   string          `json:"status"`
	Kind     string          `json:"kind"`
	Severity string          `json:"severity"`
	Body     string          `json:"body"`
	Labels   []string        `json:"labels"`
	Links    []rawTicketLink `json:"links"`
}

type rawTicketLink struct {
	Kind string `json:"kind"`
	To   string `json:"to"`
}

// importedTicket is a parsed, field-validated, canonicalised row. Ids are
// compound (composed with id_prefix). Links carry canonical endpoints.
type importedTicket struct {
	Line     int
	ID       string
	Prefix   string
	Number   int64
	Title    string
	Status   domain.Status
	Kind     domain.Kind
	Severity domain.Severity
	Body     string
	Labels   []string
	Links    []importedLink
}

type importedLink struct {
	Line int
	Kind domain.RelationKind
	From string
	To   string
}

type ticketAllocation struct {
	WorktreeID string
	State      string
	Path       string
	Seq        int64
	Kind       string
}

// ImportTickets opens and imports a JSONL ticket batch. A missing file is a
// stable E_NOT_FOUND.
func (s *Store) ImportTickets(ctx context.Context, path string, strict bool) (ImportTicketsSummary, error) {
	if strings.TrimSpace(path) == "" {
		return ImportTicketsSummary{}, errors.New("E_NOT_FOUND: import requires a file path")
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ImportTicketsSummary{}, fmt.Errorf("E_NOT_FOUND: import file %q does not exist", path)
	}
	if err != nil {
		return ImportTicketsSummary{}, fmt.Errorf("E_IMPORT_INVALID: cannot read import file %q: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return ImportTicketsSummary{}, fmt.Errorf("E_IMPORT_INVALID: cannot read import file %q: %w", path, err)
	}
	return s.ImportTicketsBytes(ctx, data, strict)
}

// ImportTicketsBytes imports caller-read JSONL without resolving a path in the
// daemon process.
func (s *Store) ImportTicketsBytes(ctx context.Context, data []byte, strict bool) (ImportTicketsSummary, error) {
	rows, rowErrors, total := s.parseTicketRows(data)

	// Batch id-set: an endpoint is resolvable if it is a row in THIS batch or
	// already materialised on disk (pass-0 probe below).
	batch := make(map[string]bool, len(rows))
	for _, row := range rows {
		batch[row.ID] = true
	}

	// Pass 0 — validate every link endpoint BEFORE any write (mirrors
	// ImportFindings' pre-write probe), so strict mode can abort with zero
	// writes. A bad link is a NAMED error removed from its row; the row's own
	// (valid) fields are still imported in non-strict mode.
	var errored []ImportTicketError
	errored = append(errored, rowErrors...)
	for i := range rows {
		kept := rows[i].Links[:0]
		for _, link := range rows[i].Links {
			if err := s.probeImportedLink(rows[i], link, batch); err != nil {
				errored = append(errored, ImportTicketError{Line: link.Line, ID: rows[i].ID, Error: err.Error()})
				continue
			}
			kept = append(kept, link)
		}
		rows[i].Links = kept
	}

	if strict && len(errored) > 0 {
		// Zero writes: nothing above this point has mutated durable state.
		return ImportTicketsSummary{}, errors.New(errored[0].Error)
	}

	summary := ImportTicketsSummary{
		Created: []string{}, Refreshed: []string{}, Unchanged: []string{},
		AbsentFromBatch: []string{}, Errored: errored, Total: total,
	}

	// Pass 1 — create/refresh each parsed row.
	maxima := make(map[string]int64)
	for _, row := range rows {
		outcome, err := s.importTicketRow(ctx, row)
		if err != nil {
			if strict {
				return ImportTicketsSummary{}, err
			}
			summary.Errored = append(summary.Errored, ImportTicketError{Line: row.Line, ID: row.ID, Error: err.Error()})
			continue
		}
		switch outcome {
		case "created":
			summary.Created = append(summary.Created, row.ID)
		case "refreshed":
			summary.Refreshed = append(summary.Refreshed, row.ID)
		case "unchanged":
			summary.Unchanged = append(summary.Unchanged, row.ID)
		default:
			return ImportTicketsSummary{}, fmt.Errorf("E_INTERNAL: unknown ticket import outcome %q", outcome)
		}
		if row.Number > maxima[row.Prefix] {
			maxima[row.Prefix] = row.Number
		}
	}
	if err := s.advanceImportedTicketCounters(ctx, maxima); err != nil {
		return ImportTicketsSummary{}, err
	}

	// Pass 2 — apply links, grouped by their canonical (lower-id) owner file so
	// each owner is read-merged and rewritten ONCE, not once per Link() call.
	owners := make(map[string][]importedLink)
	var ownerOrder []string
	for _, row := range rows {
		for _, link := range row.Links {
			owner := domain.CanonicalRelationOwner(link.From, link.To)
			if _, seen := owners[owner]; !seen {
				ownerOrder = append(ownerOrder, owner)
			}
			owners[owner] = append(owners[owner], link)
		}
	}
	for _, owner := range ownerOrder {
		if err := s.applyImportedLinks(ctx, owner, owners[owner]); err != nil {
			if strict {
				return ImportTicketsSummary{}, err
			}
			summary.Errored = append(summary.Errored, ImportTicketError{ID: owner, Error: err.Error()})
		}
	}

	// Disappear = REPORT. A previously-imported id (one bearing a journaled
	// ticket.import event) absent from this batch is surfaced, never retired. A
	// natively-created ticket has no such event and is therefore never here.
	absent, err := s.absentImportedTickets(batch)
	if err != nil {
		return ImportTicketsSummary{}, err
	}
	summary.AbsentFromBatch = absent
	return summary, nil
}

// parseTicketRows decodes one JSON object per non-blank line. A row that fails
// to parse, carries an invalid field, or duplicates an id already seen is a
// named error (whole row rejected); everything else is a canonicalised
// importedTicket. total counts every non-blank line.
func (s *Store) parseTicketRows(data []byte) ([]importedTicket, []ImportTicketError, int) {
	var rows []importedTicket
	var errs []ImportTicketError
	seen := make(map[string]int)
	total := 0
	for i, line := range strings.Split(string(data), "\n") {
		lineNum := i + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
		row, err := s.parseTicketRow(line, lineNum)
		if err != nil {
			errs = append(errs, ImportTicketError{Line: lineNum, Error: err.Error()})
			continue
		}
		if prev, dup := seen[row.ID]; dup {
			errs = append(errs, ImportTicketError{Line: lineNum, ID: row.ID,
				Error: fmt.Sprintf("E_IMPORT_INVALID: duplicate ticket id %q (first at line %d)", row.ID, prev)})
			continue
		}
		seen[row.ID] = lineNum
		rows = append(rows, row)
	}
	return rows, errs, total
}

func (s *Store) parseTicketRow(line string, lineNum int) (importedTicket, error) {
	var raw rawTicketRow
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: JSON decode error: %v", lineNum, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: trailing content after JSON object", lineNum)
	}

	id, prefix, number, err := s.canonicaliseImportID(raw.ID)
	if err != nil {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: %v", lineNum, err)
	}
	if registered, ok := s.prefixes[prefix]; !ok || registered != kindTicket {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: ticket %s uses unowned or non-ticket prefix %q", lineNum, s.displayID(id), prefix)
	}

	status := domain.Status(strings.TrimSpace(raw.Status))
	if !validImportStatus(status) {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: invalid status %q; allowed: %s", lineNum, raw.Status, strings.Join(domain.AllowedStatusStrings(), ", "))
	}

	kind := domain.Kind(strings.TrimSpace(raw.Kind))
	if kind == "" {
		// Per-file default: a backlog row carries no kind → chore. (The extractor
		// sets requirement-work for REQUIREMENTS.md rows; kind is create-only —
		// NOT in the re-import overlay set.)
		kind = domain.KindChore
	}
	if !domain.ValidKind(kind) {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: invalid kind %q; allowed: %s", lineNum, raw.Kind, strings.Join(domain.AllowedKindStrings(), ", "))
	}

	severity, err := importSeverity(raw.Severity)
	if err != nil {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: %v", lineNum, err)
	}

	links, err := s.parseImportedLinks(id, raw.Links, lineNum)
	if err != nil {
		return importedTicket{}, err
	}

	row := importedTicket{
		Line: lineNum, ID: id, Prefix: prefix, Number: number,
		Title: raw.Title, Status: status, Kind: kind, Severity: severity,
		Body: raw.Body, Labels: raw.Labels, Links: links,
	}
	// Dry render as a fresh ticket to surface Validate errors (empty title,
	// non-lowercase/unsorted labels, bad enum) BEFORE any write — the same
	// Validate the create render will hit.
	if _, err := domain.RenderTicket(s.freshImportTicket(row), row.Body); err != nil {
		return importedTicket{}, fmt.Errorf("E_IMPORT_INVALID: line %d: %v", lineNum, err)
	}
	return row, nil
}

// canonicaliseImportID enforces the bare id contract, composes the project
// prefix, and returns the compound id plus its (composed) prefix and number.
func (s *Store) canonicaliseImportID(raw string) (id, prefix string, number int64, err error) {
	raw = strings.TrimSpace(raw)
	if !bareTicketIDPattern.MatchString(raw) {
		return "", "", 0, fmt.Errorf("import ids must be a bare PREFIX-N (got %q)", raw)
	}
	id = s.canonicalID(raw)
	p, n := splitTicketID(id)
	if n < 1 {
		return "", "", 0, fmt.Errorf("ticket id %q has an invalid number", raw)
	}
	return id, p, int64(n), nil
}

func (s *Store) parseImportedLinks(from string, raw []rawTicketLink, lineNum int) ([]importedLink, error) {
	links := make([]importedLink, 0, len(raw))
	for _, r := range raw {
		to := strings.TrimSpace(r.To)
		if !bareTicketIDPattern.MatchString(to) {
			return nil, fmt.Errorf("E_IMPORT_INVALID: line %d: link target %q must be a bare PREFIX-N", lineNum, r.To)
		}
		links = append(links, importedLink{Line: lineNum, Kind: domain.RelationKind(strings.TrimSpace(r.Kind)), From: from, To: s.canonicalID(to)})
	}
	return links, nil
}

// probeImportedLink validates one link without writing: forward kind, distinct
// endpoints, and a target that is either in the batch or already on disk. Only
// E_NOT_FOUND counts as "missing"; any other read failure is itself the error.
func (s *Store) probeImportedLink(row importedTicket, link importedLink, batch map[string]bool) error {
	if !link.Kind.IsForward() {
		return fmt.Errorf("E_IMPORT_INVALID: link kind %q is not a writable forward relation", link.Kind)
	}
	if link.To == link.From {
		return errors.New("E_RELATION_INVALID: relation endpoints must differ")
	}
	if batch[link.To] {
		return nil
	}
	if _, err := s.exactRecord(link.To, ""); err != nil {
		if ErrorCode(err) == "E_NOT_FOUND" {
			return fmt.Errorf("E_RELATION_TARGET_MISSING: relation target %s does not exist", s.displayID(link.To))
		}
		return fmt.Errorf("E_IMPORT_INVALID: relation target %s is unreadable: %v", s.displayID(link.To), err)
	}
	return nil
}

// importTicketRow creates or read-merge-refreshes one row. It PRESERVES aira
// coordination state on re-import and digests POST-merge.
func (s *Store) importTicketRow(ctx context.Context, row importedTicket) (string, error) {
	path := s.ticketPath(row.ID)
	localDigest, err := fileDigest(path)
	if err != nil {
		return "", err
	}
	alloc, allocExists, err := s.findTicketAllocation(ctx, row.Prefix, row.Number)
	if err != nil {
		return "", err
	}

	// Refuse a genuine collision: a materialised/recovered allocation whose path
	// is a DIFFERENT existing file (do NOT copy import_requirements.go's blanket
	// path-equality refusal — a state='allocated' row from any worktree is a
	// pre-allocated mint to materialise here).
	if allocExists && alloc.State != "allocated" && alloc.Path != path {
		if _, statErr := os.Lstat(alloc.Path); statErr == nil {
			return "", fmt.Errorf("E_IMPORT_INVALID: ticket %s already materialised at a different path %q", s.displayID(row.ID), alloc.Path)
		}
	}

	if localDigest != "" {
		return s.refreshImportedTicket(ctx, row, path, localDigest, alloc, allocExists)
	}
	return s.createImportedTicket(ctx, row, path)
}

// createImportedTicket materialises a brand-new (or cross-worktree pre-allocated)
// ticket, preserving the id.
func (s *Store) createImportedTicket(ctx context.Context, row importedTicket, path string) (string, error) {
	data, err := domain.RenderTicket(s.freshImportTicket(row), row.Body)
	if err != nil {
		return "", err
	}
	intent, receipt, err := s.registerImportedTicket(ctx, row, data, path)
	if err != nil {
		return "", err
	}
	if err := s.appendReceiptIfMissing(receipt); err != nil {
		return "", err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return "", err
	}
	return "created", nil
}

// refreshImportedTicket read-merges the backlog-owned fields onto the existing
// file. aira-owned frontmatter (Relations, Hold, Assignee, Milestone) is left
// untouched; the digest is computed POST-merge so an unchanged re-import is a
// true no-op.
func (s *Store) refreshImportedTicket(ctx context.Context, row importedTicket, path, localDigest string, alloc ticketAllocation, allocExists bool) (string, error) {
	data, outcome, err := readRegularTicket(path)
	if outcome == scanReadInconclusive {
		return "", indexUnestablishedError()
	}
	if err != nil {
		return "", err
	}
	existing, _, err := domain.ParseTicket(data)
	if err != nil {
		return "", err
	}
	if existing.ID != row.ID {
		return "", fmt.Errorf("E_IMPORT_INVALID: ticket %s identity changed at existing path", s.displayID(row.ID))
	}

	// Overlay ONLY the backlog-authoritative fields. Kind is create-only and is
	// NOT overlaid; Relations/Hold/Assignee/Milestone are aira-owned and preserved.
	existing.Title = row.Title
	existing.Status = row.Status
	existing.Severity = row.Severity
	existing.Labels = row.Labels
	newData, err := domain.RenderTicket(existing, row.Body)
	if err != nil {
		return "", err
	}

	// Unchanged only when the content is byte-identical AND the allocation is not
	// stuck mid-materialise. A file that exists with a state='allocated' row is a
	// crash window; route it through the intent path so markMaterialised finishes
	// the transition (materialiseIntent no-ops the write when nothing changed).
	if digestBytes(newData) == localDigest && !(allocExists && alloc.State == "allocated") {
		return "unchanged", nil
	}

	intent, err := s.preparePathMutationEventKind(ctx, path, localDigest, newData, "ticket.import", row.ID, IntentKindTicketFile)
	if err != nil {
		return "", err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return "", err
	}
	return "refreshed", nil
}

// freshImportTicket builds the domain ticket for a create/dry-render: a fresh
// coordination shell with empty aira-owned fields and the backlog fields set.
func (s *Store) freshImportTicket(row importedTicket) domain.Ticket {
	labels := row.Labels
	if labels == nil {
		labels = []string{}
	}
	return domain.Ticket{
		Schema: 1, ID: row.ID, Project: s.projectSlug, Title: row.Title,
		Status: row.Status, Kind: row.Kind, Severity: row.Severity,
		Labels: labels, Relations: []domain.Relation{},
	}
}

func (s *Store) findTicketAllocation(ctx context.Context, prefix string, number int64) (ticketAllocation, bool, error) {
	var a ticketAllocation
	err := s.db.QueryRowContext(ctx, `SELECT worktree_id, state, path, seq, kind
        FROM allocations WHERE project_id=? AND prefix=? AND number=?`, s.projectID, prefix, number).Scan(
		&a.WorktreeID, &a.State, &a.Path, &a.Seq, &a.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ticketAllocation{}, false, nil
	}
	if err != nil {
		return ticketAllocation{}, false, err
	}
	return a, true, nil
}

// registerImportedTicket writes the crash-safe allocation/outbox/event/counter
// row set for a new materialisation. A pre-existing state='allocated' row (a
// cross-worktree mint) is ADOPTED — its allocation row and receipt are left as
// authored by the minting worktree; only a fresh import intent + event get a new
// sequence, and markTicketMaterialised rewrites the allocation path here.
func (s *Store) registerImportedTicket(ctx context.Context, row importedTicket, data []byte, path string) (Intent, AllocationReceipt, error) {
	var intent Intent
	var receipt AllocationReceipt
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		existing, allocExists, err := scanAllocationTx(ctx, conn, s.projectID, row.Prefix, row.Number)
		if err != nil {
			return err
		}
		receiptSeq := int64(0)
		receiptWorktree := s.worktreeID
		receiptPath := path
		if allocExists {
			// Adopt the pre-allocated mint; the receipt already exists (dedup no-op).
			receiptSeq = existing.Seq
			receiptWorktree = existing.WorktreeID
			receiptPath = existing.Path
		} else {
			allocSeq, err := nextSequence(ctx, conn, s.projectID)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq, kind)
                VALUES(?, ?, ?, ?, 'allocated', ?, ?, ?)`, s.projectID, row.Prefix, row.Number, s.worktreeID, path, allocSeq, kindTicket); err != nil {
				return err
			}
			receiptSeq = allocSeq
		}

		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes, allocation_id, kind)
            VALUES(?, ?, ?, ?, 'ticket.import', '', ?, ?, ?, ?)`, s.projectID, seq, s.worktreeID, path,
			digestBytes(data), data, row.ID, string(IntentKindTicketFile)); err != nil {
			return err
		}
		if err := insertEvent(ctx, conn, s.projectID, seq, "ticket.import", row.ID); err != nil {
			return err
		}
		if err := upsertImportedCounter(ctx, conn, s.projectID, row.Prefix, row.Number); err != nil {
			return err
		}
		intent = Intent{ProjectID: s.projectID, WorktreeID: s.worktreeID, Seq: seq, Path: path,
			Kind: IntentKindTicketFile, Precondition: "", Intended: data, AllocationID: row.ID}
		receipt = AllocationReceipt{ProjectID: s.projectID, WorktreeID: receiptWorktree, ID: row.ID,
			Path: receiptPath, Seq: receiptSeq, State: "allocated", Kind: kindTicket}
		return nil
	})
	if err != nil {
		return Intent{}, AllocationReceipt{}, err
	}
	intent.Receipt = receipt
	return intent, receipt, nil
}

func scanAllocationTx(ctx context.Context, conn *sql.Conn, projectID, prefix string, number int64) (ticketAllocation, bool, error) {
	var a ticketAllocation
	err := conn.QueryRowContext(ctx, `SELECT worktree_id, state, path, seq, kind
        FROM allocations WHERE project_id=? AND prefix=? AND number=?`, projectID, prefix, number).Scan(
		&a.WorktreeID, &a.State, &a.Path, &a.Seq, &a.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ticketAllocation{}, false, nil
	}
	if err != nil {
		return ticketAllocation{}, false, err
	}
	return a, true, nil
}

// applyImportedLinks adds every batch relation whose canonical owner is `owner`
// to that owner's file in ONE read-merge-render. Re-applying an existing
// relation is a no-op; a relation stored on the wrong side is left for `link`/
// reconcile to surface (never repaired implicitly here).
func (s *Store) applyImportedLinks(ctx context.Context, owner string, links []importedLink) error {
	path := s.ticketPath(owner)
	data, outcome, err := readRegularTicket(path)
	if outcome == scanReadInconclusive {
		return indexUnestablishedError()
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("E_NOT_FOUND: canonical relation owner %s is missing", s.displayID(owner))
		}
		return err
	}
	ticket, body, err := domain.ParseTicket(data)
	if err != nil {
		return err
	}
	added := false
	for _, link := range links {
		relation := domain.Relation{Kind: link.Kind, From: link.From, To: link.To}
		if relationExists(ticket.Relations, relation) {
			continue
		}
		ticket.Relations = append(ticket.Relations, relation)
		added = true
	}
	if !added {
		return nil
	}
	newData, err := domain.RenderTicket(ticket, body)
	if err != nil {
		return err
	}
	intent, err := s.preparePathMutationEvent(ctx, path, digestBytes(data), newData, "relation.add", owner)
	if err != nil {
		return err
	}
	return s.materialiseIntent(ctx, intent)
}

func relationExists(existing []domain.Relation, want domain.Relation) bool {
	for _, r := range existing {
		if r == want {
			return true
		}
	}
	return false
}

// absentImportedTickets lists previously-imported ids (bearing a journaled
// ticket.import event) that are missing from the current batch. It is a REPORT
// only — the importer never retires them.
func (s *Store) absentImportedTickets(batch map[string]bool) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT target FROM events WHERE project_id=? AND verb='ticket.import' ORDER BY target`, s.projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	absent := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, err
		}
		if !batch[target] {
			absent = append(absent, target)
		}
	}
	return absent, rows.Err()
}

func (s *Store) advanceImportedTicketCounters(ctx context.Context, maxima map[string]int64) error {
	if len(maxima) == 0 {
		return nil
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		for prefix, maximum := range maxima {
			if err := upsertImportedCounter(ctx, conn, s.projectID, prefix, maximum); err != nil {
				return err
			}
		}
		return nil
	})
}

// upsertImportedCounter advances the per-prefix next_number high-water mark to
// number+1 (never below), so the forward allocator cannot re-mint an imported
// id. The counter is number-only (the allocator never mints a suffix).
func upsertImportedCounter(ctx context.Context, conn *sql.Conn, projectID, prefix string, number int64) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO id_counters(project_id, prefix, next_number)
        VALUES(?, ?, ?) ON CONFLICT(project_id, prefix) DO UPDATE SET next_number=
        CASE WHEN id_counters.next_number < excluded.next_number THEN excluded.next_number ELSE id_counters.next_number END`,
		projectID, prefix, number+1)
	return err
}

func validImportStatus(status domain.Status) bool {
	for _, allowed := range domain.AllowedStatusStrings() {
		if string(status) == allowed {
			return true
		}
	}
	return false
}

// importSeverity maps the backlog Sev column to the aira enum: P0 kept, P1-P3
// direct, P4 clamped to P3, an absent value defaulted to P2, anything else a
// row error.
func importSeverity(raw string) (domain.Severity, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return domain.SeverityP2, nil
	case "P0":
		return domain.SeverityP0, nil
	case "P1":
		return domain.SeverityP1, nil
	case "P2":
		return domain.SeverityP2, nil
	case "P3":
		return domain.SeverityP3, nil
	case "P4":
		return domain.SeverityP3, nil
	default:
		return "", fmt.Errorf("invalid severity %q; allowed: P0, P1, P2, P3, P4", raw)
	}
}
