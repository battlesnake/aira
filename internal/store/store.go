package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aira/internal/domain"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

// covers: AR-5, AR-6, AR-7

var (
	ErrPathIntentBusy = errors.New("E_PATH_INTENT_BUSY")
	ErrWriteConflict  = errors.New("E_WRITE_CONFLICT")
	ErrDBBusy         = errors.New("E_DB_BUSY")
)

type Options struct {
	Root         string
	CommonDir    string
	DBPath       string
	RegistryPath string
	ProjectID    string
	WorktreeID   string
	ProjectSlug  string
	Prefixes     []string
}

type Store struct {
	db           *sql.DB
	root         string
	commonDir    string
	auditDir     string
	dbPath       string
	registryPath string
	projectID    string
	worktreeID   string
	projectSlug  string
	prefixes     map[string]bool
	// beforeMaterialise is intentionally nil in production; tests use it to
	// observe the receipt-before-file ordering at the crash boundary.
	beforeMaterialise func(Intent) error
}

type Intent struct {
	ProjectID    string
	WorktreeID   string
	Seq          int64
	Path         string
	Precondition string
	Intended     []byte
	Ticket       domain.Ticket
	AllocationID string
	Receipt      AllocationReceipt
}

type AllocationReceipt struct {
	ProjectID  string `json:"project_id"`
	WorktreeID string `json:"worktree_id"`
	ID         string `json:"id"`
	Path       string `json:"path"`
	Seq        int64  `json:"seq"`
	At         string `json:"at"`
	State      string `json:"state"`
}

type registryEntry struct {
	ProjectID  string   `json:"project_id"`
	CommonDir  string   `json:"common_dir"`
	WorktreeID string   `json:"worktree_id"`
	Root       string   `json:"root"`
	Prefixes   []string `json:"prefixes"`
	At         string   `json:"at"`
}

type eventRecord struct {
	ProjectID     string `json:"project_id"`
	Seq           int64  `json:"seq"`
	At            string `json:"at"`
	Actor         string `json:"actor"`
	Verb          string `json:"verb"`
	Target        string `json:"target"`
	PayloadDigest string `json:"payload_digest"`
}

type scannedTicket struct {
	WorktreeID string
	Path       string
	Ticket     domain.Ticket
	Digest     string
}

