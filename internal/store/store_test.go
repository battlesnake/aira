// verifies: AR-5, AR-6, AR-7

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/domain"
)

func persistentTemp(t *testing.T, name string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, "tmp", "aira-go-tests", name+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

func testStore(t *testing.T, root, common, state string) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Root:         root,
		CommonDir:    common,
		DBPath:       filepath.Join(state, "state.db"),
		RegistryPath: filepath.Join(state, "registry.jsonl"),
		ProjectID:    "project-aira",
		WorktreeID:   filepath.Base(root),
		ProjectSlug:  "aira",
		Prefixes:     []string{"AIRA"},
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openTestStore(t *testing.T, root, common, state, worktree string, prefixes ...string) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: common, DBPath: filepath.Join(state, "state.db"),
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "project-aira",
		WorktreeID: worktree, ProjectSlug: "aira", Prefixes: prefixes,
	})
	if err != nil {
		t.Fatalf("open store %s: %v", worktree, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateTicketWritesReceiptAndFile(t *testing.T) {
	base := persistentTemp(t, "create")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, common, filepath.Join(base, "state"))

	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{
		Title:    "First ticket",
		Kind:     domain.KindFeature,
		Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if ticket.ID != "AIRA-1" {
		t.Fatalf("allocated ID = %s", ticket.ID)
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "tickets", "AIRA-1.md")); err != nil {
		t.Fatalf("ticket file: %v", err)
	}
	receipts, err := os.ReadFile(filepath.Join(common, "aira", "receipts.jsonl"))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	if !strings.Contains(string(receipts), "AIRA-1") {
		t.Fatalf("receipt did not contain ID: %s", receipts)
	}
}

func TestUnresolvedPathIntentRejectsSecondWriter(t *testing.T) {
	base := persistentTemp(t, "intent")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	intent, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{
		Title: "Pending", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := s.preparePathMutation(context.Background(), intent.Path, intent.Precondition, []byte("other"), "set"); !errors.Is(err, ErrPathIntentBusy) {
		t.Fatalf("second writer error = %v, want ErrPathIntentBusy", err)
	}
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile pending intent: %v", err)
	}
}

func TestReconcileResumesReceiptBeforeMaterialisation(t *testing.T) {
	base := persistentTemp(t, "replay")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	intent, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{
		Title: "Replay", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.beforeMaterialise = func(_ Intent) error {
		receipts, err := os.ReadFile(filepath.Join(base, "common", "aira", "receipts.jsonl"))
		if err != nil {
			return err
		}
		if !strings.Contains(string(receipts), intent.Ticket.ID) {
			return errors.New("receipt was not durable before materialisation")
		}
		return nil
	}
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(intent.Path); err != nil {
		t.Fatalf("replayed ticket: %v", err)
	}
	receipts, err := os.ReadFile(filepath.Join(base, "common", "aira", "receipts.jsonl"))
	if err != nil {
		t.Fatalf("replayed receipts: %v", err)
	}
	if !strings.Contains(string(receipts), intent.Ticket.ID) {
		t.Fatalf("replayed receipt missing %s", intent.Ticket.ID)
	}
	journal, err := os.ReadFile(filepath.Join(base, "common", "aira", "journal.jsonl"))
	if err != nil {
		t.Fatalf("replayed journal: %v", err)
	}
	if !strings.Contains(string(journal), intent.Ticket.ID) {
		t.Fatalf("replayed journal missing %s", intent.Ticket.ID)
	}
}

func TestReconcileRepairsPartialJournalTail(t *testing.T) {
	base := persistentTemp(t, "journal-tail")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	intent, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{Title: "Journal tail", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(base, "common", "aira", "journal.jsonl")
	if err := os.WriteFile(journal, []byte(`{"partial"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile partial journal: %v", err)
	}
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var event eventRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatalf("journal was not repaired to one JSON record: %v; data=%q", err, data)
	}
	if event.Seq != intent.Seq {
		t.Fatalf("journal seq = %d, want %d", event.Seq, intent.Seq)
	}
	evidence := journal + ".torn-tail-" + digestBytes([]byte(`{"partial"`))[:16]
	if _, err := os.Stat(evidence); err != nil {
		t.Fatalf("torn journal tail was not preserved: %v", err)
	}
}

func TestReconcileRefusesPostCrashUserEdit(t *testing.T) {
	base := persistentTemp(t, "user-edit")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	intent, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{Title: "User edit", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(intent.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intent.Path, []byte("user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(context.Background()); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("post-crash user edit error = %v, want ErrWriteConflict", err)
	}
}

func TestConcurrentAllocationsAreUnique(t *testing.T) {
	// One Store intentionally has MaxOpenConns(1), so this checks uniqueness but
	// not SQLite write-write contention. The short-lived-process test below is
	// the contention and busy-timeout coverage.
	base := persistentTemp(t, "alloc")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	const workers = 32
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.AllocateID(context.Background(), "AIRA")
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("allocation error: %v", err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate allocated ID %s", id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("got %d IDs, want %d", len(seen), workers)
	}
}

func TestConcurrentAllocationsAcrossShortLivedProcesses(t *testing.T) {
	if os.Getenv("AIRA_ALLOC_WORKER") == "1" {
		root := os.Getenv("AIRA_ALLOC_ROOT")
		common := os.Getenv("AIRA_ALLOC_COMMON")
		state := os.Getenv("AIRA_ALLOC_STATE")
		s, err := Open(context.Background(), Options{
			Root: root, CommonDir: common, DBPath: filepath.Join(state, "state.db"),
			RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "project-aira",
			WorktreeID: filepath.Base(root), ProjectSlug: "aira", Prefixes: []string{"AIRA"},
		})
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		id, err := s.AllocateID(context.Background(), "AIRA")
		_ = s.Close()
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(3)
		}
		os.Stdout.WriteString(id)
		os.Exit(0)
	}

	base := persistentTemp(t, "alloc-process")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	const workers = 32
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentAllocationsAcrossShortLivedProcesses$", "-test.v=false")
			cmd.Env = append(os.Environ(), "AIRA_ALLOC_WORKER=1", "AIRA_ALLOC_ROOT="+root,
				"AIRA_ALLOC_COMMON="+filepath.Join(base, "common"), "AIRA_ALLOC_STATE="+filepath.Join(base, "state"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- errors.New(string(out))
				return
			}
			ids <- string(out)
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("process allocation error: %v", err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate process-allocated ID %s", id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("got %d process IDs, want %d", len(seen), workers)
	}
}

func TestRebuildScansSiblingWorkingTreeAndCommittedRef(t *testing.T) {
	base := persistentTemp(t, "rebuild")
	mainRoot := filepath.Join(base, "main")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(filepath.Join(mainRoot, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sibling, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, mainRoot, "init")
	gitRun(t, sibling, "init")
	writeTicketFile(t, filepath.Join(mainRoot, ".aira", "tickets", "AIRA-50.md"), "AIRA-50")
	gitRun(t, mainRoot, "add", ".")
	gitRun(t, mainRoot, "-c", "user.name=AIRA", "-c", "user.email=aira@example.invalid", "commit", "-m", "seed")
	if err := os.Remove(filepath.Join(mainRoot, ".aira", "tickets", "AIRA-50.md")); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(sibling, ".aira", "tickets", "AIRA-40.md"), "AIRA-40")

	s := testStore(t, mainRoot, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := s.RegisterWorktree(context.Background(), "sibling-id", sibling); err != nil {
		t.Fatalf("register sibling: %v", err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	id, err := s.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatalf("allocate after rebuild: %v", err)
	}
	if id != "AIRA-51" {
		t.Fatalf("rebuilt next ID = %s, want AIRA-51", id)
	}
}

func TestRebuildScansReceiptOnlyAndJournalHighWaterMarks(t *testing.T) {
	base := persistentTemp(t, "rebuild-audit")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, common, state, "main", "AIRA")
	_ = s.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(state, "state.db"+suffix))
	}
	if err := os.MkdirAll(filepath.Join(common, "aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := AllocationReceipt{ProjectID: "project-aira", WorktreeID: "burned", ID: "AIRA-99", Path: filepath.Join(root, ".aira", "tickets", "AIRA-99.md"), Seq: 41, State: "allocated"}
	writeJSONL(t, filepath.Join(common, "aira", "receipts.jsonl"), receipt)
	writeJSONL(t, filepath.Join(common, "aira", "journal.jsonl"),
		eventRecord{ProjectID: "project-aira", Seq: 41, Verb: "id.allocate", Target: "AIRA-99", PayloadDigest: digestBytes([]byte("id.allocate\x00AIRA-99"))},
		eventRecord{ProjectID: "project-aira", Seq: 60, Verb: "ticket.update", Target: "AIRA-99", PayloadDigest: digestBytes([]byte("ticket.update\x00AIRA-99"))})
	recovered := openTestStore(t, root, common, state, "main", "AIRA")
	if err := recovered.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild from audit: %v", err)
	}
	id, err := recovered.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatalf("allocate after audit rebuild: %v", err)
	}
	if id != "AIRA-100" {
		t.Fatalf("rebuilt receipt HWM allocated %s, want AIRA-100", id)
	}
	var seq int64
	if err := recovered.db.QueryRow(`SELECT seq FROM allocations WHERE project_id=? AND prefix=? AND number=?`, "project-aira", "AIRA", 100).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 61 {
		t.Fatalf("rebuilt journal HWM allocated seq %d, want 61", seq)
	}
}

func TestRebuildIsIdempotentForExistingUnjournaledAllocationEvent(t *testing.T) {
	base := persistentTemp(t, "rebuild-event-retry")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, common, state, "main", "AIRA")
	receipt := AllocationReceipt{ProjectID: "project-aira", WorktreeID: "main", ID: "AIRA-12", Path: filepath.Join(root, ".aira", "tickets", "AIRA-12.md"), Seq: 42, State: "allocated"}
	writeJSONL(t, filepath.Join(common, "aira", "receipts.jsonl"), receipt)
	digest := digestBytes([]byte("id.allocate\x00AIRA-12"))
	if _, err := s.db.Exec(`INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest,journaled) VALUES(?,?,?,?,?,?,?,0)`,
		"project-aira", receipt.Seq, time.Now().UTC().Format(time.RFC3339Nano), "aira", "id.allocate", receipt.ID, digest); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	var events, allocations int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE project_id=? AND seq=?`, receipt.ProjectID, receipt.Seq).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM allocations WHERE project_id=? AND seq=?`, receipt.ProjectID, receipt.Seq).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if events != 1 || allocations != 1 {
		t.Fatalf("rebuild retry counts = events %d, allocations %d; want 1, 1", events, allocations)
	}
}

func TestRebuildToleratesTornReceiptAndJournalTails(t *testing.T) {
	base := persistentTemp(t, "rebuild-torn-tail")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, common, state, "main", "AIRA")
	_ = s.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(state, "state.db"+suffix))
	}
	audit := filepath.Join(common, "aira")
	receiptPath := filepath.Join(audit, "receipts.jsonl")
	journalPath := filepath.Join(audit, "journal.jsonl")
	receipt := AllocationReceipt{ProjectID: "project-aira", WorktreeID: "burned", ID: "AIRA-99", Path: filepath.Join(root, ".aira", "tickets", "AIRA-99.md"), Seq: 41, State: "allocated"}
	writeJSONL(t, receiptPath, receipt)
	appendFileBytes(t, receiptPath, []byte(`{"project_id":"project-aira","id":"AIRA-100"`))
	event41 := eventRecord{ProjectID: "project-aira", Seq: 41, Verb: "id.allocate", Target: "AIRA-99", PayloadDigest: digestBytes([]byte("id.allocate\x00AIRA-99"))}
	event60 := eventRecord{ProjectID: "project-aira", Seq: 60, Verb: "ticket.update", Target: "AIRA-99", PayloadDigest: digestBytes([]byte("ticket.update\x00AIRA-99"))}
	writeJSONL(t, journalPath, event41, event60)
	appendFileBytes(t, journalPath, []byte(`{"project_id":"project-aira","seq":61,"verb":"ticket.update"`))
	recovered := openTestStore(t, root, common, state, "main", "AIRA")
	if err := recovered.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild with torn tails: %v", err)
	}
	id, err := recovered.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatalf("allocate after torn-tail rebuild: %v", err)
	}
	if id != "AIRA-100" {
		t.Fatalf("valid receipt was not used; allocated %s, want AIRA-100", id)
	}
	var seq int64
	if err := recovered.db.QueryRow(`SELECT seq FROM allocations WHERE project_id=? AND prefix=? AND number=?`, "project-aira", "AIRA", 100).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 61 {
		t.Fatalf("valid journal HWM was not used; allocated seq %d, want 61", seq)
	}
	for _, tail := range []struct {
		path string
		data []byte
	}{
		{receiptPath, []byte(`{"project_id":"project-aira","id":"AIRA-100"`)},
		{journalPath, []byte(`{"project_id":"project-aira","seq":61,"verb":"ticket.update"`)},
	} {
		evidence := tail.path + ".torn-tail-" + digestBytes(tail.data)[:16]
		if _, err := os.Stat(evidence); err != nil {
			t.Fatalf("missing torn-tail evidence %s: %v", evidence, err)
		}
	}
}

func TestRebuildSkipsStaleRegistryWorktree(t *testing.T) {
	base := persistentTemp(t, "rebuild-stale-worktree")
	root := filepath.Join(base, "main")
	stale := filepath.Join(base, "removed")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init")
	writeTicketFile(t, filepath.Join(root, ".aira", "tickets", "AIRA-3.md"), "AIRA-3")
	s := openTestStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"), "main", "AIRA")
	if err := s.RegisterWorktree(context.Background(), "removed", stale); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild with stale registry worktree: %v", err)
	}
	var active int
	if err := s.db.QueryRow(`SELECT active FROM worktrees WHERE project_id=? AND worktree_id=?`, s.projectID, "removed").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("stale worktree active=%d, want inactive", active)
	}
	id, err := s.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatal(err)
	}
	if id != "AIRA-4" {
		t.Fatalf("live worktree was not rebuilt after stale entry; allocated %s, want AIRA-4", id)
	}
	var findings int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND code=?`, s.projectID, "E_GIT_SCAN").Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if findings != 1 {
		t.Fatalf("stale worktree findings = %d, want 1", findings)
	}
}

func TestRebuildRetriesMissingRecoveredReceipt(t *testing.T) {
	base := persistentTemp(t, "rebuild-receipt-retry")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(root, ".aira", "tickets", "AIRA-7.md"), "AIRA-7")
	s := openTestStore(t, root, common, state, "main", "AIRA")
	if _, err := s.db.Exec(`INSERT INTO allocations(project_id,prefix,number,worktree_id,state,path,seq) VALUES(?,?,?,?,?,?,?)`, "project-aira", "AIRA", 7, "main", "recovered", filepath.Join(root, ".aira", "tickets", "AIRA-7.md"), 12); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipts, err := os.ReadFile(filepath.Join(common, "aira", "receipts.jsonl"))
	if err != nil || !strings.Contains(string(receipts), "AIRA-7") {
		t.Fatalf("missing retryable recovered receipt: %v %q", err, receipts)
	}
	id, err := s.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatalf("allocate after non-git rebuild: %v", err)
	}
	if id != "AIRA-8" {
		t.Fatalf("working-tree ticket was not folded by non-git rebuild; allocated %s, want AIRA-8", id)
	}
}

func TestRebuildScansSymlinkedGitWorktree(t *testing.T) {
	base := persistentTemp(t, "rebuild-symlink-worktree")
	realRoot := filepath.Join(base, "real")
	linkedRoot := filepath.Join(base, "linked")
	if err := os.MkdirAll(filepath.Join(realRoot, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, realRoot, "init")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(realRoot, ".aira", "tickets", "AIRA-24.md"), "AIRA-24")
	gitRun(t, realRoot, "add", ".")
	gitRun(t, realRoot, "-c", "user.name=AIRA", "-c", "user.email=aira@example.invalid", "commit", "-m", "seed")
	if err := os.Remove(filepath.Join(realRoot, ".aira", "tickets", "AIRA-24.md")); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(realRoot, ".aira", "tickets", "AIRA-23.md"), "AIRA-23")
	s := openTestStore(t, linkedRoot, filepath.Join(base, "common"), filepath.Join(base, "state"), "main", "AIRA")
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild through symlink: %v", err)
	}
	id, err := s.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatalf("allocate after symlink rebuild: %v", err)
	}
	if id != "AIRA-25" {
		t.Fatalf("symlinked ref was not scanned; allocated %s, want AIRA-25", id)
	}
	var skipped, indexed int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND code=?`, s.projectID, "E_GIT_SCAN").Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM tickets WHERE project_id=? AND id=?`, s.projectID, "AIRA-23").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("symlinked root recorded %d skip findings, want 0", skipped)
	}
	if indexed != 1 {
		t.Fatalf("symlinked worktree indexed AIRA-23 %d times, want 1", indexed)
	}
}

