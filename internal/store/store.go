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
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/runner"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// covers: AR-5, AR-6, AR-7

var (
	ErrPathIntentBusy     = errors.New("E_PATH_INTENT_BUSY")
	ErrWriteConflict      = errors.New("E_WRITE_CONFLICT")
	ErrDBBusy             = errors.New("E_DB_BUSY")
	errJournalKeyConflict = errors.New("E_JOURNAL_CORRUPT: journal key conflict")
	errJournalMalformed   = errors.New("E_JOURNAL_CORRUPT: malformed journal")
)

const journalLockTimeout = 2 * time.Second

// StoreOpBodyMax is the compile-time transport ceiling for a relayed store
// operation body. It lives in store so config validation and the daemon wire
// layer share one value without an app<->daemon import cycle.
const StoreOpBodyMax int64 = 64 << 20

// Test-only fault/observation seams. Production leaves both nil.
var (
	beforeFileSync func(*os.File) error
	beforeDirSync  func(*os.File) error
)

type Options struct {
	Root         string
	CommonDir    string
	GitDir       string
	DBPath       string
	RegistryPath string
	ProjectID    string
	WorktreeID   string
	ProjectSlug  string
	Prefixes     []string
	// RequirementPrefixes registers requirement-kind ID prefixes (e.g. "AR").
	// They must be disjoint from Prefixes (ticket-kind); a prefix belongs to
	// exactly one kind.
	RequirementPrefixes []string
	// ReviewPolicy is validated eagerly by Open. A zero policy means the
	// project has no review block and therefore defaults to tier 3.
	ReviewPolicy      ReviewPolicy
	LeaseStateDir     string
	LeaseTTLNS        uint64
	MaxReports        int
	MaxAgeDays        int
	MaxComputeEvents  int
	MaxComputeAgeDays int
	MaxCommandEvents  int
	MaxCommandAgeDays int
	MaxQuotaSnapshots int
	Clock             Clock
}

// ScopeOptions describes one worktree view over a machine-wide DB. DB and
// registry paths deliberately do not belong here: the DB owner pins them when
// it calls OpenDB.
type ScopeOptions struct {
	Root                string
	CommonDir           string
	GitDir              string
	ProjectID           string
	WorktreeID          string
	ProjectSlug         string
	Prefixes            []string
	RequirementPrefixes []string
	ReviewPolicy        ReviewPolicy
	LeaseStateDir       string
	LeaseTTLNS          uint64
	MaxReports          int
	MaxAgeDays          int
	MaxComputeEvents    int
	MaxComputeAgeDays   int
	MaxCommandEvents    int
	MaxCommandAgeDays   int
	MaxQuotaSnapshots   int
	ConfigDigest        string
	Bootstrap           bool
	Clock               Clock
}

// DB is the owner of one machine-wide SQLite connection and its pinned path
// identity. Many Store scopes may share it; only DB.Close closes the
// connection.
type DB struct {
	db           *sql.DB
	dbPath       string
	registryPath string
}

// Close releases the connection owned by db.
func (db *DB) Close() error {
	if db == nil || db.db == nil {
		return nil
	}
	return db.db.Close()
}

// Execution is the deliberately narrow process-execution dependency used by
// command gate lanes. *runner.Runner satisfies it in production; tests may
// inject a recording implementation.
type Execution interface {
	Launch(context.Context, runner.Request) (*runner.RunRecord, error)
	ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error)
}

type Store struct {
	db                *sql.DB
	owner             *DB
	root              string
	commonDir         string
	auditDir          string
	dbPath            string
	registryPath      string
	projectID         string
	worktreeID        string
	projectSlug       string
	configDigest      string
	reviewPolicy      ReviewPolicy
	prefixes          map[string]string // prefix -> entity kind (ticket|requirement)
	leaseStateDir     string
	leaseTTLNS        uint64
	maxReports        int
	maxAgeDays        int
	maxComputeEvents  int
	maxComputeAgeDays int
	maxCommandEvents  int
	maxCommandAgeDays int
	maxQuotaSnapshots int
	clock             Clock
	bootstrap         bool
	runner            Execution
	// beforeMaterialise is intentionally nil in production; tests use it to
	// observe the receipt-before-file ordering at the crash boundary.
	beforeMaterialise func(Intent) error
	// beforeLeaseCommit is a test-only crash hook for the DB/token ordering
	// boundary; production leaves it nil.
	beforeLeaseCommit func() error
	// beforeCommit is a test-only seam immediately before COMMIT for proving
	// cross-handle event allocation/commit ordering. Production leaves it nil.
	beforeCommit func()
	// afterLeaseBegin is a test-only observation hook for the lease clock
	// sampling boundary; production leaves it nil.
	afterLeaseBegin func()
	// beforeReapCAS is a test-only seam between advisory expiry detection and
	// the guarded reaping transaction; production leaves it nil.
	beforeReapCAS func(string)
	// Supervisor-lease equivalents of the D1 lease seams. Production leaves
	// both nil; tests use them to prove clock-after-lock and the reap CAS race.
	afterSupervisorLeaseBegin func()
	beforeSupervisorReapCAS   func(string)
	// afterSupervisorReapBegin fires inside each reaping transaction, after the
	// writer lock is held and before the clock is re-sampled; tests use it to
	// prove the reap CAS samples the clock under the lock. Production leaves nil.
	afterSupervisorReapBegin func()
	// afterJournalFlushEvent is a test-only cancellation seam between
	// successfully journaled events; production leaves it nil.
	afterJournalFlushEvent func(EventKey)
	// beforeRebuildFindingReconstruct is a test-only seam for the finding
	// scan/reconstruct race boundary; production leaves it nil.
	beforeRebuildFindingReconstruct func()
	// findingsMigrationHook is a test-only seam for crash points between
	// transactional schema-migration statements; production leaves it nil.
	findingsMigrationHook func(string) error
	// allocationMigrationHook is a test-only seam for crash points between the
	// two-table entity-kind schema migration statements; production leaves it nil.
	allocationMigrationHook func(string) error
	// beforeSearchQuery is a test-only seam between search index replacement
	// and the MATCH query; production leaves it nil.
	beforeSearchQuery func() error
	// beforeSearchReconcileCommit is a test-only seam after the canonical scan
	// and before its replacement transaction; production leaves it nil.
	beforeSearchReconcileCommit func()
	// traceabilitySnapshotHook is a test-only seam used to reproduce a mutation
	// during the tracked-file snapshot validation window.
	traceabilitySnapshotHook func()
	// testReportInsertHook is a test-only crash seam for proving report/result
	// atomicity. Production leaves it nil.
	testReportInsertHook func(int) error
	// findingScanHook is a test-only seam fired once per finding-file scan, used
	// to prove a gauge reads findings a single time. Production leaves it nil.
	findingScanHook func()
	// rebuildPhaseBHook is a test-only failure seam inside the atomic rebuild
	// transaction, after projection replacement and before deferred findings.
	rebuildPhaseBHook func() error
}

type Intent struct {
	ProjectID    string
	WorktreeID   string
	Seq          int64
	Path         string
	Kind         IntentKind
	Precondition string
	Intended     []byte
	Ticket       domain.Ticket
	Finding      domain.Finding
	AllocationID string
	Receipt      AllocationReceipt
}

type IntentKind string

const (
	IntentKindTicketFile      IntentKind = "ticket-file"
	IntentKindFindingFile     IntentKind = "finding-file"
	IntentKindRequirementFile IntentKind = "requirement-file"
)

type AllocationReceipt struct {
	ProjectID  string `json:"project_id"`
	WorktreeID string `json:"worktree_id"`
	ID         string `json:"id"`
	Path       string `json:"path"`
	Seq        int64  `json:"seq"`
	At         string `json:"at"`
	State      string `json:"state"`
	// Kind is the entity kind of the allocation. A pre-M9 receipt has no kind
	// field, which decodes to "" and is normalised to ticket on read.
	Kind string `json:"kind,omitempty"`
}

type EventKey struct {
	ProjectID string `json:"project_id"`
	Seq       int64  `json:"seq"`
}

type registryEntry struct {
	ProjectID  string `json:"project_id"`
	CommonDir  string `json:"common_dir"`
	WorktreeID string `json:"worktree_id"`
	Root       string `json:"root"`
	// Prefixes lists ticket-kind prefixes; RequirementPrefixes lists
	// requirement-kind prefixes. A pre-M9 breadcrumb has no requirement_prefixes
	// field, which decodes to nil ⇒ every recorded prefix is ticket-kind (the
	// legacy decoder). omitempty keeps old-shaped output when there are none.
	Prefixes            []string `json:"prefixes"`
	RequirementPrefixes []string `json:"requirement_prefixes,omitempty"`
	At                  string   `json:"at"`
}

// RegistryEntry is the discovery-safe public projection of a registry
// breadcrumb. At is deliberately omitted because registry discovery needs the
// recorded identity and configuration only, not append chronology.
type RegistryEntry struct {
	ProjectID           string   `json:"project_id"`
	CommonDir           string   `json:"common_dir"`
	WorktreeID          string   `json:"worktree_id"`
	Root                string   `json:"root"`
	Prefixes            []string `json:"prefixes"`
	RequirementPrefixes []string `json:"requirement_prefixes,omitempty"`
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
	Root       string
	Path       string
	Ticket     domain.Ticket
	Body       string
	Digest     string
}

// Open is the in-process convenience: it opens a private DB and one scope which
// owns that DB. Daemon callers use OpenDB and NewScope separately.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Root == "" || opts.CommonDir == "" || opts.DBPath == "" || opts.RegistryPath == "" || opts.ProjectID == "" || opts.WorktreeID == "" {
		return nil, errors.New("E_CONFIG_INVALID: store options are incomplete")
	}
	db, err := openDBContext(ctx, opts.DBPath, opts.RegistryPath)
	if err != nil {
		return nil, err
	}
	scopeOpts := ScopeOptions{
		Root: opts.Root, CommonDir: opts.CommonDir, GitDir: opts.GitDir,
		ProjectID: opts.ProjectID, WorktreeID: opts.WorktreeID,
		ProjectSlug: opts.ProjectSlug, Prefixes: opts.Prefixes,
		RequirementPrefixes: opts.RequirementPrefixes, ReviewPolicy: opts.ReviewPolicy,
		LeaseStateDir: opts.LeaseStateDir, LeaseTTLNS: opts.LeaseTTLNS,
		MaxReports: opts.MaxReports, MaxAgeDays: opts.MaxAgeDays,
		MaxComputeEvents: opts.MaxComputeEvents, MaxComputeAgeDays: opts.MaxComputeAgeDays,
		MaxCommandEvents: opts.MaxCommandEvents, MaxCommandAgeDays: opts.MaxCommandAgeDays,
		MaxQuotaSnapshots: opts.MaxQuotaSnapshots, Clock: opts.Clock,
	}
	// GitDir did not exist in the pre-M21 Options API. Keep that source-level
	// compatibility for isolated in-process callers; production discovery and
	// every daemon descriptor provide GitDir and take the checked path.
	s, err := newScopeContext(ctx, db, scopeOpts, opts.GitDir != "", true)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.owner = db
	return s, nil
}

// OpenReadOnly opens an existing state database as a query-only WAL reader.
// It deliberately performs no schema initialisation, directory creation, or
// worktree registration. The returned Store owns only this read connection.
func OpenReadOnly(dbPath string, opts ScopeOptions) (*Store, error) {
	if dbPath == "" || opts.Root == "" || opts.CommonDir == "" || opts.GitDir == "" || opts.ProjectSlug == "" {
		return nil, errors.New("E_CONFIG_INVALID: read-only store options are incomplete")
	}
	reviewPolicy, err := ValidateReviewPolicy(opts.ReviewPolicy)
	if err != nil {
		return nil, err
	}
	root, err := canonicalPath(opts.Root)
	if err != nil {
		return nil, err
	}
	common, err := canonicalPath(opts.CommonDir)
	if err != nil {
		return nil, err
	}
	gitDir, err := canonicalPath(opts.GitDir)
	if err != nil {
		return nil, err
	}
	projectID, worktreeID := hashPath(common), hashPath(gitDir)
	if opts.ProjectID != "" && opts.ProjectID != projectID {
		return nil, fmt.Errorf("E_DAEMON_PROJECT_INVALID: project identity %q does not match canonical common directory", opts.ProjectID)
	}
	if opts.WorktreeID != "" && opts.WorktreeID != worktreeID {
		return nil, fmt.Errorf("E_DAEMON_PROJECT_INVALID: worktree identity %q does not match canonical git directory", opts.WorktreeID)
	}
	if err := domain.ValidateProjectSlug(opts.ProjectSlug); err != nil {
		return nil, err
	}
	dbPath, err = filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"}).String()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(context.Background()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	owner := &DB{db: conn, dbPath: dbPath, registryPath: filepath.Join(filepath.Dir(dbPath), "registry.jsonl")}
	s := &Store{
		db: conn, owner: owner, root: root, commonDir: common,
		auditDir: filepath.Join(common, "aira"), dbPath: dbPath, registryPath: owner.registryPath,
		projectID: projectID, worktreeID: worktreeID, projectSlug: opts.ProjectSlug,
		configDigest: opts.ConfigDigest, reviewPolicy: reviewPolicy, prefixes: map[string]string{},
		leaseStateDir: opts.LeaseStateDir, leaseTTLNS: opts.LeaseTTLNS,
		maxReports: opts.MaxReports, maxAgeDays: opts.MaxAgeDays,
		maxComputeEvents: opts.MaxComputeEvents, maxComputeAgeDays: opts.MaxComputeAgeDays,
		maxCommandEvents: opts.MaxCommandEvents, maxCommandAgeDays: opts.MaxCommandAgeDays,
		maxQuotaSnapshots: opts.MaxQuotaSnapshots, clock: opts.Clock, bootstrap: opts.Bootstrap,
	}
	if s.maxReports == 0 {
		s.maxReports = 5000
	}
	if s.maxComputeEvents == 0 {
		s.maxComputeEvents = 20000
	}
	if s.maxCommandEvents == 0 {
		s.maxCommandEvents = 50000
	}
	if s.maxQuotaSnapshots == 0 {
		s.maxQuotaSnapshots = 5000
	}
	if s.maxReports < 1 || s.maxAgeDays < 0 || s.maxComputeEvents < 1 || s.maxComputeAgeDays < 0 || s.maxCommandEvents < 1 || s.maxCommandAgeDays < 0 || s.maxQuotaSnapshots < 1 {
		_ = conn.Close()
		return nil, errors.New("E_CONFIG_INVALID: telemetry retention is invalid")
	}
	if s.leaseStateDir == "" {
		resolved, err := DefaultStateDir()
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		s.leaseStateDir = resolved
	}
	if s.leaseTTLNS == 0 {
		s.leaseTTLNS = defaultLeaseTTLNS
	}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	for _, prefix := range opts.Prefixes {
		if !validPrefix(prefix) {
			_ = conn.Close()
			return nil, fmt.Errorf("E_ID_INVALID: invalid prefix %q", prefix)
		}
		s.prefixes[strings.ToUpper(prefix)] = kindTicket
	}
	for _, prefix := range opts.RequirementPrefixes {
		if !validPrefix(prefix) {
			_ = conn.Close()
			return nil, fmt.Errorf("E_ID_INVALID: invalid prefix %q", prefix)
		}
		up := strings.ToUpper(prefix)
		if existing, duplicate := s.prefixes[up]; duplicate && existing != kindRequirement {
			_ = conn.Close()
			return nil, fmt.Errorf("E_PREFIX_OWNERSHIP_CONFLICT: prefix %q registered as both ticket and requirement", up)
		}
		s.prefixes[up] = kindRequirement
	}
	return s, nil
}

