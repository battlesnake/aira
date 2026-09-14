package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// AIRA-237 Task 2 — id-accepting ticket-import verb.

// allocationRow reads a single allocation row for assertions.
func allocationRow(t *testing.T, s *Store, prefix string, number int64) (state, path string, ok bool) {
	t.Helper()
	err := s.db.QueryRow(`SELECT state, path FROM allocations WHERE project_id=? AND prefix=? AND number=?`,
		s.projectID, prefix, number).Scan(&state, &path)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("allocationRow(%s-%d): %v", prefix, number, err)
	}
	return state, path, true
}

func counterValue(t *testing.T, s *Store, prefix string) (int64, bool) {
	t.Helper()
	var n int64
	err := s.db.QueryRow(`SELECT next_number FROM id_counters WHERE project_id=? AND prefix=?`, s.projectID, prefix).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("counterValue(%s): %v", prefix, err)
	}
	return n, true
}

func importEventCount(t *testing.T, s *Store, target string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND verb='ticket.import' AND target=?`,
		s.projectID, target).Scan(&n); err != nil {
		t.Fatalf("importEventCount(%s): %v", target, err)
	}
	return n
}

// verifies: a 2-row bare-id JSONL import creates both tickets under the project
// prefix, keys/filenames the compound id, writes a ticket.import event per row,
// and seeds id_counters to number+1.
func TestImportTicketsCreatesPrefixedAndSeedsCounter(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL", "NF")

	data := strings.Join([]string{
		`{"id":"BL-1","title":"first backlog item","status":"planned","kind":"chore","severity":"P1","body":"body one","labels":["area:x"]}`,
		`{"id":"NF-1","title":"first nf item","status":"in-progress","kind":"feature","severity":"P0","body":"body two"}`,
	}, "\n")

	summary, err := s.ImportTicketsBytes(ctx, []byte(data), false, nil)
	if err != nil {
		t.Fatalf("ImportTicketsBytes: %v", err)
	}
	if len(summary.Created) != 2 {
		t.Fatalf("created = %v; want 2 ids", summary.Created)
	}
	if len(summary.Errored) != 0 {
		t.Fatalf("errored = %v; want none", summary.Errored)
	}

	for _, id := range []string{"FEE-BL-1", "FEE-NF-1"} {
		rec, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if rec.Ticket.ID != id {
			t.Fatalf("stored id = %q; want %q", rec.Ticket.ID, id)
		}
		if _, err := os.Stat(filepath.Join(s.root, ".aira", "tickets", id+".md")); err != nil {
			t.Fatalf("ticket file %s.md missing: %v", id, err)
		}
		if importEventCount(t, s, id) != 1 {
			t.Fatalf("ticket.import event count for %s = %d; want 1", id, importEventCount(t, s, id))
		}
		state, _, ok := allocationRow(t, s, "FEE-"+strings.SplitN(id, "-", 3)[1], mustNumber(t, id))
		if !ok || state != "materialised" {
			t.Fatalf("allocation for %s: ok=%v state=%q; want materialised", id, ok, state)
		}
	}

	// Severity/status/kind mapping landed as authored (P0 kept, in-progress
	// force-set without a transition graph, feature kind).
	nf, _ := s.Get("FEE-NF-1")
	if nf.Ticket.Severity != domain.SeverityP0 || nf.Ticket.Status != domain.StatusInProgress || nf.Ticket.Kind != domain.KindFeature {
		t.Fatalf("NF-1 fields = %+v; want P0/in-progress/feature", nf.Ticket)
	}

	// id_counters seeded to number+1 so the forward allocator never re-mints.
	if v, ok := counterValue(t, s, "FEE-BL"); !ok || v != 2 {
		t.Fatalf("FEE-BL counter = %d ok=%v; want 2", v, ok)
	}
	if v, ok := counterValue(t, s, "FEE-NF"); !ok || v != 2 {
		t.Fatalf("FEE-NF counter = %d ok=%v; want 2", v, ok)
	}
}

// TestImportTicketsAcceptsExtractorOriginField pins AIRA-244: the fastest.ee
// extractor stamps a per-row "origin":"fastest-ee-backlog-export" on EVERY row
// of a real export (verified against a 2007-row export), and rawTicketRow's
// DisallowUnknownFields decoder used to reject every such row with
// `json: unknown field "origin"`, failing the whole import. A row carrying
// origin must parse and import cleanly on BOTH the create and the refresh
// path, and the field must otherwise be ignored — not persisted onto the
// ticket, and not consulted by the disappear-on-reimport scoping, which keys
// off the journaled `events` table (verb=ticket.import), never this field.
//
// verifies: AIRA-244
func TestImportTicketsAcceptsExtractorOriginField(t *testing.T) {
	s := namespacedStore(t, "FEE", "BL")

	created := importOne(t, s, `{"id":"BL-1","title":"first","status":"planned","kind":"chore","severity":"P1","body":"b","origin":"fastest-ee-backlog-export"}`)
	if len(created.Errored) != 0 {
		t.Fatalf("row carrying origin should import cleanly; errored=%v", created.Errored)
	}
	if len(created.Created) != 1 || created.Created[0] != "FEE-BL-1" {
		t.Fatalf("created = %v; want [FEE-BL-1]", created.Created)
	}
	rec, err := s.Get("FEE-BL-1")
	if err != nil {
		t.Fatalf("Get(FEE-BL-1): %v", err)
	}
	if rec.Ticket.Title != "first" {
		t.Fatalf("ticket = %+v; origin must not corrupt normal fields", rec.Ticket)
	}
	// Origin is accepted-as-provenance only: it is not rendered anywhere onto
	// the stored ticket file (domain.Ticket carries no Origin field at all, so
	// this also guards against ever adding one by accident).
	raw, err := os.ReadFile(filepath.Join(s.root, ".aira", "tickets", "FEE-BL-1.md"))
	if err != nil {
		t.Fatalf("read stored ticket file: %v", err)
	}
	if strings.Contains(string(raw), "fastest-ee-backlog-export") {
		t.Fatalf("origin leaked into the stored ticket file: %s", raw)
	}

	// Refresh path: a re-import of the same id, still carrying origin (with a
	// changed title so the refresh is not a no-op), must also import cleanly.
	refreshed := importOne(t, s, `{"id":"BL-1","title":"first, revised","status":"planned","kind":"chore","severity":"P1","body":"b","origin":"fastest-ee-backlog-export"}`)
	if len(refreshed.Errored) != 0 {
		t.Fatalf("re-import carrying origin should refresh cleanly; errored=%v", refreshed.Errored)
	}
	if len(refreshed.Refreshed) != 1 || refreshed.Refreshed[0] != "FEE-BL-1" {
		t.Fatalf("refreshed = %v; want [FEE-BL-1]", refreshed.Refreshed)
	}
	rec2, err := s.Get("FEE-BL-1")
	if err != nil {
		t.Fatalf("Get(FEE-BL-1) after refresh: %v", err)
	}
	if rec2.Ticket.Title != "first, revised" {
		t.Fatalf("refreshed ticket = %+v; want revised title", rec2.Ticket)
	}
}

func mustNumber(t *testing.T, id string) int64 {
	t.Helper()
	_, n, _ := splitTicketID(id)
	return int64(n)
}

// namespacedStoreSharing opens a second worktree store on the SAME machine-wide
// state.db / common dir / project as `base`, with a distinct root+worktree — the
// cross-worktree shape shape-2 relies on (aira id in one worktree, aira import in
// another).
func namespacedStoreSharing(t *testing.T, base, worktreeID string, idPrefix string, prefixes ...string) *Store {
	t.Helper()
	root := filepath.Join(base, worktreeID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"),
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		ProjectID: "project-fee", WorktreeID: worktreeID, ProjectSlug: "fee",
		Prefixes: prefixes, IDPrefix: idPrefix,
	})
	if err != nil {
		t.Fatalf("open sharing store %s: %v", worktreeID, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func importOne(t *testing.T, s *Store, jsonl string) ImportTicketsSummary {
	t.Helper()
	summary, err := s.ImportTicketsBytes(context.Background(), []byte(jsonl), false, nil)
	if err != nil {
		t.Fatalf("ImportTicketsBytes: %v", err)
	}
	return summary
}

// verifies (Step 5): a re-import overlays only backlog fields; an aira-added
// relation and Hold/Assignee/Milestone (frontmatter-resident aira state) SURVIVE
// — the mutation that reds a render-from-row impl — while title/status refresh.
func TestImportTicketsReadMergePreservesCoordination(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")

	importOne(t, s, strings.Join([]string{
		`{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b1"}`,
		`{"id":"BL-9","title":"nine","status":"planned","kind":"chore","severity":"P2","body":"b9"}`,
	}, "\n"))

	// An aira-added relation whose canonical owner is FEE-BL-1 (the lower id),
	// present in NO import row.
	if _, err := s.Link(ctx, "BL-1", domain.RelationBlocks, "BL-9"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	// Frontmatter-resident aira state placed before re-import.
	assignee, milestone := "mark", "m1"
	if err := s.UpdateTicket(ctx, "FEE-BL-1", func(tk domain.Ticket) (domain.Ticket, error) {
		tk.Hold, tk.Assignee, tk.Milestone = true, &assignee, &milestone
		return tk, nil
	}); err != nil {
		t.Fatalf("UpdateTicket set coordination: %v", err)
	}

	// Re-import BL-1 with a changed title/status (backlog is truth), no links.
	summary := importOne(t, s, `{"id":"BL-1","title":"one-renamed","status":"in-progress","kind":"chore","severity":"P2","body":"b1"}`)
	if len(summary.Refreshed) != 1 || summary.Refreshed[0] != "FEE-BL-1" {
		t.Fatalf("refreshed = %v; want [FEE-BL-1]", summary.Refreshed)
	}

	rec, err := s.Get("FEE-BL-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	tk := rec.Ticket
	if tk.Title != "one-renamed" || tk.Status != domain.StatusInProgress {
		t.Fatalf("backlog fields not refreshed: %+v", tk)
	}
	if !tk.Hold || tk.Assignee == nil || *tk.Assignee != "mark" || tk.Milestone == nil || *tk.Milestone != "m1" {
		t.Fatalf("aira coordination fields clobbered: hold=%v assignee=%v milestone=%v", tk.Hold, tk.Assignee, tk.Milestone)
	}
	if len(tk.Relations) != 1 || tk.Relations[0].Kind != domain.RelationBlocks || tk.Relations[0].To != "FEE-BL-9" {
		t.Fatalf("aira-added relation was stripped by re-import: %+v", tk.Relations)
	}
}

// verifies (Step 5): a byte-identical re-import is a true no-op (post-merge
// digest), and a dropped previously-imported id is REPORTED (absent_from_batch),
// not retired; a natively-created ticket is never in the set.
func TestImportTicketsUnchangedAndDisappearReport(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")

	row1 := `{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b1"}`
	row2 := `{"id":"BL-2","title":"two","status":"planned","kind":"chore","severity":"P2","body":"b2"}`
	importOne(t, s, strings.Join([]string{row1, row2}, "\n"))

	// A natively-created ticket (no ticket.import event).
	native, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "native", Kind: domain.KindChore, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}

	before, _ := fileDigest(s.ticketPath("FEE-BL-1"))

	// Re-import ONLY row1, byte-identical → unchanged; BL-2 dropped → reported.
	summary := importOne(t, s, row1)
	if len(summary.Unchanged) != 1 || summary.Unchanged[0] != "FEE-BL-1" {
		t.Fatalf("unchanged = %v; want [FEE-BL-1]", summary.Unchanged)
	}
	after, _ := fileDigest(s.ticketPath("FEE-BL-1"))
	if before != after {
		t.Fatalf("unchanged re-import rewrote the file (digest changed)")
	}
	if len(summary.AbsentFromBatch) != 1 || summary.AbsentFromBatch[0] != "FEE-BL-2" {
		t.Fatalf("absent_from_batch = %v; want [FEE-BL-2] (native ticket %s MUST NOT appear)", summary.AbsentFromBatch, native.ID)
	}
	// BL-2 still exists (report, not retire).
	if _, err := s.Get("FEE-BL-2"); err != nil {
		t.Fatalf("dropped ticket FEE-BL-2 was retired/deleted: %v", err)
	}
}