func TestRunGitForcesEnglishLocale(t *testing.T) {
	base := persistentTemp(t, "run-git-locale")
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(base, "git")
	script := "#!/bin/sh\nif [ \"$LC_ALL\" = C ] && [ \"$LANG\" = C ]; then\n  echo 'fatal: not a git repository' >&2\nelse\n  echo 'fatal: kein Git-Repository' >&2\nfi\nexit 128\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", base+string(os.PathListSeparator)+filepath.Dir(gitPath))
	_, stderr, err := runGit(root, "rev-parse", "--show-toplevel")
	if err == nil || !isNotGitRepository(stderr) {
		t.Fatalf("runGit stderr = %q, err = %v; want English not-a-repository classification", stderr, err)
	}
}

func TestDiscoverWorktreesDeduplicatesResolvedIdentity(t *testing.T) {
	base := persistentTemp(t, "discover-worktree-dedup")
	realRoot := filepath.Join(base, "real")
	linkedRoot := filepath.Join(base, "linked")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, realRoot, "init")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	entries, err := discoverWorktrees(linkedRoot, "project-aira", []registryEntry{{
		ProjectID: "project-aira", WorktreeID: "registered", Root: linkedRoot,
	}})
	if err != nil {
		t.Fatalf("discover worktrees: %v", err)
	}
	if len(entries) != 1 || entries[0].WorktreeID != "registered" {
		t.Fatalf("discovered worktrees = %#v, want only registered symlink entry", entries)
	}
}