// OpenDB opens and initialises the one machine-wide connection. It pins the DB
// and registry paths for its lifetime and retains the bounded transient-I/OERR
// retry used before the M21 split.
func OpenDB(dbPath, registryPath string) (*DB, error) {
	return openDBContext(context.Background(), dbPath, registryPath)
}

func openDBContext(ctx context.Context, dbPath, registryPath string) (*DB, error) {
	var lastErr error
	for attempt := 0; attempt < storeOpenRetries; attempt++ {
		db, err := openOnceFn(ctx, dbPath, registryPath)
		if err == nil {
			return db, nil
		}
		lastErr = err
		if !isTransientDiskIOError(err) || ctx.Err() != nil {
			return nil, err
		}
		if attempt == storeOpenRetries-1 {
			break // budget exhausted — do not back off after the final attempt.
		}
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(storeOpenBackoff):
		}
	}
	return nil, lastErr
}

// openOnceFn is the single-shot DB open primitive OpenDB retries.
var openOnceFn = openOnce

// registerOnceFn is the single-shot per-worktree registration primitive which
// NewScope retries. It is a package variable solely for typed fault injection in
// the transient-I/OERR regression test.
var registerOnceFn = func(ctx context.Context, s *Store) error { return s.Register(ctx) }

const (
	// storeOpenRetries bounds the retry budget for a transient disk I/O error.
	storeOpenRetries = 3
	// storeOpenBackoff is the pause between transient-failure retries.
	storeOpenBackoff = 50 * time.Millisecond
)

// sqliteCoder is satisfied by *modernc.org/sqlite.Error, which exposes the numeric
// SQLite result code. Classifying by code (not message text) is what keeps an
// unrelated error whose text merely contains "disk I/O error" from being retried.
type sqliteCoder interface{ Code() int }

// isTransientDiskIOError reports whether err is a SQLite disk I/O error — the
// SQLITE_IOERR family (its extended codes, e.g. SQLITE_IOERR_WRITE=778, all share
// primary code SQLITE_IOERR). That is the one class safe to retry here;
// configuration, constraint, and corruption errors are deliberately not matched.
func isTransientDiskIOError(err error) bool {
	var coder sqliteCoder
	if !errors.As(err, &coder) {
		return false
	}
	return coder.Code()&0xff == sqlite3.SQLITE_IOERR
}

func openOnce(ctx context.Context, dbPath, registryPath string) (*DB, error) {
	if dbPath == "" || registryPath == "" {
		return nil, errors.New("E_CONFIG_INVALID: DB paths are incomplete")
	}
	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	registry, err := filepath.Abs(registryPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(registry), 0o755); err != nil {
		return nil, err
	}
	// secure_delete overwrites freed content so a redacted rant body/note does
	// not linger as recoverable bytes in freelist pages after the logical scrub.
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	db := &DB{db: conn, dbPath: dbPath, registryPath: registry}
	if err := (&Store{db: conn}).initDB(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// NewScope creates a checked worktree view over db. Project and worktree
// identities are always recomputed from canonical paths; supplied identities
// are evidence to validate, never authority.
func NewScope(db *DB, opts ScopeOptions) (*Store, error) {
	return newScopeContext(context.Background(), db, opts, true, true)
}

// NewUnregisteredScope builds a checked view without Register side effects.
// Lifecycle durability checks use it while the daemon's project exclusion is
// active, so checking a project can never resurrect it.
func NewUnregisteredScope(db *DB, opts ScopeOptions) (*Store, error) {
	return newScopeContext(context.Background(), db, opts, true, false)
}

func newScopeContext(ctx context.Context, db *DB, opts ScopeOptions, checkIdentity, register bool) (*Store, error) {
	if db == nil || db.db == nil {
		return nil, errors.New("E_CONFIG_INVALID: DB is unavailable")
	}
	if opts.Root == "" || opts.CommonDir == "" || opts.ProjectSlug == "" || (checkIdentity && opts.GitDir == "") {
		return nil, errors.New("E_CONFIG_INVALID: scope options are incomplete")
	}
	reviewPolicy, err := ValidateReviewPolicy(opts.ReviewPolicy)
	if err != nil {
		return nil, err
	}
	root, err := canonicalPath(opts.Root)
	if err != nil {
		return nil, err
	}
	common, err := canonicalPath(opts.CommonDir)
	if err != nil {
		return nil, err
	}
	gitDir := opts.GitDir
	if gitDir == "" {
		gitDir = root
	}
	gitDir, err = canonicalPath(gitDir)
	if err != nil {
		return nil, err
	}
	projectID, worktreeID := hashPath(common), hashPath(gitDir)
	if checkIdentity {
		if opts.ProjectID != "" && opts.ProjectID != projectID {
			return nil, fmt.Errorf("E_DAEMON_PROJECT_INVALID: project identity %q does not match canonical common directory", opts.ProjectID)
		}
		if opts.WorktreeID != "" && opts.WorktreeID != worktreeID {
			return nil, fmt.Errorf("E_DAEMON_PROJECT_INVALID: worktree identity %q does not match canonical git directory", opts.WorktreeID)
		}
	} else {
		projectID, worktreeID = opts.ProjectID, opts.WorktreeID
	}
	if err := domain.ValidateProjectSlug(opts.ProjectSlug); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(common, "aira", "locks"), 0o755); err != nil {
		return nil, err
	}
	if err := syncDir(common); err != nil {
		return nil, err
	}
	s := &Store{
		db: db.db, root: root, commonDir: common, auditDir: filepath.Join(common, "aira"),
		dbPath: db.dbPath, registryPath: db.registryPath, projectID: projectID,
		worktreeID: worktreeID, projectSlug: opts.ProjectSlug, configDigest: opts.ConfigDigest,
		reviewPolicy: reviewPolicy, prefixes: map[string]string{},
		leaseStateDir: opts.LeaseStateDir, leaseTTLNS: opts.LeaseTTLNS,
		maxReports: opts.MaxReports, maxAgeDays: opts.MaxAgeDays,
		maxComputeEvents: opts.MaxComputeEvents, maxComputeAgeDays: opts.MaxComputeAgeDays,
		maxCommandEvents: opts.MaxCommandEvents, maxCommandAgeDays: opts.MaxCommandAgeDays,
		maxQuotaSnapshots: opts.MaxQuotaSnapshots, clock: opts.Clock, bootstrap: opts.Bootstrap,
	}
	if s.maxReports == 0 {
		s.maxReports = 5000
	}
	if s.maxComputeEvents == 0 {
		s.maxComputeEvents = 20000
	}
	if s.maxCommandEvents == 0 {
		s.maxCommandEvents = 50000
	}
	if s.maxQuotaSnapshots == 0 {
		s.maxQuotaSnapshots = 5000
	}
	if s.maxReports < 1 || s.maxAgeDays < 0 || s.maxComputeEvents < 1 || s.maxComputeAgeDays < 0 || s.maxCommandEvents < 1 || s.maxCommandAgeDays < 0 || s.maxQuotaSnapshots < 1 {
		return nil, errors.New("E_CONFIG_INVALID: telemetry retention is invalid")
	}
	if s.leaseStateDir == "" {
		resolved, err := DefaultStateDir()
		if err != nil {
			return nil, err
		}
		s.leaseStateDir = resolved
	}
	if s.leaseTTLNS == 0 {
		s.leaseTTLNS = defaultLeaseTTLNS
	}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	for _, prefix := range opts.Prefixes {
		if !validPrefix(prefix) {
			return nil, fmt.Errorf("E_ID_INVALID: invalid prefix %q", prefix)
		}
		s.prefixes[strings.ToUpper(prefix)] = kindTicket
	}
	for _, prefix := range opts.RequirementPrefixes {
		if !validPrefix(prefix) {
			return nil, fmt.Errorf("E_ID_INVALID: invalid prefix %q", prefix)
		}
		up := strings.ToUpper(prefix)
		if existing, dup := s.prefixes[up]; dup && existing != kindRequirement {
			// Prefixes are disjoint by kind: a prefix may not be both a ticket
			// and a requirement prefix.
			return nil, fmt.Errorf("E_PREFIX_OWNERSHIP_CONFLICT: prefix %q registered as both ticket and requirement", up)
		}
		s.prefixes[up] = kindRequirement
	}
	if !register {
		return s, nil
	}
	var lastErr error
	for attempt := 0; attempt < storeOpenRetries; attempt++ {
		if err := registerOnceFn(ctx, s); err == nil {
			return s, nil
		} else {
			lastErr = err
		}
		if !isTransientDiskIOError(lastErr) || ctx.Err() != nil {
			return nil, lastErr
		}
		if attempt == storeOpenRetries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(storeOpenBackoff):
		}
	}
	return nil, lastErr
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	candidate := abs
	remainder := []string(nil)
	for {
		_, statErr := os.Lstat(candidate)
		if statErr == nil {
			canonical, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(remainder) - 1; index >= 0; index-- {
				canonical = filepath.Join(canonical, remainder[index])
			}
			return filepath.Clean(canonical), nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", statErr
		}
		remainder = append(remainder, filepath.Base(candidate))
		candidate = parent
	}
}

func hashPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

// CanonicalScopeIdentity returns the path-derived identity used by NewScope and
// by daemon-side descriptor validation.
func CanonicalScopeIdentity(commonDir, gitDir string) (string, string, error) {
	common, err := canonicalPath(commonDir)
	if err != nil {
		return "", "", err
	}
	git, err := canonicalPath(gitDir)
	if err != nil {
		return "", "", err
	}
	return hashPath(common), hashPath(git), nil
}

// Close closes only a private DB opened by Open. A Store returned by NewScope
// owns no connection, so its Close is intentionally a no-op.
func (s *Store) Close() error {
	if s == nil || s.owner == nil {
		return nil
	}
	return s.owner.Close()
}

// SetRunner attaches the only process-execution seam used by command gates.
// The store never falls back to os/exec for gate commands.
func (s *Store) SetRunner(execution Execution) { s.runner = execution }

// RunnerConfigured reports whether command-gate execution has been wired. It
// keeps alternate composition paths testable against OpenWithDiagnostics.
func (s *Store) RunnerConfigured() bool { return s != nil && s.runner != nil }

// ProjectID returns the path-derived project identity owned by this scope.
func (s *Store) ProjectID() string { return s.projectID }

// WorktreeID returns the path-derived worktree identity owned by this scope.
func (s *Store) WorktreeID() string { return s.worktreeID }