func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Root == "" || opts.CommonDir == "" || opts.DBPath == "" || opts.RegistryPath == "" || opts.ProjectID == "" || opts.WorktreeID == "" {
		return nil, errors.New("E_CONFIG_INVALID: store options are incomplete")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	common, err := filepath.Abs(opts.CommonDir)
	if err != nil {
		return nil, err
	}
	dbPath, err := filepath.Abs(opts.DBPath)
	if err != nil {
		return nil, err
	}
	registry, err := filepath.Abs(opts.RegistryPath)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateProjectSlug(opts.ProjectSlug); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(registry), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(common, "aira", "locks"), 0o755); err != nil {
		return nil, err
	}
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{
		db: db, root: root, commonDir: common, auditDir: filepath.Join(common, "aira"),
		dbPath: dbPath, registryPath: registry, projectID: opts.ProjectID,
		worktreeID: opts.WorktreeID, projectSlug: opts.ProjectSlug, prefixes: map[string]bool{},
	}
	for _, prefix := range opts.Prefixes {
		if !validPrefix(prefix) {
			_ = db.Close()
			return nil, fmt.Errorf("E_ID_INVALID: invalid prefix %q", prefix)
		}
		s.prefixes[strings.ToUpper(prefix)] = true
	}
	if err := s.initDB(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.register(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initDB(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS projects (
            project_id TEXT PRIMARY KEY, slug TEXT NOT NULL, common_dir TEXT NOT NULL,
            config_digest TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS worktrees (
            project_id TEXT NOT NULL, worktree_id TEXT NOT NULL, root TEXT NOT NULL,
            active INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL,
            PRIMARY KEY(project_id, worktree_id)
        )`,
		`CREATE TABLE IF NOT EXISTS prefix_ownership (
            prefix TEXT PRIMARY KEY, project_id TEXT NOT NULL, registered_seq INTEGER NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS event_counters (
            project_id TEXT PRIMARY KEY, next_seq INTEGER NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS id_counters (
            project_id TEXT NOT NULL, prefix TEXT NOT NULL, next_number INTEGER NOT NULL,
            PRIMARY KEY(project_id, prefix)
        )`,
		`CREATE TABLE IF NOT EXISTS allocations (
            project_id TEXT NOT NULL, prefix TEXT NOT NULL, number INTEGER NOT NULL,
            worktree_id TEXT NOT NULL, state TEXT NOT NULL, path TEXT NOT NULL,
            seq INTEGER NOT NULL, PRIMARY KEY(project_id, prefix, number)
        )`,
		`CREATE TABLE IF NOT EXISTS outbox (
            project_id TEXT NOT NULL, seq INTEGER NOT NULL, worktree_id TEXT NOT NULL,
            path TEXT NOT NULL, verb TEXT NOT NULL, precondition_digest TEXT NOT NULL,
            intended_digest TEXT NOT NULL, intended_bytes BLOB, materialised INTEGER NOT NULL DEFAULT 0,
            resolution TEXT, journaled INTEGER NOT NULL DEFAULT 0, allocation_id TEXT NOT NULL DEFAULT '',
            PRIMARY KEY(project_id, seq)
        )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS unresolved_path_intent
            ON outbox(project_id, worktree_id, path)
            WHERE materialised = 0 AND resolution IS NULL`,
		`CREATE TABLE IF NOT EXISTS events (
            project_id TEXT NOT NULL, seq INTEGER NOT NULL, at_wall TEXT NOT NULL,
            actor TEXT NOT NULL, verb TEXT NOT NULL, target TEXT NOT NULL,
            payload_digest TEXT NOT NULL, journaled INTEGER NOT NULL DEFAULT 0,
            PRIMARY KEY(project_id, seq)
        )`,
		`CREATE TABLE IF NOT EXISTS tickets (
            project_id TEXT NOT NULL, worktree_id TEXT NOT NULL, id TEXT NOT NULL,
            path TEXT NOT NULL, digest TEXT NOT NULL, status TEXT NOT NULL, hold INTEGER NOT NULL,
            title TEXT NOT NULL, kind TEXT NOT NULL, severity TEXT NOT NULL,
            PRIMARY KEY(project_id, worktree_id, id)
        )`,
		`CREATE TABLE IF NOT EXISTS findings (
            project_id TEXT NOT NULL, finding_key TEXT NOT NULL, code TEXT NOT NULL,
            subject TEXT NOT NULL, details TEXT NOT NULL, created_at TEXT NOT NULL,
            PRIMARY KEY(project_id, finding_key)
        )`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return translateDBError(err)
		}
	}
	return nil
}

func (s *Store) register(ctx context.Context) error {
	entry := registryEntry{
		ProjectID: s.projectID, CommonDir: s.commonDir, WorktreeID: s.worktreeID,
		Root: s.root, Prefixes: sortedKeys(s.prefixes), At: time.Now().UTC().Format(time.RFC3339Nano),
	}
	// The breadcrumb is written before the DB transaction. A stale breadcrumb is
	// recoverable evidence; a DB row without a breadcrumb is not recoverable after DB loss.
	if err := appendJSONLine(s.registryPath, entry, s.registryPath+".lock"); err != nil {
		return err
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `INSERT INTO projects(project_id, slug, common_dir, config_digest, created_at)
            VALUES(?, ?, ?, '', ?) ON CONFLICT(project_id) DO UPDATE SET slug=excluded.slug, common_dir=excluded.common_dir`,
			s.projectID, s.projectSlug, s.commonDir, now); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO worktrees(project_id, worktree_id, root, active, updated_at)
            VALUES(?, ?, ?, 1, ?) ON CONFLICT(project_id, worktree_id) DO UPDATE SET root=excluded.root, active=1, updated_at=excluded.updated_at`,
			s.projectID, s.worktreeID, s.root, now); err != nil {
			return err
		}
		for prefix := range s.prefixes {
			var owner string
			err := conn.QueryRowContext(ctx, `SELECT project_id FROM prefix_ownership WHERE prefix=?`, prefix).Scan(&owner)
			if err == nil && owner != s.projectID {
				return fmt.Errorf("E_PREFIX_OWNERSHIP_CONFLICT: %s owned by %s", prefix, owner)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO prefix_ownership(prefix, project_id, registered_seq)
                VALUES(?, ?, 0) ON CONFLICT(prefix) DO NOTHING`, prefix, s.projectID); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO id_counters(project_id, prefix, next_number)
                VALUES(?, ?, 1) ON CONFLICT(project_id, prefix) DO NOTHING`, s.projectID, prefix); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO event_counters(project_id, next_seq)
            VALUES(?, 1) ON CONFLICT(project_id) DO NOTHING`, s.projectID)
		return err
	})
}

func (s *Store) RegisterWorktree(ctx context.Context, worktreeID, root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	entry := registryEntry{
		ProjectID: s.projectID, CommonDir: s.commonDir, WorktreeID: worktreeID,
		Root: root, Prefixes: sortedKeys(s.prefixes), At: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := appendJSONLine(s.registryPath, entry, s.registryPath+".lock"); err != nil {
		return err
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO worktrees(project_id, worktree_id, root, active, updated_at)
            VALUES(?, ?, ?, 1, ?) ON CONFLICT(project_id, worktree_id) DO UPDATE SET root=excluded.root, active=1, updated_at=excluded.updated_at`,
			s.projectID, worktreeID, root, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (s *Store) AllocateID(ctx context.Context, prefix string) (string, error) {
	prefix = strings.ToUpper(prefix)
	if !validPrefix(prefix) || !s.prefixes[prefix] {
		return "", fmt.Errorf("E_ID_INVALID: unowned prefix %q", prefix)
	}
	var id string
	var receipt AllocationReceipt
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		number, err := nextNumber(ctx, conn, s.projectID, prefix)
		if err != nil {
			return err
		}
		id = fmt.Sprintf("%s-%d", prefix, number)
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		path := s.ticketPath(id)
		if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq)
            VALUES(?, ?, ?, ?, 'allocated', ?, ?)`, s.projectID, prefix, number, s.worktreeID, path, seq); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes, allocation_id)
			VALUES(?, ?, ?, ?, 'id.allocate', '', '', NULL, ?)`, s.projectID, seq, s.worktreeID, path, id); err != nil {
			return err
		}
		if err := insertEvent(ctx, conn, s.projectID, seq, "id.allocate", id); err != nil {
			return err
		}
		receipt = AllocationReceipt{ProjectID: s.projectID, WorktreeID: s.worktreeID, ID: id, Path: path,
			Seq: seq, At: time.Now().UTC().Format(time.RFC3339Nano), State: "allocated"}
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := s.appendReceiptIfMissing(receipt); err != nil {
		return "", err
	}
	if err := s.markReceiptOnly(ctx, s.projectID, receipt.Seq); err != nil {
		return "", err
	}
	if err := s.journalEvent(ctx, s.projectID, receipt.Seq); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) CreateTicket(ctx context.Context, input domain.CreateTicketInput) (domain.Ticket, error) {
	intent, err := s.prepareCreate(ctx, input)
	if err != nil {
		return domain.Ticket{}, err
	}
	if err := s.appendReceiptIfMissing(intent.Receipt); err != nil {
		return domain.Ticket{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return domain.Ticket{}, err
	}
	return intent.Ticket, nil
}

func (s *Store) prepareCreate(ctx context.Context, input domain.CreateTicketInput) (Intent, error) {
	if strings.TrimSpace(input.Title) == "" {
		return Intent{}, errors.New("E_CONFIG_INVALID: empty title")
	}
	if input.Kind == "" {
		input.Kind = domain.KindFeature
	}
	if input.Severity == "" {
		input.Severity = domain.SeverityP2
	}
	prefix, err := s.defaultPrefix()
	if err != nil {
		return Intent{}, err
	}
	var intent Intent
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		number, err := nextNumber(ctx, conn, s.projectID, prefix)
		if err != nil {
			return err
		}
		id := fmt.Sprintf("%s-%d", prefix, number)
		ticket := domain.Ticket{Schema: 1, ID: id, Project: s.projectSlug, Title: input.Title,
			Status: domain.StatusPlanned, Kind: input.Kind, Severity: input.Severity, Labels: input.Labels}
		data, err := domain.RenderTicket(ticket, input.Body)
		if err != nil {
			return err
		}
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		path := s.ticketPath(id)
		digest := digestBytes(data)
		if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq)
            VALUES(?, ?, ?, ?, 'allocated', ?, ?)`, s.projectID, prefix, number, s.worktreeID, path, seq); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes, allocation_id)
            VALUES(?, ?, ?, ?, 'ticket.create', '', ?, ?, ?)`, s.projectID, seq, s.worktreeID, path, digest, data, id); err != nil {
			return err
		}
		if err := insertEvent(ctx, conn, s.projectID, seq, "ticket.create", id); err != nil {
			return err
		}
		intent = Intent{ProjectID: s.projectID, WorktreeID: s.worktreeID, Seq: seq, Path: path,
			Precondition: "", Intended: data, Ticket: ticket, AllocationID: id,
			Receipt: AllocationReceipt{ProjectID: s.projectID, WorktreeID: s.worktreeID, ID: id, Path: path, Seq: seq, State: "allocated"}}
		return nil
	})
	return intent, err
}

func (s *Store) defaultPrefix() (string, error) {
	prefixes := sortedKeys(s.prefixes)
	if len(prefixes) == 0 {
		return "", errors.New("E_CONFIG_INVALID: no owned ticket prefix")
	}
	return prefixes[0], nil
}

func (s *Store) preparePathMutation(ctx context.Context, path, precondition string, intended []byte, verb string) (Intent, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Intent{}, err
	}
	var intent Intent
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		var existing int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM outbox WHERE project_id=? AND worktree_id=? AND path=? AND materialised=0 AND resolution IS NULL LIMIT 1`,
			s.projectID, s.worktreeID, path).Scan(&existing)
		if err == nil {
			return ErrPathIntentBusy
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		digest := digestBytes(intended)
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes)
            VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, s.projectID, seq, s.worktreeID, path, verb, precondition, digest, intended); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrPathIntentBusy
			}
			return err
		}
		if err := insertEvent(ctx, conn, s.projectID, seq, verb, filepath.Base(path)); err != nil {
			return err
		}
		intent = Intent{ProjectID: s.projectID, WorktreeID: s.worktreeID, Seq: seq, Path: path,
			Precondition: precondition, Intended: intended}
		return nil
	})
	return intent, err
}

func (s *Store) UpdateTicket(ctx context.Context, id string, update func(domain.Ticket) (domain.Ticket, error)) error {
	path := s.ticketPath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ticket, body, err := domain.ParseTicket(data)
	if err != nil {
		return err
	}
	updated, err := update(ticket)
	if err != nil {
		return err
	}
	if updated.Status != ticket.Status {
		if err := domain.ValidateTransition(ticket.Status, updated.Status); err != nil {
			return err
		}
	}
	newData, err := domain.RenderTicket(updated, body)
	if err != nil {
		return err
	}
	intent, err := s.preparePathMutation(ctx, path, digestBytes(data), newData, "ticket.update")
	if err != nil {
		return err
	}
	intent.Ticket = updated
	return s.materialiseIntent(ctx, intent)
}

func (s *Store) materialiseIntent(ctx context.Context, intent Intent) error {
	if len(intent.Intended) == 0 {
		return nil
	}
	lock, err := acquireLock(s.pathLockFor(intent.WorktreeID, intent.Path))
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := fileDigest(intent.Path)
	if err != nil {
		return err
	}
	if current == digestBytes(intent.Intended) {
		if err := s.markMaterialised(ctx, intent); err != nil {
			return err
		}
		return s.journalEvent(ctx, intent.ProjectID, intent.Seq)
	}
	if current != intent.Precondition {
		return fmt.Errorf("%w: %s", ErrWriteConflict, intent.Path)
	}
	if s.beforeMaterialise != nil {
		if err := s.beforeMaterialise(intent); err != nil {
			return err
		}
	}
	if err := writeAtomic(intent.Path, intent.Intended, intent.Seq); err != nil {
		return err
	}
	// The path flock coordinates AIRA writers. A non-cooperative user editing
	// the file directly during this window cannot be made into a POSIX CAS;
	// rename therefore accepts the documented limitation of check-then-rename.
	if err := s.markMaterialised(ctx, intent); err != nil {
		return err
	}
	return s.journalEvent(ctx, intent.ProjectID, intent.Seq)
}

func (s *Store) markMaterialised(ctx context.Context, intent Intent) error {
	ticket, _, err := domain.ParseTicket(intent.Intended)
	if err != nil {
		return err
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE outbox SET materialised=1 WHERE project_id=? AND seq=? AND materialised=0 AND resolution IS NULL`, intent.ProjectID, intent.Seq); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE allocations SET state='materialised' WHERE project_id=? AND prefix=? AND number=?`, intent.ProjectID, prefixOf(ticket.ID), numberOf(ticket.ID)); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO tickets(project_id, worktree_id, id, path, digest, status, hold, title, kind, severity)
            VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(project_id, worktree_id, id) DO UPDATE SET path=excluded.path, digest=excluded.digest,
            status=excluded.status, hold=excluded.hold, title=excluded.title, kind=excluded.kind, severity=excluded.severity`,
			intent.ProjectID, intent.WorktreeID, ticket.ID, intent.Path, digestBytes(intent.Intended), ticket.Status, boolInt(ticket.Hold), ticket.Title, ticket.Kind, ticket.Severity)
		return err
	})
}