func TestValidGitRootAcceptsCaseInsensitiveAlias(t *testing.T) {
	base := persistentTemp(t, "valid-git-root-case")
	root := filepath.Join(base, "MixedCase")
	alias := filepath.Join(base, "mixedcase")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(aliasInfo, mustStat(t, root)) {
		t.Skip("filesystem is case-sensitive; WSL/DrvFs alias unavailable")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(base, "git")
	script := "#!/bin/sh\nif [ \"$1\" = -C ] && [ \"$3\" = rev-parse ]; then\n  echo '" + alias + "'\n  exit 0\nfi\nexec " + gitPath + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", base+string(os.PathListSeparator)+filepath.Dir(gitPath))
	valid, _, err := validGitRoot(root)
	if err != nil || !valid {
		t.Fatalf("validGitRoot case-insensitive alias = valid %v, err %v; want valid", valid, err)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestRebuildAbortsOnGitRootFailure(t *testing.T) {
	base := persistentTemp(t, "rebuild-git-root-failure")
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(base, "git")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"rev-parse\" ]; then\n    echo 'fatal: planted git failure' >&2\n    exit 128\n  fi\ndone\nexec "+gitPath+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", base+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := openTestStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"), "main", "AIRA")
	err = s.Rebuild(context.Background())
	if err == nil || !strings.Contains(err.Error(), "E_GIT_SCAN") {
		t.Fatalf("genuine git-root failure = %v, want E_GIT_SCAN", err)
	}
}

func TestRebuildAcceptsExistingTicketCreateEvent(t *testing.T) {
	base := persistentTemp(t, "rebuild-ticket-create-event")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, common, state, "main", "AIRA")
	receipt := AllocationReceipt{ProjectID: "project-aira", WorktreeID: "main", ID: "AIRA-31", Path: filepath.Join(root, ".aira", "tickets", "AIRA-31.md"), Seq: 42, State: "allocated"}
	writeJSONL(t, filepath.Join(common, "aira", "receipts.jsonl"), receipt)
	digest := digestBytes([]byte("ticket.create\x00" + receipt.ID))
	if _, err := s.db.Exec(`INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest,journaled) VALUES(?,?,?,?,?,?,?,0)`,
		receipt.ProjectID, receipt.Seq, time.Now().UTC().Format(time.RFC3339Nano), "aira", "ticket.create", receipt.ID, digest); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild accepted ticket.create event: %v", err)
	}
}

