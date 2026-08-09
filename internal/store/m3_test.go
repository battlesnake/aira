package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aira/internal/domain"
)

type m3Clock struct {
	boot string
	mono uint64
	err  error
}

func (c *m3Clock) Now() (string, uint64, error) { return c.boot, c.mono, c.err }

func m3Store(t *testing.T) (*Store, *m3Clock, string) {
	t.Helper()
	base := t.TempDir()
	clock := &m3Clock{boot: "boot-a", mono: 100}
	s, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"),
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		LeaseStateDir: filepath.Join(base, "lease-state"), ProjectID: "project-aira", WorktreeID: "worktree-a",
		ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clock, base
}

func m3Ticket(t *testing.T, s *Store, title string) domain.Ticket {
	t.Helper()
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: title, Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestLeaseSumTypeLivenessAndCASPredicates(t *testing.T) {
	s, clock, base := m3Store(t)
	ticket := m3Ticket(t, s, "lease me")

	claim, err := s.Claim(context.Background(), ticket.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	held, ok := claim.Lease.Held()
	if !ok || !held.IsLive(clock.boot, clock.mono) || held.Actor != "alice" || held.Worktree != "worktree-a" {
		t.Fatalf("held sum type = %#v", claim.Lease)
	}
	if claim.Token == "" || claim.Token == "expired" {
		t.Fatalf("claim token = %q", claim.Token)
	}
	mode, err := os.Stat(filepath.Join(base, "lease-state", "leases", "project-aira", "worktree-a", ticket.ID+".token"))
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o, want 600", mode.Mode().Perm())
	}
	if _, err := s.Claim(context.Background(), ticket.ID, false, "bob"); ErrorCode(err) != "E_LEASE_HELD" {
		t.Fatalf("live claim error = %v", err)
	}
	if _, err := s.Claim(context.Background(), ticket.ID, true, "bob"); ErrorCode(err) != "E_LEASE_HELD" {
		t.Fatalf("live steal error = %v", err)
	}
	if _, err := s.Heartbeat(context.Background(), ticket.ID, "wrong"); ErrorCode(err) != "E_LEASE_TOKEN" {
		t.Fatalf("wrong heartbeat token = %v", err)
	}

	clock.mono = 100 + 900
	if _, err := s.Heartbeat(context.Background(), ticket.ID, claim.Token); ErrorCode(err) != "E_LEASE_EXPIRED" {
		t.Fatalf("expired heartbeat error = %v", err)
	}
	if _, err := s.Claim(context.Background(), ticket.ID, false, "bob"); ErrorCode(err) != "E_LEASE_EXPIRED" {
		t.Fatalf("expired non-steal claim error = %v", err)
	}
	stolen, err := s.Claim(context.Background(), ticket.ID, true, "bob")
	if err != nil {
		t.Fatal(err)
	}
	newHeld, _ := stolen.Lease.Held()
	if newHeld.Generation != held.Generation+1 || stolen.Token == claim.Token || newHeld.Actor != "bob" {
		t.Fatalf("steal generation/token = old=%#v new=%#v", held, newHeld)
	}
	clock.boot = "boot-b"
	if _, err := s.Claim(context.Background(), ticket.ID, false, "carol"); ErrorCode(err) != "E_LEASE_EXPIRED" {
		t.Fatalf("boot change should make the lease expired: error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(base, "lease-state", "leases", "project-aira", "worktree-a", ticket.ID+".token"), []byte(stolen.Token), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release(context.Background(), ticket.ID, "wrong"); ErrorCode(err) != "E_LEASE_TOKEN" {
		t.Fatalf("wrong release token = %v", err)
	}
}

func TestLeaseBootChangeExpiresAndClockUnavailableRefuses(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "reboot")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	clock.boot = "boot-rebooted"
	stolen, err := s.Claim(context.Background(), ticket.ID, true, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if stolen.Token == claim.Token {
		t.Fatal("boot-id change did not produce a new token")
	}

	clock.err = errors.New("no monotonic clock")
	if _, err := s.Heartbeat(context.Background(), ticket.ID, stolen.Token); ErrorCode(err) != "E_CLOCK_UNAVAILABLE" {
		t.Fatalf("clock unavailable = %v", err)
	}
	if _, err := s.Claim(context.Background(), ticket.ID, true, "carol"); ErrorCode(err) != "E_CLOCK_UNAVAILABLE" {
		t.Fatalf("claim without monotonic clock = %v", err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["lease-integrity"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "E_CLOCK_UNAVAILABLE") {
		t.Fatalf("clock-unavailable check = %#v", report)
	}
}

func TestLeaseClockAndTTLOverflowAreUnavailable(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "clock bounds")
	clock.mono = ^uint64(0)
	if _, err := s.Claim(context.Background(), ticket.ID, false, "owner"); ErrorCode(err) != "E_CLOCK_UNAVAILABLE" {
		t.Fatalf("overflowing monotonic sample error = %v", err)
	}
	clock.mono = 100
	s.leaseTTLNS = ^uint64(0)
	if _, err := s.Claim(context.Background(), ticket.ID, false, "owner"); ErrorCode(err) != "E_CLOCK_UNAVAILABLE" {
		t.Fatalf("overflowing lease TTL error = %v", err)
	}
}

func TestConcurrentExpiredClaimersHaveOneWinnerAndNextGeneration(t *testing.T) {
	s, clock, base := m3Store(t)
	ticket := m3Ticket(t, s, "concurrent expired lease")
	first, err := s.Claim(context.Background(), ticket.ID, false, "incumbent")
	if err != nil {
		t.Fatal(err)
	}
	firstHeld, ok := first.Lease.Held()
	if !ok {
		t.Fatal("initial claim was not held")
	}
	clock.mono = 1000

	open := func(worktree string) *Store {
		other, err := Open(context.Background(), Options{
			Root: base, CommonDir: filepath.Join(base, "common"),
			DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
			LeaseStateDir: filepath.Join(base, "lease-state"), ProjectID: "project-aira", WorktreeID: worktree,
			ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close() })
		return other
	}
	one, two := open("worktree-b"), open("worktree-c")
	type outcome struct {
		claim LeaseClaim
		err   error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, contender := range []*Store{one, two} {
		wg.Add(1)
		go func(contender *Store) {
			defer wg.Done()
			claim, err := contender.Claim(context.Background(), ticket.ID, true, contender.worktreeID)
			results <- outcome{claim: claim, err: err}
		}(contender)
	}
	wg.Wait()
	close(results)

	wins := 0
	winningGeneration := uint64(0)
	for result := range results {
		if result.err == nil {
			wins++
			held, ok := result.claim.Lease.Held()
			if !ok {
				t.Fatal("winning claim was not held")
			}
			winningGeneration = held.Generation
		} else if ErrorCode(result.err) != "E_LEASE_HELD" {
			t.Fatalf("losing claim error = %v", result.err)
		}
	}
	if wins != 1 || winningGeneration == firstHeld.Generation || winningGeneration != 2 {
		t.Fatalf("concurrent claim outcomes: wins=%d generation=%d first=%#v", wins, winningGeneration, first.Lease)
	}
}

func TestLeaseSchemaAndRowValidationRejectCorruption(t *testing.T) {
	s, _, _ := m3Store(t)
	_, err := s.db.Exec(`INSERT INTO leases(project_id, ticket_id, state, generation, holder_token_hash, boot_id,
	        last_heartbeat_mono_ns, ttl_ns, actor, worktree_id)
	        VALUES(?, ?, 'held', 0, '', '', -1, 0, '', '')`, s.projectID, "AIRA-999")
	if err == nil {
		t.Fatal("lease CHECK accepted illegal held row")
	}
	_, err = s.db.Exec(`INSERT INTO leases(project_id, ticket_id, state, generation)
        VALUES(?, ?, 'free', -1)`, s.projectID, "AIRA-998")
	if err == nil {
		t.Fatal("lease CHECK accepted negative free generation")
	}
	_, err = s.db.Exec(`INSERT INTO leases(project_id, ticket_id, state, generation, holder_token_hash, boot_id,
	        last_heartbeat_mono_ns, ttl_ns, actor, worktree_id)
	        VALUES(?, ?, 'held', 1, 'x', 'boot', 0, 1, 'actor', 'worktree')`, s.projectID, "AIRA-997")
	if err == nil {
		t.Fatal("lease CHECK accepted a nonempty malformed holder hash")
	}

	hash := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	valid := leaseRow{
		state: "held", generation: 1,
		holderTokenHash:     sql.NullString{String: hash, Valid: true},
		bootID:              sql.NullString{String: "boot", Valid: true},
		lastHeartbeatMonoNS: sql.NullInt64{Int64: 0, Valid: true},
		ttlNS:               sql.NullInt64{Int64: 1, Valid: true},
		actor:               sql.NullString{String: "actor", Valid: true},
		worktree:            sql.NullString{String: "worktree", Valid: true},
	}
	for name, mutate := range map[string]func(*leaseRow){
		"empty holder hash":  func(row *leaseRow) { row.holderTokenHash.String = "" },
		"empty boot":         func(row *leaseRow) { row.bootID.String = "" },
		"empty actor":        func(row *leaseRow) { row.actor.String = "" },
		"empty worktree":     func(row *leaseRow) { row.worktree.String = "" },
		"negative heartbeat": func(row *leaseRow) { row.lastHeartbeatMonoNS.Int64 = -1 },
		"non-positive ttl":   func(row *leaseRow) { row.ttlNS.Int64 = 0 },
		"zero generation":    func(row *leaseRow) { row.generation = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			row := valid
			mutate(&row)
			if _, err := leaseFromRow("AIRA-1", row); err == nil {
				t.Fatal("leaseFromRow accepted illegal lease row")
			}
		})
	}
}

func TestReadyFailsClosedWhenIndexedPrerequisiteOwnerIsMalformed(t *testing.T) {
	s, _, _ := m3Store(t)
	prerequisite := m3Ticket(t, s, "prerequisite")
	dependent := m3Ticket(t, s, "dependent")
	if _, err := s.Link(context.Background(), prerequisite.ID, domain.RelationBlocks, dependent.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", prerequisite.ID+".md"), []byte("malformed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Ready(dependent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Ready || rows[0].Verdict != "fail" || len(rows[0].Findings) == 0 {
		t.Fatalf("malformed prerequisite was treated as ready: %#v", rows)
	}
}

func TestRelationEndpointsMustShareProjectSlug(t *testing.T) {
	s, _, _ := m3Store(t)
	from := m3Ticket(t, s, "from")
	to := m3Ticket(t, s, "to")
	if _, err := s.Link(context.Background(), from.ID, domain.RelationBlocks, to.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.root, ".aira", "tickets", to.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	updated := rawM3Ticket(t, domain.Ticket{Schema: 1, ID: to.ID, Project: "other-project", Title: to.Title,
		Status: to.Status, Kind: to.Kind, Severity: to.Severity, Labels: []string{}, Relations: nil}, "body\n")
	if string(data) == string(updated) {
		t.Fatal("test did not change endpoint project")
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", to.ID+".md"), updated, 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Ready(from.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Ready || !hasFinding(rows[0].Findings, "E_CROSS_PROJECT_RELATION") {
		t.Fatalf("cross-project relation was accepted: %#v", rows)
	}
}

func TestRelationsDerivedIndexMaterialisesAndRebuilds(t *testing.T) {
	s, _, _ := m3Store(t)
	from := m3Ticket(t, s, "from")
	to := m3Ticket(t, s, "to")
	if _, err := s.Link(context.Background(), from.ID, domain.RelationBlocks, to.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM relations WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("relations index count = %d, want 1", count)
	}
	if err := os.Remove(filepath.Join(s.root, ".aira", "tickets", from.ID+".md")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM relations WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rebuilt relations index count = %d, want stale row deleted", count)
	}
}

func TestReadyUsesCanonicalRelationsAndReportsStaleIndex(t *testing.T) {
	s, _, _ := m3Store(t)
	prerequisite := m3Ticket(t, s, "prerequisite")
	dependent := m3Ticket(t, s, "dependent")
	if _, err := s.Link(context.Background(), prerequisite.ID, domain.RelationBlocks, dependent.ID); err != nil {
		t.Fatal(err)
	}
	withoutRelation := rawM3Ticket(t, domain.Ticket{Schema: 1, ID: prerequisite.ID, Project: prerequisite.Project,
		Title: prerequisite.Title, Status: prerequisite.Status, Kind: prerequisite.Kind, Severity: prerequisite.Severity,
		Labels: []string{}, Relations: nil}, "body\n")
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", prerequisite.ID+".md"), withoutRelation, 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Ready(dependent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Ready || len(rows[0].Blockers) != 0 || !hasFinding(rows[0].Findings, "E_RELATION_INDEX_DIVERGENCE") {
		t.Fatalf("stale relation index affected ready result: %#v", rows)
	}
	report, err := s.Check(context.Background())
	if err != nil || report.Dimensions["relation-integrity"] != "fail" || !hasFinding(report.Findings, "E_RELATION_INDEX_DIVERGENCE") {
		t.Fatalf("check did not report stale relation index: report=%#v err=%v", report, err)
	}
}

func TestReadyFindingAttributionUsesExactRelationEndpoints(t *testing.T) {
	s, _, _ := m3Store(t)
	short := m3Ticket(t, s, "short ID")
	target := m3Ticket(t, s, "relation target")
	long := domain.Ticket{Schema: 1, ID: "AIRA-10", Project: "aira", Title: "long ID", Status: domain.StatusPlanned,
		Kind: domain.KindFeature, Severity: domain.SeverityP2, Labels: []string{}, Relations: []domain.Relation{{
			Kind: domain.RelationBlocks, From: "AIRA-10", To: target.ID,
		}}}
	data := rawM3Ticket(t, long, "body\n")
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", long.ID+".md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Ready(short.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Ready || len(rows[0].Findings) != 0 {
		t.Fatalf("substring relation finding was attributed to %s: %#v", short.ID, rows)
	}
}

func TestCanonicalRelationOrderingAcrossPrefixes(t *testing.T) {
	s, _, _ := m3Store(t)
	if err := os.MkdirAll(filepath.Join(s.root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	low := domain.Ticket{Schema: 1, ID: "AIRA-10", Project: "aira", Title: "low", Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2, Labels: []string{}, Relations: []domain.Relation{}}
	high := domain.Ticket{Schema: 1, ID: "ZZ-2", Project: "aira", Title: "high", Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2, Labels: []string{}, Relations: []domain.Relation{}}
	for _, ticket := range []domain.Ticket{low, high} {
		data, err := domain.RenderTicket(ticket, "body\n")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", ticket.ID+".md"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Link(context.Background(), high.ID, domain.RelationBlocks, low.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.root, ".aira", "tickets", low.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"from":"ZZ-2"`) {
		t.Fatalf("cross-prefix relation was not stored on canonical owner: %s", data)
	}
}

func TestHeartbeatAndStealRaceHasOneStateTransition(t *testing.T) {
	s, heartbeatClock, base := m3Store(t)
	ticket := m3Ticket(t, s, "heartbeat race")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "owner")
	if err != nil {
		t.Fatal(err)
	}
	stealClock := &m3Clock{boot: heartbeatClock.boot, mono: 1000}
	stealer, err := Open(context.Background(), Options{Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "steal-state"), ProjectID: s.projectID, WorktreeID: "stealer", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: stealClock, LeaseTTLNS: 900})
	if err != nil {
		t.Fatal(err)
	}
	defer stealer.Close()
	heartbeatClock.mono = 1000
	start := make(chan struct{})
	type outcome struct {
		heartbeat bool
		steal     bool
		err       error
	}
	results := make(chan outcome, 2)
	go func() {
		<-start
		_, err := s.Heartbeat(context.Background(), ticket.ID, claim.Token)
		results <- outcome{heartbeat: err == nil, err: err}
	}()
	go func() {
		<-start
		_, err := stealer.Claim(context.Background(), ticket.ID, true, "stealer")
		results <- outcome{steal: err == nil, err: err}
	}()
	close(start)
	var got []outcome
	for i := 0; i < 2; i++ {
		got = append(got, <-results)
	}
	wins := 0
	for _, result := range got {
		if result.heartbeat || result.steal {
			wins++
		} else if ErrorCode(result.err) != "E_LEASE_EXPIRED" && ErrorCode(result.err) != "E_LEASE_HELD" && ErrorCode(result.err) != "E_LEASE_TOKEN" {
			t.Fatalf("unexpected race loser error: %v", result.err)
		}
	}
	if wins != 1 {
		t.Fatalf("heartbeat/steal race had %d successful transitions: %#v", wins, got)
	}
}

func TestReleaseAndStealRacePreservesWinningToken(t *testing.T) {
	s, releaseClock, base := m3Store(t)
	ticket := m3Ticket(t, s, "release token race")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "owner")
	if err != nil {
		t.Fatal(err)
	}
	stealClock := &m3Clock{boot: releaseClock.boot, mono: 1000}
	stealer, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "lease-state"),
		ProjectID: s.projectID, WorktreeID: s.worktreeID, ProjectSlug: "aira", Prefixes: []string{"AIRA"},
		Clock: stealClock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stealer.Close()
	type outcome struct {
		name  string
		err   error
		claim LeaseClaim
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := s.Release(context.Background(), ticket.ID, claim.Token)
		results <- outcome{name: "release", err: err}
	}()
	go func() {
		<-start
		got, err := stealer.Claim(context.Background(), ticket.ID, true, "stealer")
		results <- outcome{name: "steal", err: err, claim: got}
	}()
	close(start)
	var steal outcome
	for i := 0; i < 2; i++ {
		got := <-results
		if got.name == "steal" {
			steal = got
		} else if got.err != nil && ErrorCode(got.err) != "E_LEASE_TOKEN" && ErrorCode(got.err) != "E_LEASE_EXPIRED" {
			t.Fatalf("unexpected release race error: %v", got.err)
		}
	}
	if steal.err != nil {
		t.Fatalf("stealer lost the release race: %v", steal.err)
	}
	lease, err := s.GetLease(context.Background(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	winnerBytes, err := base64.RawURLEncoding.DecodeString(steal.claim.Token)
	if err != nil {
		t.Fatal(err)
	}
	winnerHash := sha256.Sum256(winnerBytes)
	held, ok := lease.Held()
	if !ok || (held.Generation != 2 && held.Generation != 3) || held.HolderTokenHash != winnerHash {
		t.Fatalf("final lease/token generation = %#v", lease)
	}
	if token, err := s.LeaseToken(ticket.ID); err != nil || token != steal.claim.Token {
		t.Fatalf("final token = %q err=%v want stealer token", token, err)
	}
}

func TestLeaseEventRebuildRecoversAfterJournalAppendFailure(t *testing.T) {
	s, _, base := m3Store(t)
	ticket := m3Ticket(t, s, "journal lease")
	badAudit := filepath.Join(base, "audit-file")
	if err := os.WriteFile(badAudit, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.auditDir = badAudit
	if _, err := s.Claim(context.Background(), ticket.ID, false, "owner"); err == nil {
		t.Fatal("claim unexpectedly succeeded with a broken journal path")
	}
	recovered, err := Open(context.Background(), Options{Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "recovered-state"), ProjectID: s.projectID, WorktreeID: "recovery", ProjectSlug: "aira", Prefixes: []string{"AIRA"}})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var journaled int
	if err := recovered.db.QueryRow(`SELECT journaled FROM events WHERE project_id=? AND verb='lease.claim'`, s.projectID).Scan(&journaled); err != nil {
		t.Fatal(err)
	}
	if journaled != 1 {
		t.Fatalf("recovered lease event journaled=%d", journaled)
	}
	data, err := os.ReadFile(filepath.Join(base, "common", "aira", "journal.jsonl"))
	if err != nil || !strings.Contains(string(data), `"verb":"lease.claim"`) {
		t.Fatalf("recovered journal = %q, err=%v", data, err)
	}
}

func TestLeaseHeartbeatIsDBOnlyAndReleaseFreesSumType(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "heartbeat")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	clock.mono = 200
	if _, err := s.Heartbeat(context.Background(), ticket.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	var eventCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=?`, s.projectID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 { // ticket.create + lease.claim; heartbeat is DB-only.
		t.Fatalf("heartbeat emitted an event: count=%d", eventCount)
	}
	clock.mono = 300
	event, err := s.Release(context.Background(), ticket.ID, claim.Token)
	if err != nil || event.Seq == 0 {
		t.Fatalf("release = event=%#v err=%v", event, err)
	}
	lease, err := s.GetLease(context.Background(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	free, ok := lease.Free()
	if !ok || free.Generation != 2 {
		t.Fatalf("released lease = %#v", lease)
	}
	if _, err := s.LeaseToken(ticket.ID); ErrorCode(err) != "E_LEASE_TOKEN" {
		t.Fatalf("release did not remove local token: %v", err)
	}
}

func TestReleaseWithEmptyTokenReturnsTokenError(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "empty release token")
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	clock.mono = 1000
	if _, err := s.Release(context.Background(), ticket.ID, ""); ErrorCode(err) != "E_LEASE_TOKEN" {
		t.Fatalf("empty release token error = %v", err)
	}
}

func TestCanonicalRelationsDerivedInverseAndDuplicate(t *testing.T) {
	s, _, _ := m3Store(t)
	first := m3Ticket(t, s, "first")
	second := m3Ticket(t, s, "second")

	event, err := s.Link(context.Background(), second.ID, domain.RelationBlocks, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if event.Seq == 0 {
		t.Fatal("relation add did not emit event")
	}
	canonical, err := os.ReadFile(filepath.Join(s.root, ".aira", "tickets", first.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"from":"`+second.ID+`"`) {
		t.Fatalf("canonical file does not store relation: %s", canonical)
	}
	other, err := os.ReadFile(filepath.Join(s.root, ".aira", "tickets", second.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(other), `"kind":"blocks"`) {
		t.Fatal("inverse was stored on the other ticket")
	}
	views, err := s.Relations(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Kind != domain.RelationBlockedBy || views[0].From != first.ID || views[0].To != second.ID {
		t.Fatalf("derived inverse = %#v", views)
	}
	if _, err := s.Link(context.Background(), second.ID, domain.RelationBlocks, first.ID); ErrorCode(err) != "E_RELATION_EXISTS" {
		t.Fatalf("duplicate relation = %v", err)
	}
	if _, err := s.Link(context.Background(), first.ID, domain.RelationBlocks, "AIRA-999"); ErrorCode(err) != "E_RELATION_TARGET_MISSING" {
		t.Fatalf("missing relation target = %v", err)
	}
	if _, err := s.Unlink(context.Background(), second.ID, domain.RelationBlocks, first.ID); err != nil {
		t.Fatal(err)
	}
	views, err = s.Relations(first.ID)
	if err != nil || len(views) != 0 {
		t.Fatalf("relation removal = views=%#v err=%v", views, err)
	}
}

func TestRelationCheckFindsMissingTargetAndWrongCanonicalSide(t *testing.T) {
	s, _, _ := m3Store(t)
	first := m3Ticket(t, s, "first")
	second := m3Ticket(t, s, "second")
	path := filepath.Join(s.root, ".aira", "tickets", second.ID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := rawM3Ticket(t, domain.Ticket{Schema: 1, ID: second.ID, Project: "aira", Title: second.Title, Status: second.Status, Kind: second.Kind, Severity: second.Severity, Relations: []domain.Relation{{Kind: domain.RelationBlocks, From: second.ID, To: first.ID}}, Labels: []string{}}, "body\n")
	if string(data) == string(updated) {
		t.Fatal("test did not create a wrong-side relation")
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["relation-integrity"] != "fail" || !hasFinding(report.Findings, "E_RELATION_INVALID") {
		t.Fatalf("wrong-side relation report = %#v", report)
	}
	missing := rawM3Ticket(t, domain.Ticket{Schema: 1, ID: second.ID, Project: "aira", Title: second.Title, Status: second.Status, Kind: domain.KindFeature, Severity: domain.SeverityP2, Relations: []domain.Relation{{Kind: domain.RelationBlocks, From: second.ID, To: "AIRA-999"}}, Labels: []string{}}, "body\n")
	if err := os.WriteFile(path, missing, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = s.Check(context.Background())
	if err != nil || report.Dimensions["relation-integrity"] != "fail" || !hasFinding(report.Findings, "E_RELATION_TARGET_MISSING") {
		t.Fatalf("missing-target relation report = %#v err=%v", report, err)
	}
}

func TestRelationCheckFlagsDuplicateAndUnsortedRawRelations(t *testing.T) {
	s, _, _ := m3Store(t)
	ticket := m3Ticket(t, s, "malformed relations")
	for name, relations := range map[string][]domain.Relation{
		"duplicate": {
			{Kind: domain.RelationBlocks, From: ticket.ID, To: "AIRA-2"},
			{Kind: domain.RelationBlocks, From: ticket.ID, To: "AIRA-2"},
		},
		"unsorted": {
			{Kind: domain.RelationRelates, From: ticket.ID, To: "AIRA-3"},
			{Kind: domain.RelationBlocks, From: ticket.ID, To: "AIRA-2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := rawM3Ticket(t, domain.Ticket{Schema: 1, ID: ticket.ID, Project: "aira", Title: ticket.Title, Status: ticket.Status, Kind: ticket.Kind, Severity: ticket.Severity, Relations: relations, Labels: []string{}}, "body\n")
			if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", ticket.ID+".md"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := s.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Dimensions["relation-integrity"] != "fail" || !hasFinding(report.Findings, "E_RELATION_INVALID") {
				t.Fatalf("%s relation report = %#v", name, report)
			}
		})
	}
}

func TestReadyQueueIsDerivedAndExcludesBlockedTickets(t *testing.T) {
	s, _, _ := m3Store(t)
	prerequisite := m3Ticket(t, s, "prerequisite")
	dependent := m3Ticket(t, s, "dependent")
	if _, err := s.Link(context.Background(), prerequisite.ID, domain.RelationBlocks, dependent.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := s.Ready("")
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].Ticket.Ticket.ID != prerequisite.ID {
		t.Fatalf("blocked ready set = %#v", ready)
	}
	for _, status := range []domain.Status{domain.StatusInProgress, domain.StatusInReview, domain.StatusDone} {
		if _, err := s.MoveTicket(context.Background(), prerequisite.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	ready, err = s.Ready("")
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].Ticket.Ticket.ID != dependent.ID {
		t.Fatalf("dependent was not derived ready = %#v", ready)
	}
	dependentReady, err := s.Ready(dependent.ID)
	if err != nil || len(dependentReady) != 1 || !dependentReady[0].Ready {
		t.Fatalf("dependent readiness = %#v err=%v", dependentReady, err)
	}
}

func TestReadySurfacesMissingPrerequisiteAndMalformedCandidate(t *testing.T) {
	s, _, _ := m3Store(t)
	dependent := m3Ticket(t, s, "missing prerequisite")
	missingRelation := rawM3Ticket(t, domain.Ticket{Schema: 1, ID: dependent.ID, Project: "aira", Title: dependent.Title, Status: dependent.Status, Kind: dependent.Kind, Severity: dependent.Severity, Relations: []domain.Relation{{Kind: domain.RelationBlocks, From: "AIRA-999", To: dependent.ID}}, Labels: []string{}}, "body\n")
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", dependent.ID+".md"), missingRelation, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", "AIRA-99.md"), []byte("not a ticket\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Ready("")
	if err != nil {
		t.Fatalf("ready with integrity findings: %v", err)
	}
	var missing, malformed bool
	for _, row := range rows {
		if row.Ticket.Ticket.ID == dependent.ID {
			missing = hasFinding(row.Findings, "E_RELATION_TARGET_MISSING") && !row.Ready && row.Verdict == "fail"
		}
		if strings.HasSuffix(row.Ticket.Path, "AIRA-99.md") {
			malformed = hasFinding(row.Findings, "E_CONFIG_INVALID") && !row.Ready && row.Verdict == "fail"
		}
	}
	if !missing || !malformed {
		t.Fatalf("ready integrity rows = %#v", rows)
	}
}

func TestClaimTokenSaveFailureDoesNotCommitLease(t *testing.T) {
	s, _, base := m3Store(t)
	ticket := m3Ticket(t, s, "token save failure")
	statePath := filepath.Join(base, "token-state-file")
	if err := os.WriteFile(statePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.leaseStateDir = statePath
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err == nil {
		t.Fatal("claim unexpectedly succeeded with an unwritable token directory")
	}
	lease, err := s.GetLease(context.Background(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := lease.Held(); held {
		t.Fatalf("token-save failure stranded committed lease: %#v", lease)
	}
}

func TestFailedClaimLeavesIncumbentTokenUntouched(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "incumbent token")
	incumbent, err := s.Claim(context.Background(), ticket.ID, false, "incumbent")
	if err != nil {
		t.Fatal(err)
	}
	s.beforeLeaseCommit = func() error { return errors.New("forced claim rollback") }
	clock.mono = 1000
	if _, err := s.Claim(context.Background(), ticket.ID, true, "contender"); err == nil {
		t.Fatal("forced failed claim unexpectedly succeeded")
	}
	got, err := s.LeaseToken(ticket.ID)
	if err != nil || got != incumbent.Token {
		t.Fatalf("failed claim clobbered incumbent token: got=%q err=%v want=%q", got, err, incumbent.Token)
	}
}

func rawM3Ticket(t *testing.T, ticket domain.Ticket, body string) []byte {
	t.Helper()
	ticket.Schema = 1
	data, err := json.Marshal(ticket)
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte("---\n"), data...), []byte("\n---\n"+body)...)
}

func hasFinding(findings []CheckFinding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