func (s *Store) markReceiptOnly(ctx context.Context, projectID string, seq int64) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `UPDATE outbox SET materialised=1, intended_bytes=NULL
            WHERE project_id=? AND seq=? AND materialised=0 AND resolution IS NULL`, projectID, seq)
		return err
	})
}

func (s *Store) journalEvent(ctx context.Context, projectID string, seq int64) error {
	var event eventRecord
	row := s.db.QueryRowContext(ctx, `SELECT project_id, seq, at_wall, actor, verb, target, payload_digest FROM events WHERE project_id=? AND seq=?`, projectID, seq)
	err := row.Scan(&event.ProjectID, &event.Seq, &event.At, &event.Actor, &event.Verb, &event.Target, &event.PayloadDigest)
	if err != nil {
		return err
	}
	if err := appendEventIfMissing(filepath.Join(s.auditDir, "journal.jsonl"), event, filepath.Join(s.auditDir, "journal.lock")); err != nil {
		return err
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE events SET journaled=1 WHERE project_id=? AND seq=?`, projectID, seq); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE outbox SET journaled=1, intended_bytes=NULL WHERE project_id=? AND seq=? AND materialised=1`, projectID, seq)
		return err
	})
}

func (s *Store) appendReceiptIfMissing(receipt AllocationReceipt) error {
	path := filepath.Join(s.auditDir, "receipts.jsonl")
	if receipt.At == "" {
		receipt.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	defer unlockFile(lock)
	f, err := openAppendFile(path)
	if err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	defer f.Close()
	if err := repairJSONLTail(f); err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	dec := json.NewDecoder(f)
	for {
		var existing AllocationReceipt
		err := dec.Decode(&existing)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("E_RECEIPT_IO: malformed receipts: %w", err)
		}
		if existing.ProjectID == receipt.ProjectID && existing.ID == receipt.ID && existing.Seq == receipt.Seq {
			return nil
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	if err := appendJSONValue(f, receipt); err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	return nil
}

func (s *Store) intentMaterialised(ctx context.Context, intent Intent) (bool, error) {
	var materialised int
	err := s.db.QueryRowContext(ctx, `SELECT materialised FROM outbox WHERE project_id=? AND seq=?`, intent.ProjectID, intent.Seq).Scan(&materialised)
	return materialised != 0, err
}

func (s *Store) recordFinding(ctx context.Context, intent Intent, cause error) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO findings(project_id, finding_key, code, subject, details, created_at)
			VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(project_id, finding_key) DO UPDATE SET details=excluded.details`,
			intent.ProjectID, fmt.Sprintf("reconcile:%s:%d", intent.WorktreeID, intent.Seq), "E_WRITE_CONFLICT", intent.Path,
			cause.Error(), time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (s *Store) recordRebuildFinding(ctx context.Context, entry registryEntry, reason string) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO findings(project_id, finding_key, code, subject, details, created_at)
			VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(project_id, finding_key) DO UPDATE SET details=excluded.details`,
			s.projectID, "rebuild:git-root:"+digestBytes([]byte(entry.Root)), "E_GIT_SCAN", entry.Root,
			reason, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (s *Store) reconcile(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, seq, worktree_id, path, verb, precondition_digest,
		intended_digest, intended_bytes, materialised, journaled, allocation_id FROM outbox
		WHERE project_id=? AND worktree_id=? AND resolution IS NULL AND (materialised=0 OR journaled=0) ORDER BY seq`, s.projectID, s.worktreeID)
	if err != nil {
		return err
	}
	var pending []Intent
	for rows.Next() {
		var intent Intent
		var materialised int
		var journaled int
		var intended []byte
		var verb string
		var intendedDigest string
		if err := rows.Scan(&intent.ProjectID, &intent.Seq, &intent.WorktreeID, &intent.Path, &verb,
			&intent.Precondition, &intendedDigest, &intended, &materialised, &journaled, &intent.AllocationID); err != nil {
			return err
		}
		_ = verb
		_ = intendedDigest
		_ = journaled
		intent.Intended = intended
		pending = append(pending, intent)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var firstErr error
	for _, intent := range pending {
		// Replay always resumes at the first incomplete stage. In particular,
		// an allocation receipt is repaired before any file materialisation.
		if intent.AllocationID != "" {
			if err := s.appendReceiptIfMissing(AllocationReceipt{ProjectID: s.projectID, WorktreeID: intent.WorktreeID,
				ID: intent.AllocationID, Path: intent.Path, Seq: intent.Seq, State: "allocated"}); err != nil {
				return err
			}
		}
		if materialised, err := s.intentMaterialised(ctx, intent); err != nil {
			return err
		} else if materialised {
			if err := s.journalEvent(ctx, intent.ProjectID, intent.Seq); err != nil {
				return err
			}
			continue
		}
		if len(intent.Intended) == 0 {
			if err := s.markReceiptOnly(ctx, intent.ProjectID, intent.Seq); err != nil {
				return err
			}
			if err := s.journalEvent(ctx, intent.ProjectID, intent.Seq); err != nil {
				return err
			}
			continue
		}
		current, err := fileDigest(intent.Path)
		if err != nil {
			return err
		}
		if current == digestBytes(intent.Intended) {
			if err := s.markMaterialised(ctx, intent); err != nil {
				return err
			}
			if err := s.journalEvent(ctx, intent.ProjectID, intent.Seq); err != nil {
				return err
			}
			continue
		}
		if current == intent.Precondition {
			if err := s.materialiseIntent(ctx, intent); err != nil {
				return err
			}
			continue
		}
		conflict := fmt.Errorf("%w: pending intent %s", ErrWriteConflict, intent.Path)
		if err := s.recordFinding(ctx, intent, conflict); err != nil {
			return err
		}
		if firstErr == nil {
			firstErr = conflict
		}
	}
	return firstErr
}

func (s *Store) Reconcile(ctx context.Context) error { return s.reconcile(ctx) }

func (s *Store) Rebuild(ctx context.Context) error {
	lock, err := acquireLock(filepath.Join(filepath.Dir(s.dbPath), "rebuild.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	entries, err := readRegistry(s.registryPath)
	if err != nil {
		return err
	}
	entries, err = discoverWorktrees(s.root, s.projectID, entries)
	if err != nil {
		return err
	}
	// These JSONL reads intentionally remain lock-free during Rebuild. A concurrent
	// append may be observed as a benign torn tail and repaired on the next run.
	receipts, err := readReceipts(filepath.Join(s.auditDir, "receipts.jsonl"))
	if err != nil {
		return err
	}
	journal, err := readJournal(filepath.Join(s.auditDir, "journal.jsonl"))
	if err != nil {
		return err
	}
	receiptKeys := make(map[string]bool, len(receipts))
	maxima := map[string]int64{}
	maxSeq := int64(0)
	for _, receipt := range receipts {
		if receipt.ProjectID != s.projectID {
			continue
		}
		if domain.ValidateID(receipt.ID) != nil {
			continue
		}
		prefix, number := splitTicketID(receipt.ID)
		if int64(number) > maxima[prefix] {
			maxima[prefix] = int64(number)
		}
		if receipt.Seq > maxSeq {
			maxSeq = receipt.Seq
		}
		receiptKeys[receiptKey(receipt.ProjectID, receipt.ID, receipt.Seq)] = true
	}
	for _, event := range journal {
		if event.ProjectID == s.projectID && event.Seq > maxSeq {
			maxSeq = event.Seq
		}
	}
	var scanned []scannedTicket
	for _, entry := range entries {
		valid, reason, gitErr := validGitRoot(entry.Root)
		if gitErr != nil {
			return gitErr
		}
		tickets, err := scanTickets(entry.Root, entry.WorktreeID)
		if err != nil {
			return err
		}
		scanned = append(scanned, tickets...)
		for _, ticket := range tickets {
			prefix, number := splitTicketID(ticket.Ticket.ID)
			if int64(number) > maxima[prefix] {
				maxima[prefix] = int64(number)
			}
		}
		if !valid {
			if err := s.recordRebuildFinding(ctx, entry, reason); err != nil {
				return err
			}
			continue
		}
		refMax, err := scanRefMax(entry.Root)
		if err != nil {
			return err
		}
		for prefix, number := range refMax {
			if number > maxima[prefix] {
				maxima[prefix] = number
			}
		}
	}
	var recovered []AllocationReceipt
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO event_counters(project_id,next_seq) VALUES(?,?)
			ON CONFLICT(project_id) DO UPDATE SET next_seq=CASE WHEN event_counters.next_seq < excluded.next_seq THEN excluded.next_seq ELSE event_counters.next_seq END`,
			s.projectID, maxSeq+1); err != nil {
			return err
		}
		for _, event := range journal {
			if event.ProjectID != s.projectID {
				continue
			}
			if event.PayloadDigest != digestBytes([]byte(event.Verb+"\x00"+event.Target)) {
				return fmt.Errorf("E_JOURNAL_CORRUPT: event %s/%d has invalid payload digest", event.ProjectID, event.Seq)
			}
			var existing eventRecord
			err := conn.QueryRowContext(ctx, `SELECT project_id, seq, at_wall, actor, verb, target, payload_digest FROM events WHERE project_id=? AND seq=?`, event.ProjectID, event.Seq).
				Scan(&existing.ProjectID, &existing.Seq, &existing.At, &existing.Actor, &existing.Verb, &existing.Target, &existing.PayloadDigest)
			if errors.Is(err, sql.ErrNoRows) {
				if _, err := conn.ExecContext(ctx, `INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest,journaled) VALUES(?,?,?,?,?,?,?,1)`,
					event.ProjectID, event.Seq, event.At, event.Actor, event.Verb, event.Target, event.PayloadDigest); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if existing.PayloadDigest != event.PayloadDigest || existing.Verb != event.Verb || existing.Target != event.Target {
				return fmt.Errorf("E_JOURNAL_CORRUPT: duplicate project/seq %s/%d has different payload", event.ProjectID, event.Seq)
			}
		}
		for _, receipt := range receipts {
			if receipt.ProjectID != s.projectID || domain.ValidateID(receipt.ID) != nil || receipt.Seq <= 0 {
				continue
			}
			if err := ensureReceiptAllocation(ctx, conn, receipt, journal); err != nil {
				return err
			}
		}
		for _, ticket := range scanned {
			prefix, number := splitTicketID(ticket.Ticket.ID)
			if _, err := conn.ExecContext(ctx, `INSERT INTO tickets(project_id, worktree_id, id, path, digest, status, hold, title, kind, severity)
                    VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    ON CONFLICT(project_id, worktree_id, id) DO UPDATE SET path=excluded.path, digest=excluded.digest,
                    status=excluded.status, hold=excluded.hold, title=excluded.title, kind=excluded.kind, severity=excluded.severity`,
				s.projectID, ticket.WorktreeID, ticket.Ticket.ID, ticket.Path, ticket.Digest,
				ticket.Ticket.Status, boolInt(ticket.Ticket.Hold), ticket.Ticket.Title, ticket.Ticket.Kind, ticket.Ticket.Severity); err != nil {
				return err
			}
			var allocationSeq int64
			var allocationWorktree, allocationPath, allocationState string
			err := conn.QueryRowContext(ctx, `SELECT seq, worktree_id, path, state FROM allocations WHERE project_id=? AND prefix=? AND number=?`,
				s.projectID, prefix, number).Scan(&allocationSeq, &allocationWorktree, &allocationPath, &allocationState)
			if errors.Is(err, sql.ErrNoRows) {
				allocationSeq, err = nextSequence(ctx, conn, s.projectID)
				if err != nil {
					return err
				}
				allocationWorktree, allocationPath, allocationState = ticket.WorktreeID, ticket.Path, "recovered"
				if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq)
                        VALUES(?, ?, ?, ?, ?, ?, ?)`, s.projectID, prefix, number, allocationWorktree, allocationState, allocationPath, allocationSeq); err != nil {
					return err
				}
				recovered = append(recovered, AllocationReceipt{ProjectID: s.projectID, WorktreeID: allocationWorktree,
					ID: ticket.Ticket.ID, Path: allocationPath, Seq: allocationSeq, State: "recovered"})
				if err := ensureRecoveredEvent(ctx, conn, ticket.Ticket.ID, ticket.WorktreeID, ticket.Path, allocationSeq, s.projectID, journal); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if !receiptKeys[receiptKey(s.projectID, ticket.Ticket.ID, allocationSeq)] {
				recovered = append(recovered, AllocationReceipt{ProjectID: s.projectID, WorktreeID: allocationWorktree,
					ID: ticket.Ticket.ID, Path: allocationPath, Seq: allocationSeq, State: "recovered"})
			}
			if allocationSeq > maxSeq {
				maxSeq = allocationSeq
			}
		}
		for prefix, maxNumber := range maxima {
			if _, err := conn.ExecContext(ctx, `INSERT INTO id_counters(project_id, prefix, next_number)
                VALUES(?, ?, ?) ON CONFLICT(project_id, prefix) DO UPDATE SET next_number=
                CASE WHEN id_counters.next_number < excluded.next_number THEN excluded.next_number ELSE id_counters.next_number END`,
				s.projectID, prefix, maxNumber+1); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO event_counters(project_id,next_seq) VALUES(?,?)
			ON CONFLICT(project_id) DO UPDATE SET next_seq=CASE WHEN event_counters.next_seq < excluded.next_seq THEN excluded.next_seq ELSE event_counters.next_seq END`,
			s.projectID, maxSeq+1); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, receipt := range recovered {
		if err := s.appendReceiptIfMissing(receipt); err != nil {
			return err
		}
	}
	return nil
}

func receiptKey(project, id string, seq int64) string {
	return fmt.Sprintf("%s\x00%s\x00%d", project, id, seq)
}

func journalEventFor(journal []eventRecord, project string, seq int64) (eventRecord, bool) {
	for _, event := range journal {
		if event.ProjectID == project && event.Seq == seq {
			return event, true
		}
	}
	return eventRecord{}, false
}

func ensureReceiptAllocation(ctx context.Context, conn *sql.Conn, receipt AllocationReceipt, journal []eventRecord) error {
	prefix, number := splitTicketID(receipt.ID)
	var allocationSeq int64
	err := conn.QueryRowContext(ctx, `SELECT seq FROM allocations WHERE project_id=? AND prefix=? AND number=?`, receipt.ProjectID, prefix, number).Scan(&allocationSeq)
	if errors.Is(err, sql.ErrNoRows) {
		allocationSeq = receipt.Seq
		_, err = conn.ExecContext(ctx, `INSERT INTO allocations(project_id,prefix,number,worktree_id,state,path,seq) VALUES(?,?,?,?,?,?,?)`,
			receipt.ProjectID, prefix, number, receipt.WorktreeID, receipt.State, receipt.Path, allocationSeq)
	} else if err == nil && allocationSeq != receipt.Seq {
		return fmt.Errorf("E_JOURNAL_CORRUPT: receipt %s has seq %d but allocation has seq %d", receipt.ID, receipt.Seq, allocationSeq)
	}
	if err != nil {
		return err
	}
	return ensureAllocationEvent(ctx, conn, receipt.ProjectID, receipt.ID, receipt.WorktreeID, receipt.Path, receipt.Seq, journal)
}

func ensureRecoveredEvent(ctx context.Context, conn *sql.Conn, id, worktreeID, path string, seq int64, project string, journal []eventRecord) error {
	return ensureAllocationEvent(ctx, conn, project, id, worktreeID, path, seq, journal)
}

func ensureAllocationEvent(ctx context.Context, conn *sql.Conn, project, id, worktreeID, path string, seq int64, journal []eventRecord) error {
	fromJournal, journaled := journalEventFor(journal, project, seq)
	verb, target, payload := "id.allocate", id, digestBytes([]byte("id.allocate\x00"+id))
	if fromJournal.ProjectID != "" {
		if fromJournal.Target != id {
			return fmt.Errorf("E_JOURNAL_CORRUPT: duplicate project/seq %s/%d has target %s and %s", project, seq, fromJournal.Target, id)
		}
		if fromJournal.PayloadDigest != digestBytes([]byte(fromJournal.Verb+"\x00"+fromJournal.Target)) {
			return fmt.Errorf("E_JOURNAL_CORRUPT: event %s/%d has invalid payload digest", project, seq)
		}
		verb, target, payload = fromJournal.Verb, fromJournal.Target, fromJournal.PayloadDigest
	} else {
		var existing eventRecord
		err := conn.QueryRowContext(ctx, `SELECT project_id, seq, at_wall, actor, verb, target, payload_digest FROM events WHERE project_id=? AND seq=?`, project, seq).
			Scan(&existing.ProjectID, &existing.Seq, &existing.At, &existing.Actor, &existing.Verb, &existing.Target, &existing.PayloadDigest)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := conn.ExecContext(ctx, `INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest,journaled) VALUES(?,?,?,'aira',?,?,?,0)`,
				project, seq, time.Now().UTC().Format(time.RFC3339Nano), verb, target, payload); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if existing.Target != id {
			return fmt.Errorf("E_JOURNAL_CORRUPT: duplicate project/seq %s/%d has different payload", project, seq)
		} else if existing.PayloadDigest != digestBytes([]byte(existing.Verb+"\x00"+existing.Target)) {
			return fmt.Errorf("E_JOURNAL_CORRUPT: event %s/%d has invalid payload digest", project, seq)
		} else {
			verb, target, payload = existing.Verb, existing.Target, existing.PayloadDigest
		}
	}
	_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest,intended_bytes,materialised,journaled,allocation_id)
		VALUES(?, ?, ?, ?, ?, '', '', NULL, 1, ?, ?)`, project, seq, worktreeID, path, verb, boolInt(journaled), id)
	return err
}

func (s *Store) withImmediate(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return translateDBError(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return translateDBError(err)
	}
	if err := fn(conn); err != nil {
		if rollbackErr := rollbackConn(conn); rollbackErr != nil {
			discardConn(conn)
			return translateDBError(fmt.Errorf("%w; rollback failed: %v", err, rollbackErr))
		}
		return translateDBError(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		if rollbackErr := rollbackConn(conn); rollbackErr != nil {
			discardConn(conn)
			return translateDBError(fmt.Errorf("%w; rollback failed: %v", err, rollbackErr))
		}
		return translateDBError(err)
	}
	return nil
}

func rollbackConn(conn *sql.Conn) error {
	_, err := conn.ExecContext(context.Background(), "ROLLBACK")
	return err
}

func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(driverConn any) error {
		if closer, ok := driverConn.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	})
}

func translateDBError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDBBusy) || strings.Contains(strings.ToLower(err.Error()), "database is locked") {
		return fmt.Errorf("%w: %v", ErrDBBusy, err)
	}
	return err
}

func nextNumber(ctx context.Context, conn *sql.Conn, project, prefix string) (int64, error) {
	var next int64
	err := conn.QueryRowContext(ctx, `SELECT next_number FROM id_counters WHERE project_id=? AND prefix=?`, project, prefix).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		next = 1
		if _, err := conn.ExecContext(ctx, `INSERT INTO id_counters(project_id, prefix, next_number) VALUES(?, ?, 2)`, project, prefix); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE id_counters SET next_number=? WHERE project_id=? AND prefix=?`, next+1, project, prefix); err != nil {
		return 0, err
	}
	return next, nil
}