// verifies (Step 5/6): status is force-set through the file-write path, bypassing
// the transition graph UpdateTicketContent enforces (a graph-forbidden
// done→planned lands).
func TestImportTicketsStatusForceSetBypassesTransition(t *testing.T) {
	s := namespacedStore(t, "FEE", "BL")
	importOne(t, s, `{"id":"BL-1","title":"one","status":"done","kind":"chore","severity":"P2","body":"b"}`)
	// done→planned is not a legal transition (ValidateTransition), but the backlog
	// is authoritative for status, so the importer force-sets it.
	summary := importOne(t, s, `{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b"}`)
	if len(summary.Refreshed) != 1 {
		t.Fatalf("refreshed = %v; want the forced transition to land", summary.Refreshed)
	}
	rec, _ := s.Get("FEE-BL-1")
	if rec.Ticket.Status != domain.StatusPlanned {
		t.Fatalf("status = %q; want planned (graph-forbidden transition must land)", rec.Ticket.Status)
	}
}

// verifies (Step 6): two links to one owner rewrite that owner file ONCE (one
// relation.add event), re-applying an existing link is a no-op, and an inverse
// kind is a named row error.
func TestImportTicketsTwoPassLinksGroupedByOwner(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL", "NF")

	// BL-1 is the canonical owner for both links (lower id).
	summary := importOne(t, s, strings.Join([]string{
		`{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b","links":[{"kind":"blocks","to":"BL-2"},{"kind":"relates","to":"NF-1"}]}`,
		`{"id":"BL-2","title":"two","status":"planned","kind":"chore","severity":"P2","body":"b"}`,
		`{"id":"NF-1","title":"nf","status":"planned","kind":"chore","severity":"P2","body":"b"}`,
	}, "\n"))
	if len(summary.Errored) != 0 {
		t.Fatalf("errored = %v; want none", summary.Errored)
	}
	rels, err := s.Relations("FEE-BL-1")
	if err != nil {
		t.Fatalf("Relations: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("relations on FEE-BL-1 = %d; want 2", len(rels))
	}
	// Owner file rewritten exactly once: one relation.add event.
	var addEvents int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND verb='relation.add'`, s.projectID).Scan(&addEvents); err != nil {
		t.Fatalf("count relation.add: %v", err)
	}
	if addEvents != 1 {
		t.Fatalf("relation.add events = %d; want 1 (one read-merge-render per owner)", addEvents)
	}

	// Re-import the same links → no-op (no new relation.add event).
	importOne(t, s, `{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b","links":[{"kind":"blocks","to":"BL-2"},{"kind":"relates","to":"NF-1"}]}`)
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND verb='relation.add'`, s.projectID).Scan(&addEvents); err != nil {
		t.Fatalf("count relation.add after re-import: %v", err)
	}
	if addEvents != 1 {
		t.Fatalf("re-applying an existing link must be a no-op; relation.add events = %d; want 1", addEvents)
	}

	// An inverse-kind row is a named error, counted.
	inv := importOne(t, s, `{"id":"BL-3","title":"three","status":"planned","kind":"chore","severity":"P2","body":"b","links":[{"kind":"blocked-by","to":"BL-1"}]}`)
	if len(inv.Errored) != 1 || !strings.Contains(inv.Errored[0].Error, "forward relation") {
		t.Fatalf("inverse-kind link should be a named error; got %v", inv.Errored)
	}
	_ = ctx
}

// verifies (Step 6): a dangling endpoint is a NAMED per-row error, counted in
// non-strict; in strict the whole import writes nothing.
func TestImportTicketsDanglingEndpoint(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	_ = base

	// Non-strict: the row's own fields still import; the bad link is counted.
	s := namespacedStore(t, "FEE", "BL", "NF")
	summary := importOne(t, s, `{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b","links":[{"kind":"blocks","to":"NF-99"}]}`)
	if len(summary.Errored) != 1 || !strings.Contains(summary.Errored[0].Error, "E_RELATION_TARGET_MISSING") {
		t.Fatalf("dangling endpoint should be a named counted error; got %v", summary.Errored)
	}
	if len(summary.Created) != 1 {
		t.Fatalf("the row's valid fields should still import in non-strict; created=%v", summary.Created)
	}

	// Strict: zero writes on any error.
	s2 := namespacedStore(t, "FEE", "BL", "NF")
	_, err := s2.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b","links":[{"kind":"blocks","to":"NF-99"}]}`), true, nil)
	if err == nil || !strings.Contains(err.Error(), "E_RELATION_TARGET_MISSING") {
		t.Fatalf("strict import should fail with the dangling error; got %v", err)
	}
	if _, gerr := s2.Get("FEE-BL-1"); ErrorCode(gerr) != "E_NOT_FOUND" {
		t.Fatalf("strict abort must write nothing; FEE-BL-1 exists: %v", gerr)
	}
	if n := importEventCount(t, s2, "FEE-BL-1"); n != 0 {
		t.Fatalf("strict abort must write no ticket.import event; got %d", n)
	}
}

// verifies (Step 6): the id/prefix/status/severity contract — unowned prefix,
// an already-prefixed id, malformed status, and the severity ladder (P0 kept,
// P4→P3, default P2) are each named row errors or applied as specified.
func TestImportTicketsValidationContract(t *testing.T) {
	s := namespacedStore(t, "FEE", "BL")

	// Unowned prefix.
	unowned := importOne(t, s, `{"id":"ZZ-1","title":"x","status":"planned","kind":"chore","severity":"P2","body":"b"}`)
	if len(unowned.Errored) != 1 || !strings.Contains(unowned.Errored[0].Error, "unowned") {
		t.Fatalf("unowned prefix should be E_IMPORT_INVALID unowned; got %v", unowned.Errored)
	}

	// An already-prefixed id (import is strict — the canonicaliser prepends only
	// interactive input, never an import row).
	prefixed := importOne(t, s, `{"id":"FEE-BL-1","title":"x","status":"planned","kind":"chore","severity":"P2","body":"b"}`)
	if len(prefixed.Errored) != 1 || !strings.Contains(prefixed.Errored[0].Error, "bare PREFIX-N") {
		t.Fatalf("already-prefixed id should be a bare-shape error; got %v", prefixed.Errored)
	}

	// Malformed status.
	badStatus := importOne(t, s, `{"id":"BL-5","title":"x","status":"wip","kind":"chore","severity":"P2","body":"b"}`)
	if len(badStatus.Errored) != 1 || !strings.Contains(badStatus.Errored[0].Error, "invalid status") {
		t.Fatalf("malformed status should be a named error; got %v", badStatus.Errored)
	}

	// Severity ladder + kind default: P4→P3, empty severity→P2, empty kind→chore.
	importOne(t, s, strings.Join([]string{
		`{"id":"BL-10","title":"p4","status":"planned","severity":"P4","body":"b"}`,
		`{"id":"BL-11","title":"nosev","status":"planned","body":"b"}`,
	}, "\n"))
	r10, _ := s.Get("FEE-BL-10")
	if r10.Ticket.Severity != domain.SeverityP3 || r10.Ticket.Kind != domain.KindChore {
		t.Fatalf("BL-10 = %+v; want P3 clamp + chore default", r10.Ticket)
	}
	r11, _ := s.Get("FEE-BL-11")
	if r11.Ticket.Severity != domain.SeverityP2 {
		t.Fatalf("BL-11 severity = %q; want P2 default", r11.Ticket.Severity)
	}
}

// verifies (Step 6 / cross-worktree): aira id in worktree A records a
// state='allocated' row with A's path; aira import in worktree B materialises it
// (path rewritten to B), NOT the import_requirements.go path-refusal. A
// subsequent import of the same id from A (its file absent) is ADOPTED — the
// same ticket, materialised here — never refused, and the allocation path stays
// at B. (The file-present → refreshed direction is pinned in
// TestImportCrossWorktreeReimportRefreshesPathStaysB.)
func TestImportTicketsCrossWorktreeMaterialise(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := namespacedStoreSharing(t, base, "wt-a", "FEE", "BL")
	b := namespacedStoreSharing(t, base, "wt-b", "FEE", "BL")

	// A mints FEE-BL-1 (pre-allocated, state='allocated', A's path).
	id, err := a.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("AllocateID: %v", err)
	}
	if id != "FEE-BL-1" {
		t.Fatalf("minted %q; want FEE-BL-1", id)
	}
	state, allocPath, ok := allocationRow(t, a, "FEE-BL", 1)
	if !ok || state != "allocated" {
		t.Fatalf("post-mint allocation state = %q; want allocated", state)
	}
	if !strings.Contains(allocPath, filepath.Join("wt-a", ".aira")) {
		t.Fatalf("pre-alloc path = %q; want A's worktree", allocPath)
	}

	// B imports the same id → materialise HERE (accept the cross-worktree mint).
	summary, err := b.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"adopted","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil)
	if err != nil {
		t.Fatalf("cross-worktree import: %v", err)
	}
	if len(summary.Created) != 1 {
		t.Fatalf("created = %v; want [FEE-BL-1] materialised in B", summary.Created)
	}
	state, allocPath, _ = allocationRow(t, b, "FEE-BL", 1)
	if state != "materialised" {
		t.Fatalf("allocation state after import = %q; want materialised", state)
	}
	if !strings.Contains(allocPath, filepath.Join("wt-b", ".aira")) {
		t.Fatalf("allocation path after import = %q; want rewritten to B's worktree", allocPath)
	}
	if _, err := b.Get("FEE-BL-1"); err != nil {
		t.Fatalf("Get in B after materialise: %v", err)
	}

	// Now A imports the same id → the same ticket, ADOPTED (A has no file, so it
	// materialises A's own copy); the false "different path" refusal is gone and
	// the allocation path/worktree stay at B (state already materialised).
	adopt, err := a.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"adopted-in-a","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil)
	if err != nil {
		t.Fatalf("A importing a row materialised at B's path must be adopted, not refused; got %v", err)
	}
	if len(adopt.Created) != 1 || adopt.Created[0] != "FEE-BL-1" {
		t.Fatalf("A adopt import = %+v; want created [FEE-BL-1]", adopt)
	}
	state, allocPath, _ = allocationRow(t, b, "FEE-BL", 1)
	if state != "materialised" || !strings.Contains(allocPath, filepath.Join("wt-b", ".aira")) {
		t.Fatalf("allocation after A adopt = (state=%q,path=%q); want materialised at B's worktree", state, allocPath)
	}
}

// (a) HIGH finding + markTicketMaterialised CASE no-clobber: once B has
// materialised FEE-BL-1, a re-import from worktree A (A's own file now present,
// content changed) is REFRESHED — never the deleted E_IMPORT_INVALID
// "different path" refusal — and the allocation row's path/worktree still point
// at B. A CASE-removal mutation in markTicketMaterialised would flip the path to
// A, so this also pins the no-clobber direction.
func TestImportCrossWorktreeReimportRefreshesPathStaysB(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := namespacedStoreSharing(t, base, "wt-a", "FEE", "BL")
	b := namespacedStoreSharing(t, base, "wt-b", "FEE", "BL")

	// A mints; B materialises (allocation path rewritten to B while it was
	// state='allocated').
	if _, err := a.AllocateID(ctx, "BL"); err != nil {
		t.Fatalf("AllocateID: %v", err)
	}
	if _, err := b.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"adopted","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil); err != nil {
		t.Fatalf("B materialise: %v", err)
	}
	_, bPath, _ := allocationRow(t, b, "FEE-BL", 1)
	if !strings.Contains(bPath, filepath.Join("wt-b", ".aira")) {
		t.Fatalf("post-materialise alloc path = %q; want B's worktree", bPath)
	}

	// A imports the same id → adopt the SAME ticket, creating A's own file; the
	// allocation path/worktree stay at B (state already materialised).
	created, err := a.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"adopted","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil)
	if err != nil {
		t.Fatalf("A cross-worktree import (adopt) must succeed, not E_IMPORT_INVALID: %v", err)
	}
	if len(created.Created) != 1 || created.Created[0] != "FEE-BL-1" {
		t.Fatalf("A first import = %+v; want created [FEE-BL-1]", created)
	}

	// A re-imports with CHANGED content → REFRESHED; path/worktree still B.
	refreshed, err := a.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"changed-in-A","status":"in-progress","kind":"chore","severity":"P1","body":"b2"}`), true, nil)
	if err != nil {
		t.Fatalf("A re-import (refresh) must succeed, not E_IMPORT_INVALID: %v", err)
	}
	if len(refreshed.Refreshed) != 1 || refreshed.Refreshed[0] != "FEE-BL-1" {
		t.Fatalf("A re-import = %+v; want refreshed [FEE-BL-1]", refreshed)
	}
	state, path, _ := allocationRow(t, b, "FEE-BL", 1)
	if state != "materialised" {
		t.Fatalf("allocation state = %q; want materialised", state)
	}
	if !strings.Contains(path, filepath.Join("wt-b", ".aira")) {
		t.Fatalf("allocation path after A re-import = %q; want STILL B's worktree (CASE no-clobber)", path)
	}
}

// (b) After a state.db loss, a Rebuild of a cross-worktree-adopted ticket
// re-points the reconstructed allocation (receipt-replayed as state='allocated'
// at the minting worktree A's path) at the worktree that actually holds the
// file (B), so Check finds no fabricated E_ID_UNRESOLVED. Reds before fix #2.
func TestImportRebuildResolvesCrossWorktreeAdoptedAllocation(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := namespacedStoreSharing(t, base, "wt-a", "FEE", "BL")
	b := namespacedStoreSharing(t, base, "wt-b", "FEE", "BL")

	if _, err := a.AllocateID(ctx, "BL"); err != nil {
		t.Fatalf("AllocateID: %v", err)
	}
	if _, err := b.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"adopted","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil); err != nil {
		t.Fatalf("B materialise: %v", err)
	}
	_ = a.Close()
	_ = b.Close()

	// Lose the whole database; the durable receipts + journal survive.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(base, "state", "state.db"+suffix))
	}

	rebuilt := namespacedStoreSharing(t, base, "wt-b", "FEE", "BL")
	if err := rebuilt.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild after db loss: %v", err)
	}
	report, err := rebuilt.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, f := range report.Findings {
		if f.Code == "E_ID_UNRESOLVED" {
			t.Fatalf("Check reports E_ID_UNRESOLVED after cross-worktree rebuild: %+v", f)
		}
	}
	_, path, ok := allocationRow(t, rebuilt, "FEE-BL", 1)
	if !ok {
		t.Fatal("allocation row for FEE-BL-1 missing after rebuild")
	}
	if !strings.Contains(path, filepath.Join("wt-b", ".aira")) {
		t.Fatalf("allocation path after rebuild = %q; want re-pointed to B's worktree", path)
	}
}

// (c) Strict zero-write on a pass-0-detectable failure: a strict import of
// [valid row1, bad-link row2] writes NOTHING — row1's file never appears and no
// ticket.import event is recorded — pinning the honest strict contract (zero
// writes on parse/link validation failures, caught by the pass-0 probe).
func TestImportStrictZeroWriteOnPass0LinkFailure(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL", "NF")

	_, err := s.ImportTicketsBytes(ctx, []byte(strings.Join([]string{
		`{"id":"BL-1","title":"valid row one","status":"planned","kind":"chore","severity":"P2","body":"b1"}`,
		`{"id":"BL-2","title":"bad link row","status":"planned","kind":"chore","severity":"P2","body":"b2","links":[{"kind":"blocks","to":"NF-99"}]}`,
	}, "\n")), true, nil)
	if err == nil || !strings.Contains(err.Error(), "E_RELATION_TARGET_MISSING") {
		t.Fatalf("strict import with a dangling link should fail zero-write; got %v", err)
	}
	// The fully-valid row1 must NOT have been written before the batch aborted.
	if _, gerr := s.Get("FEE-BL-1"); ErrorCode(gerr) != "E_NOT_FOUND" {
		t.Fatalf("strict pass-0 abort must write nothing; FEE-BL-1 exists: %v", gerr)
	}
	if n := importEventCount(t, s, "FEE-BL-1"); n != 0 {
		t.Fatalf("strict pass-0 abort must write no ticket.import event for row1; got %d", n)
	}
}

// verifies (Step 6 follow-up): requirement verbs canonicalize a bare id on input
// (the 405-row cutover break), and a finding's bare requirement id keys the
// stored compound.
func TestRequirementVerbsCanonicalizeUnderNamespacing(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, Options{
		Root: root, CommonDir: filepath.Join(base, "common"),
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		ProjectID: "project-fee", WorktreeID: "wt-main", ProjectSlug: "fee",
		Prefixes: []string{"BL"}, RequirementPrefixes: []string{"VR"}, IDPrefix: "FEE",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	req, _, err := s.AddRequirement(ctx, domain.RequirementInput{Text: "must hold", Status: domain.RequirementPlanned})
	if err != nil {
		t.Fatalf("AddRequirement: %v", err)
	}
	if req.ID != "FEE-VR-1" {
		t.Fatalf("minted requirement id = %q; want FEE-VR-1 (composed)", req.ID)
	}
	// A bare VR-1 must resolve the stored FEE-VR-1.
	rec, err := s.GetRequirement("VR-1")
	if err != nil {
		t.Fatalf("GetRequirement(VR-1) bare: %v", err)
	}
	if rec.Requirement.ID != "FEE-VR-1" {
		t.Fatalf("GetRequirement stored id = %q; want FEE-VR-1", rec.Requirement.ID)
	}
	// SetRequirement bare resolves too.
	if _, err := s.SetRequirement(ctx, "VR-1", domain.RequirementStatus("built")); err != nil {
		t.Fatalf("SetRequirement(VR-1) bare: %v", err)
	}

	// A finding created with a bare requirement id keys the compound + matches
	// the canonicalised requirement: term.
	// Need a ticket for the finding to hang off.
	importOne(t, s, `{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b"}`)
	if _, _, err := s.AddFinding(ctx, domain.ReviewFindingInput{
		TicketID: "BL-1", Category: "correctness", Severity: domain.SeverityP2,
		Verdict: domain.Verdict("confirmed"), Source: "sol", Message: "m", RequirementID: "VR-1",
	}); err != nil {
		t.Fatalf("AddFinding: %v", err)
	}
	rows, err := s.ListFindings("requirement:VR-1")
	if err != nil {
		t.Fatalf("ListFindings(requirement:VR-1): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("find requirement:VR-1 matched %d; want 1 (bare term canonicalised)", len(rows))
	}
	if rows[0].Finding.RequirementID != "FEE-VR-1" {
		t.Fatalf("finding requirement id stored = %q; want FEE-VR-1", rows[0].Finding.RequirementID)
	}
}