func TestRebuildRejectsDivergentExistingAllocationEvent(t *testing.T) {
	base := persistentTemp(t, "rebuild-divergent-event")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, common, state, "main", "AIRA")
	receipt := AllocationReceipt{ProjectID: "project-aira", WorktreeID: "main", ID: "AIRA-32", Path: filepath.Join(root, ".aira", "tickets", "AIRA-32.md"), Seq: 43, State: "allocated"}
	writeJSONL(t, filepath.Join(common, "aira", "receipts.jsonl"), receipt)
	digest := digestBytes([]byte("ticket.create\x00AIRA-999"))
	if _, err := s.db.Exec(`INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest,journaled) VALUES(?,?,?,?,?,?,?,0)`,
		receipt.ProjectID, receipt.Seq, time.Now().UTC().Format(time.RFC3339Nano), "aira", "ticket.create", "AIRA-999", digest); err != nil {
		t.Fatal(err)
	}
	err := s.Rebuild(context.Background())
	if err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("divergent existing event = %v, want E_JOURNAL_CORRUPT", err)
	}
}

func TestRebuildRejectsCorruptJournalPayloadDigest(t *testing.T) {
	base := persistentTemp(t, "rebuild-corrupt-journal-digest")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, common, state, "main", "AIRA")
	receipt := AllocationReceipt{ProjectID: "project-aira", WorktreeID: "main", ID: "AIRA-33", Path: filepath.Join(root, ".aira", "tickets", "AIRA-33.md"), Seq: 44, State: "allocated"}
	writeJSONL(t, filepath.Join(common, "aira", "receipts.jsonl"), receipt)
	writeJSONL(t, filepath.Join(common, "aira", "journal.jsonl"), eventRecord{
		ProjectID: receipt.ProjectID, Seq: receipt.Seq, Verb: "ticket.create", Target: receipt.ID, PayloadDigest: "corrupt-digest",
	})
	err := s.Rebuild(context.Background())
	if err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("corrupt journal digest = %v, want E_JOURNAL_CORRUPT", err)
	}
}

