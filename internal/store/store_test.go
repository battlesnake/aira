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
	if err := s.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(intent.Path); err != nil {
		t.Fatalf("replayed ticket: %v", err)
	}
	receipts, err := os.ReadFile(filepath.Join(base, "common", "aira", "receipts.jsonl"))
	if err == nil && !strings.Contains(string(receipts), intent.Ticket.ID) {
		t.Fatalf("replayed receipt missing %s", intent.Ticket.ID)
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
	writeTicketFile(t, filepath.Join(mainRoot, ".aira", "tickets", "AIRA-30.md"), "AIRA-30")
	gitRun(t, mainRoot, "add", ".")
	gitRun(t, mainRoot, "-c", "user.name=AIRA", "-c", "user.email=aira@example.invalid", "commit", "-m", "seed")
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
	if id != "AIRA-41" {
		t.Fatalf("rebuilt next ID = %s, want AIRA-41", id)
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
