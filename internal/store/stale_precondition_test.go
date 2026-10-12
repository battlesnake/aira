// verifies: AIRA-289

package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"aira/internal/domain"
)

// pendingAt counts the pending (unmaterialised) outbox rows for one path.
func pendingAt(t *testing.T, s *Store, path string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE project_id=? AND worktree_id=? AND path=? AND materialised=0`,
		s.projectID, s.worktreeID, path).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A writer that read a file, lost a race to a peer that wrote it, and only then
// prepared its own intent used to leave a pending row behind. The row's
// precondition was already stale, so materialise refused it E_WRITE_CONFLICT
// forever, and every later write to that path (the retry included) was refused
// E_PATH_INTENT_BUSY until `aira intent-retire`. The refusal must come BEFORE the
// row exists: a stale writer fails clean, and the path stays writable.
func TestStalePreconditionIsRefusedWithoutLeavingAPendingIntent(t *testing.T) {
	s, _, _ := m3Store(t)
	ticket := m3Ticket(t, s, "raced")
	ctx := context.Background()
	path := s.ticketPath(ticket.ID)

	// The "slow" writer: it has read the file when the update callback runs, and
	// a peer write lands between that read and the slow writer's prepare.
	_, err := s.UpdateTicketContent(ctx, ticket.ID, func(current domain.Ticket, body string) (domain.Ticket, string, error) {
		if peerErr := s.UpdateTicket(ctx, ticket.ID, func(peer domain.Ticket) (domain.Ticket, error) {
			peer.Title = "peer won"
			return peer, nil
		}); peerErr != nil {
			t.Fatalf("peer write: %v", peerErr)
		}
		current.Title = "slow writer"
		return current, body, nil
	})
	if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("stale writer error = %v, want ErrWriteConflict", err)
	}
	if n := pendingAt(t, s, path); n != 0 {
		t.Fatalf("the refused writer left %d pending intent(s); the path is wedged until intent-retire", n)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := domain.ParseTicket(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Title != "peer won" {
		t.Fatalf("title = %q, want the peer's write to survive", parsed.Title)
	}
	// The retry must not be wedged.
	if err := s.UpdateTicket(ctx, ticket.ID, func(retry domain.Ticket) (domain.Ticket, error) {
		retry.Title = "retried"
		return retry, nil
	}); err != nil {
		t.Fatalf("retry after the refused write: %v", err)
	}
	// And reconcile has nothing to complain about.
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile after a refused stale write: %v", err)
	}
}

// The same window for a relation write: link and unlink read the owner file and
// call preparePathMutationEvent with its digest as the precondition.
func TestStaleRelationPreconditionIsRefusedWithoutLeavingAPendingIntent(t *testing.T) {
	s, _, _ := m3Store(t)
	a := m3Ticket(t, s, "a")
	b := m3Ticket(t, s, "b")
	c := m3Ticket(t, s, "c")
	ctx := context.Background()
	path := s.ticketPath(domain.CanonicalRelationOwner(a.ID, b.ID))
	if path != s.ticketPath(domain.CanonicalRelationOwner(a.ID, c.ID)) {
		t.Fatalf("fixture: want one owner file for both links")
	}

	// What Link reads before it prepares.
	read, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A peer link to the same owner file lands in between.
	if _, err := s.Link(ctx, a.ID, domain.RelationRelates, c.ID); err != nil {
		t.Fatal(err)
	}
	ticket, body, err := domain.ParseTicket(read)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Relations = append(ticket.Relations, domain.Relation{Kind: domain.RelationRelates, From: a.ID, To: b.ID})
	stale, err := domain.RenderTicket(ticket, body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.preparePathMutationEvent(ctx, path, digestBytes(read), stale, "relation.add", "stale")
	if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("stale prepare error = %v, want ErrWriteConflict", err)
	}
	if n := pendingAt(t, s, path); n != 0 {
		t.Fatalf("the refused prepare left %d pending intent(s)", n)
	}
	// The retried link (a fresh read) goes through and both relations are kept.
	if _, err := s.Link(ctx, a.ID, domain.RelationRelates, b.ID); err != nil {
		t.Fatalf("retried link: %v", err)
	}
	views, err := s.Relations(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("relations = %#v, want both links", views)
	}
}

// Section 3.7's idempotent clause: a writer whose precondition is stale only
// because a peer already wrote EXACTLY the bytes it intends (two identical
// `aira link` calls racing from one read) has nothing to conflict with. Both
// return OK, the file holds those bytes, and no pending row is left (build
// review round 3: refusing it E_WRITE_CONFLICT left the package green).
func TestAnIdenticalRacedWriteIsNotAConflict(t *testing.T) {
	s, _, _ := m3Store(t)
	a := m3Ticket(t, s, "a")
	b := m3Ticket(t, s, "b")
	ctx := context.Background()
	path := s.ticketPath(domain.CanonicalRelationOwner(a.ID, b.ID))

	// Both writers read the same file and compute the same new bytes.
	read, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ticket, body, err := domain.ParseTicket(read)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Relations = append(ticket.Relations, domain.Relation{Kind: domain.RelationRelates, From: a.ID, To: b.ID})
	intended, err := domain.RenderTicket(ticket, body)
	if err != nil {
		t.Fatal(err)
	}
	for _, writer := range []string{"first", "second"} {
		intent, err := s.preparePathMutationEvent(ctx, path, digestBytes(read), intended, "relation.add", writer)
		if err != nil {
			t.Fatalf("%s writer prepare: %v", writer, err)
		}
		if err := s.materialiseIntent(ctx, intent); err != nil {
			t.Fatalf("%s writer materialise: %v", writer, err)
		}
	}
	if n := pendingAt(t, s, path); n != 0 {
		t.Fatalf("%d pending intent(s) left after two identical writes", n)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if digestBytes(onDisk) != digestBytes(intended) {
		t.Fatal("the file does not hold the bytes both writers intended")
	}
	views, err := s.Relations(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("relations = %#v, want the one relation", views)
	}
}