func nextSequence(ctx context.Context, conn *sql.Conn, project string) (int64, error) {
	var next int64
	err := conn.QueryRowContext(ctx, `SELECT next_seq FROM event_counters WHERE project_id=?`, project).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO event_counters(project_id, next_seq) VALUES(?, 2)`, project); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE event_counters SET next_seq=? WHERE project_id=?`, next+1, project); err != nil {
		return 0, err
	}
	return next, nil
}

func insertEvent(ctx context.Context, conn *sql.Conn, project string, seq int64, verb, target string) error {
	payload := digestBytes([]byte(verb + "\x00" + target))
	_, err := conn.ExecContext(ctx, `INSERT INTO events(project_id, seq, at_wall, actor, verb, target, payload_digest)
        VALUES(?, ?, ?, 'aira', ?, ?, ?)`, project, seq, time.Now().UTC().Format(time.RFC3339Nano), verb, target, payload)
	return err
}

func (s *Store) ticketPath(id string) string {
	return filepath.Join(s.root, ".aira", "tickets", id+".md")
}

func (s *Store) pathLock(path string) string {
	return s.pathLockFor(s.worktreeID, path)
}

func (s *Store) pathLockFor(worktreeID, path string) string {
	triple := s.projectID + "\x00" + worktreeID + "\x00" + path
	return filepath.Join(s.auditDir, "locks", "path-"+digestBytes([]byte(triple))+".lock")
}