func TestRebuildRejectsCorruptNonAllocationJournalPayloadDigest(t *testing.T) {
	base := persistentTemp(t, "rebuild-corrupt-nonallocation-journal-digest")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, common, state, "main", "AIRA")
	writeJSONL(t, filepath.Join(common, "aira", "journal.jsonl"), eventRecord{
		ProjectID: "project-aira", Seq: 45, Verb: "ticket.update", Target: "AIRA-34", PayloadDigest: "corrupt-digest",
	})
	err := s.Rebuild(context.Background())
	if err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("corrupt non-allocation journal digest = %v, want E_JOURNAL_CORRUPT", err)
	}
}

func TestRebuildIncludesUnregisteredGitWorktree(t *testing.T) {
	base := persistentTemp(t, "rebuild-git-worktree")
	mainRoot := filepath.Join(base, "main")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(mainRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, mainRoot, "init")
	if err := os.MkdirAll(filepath.Join(mainRoot, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(mainRoot, ".aira", "tickets", "AIRA-1.md"), "AIRA-1")
	gitRun(t, mainRoot, "add", ".")
	gitRun(t, mainRoot, "-c", "user.name=AIRA", "-c", "user.email=aira@example.invalid", "commit", "-m", "seed")
	gitRun(t, mainRoot, "worktree", "add", "--detach", sibling, "HEAD")
	writeTicketFile(t, filepath.Join(sibling, ".aira", "tickets", "AIRA-55.md"), "AIRA-55")
	s := openTestStore(t, mainRoot, filepath.Join(base, "common"), filepath.Join(base, "state"), "main", "AIRA")
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	id, err := s.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatal(err)
	}
	if id != "AIRA-56" {
		t.Fatalf("unregistered worktree rebuilt next ID %s, want AIRA-56", id)
	}
}