func (s *Store) initDB(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS projects (
            project_id TEXT PRIMARY KEY, slug TEXT NOT NULL, common_dir TEXT NOT NULL,
            config_digest TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS worktrees (
            project_id TEXT NOT NULL, worktree_id TEXT NOT NULL, root TEXT NOT NULL,
            active INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL,
            PRIMARY KEY(project_id, worktree_id),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS prefix_ownership (
            prefix TEXT PRIMARY KEY, project_id TEXT NOT NULL, registered_seq INTEGER NOT NULL,
            kind TEXT NOT NULL DEFAULT 'ticket',
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS event_counters (
            project_id TEXT PRIMARY KEY, next_seq INTEGER NOT NULL,
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS id_counters (
            project_id TEXT NOT NULL, prefix TEXT NOT NULL, next_number INTEGER NOT NULL,
            PRIMARY KEY(project_id, prefix),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS allocations (
            project_id TEXT NOT NULL, prefix TEXT NOT NULL, number INTEGER NOT NULL,
            worktree_id TEXT NOT NULL, state TEXT NOT NULL, path TEXT NOT NULL,
            seq INTEGER NOT NULL, kind TEXT NOT NULL DEFAULT 'ticket',
            PRIMARY KEY(project_id, prefix, number),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS outbox (
            project_id TEXT NOT NULL, seq INTEGER NOT NULL, worktree_id TEXT NOT NULL,
            path TEXT NOT NULL, verb TEXT NOT NULL, precondition_digest TEXT NOT NULL,
            intended_digest TEXT NOT NULL, intended_bytes BLOB, materialised INTEGER NOT NULL DEFAULT 0,
            journaled INTEGER NOT NULL DEFAULT 0, allocation_id TEXT NOT NULL DEFAULT '',
            kind TEXT NOT NULL DEFAULT 'ticket-file',
            PRIMARY KEY(project_id, seq),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		// One truth per intent (AIRA-73): `materialised` alone says whether an
		// intent is still outstanding. The former companion column `resolution`
		// was never written by any code path, so `resolution IS NULL` was a
		// tautology on every row and the predicate below selects exactly the
		// same set it always did.
		`CREATE UNIQUE INDEX IF NOT EXISTS unresolved_path_intent
            ON outbox(project_id, worktree_id, path)
            WHERE materialised = 0`,
		`CREATE TABLE IF NOT EXISTS events (
            project_id TEXT NOT NULL, seq INTEGER NOT NULL, at_wall TEXT NOT NULL,
            actor TEXT NOT NULL, verb TEXT NOT NULL, target TEXT NOT NULL,
            payload_digest TEXT NOT NULL, journaled INTEGER NOT NULL DEFAULT 0,
            PRIMARY KEY(project_id, seq),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS tickets (
            project_id TEXT NOT NULL, worktree_id TEXT NOT NULL, id TEXT NOT NULL,
            path TEXT NOT NULL, digest TEXT NOT NULL, status TEXT NOT NULL, hold INTEGER NOT NULL,
            title TEXT NOT NULL, kind TEXT NOT NULL, severity TEXT NOT NULL,
	            PRIMARY KEY(project_id, worktree_id, id),
	            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
	        )`,
		`CREATE TABLE IF NOT EXISTS requirements (
            project_id TEXT NOT NULL, worktree_id TEXT NOT NULL, id TEXT NOT NULL,
            path TEXT NOT NULL, digest TEXT NOT NULL, status TEXT NOT NULL, text TEXT NOT NULL DEFAULT '',
            PRIMARY KEY(project_id, worktree_id, id),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`,
		`CREATE TABLE IF NOT EXISTS relations (
	            project_id TEXT NOT NULL, worktree_id TEXT NOT NULL, kind TEXT NOT NULL,
	            from_id TEXT NOT NULL, to_id TEXT NOT NULL, canonical_file TEXT NOT NULL,
	            PRIMARY KEY(project_id, worktree_id, kind, from_id, to_id),
	            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
	        )`,
		`CREATE TABLE IF NOT EXISTS findings (
			project_id TEXT NOT NULL, worktree_id TEXT NOT NULL DEFAULT '', finding_key TEXT NOT NULL,
			subtype TEXT NOT NULL DEFAULT 'reconciliation', code TEXT NOT NULL DEFAULT '',
			subject TEXT NOT NULL DEFAULT '', details TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
			ticket_id TEXT NOT NULL DEFAULT '', category TEXT NOT NULL DEFAULT '', severity TEXT NOT NULL DEFAULT '',
			verdict TEXT NOT NULL DEFAULT '', disposition TEXT NOT NULL DEFAULT 'open', source TEXT NOT NULL DEFAULT '',
			file TEXT NOT NULL DEFAULT '', line INTEGER NOT NULL DEFAULT 0, requirement_id TEXT NOT NULL DEFAULT '',
			waiver_reason TEXT NOT NULL DEFAULT '', waiver_actor TEXT NOT NULL DEFAULT '', canonical_file TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(project_id, worktree_id, finding_key),
			FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
	        )`,
		`CREATE TABLE IF NOT EXISTS leases (
            project_id TEXT NOT NULL, ticket_id TEXT NOT NULL, state TEXT NOT NULL,
            generation INTEGER NOT NULL, holder_token_hash TEXT, boot_id TEXT,
            last_heartbeat_mono_ns INTEGER, ttl_ns INTEGER, actor TEXT, worktree_id TEXT,
            PRIMARY KEY(project_id, ticket_id),
            CHECK (state IN ('free', 'held')),
            CHECK ((state = 'free' AND generation >= 0 AND holder_token_hash IS NULL AND boot_id IS NULL AND
                    last_heartbeat_mono_ns IS NULL AND ttl_ns IS NULL AND actor IS NULL AND worktree_id IS NULL)
	                OR (state = 'held' AND generation >= 1 AND holder_token_hash IS NOT NULL AND length(holder_token_hash) = 43 AND
                    boot_id IS NOT NULL AND length(trim(boot_id)) > 0 AND
                    last_heartbeat_mono_ns IS NOT NULL AND last_heartbeat_mono_ns >= 0 AND
                    ttl_ns IS NOT NULL AND ttl_ns > 0 AND actor IS NOT NULL AND length(trim(actor)) > 0 AND
                    worktree_id IS NOT NULL AND length(trim(worktree_id)) > 0)),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
	        )`,
		`CREATE TABLE IF NOT EXISTS supervisor_leases (
			project_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('held','lapsed')),
			generation INTEGER NOT NULL CHECK (generation >= 1),
			holder_token_hash TEXT NOT NULL,
			holder_pid INTEGER NOT NULL,
			holder_start_tick INTEGER NOT NULL,
			holder_boot_id TEXT NOT NULL,
			last_heartbeat_mono_ns INTEGER NOT NULL,
			ttl_ns INTEGER NOT NULL CHECK (ttl_ns > 0),
			actor TEXT NOT NULL,
			worktree_id TEXT NOT NULL,
			PRIMARY KEY (project_id, run_id),
			FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS confine_peak_history (
		    signature TEXT NOT NULL, peak_rss INTEGER, oom INTEGER NOT NULL, at TEXT NOT NULL,
		    CHECK(length(signature)>0), CHECK(peak_rss IS NULL OR peak_rss>0), CHECK(oom IN (0,1))
		)`,
		`CREATE INDEX IF NOT EXISTS confine_peak_history_signature
		    ON confine_peak_history(signature)`,
		`CREATE TABLE IF NOT EXISTS area_hints (
            project_id TEXT NOT NULL, ticket_id TEXT NOT NULL, worktree_id TEXT NOT NULL,
            generation INTEGER NOT NULL DEFAULT 0, glob TEXT NOT NULL,
            PRIMARY KEY(project_id, ticket_id, worktree_id, glob),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
	        )`,
		`CREATE TABLE IF NOT EXISTS gates (
		    project_id TEXT NOT NULL, gate_id TEXT NOT NULL, definition_digest TEXT NOT NULL,
		    definition_json TEXT NOT NULL, PRIMARY KEY(project_id, gate_id),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS gate_results (
		    project_id TEXT NOT NULL, gate_id TEXT NOT NULL, subject TEXT NOT NULL,
		    seq INTEGER NOT NULL, verdict TEXT NOT NULL, code TEXT NOT NULL,
		    trusted INTEGER NOT NULL, suspect INTEGER NOT NULL, record_json TEXT NOT NULL,
		    PRIMARY KEY(project_id, gate_id, subject),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS gate_proofs (
		    project_id TEXT NOT NULL, seq INTEGER NOT NULL, gate_id TEXT NOT NULL,
		    record_json TEXT NOT NULL, PRIMARY KEY(project_id, seq),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS gate_attestations (
		    project_id TEXT NOT NULL, seq INTEGER NOT NULL, gate_id TEXT NOT NULL,
		    record_json TEXT NOT NULL, PRIMARY KEY(project_id, seq),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS test_report_counter (
		    project_id TEXT PRIMARY KEY, next_number INTEGER NOT NULL, next_seq INTEGER NOT NULL,
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS rant_counter (
		    project_id TEXT PRIMARY KEY, next_number INTEGER NOT NULL CHECK(next_number >= 1),
		    next_seq INTEGER NOT NULL CHECK(next_seq >= 1),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS rants (
		    project_id TEXT NOT NULL, id TEXT NOT NULL, body TEXT NOT NULL,
		    severity TEXT NOT NULL DEFAULT '' CHECK(severity IN ('','papercut','annoyance','blocker')),
		    idempotency_key TEXT, actor TEXT NOT NULL, session TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		    observed_at TEXT NOT NULL DEFAULT '', received_at TEXT NOT NULL, resolver_version TEXT NOT NULL DEFAULT '',
		    seq INTEGER NOT NULL CHECK(seq >= 1), redacted INTEGER NOT NULL DEFAULT 0 CHECK(redacted IN (0,1)),
		    PRIMARY KEY(project_id,id), UNIQUE(project_id,seq), UNIQUE(project_id,idempotency_key),
		    CHECK(length(CAST(body AS BLOB)) BETWEEN 1 AND 8192), CHECK(instr(body,char(0)) = 0),
		    CHECK(idempotency_key IS NULL OR (length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 256 AND instr(idempotency_key,char(0)) = 0)),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS rant_tags (
		    project_id TEXT NOT NULL, rant_id TEXT NOT NULL, tag TEXT NOT NULL,
		    PRIMARY KEY(project_id,rant_id,tag),
		    CHECK(length(CAST(tag AS BLOB)) BETWEEN 1 AND 64), CHECK(instr(tag,char(0)) = 0),
		    FOREIGN KEY(project_id,rant_id) REFERENCES rants(project_id,id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS rant_git_context (
		    project_id TEXT NOT NULL, rant_id TEXT NOT NULL, field TEXT NOT NULL,
		    value TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
		    PRIMARY KEY(project_id,rant_id,field),
		    CHECK(field IN ('repo_root','worktree_path','worktree_id','head_hash','head_ref','remote_url')),
		    CHECK(status IN ('value','none','unevaluated','mismatch')),
		    CHECK((status IN ('value','mismatch') AND length(value)>0) OR (status IN ('none','unevaluated') AND value='')),
		    FOREIGN KEY(project_id,rant_id) REFERENCES rants(project_id,id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS rant_context_refs (
		    project_id TEXT NOT NULL, rant_id TEXT NOT NULL, kind TEXT NOT NULL, ref_id TEXT NOT NULL,
		    PRIMARY KEY(project_id,rant_id,kind,ref_id), CHECK(kind IN ('run','ticket','finding','gate')),
		    CHECK(length(ref_id)>0), CHECK(instr(ref_id,char(0)) = 0),
		    FOREIGN KEY(project_id,rant_id) REFERENCES rants(project_id,id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS rant_reviews (
		    project_id TEXT NOT NULL, review_id INTEGER PRIMARY KEY AUTOINCREMENT, rant_id TEXT NOT NULL,
		    reviewer TEXT NOT NULL, at TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT '',
		    resolved_kind TEXT, resolved_id TEXT,
		    CHECK(length(reviewer)>0), CHECK(length(at)>0),
		    CHECK(outcome IN ('','actioned','planned','duplicate','wont-fix','needs-evidence')),
		    CHECK((resolved_kind IS NULL AND resolved_id IS NULL) OR
		          (resolved_kind IN ('run','ticket','finding','gate') AND resolved_id IS NOT NULL AND length(resolved_id)>0)),
		    CHECK(length(CAST(note AS BLOB)) <= 8192), CHECK(instr(note,char(0)) = 0),
		    FOREIGN KEY(project_id,rant_id) REFERENCES rants(project_id,id) ON DELETE CASCADE
		)`,
		// rant_reviews_no_update is created/upgraded atomically by
		// ensureRantReviewTriggerCurrent after this loop, not here, so a stale
		// pre-redaction-exception definition can be replaced without a window in
		// which append-only protection is absent.
		`CREATE TRIGGER IF NOT EXISTS rant_reviews_no_delete BEFORE DELETE ON rant_reviews
		 BEGIN SELECT RAISE(ABORT,'rant reviews are append-only'); END`,
		`CREATE TABLE IF NOT EXISTS test_reports (
		    project_id TEXT NOT NULL, id TEXT NOT NULL, ticket_id TEXT NOT NULL DEFAULT '',
		    phase TEXT NOT NULL DEFAULT '', "commit" TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
		    worktree_id TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '',
		    at TEXT NOT NULL, run_ref TEXT NOT NULL DEFAULT '', suite_id TEXT NOT NULL DEFAULT '',
		    runner TEXT NOT NULL DEFAULT '', config TEXT NOT NULL DEFAULT '', env_digest TEXT NOT NULL DEFAULT '',
		    shard TEXT NOT NULL, retry_index INTEGER NOT NULL DEFAULT 0, parser_complete INTEGER NOT NULL,
		    coverage_pct REAL, lines_covered INTEGER, lines_total INTEGER, format TEXT NOT NULL,
		    source_digest TEXT NOT NULL, at_seq INTEGER NOT NULL, pinned INTEGER NOT NULL DEFAULT 0,
		    PRIMARY KEY(project_id, id),
		    UNIQUE(project_id, source_digest, format, "commit", suite_id, config, env_digest, shard, retry_index),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS test_report_results (
		    project_id TEXT NOT NULL, report_id TEXT NOT NULL, name TEXT NOT NULL,
		    outcome TEXT NOT NULL, duration_ns INTEGER, message TEXT NOT NULL DEFAULT '',
		    PRIMARY KEY(project_id, report_id, name),
		    FOREIGN KEY(project_id, report_id) REFERENCES test_reports(project_id, id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS compute_event_counter (
		    project_id TEXT PRIMARY KEY, next_number INTEGER NOT NULL, next_seq INTEGER NOT NULL,
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS compute_events (
		    project_id TEXT NOT NULL, id TEXT NOT NULL, ticket_id TEXT NOT NULL DEFAULT '',
		    phase TEXT NOT NULL DEFAULT '', model TEXT NOT NULL, provider TEXT NOT NULL,
		    at TEXT NOT NULL, session TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '',
		    source TEXT NOT NULL, fresh_input INTEGER, cache_read INTEGER, cache_write INTEGER,
		    output INTEGER, reasoning INTEGER, reported_total INTEGER, cost_usd REAL,
		    conservation TEXT NOT NULL, reasoning_subset INTEGER NOT NULL DEFAULT 0,
		    wall_ms INTEGER, cpu_user INTEGER, cpu_sys INTEGER, peak_rss INTEGER,
		    head_hash TEXT NOT NULL DEFAULT '', head_hash_status TEXT NOT NULL DEFAULT 'unevaluated',
		    head_ref TEXT NOT NULL DEFAULT '', head_ref_status TEXT NOT NULL DEFAULT 'unevaluated',
		    worktree_id TEXT NOT NULL DEFAULT '', worktree_id_status TEXT NOT NULL DEFAULT 'unevaluated',
		    at_seq INTEGER NOT NULL,
		    PRIMARY KEY(project_id, id),
		    CHECK(head_hash_status IN ('value','none','unevaluated','mismatch')),
		    CHECK(head_ref_status IN ('value','none','unevaluated','mismatch')),
		    CHECK(worktree_id_status IN ('value','none','unevaluated','mismatch')),
		    CHECK((head_hash_status IN ('value','mismatch') AND length(head_hash)>0) OR (head_hash_status IN ('none','unevaluated') AND head_hash='')),
		    CHECK((head_ref_status IN ('value','mismatch') AND length(head_ref)>0) OR (head_ref_status IN ('none','unevaluated') AND head_ref='')),
		    CHECK((worktree_id_status IN ('value','mismatch') AND length(worktree_id)>0) OR (worktree_id_status IN ('none','unevaluated') AND worktree_id='')),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS command_event_counter (
		    project_id TEXT PRIMARY KEY, next_number INTEGER NOT NULL, next_seq INTEGER NOT NULL,
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS command_events (
		    project_id TEXT NOT NULL, id TEXT NOT NULL, at TEXT NOT NULL, at_seq INTEGER NOT NULL,
		    key TEXT NOT NULL, key_source TEXT NOT NULL, program TEXT NOT NULL DEFAULT '',
		    argv_preview TEXT NOT NULL DEFAULT '', argv_digest TEXT NOT NULL DEFAULT '',
		    prefix_preview TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, exit_code INTEGER,
		    signal TEXT NOT NULL DEFAULT '', wall_ms INTEGER, ticket_id TEXT NOT NULL DEFAULT '',
		    phase TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '',
		    cwd TEXT NOT NULL DEFAULT '',
		    head_hash TEXT NOT NULL DEFAULT '', head_hash_status TEXT NOT NULL DEFAULT 'unevaluated',
		    head_ref TEXT NOT NULL DEFAULT '', head_ref_status TEXT NOT NULL DEFAULT 'unevaluated',
		    worktree_id TEXT NOT NULL DEFAULT '', worktree_id_status TEXT NOT NULL DEFAULT 'unevaluated',
		    PRIMARY KEY(project_id,id),
		    CHECK(status IN ('exited','signalled','timeout','launch-failed','unknown')),
		    CHECK(key_source IN ('label','program-subcommand','program')),
		    CHECK((status='exited' AND exit_code IS NOT NULL AND signal='' AND wall_ms IS NOT NULL)
		       OR (status='signalled' AND exit_code IS NULL AND signal<>'' AND wall_ms IS NOT NULL)
		       OR (status='timeout' AND exit_code IS NULL AND signal<>'' AND wall_ms IS NOT NULL)
		       OR (status='launch-failed' AND exit_code IS NULL AND signal='' AND wall_ms IS NULL)
		       OR (status='unknown' AND exit_code IS NULL AND signal='')),
		    CHECK(head_hash_status IN ('value','none','unevaluated','mismatch')),
		    CHECK(head_ref_status IN ('value','none','unevaluated','mismatch')),
		    CHECK(worktree_id_status IN ('value','none','unevaluated','mismatch')),
		    CHECK((head_hash_status IN ('value','mismatch') AND length(head_hash)>0) OR (head_hash_status IN ('none','unevaluated') AND head_hash='')),
		    CHECK((head_ref_status IN ('value','mismatch') AND length(head_ref)>0) OR (head_ref_status IN ('none','unevaluated') AND head_ref='')),
		    CHECK((worktree_id_status IN ('value','mismatch') AND length(worktree_id)>0) OR (worktree_id_status IN ('none','unevaluated') AND worktree_id='')),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS quota_snapshot_counter (
		    project_id TEXT PRIMARY KEY, next_number INTEGER NOT NULL, next_seq INTEGER NOT NULL,
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS quota_snapshots (
		    project_id TEXT NOT NULL, id TEXT NOT NULL, provider TEXT NOT NULL, at TEXT NOT NULL,
		    window TEXT NOT NULL DEFAULT '', used INTEGER, limit_value INTEGER, remaining INTEGER,
		    reset_at TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, at_seq INTEGER NOT NULL,
		    PRIMARY KEY(project_id, id),
		    FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS ejections (
		    project_id TEXT PRIMARY KEY, ejected_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return translateDBError(err)
		}
	}
	// gate_baselines/gate_baseline_active served only the ratchet gate kind,
	// which never accumulated a production row before being deleted (AIRA-78).
	// No data-preserving migration is needed -- an unconditional, idempotent
	// drop is the whole story, unlike the careful in-place rebuilds below for
	// tables that carry real data.
	for _, table := range []string{"gate_baselines", "gate_baseline_active"} {
		if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return translateDBError(err)
		}
	}
	// The rant_reviews_no_update trigger gained a redaction exception. Upgrading
	// a stale pre-exception definition (which CREATE ... IF NOT EXISTS cannot
	// replace, and which would abort the redaction note scrub) is done atomically
	// so append-only protection is never absent for a concurrent opener.
	if err := s.ensureRantReviewTriggerCurrent(ctx); err != nil {
		return err
	}
	for _, column := range []string{"wall_ms", "cpu_user", "cpu_sys", "peak_rss"} {
		if err := s.ensureColumnAdded(ctx, "compute_events", column,
			`ALTER TABLE compute_events ADD COLUMN `+column+` INTEGER`); err != nil {
			return err
		}
	}
	computeGitColumns := []struct {
		name, definition string
	}{
		{name: "head_hash", definition: `head_hash TEXT NOT NULL DEFAULT ''`},
		{name: "head_hash_status", definition: `head_hash_status TEXT NOT NULL DEFAULT 'unevaluated'
			CHECK(head_hash_status IN ('value','none','unevaluated','mismatch'))
			CHECK((head_hash_status IN ('value','mismatch') AND length(head_hash)>0) OR (head_hash_status IN ('none','unevaluated') AND head_hash=''))`},
		{name: "head_ref", definition: `head_ref TEXT NOT NULL DEFAULT ''`},
		{name: "head_ref_status", definition: `head_ref_status TEXT NOT NULL DEFAULT 'unevaluated'
			CHECK(head_ref_status IN ('value','none','unevaluated','mismatch'))
			CHECK((head_ref_status IN ('value','mismatch') AND length(head_ref)>0) OR (head_ref_status IN ('none','unevaluated') AND head_ref=''))`},
		{name: "worktree_id", definition: `worktree_id TEXT NOT NULL DEFAULT ''`},
		{name: "worktree_id_status", definition: `worktree_id_status TEXT NOT NULL DEFAULT 'unevaluated'
			CHECK(worktree_id_status IN ('value','none','unevaluated','mismatch'))
			CHECK((worktree_id_status IN ('value','mismatch') AND length(worktree_id)>0) OR (worktree_id_status IN ('none','unevaluated') AND worktree_id=''))`},
	}
	for _, column := range computeGitColumns {
		if err := s.ensureColumnAdded(ctx, "compute_events", column.name,
			`ALTER TABLE compute_events ADD COLUMN `+column.definition); err != nil {
			return err
		}
	}
	if err := s.ensureAreaHintsGeneration(ctx); err != nil {
		return err
	}
	if err := s.ensureOutboxKind(ctx); err != nil {
		return err
	}
	// Runs before ensureProjectOwnershipFKs, which recreates tables by
	// replaying their existing DDL: dropping the column first means the FK
	// migration carries forward the current shape, not the deleted one.
	if err := s.ensureOutboxResolutionDropped(ctx); err != nil {
		return err
	}
	if err := s.ensureAllocationKind(ctx); err != nil {
		return err
	}
	if err := s.ensureFindingsSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureSearchFTS(ctx); err != nil {
		return err
	}
	return s.ensureProjectOwnershipFKs(ctx)
}

// rantReviewNoUpdateTriggerDDL is the current append-only trigger with the one
// narrow redaction exception: redaction may scrub a review's free-text note to
// the '[redacted]' sentinel (domain.RedactedRantBody), leaving every other
// column identical. Any other update still aborts.
const rantReviewNoUpdateTriggerDDL = `CREATE TRIGGER rant_reviews_no_update BEFORE UPDATE ON rant_reviews
		 WHEN NEW.review_id IS NOT OLD.review_id OR NEW.project_id IS NOT OLD.project_id
		   OR NEW.rant_id IS NOT OLD.rant_id OR NEW.reviewer IS NOT OLD.reviewer
		   OR NEW.at IS NOT OLD.at OR NEW.outcome IS NOT OLD.outcome
		   OR NEW.resolved_kind IS NOT OLD.resolved_kind OR NEW.resolved_id IS NOT OLD.resolved_id
		   OR NEW.note <> '[redacted]'
		 BEGIN SELECT RAISE(ABORT,'rant reviews are append-only'); END`

// rantReviewTriggerIsCurrent reports whether the installed trigger already
// carries the redaction-exception sentinel, using a single read so the common
// (already-current) path never takes the write lock.
func rantReviewTriggerIsCurrent(sqlText sql.NullString) bool {
	return sqlText.Valid && strings.Contains(sqlText.String, "'"+domain.RedactedRantBody+"'")
}

// ensureRantReviewTriggerCurrent installs the current rant_reviews_no_update
// trigger, replacing a stale pre-redaction-exception definition. A read-only
// fast path avoids the write lock when the trigger is already current (every
// Open would otherwise serialise on it). Otherwise detect, drop, and recreate
// run in one BEGIN IMMEDIATE transaction, re-checked under the lock, so a
// concurrent opener never observes a window with append-only protection absent
// and a partial init cannot leave the trigger missing.
func (s *Store) ensureRantReviewTriggerCurrent(ctx context.Context) error {
	var triggerSQL sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name='rant_reviews_no_update'`).Scan(&triggerSQL)
	if err == nil && rantReviewTriggerIsCurrent(triggerSQL) {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return translateDBError(err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return translateDBError(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return translateDBError(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// Re-check under the write lock: another opener may have installed it since
	// the fast read.
	err = conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name='rant_reviews_no_update'`).Scan(&triggerSQL)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// absent — create below
	case err != nil:
		return translateDBError(err)
	case rantReviewTriggerIsCurrent(triggerSQL):
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return translateDBError(err)
		}
		committed = true
		return nil
	default:
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER rant_reviews_no_update`); err != nil {
			return translateDBError(err)
		}
	}
	if _, err := conn.ExecContext(ctx, rantReviewNoUpdateTriggerDDL); err != nil {
		return translateDBError(err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return translateDBError(err)
	}
	committed = true
	return nil
}

// ensureAreaHintsGeneration adds the area_hints.generation column to a database
// written before it existed. Guarded against the concurrent-opener race — see
// ensureColumnAdded (AIRA-97 Finding 1).
func (s *Store) ensureAreaHintsGeneration(ctx context.Context) error {
	return s.ensureColumnAdded(ctx, "area_hints", "generation",
		`ALTER TABLE area_hints ADD COLUMN generation INTEGER NOT NULL DEFAULT 0`)
}

// ensureOutboxKind adds the outbox.kind column to a database written before it
// existed. Guarded against the concurrent-opener race — see ensureColumnAdded
// (AIRA-97 Finding 1).
func (s *Store) ensureOutboxKind(ctx context.Context) error {
	return s.ensureColumnAdded(ctx, "outbox", "kind",
		`ALTER TABLE outbox ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket-file'`)
}

// ensureColumnAdded is the guarded form of "add this column if it is absent",
// and is the only way this package should reach an ALTER TABLE ... ADD COLUMN.
//
// Several processes Open this one machine-wide database at once (the daemon,
// the CLI fallback through app.OpenWithDiagnostics, a detached supervisor), so
// an unguarded check-then-ALTER lets two of them both observe the column absent
// and both issue the ALTER; the loser gets `duplicate column name` and its
// whole Open fails (AIRA-97 Finding 1). The cheap read below is ONLY a fast
// path for the already-migrated case, so a current database takes no write
// lock; it is not the decision. The decision is re-taken inside a BEGIN
// IMMEDIATE transaction, which holds the write lock across the re-check, so the
// process that raced through the fast path and lost finds the column already
// present and returns without writing.
//
// Same shape as ensureProjectOwnershipFKs and ensureOutboxResolutionDropped,
// which is deliberate: this is one settled pattern, not a third one.
func (s *Store) ensureColumnAdded(ctx context.Context, table, column, ddl string) error {
	present, err := tableHasColumn(ctx, s.db, table, column)
	if err != nil {
		return translateDBError(err)
	}
	if present {
		return nil
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		return addColumnLocked(ctx, conn, table, column, ddl)
	})
}

// addColumnLocked is the write half of ensureColumnAdded, split out so the
// re-check can be exercised directly: calling it against an already-migrated
// connection must be a no-op returning nil, which is exactly what the losing
// side of a two-process race executes. It assumes the caller already holds the
// write lock (BEGIN IMMEDIATE).
func addColumnLocked(ctx context.Context, conn schemaConn, table, column, ddl string) error {
	present, err := tableHasColumn(ctx, conn, table, column)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	_, err = conn.ExecContext(ctx, ddl)
	return err
}

// ensureOutboxResolutionDropped removes the never-written `outbox.resolution`
// column and re-points the partial unique index at `materialised` alone
// (AIRA-73). No code path in any released version ever assigned the column, so
// every existing row holds NULL and the old index predicate
// `materialised = 0 AND resolution IS NULL` covered exactly the rows the new
// `materialised = 0` predicate covers — the migration is set-preserving, not a
// widening.
//
// SQLite refuses to drop a column a partial index references, so the index is
// dropped first and recreated after. Both statements plus the DROP COLUMN run
// in one transaction, so a process that dies mid-migration leaves either the
// full pre-AIRA-73 schema or the full current one — never a table without its
// single-writer index (the M5/M9 non-transactional-migration lesson).
//
// Fail-closed in both directions it can fail. If a row somehow did carry a
// non-NULL resolution, recreating the index could find a duplicate: that aborts
// the transaction and fails Open loudly rather than silently discarding one of
// two conflicting intents (such a row cannot be produced by this codebase, so
// its existence means something outside it wrote to the DB). And if the column
// probe itself cannot be answered, that error is surfaced rather than read as
// "already migrated" — see tableHasColumn.
//
// Concurrency: several processes may Open the same machine-wide database at
// once (the daemon, the CLI fallback via app.OpenWithDiagnostics, a detached
// supervisor). The cheap read below is only a fast path for the already-
// migrated case, so an already-current Open takes no write lock; it is NOT the
// decision. The decision is re-taken inside a BEGIN IMMEDIATE transaction,
// which holds the write lock across the re-check, so a second process that
// raced through the fast path finds the column already gone and returns
// without attempting a DROP that would fail with "no such column" and take
// its whole Open down with it.
func (s *Store) ensureOutboxResolutionDropped(ctx context.Context) error {
	present, err := tableHasColumn(ctx, s.db, "outbox", "resolution")
	if err != nil {
		return translateDBError(err)
	}
	if !present {
		return nil
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		return dropOutboxResolutionLocked(ctx, conn)
	})
}

// dropOutboxResolutionLocked is the write half of ensureOutboxResolutionDropped,
// split out so the re-check can be exercised directly: calling it against an
// already-migrated connection must be a no-op returning nil, which is exactly
// what the losing side of a two-process race executes. It assumes the caller
// already holds the write lock (BEGIN IMMEDIATE).
func dropOutboxResolutionLocked(ctx context.Context, conn schemaConn) error {
	present, err := tableHasColumn(ctx, conn, "outbox", "resolution")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `DROP INDEX IF EXISTS unresolved_path_intent`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE outbox DROP COLUMN resolution`); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS unresolved_path_intent
            ON outbox(project_id, worktree_id, path)
            WHERE materialised = 0`)
	return err
}

// allocationKindSchemaCurrent reports, using only reads, whether both
// kind-bearing tables already carry the entity-kind column, so an
// already-migrated Open takes no write lock (the M5 ensureFindingsSchema lesson).
// It takes its querier rather than reading s.db so a test can fail one probe
// and not the other, which is the only way to show the error is propagated
// rather than collapsed into "not current".
func allocationKindSchemaCurrent(ctx context.Context, q schemaQuerier) (bool, error) {
	allocations, err := tableHasColumn(ctx, q, "allocations", "kind")
	if err != nil || !allocations {
		return false, err
	}
	return tableHasColumn(ctx, q, "prefix_ownership", "kind")
}

// ensureAllocationKind adds the entity-kind column to the allocations and
// prefix_ownership tables and backfills existing rows to 'ticket'. A single
// transaction covers BOTH tables so the upgrade is crash-atomic: a process that
// dies mid-migration leaves either the fully pre-M9 schema or the fully migrated
// one, never one table ahead of the other. CREATE TABLE IF NOT EXISTS never
// alters an existing table, so a pre-M9 database reaches these columns only
// here (the M5 F1 non-transactional-migration lesson applied to two tables).
func (s *Store) ensureAllocationKind(ctx context.Context) error {
	current, err := allocationKindSchemaCurrent(ctx, s.db)
	if err != nil {
		return translateDBError(err)
	}
	if current {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return translateDBError(err)
	}
	defer tx.Rollback()
	allocations, err := tableHasColumn(ctx, tx, "allocations", "kind")
	if err != nil {
		return translateDBError(err)
	}
	if !allocations {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE allocations ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket'`); err != nil {
			return translateDBError(err)
		}
	}
	if err := s.runAllocationMigrationHook("after-allocations"); err != nil {
		return err
	}
	ownership, err := tableHasColumn(ctx, tx, "prefix_ownership", "kind")
	if err != nil {
		return translateDBError(err)
	}
	if !ownership {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE prefix_ownership ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket'`); err != nil {
			return translateDBError(err)
		}
	}
	if err := s.runAllocationMigrationHook("after-prefix-ownership"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return translateDBError(err)
	}
	return nil
}

func (s *Store) runAllocationMigrationHook(statement string) error {
	if s.allocationMigrationHook == nil {
		return nil
	}
	return s.allocationMigrationHook(statement)
}

// schemaQuerier is the read surface a schema probe needs. *sql.DB, *sql.Tx and
// *sql.Conn all satisfy it, so one probe serves the pool fast path and the
// in-transaction re-check alike.
type schemaQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// schemaConn is the read+write surface a locked migration step needs. It is an
// interface rather than *sql.Conn for one reason: it is the seam that lets a
// test fail the column probe WITHOUT failing the write, which is the only way
// to distinguish a probe that fails closed from one that collapses its error
// into "column absent" and then blindly issues the ALTER.
type schemaConn interface {
	schemaQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// tableHasColumn reports whether table carries the named column. It is a
// fail-CLOSED probe: a query, scan or iteration error is returned, never
// collapsed into "the column is absent".
//
// That distinction is the whole of AIRA-97 Finding 2. A migration that reads an
// unanswered probe as a verdict either skips a migration that never ran (when
// "present" means "return nil") or takes a write branch it was never entitled
// to take. This repo's rule is that a check which cannot establish its result
// must say so rather than report a convenient answer, so the error is surfaced
// and Open fails loudly instead. Same signature and same discipline as
// tableHasAnyForeignKey.
//
// Invariant: this must drain and close its rows before returning. The pool is
// pinned to a single connection (openOnce sets SetMaxOpenConns(1)), so a
// *sql.Rows still open on it would hold the only connection, and the
// fast-path-then-withImmediate handoff in ensureColumnAdded — which asks the
// pool for that connection — would block forever rather than fail (the caller
// there is OpenDB's context.Background()). It is what the explicit rows.Close()
// in the pre-AIRA-97 ensureAreaHintsGeneration was defending.
func tableHasColumn(ctx context.Context, db schemaQuerier, table, wanted string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdentifier(table)+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == wanted {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return found, nil
}

// tableExists reports whether the named table exists, fail-closed for the same
// reason as tableHasColumn: findingsSchemaCurrent negates this answer, so a
// collapsed error there would report an unmigrated schema "already current" on
// the strength of a question that was never answered.
func tableExists(ctx context.Context, db schemaQuerier, table string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, table)
	if err != nil {
		return false, err
	}
	exists := rows.Next()
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	return exists, nil
}

// hasTableColumn is the fail-OPEN form of tableHasColumn: it collapses a probe
// error into "absent". It is retained for exactly one production caller,
// ensureSearchFTS, whose own unguarded check-then-CREATE is AIRA-97 Finding 1b
// and is owned by AIRA-74 — which is rewriting that function, so converting it
// here would collide with that work. Do not add callers: use tableHasColumn.
func hasTableColumn(ctx context.Context, db schemaQuerier, table, wanted string) bool {
	found, err := tableHasColumn(ctx, db, table, wanted)
	return err == nil && found
}

// hasTable is the fail-OPEN form of tableExists, retained for the same single
// caller and the same reason as hasTableColumn. Do not add callers.
func hasTable(ctx context.Context, db schemaQuerier, table string) bool {
	exists, err := tableExists(ctx, db, table)
	return err == nil && exists
}

func (s *Store) ensureSearchFTS(ctx context.Context) error {
	if hasTable(ctx, s.db, "search_fts") && hasTableColumn(ctx, s.db, "search_fts", "project_id") && hasTableColumn(ctx, s.db, "search_fts", "worktree_id") {
		return nil
	}
	if hasTable(ctx, s.db, "search_fts") {
		// The FTS table is disposable. Drop the pre-M6 schema rather than
		// attempting to ALTER a virtual table; the next search/rebuild repopulates it.
		if _, err := s.db.ExecContext(ctx, `DROP TABLE search_fts`); err != nil {
			return translateDBError(err)
		}
	}
	_, err := s.db.ExecContext(ctx, `CREATE VIRTUAL TABLE search_fts USING fts5(
		project_id UNINDEXED, kind UNINDEXED, ref_id UNINDEXED, worktree_id UNINDEXED, content
	)`)
	return translateDBError(err)
}

// findingsSchemaCurrent reports whether the findings schema is already at the
// M5 target (all typed columns, composite PK, no orphan rebuild table) using
// only reads. When true, ensureFindingsSchema is a pure no-op and must NOT open
// a write transaction — every Open would otherwise take the write lock, adding
// needless contention for the common already-migrated case.
// It takes its querier rather than reading s.db so a test can fail exactly one
// of its probes. The findings_m5 probe is the one that matters: its answer is
// NEGATED, so a collapsed error there used to make this function report an
// unmigrated schema "already current" on the strength of a question it never
// answered (AIRA-97 Finding 2).
func findingsSchemaCurrent(ctx context.Context, q schemaQuerier) (bool, error) {
	for _, name := range []string{
		"worktree_id", "subtype", "ticket_id", "category", "severity", "verdict",
		"disposition", "source", "file", "line", "requirement_id", "waiver_reason",
		"waiver_actor", "canonical_file", "message",
	} {
		present, err := tableHasColumn(ctx, q, "findings", name)
		if err != nil {
			return false, err
		}
		if !present {
			return false, nil
		}
	}
	composite, err := findingsHasCompositePrimaryKey(ctx, q)
	if err != nil || !composite {
		return false, err
	}
	orphan, err := tableExists(ctx, q, "findings_m5")
	if err != nil {
		return false, err
	}
	return !orphan, nil
}

func (s *Store) ensureFindingsSchema(ctx context.Context) error {
	current, err := findingsSchemaCurrent(ctx, s.db)
	if err != nil {
		return translateDBError(err)
	}
	if current {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return translateDBError(err)
	}
	defer tx.Rollback()

	columns := []struct{ name, definition string }{
		{"worktree_id", `TEXT NOT NULL DEFAULT ''`},
		{"subtype", `TEXT NOT NULL DEFAULT 'reconciliation'`},
		{"ticket_id", `TEXT NOT NULL DEFAULT ''`}, {"category", `TEXT NOT NULL DEFAULT ''`},
		{"severity", `TEXT NOT NULL DEFAULT ''`}, {"verdict", `TEXT NOT NULL DEFAULT ''`},
		{"disposition", `TEXT NOT NULL DEFAULT 'open'`}, {"source", `TEXT NOT NULL DEFAULT ''`},
		{"file", `TEXT NOT NULL DEFAULT ''`}, {"line", `INTEGER NOT NULL DEFAULT 0`},
		{"requirement_id", `TEXT NOT NULL DEFAULT ''`}, {"waiver_reason", `TEXT NOT NULL DEFAULT ''`},
		{"waiver_actor", `TEXT NOT NULL DEFAULT ''`}, {"canonical_file", `TEXT NOT NULL DEFAULT ''`},
		{"message", `TEXT NOT NULL DEFAULT ''`},
	}
	for _, column := range columns {
		present, err := tableHasColumn(ctx, tx, "findings", column.name)
		if err != nil {
			return translateDBError(err)
		}
		if present {
			continue
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE findings ADD COLUMN `+column.name+` `+column.definition); err != nil {
			return translateDBError(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET subtype=COALESCE(NULLIF(subtype,''),'reconciliation'), message=CASE WHEN message='' THEN details ELSE message END, disposition=CASE WHEN disposition='' THEN 'open' ELSE disposition END`); err != nil {
		return translateDBError(err)
	}
	// Fail closed on the branch selector itself: the else-branch below DROPs and
	// rebuilds the findings table, and selecting a destructive rebuild because a
	// PRAGMA could not be read is the worst form of AIRA-97 Finding 2.
	//
	// ACCEPTED COVERAGE GAP (AIRA-97, recorded rather than left silent). These
	// two probes run on the *sql.Tx that this function itself begins, so there
	// is no seam to fail one probe and not the writes; a mutation that dropped
	// these errors (`composite, _ := …`) would compile and no test would fail.
	// What IS covered: findingsHasCompositePrimaryKey and tableExists are each
	// shown to surface an unanswerable probe (migration_guard_test.go), and
	// findingsSchemaCurrent's identical composition is shown to propagate
	// through its schemaQuerier seam. Closing the gap here needs the deferred-
	// to-immediate restructuring this ticket explicitly deferred.
	composite, err := findingsHasCompositePrimaryKey(ctx, tx)
	if err != nil {
		return translateDBError(err)
	}
	if composite {
		// If a process died after DROP and before RENAME, initDB has already
		// recreated an empty modern findings table. Recover any copied rows
		// before removing the orphan instead of accepting silent data loss.
		orphan, err := tableExists(ctx, tx, "findings_m5")
		if err != nil {
			return translateDBError(err)
		}
		if orphan {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO findings(project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message) SELECT project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message FROM findings_m5`); err != nil {
				return translateDBError(err)
			}
			if _, err := tx.ExecContext(ctx, `DROP TABLE findings_m5`); err != nil {
				return translateDBError(err)
			}
		}
	} else {
		// Clear a table left by an interrupted prior attempt before starting
		// the rebuild. The transaction keeps the legacy table authoritative
		// until the replacement is completely ready.
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS findings_m5`); err != nil {
			return translateDBError(err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE findings_m5 (
			project_id TEXT NOT NULL, worktree_id TEXT NOT NULL DEFAULT '', finding_key TEXT NOT NULL,
			subtype TEXT NOT NULL DEFAULT 'reconciliation', code TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL DEFAULT '', details TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
			ticket_id TEXT NOT NULL DEFAULT '', category TEXT NOT NULL DEFAULT '', severity TEXT NOT NULL DEFAULT '', verdict TEXT NOT NULL DEFAULT '', disposition TEXT NOT NULL DEFAULT 'open', source TEXT NOT NULL DEFAULT '', file TEXT NOT NULL DEFAULT '', line INTEGER NOT NULL DEFAULT 0, requirement_id TEXT NOT NULL DEFAULT '', waiver_reason TEXT NOT NULL DEFAULT '', waiver_actor TEXT NOT NULL DEFAULT '', canonical_file TEXT NOT NULL DEFAULT '', message TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(project_id, worktree_id, finding_key)
		)`); err != nil {
			return translateDBError(err)
		}
		if err := s.runFindingsMigrationHook("after-create"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO findings_m5(project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message) SELECT project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message FROM findings`); err != nil {
			return translateDBError(err)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE findings`); err != nil {
			return translateDBError(err)
		}
		if err := s.runFindingsMigrationHook("after-drop"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE findings_m5 RENAME TO findings`); err != nil {
			return translateDBError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return translateDBError(err)
	}
	return nil
}

func (s *Store) runFindingsMigrationHook(statement string) error {
	if s.findingsMigrationHook == nil {
		return nil
	}
	return s.findingsMigrationHook(statement)
}

// findingsHasCompositePrimaryKey is fail-closed for the same reason as
// tableHasColumn, and more urgently: ensureFindingsSchema uses its answer to
// choose between "adopt the existing table" and "DROP and rebuild it", so a
// collapsed error would pick a destructive branch on an unanswered question.
func findingsHasCompositePrimaryKey(ctx context.Context, db schemaQuerier) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(findings)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	project, worktree, key := 0, 0, 0
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		switch name {
		case "project_id":
			project = pk
		case "worktree_id":
			worktree = pk
		case "finding_key":
			key = pk
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return project > 0 && worktree > 0 && key > 0, nil
}

// Register refreshes this project/worktree registration and validates global
// prefix ownership. NewScope already calls it once while constructing a scope.
func (s *Store) Register(ctx context.Context) error {
	if s.bootstrap {
		return s.RegisterBootstrap(ctx)
	}
	var ejected int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM ejections WHERE project_id=?`, s.projectID).Scan(&ejected)
	if err == nil {
		return fmt.Errorf("E_NOT_ADOPTED: project %s was ejected; run aira init to re-adopt", s.projectID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return translateDBError(err)
	}
	entry := registryEntry{
		ProjectID: s.projectID, CommonDir: s.commonDir, WorktreeID: s.worktreeID,
		Root: s.root, Prefixes: s.prefixesByKind(kindTicket), RequirementPrefixes: s.prefixesByKind(kindRequirement),
		At: time.Now().UTC().Format(time.RFC3339Nano),
	}
	// The breadcrumb is written before the DB transaction. A stale breadcrumb is
	// recoverable evidence; a DB row without a breadcrumb is not recoverable after DB loss.
	if err := appendJSONLine(s.registryPath, entry, s.registryPath+".lock"); err != nil {
		return err
	}
	return s.registerDB(ctx, false)
}

// RegisterBootstrap is the only registration path allowed to clear an eject
// tombstone. It is called by explicit init after any adoption rebuild has
// succeeded; ordinary discovery and verbs always use Register.
func (s *Store) RegisterBootstrap(ctx context.Context) error {
	entry := registryEntry{
		ProjectID: s.projectID, CommonDir: s.commonDir, WorktreeID: s.worktreeID,
		Root: s.root, Prefixes: s.prefixesByKind(kindTicket), RequirementPrefixes: s.prefixesByKind(kindRequirement),
		At: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := appendJSONLine(s.registryPath, entry, s.registryPath+".lock"); err != nil {
		return err
	}
	if err := s.registerDB(ctx, true); err != nil {
		if trimErr := TrimRegistryProject(s.registryPath, s.projectID); trimErr != nil {
			return fmt.Errorf("%w; bootstrap registry rollback failed: %v", err, trimErr)
		}
		return err
	}
	return nil
}

func (s *Store) registerDB(ctx context.Context, bootstrap bool) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if bootstrap {
			if _, err := conn.ExecContext(ctx, `DELETE FROM ejections WHERE project_id=?`, s.projectID); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO projects(project_id, slug, common_dir, config_digest, created_at)
			VALUES(?, ?, ?, ?, ?) ON CONFLICT(project_id) DO UPDATE SET slug=excluded.slug, common_dir=excluded.common_dir, config_digest=excluded.config_digest`,
			s.projectID, s.projectSlug, s.commonDir, s.configDigest, now); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO worktrees(project_id, worktree_id, root, active, updated_at)
            VALUES(?, ?, ?, 1, ?) ON CONFLICT(project_id, worktree_id) DO UPDATE SET root=excluded.root, active=1, updated_at=excluded.updated_at`,
			s.projectID, s.worktreeID, s.root, now); err != nil {
			return err
		}
		for prefix, kind := range s.prefixes {
			var owner, ownerKind string
			err := conn.QueryRowContext(ctx, `SELECT project_id, kind FROM prefix_ownership WHERE prefix=?`, prefix).Scan(&owner, &ownerKind)
			if err == nil && owner != s.projectID {
				var root string
				_ = conn.QueryRowContext(ctx, `SELECT root FROM worktrees WHERE project_id=? AND active=1 ORDER BY updated_at DESC LIMIT 1`, owner).Scan(&root)
				return fmt.Errorf("E_PREFIX_OWNERSHIP_CONFLICT: %s owned by project %s at %s; run aira eject --project %s", prefix, owner, root, owner)
			}
			if err == nil && normaliseKind(ownerKind) != kind {
				// A prefix's kind is immutable: it may not be re-registered under
				// a different entity kind (disjoint-by-kind, enforced durably).
				return fmt.Errorf("E_PREFIX_OWNERSHIP_CONFLICT: %s registered as %s, cannot re-register as %s", prefix, normaliseKind(ownerKind), kind)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO prefix_ownership(prefix, project_id, registered_seq, kind)
                VALUES(?, ?, 0, ?) ON CONFLICT(prefix) DO NOTHING`, prefix, s.projectID, kind); err != nil {
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
		Root: root, Prefixes: s.prefixesByKind(kindTicket), RequirementPrefixes: s.prefixesByKind(kindRequirement),
		At: time.Now().UTC().Format(time.RFC3339Nano),
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
	kind, owned := s.prefixes[prefix]
	if !validPrefix(prefix) || !owned {
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
		path := s.entityPathForKind(kind, id)
		if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq, kind)
            VALUES(?, ?, ?, ?, 'allocated', ?, ?, ?)`, s.projectID, prefix, number, s.worktreeID, path, seq, kind); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
            precondition_digest, intended_digest, intended_bytes, allocation_id)
			VALUES(?, ?, ?, ?, 'id.allocate', '', '', NULL, ?)`, s.projectID, seq, s.worktreeID, path, id); err != nil {
			return err
		}
		if err := insertAllocationEvent(ctx, conn, s.projectID, seq, "id.allocate", id, kind); err != nil {
			return err
		}
		receipt = AllocationReceipt{ProjectID: s.projectID, WorktreeID: s.worktreeID, ID: id, Path: path,
			Seq: seq, At: time.Now().UTC().Format(time.RFC3339Nano), State: "allocated", Kind: kind}
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
	ticket, _, err := s.CreateTicketWithEvent(ctx, input)
	return ticket, err
}

func (s *Store) CreateTicketWithEvent(ctx context.Context, input domain.CreateTicketInput) (domain.Ticket, EventKey, error) {
	intent, err := s.prepareCreate(ctx, input)
	if err != nil {
		return domain.Ticket{}, EventKey{}, err
	}
	if err := s.appendReceiptIfMissing(intent.Receipt); err != nil {
		return domain.Ticket{}, EventKey{}, err
	}
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return domain.Ticket{}, EventKey{}, err
	}
	return intent.Ticket, EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
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
		if ticket.Labels == nil {
			ticket.Labels = []string{}
		}
		ticket.Relations = []domain.Relation{}
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
			Kind: IntentKindTicketFile, Precondition: "", Intended: data, Ticket: ticket, AllocationID: id,
			Receipt: AllocationReceipt{ProjectID: s.projectID, WorktreeID: s.worktreeID, ID: id, Path: path, Seq: seq, State: "allocated"}}
		return nil
	})
	return intent, err
}

func (s *Store) defaultPrefix() (string, error) {
	// Ticket creation must never draw a requirement-kind prefix, so restrict the
	// default to ticket-kind prefixes.
	prefixes := s.prefixesByKind(kindTicket)
	if len(prefixes) == 0 {
		return "", errors.New("E_CONFIG_INVALID: no owned ticket prefix")
	}
	return prefixes[0], nil
}

func (s *Store) preparePathMutation(ctx context.Context, path, precondition string, intended []byte, verb string) (Intent, error) {
	return s.preparePathMutationEvent(ctx, path, precondition, intended, verb, filepath.Base(path))
}

func (s *Store) preparePathMutationEvent(ctx context.Context, path, precondition string, intended []byte, verb, target string) (Intent, error) {
	return s.preparePathMutationEventKind(ctx, path, precondition, intended, verb, target, IntentKindTicketFile)
}

func (s *Store) preparePathMutationEventKind(ctx context.Context, path, precondition string, intended []byte, verb, target string, kind IntentKind) (Intent, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Intent{}, err
	}
	var intent Intent
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		var existing int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM outbox WHERE project_id=? AND worktree_id=? AND path=? AND materialised=0 LIMIT 1`,
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
            precondition_digest, intended_digest, intended_bytes, kind)
            VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, s.projectID, seq, s.worktreeID, path, verb, precondition, digest, intended, kind); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrPathIntentBusy
			}
			return err
		}
		if err := insertEvent(ctx, conn, s.projectID, seq, verb, target); err != nil {
			return err
		}
		intent = Intent{ProjectID: s.projectID, WorktreeID: s.worktreeID, Seq: seq, Path: path,
			Kind: kind, Precondition: precondition, Intended: intended}
		return nil
	})
	return intent, err
}

func (s *Store) UpdateTicket(ctx context.Context, id string, update func(domain.Ticket) (domain.Ticket, error)) error {
	_, err := s.UpdateTicketContent(ctx, id, func(ticket domain.Ticket, body string) (domain.Ticket, string, error) {
		updated, err := update(ticket)
		return updated, body, err
	})
	return err
}

// UpdateTicketContent performs one optimistic frontmatter/body mutation and
// keeps the existing SQLite → file → journal protocol intact.
func (s *Store) UpdateTicketContent(ctx context.Context, id string, update func(domain.Ticket, string) (domain.Ticket, string, error)) (EventKey, error) {
	path := s.ticketPath(id)
	data, outcome, err := readRegularTicket(path)
	if outcome == scanReadInconclusive {
		return EventKey{}, indexUnestablishedError()
	}
	if err != nil {
		return EventKey{}, err
	}
	ticket, body, err := domain.ParseTicket(data)
	if err != nil {
		return EventKey{}, err
	}
	updated, body, err := update(ticket, body)
	if err != nil {
		return EventKey{}, err
	}
	if updated.Status != ticket.Status {
		if err := domain.ValidateTransition(ticket.Status, updated.Status); err != nil {
			return EventKey{}, err
		}
	}
	newData, err := domain.RenderTicket(updated, body)
	if err != nil {
		return EventKey{}, err
	}
	intent, err := s.preparePathMutation(ctx, path, digestBytes(data), newData, "ticket.update")
	if err != nil {
		return EventKey{}, err
	}
	intent.Ticket = updated
	if err := s.materialiseIntent(ctx, intent); err != nil {
		return EventKey{}, err
	}
	return EventKey{ProjectID: intent.ProjectID, Seq: intent.Seq}, nil
}

func (s *Store) materialiseIntent(ctx context.Context, intent Intent) error {
	if len(intent.Intended) == 0 {
		return nil
	}
	searchLock, err := s.acquireSearchLock()
	if err != nil {
		return err
	}
	defer unlockFile(searchLock)
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
	// Dispatch on the exact intent kind; an unknown kind is a programming error
	// and must never silently fall through to ticket materialisation.
	switch intent.Kind {
	case IntentKindTicketFile:
		return s.markTicketMaterialised(ctx, intent)
	case IntentKindFindingFile:
		return s.markFindingMaterialised(ctx, intent)
	case IntentKindRequirementFile:
		return s.markRequirementMaterialised(ctx, intent)
	default:
		return fmt.Errorf("E_INTERNAL: unknown intent kind %q", intent.Kind)
	}
}

func (s *Store) markTicketMaterialised(ctx context.Context, intent Intent) error {
	ticket, _, err := domain.ParseTicket(intent.Intended)
	if err != nil {
		return err
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE outbox SET materialised=1 WHERE project_id=? AND seq=? AND materialised=0`, intent.ProjectID, intent.Seq); err != nil {
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
		if err != nil {
			return err
		}
		return replaceRelationIndex(ctx, conn, intent.ProjectID, intent.WorktreeID, ticket, s.root, intent.Path)
	})
}

