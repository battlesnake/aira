package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// quarantinedTicketFiles is the set of ticket files in THIS repository that
// domain.ParseTicket still refuses, with the hand-written defect in each. Every
// one predates AIRA-170 and none is caused by it; they are recorded on AIRA-171
// and repairing them is that ticket's whole content.
//
// The list is a CLOSED set, asserted in both directions below: a file that
// starts failing and is not named here fails the test, and a file named here
// that has been repaired also fails it, so the quarantine can neither grow
// silently nor rot into a vacuous allow-list once AIRA-171 lands.
var quarantinedTicketFiles = map[string]string{
	"AIRA-28":  "relations must be sorted",
	"AIRA-62":  "relation is not stored on its canonical lower-ID ticket",
	"AIRA-117": "ticket labels must be unique and sorted",
	"AIRA-141": "body does not end in a newline",
	"AIRA-144": "ticket labels must be unique and sorted",
	"AIRA-145": "body does not end in a newline",
	"AIRA-152": "relation is not stored on its canonical lower-ID ticket",
	"AIRA-153": "relation is not stored on its canonical lower-ID ticket",
	"AIRA-160": "ticket labels must be unique and sorted",
}

// TestTheRepositorysOwnTicketFilesAreReadable is the AIRA-170 reproduction the
// synthetic fixtures could not run.
//
// AIRA-170 was filed because `aira show AIRA-165` refused a ticket whose file
// was merged to master. The store tests that accompanied the first fix built
// their own P3 fixture, so they proved the ENUM was widened and nothing about
// the seven real files the ticket was named for — which, once the severity
// check passed, simply advanced to the NEXT hand-written defect in the same
// frontmatter and stayed unreadable. This test drives the real files, so the
// headline reproduction is what is actually asserted.
//
// It walks the whole ticket directory rather than only the AIRA-170 population
// because that is the durable form: any future ticket file that stops parsing
// fails here, whoever wrote it.
//
// verifies: AIRA-170
func TestTheRepositorysOwnTicketFilesAreReadable(t *testing.T) {
	tickets := repositoryTicketDir(t)
	base := t.TempDir()
	root := filepath.Join(base, "main")
	ids := copyTicketDir(t, tickets, filepath.Join(root, ".aira", "tickets"))
	if len(ids) < 100 {
		t.Fatalf("copied only %d ticket files from %s; the repository has far more", len(ids), tickets)
	}

	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The index is a rebuildable projection of those files, and the rant-ref
	// check reads the index rather than the file, so the reproduction needs the
	// projection built from the real content.
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	unreadable := map[string]string{}
	for _, id := range ids {
		if _, err := s.Get(id); err != nil {
			unreadable[id] = err.Error()
		}
	}
	for id, err := range unreadable {
		if _, quarantined := quarantinedTicketFiles[id]; !quarantined {
			t.Errorf("%s is unreadable and is not a known AIRA-171 quarantine: %s", id, err)
		}
	}
	for id, why := range quarantinedTicketFiles {
		if _, still := unreadable[id]; !still {
			t.Errorf("%s (%s) now parses: delete it from quarantinedTicketFiles, and close AIRA-171 when the map is empty", id, why)
		}
	}

	// The seven tickets AIRA-170 was filed about, plus AIRA-169 whose own
	// frontmatter this PR repaired. Named individually so the walk above cannot
	// be satisfied by an empty or truncated directory.
	for _, id := range []string{"AIRA-162", "AIRA-163", "AIRA-164", "AIRA-165", "AIRA-166", "AIRA-167", "AIRA-168"} {
		record, err := s.Get(id)
		if err != nil {
			t.Fatalf("show %s: %v", id, err)
		}
		if record.Ticket.Severity != domain.SeverityP3 {
			t.Fatalf("%s severity = %q, want P3 (the value the merged file carries)", id, record.Ticket.Severity)
		}
	}
	if _, err := s.Get("AIRA-169"); err != nil {
		t.Fatalf("show AIRA-169: %v", err)
	}

	// The repair that was NOT a deletion. All seven `N->AIRA-153` relations were
	// duplicates of a copy AIRA-153.md already held canonically and were simply
	// removed; `AIRA-165 relates AIRA-151` was stored nowhere
	// else, so it MOVED to its canonical owner. If a later edit drops it while
	// tidying AIRA-151.md, the relation is gone for good and nothing above would
	// notice, so it is asserted here.
	owner, err := s.Get("AIRA-151")
	if err != nil {
		t.Fatalf("show AIRA-151: %v", err)
	}
	moved := domain.Relation{Kind: domain.RelationRelates, From: "AIRA-165", To: "AIRA-151"}
	if !slices.Contains(owner.Ticket.Relations, moved) {
		t.Fatalf("AIRA-151 no longer stores the relation moved off AIRA-165: %#v", owner.Ticket.Relations)
	}

	// The other half of that losslessness claim: the seven deletions were safe
	// only because AIRA-153.md holds the mirror of each. AIRA-153 is itself
	// quarantined, so this reads its frontmatter directly rather than through
	// the parser that refuses it.
	assertAIRA153MirrorsSurvive(t, filepath.Join(root, ".aira", "tickets", "AIRA-153.md"))

	// The two operations AIRA-170's body names besides `show`, against the real
	// AIRA-165 file rather than a fixture.
	if _, err := s.AddRant(context.Background(),
		domain.RantInput{Body: "real-file ref", Refs: []domain.RantRef{{Kind: domain.RantRefTicket, ID: "AIRA-165"}}},
		gitcontext.GitContext{}); err != nil {
		t.Fatalf("rant --ref ticket:AIRA-165 against the real file: %v", err)
	}
	if _, err := s.Link(context.Background(), "AIRA-170", domain.RelationRelates, "AIRA-165"); err != nil {
		t.Fatalf("link AIRA-170 relates AIRA-165 against the real files: %v", err)
	}
}