func TestRebuildScansCommittedDeletedRefAboveWorktree(t *testing.T) {
	base := persistentTemp(t, "rebuild-git-ref-hwm")
	mainRoot := filepath.Join(base, "main")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(filepath.Join(mainRoot, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sibling, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, mainRoot, "init")
	gitRun(t, sibling, "init")
	writeTicketFile(t, filepath.Join(mainRoot, ".aira", "tickets", "AIRA-60.md"), "AIRA-60")
	gitRun(t, mainRoot, "add", ".")
	gitRun(t, mainRoot, "-c", "user.name=AIRA", "-c", "user.email=aira@example.invalid", "commit", "-m", "seed")
	if err := os.Remove(filepath.Join(mainRoot, ".aira", "tickets", "AIRA-60.md")); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(sibling, ".aira", "tickets", "AIRA-55.md"), "AIRA-55")
	s := openTestStore(t, mainRoot, filepath.Join(base, "common"), filepath.Join(base, "state"), "main", "AIRA")
	if err := s.RegisterWorktree(context.Background(), "sibling-id", sibling); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	id, err := s.AllocateID(context.Background(), "AIRA")
	if err != nil {
		t.Fatal(err)
	}
	if id != "AIRA-61" {
		t.Fatalf("committed deleted ref rebuilt next ID %s, want AIRA-61", id)
	}
}