func replaceRelationIndex(ctx context.Context, conn *sql.Conn, projectID, worktreeID string, ticket domain.Ticket, root, path string) error {
	canonicalFile := repoPath(root, path)
	if _, err := conn.ExecContext(ctx, `DELETE FROM relations WHERE project_id=? AND worktree_id=? AND canonical_file=?`, projectID, worktreeID, canonicalFile); err != nil {
		return err
	}
	for _, relation := range ticket.Relations {
		if _, err := conn.ExecContext(ctx, `INSERT INTO relations(project_id, worktree_id, kind, from_id, to_id, canonical_file)
			VALUES(?, ?, ?, ?, ?, ?)`, projectID, worktreeID, relation.Kind, relation.From, relation.To, canonicalFile); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) markReceiptOnly(ctx context.Context, projectID string, seq int64) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `UPDATE outbox SET materialised=1, intended_bytes=NULL
            WHERE project_id=? AND seq=? AND materialised=0`, projectID, seq)
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

func (s *Store) journalEventBounded(ctx context.Context, projectID string, seq int64, timeout time.Duration) error {
	var event eventRecord
	row := s.db.QueryRowContext(ctx, `SELECT project_id, seq, at_wall, actor, verb, target, payload_digest FROM events WHERE project_id=? AND seq=?`, projectID, seq)
	if err := row.Scan(&event.ProjectID, &event.Seq, &event.At, &event.Actor, &event.Verb, &event.Target, &event.PayloadDigest); err != nil {
		return err
	}
	if err := appendEventIfMissingBounded(ctx, filepath.Join(s.auditDir, "journal.jsonl"), event, filepath.Join(s.auditDir, "journal.lock"), timeout); err != nil {
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
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("E_RECEIPT_IO: %w", err)
	}
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
	if err := syncFile(f); err != nil {
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
		return upsertReconciliationFinding(ctx, conn, intent.ProjectID, intent.WorktreeID, fmt.Sprintf("reconcile:%s:%d", intent.WorktreeID, intent.Seq), "E_WRITE_CONFLICT", intent.Path, cause.Error())
	})
}