func prefixOf(id string) string { return id[:strings.LastIndexByte(id, '-')] }

func numberOf(id string) int64 {
	n, _ := strconv.ParseInt(id[strings.LastIndexByte(id, '-')+1:], 10, 64)
	return n
}

func splitTicketID(id string) (string, int) {
	idx := strings.LastIndexByte(id, '-')
	n, _ := strconv.Atoi(id[idx+1:])
	return id[:idx], n
}

func validPrefix(prefix string) bool {
	if len(prefix) < 2 {
		return false
	}
	for _, r := range prefix {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}

func writeAtomic(path string, data []byte, seq int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".aira-tmp-"+strconv.FormatInt(seq, 10))
	var f *os.File
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		f, err = os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o644)
		if err == nil {
			break
		}
		if (!errors.Is(err, os.ErrExist) && !errors.Is(err, unix.ELOOP)) || attempt == 1 {
			return err
		}
		if removeErr := os.Remove(tmp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	}
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = f.Close()
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	_ = dir.Close()
	cleanup = false
	return err
}

func acquireLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Store) appendReceiptForIntent(intent Intent) error {
	return s.appendReceiptIfMissing(intent.Receipt)
}

func appendJSONLine(path string, value any, lockPath string) error {
	lock, err := acquireLock(lockPath)
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	f, err := openAppendFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := repairJSONLTail(f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if err := appendJSONValue(f, value); err != nil {
		return err
	}
	return f.Sync()
}

func appendEventIfMissing(path string, event eventRecord, lockPath string) error {
	lock, err := acquireLock(lockPath)
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	f, err := openAppendFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := repairJSONLTail(f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dec := json.NewDecoder(f)
	for {
		var existing eventRecord
		err := dec.Decode(&existing)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("E_JOURNAL_CORRUPT: %w", err)
		}
		if existing.ProjectID == event.ProjectID && existing.Seq == event.Seq {
			if existing.PayloadDigest != event.PayloadDigest || existing.Verb != event.Verb || existing.Target != event.Target {
				return fmt.Errorf("E_JOURNAL_CORRUPT: duplicate project/seq %s/%d has different payload", event.ProjectID, event.Seq)
			}
			return nil
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if err := appendJSONValue(f, event); err != nil {
		return err
	}
	return f.Sync()
}

func openAppendFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
}

func appendJSONValue(f *os.File, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func repairJSONLTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	cut := bytes.LastIndexByte(data, '\n')
	if cut < 0 {
		cut = 0
	} else {
		cut++
	}
	tail := append([]byte(nil), data[cut:]...)
	if len(tail) > 0 {
		if err := preserveTornTail(f.Name(), tail); err != nil {
			return err
		}
	}
	if err := f.Truncate(int64(cut)); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return f.Sync()
}

func preserveTornTail(path string, tail []byte) error {
	evidence := path + ".torn-tail-" + digestBytes(tail)[:16]
	evidenceFile, err := os.OpenFile(evidence, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := evidenceFile.Write(tail); err != nil {
		_ = evidenceFile.Close()
		return err
	}
	if err := evidenceFile.Sync(); err != nil {
		_ = evidenceFile.Close()
		return err
	}
	return evidenceFile.Close()
}

func unlockFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}

func readRegistry(path string) ([]registryEntry, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []registryEntry
	dec := json.NewDecoder(f)
	for {
		var entry registryEntry
		err := dec.Decode(&entry)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("E_CONFIG_INVALID: registry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func discoverWorktrees(root, projectID string, registry []registryEntry) ([]registryEntry, error) {
	byRoot := map[string]registryEntry{}
	for _, entry := range registry {
		if entry.ProjectID != projectID || entry.Root == "" {
			continue
		}
		absolute, err := filepath.Abs(entry.Root)
		if err != nil {
			return nil, err
		}
		entry.Root = absolute
		byRoot[absolute] = entry
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, ok := byRoot[absoluteRoot]; !ok {
		byRoot[absoluteRoot] = registryEntry{ProjectID: projectID, WorktreeID: "current", Root: absoluteRoot}
	}
	if _, err := os.Stat(absoluteRoot); err != nil {
		return sortedRegistryEntries(byRoot), nil
	}
	out, stderr, err := runGit(absoluteRoot, "worktree", "list", "--porcelain")
	if err != nil {
		if isNotGitRepository(stderr) {
			return sortedRegistryEntries(byRoot), nil
		}
		return nil, fmt.Errorf("E_GIT_SCAN: worktree list: %w: %s", err, strings.TrimSpace(stderr))
	}
	var worktreeRoot string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			worktreeRoot = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case line == "" && worktreeRoot != "":
			if err := addDiscoveredWorktree(byRoot, projectID, worktreeRoot); err != nil {
				return nil, err
			}
			worktreeRoot = ""
		}
	}
	if worktreeRoot != "" {
		if err := addDiscoveredWorktree(byRoot, projectID, worktreeRoot); err != nil {
			return nil, err
		}
	}
	return sortedRegistryEntries(byRoot), nil
}

func addDiscoveredWorktree(byRoot map[string]registryEntry, projectID, root string) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if _, ok := byRoot[absolute]; ok {
		return nil
	}
	if candidateInfo, err := os.Stat(absolute); err == nil {
		for existingRoot := range byRoot {
			existingInfo, err := os.Stat(existingRoot)
			if err == nil && os.SameFile(existingInfo, candidateInfo) {
				return nil
			}
		}
	}
	byRoot[absolute] = registryEntry{ProjectID: projectID, WorktreeID: "worktree-" + digestBytes([]byte(absolute))[:16], Root: absolute}
	return nil
}

func sortedRegistryEntries(byRoot map[string]registryEntry) []registryEntry {
	roots := make([]string, 0, len(byRoot))
	for root := range byRoot {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	entries := make([]registryEntry, 0, len(roots))
	for _, root := range roots {
		entries = append(entries, byRoot[root])
	}
	return entries
}

func isNotGitRepository(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "not a git repository") || strings.Contains(output, "not a git work tree")
}

func runGit(root string, args ...string) (string, string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func validGitRoot(root string) (bool, string, error) {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, "worktree root does not exist", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("E_GIT_SCAN: stat worktree root: %w", err)
	}
	if !info.IsDir() {
		return false, "worktree root is not a directory", nil
	}
	top, stderr, err := runGit(root, "rev-parse", "--show-toplevel")
	if err != nil {
		if isNotGitRepository(stderr) {
			return false, fmt.Sprintf("rev-parse: %v: %s", err, strings.TrimSpace(stderr)), nil
		}
		return false, "", fmt.Errorf("E_GIT_SCAN: rev-parse: %w: %s", err, strings.TrimSpace(stderr))
	}
	topPath := strings.TrimSpace(top)
	topInfo, err := os.Stat(topPath)
	if err != nil {
		return false, "", fmt.Errorf("E_GIT_SCAN: stat git top-level %q: %w", topPath, err)
	}
	if !os.SameFile(info, topInfo) {
		return false, fmt.Sprintf("git top-level %q does not identify root %q", topPath, root), nil
	}
	return true, "", nil
}

func readReceipts(path string) ([]AllocationReceipt, error) {
	records, err := readJSONLRecords(path)
	if err != nil {
		return nil, fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
	var receipts []AllocationReceipt
	for _, record := range records {
		var receipt AllocationReceipt
		if err := json.Unmarshal(record, &receipt); err != nil {
			return nil, fmt.Errorf("E_RECEIPT_IO: malformed receipts: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func readJournal(path string) ([]eventRecord, error) {
	records, err := readJSONLRecords(path)
	if err != nil {
		return nil, fmt.Errorf("E_JOURNAL_CORRUPT: %w", err)
	}
	var events []eventRecord
	seen := map[string]eventRecord{}
	for _, record := range records {
		var event eventRecord
		if err := json.Unmarshal(record, &event); err != nil {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: %w", err)
		}
		key := fmt.Sprintf("%s\x00%d", event.ProjectID, event.Seq)
		if prior, ok := seen[key]; ok && (prior.PayloadDigest != event.PayloadDigest || prior.Verb != event.Verb || prior.Target != event.Target) {
			return nil, fmt.Errorf("E_JOURNAL_CORRUPT: duplicate project/seq %s/%d has different payload", event.ProjectID, event.Seq)
		}
		seen[key] = event
		events = append(events, event)
	}
	return events, nil
}

func readJSONLRecords(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		tail := lines[len(lines)-1]
		if len(bytes.TrimSpace(tail)) > 0 && !json.Valid(tail) {
			if err := preserveTornTail(path, tail); err != nil {
				return nil, err
			}
			lines = lines[:len(lines)-1]
		}
	}
	records := make([][]byte, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var record json.RawMessage
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("malformed JSONL record: %w", err)
		}
		records = append(records, append([]byte(nil), record...))
	}
	return records, nil
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func scanTickets(root, worktreeID string) ([]scannedTicket, error) {
	dir := filepath.Join(root, ".aira", "tickets")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var result []scannedTicket
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		ticket, _, err := domain.ParseTicket(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if seen[ticket.ID] {
			return nil, fmt.Errorf("E_DUPLICATE_ID: %s in worktree %s", ticket.ID, worktreeID)
		}
		if filepath.Base(path) != ticket.ID+".md" {
			return nil, fmt.Errorf("E_CONFIG_INVALID: filename/frontmatter mismatch %s", path)
		}
		seen[ticket.ID] = true
		result = append(result, scannedTicket{WorktreeID: worktreeID, Path: path, Ticket: ticket, Digest: digestBytes(data)})
	}
	return result, nil
}

func scanRefMax(root string) (map[string]int64, error) {
	result := map[string]int64{}
	out, stderr, err := runGit(root, "for-each-ref", "--format=%(refname)")
	if err != nil {
		if isNotGitRepository(stderr) {
			return result, nil
		}
		return nil, fmt.Errorf("E_GIT_SCAN: for-each-ref: %w: %s", err, strings.TrimSpace(stderr))
	}
	for _, ref := range strings.Split(strings.TrimSpace(out), "\n") {
		if ref == "" {
			continue
		}
		paths, stderr, err := runGit(root, "ls-tree", "-r", "--name-only", ref, "--", ".aira/tickets")
		if err != nil {
			if isNotGitRepository(stderr) {
				return result, nil
			}
			return nil, fmt.Errorf("E_GIT_SCAN: ls-tree %s: %w: %s", ref, err, strings.TrimSpace(stderr))
		}
		for _, path := range strings.Split(strings.TrimSpace(string(paths)), "\n") {
			base := filepath.Base(path)
			if !strings.HasSuffix(base, ".md") {
				continue
			}
			id := strings.TrimSuffix(base, ".md")
			if err := domain.ValidateID(id); err != nil {
				continue
			}
			prefix, number := splitTicketID(id)
			if int64(number) > result[prefix] {
				result[prefix] = int64(number)
			}
		}
	}
	return result, nil
}
