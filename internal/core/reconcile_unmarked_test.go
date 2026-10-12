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
