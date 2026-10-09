package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// verifies: AIRA-284 — turn-id idempotency, write-time ticket association from a
// live lease only, per-partition retention, and the per-session summary.

func anthropicTurn(session, turn string, fresh, out int64) domain.ComputeEventInput {
	return domain.ComputeEventInput{
		Model: "claude-x", Provider: "anthropic", Source: "claude-mod", Session: session, TurnID: turn,
		Raw: domain.RawUsage{InputTokens: computeI64(fresh), CacheReadInputTokens: computeI64(1), CacheCreationInputTokens: computeI64(2), OutputTokens: computeI64(out)},
	}
}

// turnStore opens a lease-capable store (clock-injected) with small retention caps.
func turnStore(t *testing.T, maxEvents, ageDays int) (*Store, *m3Clock, string) {
	t.Helper()
	base := t.TempDir()
	clock := &m3Clock{boot: "boot-a", mono: 100}
	s, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"),
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		LeaseStateDir: filepath.Join(base, "lease-state"), ProjectID: "project-aira", WorktreeID: "worktree-a",
		ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
		MaxComputeEvents: maxEvents, MaxComputeAgeDays: ageDays,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clock, base
}

func TestComputeTurnDuplicateReturnsOriginalAndBurnsNoCounter(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	ctx := context.Background()
	first, err := s.AddComputeEvent(ctx, anthropicTurn("s1", "t1", 10, 5))
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate || first.ID != "CE-1" || first.Event.TurnID != "t1" {
		t.Fatalf("first add = %+v", first)
	}
	dup, err := s.AddComputeEvent(ctx, anthropicTurn("s1", "t1", 10, 5))
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Duplicate || dup.ID != "CE-1" || dup.Event.AtSeq != first.Event.AtSeq || dup.Event.At != first.Event.At || dup.Remaining != 1 || dup.EvictedCount != 0 {
		t.Fatalf("duplicate = %+v, want the original row flagged duplicate", dup)
	}
	// The duplicate must not have consumed a CE number or an at_seq.
	next, err := s.AddComputeEvent(ctx, anthropicTurn("s1", "t2", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "CE-2" || next.Event.AtSeq != 2 {
		t.Fatalf("duplicate burned a counter: next=%s at_seq=%d, want CE-2 at_seq 2", next.ID, next.Event.AtSeq)
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v, want 2", len(rows), err)
	}
}

func TestComputeTurnConflictRefusesAndFirstPayloadStands(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	ctx := context.Background()
	if _, err := s.AddComputeEvent(ctx, anthropicTurn("s1", "t1", 10, 5)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.ComputeEventInput){
		"different counters": func(in *domain.ComputeEventInput) { in.Raw.OutputTokens = computeI64(6) },
		"different model":    func(in *domain.ComputeEventInput) { in.Model = "claude-y" },
		"counter now absent": func(in *domain.ComputeEventInput) { in.Raw.CacheReadInputTokens = nil },
	} {
		in := anthropicTurn("s1", "t1", 10, 5)
		mutate(&in)
		_, err := s.AddComputeEvent(ctx, in)
		if ErrorCode(err) != domain.ComputeCodeTurnConflict {
			t.Fatalf("%s: err=%v code=%q, want %s", name, err, ErrorCode(err), domain.ComputeCodeTurnConflict)
		}
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 1 || rows[0].Buckets.Output == nil || *rows[0].Buckets.Output != 5 || rows[0].Model != "claude-x" {
		t.Fatalf("first payload did not stand: rows=%+v err=%v", rows, err)
	}
	next, err := s.AddComputeEvent(ctx, anthropicTurn("s1", "t2", 1, 1))
	if err != nil || next.ID != "CE-2" {
		t.Fatalf("a refused conflict burned a counter: %+v err=%v", next, err)
	}
}

func TestComputeTurnKeyIsPerSourceSessionAgent(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	ctx := context.Background()
	base := anthropicTurn("s1", "t1", 10, 5)
	if _, err := s.AddComputeEvent(ctx, base); err != nil {
		t.Fatal(err)
	}
	otherSession := anthropicTurn("s2", "t1", 99, 99)
	otherAgent := anthropicTurn("s1", "t1", 98, 98)
	otherAgent.Agent = "sub1"
	otherSource := anthropicTurn("s1", "t1", 97, 97)
	otherSource.Source = "manual"
	for name, in := range map[string]domain.ComputeEventInput{"session": otherSession, "agent": otherAgent, "source": otherSource} {
		got, err := s.AddComputeEvent(ctx, in)
		if err != nil || got.Duplicate {
			t.Fatalf("different %s must insert: %+v err=%v", name, got, err)
		}
	}
	rows, _ := s.ListComputeEvents("")
	if len(rows) != 4 {
		t.Fatalf("rows=%d, want 4", len(rows))
	}
}

func TestComputeTurnIDRequiresSessionAndHasCharset(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	in := anthropicTurn("", "t1", 1, 1)
	if _, err := s.AddComputeEvent(context.Background(), in); ErrorCode(err) != domain.ComputeCodeInvalid {
		t.Fatalf("turn-id without session err=%v", err)
	}
	in = anthropicTurn("s1", "bad turn", 1, 1)
	if _, err := s.AddComputeEvent(context.Background(), in); ErrorCode(err) != domain.ComputeCodeInvalid {
		t.Fatalf("turn-id with space err=%v", err)
	}
}

// The unique index is the backstop behind the in-transaction lookup: a writer
// that bypasses AddComputeEvent (or a lookup bug) still cannot store two rows
// for one key, while legacy rows (turn_id = ”) stay free to repeat.
func TestComputeTurnUniqueIndexBackstopsTheLookup(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	insert := `INSERT INTO compute_events(project_id,id,model,provider,at,session,agent,source,conservation,at_seq,turn_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`
	args := func(id string, seq int, turn string) []any {
		return []any{s.projectID, id, "m", "anthropic", "2026-10-09T00:00:00Z", "s1", "", "claude-mod", "unevaluated", seq, turn}
	}
	if _, err := s.db.Exec(insert, args("CE-x1", 1, "t1")...); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(insert, args("CE-x2", 2, "t1")...); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second row with the same key: err=%v, want UNIQUE violation", err)
	}
	if _, err := s.db.Exec(insert, args("CE-x3", 3, "")...); err != nil {
		t.Fatalf("legacy empty turn_id row refused: %v", err)
	}
	if _, err := s.db.Exec(insert, args("CE-x4", 4, "")...); err != nil {
		t.Fatalf("a second empty turn_id row must be allowed: %v", err)
	}
}

func TestComputeTurnMigrationOnLegacyDatabase(t *testing.T) {
	base, dbPath := legacyComputeDB(t, false)
	s, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: dbPath,
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira",
		WorktreeID: "main", ProjectSlug: "aira", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, column := range []string{"turn_id", "ticket_status"} {
		if !columnPresent(t, s.db, "compute_events", column) {
			t.Fatalf("migration omitted %s", column)
		}
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 1 || rows[0].TurnID != "" || rows[0].TicketStatus != domain.TicketStatusUnknown {
		t.Fatalf("legacy row = %+v err=%v, want turn_id empty and ticket_status unknown", rows, err)
	}
	// The index exists on the migrated database too: the backstop must hold there.
	if _, err := s.AddComputeEvent(context.Background(), anthropicTurn("s1", "t1", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO compute_events(project_id,id,model,provider,at,session,agent,source,conservation,at_seq,turn_id)
		VALUES('project-aira','CE-dup','m','anthropic','2026-10-09T00:00:00Z','s1','','claude-mod','unevaluated',99,'t1')`); err == nil {
		t.Fatal("migrated database lacks the unique turn index")
	}
}

// ---- ticket association (opt-in --resolve-ticket) ----

func resolveInput(session, turn string) domain.ComputeEventInput {
	in := anthropicTurn(session, turn, 3, 4)
	in.ResolveTicket = true
	return in
}

func TestResolveTicketFromLiveLease(t *testing.T) {
	ctx := context.Background()
	t.Run("exactly one live lease", func(t *testing.T) {
		s, _, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "work")
		if _, err := s.Claim(ctx, ticket.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil {
			t.Fatal(err)
		}
		if got.Event.TicketID != ticket.ID || got.Event.TicketStatus != domain.TicketStatusLeaseHeld {
			t.Fatalf("event ticket=%q status=%q, want %s lease-held", got.Event.TicketID, got.Event.TicketStatus, ticket.ID)
		}
		rows, _ := s.ListComputeEvents("")
		if rows[0].TicketID != ticket.ID || rows[0].TicketStatus != domain.TicketStatusLeaseHeld {
			t.Fatalf("stored row = %+v", rows[0])
		}
	})
	t.Run("no lease", func(t *testing.T) {
		s, _, _ := turnStore(t, 0, 0)
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusNone {
			t.Fatalf("got=%+v err=%v, want none", got.Event, err)
		}
	})
	t.Run("two live leases is unevaluated not a pick", func(t *testing.T) {
		s, _, _ := turnStore(t, 0, 0)
		a, b := m3Ticket(t, s, "a"), m3Ticket(t, s, "b")
		for _, id := range []string{a.ID, b.ID} {
			if _, err := s.Claim(ctx, id, false, "alice"); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusUnevaluated {
			t.Fatalf("got=%+v err=%v, want unevaluated with no ticket", got.Event, err)
		}
	})
	t.Run("expired held lease does not count", func(t *testing.T) {
		s, clock, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "work")
		if _, err := s.Claim(ctx, ticket.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		clock.mono = 100 + 900 // ttl elapsed; the row is still state='held' (unreaped)
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusNone {
			t.Fatalf("expired lease attributed: %+v err=%v", got.Event, err)
		}
	})
	t.Run("lease from a prior boot does not count", func(t *testing.T) {
		s, clock, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "work")
		if _, err := s.Claim(ctx, ticket.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		clock.boot = "boot-b"
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusNone {
			t.Fatalf("prior-boot lease attributed: %+v err=%v", got.Event, err)
		}
	})
	t.Run("expired plus one live is exactly one", func(t *testing.T) {
		s, clock, _ := turnStore(t, 0, 0)
		old, live := m3Ticket(t, s, "old"), m3Ticket(t, s, "live")
		if _, err := s.Claim(ctx, old.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		clock.mono = 600
		if _, err := s.Claim(ctx, live.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		clock.mono = 100 + 900 // old (hb at 100) expired, live (hb at 600) still live
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != live.ID || got.Event.TicketStatus != domain.TicketStatusLeaseHeld {
			t.Fatalf("got=%+v err=%v, want %s lease-held", got.Event, err, live.ID)
		}
	})
	t.Run("released lease", func(t *testing.T) {
		s, _, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "work")
		claim, err := s.Claim(ctx, ticket.ID, false, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Release(ctx, ticket.ID, claim.Token); err != nil {
			t.Fatal(err)
		}
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusNone {
			t.Fatalf("released lease attributed: %+v err=%v", got.Event, err)
		}
	})
	t.Run("binding only is not used", func(t *testing.T) {
		s, _, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "work")
		if _, err := s.RegisterWorktreeBinding(ctx, domain.WorktreeBindingInput{TicketID: ticket.ID}); err != nil {
			t.Fatal(err)
		}
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusNone {
			t.Fatalf("binding attributed spend: %+v err=%v", got.Event, err)
		}
	})
	t.Run("another worktrees lease does not count", func(t *testing.T) {
		s, clock, base := turnStore(t, 0, 0)
		other, err := Open(ctx, Options{
			Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
			RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "other-state"),
			ProjectID: s.projectID, WorktreeID: "worktree-b", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close() })
		ticket := m3Ticket(t, other, "elsewhere")
		if _, err := other.Claim(ctx, ticket.ID, false, "bob"); err != nil {
			t.Fatal(err)
		}
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusNone {
			t.Fatalf("foreign worktree lease attributed: %+v err=%v", got.Event, err)
		}
	})
	t.Run("clock unavailable is unevaluated and the row is kept", func(t *testing.T) {
		s, clock, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "work")
		if _, err := s.Claim(ctx, ticket.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		clock.err = fmt.Errorf("%w: test", ErrClockUnavailable)
		got, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusUnevaluated {
			t.Fatalf("got=%+v err=%v, want unevaluated, row kept", got.Event, err)
		}
	})
	t.Run("explicit ticket is declared and never resolved", func(t *testing.T) {
		s, _, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "leased")
		if _, err := s.Claim(ctx, ticket.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		in := resolveInput("s1", "t1")
		in.TicketID = "BL-9"
		got, err := s.AddComputeEvent(ctx, in)
		if err != nil || got.Event.TicketID != "BL-9" || got.Event.TicketStatus != domain.TicketStatusDeclared {
			t.Fatalf("got=%+v err=%v, want BL-9 declared", got.Event, err)
		}
	})
	t.Run("without the flag nothing is resolved and status is unknown", func(t *testing.T) {
		s, _, _ := turnStore(t, 0, 0)
		ticket := m3Ticket(t, s, "leased")
		if _, err := s.Claim(ctx, ticket.ID, false, "alice"); err != nil {
			t.Fatal(err)
		}
		in := anthropicTurn("s1", "t1", 3, 4) // ResolveTicket false
		got, err := s.AddComputeEvent(ctx, in)
		if err != nil || got.Event.TicketID != "" || got.Event.TicketStatus != domain.TicketStatusUnknown {
			t.Fatalf("existing producer changed: %+v err=%v", got.Event, err)
		}
		rows, _ := s.ListComputeEvents("")
		if rows[0].TicketStatus != domain.TicketStatusUnknown {
			t.Fatalf("stored status = %q, want unknown", rows[0].TicketStatus)
		}
	})
}

// A duplicate delivery must not re-resolve: the first stamp stands even if the
// lease changed between the original and the retry.
func TestResolveTicketDuplicateKeepsOriginalStamp(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	ctx := context.Background()
	ticket := m3Ticket(t, s, "work")
	claim, err := s.Claim(ctx, ticket.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release(ctx, ticket.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	dup, err := s.AddComputeEvent(ctx, resolveInput("s1", "t1"))
	if err != nil || !dup.Duplicate || dup.Event.TicketID != ticket.ID || dup.Event.TicketStatus != domain.TicketStatusLeaseHeld {
		t.Fatalf("duplicate = %+v err=%v, want the original lease-held stamp", dup, err)
	}
}

// ---- retention partitions ----

func TestComputeRetentionPartitionsModRowsFromOthers(t *testing.T) {
	s, _, _ := turnStore(t, 5, 0)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		in := computeInput(domain.RawUsage{})
		if _, err := s.AddComputeEvent(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		if _, err := s.AddComputeEvent(ctx, anthropicTurn("s1", fmt.Sprintf("t%d", i), 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	var mod, other int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM compute_events WHERE project_id=? AND source='claude-mod'`, s.projectID).Scan(&mod); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM compute_events WHERE project_id=? AND source<>'claude-mod'`, s.projectID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other != 3 {
		t.Fatalf("claude-mod volume evicted other rows: other=%d, want 3", other)
	}
	if mod != 5 {
		t.Fatalf("mod pool = %d, want the cap 5", mod)
	}
	// And the oldest MOD rows are the ones that went (t7..t11 remain).
	rows, _ := s.ListComputeEvents("")
	var turns []string
	for _, r := range rows {
		if r.Source == "claude-mod" {
			turns = append(turns, r.TurnID)
		}
	}
	if strings.Join(turns, ",") != "t11,t10,t9,t8,t7" {
		t.Fatalf("surviving mod turns = %v", turns)
	}
}

func TestComputeRetentionOtherPoolIsBoundedAcrossDistinctSources(t *testing.T) {
	s, _, _ := turnStore(t, 4, 0)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		in := computeInput(domain.RawUsage{})
		in.Source = fmt.Sprintf("src-%d", i)
		if _, err := s.AddComputeEvent(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM compute_events WHERE project_id=?`, s.projectID).Scan(&n)
	if n != 4 {
		t.Fatalf("distinct --source values created unbounded pools: %d rows, want 4", n)
	}
	// And mod rows do not evict them either.
	for i := 0; i < 6; i++ {
		if _, err := s.AddComputeEvent(ctx, anthropicTurn("s1", fmt.Sprintf("t%d", i), 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM compute_events WHERE project_id=? AND source<>'claude-mod'`, s.projectID).Scan(&n)
	if n != 4 {
		t.Fatalf("other pool = %d after mod volume, want 4", n)
	}
}

func TestComputeRetentionAgeCutoffAppliesToBothPartitions(t *testing.T) {
	for _, inserter := range []string{"claude-mod", "manual"} {
		t.Run("insert from "+inserter, func(t *testing.T) {
			s, _, _ := turnStore(t, 100, 30)
			ctx := context.Background()
			// Aged rows are planted raw (an insert would evict them at once).
			for i, src := range []string{"claude-mod", "manual", "other-src"} {
				if _, err := s.db.Exec(`INSERT INTO compute_events(project_id,id,model,provider,at,source,conservation,at_seq) VALUES(?,?,?,?,?,?,?,?)`,
					s.projectID, fmt.Sprintf("CE-old%d", i), "m", "anthropic", "2020-01-01T00:00:00Z", src, "unevaluated", 1000+i); err != nil {
					t.Fatal(err)
				}
			}
			var in domain.ComputeEventInput
			if inserter == "claude-mod" {
				in = anthropicTurn("s1", "fresh", 1, 1)
			} else {
				in = computeInput(domain.RawUsage{})
			}
			added, err := s.AddComputeEvent(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if added.EvictedCount != 3 || added.Remaining != 1 {
				t.Fatalf("evicted=%d remaining=%d, want all 3 aged rows gone across both pools", added.EvictedCount, added.Remaining)
			}
		})
	}
}

// ---- per-session summary ----

func TestSpendBySessionSumsPerSessionTicketStatusNullAware(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	ctx := context.Background()
	ticket := m3Ticket(t, s, "work")
	claim, err := s.Claim(ctx, ticket.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// s1: two turns under the lease.
	for i, out := range []int64{5, 7} {
		if _, err := s.AddComputeEvent(ctx, resolveInput("s1", fmt.Sprintf("a%d", i))); err != nil {
			t.Fatal(err)
		}
		_ = out
	}
	// s1 again after release: a different (empty ticket, none) group.
	if _, err := s.Release(ctx, ticket.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComputeEvent(ctx, resolveInput("s1", "b0")); err != nil {
		t.Fatal(err)
	}
	// s1 again, unflagged: same session and same empty ticket as the none row above,
	// but unknown must not merge with none.
	if _, err := s.AddComputeEvent(ctx, anthropicTurn("s1", "u0", 2, 2)); err != nil {
		t.Fatal(err)
	}
	// s2: unflagged (unknown), and one turn lacking the output counter entirely.
	if _, err := s.AddComputeEvent(ctx, anthropicTurn("s2", "c0", 10, 1)); err != nil {
		t.Fatal(err)
	}
	noOut := domain.ComputeEventInput{Model: "claude-x", Provider: "anthropic", Source: "claude-mod", Session: "s3", TurnID: "d0",
		Raw: domain.RawUsage{InputTokens: computeI64(4)}}
	if _, err := s.AddComputeEvent(ctx, noOut); err != nil {
		t.Fatal(err)
	}

	got, err := s.SpendBySession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ session, ticket, status string }
	byKey := map[key]ComputeSessionSummary{}
	for _, row := range got {
		byKey[key{row.Session, row.TicketID, row.TicketStatus}] = row
	}
	if len(got) != 5 {
		t.Fatalf("groups = %d (%+v), want 5", len(got), got)
	}
	held := byKey[key{"s1", ticket.ID, domain.TicketStatusLeaseHeld}]
	if held.Turns != 2 || held.FreshInput == nil || *held.FreshInput != 6 || held.Output == nil || *held.Output != 8 || held.CacheRead == nil || *held.CacheRead != 2 || held.CacheWrite == nil || *held.CacheWrite != 4 {
		t.Fatalf("s1 lease-held group = %+v", held)
	}
	none := byKey[key{"s1", "", domain.TicketStatusNone}]
	if none.Turns != 1 {
		t.Fatalf("s1 none group = %+v (none/unknown/lease-held must not merge)", none)
	}
	s1unknown := byKey[key{"s1", "", domain.TicketStatusUnknown}]
	if s1unknown.Turns != 1 || s1unknown.FreshInput == nil || *s1unknown.FreshInput != 2 {
		t.Fatalf("s1 unknown group = %+v (unknown must not merge into none)", s1unknown)
	}
	unknown := byKey[key{"s2", "", domain.TicketStatusUnknown}]
	if unknown.Turns != 1 {
		t.Fatalf("s2 unknown group = %+v", unknown)
	}
	partial := byKey[key{"s3", "", domain.TicketStatusUnknown}]
	if partial.Turns != 1 || partial.FreshInput == nil || *partial.FreshInput != 4 {
		t.Fatalf("s3 group = %+v", partial)
	}
	if partial.Output != nil || partial.CacheRead != nil || partial.CacheWrite != nil {
		t.Fatalf("a bucket with no contributor must stay NULL, not 0: %+v", partial)
	}
}

func TestSpendBySessionHonoursSessionFilter(t *testing.T) {
	s, _, _ := turnStore(t, 0, 0)
	ctx := context.Background()
	for _, in := range []domain.ComputeEventInput{anthropicTurn("s1", "t1", 1, 1), anthropicTurn("s2", "t1", 2, 2)} {
		if _, err := s.AddComputeEvent(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SpendBySession(ctx, "session:s2")
	if err != nil || len(got) != 1 || got[0].Session != "s2" {
		t.Fatalf("filtered summary = %+v err=%v", got, err)
	}
	rows, err := s.ListComputeEvents("session:s2")
	if err != nil || len(rows) != 1 || rows[0].Session != "s2" {
		t.Fatalf("filtered list = %+v err=%v", rows, err)
	}
	phase, err := s.SpendByPhase(ctx, "session:s1")
	if err != nil || len(phase) != 1 || phase[0].Events != 1 {
		t.Fatalf("filtered phase summary = %+v err=%v", phase, err)
	}
}
