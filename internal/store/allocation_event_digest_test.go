package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"encoding/json"
)

// TestAllocationEventsBindKindInDigest locks the write-side digest formula: a
// ticket allocation keeps the legacy two-part digest (zero churn on landed
// flows), while a requirement allocation (both AllocateID over a requirement
// prefix and AddRequirement) gets the three-part kind-inclusive digest.
func TestAllocationEventsBindKindInDigest(t *testing.T) {
	base := persistentTemp(t, "alloc-event-digest")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	// A ticket-prefix id.allocate keeps the D0 (two-part) digest.
	if _, err := s.AllocateID(context.Background(), "AIRA"); err != nil {
		t.Fatal(err)
	}
	assertEventDigest(t, s, "id.allocate", "AIRA-1", digestBytes([]byte("id.allocate\x00AIRA-1")))

	// A requirement-prefix id.allocate binds kind: D1 (three-part) digest.
	if _, err := s.AllocateID(context.Background(), "AR"); err != nil {
		t.Fatal(err)
	}
	wantD1 := digestBytes([]byte("id.allocate\x00AR-1\x00requirement"))
	assertEventDigest(t, s, "id.allocate", "AR-1", wantD1)
	if wantD1 == digestBytes([]byte("id.allocate\x00AR-1")) {
		t.Fatal("D1 digest collides with D0 — kind is not actually bound")
	}

	// AddRequirement's requirement.create event is likewise kind-bound.
	req, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "R.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	assertEventDigest(t, s, "requirement.create", req.ID, digestBytes([]byte("requirement.create\x00"+req.ID+"\x00requirement")))
}

// TestRebuildRejectsKindDowngradedAllocationEvent is the Sol P1(B) defense: an
// allocation event whose digest is well-formed but encodes the WRONG kind (the
// ticket/D0 form for a requirement allocation) passes the weak generic gate yet
// is caught by the strict, reconciled-kind binding in ensureAllocationEvent. It
// models a coordinated tamper of DB/receipt/path/registry that missed the
// independent journal — the journal still pins the original kind.
func TestRebuildRejectsKindDowngradedAllocationEvent(t *testing.T) {
	base := persistentTemp(t, "alloc-event-downgrade")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	if _, err := s.AllocateID(context.Background(), "AR"); err != nil {
		t.Fatal(err)
	}

	// Downgrade the journal event to the ticket (D0) digest: a valid digest for
	// the wrong kind. The generic gate accepts D0 for any verb, so only the
	// kind-binding check can reject it.
	downgraded := digestBytes([]byte("id.allocate\x00AR-1"))
	tamperJournalEventDigest(t, filepath.Join(common, "aira", "journal.jsonl"), "id.allocate", "AR-1", downgraded)

	// Drop the index so rebuild reconstructs from the (tampered) journal + the
	// untampered requirement receipt, forcing the reconciled-kind comparison.
	if _, err := s.db.Exec(`DELETE FROM allocations`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("rebuild should reject a kind-downgraded allocation event, got %v", err)
	}
}

// TestValidJournalEventDigestGate locks the weak well-formedness gate: legacy
// two-part digests are accepted for any verb; the three-part requirement digest
// is accepted only for allocation-bearing verbs; everything else is refused.
func TestValidJournalEventDigestGate(t *testing.T) {
	d0 := func(verb, target string) string { return digestBytes([]byte(verb + "\x00" + target)) }
	d1req := func(verb, target string) string {
		return digestBytes([]byte(verb + "\x00" + target + "\x00requirement"))
	}

	// Legacy D0 is accepted for an ordinary (non-allocation) verb.
	if !validJournalEventDigest("ticket.update", "AIRA-1", d0("ticket.update", "AIRA-1")) {
		t.Fatal("D0 rejected for ticket.update")
	}
	// D1 is accepted only for allocation-bearing verbs.
	for _, verb := range []string{"id.allocate", "requirement.create", "requirement.import"} {
		if !validJournalEventDigest(verb, "AR-1", d1req(verb, "AR-1")) {
			t.Fatalf("D1 rejected for allocation verb %s", verb)
		}
	}
	// A three-part digest on a non-allocation verb is refused (no kind smuggling).
	if validJournalEventDigest("ticket.update", "AIRA-1", d1req("ticket.update", "AIRA-1")) {
		t.Fatal("D1 accepted for non-allocation verb ticket.update")
	}
	// Garbage is refused.
	if validJournalEventDigest("id.allocate", "AR-1", "deadbeef") {
		t.Fatal("garbage digest accepted")
	}
}

func assertEventDigest(t *testing.T, s *Store, verb, target, want string) {
	t.Helper()
	var got string
	if err := s.db.QueryRow(`SELECT payload_digest FROM events WHERE verb=? AND target=?`, verb, target).Scan(&got); err != nil {
		t.Fatalf("event %s/%s not found: %v", verb, target, err)
	}
	if got != want {
		t.Fatalf("event %s/%s digest=%s, want %s", verb, target, got, want)
	}
}

func tamperJournalEventDigest(t *testing.T, path, verb, target, newDigest string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e eventRecord
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Verb == verb && e.Target == target {
			e.PayloadDigest = newDigest
			found = true
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	if !found {
		t.Fatalf("journal event %s/%s not found to tamper", verb, target)
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
