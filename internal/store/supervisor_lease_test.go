package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func supervisorToken(seed byte) (string, string) {
	clear := make([]byte, 32)
	for i := range clear {
		clear[i] = seed
	}
	token := base64.RawURLEncoding.EncodeToString(clear)
	hash := sha256.Sum256(clear)
	return token, base64.RawURLEncoding.EncodeToString(hash[:])
}

func TestSupervisorLeaseClaimRenewReleaseAndAudit(t *testing.T) {
	s, clock, _ := m3Store(t)
	token, tokenHash := supervisorToken(1)
	generation, outcome, err := s.ClaimSupervisorLease(context.Background(), "RUN-1", 101, 202, clock.boot, tokenHash, 900)
	if err != nil || generation != 1 || outcome != SupervisorLeaseClaimed {
		t.Fatalf("claim generation=%d outcome=%q err=%v", generation, outcome, err)
	}
	clock.mono = 150
	if got, err := s.RenewSupervisorLease(context.Background(), "RUN-1", generation, token); err != nil || got != SupervisorLeaseOK {
		t.Fatalf("renew outcome=%q err=%v", got, err)
	}
	lease, err := s.GetSupervisorLease(context.Background(), "RUN-1")
	if err != nil || lease.Generation != 1 || lease.LastHeartbeatMonoNS != 150 || !lease.IsLive(clock.boot, clock.mono) {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	if got, err := s.ReleaseSupervisorLease(context.Background(), "RUN-1", generation, token); err != nil || got != SupervisorLeaseOK {
		t.Fatalf("release outcome=%q err=%v", got, err)
	}
	lease, err = s.GetSupervisorLease(context.Background(), "RUN-1")
	if err != nil || lease.State != SupervisorLeaseLapsed || lease.Generation != 2 {
		t.Fatalf("released lease=%+v err=%v", lease, err)
	}
	var claims, releases, outbox int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE project_id=? AND verb='lease.claim' AND target='RUN-1'`, s.projectID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE project_id=? AND verb='lease.release' AND target='RUN-1'`, s.projectID).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id=? AND verb IN ('lease.claim','lease.release')`, s.projectID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || releases != 1 || outbox != 2 {
		t.Fatalf("audit claims=%d releases=%d outbox=%d", claims, releases, outbox)
	}
}

func TestSupervisorLeaseFencingTokenAndExpiredRenewDiscriminators(t *testing.T) {
	s, clock, _ := m3Store(t)
	firstToken, firstHash := supervisorToken(2)
	wrongToken, _ := supervisorToken(3)
	generation, _, err := s.ClaimSupervisorLease(context.Background(), "RUN-2", 11, 22, clock.boot, firstHash, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.RenewSupervisorLease(context.Background(), "RUN-2", generation, wrongToken); err != nil || got != SupervisorLeaseToken {
		t.Fatalf("wrong-token renew outcome=%q err=%v", got, err)
	}
	if got, err := s.ReleaseSupervisorLease(context.Background(), "RUN-2", generation, wrongToken); err != nil || got != SupervisorLeaseToken {
		t.Fatalf("wrong-token release outcome=%q err=%v", got, err)
	}
	clock.mono = 200
	before, err := s.GetSupervisorLease(context.Background(), "RUN-2")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.RenewSupervisorLease(context.Background(), "RUN-2", generation, firstToken); err != nil || got != SupervisorLeaseExpired {
		t.Fatalf("expired renew outcome=%q err=%v", got, err)
	}
	after, err := s.GetSupervisorLease(context.Background(), "RUN-2")
	if err != nil {
		t.Fatal(err)
	}
	if after.LastHeartbeatMonoNS != before.LastHeartbeatMonoNS || after.Generation != before.Generation || after.State != SupervisorLeaseHeld {
		t.Fatalf("expired renew revived/mutated lease: before=%+v after=%+v", before, after)
	}
	secondToken, secondHash := supervisorToken(4)
	secondGeneration, _, err := s.ClaimSupervisorLease(context.Background(), "RUN-2", 33, 44, clock.boot, secondHash, 100)
	if err != nil || secondGeneration != generation+1 {
		t.Fatalf("reclaim generation=%d err=%v", secondGeneration, err)
	}
	if got, err := s.RenewSupervisorLease(context.Background(), "RUN-2", generation, firstToken); err != nil || got != SupervisorLeaseFenced {
		t.Fatalf("stale renew outcome=%q err=%v", got, err)
	}
	if got, err := s.ReleaseSupervisorLease(context.Background(), "RUN-2", generation, firstToken); err != nil || got != SupervisorLeaseFenced {
		t.Fatalf("stale release outcome=%q err=%v", got, err)
	}
	current, err := s.GetSupervisorLease(context.Background(), "RUN-2")
	if err != nil || current.Generation != secondGeneration || current.HolderTokenHash != secondHash || current.State != SupervisorLeaseHeld {
		t.Fatalf("stale holder mutated current lease: %+v err=%v", current, err)
	}
	if got, err := s.ReleaseSupervisorLease(context.Background(), "RUN-2", secondGeneration, secondToken); err != nil || got != SupervisorLeaseOK {
		t.Fatalf("current release outcome=%q err=%v", got, err)
	}
}

func TestSupervisorLeaseIdempotentClaimAndConflict(t *testing.T) {
	s, clock, _ := m3Store(t)
	_, tokenHash := supervisorToken(5)
	first, outcome, err := s.ClaimSupervisorLease(context.Background(), "RUN-3", 51, 52, clock.boot, tokenHash, 900)
	if err != nil || outcome != SupervisorLeaseClaimed {
		t.Fatalf("first claim=%d %q %v", first, outcome, err)
	}
	second, outcome, err := s.ClaimSupervisorLease(context.Background(), "RUN-3", 51, 52, clock.boot, tokenHash, 900)
	if err != nil || second != first || outcome != SupervisorLeaseExisting {
		t.Fatalf("idempotent claim=%d %q %v", second, outcome, err)
	}
	_, otherHash := supervisorToken(6)
	if _, _, err := s.ClaimSupervisorLease(context.Background(), "RUN-3", 51, 52, clock.boot, otherHash, 900); !errors.Is(err, ErrSupervisorLeaseConflict) || ErrorCode(err) != "E_RUN_SUPERVISOR_LEASE_CONFLICT" {
		t.Fatalf("different token conflict=%v", err)
	}
	if _, _, err := s.ClaimSupervisorLease(context.Background(), "RUN-3", 99, 52, clock.boot, tokenHash, 900); !errors.Is(err, ErrSupervisorLeaseConflict) {
		t.Fatalf("different identity conflict=%v", err)
	}
	var claims int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE project_id=? AND verb='lease.claim' AND target='RUN-3'`, s.projectID).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claim events=%d err=%v", claims, err)
	}
}

