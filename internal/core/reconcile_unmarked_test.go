package core

// verifies: AIRA-301
//
// Build-review round 4, finding 2: 'aira reconcile --rebuild' is one of the two
// remedies E_RECONCILE_REQUIRED names. In an unmarked repository with a
// deferred journal line, the verb's reconcile step is refused at the append
// (no state owns the log yet), so the verb must let Rebuild absorb the log and
// claim it first, rather than returning that refusal on every retry.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/store"
)

func TestReconcileRebuildClaimsAnUnmarkedRepositoryWithADeferredLine(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	s, _ := coreTestStoreAt(t, base)
	for i := 0; i < 2; i++ {
		if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: fmt.Sprintf("ticket %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(base, "common", "aira", "owner.json")
	// The marker changes during the session: the update writes its file and
	// its event, and is refused at the journal append (a deferred line).
	if err := os.WriteFile(marker, []byte(`{"state_dir":"/somewhere/else"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := s.UpdateTicket(ctx, "AIRA-1", func(ticket domain.Ticket) (domain.Ticket, error) {
		ticket.Title = "deferred"
		return ticket, nil
	})
	if store.ErrorCode(err) != "E_STATE_NOT_OWNER" {
		t.Fatalf("fixture: update over a changed marker: %v", err)
	}
	_ = s.Close()
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// A line above this state's counter, so its registration demotes it.
	receipt := fmt.Sprintf(`{"project_id":"project-core","worktree_id":"other-wt","id":"AIRA-9","path":%q,"seq":20,"at":"2026-10-12T00:00:00Z","state":"allocated","kind":"ticket"}`+"\n",
		filepath.Join(base, ".aira", "tickets", "AIRA-9.md"))
	file, err := os.OpenFile(filepath.Join(base, "common", "aira", "receipts.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(receipt); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	s2, _ := coreTestStoreAt(t, base)
	if _, err := s2.CreateTicket(ctx, domain.CreateTicketInput{Title: "before"}); store.ErrorCode(err) != "E_RECONCILE_REQUIRED" {
		t.Fatalf("fixture: create before the remedy: %v, want E_RECONCILE_REQUIRED", err)
	}
	response := New(s2).Do(ctx, Request{Verb: "reconcile", Args: map[string]any{"rebuild": true}})
	if !response.OK {
		t.Fatalf("reconcile --rebuild in an unmarked repository with a deferred line: %#v", response)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reconcile --rebuild did not claim the repository: %v", err)
	}
	var record struct {
		StateDir string `json:"state_dir"`
	}
	if err := json.Unmarshal(data, &record); err != nil || !strings.HasSuffix(record.StateDir, "state") {
		t.Fatalf("marker %q does not name this state (%v)", data, err)
	}
	journal, err := os.ReadFile(filepath.Join(base, "common", "aira", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(journal), `"verb":"ticket.update"`) != 1 {
		t.Fatalf("the deferred update was not appended once: %s", journal)
	}
	if _, err := s2.CreateTicket(ctx, domain.CreateTicketInput{Title: "after"}); err != nil {
		t.Fatalf("create after reconcile --rebuild: %v", err)
	}
}

// pendingCreateShape: two tickets, then the marker changes during the session
// and a third create is refused at its receipt append, after its allocation
// committed: a pending (unmaterialised) outbox row that only reconcile
// completes. Returns the store, the base directory and the marker path.
func pendingCreateShape(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	s, _ := coreTestStoreAt(t, base)
	for i := 0; i < 2; i++ {
		if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: fmt.Sprintf("ticket %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(base, "common", "aira", "owner.json")
	if err := os.WriteFile(marker, []byte(`{"state_dir":"/somewhere/else"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "pending"}); store.ErrorCode(err) != "E_STATE_NOT_OWNER" {
		t.Fatalf("fixture: create over a changed marker: %v", err)
	}
	return s, base, marker
}

func readFileOrNil(t *testing.T, path string) []byte {
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

// Review round 5, P2-3: after the claiming Rebuild, 'reconcile --rebuild' must
// run its reconcile step again. A create refused at its receipt append leaves a
// pending row that only reconcile completes (receipt, file, journal line);
// answering ok without that second pass reports reconciled with the receipt and
// the ticket file still missing.
func TestReconcileRebuildCompletesAPendingCreateAfterItClaims(t *testing.T) {
	ctx := context.Background()
	s, base, marker := pendingCreateShape(t)
	_ = s.Close()
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	receipts := filepath.Join(base, "common", "aira", "receipts.jsonl")
	// A line above this state's counter, so its registration demotes it.
	line := fmt.Sprintf(`{"project_id":"project-core","worktree_id":"other-wt","id":"AIRA-9","path":%q,"seq":20,"at":"2026-10-12T00:00:00Z","state":"allocated","kind":"ticket"}`+"\n",
		filepath.Join(base, ".aira", "tickets", "AIRA-9.md"))
	file, err := os.OpenFile(receipts, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(line); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if strings.Contains(string(readFileOrNil(t, receipts)), `"id":"AIRA-3"`) {
		t.Fatal("fixture: the pending create's receipt is already in the log")
	}

	s2, _ := coreTestStoreAt(t, base)
	response := New(s2).Do(ctx, Request{Verb: "reconcile", Args: map[string]any{"rebuild": true}})
	if !response.OK {
		t.Fatalf("reconcile --rebuild in an unmarked repository with a pending create: %#v", response)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reconcile --rebuild did not claim the repository: %v", err)
	}
	if !strings.Contains(string(readFileOrNil(t, receipts)), `"id":"AIRA-3"`) {
		t.Fatalf("reconcile --rebuild answered ok but the pending create's receipt was not appended:\n%s", readFileOrNil(t, receipts))
	}
	if _, err := os.Stat(filepath.Join(base, ".aira", "tickets", "AIRA-3.md")); err != nil {
		t.Fatalf("reconcile --rebuild answered ok but the pending create's file was not written: %v", err)
	}
}

// Over another state's marker, or an unreadable one, 'reconcile --rebuild'
// does not get past the claim: its Rebuild refuses with E_STATE_NOT_OWNER, the
// verb answers that refusal, and neither the marker nor the shared log changes.
func TestReconcileRebuildOverAForeignOrUnreadableMarkerIsRefused(t *testing.T) {
	for name, content := range map[string]string{
		"other":      `{"state_dir":"/somewhere/else"}` + "\n",
		"unreadable": "{not json\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, base, marker := pendingCreateShape(t)
			if err := os.WriteFile(marker, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			receipts := filepath.Join(base, "common", "aira", "receipts.jsonl")
			journal := filepath.Join(base, "common", "aira", "journal.jsonl")
			receiptsBefore, journalBefore := readFileOrNil(t, receipts), readFileOrNil(t, journal)

			response := New(s).Do(ctx, Request{Verb: "reconcile", Args: map[string]any{"rebuild": true}})
			if response.OK || response.Code != "E_STATE_NOT_OWNER" {
				t.Fatalf("reconcile --rebuild over a %s marker: ok=%v code=%q (%s), want E_STATE_NOT_OWNER", name, response.OK, response.Code, response.Error)
			}
			if got := string(readFileOrNil(t, marker)); got != content {
				t.Fatalf("marker changed to %q", got)
			}
			if string(readFileOrNil(t, receipts)) != string(receiptsBefore) || string(readFileOrNil(t, journal)) != string(journalBefore) {
				t.Fatal("reconcile --rebuild over a foreign or unreadable marker changed the shared log")
			}
		})
	}
}