// assertAIRA153MirrorsSurvive checks that AIRA-153.md still canonically stores
// `AIRA-153 relates AIRA-16x` for each of the seven tickets whose non-canonical
// copy this PR deleted. `relates` is its own inverse, so the mirror is what
// makes each deletion lossless rather than a quiet loss of seven edges.
func assertAIRA153MirrorsSurvive(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read AIRA-153.md: %v", err)
	}
	head, _, ok := strings.Cut(strings.TrimPrefix(string(data), "---\n"), "\n---")
	if !ok {
		t.Fatalf("AIRA-153.md has no frontmatter")
	}
	var frontmatter struct {
		Relations []domain.Relation `json:"relations"`
	}
	if err := json.Unmarshal([]byte(head), &frontmatter); err != nil {
		t.Fatalf("AIRA-153.md frontmatter: %v", err)
	}
	for _, id := range []string{"AIRA-162", "AIRA-163", "AIRA-164", "AIRA-165", "AIRA-166", "AIRA-167", "AIRA-168"} {
		mirror := domain.Relation{Kind: domain.RelationRelates, From: "AIRA-153", To: id}
		if !slices.Contains(frontmatter.Relations, mirror) {
			t.Fatalf("AIRA-153.md no longer mirrors %s, so deleting the copy on %s was not lossless", id, id)
		}
	}
}

// repositoryTicketDir locates this checkout's .aira/tickets. The directory is
// repository content, not an optional environment fixture, so failing to find
// it is a real failure and never a skip: a skip here would silently retire the
// only guard that reads the real files.
func repositoryTicketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, ".aira", "tickets")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no .aira/tickets directory above the test's working directory")
		}
		dir = parent
	}
}

// copyTicketDir copies every ticket file verbatim — byte for byte, so a
// trailing-newline defect survives the copy — and returns their IDs sorted.
func copyTicketDir(t *testing.T, from, to string) []string {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(to, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, strings.TrimSuffix(name, ".md"))
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		t.Fatalf("no ticket files in %s", from)
	}
	return ids
}