func TestSupervisorLeaseReapLapsesAndSkipsRevived(t *testing.T) {
	s, clock, base := m3Store(t)
	token, hash := supervisorToken(7)
	if _, _, err := s.ClaimSupervisorLease(context.Background(), "RUN-4", 71, 72, clock.boot, hash, 100); err != nil {
		t.Fatal(err)
	}
	clock.mono = 200
	reviverClock := &m3Clock{boot: clock.boot, mono: 150}
	reviver, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: s.projectID, WorktreeID: "reviver",
		ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: reviverClock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reviver.Close() })
	s.beforeSupervisorReapCAS = func(runID string) {
		s.beforeSupervisorReapCAS = nil
		if got, err := reviver.RenewSupervisorLease(context.Background(), runID, 1, token); err != nil || got != SupervisorLeaseOK {
			t.Fatalf("racing renew=%q err=%v", got, err)
		}
	}
	if count, err := s.ReapExpiredSupervisorLeases(context.Background()); err != nil || count != 0 {
		t.Fatalf("race reap count=%d err=%v", count, err)
	}
	clock.mono = 300
	if count, err := s.ReapExpiredSupervisorLeases(context.Background()); err != nil || count != 1 {
		t.Fatalf("reap count=%d err=%v", count, err)
	}
	lease, err := s.GetSupervisorLease(context.Background(), "RUN-4")
	if err != nil || lease.State != SupervisorLeaseLapsed || lease.Generation != 2 {
		t.Fatalf("reaped lease=%+v err=%v", lease, err)
	}
	var actor, verb string
	var outbox int
	if err := s.db.QueryRow(`SELECT actor, verb FROM events WHERE project_id=? AND target='RUN-4' AND verb='lease.lapse'`, s.projectID).Scan(&actor, &verb); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id=? AND verb='lease.lapse'`, s.projectID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if actor != "aira-daemon" || verb != "lease.lapse" || outbox != 1 {
		t.Fatalf("lapse actor=%q verb=%q outbox=%d", actor, verb, outbox)
	}
}

// TestSupervisorLeaseReapSamplesClockUnderLock proves the reaping CAS re-samples
// the clock AFTER BEGIN IMMEDIATE holds the writer lock (Sol build r1 P1). The
// lock-aware clock records a sample taken while the reap lock is held; the old
// pre-lock-only implementation never sampled under the lock, so it fails this.
func TestSupervisorLeaseReapSamplesClockUnderLock(t *testing.T) {
	s, clock, _ := m3Store(t)
	_, hash := supervisorToken(11)
	if _, _, err := s.ClaimSupervisorLease(context.Background(), "RUN-7", 71, 72, clock.boot, hash, 100); err != nil {
		t.Fatal(err)
	}
	var lockAcquired, sampledAfter atomic.Bool
	// Both the pre-lock sweep sample (500) and the under-lock CAS sample (600) are
	// far past expiry, so the reap fires AND reaches the under-lock re-sample.
	s.clock = &lockAwareClock{boot: clock.boot, staleMono: 500, liveMono: 600, lockAcquired: &lockAcquired, sampledAfter: &sampledAfter}
	s.afterSupervisorReapBegin = func() { lockAcquired.Store(true) }
	count, err := s.ReapExpiredSupervisorLeases(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("reap count=%d err=%v", count, err)
	}
	if !sampledAfter.Load() {
		t.Fatal("reaping CAS did not sample the clock under the writer lock")
	}
}

func TestSupervisorLeaseConcurrentClaimHasOneWinner(t *testing.T) {
	s, clock, base := m3Store(t)
	open := func(worktree string) *Store {
		other, err := Open(context.Background(), Options{
			Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
			RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: s.projectID, WorktreeID: worktree,
			ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close() })
		return other
	}
	a, b := open("a"), open("b")
	_, hashA := supervisorToken(8)
	_, hashB := supervisorToken(9)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, contender := range []struct {
		s *Store
		p int
		h string
	}{{a, 81, hashA}, {b, 91, hashB}} {
		wg.Add(1)
		go func(c struct {
			s *Store
			p int
			h string
		}) {
			defer wg.Done()
			<-start
			_, _, err := c.s.ClaimSupervisorLease(context.Background(), "RUN-5", c.p, uint64(c.p), clock.boot, c.h, 900)
			results <- err
		}(contender)
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrSupervisorLeaseConflict):
			conflicts++
		default:
			t.Fatalf("unexpected contender error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestSupervisorLeaseClockSampledAfterWriterLock(t *testing.T) {
	s, _, _ := m3Store(t)
	var lockAcquired, sampledBefore atomic.Bool
	s.clock = &lockAwareClock{boot: "boot-a", staleMono: 1, liveMono: 100, lockAcquired: &lockAcquired, sampledBefore: &sampledBefore}
	s.afterSupervisorLeaseBegin = func() { lockAcquired.Store(true) }
	_, hash := supervisorToken(10)
	if _, _, err := s.ClaimSupervisorLease(context.Background(), "RUN-6", 101, 102, "boot-a", hash, 900); err != nil {
		t.Fatal(err)
	}
	if sampledBefore.Load() {
		t.Fatal("supervisor lease clock was sampled before BEGIN IMMEDIATE acquired the writer lock")
	}
}
