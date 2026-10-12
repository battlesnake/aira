// verifies: AIRA-301
//
// The shared audit log (<common>/aira/receipts.jsonl and journal.jsonl) is
// written by one AIRA state only, and only after that state has absorbed it.
// Part A: a counter that was never set from the log refuses to mint
// (E_RECONCILE_REQUIRED). Part B: <common>/aira/owner.json names the one state
// that may register and append (E_STATE_NOT_OWNER for every other state).

package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// ledgerFixture is one repository (a common directory and one checkout) that
// several AIRA states can be pointed at. Identity is path-derived (GitDir set),
// so NewUnregisteredScope and Open agree on the project id.
type ledgerFixture struct {
	base, common, root string
}

func newLedgerFixture(t *testing.T) ledgerFixture {
	t.Helper()
	base := t.TempDir()
	f := ledgerFixture{base: base, common: filepath.Join(base, "common"), root: filepath.Join(base, "main")}
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.common, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f ledgerFixture) state(name string) string { return filepath.Join(f.base, name) }

func (f ledgerFixture) options(state string) Options {
	common, _ := canonicalPath(f.common)
	gitDir, _ := canonicalPath(f.root)
	return Options{
		Root: f.root, CommonDir: f.common, GitDir: f.root,
		ProjectID: hashPath(common), WorktreeID: hashPath(gitDir),
		DBPath: filepath.Join(state, "state.db"), RegistryPath: filepath.Join(state, "registry.jsonl"),
		ProjectSlug: "aira", Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"AR"},
	}
}

func (f ledgerFixture) scopeOptions() ScopeOptions {
	return ScopeOptions{
		Root: f.root, CommonDir: f.common, GitDir: f.root, ProjectSlug: "aira",
		Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"AR"}, Bootstrap: true,
	}
}

// adoptionView is init adoption up to its Rebuild: an unregistered scope over a
// fresh database, with the projects/worktrees parent rows staged as the daemon
// and app init paths stage them (PreflightAdoption + StageAdoption).
func (f ledgerFixture) adoptionView(t *testing.T, state string) *Store {
	t.Helper()
	opts := f.options(state)
	db, err := OpenDB(opts.DBPath, opts.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	view, err := NewUnregisteredScope(db, f.scopeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := view.PreflightAdoption(context.Background()); err != nil {
		t.Fatalf("PreflightAdoption: %v", err)
	}
	if err := view.StageAdoption(context.Background()); err != nil {
		t.Fatalf("StageAdoption: %v", err)
	}
	return view
}

// seedForeignDB gives a state database a projects row (the counter's foreign-key
// parent) and a counter row for project, as a database written before this
// change would hold.
func seedForeignDB(t *testing.T, state, project, common string, next int64) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(state, "state.db"), filepath.Join(state, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO projects(project_id,slug,common_dir,config_digest,created_at) VALUES (?,'aira',?,'','now')`, project, common); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO event_counters(project_id,next_seq) VALUES(?,?)`, project, next); err != nil {
		t.Fatal(err)
	}
	return db
}

func (f ledgerFixture) tryOpen(state string) (*Store, error) {
	return Open(context.Background(), f.options(state))
}

func (f ledgerFixture) open(t *testing.T, state string) *Store {
	t.Helper()
	s, err := f.tryOpen(state)
	if err != nil {
		t.Fatalf("open state %s: %v", state, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (f ledgerFixture) marker() string { return filepath.Join(f.common, "aira", "owner.json") }

func (f ledgerFixture) receipts() string { return filepath.Join(f.common, "aira", "receipts.jsonl") }

func (f ledgerFixture) journal() string { return filepath.Join(f.common, "aira", "journal.jsonl") }

// auditSnapshot is the exact bytes of both shared log files (nil when absent).
type auditSnapshot struct{ receipts, journal []byte }

func readOptional(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f ledgerFixture) audit(t *testing.T) auditSnapshot {
	t.Helper()
	return auditSnapshot{receipts: readOptional(t, f.receipts()), journal: readOptional(t, f.journal())}
}

func (f ledgerFixture) requireAuditUnchanged(t *testing.T, before auditSnapshot, what string) {
	t.Helper()
	after := f.audit(t)
	if !bytes.Equal(before.receipts, after.receipts) {
		t.Fatalf("%s changed receipts.jsonl:\nbefore=%q\nafter=%q", what, before.receipts, after.receipts)
	}
	if !bytes.Equal(before.journal, after.journal) {
		t.Fatalf("%s changed journal.jsonl:\nbefore=%q\nafter=%q", what, before.journal, after.journal)
	}
}

// fileSet lists every regular file and directory under dir, with sizes, so a
// refused operation can be shown to have written nothing there.
func fileSet(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		size := int64(0)
		if !d.IsDir() {
			size = info.Size()
		}
		out = append(out, fmt.Sprintf("%s %v %d", path, d.IsDir(), size))
		return nil
	})
	sort.Strings(out)
	return out
}

// logHighWater is the test's own reading of the log's highest sequence number
// for project, over both files.
func (f ledgerFixture) logHighWater(t *testing.T, project string) int64 {
	t.Helper()
	high := int64(0)
	for _, path := range []string{f.receipts(), f.journal()} {
		data := readOptional(t, path)
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			var line struct {
				ProjectID string `json:"project_id"`
				Seq       int64  `json:"seq"`
			}
			if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
				continue
			}
			if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if line.ProjectID == project && line.Seq > high {
				high = line.Seq
			}
		}
	}
	return high
}