func (s *Store) recordRebuildFinding(ctx context.Context, entry registryEntry, reason string) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		return recordRebuildFindingConn(ctx, conn, s.projectID, entry, reason)
	})
}

func (s *Store) reconcile(ctx context.Context) error {
	findingLock, err := s.acquireFindingMutationLock()
	if err != nil {
		return err
	}
	defer unlockFile(findingLock)
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, seq, worktree_id, path, verb, kind, precondition_digest,
		intended_digest, intended_bytes, materialised, journaled, allocation_id FROM outbox
		WHERE project_id=? AND (worktree_id=? OR path='') AND (materialised=0 OR journaled=0) ORDER BY seq`, s.projectID, s.worktreeID)
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
		var kind IntentKind
		var intendedDigest string
		if err := rows.Scan(&intent.ProjectID, &intent.Seq, &intent.WorktreeID, &intent.Path, &verb, &kind,
			&intent.Precondition, &intendedDigest, &intended, &materialised, &journaled, &intent.AllocationID); err != nil {
			return err
		}
		_ = verb
		_ = intendedDigest
		_ = journaled
		intent.Intended = intended
		intent.Kind = kind
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

// replayUnjournaledEvents closes the crash window shared by all materialised
// mutations: the DB event is durable, but the common journal append may not
// have happened yet. Empty-path lease events are intentionally project-wide so
// any registered worktree can re-drive them.
func (s *Store) replayUnjournaledEvents(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, seq FROM outbox
		WHERE project_id=? AND materialised=1 AND journaled=0 ORDER BY seq`, s.projectID)
	if err != nil {
		return err
	}
	var keys []EventKey
	for rows.Next() {
		var key EventKey
		if err := rows.Scan(&key.ProjectID, &key.Seq); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, key := range keys {
		if err := s.journalEvent(ctx, key.ProjectID, key.Seq); err != nil {
			return err
		}
	}
	return nil
}