func TestReconcileOnlyHandlesCurrentWorktree(t *testing.T) {
	base := persistentTemp(t, "reconcile-worktree")
	mainRoot := filepath.Join(base, "main")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(mainRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	common, state := filepath.Join(base, "common"), filepath.Join(base, "state")
	a := openTestStore(t, mainRoot, common, state, "main", "AIRA")
	b := openTestStore(t, sibling, common, state, "sibling", "AIRA")
	intent, err := b.prepareCreate(context.Background(), domain.CreateTicketInput{Title: "Sibling", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("main reconcile: %v", err)
	}
	if _, err := os.Stat(intent.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("main reconcile materialised sibling path: %v", err)
	}
	var materialised int
	if err := b.db.QueryRow(`SELECT materialised FROM outbox WHERE project_id=? AND seq=?`, intent.ProjectID, intent.Seq).Scan(&materialised); err != nil {
		t.Fatal(err)
	}
	if materialised != 0 {
		t.Fatalf("sibling intent materialised by main reconcile: %d", materialised)
	}
}

func TestReconcileRedrivesJournalAfterMaterialisation(t *testing.T) {
	base := persistentTemp(t, "journal-redrive")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	intent, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{Title: "Redrive", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendReceiptForIntent(intent); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(intent.Path, intent.Intended, intent.Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.markMaterialised(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(base, "common", "aira", "journal.jsonl"))
	if err != nil || !strings.Contains(string(journal), intent.Ticket.ID) {
		t.Fatalf("journal redrive missing %s: %v %q", intent.Ticket.ID, err, journal)
	}
}

func TestMaterialiseIntentTreatsAlreadyAppliedAsSuccess(t *testing.T) {
	base := persistentTemp(t, "already-applied")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	intent, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{Title: "Already applied", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendReceiptForIntent(intent); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(intent.Path, intent.Intended, intent.Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.materialiseIntent(context.Background(), intent); err != nil {
		t.Fatalf("already-applied intent: %v", err)
	}
	var materialised int
	if err := s.db.QueryRow(`SELECT materialised FROM outbox WHERE project_id=? AND seq=?`, intent.ProjectID, intent.Seq).Scan(&materialised); err != nil {
		t.Fatal(err)
	}
	if materialised != 1 {
		t.Fatalf("materialised = %d, want 1", materialised)
	}
}

func TestWriteAtomicReplaysStaleTemp(t *testing.T) {
	base := persistentTemp(t, "stale-temp")
	path := filepath.Join(base, "nested", "ticket.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(path), ".ticket.md.aira-tmp-9")
	if err := os.WriteFile(tmp, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("new"), 9); err != nil {
		t.Fatalf("replay with orphan temp: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("materialised data = %q, err=%v", data, err)
	}
}

func TestWriteAtomicDoesNotFollowTempSymlink(t *testing.T) {
	base := persistentTemp(t, "symlink-temp")
	path := filepath.Join(base, "nested", "ticket.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(base, "sentinel")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(path), ".ticket.md.aira-tmp-9")
	if err := os.Symlink(sentinel, tmp); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("ticket"), 9); err != nil {
		t.Fatalf("materialise through symlink temp: %v", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("sentinel = %q, err=%v; symlink was followed", data, err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "ticket" {
		t.Fatalf("ticket = %q, err=%v", data, err)
	}
}

func TestJournalDedupRejectsDifferentPayload(t *testing.T) {
	base := persistentTemp(t, "journal-digest")
	path := filepath.Join(base, "journal.jsonl")
	old := eventRecord{ProjectID: "project-aira", Seq: 1, Verb: "old", Target: "one", PayloadDigest: "old-digest"}
	writeJSONL(t, path, old)
	err := appendEventIfMissing(path, eventRecord{ProjectID: "project-aira", Seq: 1, Verb: "new", Target: "two", PayloadDigest: "new-digest"}, path+".lock")
	if err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("same-seq different-payload error = %v", err)
	}
}

func TestCreateAndUpdateUseOwnedPrefixAndTransitionValidation(t *testing.T) {
	base := persistentTemp(t, "prefix-transition")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"), "main", "TASK")
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "Owned", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.ID != "TASK-1" {
		t.Fatalf("created ID = %s, want TASK-1", ticket.ID)
	}
	err = s.UpdateTicket(context.Background(), ticket.ID, func(ticket domain.Ticket) (domain.Ticket, error) {
		ticket.Status = domain.StatusDone
		return ticket, nil
	})
	if err == nil || !strings.Contains(err.Error(), "E_TRANSITION_INVALID") {
		t.Fatalf("invalid transition error = %v", err)
	}
}

func TestOpenRequiresCommonDir(t *testing.T) {
	_, err := Open(context.Background(), Options{Root: ".", DBPath: "state.db", RegistryPath: "registry.jsonl", ProjectID: "p", WorktreeID: "w", ProjectSlug: "aira", Prefixes: []string{"AIRA"}})
	if err == nil || !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("empty common dir error = %v", err)
	}
}

func TestSQLiteDurabilityPragmasApplyToNewConnections(t *testing.T) {
	base := persistentTemp(t, "sqlite-pragmas")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	s.db.SetMaxOpenConns(2)
	first, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	checks := map[string]string{"busy_timeout": "5000", "synchronous": "2", "foreign_keys": "1", "journal_mode": "wal"}
	for pragma, want := range checks {
		var got string
		if err := second.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("pragma %s: %v", pragma, err)
		}
		if strings.ToLower(got) != want {
			t.Fatalf("pragma %s = %q, want %q", pragma, got, want)
		}
	}
}

func TestReconcileContinuesAfterConflictAndRecordsFinding(t *testing.T) {
	base := persistentTemp(t, "reconcile-findings")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	first, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{Title: "Conflict", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.prepareCreate(context.Background(), domain.CreateTicketInput{Title: "Repair", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(first.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Path, []byte("user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(context.Background()); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("reconcile conflict error = %v", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("later intent was not repaired: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=?`, s.projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("finding count = %d, want 1", count)
	}
	if err := os.WriteFile(first.Path, first.Intended, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check after conflict resolution: %v", err)
	}
	for _, finding := range report.Findings {
		if finding.Code == "E_WRITE_CONFLICT" {
			t.Fatalf("resolved conflict remained current: %#v", report)
		}
	}
}

func TestScanRefMaxIgnoresBrokenRefWarning(t *testing.T) {
	base := persistentTemp(t, "broken-ref")
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init")
	if err := os.MkdirAll(filepath.Join(root, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "refs", "heads", "broken"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanRefMax(root); err != nil {
		t.Fatalf("exit-0 broken-ref warning should be ignored: %v", err)
	}
}

func TestCheckReportsMalformedAndSymlinkedTicketsWithoutWedge(t *testing.T) {
	base := persistentTemp(t, "local-ticket-findings")
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "valid", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	ticketsDir := filepath.Join(root, ".aira", "tickets")
	if err := os.WriteFile(filepath.Join(ticketsDir, "notes.md"), []byte("not frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.md")
	if err := os.WriteFile(outside, []byte("not frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ticketsDir, "AIRA-99.md")); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List("")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list with local findings: rows=%#v err=%v", rows, err)
	}
	report, err := s.Check(context.Background())
	if err != nil || report.Verdict != "fail" || len(report.Findings) < 2 {
		t.Fatalf("check with local findings: report=%#v err=%v", report, err)
	}
	if report.Dimensions["ticket-file-integrity"] != "fail" {
		t.Fatalf("ticket-file dimension = %#v", report.Dimensions)
	}
	seen := map[string]bool{}
	for _, finding := range report.Findings {
		seen[finding.Code] = true
	}
	if !seen["E_CONFIG_INVALID"] {
		t.Fatalf("local config finding missing: %#v", report.Findings)
	}
}

func writeJSONL(t *testing.T, path string, values ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendFileBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeTicketFile(t *testing.T, path, id string) {
	t.Helper()
	content := "---\n{\"schema\":1,\"id\":\"" + id + "\",\"project\":\"aira\",\"title\":\"seed\",\"status\":\"planned\",\"kind\":\"feature\",\"severity\":\"P2\",\"assignee\":null,\"milestone\":null,\"labels\":[],\"hold\":false,\"relations\":[]}\n---\nseed\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