func requireCode(t *testing.T, err error, code, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: err=nil, want %s", what, code)
	}
	if got := ErrorCode(err); got != code {
		t.Fatalf("%s: code=%s (%v), want %s", what, got, err, code)
	}
}

func removeStateDB(t *testing.T, state string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(state, "state.db*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no state.db under %s", state)
	}
	for _, match := range matches {
		if err := os.Remove(match); err != nil {
			t.Fatal(err)
		}
	}
}

func eventCounter(t *testing.T, db *sql.DB, project string) (int64, bool) {
	t.Helper()
	var next int64
	err := db.QueryRow(`SELECT next_seq FROM event_counters WHERE project_id=?`, project).Scan(&next)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return next, true
}

func projectRowCounts(t *testing.T, db *sql.DB, project string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"projects", "worktrees", "event_counters", "allocations", "events", "tickets", "outbox"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE project_id=?`, project).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

func createTickets(t *testing.T, s *Store, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: fmt.Sprintf("ticket %d", i)})
		if err != nil {
			t.Fatalf("create ticket %d: %v", i, err)
		}
		ids = append(ids, ticket.ID)
	}
	return ids
}

func retitle(s *Store, id, title string) error {
	return s.UpdateTicket(context.Background(), id, func(ticket domain.Ticket) (domain.Ticket, error) {
		ticket.Title = title
		return ticket, nil
	})
}

// writeReceiptlessTicket copies an existing ticket file to a new ID: a file on
// disk whose allocation never reached the receipts log (the crash window).
func (f ledgerFixture) writeReceiptlessTicket(t *testing.T, fromID, toID string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, ".aira", "tickets", fromID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	copied := strings.Replace(string(data), `"id":"`+fromID+`"`, `"id":"`+toID+`"`, 1)
	if copied == string(data) {
		t.Fatalf("ticket %s frontmatter has no id field to rewrite: %s", fromID, data)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".aira", "tickets", toID+".md"), []byte(copied), 0o644); err != nil {
		t.Fatal(err)
	}
}

func canonicalStateDir(t *testing.T, state string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(state)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func markerStateDir(t *testing.T, f ledgerFixture) string {
	t.Helper()
	data, err := os.ReadFile(f.marker())
	if err != nil {
		t.Fatalf("owner marker: %v", err)
	}
	var record struct {
		StateDir string `json:"state_dir"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("owner marker %q: %v", data, err)
	}
	return record.StateDir
}

// ---------------------------------------------------------------- Part A