// FlushDeferredJournal durably appends the project-wide snapshot of completed
// but unjournaled events. Key-local poison is skipped so later events can make
// progress; journal-global failures abort the pass immediately.
func (s *Store) FlushDeferredJournal(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, seq FROM outbox
		WHERE project_id=? AND materialised=1 AND journaled=0 ORDER BY seq`, s.projectID)
	if err != nil {
		return 0, err
	}
	var keys []EventKey
	for rows.Next() {
		var key EventKey
		if err := rows.Scan(&key.ProjectID, &key.Seq); err != nil {
			_ = rows.Close()
			return 0, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	flushed := 0
	var firstPoison error
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return flushed, err
		}
		err := s.journalEventBounded(ctx, key.ProjectID, key.Seq, journalLockTimeout)
		if err == nil {
			flushed++
			if s.afterJournalFlushEvent != nil {
				s.afterJournalFlushEvent(key)
			}
			continue
		}
		if errors.Is(err, errJournalKeyConflict) || errors.Is(err, sql.ErrNoRows) {
			log.Printf("aira store: deferred journal poison %s/%d: %v", key.ProjectID, key.Seq, err)
			if firstPoison == nil {
				// Lead with the E_JOURNAL_CORRUPT code so ErrorCode() classifies
				// the accumulated poison as journal corruption (both a key
				// conflict and an orphaned outbox row are corruption); %w keeps
				// errors.Is for the specific sentinel / sql.ErrNoRows cause.
				firstPoison = fmt.Errorf("E_JOURNAL_CORRUPT: deferred journal poison %s/%d: %w", key.ProjectID, key.Seq, err)
			}
			continue
		}
		return flushed, err
	}
	return flushed, firstPoison
}

func (s *Store) Rebuild(ctx context.Context) error {
	lock, err := acquireLock(filepath.Join(filepath.Dir(s.dbPath), "rebuild.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := s.replayUnjournaledEvents(ctx); err != nil {
		return err
	}
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
	findingLock, err := s.acquireFindingMutationLock()
	if err != nil {
		return err
	}
	defer unlockFile(findingLock)
	requirementLock, err := s.acquireRequirementMutationLock()
	if err != nil {
		return err
	}
	defer unlockFile(requirementLock)
	searchLock, err := s.acquireSearchLock()
	if err != nil {
		return err
	}
	defer unlockFile(searchLock)
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
	var scannedFindings []scannedFinding
	var scannedRequirements []scannedRequirement
	var deferredScanFindings []struct {
		entry   registryEntry
		finding CheckFinding
	}
	var deferredRebuildFindings []struct {
		entry  registryEntry
		reason string
	}
	for _, entry := range entries {
		valid, reason, gitErr := validGitRoot(entry.Root)
		if gitErr != nil {
			return gitErr
		}
		if err := s.markWorktreeActive(ctx, entry, valid); err != nil {
			return err
		}
		tickets, scanFindings, _, inconclusive, err := scanTickets(entry.Root, entry.WorktreeID, s.projectSlug)
		if err != nil {
			return err
		}
		if inconclusive {
			return indexUnestablishedError()
		}
		for _, finding := range scanFindings {
			deferredScanFindings = append(deferredScanFindings, struct {
				entry   registryEntry
				finding CheckFinding
			}{entry: entry, finding: finding})
			if id, ok := ticketIDFromFilename(finding.Subject); ok {
				prefix, number := splitTicketID(id)
				if int64(number) > maxima[prefix] {
					maxima[prefix] = int64(number)
				}
			}
		}
		findingScan, inconclusive, err := scanFindingFiles(entry.Root, entry.WorktreeID)
		if err != nil {
			return err
		}
		if inconclusive {
			return indexUnestablishedError()
		}
		for _, finding := range findingScan.invalid {
			deferredScanFindings = append(deferredScanFindings, struct {
				entry   registryEntry
				finding CheckFinding
			}{entry: entry, finding: finding})
		}
		scannedFindings = append(scannedFindings, findingScan.valid...)
		requirementScan, inconclusive, err := scanRequirements(entry.Root, entry.WorktreeID)
		if err != nil {
			return err
		}
		if inconclusive {
			return indexUnestablishedError()
		}
		for _, invalid := range requirementScan.invalid {
			deferredScanFindings = append(deferredScanFindings, struct {
				entry   registryEntry
				finding CheckFinding
			}{entry: entry, finding: invalid})
			// A malformed file still claims its ID: advance the high-water so the
			// broken node's ID is never reallocated (mirrors the ticket scan).
			if id, ok := ticketIDFromFilename(invalid.Subject); ok {
				prefix, number := splitTicketID(id)
				if int64(number) > maxima[prefix] {
					maxima[prefix] = int64(number)
				}
			}
		}
		scannedRequirements = append(scannedRequirements, requirementScan.valid...)
		scanned = append(scanned, tickets...)
		for _, ticket := range tickets {
			prefix, number := splitTicketID(ticket.Ticket.ID)
			if int64(number) > maxima[prefix] {
				maxima[prefix] = int64(number)
			}
		}
		for _, req := range requirementScan.valid {
			prefix, number := splitTicketID(req.Requirement.ID)
			if int64(number) > maxima[prefix] {
				maxima[prefix] = int64(number)
			}
		}
		if !valid {
			deferredRebuildFindings = append(deferredRebuildFindings, struct {
				entry  registryEntry
				reason string
			}{entry: entry, reason: reason})
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
	if s.beforeRebuildFindingReconstruct != nil {
		s.beforeRebuildFindingReconstruct()
	}
	var recovered []AllocationReceipt
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		// Relations are a disposable projection of every scanned canonical
		// ticket. Clear the project slice first so rows owned by removed or
		// malformed files cannot survive a rebuild.
		if _, err := conn.ExecContext(ctx, `DELETE FROM relations WHERE project_id=?`, s.projectID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM search_fts WHERE project_id=?`, s.projectID); err != nil {
			return err
		}
		// The requirement index is a disposable projection of the scanned
		// requirement files; clear the project slice so a removed file's row
		// cannot survive a rebuild.
		if _, err := conn.ExecContext(ctx, `DELETE FROM requirements WHERE project_id=?`, s.projectID); err != nil {
			return err
		}
		for _, entry := range entries {
			if _, err := conn.ExecContext(ctx, `DELETE FROM findings WHERE project_id=? AND worktree_id=? AND subtype='review'`, s.projectID, entry.WorktreeID); err != nil {
				return err
			}
			prefix := "scan:" + entry.WorktreeID + ":"
			if _, err := conn.ExecContext(ctx, `DELETE FROM findings WHERE project_id=? AND worktree_id=? AND subtype='reconciliation' AND substr(finding_key,1,?)=?`, s.projectID, entry.WorktreeID, len(prefix), prefix); err != nil {
				return err
			}
		}
		if s.rebuildPhaseBHook != nil {
			if err := s.rebuildPhaseBHook(); err != nil {
				return err
			}
			s.rebuildPhaseBHook = nil
		}
		for _, deferred := range deferredScanFindings {
			if err := recordScanFindingConn(ctx, conn, s.projectID, s.root, deferred.entry, deferred.finding); err != nil {
				return err
			}
		}
		for _, deferred := range deferredRebuildFindings {
			if err := recordRebuildFindingConn(ctx, conn, s.projectID, deferred.entry, deferred.reason); err != nil {
				return err
			}
		}
		for _, finding := range scannedFindings {
			if err := upsertReviewFinding(ctx, conn, s.projectID, finding.WorktreeID, finding.Path, finding.Finding, finding.Digest); err != nil {
				return err
			}
		}
		if err := insertSearchRows(ctx, conn, s.projectID, scanned, scannedFindings); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := insertRantSearchRows(ctx, conn, s.projectID, entry.WorktreeID); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO event_counters(project_id,next_seq) VALUES(?,?)
			ON CONFLICT(project_id) DO UPDATE SET next_seq=CASE WHEN event_counters.next_seq < excluded.next_seq THEN excluded.next_seq ELSE event_counters.next_seq END`,
			s.projectID, maxSeq+1); err != nil {
			return err
		}
		for _, event := range journal {
			if event.ProjectID != s.projectID {
				continue
			}
			if !validJournalEventDigest(event.Verb, event.Target, event.PayloadDigest) {
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
			if err := s.ensureReceiptAllocation(ctx, conn, receipt, journal); err != nil {
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
			if err := replaceRelationIndex(ctx, conn, s.projectID, ticket.WorktreeID, ticket.Ticket, ticket.Root, ticket.Path); err != nil {
				return err
			}
			var allocationSeq int64
			var allocationWorktree, allocationPath, allocationState, allocationKind string
			err := conn.QueryRowContext(ctx, `SELECT seq, worktree_id, path, state, kind FROM allocations WHERE project_id=? AND prefix=? AND number=?`,
				s.projectID, prefix, number).Scan(&allocationSeq, &allocationWorktree, &allocationPath, &allocationState, &allocationKind)
			if errors.Is(err, sql.ErrNoRows) {
				// A scanned file lives under .aira/tickets/, so it is ticket-kind by
				// path. Cross-validate before manufacturing a durable recovery:
				// refuse a requirement-prefixed ID masquerading as a ticket file
				// rather than poisoning the append-only journal with a mis-kinded
				// "recovered" receipt.
				reconciledKind, kindErr := s.reconcileAllocationKind(prefix, kindTicket, ticket.Path)
				if kindErr != nil {
					return kindErr
				}
				allocationSeq, err = nextSequence(ctx, conn, s.projectID)
				if err != nil {
					return err
				}
				allocationWorktree, allocationPath, allocationState = ticket.WorktreeID, ticket.Path, "recovered"
				if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq, kind)
                        VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, s.projectID, prefix, number, allocationWorktree, allocationState, allocationPath, allocationSeq, reconciledKind); err != nil {
					return err
				}
				recovered = append(recovered, AllocationReceipt{ProjectID: s.projectID, WorktreeID: allocationWorktree,
					ID: ticket.Ticket.ID, Path: allocationPath, Seq: allocationSeq, State: "recovered", Kind: reconciledKind})
				if err := ensureRecoveredEvent(ctx, conn, ticket.Ticket.ID, ticket.WorktreeID, ticket.Path, allocationSeq, s.projectID, reconciledKind, journal); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else {
				// An allocation row already exists; a scanned ticket file for the
				// same ID must not disagree with the recorded kind (e.g. a fake
				// ticket file shadowing a real requirement allocation).
				reconciledKind, kindErr := s.reconcileAllocationKind(prefix, normaliseKind(allocationKind), ticket.Path)
				if kindErr != nil {
					return kindErr
				}
				// Validate (or reconstruct) the allocation event against the
				// reconciled kind even when the receipt is absent, so a mis-kinded
				// or downgraded journal event cannot survive a rebuild undetected.
				if err := ensureRecoveredEvent(ctx, conn, ticket.Ticket.ID, ticket.WorktreeID, ticket.Path, allocationSeq, s.projectID, reconciledKind, journal); err != nil {
					return err
				}
				if !receiptKeys[receiptKey(s.projectID, ticket.Ticket.ID, allocationSeq)] {
					recovered = append(recovered, AllocationReceipt{ProjectID: s.projectID, WorktreeID: allocationWorktree,
						ID: ticket.Ticket.ID, Path: allocationPath, Seq: allocationSeq, State: "recovered", Kind: normaliseKind(allocationKind)})
				}
			}
			if allocationSeq > maxSeq {
				maxSeq = allocationSeq
			}
		}
		for _, req := range scannedRequirements {
			prefix, number := splitTicketID(req.Requirement.ID)
			if _, err := conn.ExecContext(ctx, `INSERT INTO requirements(project_id, worktree_id, id, path, digest, status, text)
                    VALUES(?, ?, ?, ?, ?, ?, ?)
                    ON CONFLICT(project_id, worktree_id, id) DO UPDATE SET path=excluded.path, digest=excluded.digest,
                    status=excluded.status, text=excluded.text`,
				s.projectID, req.WorktreeID, req.Requirement.ID, req.Path, req.Digest,
				string(req.Requirement.Status), req.Requirement.Text); err != nil {
				return err
			}
			var allocationSeq int64
			var allocationWorktree, allocationPath, allocationState, allocationKind string
			err := conn.QueryRowContext(ctx, `SELECT seq, worktree_id, path, state, kind FROM allocations WHERE project_id=? AND prefix=? AND number=?`,
				s.projectID, prefix, number).Scan(&allocationSeq, &allocationWorktree, &allocationPath, &allocationState, &allocationKind)
			if errors.Is(err, sql.ErrNoRows) {
				// A scanned file lives under .aira/requirements/, so it is
				// requirement-kind by path. Cross-validate before manufacturing a
				// durable recovery: refuse a ticket-prefixed ID masquerading as a
				// requirement file rather than poisoning the append-only journal.
				reconciledKind, kindErr := s.reconcileAllocationKind(prefix, kindRequirement, req.Path)
				if kindErr != nil {
					return kindErr
				}
				allocationSeq, err = nextSequence(ctx, conn, s.projectID)
				if err != nil {
					return err
				}
				allocationWorktree, allocationPath, allocationState = req.WorktreeID, req.Path, "recovered"
				if _, err := conn.ExecContext(ctx, `INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq, kind)
                        VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, s.projectID, prefix, number, allocationWorktree, allocationState, allocationPath, allocationSeq, reconciledKind); err != nil {
					return err
				}
				recovered = append(recovered, AllocationReceipt{ProjectID: s.projectID, WorktreeID: allocationWorktree,
					ID: req.Requirement.ID, Path: allocationPath, Seq: allocationSeq, State: "recovered", Kind: reconciledKind})
				if err := ensureRecoveredEvent(ctx, conn, req.Requirement.ID, req.WorktreeID, req.Path, allocationSeq, s.projectID, reconciledKind, journal); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else {
				// An allocation row already exists; a scanned requirement file for
				// the same ID must not disagree with the recorded kind.
				reconciledKind, kindErr := s.reconcileAllocationKind(prefix, normaliseKind(allocationKind), req.Path)
				if kindErr != nil {
					return kindErr
				}
				// Validate (or reconstruct) the allocation event against the
				// reconciled kind even when the receipt is absent, so a mis-kinded
				// or downgraded journal event cannot survive a rebuild undetected.
				if err := ensureRecoveredEvent(ctx, conn, req.Requirement.ID, req.WorktreeID, req.Path, allocationSeq, s.projectID, reconciledKind, journal); err != nil {
					return err
				}
				if !receiptKeys[receiptKey(s.projectID, req.Requirement.ID, allocationSeq)] {
					recovered = append(recovered, AllocationReceipt{ProjectID: s.projectID, WorktreeID: allocationWorktree,
						ID: req.Requirement.ID, Path: allocationPath, Seq: allocationSeq, State: "recovered", Kind: normaliseKind(allocationKind)})
				}
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
	if err := s.rebuildGateProjection(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) markWorktreeActive(ctx context.Context, entry registryEntry, active bool) error {
	if entry.WorktreeID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE worktrees SET active=?, updated_at=? WHERE project_id=? AND worktree_id=?`, boolInt(active), time.Now().UTC().Format(time.RFC3339Nano), s.projectID, entry.WorktreeID)
	return err
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

func (s *Store) ensureReceiptAllocation(ctx context.Context, conn *sql.Conn, receipt AllocationReceipt, journal []eventRecord) error {
	prefix, number := splitTicketID(receipt.ID)
	kind, err := s.reconcileAllocationKind(prefix, receipt.Kind, receipt.Path)
	if err != nil {
		return err
	}
	var allocationSeq int64
	var allocationKind string
	err = conn.QueryRowContext(ctx, `SELECT seq, kind FROM allocations WHERE project_id=? AND prefix=? AND number=?`, receipt.ProjectID, prefix, number).Scan(&allocationSeq, &allocationKind)
	if errors.Is(err, sql.ErrNoRows) {
		allocationSeq = receipt.Seq
		_, err = conn.ExecContext(ctx, `INSERT INTO allocations(project_id,prefix,number,worktree_id,state,path,seq,kind) VALUES(?,?,?,?,?,?,?,?)`,
			receipt.ProjectID, prefix, number, receipt.WorktreeID, receipt.State, receipt.Path, allocationSeq, kind)
	} else if err == nil {
		if allocationSeq != receipt.Seq {
			return fmt.Errorf("E_JOURNAL_CORRUPT: receipt %s has seq %d but allocation has seq %d", receipt.ID, receipt.Seq, allocationSeq)
		}
		if normaliseKind(allocationKind) != kind {
			return fmt.Errorf("E_JOURNAL_CORRUPT: receipt %s kind %s but allocation row kind %s", receipt.ID, kind, normaliseKind(allocationKind))
		}
	}
	if err != nil {
		return err
	}
	return ensureAllocationEvent(ctx, conn, receipt.ProjectID, receipt.ID, receipt.WorktreeID, receipt.Path, receipt.Seq, kind, journal)
}

// reconcileAllocationKind cross-validates the entity kind claimed by the durable
// receipt, the entity kind implied by the allocation path, and the authoritative
// kind from the prefix registry, returning the single agreed kind or
// E_JOURNAL_CORRUPT on any disagreement. A pre-M9 receipt (no kind) normalises to
// ticket, which agrees with its ticket path and a ticket-registered prefix.
func (s *Store) reconcileAllocationKind(prefix, receiptKind, path string) (string, error) {
	kind := normaliseKind(receiptKind)
	if !validAllocationKind(kind) {
		return "", fmt.Errorf("E_JOURNAL_CORRUPT: allocation for prefix %s has invalid kind %q", prefix, receiptKind)
	}
	// The path is a durable, self-describing kind witness: a real allocation
	// always materialises under an entity directory. A path under neither
	// directory cannot corroborate the kind, so it is refused rather than
	// silently accepted (which would let a rewritten path pass unchecked).
	pathKind := kindForPath(path)
	if pathKind == "" {
		return "", fmt.Errorf("E_JOURNAL_CORRUPT: allocation for prefix %s has a path outside the entity directories: %q", prefix, path)
	}
	if pathKind != kind {
		return "", fmt.Errorf("E_JOURNAL_CORRUPT: allocation kind %s disagrees with path %s", kind, path)
	}
	if registered, ok := s.prefixes[prefix]; ok && registered != kind {
		return "", fmt.Errorf("E_JOURNAL_CORRUPT: prefix %s is registered as %s but allocation kind is %s", prefix, registered, kind)
	}
	return kind, nil
}

func ensureRecoveredEvent(ctx context.Context, conn *sql.Conn, id, worktreeID, path string, seq int64, project, kind string, journal []eventRecord) error {
	return ensureAllocationEvent(ctx, conn, project, id, worktreeID, path, seq, kind, journal)
}

// ensureAllocationEvent validates or reconstructs the allocation event for a
// recovered allocation. kind is the already-reconciled entity kind (from
// reconcileAllocationKind); the payload digest is validated strictly against it,
// so a journal event whose kind-inclusive digest disagrees with the reconciled
// kind — a coordinated tamper of DB/receipt/path/registry that missed the
// journal — is caught as E_JOURNAL_CORRUPT.
func ensureAllocationEvent(ctx context.Context, conn *sql.Conn, project, id, worktreeID, path string, seq int64, kind string, journal []eventRecord) error {
	fromJournal, journaled := journalEventFor(journal, project, seq)
	verb, target, payload := "id.allocate", id, allocationEventDigest("id.allocate", id, kind)
	if fromJournal.ProjectID != "" {
		if fromJournal.Target != id {
			// Name the conflicting allocation's PATH, not just the two ids
			// (AIRA-93). The message used to read "duplicate project/seq …/1 has
			// target AIRA-1 and LIFE-1" and leave the reader with no way to tell
			// which of the two entries is the intruder or where it came from. The
			// path is what makes it obvious: the entries that corrupted this
			// repository's own journal carry /tmp/TestInitAdopts…/ and
			// /tmp/TestSkillExamples…/ paths, i.e. they were written by a test
			// working directory that resolved to this project's id.
			return fmt.Errorf("E_JOURNAL_CORRUPT: duplicate project/seq %s/%d has target %s and %s (conflicting allocation path %s; inspect <common>/aira/receipts.jsonl)", project, seq, fromJournal.Target, id, path)
		}
		if fromJournal.PayloadDigest != allocationEventDigest(fromJournal.Verb, fromJournal.Target, kind) {
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
		} else if existing.PayloadDigest != allocationEventDigest(existing.Verb, existing.Target, kind) {
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
	if s.beforeCommit != nil {
		s.beforeCommit()
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
	return insertEventActor(ctx, conn, project, seq, "aira", verb, target)
}

func insertEventActor(ctx context.Context, conn *sql.Conn, project string, seq int64, actor, verb, target string) error {
	payload := digestBytes([]byte(verb + "\x00" + target))
	_, err := conn.ExecContext(ctx, `INSERT INTO events(project_id, seq, at_wall, actor, verb, target, payload_digest)
        VALUES(?, ?, ?, ?, ?, ?, ?)`, project, seq, time.Now().UTC().Format(time.RFC3339Nano), actor, verb, target, payload)
	return err
}

// allocationEventDigest binds the entity kind into an allocation-bearing event's
// authenticated payload digest. A ticket allocation keeps the pre-M9 two-part
// digest — kind is the legacy default, so existing ticket journals validate
// unchanged — while any non-ticket kind gets an explicit three-part digest, so
// the journal is an independent cross-check source for kind. A kind tamper that
// changes the DB/receipt/path/registry but leaves the journal is then caught as a
// digest disagreement in ensureAllocationEvent. digestBytes is unkeyed, so this
// detects inconsistent corruption/tamper across sources, not a fully consistent
// rewrite of every source (an unkeyed hash cannot, and that is out of scope).
func allocationEventDigest(verb, id, kind string) string {
	if normaliseKind(kind) == kindTicket {
		return digestBytes([]byte(verb + "\x00" + id))
	}
	return digestBytes([]byte(verb + "\x00" + id + "\x00" + normaliseKind(kind)))
}

// insertAllocationEvent writes an allocation-bearing event with the kind-inclusive
// payload digest. Used by every path that establishes an allocation's kind
// (id.allocate, requirement.create, requirement.import); ticket.create keeps the
// plain insertEvent, which allocationEventDigest reproduces byte-for-byte.
func insertAllocationEvent(ctx context.Context, conn *sql.Conn, project string, seq int64, verb, id, kind string) error {
	payload := allocationEventDigest(verb, id, kind)
	_, err := conn.ExecContext(ctx, `INSERT INTO events(project_id, seq, at_wall, actor, verb, target, payload_digest)
        VALUES(?, ?, ?, 'aira', ?, ?, ?)`, project, seq, time.Now().UTC().Format(time.RFC3339Nano), verb, id, payload)
	return err
}

// validJournalEventDigest is the weak, kind-agnostic well-formedness gate used
// when replaying the journal into the events index. It accepts the legacy
// two-part digest for any event, and the three-part kind-inclusive digest only
// for allocation-bearing verbs (the only non-ticket kind is requirement). The
// strict binding against the reconciled kind is enforced in ensureAllocationEvent.
func validJournalEventDigest(verb, target, digest string) bool {
	if digest == digestBytes([]byte(verb+"\x00"+target)) {
		return true
	}
	switch verb {
	case "id.allocate", "requirement.create", "requirement.import":
		return digest == digestBytes([]byte(verb+"\x00"+target+"\x00"+kindRequirement))
	}
	return false
}

func (s *Store) ticketPath(id string) string {
	return filepath.Join(s.root, ".aira", "tickets", id+".md")
}

func (s *Store) pathLockFor(worktreeID, path string) string {
	triple := s.projectID + "\x00" + worktreeID + "\x00" + path
	return filepath.Join(s.commonDir, "aira", "locks", "path-"+digestBytes([]byte(triple))+".lock")
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

func acquireLockBounded(ctx context.Context, path string, timeout time.Duration) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(int(f.Fd()))
	if timeout <= 0 {
		timeout = journalLockTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		default:
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
			_ = f.Close()
			return nil, fmt.Errorf("timed out acquiring journal lock %s", path)
		case <-ticker.C:
		}
	}
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
	return appendEventIfMissingLocked(path, event)
}

func appendEventIfMissingBounded(ctx context.Context, path string, event eventRecord, lockPath string, timeout time.Duration) error {
	lock, err := acquireLockBounded(ctx, lockPath, timeout)
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	return appendEventIfMissingLocked(path, event)
}

func appendEventIfMissingLocked(path string, event eventRecord) error {
	f, err := openAppendFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := repairJSONLTail(f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dec := json.NewDecoder(f)
	found := false
	for {
		var existing eventRecord
		err := dec.Decode(&existing)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errJournalMalformed, err)
		}
		if existing.ProjectID == event.ProjectID && existing.Seq == event.Seq {
			if existing.PayloadDigest != event.PayloadDigest || existing.Verb != event.Verb || existing.Target != event.Target || existing.Actor != event.Actor || existing.At != event.At {
				return fmt.Errorf("%w: duplicate project/seq %s/%d has different identity", errJournalKeyConflict, event.ProjectID, event.Seq)
			}
			found = true
		}
	}
	if found {
		return syncFile(f)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if err := appendJSONValue(f, event); err != nil {
		return err
	}
	return syncFile(f)
}

func syncFile(f *os.File) error {
	if beforeFileSync != nil {
		if err := beforeFileSync(f); err != nil {
			return err
		}
	}
	return f.Sync()
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if beforeDirSync != nil {
		if err := beforeDirSync(dir); err != nil {
			return err
		}
	}
	return dir.Sync()
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

// ListRegistryEntries returns the intact registry breadcrumbs used by daemon
// discovery. A crash-torn final JSONL record is ignored, matching the tail
// repair performed before registry appends; malformed completed records still
// fail the read.
func ListRegistryEntries(path string) ([]RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []RegistryEntry
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var entry RegistryEntry
		err := dec.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			if isTornRegistryTail(data, dec.InputOffset()) {
				return entries, nil
			}
			return nil, fmt.Errorf("E_CONFIG_INVALID: registry: %w", err)
		}
		entries = append(entries, entry)
	}
}

func isTornRegistryTail(data []byte, decodedOffset int64) bool {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return false
	}
	start := int(decodedOffset)
	for start < len(data) {
		switch data[start] {
		case ' ', '\t', '\r', '\n':
			start++
		default:
			return start > bytes.LastIndexByte(data, '\n')
		}
	}
	return false
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
	cmd.Env = append(gitcontext.ScrubbedEnvironment(), "LC_ALL=C", "LANG=C")
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

func recordScanFindingConn(ctx context.Context, conn *sql.Conn, project, root string, entry registryEntry, finding CheckFinding) error {
	rootPath := repoPath(root, entry.Root)
	subject := filepath.ToSlash(filepath.Join(rootPath, finding.Subject))
	if rootPath == "." {
		subject = finding.Subject
	}
	return upsertReconciliationFinding(ctx, conn, project, entry.WorktreeID, "scan:"+entry.WorktreeID+":"+finding.Code+":"+digestBytes([]byte(subject)), finding.Code, subject, finding.Message)
}

func recordRebuildFindingConn(ctx context.Context, conn *sql.Conn, project string, entry registryEntry, reason string) error {
	return upsertReconciliationFinding(ctx, conn, project, entry.WorktreeID, "rebuild:git-root:"+digestBytes([]byte(entry.Root)), "E_GIT_SCAN", entry.Root, reason)
}

// scanTickets returns the canonical tickets, scan findings, and the exact set
// of ticket paths excluded from the canonical scan. Readiness uses that set as
// its graph-establishment boundary, independent of the finding code catalog.
func scanTickets(root, worktreeID, project string) ([]scannedTicket, []CheckFinding, map[string]struct{}, bool, error) {
	if scanTicketsHook != nil {
		scanTicketsHook()
	}
	dir := filepath.Join(root, ".aira", "tickets")
	entries, err := os.ReadDir(dir)
	directoryMissing := false
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
		directoryMissing = true
	}
	if err != nil && !directoryMissing {
		return nil, nil, nil, false, err
	}
	firstNames := scanEntityNames(entries)
	seen := map[string]string{}
	resultByID := map[string]int{}
	var result []scannedTicket
	var findings []CheckFinding
	excludedTicketPaths := make(map[string]struct{})
	exclude := func(path string) {
		excludedTicketPaths[repoPath(root, path)] = struct{}{}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, outcome, err := readRegularTicket(path)
		if outcome == scanReadInconclusive {
			return nil, nil, nil, true, nil
		}
		if err != nil {
			if ErrorCode(err) == "E_CONFIG_INVALID" {
				findings = append(findings, scanFinding(root, path, err))
				exclude(path)
				continue
			}
			return nil, nil, nil, false, err
		}
		ticket, body, err := domain.ParseTicket(data)
		if err != nil {
			findings = append(findings, scanFinding(root, path, err))
			exclude(path)
			continue
		}
		if ticket.Project != project {
			findings = append(findings, scanFinding(root, path, fmt.Errorf("E_PROJECT_MISMATCH: ticket project %q does not match configured project %q", ticket.Project, project)))
		}
		if prior, ok := seen[ticket.ID]; ok {
			findings = append(findings, scanFinding(root, path, fmt.Errorf("E_DUPLICATE_ID: %s and %s in worktree %s", repoPath(root, prior), repoPath(root, path), worktreeID)))
			exclude(prior)
			exclude(path)
			if index, ok := resultByID[ticket.ID]; ok {
				result = append(result[:index], result[index+1:]...)
				delete(resultByID, ticket.ID)
				for id, currentIndex := range resultByID {
					if currentIndex > index {
						resultByID[id] = currentIndex - 1
					}
				}
			}
			continue
		}
		seen[ticket.ID] = path
		if filepath.Base(path) != ticket.ID+".md" {
			findings = append(findings, scanFinding(root, path, fmt.Errorf("E_CONFIG_INVALID: filename/frontmatter mismatch %s", repoPath(root, path))))
			exclude(path)
			continue
		}
		resultByID[ticket.ID] = len(result)
		result = append(result, scannedTicket{WorktreeID: worktreeID, Root: root, Path: path, Ticket: ticket, Body: body, Digest: digestBytes(data)})
	}
	secondEntries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		if directoryMissing {
			return result, findings, excludedTicketPaths, false, nil
		}
		return nil, nil, nil, true, nil
	}
	if err != nil {
		return nil, nil, nil, false, err
	}
	if !sameScanEntityNames(firstNames, scanEntityNames(secondEntries)) {
		return nil, nil, nil, true, nil
	}
	return result, findings, excludedTicketPaths, false, nil
}

func ticketIDFromFilename(path string) (string, bool) {
	base := filepath.Base(filepath.FromSlash(path))
	if !strings.HasSuffix(base, ".md") {
		return "", false
	}
	id := strings.TrimSuffix(base, ".md")
	return id, domain.ValidateID(id) == nil
}

func scanFinding(root, path string, err error) CheckFinding {
	code := ErrorCode(err)
	if code == "E_INTERNAL" {
		code = "E_CONFIG_INVALID"
	}
	detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), code+":"))
	return CheckFinding{Code: code, Subject: repoPath(root, path), Message: code + ": " + detail, Kind: "fail"}
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