// A database recreated in the owner's own state directory (the lost-database
// shape) must not hand out numbers until it has rebuilt from the log.
func TestARecreatedStateCannotMintBeforeItReadsTheLog(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	ids := createTickets(t, owner, 3)
	if err := retitle(owner, ids[0], "updated"); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.AllocateID(ctx, "AR"); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.AddRant(ctx, domain.RantInput{Body: "a rant", Actor: "opus"}, gitcontext.GitContext{}); err != nil {
		t.Fatal(err)
	}
	project := owner.projectID
	_ = owner.Close()
	removeStateDB(t, state)

	s := f.open(t, state)
	before := f.audit(t)
	beforeFiles := fileSet(t, filepath.Join(f.root, ".aira"))
	high := f.logHighWater(t, project)
	if high < 5 {
		t.Fatalf("fixture log high water = %d, want a populated log", high)
	}

	_, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after loss"})
	requireCode(t, err, "E_RECONCILE_REQUIRED", "CreateTicket")
	if !strings.Contains(err.Error(), "aira reconcile --rebuild") || !strings.Contains(err.Error(), "Nothing was committed") {
		t.Fatalf("refusal does not name the remedy and the no-commit fact: %v", err)
	}
	_, err = s.AllocateID(ctx, "AIRA")
	requireCode(t, err, "E_RECONCILE_REQUIRED", "AllocateID(AIRA)")
	_, err = s.AllocateID(ctx, "AR")
	requireCode(t, err, "E_RECONCILE_REQUIRED", "AllocateID(AR)")
	_, _, err = s.AddRequirement(ctx, domain.RequirementInput{Text: "requirement after loss", Status: domain.RequirementBuilt})
	requireCode(t, err, "E_RECONCILE_REQUIRED", "AddRequirement")
	summary, err := s.ImportTicketsBytes(ctx, []byte(`{"id":"AIRA-50","title":"imported","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil)
	if err == nil {
		joined := fmt.Sprintf("%+v", summary.Errored)
		if !strings.Contains(joined, "E_RECONCILE_REQUIRED") {
			t.Fatalf("ImportTicketsBytes: err=nil summary=%+v, want E_RECONCILE_REQUIRED", summary)
		}
	} else if !strings.Contains(err.Error(), "E_RECONCILE_REQUIRED") {
		t.Fatalf("ImportTicketsBytes: %v, want E_RECONCILE_REQUIRED", err)
	}
	requireCode(t, retitle(s, ids[1], "after loss"), "E_RECONCILE_REQUIRED", "UpdateTicket")
	_, err = s.Claim(ctx, ids[2], false, "opus")
	requireCode(t, err, "E_RECONCILE_REQUIRED", "lease Claim")
	_, err = s.AddRant(ctx, domain.RantInput{Body: "rant after loss", Actor: "opus"}, gitcontext.GitContext{})
	requireCode(t, err, "E_RECONCILE_REQUIRED", "AddRant")

	f.requireAuditUnchanged(t, before, "a recreated state's refused mints")
	if after := fileSet(t, filepath.Join(f.root, ".aira")); strings.Join(after, "\n") != strings.Join(beforeFiles, "\n") {
		t.Fatalf("refused mints wrote under .aira:\nbefore=%v\nafter=%v", beforeFiles, after)
	}

	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after rebuild"})
	if err != nil {
		t.Fatalf("CreateTicket after Rebuild: %v", err)
	}
	if ticket.ID != "AIRA-4" {
		t.Fatalf("CreateTicket after Rebuild = %s, want AIRA-4 (after the log's highest)", ticket.ID)
	}
	var seq int64
	if err := s.db.QueryRow(`SELECT seq FROM allocations WHERE project_id=? AND prefix='AIRA' AND number=4`, s.projectID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq <= high {
		t.Fatalf("allocation seq %d is not above the log's high water %d", seq, high)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
}

// The real crash-recovery shape: a ticket file whose receipt never landed, and a
// recreated database. Rebuild is the advised remedy and must succeed.
func TestRebuildOnARecreatedStateMintsRecoveredAllocations(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 2)
	project := owner.projectID
	_ = owner.Close()
	f.writeReceiptlessTicket(t, "AIRA-2", "AIRA-3")
	removeStateDB(t, state)
	high := f.logHighWater(t, project)

	s := f.open(t, state)
	if next, ok := eventCounter(t, s.db, project); !ok || next != 0 {
		t.Fatalf("recreated counter = %d ok=%v, want 0 (not yet set from the log)", next, ok)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild on a recreated state: %v", err)
	}
	var state3 string
	var seq int64
	if err := s.db.QueryRow(`SELECT state, seq FROM allocations WHERE project_id=? AND prefix='AIRA' AND number=3`, project).Scan(&state3, &seq); err != nil {
		t.Fatalf("recovered AIRA-3: %v", err)
	}
	if state3 != "recovered" || seq <= high {
		t.Fatalf("AIRA-3 state=%s seq=%d, want recovered above high water %d", state3, seq, high)
	}
	if !bytes.Contains(readOptional(t, f.receipts()), []byte(`"id":"AIRA-3"`)) {
		t.Fatalf("recovered receipt for AIRA-3 not appended")
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
}

// A project with no event_counters row was never set from the log.
func TestNoCounterRowIsRefused(t *testing.T) {
	base := t.TempDir()
	db, err := OpenDB(filepath.Join(base, "state", "state.db"), filepath.Join(base, "state", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.Exec(`INSERT INTO projects(project_id,slug,common_dir,config_digest,created_at) VALUES ('no-counter','no-counter','/no-counter','','now')`); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db.db, projectID: "no-counter"}
	err = s.AppendWatchdogEvent(context.Background(), "watchdog.trip", "target")
	requireCode(t, err, "E_RECONCILE_REQUIRED", "mint without a counter row")
	if _, ok := eventCounter(t, db.db, "no-counter"); ok {
		t.Fatalf("a refused mint created a counter row")
	}
}

// A log holding only journal lines (no receipts.jsonl) still has a high water.
func TestAJournalOnlyLogAlsoBlocksMinting(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	if _, err := owner.AddRant(ctx, domain.RantInput{Body: "only a rant", Actor: "opus"}, gitcontext.GitContext{}); err != nil {
		t.Fatal(err)
	}
	_ = owner.Close()
	if _, err := os.Stat(f.receipts()); !os.IsNotExist(err) {
		t.Fatalf("fixture must have no receipts.jsonl: %v", err)
	}
	if len(readOptional(t, f.journal())) == 0 {
		t.Fatalf("fixture must have journal lines")
	}
	removeStateDB(t, state)
	s := f.open(t, state)
	before := f.audit(t)
	_, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after loss"})
	requireCode(t, err, "E_RECONCILE_REQUIRED", "CreateTicket over a journal-only log")
	f.requireAuditUnchanged(t, before, "refused create")
}

// False-fail twin: a brand-new project on an empty log mints at once.
func TestFirstRegistrationOnAnEmptyLogMintsAtOnce(t *testing.T) {
	f := newLedgerFixture(t)
	s := f.open(t, f.state("stateX"))
	ids := createTickets(t, s, 1)
	if ids[0] != "AIRA-1" {
		t.Fatalf("first create = %s, want AIRA-1", ids[0])
	}
}

// Init adoption (NewUnregisteredScope + Rebuild + RegisterBootstrap) leaves the
// counter Rebuild set, so the next create succeeds.
func TestInitAdoptionLeavesTheCounterSet(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 3)
	_ = owner.Close()
	removeStateDB(t, state)

	view := f.adoptionView(t, state)
	if err := view.Rebuild(ctx); err != nil {
		t.Fatalf("adoption Rebuild: %v", err)
	}
	if err := view.RegisterBootstrap(ctx); err != nil {
		t.Fatalf("RegisterBootstrap: %v", err)
	}
	ticket, err := view.CreateTicket(ctx, domain.CreateTicketInput{Title: "after adoption"})
	if err != nil {
		t.Fatalf("create after adoption: %v", err)
	}
	if ticket.ID != "AIRA-4" {
		t.Fatalf("create after adoption = %s, want AIRA-4", ticket.ID)
	}
}

// A hand-written contradicting receipt fails Rebuild, and the refusal names that
// receipt's worktree, path and time and the file it came from.
func TestRebuildContradictionNamesTheReceipt(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	s := f.open(t, f.state("stateX"))
	createTickets(t, s, 1)
	intruderPath := filepath.Join(f.base, "intruder", ".aira", "tickets", "AIRA-1.md")
	line := fmt.Sprintf(`{"project_id":%q,"worktree_id":"intruder-wt","id":"AIRA-1","path":%q,"seq":99,"at":"2026-10-11T03:04:05Z","state":"allocated","kind":"ticket"}`+"\n",
		s.projectID, intruderPath)
	file, err := os.OpenFile(f.receipts(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(line); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	err = s.Rebuild(ctx)
	requireCode(t, err, "E_JOURNAL_CORRUPT", "Rebuild over a contradicting receipt")
	for _, want := range []string{"intruder-wt", intruderPath, "2026-10-11T03:04:05Z", "receipts.jsonl"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

// ---------------------------------------------------------------- Part B

// The incident: a second state on the same common directory and checkout is
// refused at registration and writes nothing anywhere.
func TestAForeignStateIsRefusedAtRegistration(t *testing.T) {
	f := newLedgerFixture(t)
	owner := f.open(t, f.state("stateX"))
	createTickets(t, owner, 1)
	project := owner.projectID
	before := f.audit(t)
	beforeRoot := fileSet(t, f.root)
	beforeCommon := fileSet(t, f.common)

	stateY := f.state("stateY")
	y, err := f.tryOpen(stateY)
	if err == nil {
		_ = y.Close()
	}
	requireCode(t, err, "E_STATE_NOT_OWNER", "foreign Open")
	for _, want := range []string{canonicalStateDir(t, f.state("stateX")), canonicalStateDir(t, stateY), "Nothing was written", "owner.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	f.requireAuditUnchanged(t, before, "a foreign registration")
	if after := fileSet(t, f.root); strings.Join(after, "\n") != strings.Join(beforeRoot, "\n") {
		t.Fatalf("foreign registration wrote into the checkout:\nbefore=%v\nafter=%v", beforeRoot, after)
	}
	if after := fileSet(t, f.common); strings.Join(after, "\n") != strings.Join(beforeCommon, "\n") {
		t.Fatalf("foreign registration wrote into the common dir:\nbefore=%v\nafter=%v", beforeCommon, after)
	}
	if data := readOptional(t, filepath.Join(stateY, "registry.jsonl")); len(bytes.TrimSpace(data)) != 0 {
		t.Fatalf("foreign registration wrote a breadcrumb: %q", data)
	}
	ydb, err := OpenDB(filepath.Join(stateY, "state.db"), filepath.Join(stateY, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer ydb.Close()
	for table, n := range projectRowCounts(t, ydb.db, project) {
		if n != 0 {
			t.Fatalf("foreign state's %s holds %d rows for the project", table, n)
		}
	}
}

// Review P1: a counter row seeded at 1 before this change does not prove the
// state has read a populated log. It must not claim, and it must not mint.
func TestAPreUpgradeCounterDoesNotProveReconciliation(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	owner := f.open(t, f.state("stateX"))
	ids := createTickets(t, owner, 3)
	if err := retitle(owner, ids[0], "updated"); err != nil {
		t.Fatal(err)
	}
	project := owner.projectID
	_ = owner.Close()
	if err := os.Remove(f.marker()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	high := f.logHighWater(t, project)
	if high < 2 {
		t.Fatalf("fixture high water = %d", high)
	}
	stateY := f.state("stateY")
	_ = seedForeignDB(t, stateY, project, f.common, 1).Close()
	before := f.audit(t)

	y := f.open(t, stateY)
	if _, err := os.Lstat(f.marker()); !os.IsNotExist(err) {
		t.Fatalf("a pre-upgrade counter claimed the repository (marker stat err=%v, content=%q)", err, readOptional(t, f.marker()))
	}
	if next, ok := eventCounter(t, y.db, project); !ok || next != 0 {
		t.Fatalf("pre-upgrade counter after registration = %d ok=%v, want demoted to 0", next, ok)
	}
	_, err := y.CreateTicket(ctx, domain.CreateTicketInput{Title: "stale"})
	requireCode(t, err, "E_RECONCILE_REQUIRED", "CreateTicket from a pre-upgrade counter")
	_, err = y.AllocateID(ctx, "AIRA")
	requireCode(t, err, "E_RECONCILE_REQUIRED", "AllocateID from a pre-upgrade counter")
	f.requireAuditUnchanged(t, before, "a demoted state")
}

// False-fail twin: the owner's database has a counter above the log's high
// water, so its first registration after the upgrade claims the repository,
// including when the state directory is reached through a symlink.
func TestTheOwnerClaimsAtItsFirstRegistrationAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 2)
	project := owner.projectID
	counterBefore, _ := eventCounter(t, owner.db, project)
	_ = owner.Close()
	if err := os.Remove(f.marker()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if counterBefore <= f.logHighWater(t, project) {
		t.Fatalf("fixture: owner counter %d is not above the high water", counterBefore)
	}
	link := f.state("stateLink")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}

	s := f.open(t, link)
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker names %q, want the canonical state directory %q", got, want)
	}
	if counterAfter, _ := eventCounter(t, s.db, project); counterAfter != counterBefore {
		t.Fatalf("owner counter %d -> %d: the owner was demoted", counterBefore, counterAfter)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after upgrade"}); err != nil {
		t.Fatalf("owner create after upgrade: %v", err)
	}
	// The same state reached by its real path is the same owner.
	again := f.open(t, state)
	if _, err := again.CreateTicket(ctx, domain.CreateTicketInput{Title: "real path"}); err != nil {
		t.Fatalf("owner create through the real path: %v", err)
	}
}

// A brand-new repository whose <common>/aira does not exist yet is claimed (and
// the directory created) at its first registration.
func TestANewProjectIsClaimedAtItsFirstRegistration(t *testing.T) {
	f := newLedgerFixture(t)
	state := f.state("stateX")
	f.open(t, state)
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker names %q, want %q", got, want)
	}

	// Registration itself must create the directory: scope construction makes
	// <common>/aira/locks today, so remove the whole tree before registering.
	g := newLedgerFixture(t)
	gState := g.state("stateX")
	opts := g.options(gState)
	db, err := OpenDB(opts.DBPath, opts.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	view, err := NewUnregisteredScope(db, g.scopeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(g.common, "aira")); err != nil {
		t.Fatal(err)
	}
	if err := view.Register(context.Background()); err != nil {
		t.Fatalf("Register with no <common>/aira: %v", err)
	}
	if got, want := markerStateDir(t, g), canonicalStateDir(t, gState); got != want {
		t.Fatalf("marker names %q, want %q", got, want)
	}
}

// Review P1: two eligible states racing to register on a fresh repository leave
// exactly one owner, and the loser writes no breadcrumb and no rows.
func TestConcurrentRegistrationsLeaveOneOwnerAndOneRegistration(t *testing.T) {
	for round := 0; round < 25; round++ {
		f := newLedgerFixture(t)
		states := []string{f.state("stateA"), f.state("stateB")}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		stores := make([]*Store, 2)
		for i := range states {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				stores[i], errs[i] = f.tryOpen(states[i])
			}(i)
		}
		close(start)
		wg.Wait()
		winner, loser := -1, -1
		for i, err := range errs {
			if err == nil {
				if winner != -1 {
					t.Fatalf("round %d: both states registered", round)
				}
				winner = i
				continue
			}
			if ErrorCode(err) != "E_STATE_NOT_OWNER" {
				t.Fatalf("round %d: state %d: %v, want E_STATE_NOT_OWNER", round, i, err)
			}
			loser = i
		}
		if winner == -1 || loser == -1 {
			t.Fatalf("round %d: winner=%d loser=%d errs=%v", round, winner, loser, errs)
		}
		project := stores[winner].projectID
		if got, want := markerStateDir(t, f), canonicalStateDir(t, states[winner]); got != want {
			t.Fatalf("round %d: marker names %q, want the winner %q", round, got, want)
		}
		if _, err := stores[winner].CreateTicket(context.Background(), domain.CreateTicketInput{Title: "winner"}); err != nil {
			t.Fatalf("round %d: winner create: %v", round, err)
		}
		if got, want := markerStateDir(t, f), canonicalStateDir(t, states[winner]); got != want {
			t.Fatalf("round %d: marker changed after creation to %q", round, got)
		}
		_ = stores[winner].Close()
		if data := readOptional(t, filepath.Join(states[loser], "registry.jsonl")); len(bytes.TrimSpace(data)) != 0 {
			t.Fatalf("round %d: the loser wrote a breadcrumb: %q", round, data)
		}
		ldb, err := OpenDB(filepath.Join(states[loser], "state.db"), filepath.Join(states[loser], "registry.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for table, n := range projectRowCounts(t, ldb.db, project) {
			if n != 0 {
				t.Fatalf("round %d: the loser's %s holds %d rows", round, table, n)
			}
		}
		_ = ldb.Close()
	}
}

// The owner keeps creating while a foreign state keeps trying to register.
func TestAForeignStateCannotRegisterWhileTheOwnerCreates(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	owner := f.open(t, f.state("stateX"))
	stateY := f.state("stateY")
	var wg sync.WaitGroup
	ownerErrs := make(chan error, 20)
	foreignErrs := make(chan error, 20)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := owner.CreateTicket(ctx, domain.CreateTicketInput{Title: fmt.Sprintf("owner %d", i)}); err != nil {
				ownerErrs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			y, err := f.tryOpen(stateY)
			if err == nil {
				_ = y.Close()
				foreignErrs <- fmt.Errorf("attempt %d registered", i)
				continue
			}
			if ErrorCode(err) != "E_STATE_NOT_OWNER" {
				foreignErrs <- err
			}
		}
	}()
	wg.Wait()
	close(ownerErrs)
	close(foreignErrs)
	for err := range ownerErrs {
		t.Fatalf("owner create failed: %v", err)
	}
	for err := range foreignErrs {
		t.Fatalf("foreign registration not refused: %v", err)
	}
	if err := owner.Rebuild(ctx); err != nil {
		t.Fatalf("owner Rebuild after the race: %v", err)
	}
}

// With the marker changed by hand while a state is registered, the append-time
// checks refuse, leave the shared log untouched, and say only that.
func TestAppendsRequireTheMarkerToNameThisState(t *testing.T) {
	ctx := context.Background()
	tamper := map[string]func(t *testing.T, f ledgerFixture){
		"other": func(t *testing.T, f ledgerFixture) {
			if err := os.WriteFile(f.marker(), []byte(`{"state_dir":"/somewhere/else"}`+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"absent": func(t *testing.T, f ledgerFixture) {
			if err := os.Remove(f.marker()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range tamper {
		t.Run(name+"/receipt", func(t *testing.T) {
			f := newLedgerFixture(t)
			s := f.open(t, f.state("stateX"))
			createTickets(t, s, 1)
			change(t, f)
			before := f.audit(t)
			_, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after tamper"})
			requireCode(t, err, "E_STATE_NOT_OWNER", "CreateTicket at the receipt append")
			if !strings.Contains(err.Error(), "shared log was not changed") || strings.Contains(err.Error(), "Nothing was written") {
				t.Fatalf("append refusal text = %q", err)
			}
			f.requireAuditUnchanged(t, before, "a refused receipt append")
		})
		t.Run(name+"/journal", func(t *testing.T) {
			f := newLedgerFixture(t)
			s := f.open(t, f.state("stateX"))
			ids := createTickets(t, s, 1)
			change(t, f)
			before := f.audit(t)
			err := retitle(s, ids[0], "after tamper")
			requireCode(t, err, "E_STATE_NOT_OWNER", "UpdateTicket at the journal append")
			if !strings.Contains(err.Error(), "shared log was not changed") || strings.Contains(err.Error(), "Nothing was written") {
				t.Fatalf("append refusal text = %q", err)
			}
			f.requireAuditUnchanged(t, before, "a refused journal append")
			// The refused update left its event unjournaled; the daemon's
			// deferred flush (the bounded journal writer) must refuse it too.
			flushed, err := s.FlushDeferredJournal(ctx)
			requireCode(t, err, "E_STATE_NOT_OWNER", "FlushDeferredJournal")
			if flushed != 0 {
				t.Fatalf("FlushDeferredJournal flushed %d events over a foreign marker", flushed)
			}
			f.requireAuditUnchanged(t, before, "a refused deferred flush")
		})
	}
}

// Rebuild by a foreign state on a marked repository is refused before it touches
// its own database or the shared log.
func TestRebuildRefusesAForeignMarkerBeforeTouchingItsDatabase(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	owner := f.open(t, f.state("stateX"))
	createTickets(t, owner, 2)
	project := owner.projectID
	f.writeReceiptlessTicket(t, "AIRA-2", "AIRA-3")
	before := f.audit(t)

	stateY := f.state("stateY")
	ydb := seedForeignDB(t, stateY, project, f.common, 50)
	defer ydb.Close()
	countsBefore := projectRowCounts(t, ydb.db, project)
	view, err := NewUnregisteredScope(ydb, f.scopeOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = view.Rebuild(ctx)
	requireCode(t, err, "E_STATE_NOT_OWNER", "foreign Rebuild")
	countsAfter := projectRowCounts(t, ydb.db, project)
	if fmt.Sprint(countsBefore) != fmt.Sprint(countsAfter) {
		t.Fatalf("foreign Rebuild changed its database: %v -> %v", countsBefore, countsAfter)
	}
	if next, _ := eventCounter(t, ydb.db, project); next != 50 {
		t.Fatalf("foreign Rebuild moved its counter to %d", next)
	}
	f.requireAuditUnchanged(t, before, "a foreign Rebuild")
}

// Init adoption of an unmarked repository: Rebuild absorbs the log, claims, and
// only then appends its recovered receipts.
func TestAdoptionClaimsAnUnmarkedRepositoryAfterRebuild(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 2)
	_ = owner.Close()
	f.writeReceiptlessTicket(t, "AIRA-2", "AIRA-3")
	if err := os.Remove(f.marker()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	removeStateDB(t, state)

	view := f.adoptionView(t, state)
	if err := view.Rebuild(ctx); err != nil {
		t.Fatalf("adoption Rebuild on an unmarked repository: %v", err)
	}
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker names %q, want %q", got, want)
	}
	if !bytes.Contains(readOptional(t, f.receipts()), []byte(`"id":"AIRA-3"`)) {
		t.Fatalf("recovered receipt for AIRA-3 not appended")
	}
	if err := view.RegisterBootstrap(ctx); err != nil {
		t.Fatalf("RegisterBootstrap after adoption: %v", err)
	}
}

// An unreadable marker is never treated as missing: registration, Rebuild and
// appends all refuse, the marker is left as it is, and the refusal names the
// hand step.
func TestAnUnreadableOwnerMarkerRefusesWrites(t *testing.T) {
	ctx := context.Background()
	variants := map[string]func(t *testing.T, path string){
		"invalid-json":    func(t *testing.T, path string) { writeMarker(t, path, "{not json\n") },
		"empty-state-dir": func(t *testing.T, path string) { writeMarker(t, path, `{"state_dir":""}`+"\n") },
		"zero-length":     func(t *testing.T, path string) { writeMarker(t, path, "") },
		"directory": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, corrupt := range variants {
		t.Run(name, func(t *testing.T) {
			f := newLedgerFixture(t)
			state := f.state("stateX")
			s := f.open(t, state)
			createTickets(t, s, 1)
			corrupt(t, f.marker())
			markerBefore := markerSnapshot(t, f.marker())
			before := f.audit(t)
			requireHandStep := func(what string, err error) {
				t.Helper()
				requireCode(t, err, "E_STATE_NOT_OWNER", what)
				if !strings.Contains(err.Error(), "unreadable") || !strings.Contains(err.Error(), "by hand") || !strings.Contains(err.Error(), f.marker()) {
					t.Fatalf("%s: refusal %q does not name the unreadable marker and the hand step", what, err)
				}
			}

			again, err := f.tryOpen(state)
			if err == nil {
				_ = again.Close()
			}
			requireHandStep("registration", err)
			requireHandStep("Rebuild", s.Rebuild(ctx))
			_, err = s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after corruption"})
			requireHandStep("append", err)

			f.requireAuditUnchanged(t, before, "writes over an unreadable marker")
			if after := markerSnapshot(t, f.marker()); after != markerBefore {
				t.Fatalf("the marker changed: before=%q after=%q", markerBefore, after)
			}
		})
	}
}

func markerSnapshot(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		return "missing: " + err.Error()
	}
	if info.IsDir() {
		return "directory"
	}
	return "file: " + string(readOptional(t, path))
}

func writeMarker(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
