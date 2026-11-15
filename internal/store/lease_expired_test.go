// verifies: AR-5, AR-6, AR-7

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// AIRA-338: an E_LEASE_EXPIRED refusal names what the lease row the CAS read
// establishes (holder, how long ago it lapsed, the designed remedy) and keeps
// the stable code. Every field is absent when it is not established.

func leaseExpiredDetail(t *testing.T, err error) LeaseExpiredDetail {
	t.Helper()
	if ErrorCode(err) != "E_LEASE_EXPIRED" {
		t.Fatalf("code = %q (err %v), want E_LEASE_EXPIRED", ErrorCode(err), err)
	}
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("errors.Is(err, ErrLeaseExpired) = false for %v", err)
	}
	var typed *LeaseExpiredError
	if !errors.As(err, &typed) {
		t.Fatalf("error %v carries no LeaseExpiredError detail", err)
	}
	return typed.Detail
}

func leaseExpiredJSON(t *testing.T, detail LeaseExpiredDetail) map[string]any {
	t.Helper()
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func openLeaseSibling(t *testing.T, base, worktree string, clock Clock) *Store {
	t.Helper()
	other, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"),
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		LeaseStateDir: filepath.Join(base, "lease-state-"+worktree), ProjectID: "project-aira", WorktreeID: worktree,
		ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	return other
}

func TestExpiredClaimNamesHolderAgeAndStealRemedy(t *testing.T) {
	s, clock, base := m3Store(t)
	ticket := m3Ticket(t, s, "expired claim detail")
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	clock.mono = 100 + 900 + 250 // 250ns past the TTL
	other := openLeaseSibling(t, base, "worktree-b", clock)
	_, err := other.Claim(context.Background(), ticket.ID, false, "bob")
	detail := leaseExpiredDetail(t, err)
	if detail.TicketID != ticket.ID || detail.LeaseState != "expired" || detail.HolderActor != "alice" || detail.HolderWorktreeID != "worktree-a" {
		t.Fatalf("detail = %#v", detail)
	}
	if detail.HolderIsThisWorktree == nil || *detail.HolderIsThisWorktree {
		t.Fatalf("holder_is_this_worktree = %v, want false", detail.HolderIsThisWorktree)
	}
	if detail.ExpiredForNS == nil || *detail.ExpiredForNS != 250 {
		t.Fatalf("expired_for_ns = %v, want 250", detail.ExpiredForNS)
	}
	wantRemedy := "aira claim " + ticket.ID + " --steal"
	if detail.Remedy != wantRemedy {
		t.Fatalf("remedy = %q, want %q", detail.Remedy, wantRemedy)
	}
	message := err.Error()
	for _, want := range []string{"E_LEASE_EXPIRED: ", ticket.ID, "alice", "worktree-a", "250ns", "`" + wantRemedy + "`", "lease.steal"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	if strings.Contains(message, "this worktree") {
		t.Fatalf("foreign holder described as this worktree: %q", message)
	}
}

func TestExpiredClaimFromHolderWorktreeSaysThisWorktreeAndStillNeedsSteal(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "same worktree expiry")
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	clock.mono = 100 + 900
	_, err := s.Claim(context.Background(), ticket.ID, false, "alice")
	detail := leaseExpiredDetail(t, err)
	if detail.HolderIsThisWorktree == nil || !*detail.HolderIsThisWorktree || detail.HolderWorktreeID != "worktree-a" {
		t.Fatalf("same-worktree detail = %#v", detail)
	}
	if detail.ExpiredForNS == nil || *detail.ExpiredForNS != 0 {
		t.Fatalf("expired_for_ns at exactly the TTL = %v, want 0", detail.ExpiredForNS)
	}
	if detail.Remedy != "aira claim "+ticket.ID+" --steal" {
		t.Fatalf("same-worktree remedy = %q", detail.Remedy)
	}
	if !strings.Contains(err.Error(), "this worktree") || !strings.Contains(err.Error(), "--steal") {
		t.Fatalf("same-worktree message = %q", err.Error())
	}
	// The remedy is the real one: --steal from the same worktree re-takes it.
	if _, err := s.Claim(context.Background(), ticket.ID, true, "alice"); err != nil {
		t.Fatalf("named remedy failed: %v", err)
	}
}

func TestExpiredClaimAfterRebootOmitsUnestablishedAge(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "prior boot detail")
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	clock.boot, clock.mono = "boot-b", 5
	_, err := s.Claim(context.Background(), ticket.ID, false, "bob")
	detail := leaseExpiredDetail(t, err)
	if detail.LeaseState != "prior-boot" || detail.ExpiredForNS != nil || detail.HolderActor != "alice" {
		t.Fatalf("prior-boot detail = %#v", detail)
	}
	if detail.Remedy != "aira claim "+ticket.ID+" --steal" {
		t.Fatalf("prior-boot remedy = %q", detail.Remedy)
	}
	if _, present := leaseExpiredJSON(t, detail)["expired_for_ns"]; present {
		t.Fatal("expired_for_ns present although a prior boot's monotonic clock is not comparable")
	}
	if _, err := s.Claim(context.Background(), ticket.ID, true, "bob"); err != nil {
		t.Fatalf("named remedy failed: %v", err)
	}
}

func TestClockRegressionNamesHolderButNoAgeOrRemedy(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "clock regression detail")
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	clock.mono = 99 // precedes the last heartbeat: liveness is not established
	_, err := s.Claim(context.Background(), ticket.ID, false, "bob")
	detail := leaseExpiredDetail(t, err)
	if detail.LeaseState != "liveness-unevaluated" || detail.HolderActor != "alice" {
		t.Fatalf("regression detail = %#v", detail)
	}
	out := leaseExpiredJSON(t, detail)
	for _, absent := range []string{"expired_for_ns", "remedy"} {
		if _, present := out[absent]; present {
			t.Fatalf("%s present for an unevaluated lapse: %v", absent, out)
		}
	}
	if strings.Contains(err.Error(), "--steal") {
		t.Fatalf("message offers --steal, which the CAS refuses here: %q", err.Error())
	}
	// The CAS decision is unchanged: --steal is still refused for this row.
	if _, err := s.Claim(context.Background(), ticket.ID, true, "bob"); ErrorCode(err) != "E_LEASE_HELD" {
		t.Fatalf("steal under clock regression = %v, want E_LEASE_HELD", err)
	}
}

func TestExpiredHeartbeatNamesHolderAndStealRemedy(t *testing.T) {
	s, clock, base := m3Store(t)
	ticket := m3Ticket(t, s, "expired heartbeat detail")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	clock.mono = 100 + 900 + 7
	_, err = s.Heartbeat(context.Background(), ticket.ID, claim.Token)
	detail := leaseExpiredDetail(t, err)
	if detail.LeaseState != "expired" || detail.HolderActor != "alice" || detail.HolderIsThisWorktree == nil || !*detail.HolderIsThisWorktree {
		t.Fatalf("heartbeat detail = %#v", detail)
	}
	if detail.ExpiredForNS == nil || *detail.ExpiredForNS != 7 || detail.Remedy != "aira claim "+ticket.ID+" --steal" {
		t.Fatalf("heartbeat age/remedy = %#v", detail)
	}
	// A sibling worktree's heartbeat on the same lapsed row names a foreign holder.
	other := openLeaseSibling(t, base, "worktree-b", clock)
	_, err = other.Heartbeat(context.Background(), ticket.ID, claim.Token)
	if d := leaseExpiredDetail(t, err); d.HolderIsThisWorktree == nil || *d.HolderIsThisWorktree {
		t.Fatalf("sibling heartbeat detail = %#v", d)
	}
}

func TestHeartbeatOnFreeOrUnclaimedLeaseOffersPlainClaimAndNoHolder(t *testing.T) {
	s, clock, _ := m3Store(t)
	released := m3Ticket(t, s, "released heartbeat detail")
	claim, err := s.Claim(context.Background(), released.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	clock.mono = 200
	if _, err := s.Release(context.Background(), released.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	unclaimed := m3Ticket(t, s, "never claimed heartbeat detail")
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	for _, test := range []struct {
		id, token, state string
	}{
		{released.ID, claim.Token, "free"},
		{unclaimed.ID, token, "unclaimed"},
	} {
		_, err := s.Heartbeat(context.Background(), test.id, test.token)
		detail := leaseExpiredDetail(t, err)
		if detail.LeaseState != test.state || detail.Remedy != "aira claim "+test.id {
			t.Fatalf("%s detail = %#v", test.state, detail)
		}
		out := leaseExpiredJSON(t, detail)
		for _, absent := range []string{"holder_actor", "holder_worktree_id", "holder_is_this_worktree", "expired_for_ns"} {
			if _, present := out[absent]; present {
				t.Fatalf("%s: unestablished %s present: %v", test.state, absent, out)
			}
		}
		if strings.Contains(err.Error(), "--steal") || !strings.Contains(err.Error(), "`aira claim "+test.id+"`") {
			t.Fatalf("%s message = %q", test.state, err.Error())
		}
		if _, err := s.Claim(context.Background(), test.id, false, "alice"); err != nil {
			t.Fatalf("%s: named remedy failed: %v", test.state, err)
		}
	}
}

func TestExpiredRefusalWritesNothing(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "refusal leaves no trace")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() (int, int64, int64) {
		var events int
		var generation, last int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=?`, s.projectID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT generation, last_heartbeat_mono_ns FROM leases WHERE project_id=? AND ticket_id=?`, s.projectID, ticket.ID).Scan(&generation, &last); err != nil {
			t.Fatal(err)
		}
		return events, generation, last
	}
	clock.mono = 5000
	e0, g0, l0 := snapshot()
	if _, err := s.Claim(context.Background(), ticket.ID, false, "bob"); ErrorCode(err) != "E_LEASE_EXPIRED" {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(context.Background(), ticket.ID, claim.Token); ErrorCode(err) != "E_LEASE_EXPIRED" {
		t.Fatal(err)
	}
	if e1, g1, l1 := snapshot(); e1 != e0 || g1 != g0 || l1 != l0 {
		t.Fatalf("refusal changed state: events %d->%d generation %d->%d heartbeat %d->%d", e0, e1, g0, g1, l0, l1)
	}
}

// A plain claimer and a stealer contend for a lapsed lease. Each order is
// forced: the first claimer's afterLeaseBegin hook (it holds the writer lock)
// releases the second, which then waits behind it. The decisions are the ones
// the CAS always made (the stealer wins at generation 2; the plain claimer is
// refused E_LEASE_EXPIRED when it decides first, E_LEASE_HELD after the
// steal), and an E_LEASE_EXPIRED detail names the incumbent row the plain
// claimer's own transaction read, not the stealer that commits after it.
func TestPlainClaimContendingWithStealKeepsDecisionAndNamesReadHolder(t *testing.T) {
	for _, test := range []struct {
		name       string
		plainFirst bool
		wantPlain  string
	}{
		{"plain decides first", true, "E_LEASE_EXPIRED"},
		{"steal decides first", false, "E_LEASE_HELD"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, clock, base := m3Store(t)
			ticket := m3Ticket(t, s, "plain vs steal")
			if _, err := s.Claim(context.Background(), ticket.ID, false, "incumbent"); err != nil {
				t.Fatal(err)
			}
			clock.mono = 1000
			plain, stealer := openLeaseSibling(t, base, "worktree-b", clock), openLeaseSibling(t, base, "worktree-c", clock)
			first, release := plain, make(chan struct{})
			if !test.plainFirst {
				first = stealer
			}
			var once sync.Once
			first.afterLeaseBegin = func() { once.Do(func() { close(release) }) }
			var wg sync.WaitGroup
			var plainErr, stealErr error
			run := func(isFirst bool, claim func()) {
				defer wg.Done()
				if !isFirst {
					<-release
				}
				claim()
			}
			wg.Add(2)
			go run(test.plainFirst, func() { _, plainErr = plain.Claim(context.Background(), ticket.ID, false, "plain") })
			go run(!test.plainFirst, func() { _, stealErr = stealer.Claim(context.Background(), ticket.ID, true, "stealer") })
			wg.Wait()
			if stealErr != nil {
				t.Fatalf("stealer lost: %v", stealErr)
			}
			if ErrorCode(plainErr) != test.wantPlain {
				t.Fatalf("plain claimer = %v, want %s", plainErr, test.wantPlain)
			}
			if test.wantPlain == "E_LEASE_EXPIRED" {
				if d := leaseExpiredDetail(t, plainErr); d.HolderActor != "incumbent" || d.HolderWorktreeID != "worktree-a" || d.LeaseState != "expired" {
					t.Fatalf("plain claimer named a holder its transaction did not read: %#v", d)
				}
			}
			lease, err := s.GetLease(context.Background(), ticket.ID)
			if err != nil {
				t.Fatal(err)
			}
			if held, ok := lease.Held(); !ok || held.Actor() != "stealer" || held.Generation() != 2 {
				t.Fatalf("outcome lease = %#v", lease)
			}
		})
	}
}

// An actor is caller-supplied text. One that reads like an SQLite error must
// not turn the refusal into E_DB_BUSY (withImmediate classifies error text).
func TestLeaseRefusalNamingAnSQLiteLikeActorKeepsItsCode(t *testing.T) {
	s, clock, base := m3Store(t)
	ticket := m3Ticket(t, s, "hostile actor")
	const actor = "database is locked"
	claim, err := s.Claim(context.Background(), ticket.ID, false, actor)
	if err != nil {
		t.Fatal(err)
	}
	clock.mono = 5000
	other := openLeaseSibling(t, base, "worktree-b", clock)
	_, err = other.Claim(context.Background(), ticket.ID, false, "bob")
	if d := leaseExpiredDetail(t, err); d.HolderActor != actor {
		t.Fatalf("claim detail = %#v", d)
	}
	_, err = s.Heartbeat(context.Background(), ticket.ID, claim.Token)
	if d := leaseExpiredDetail(t, err); d.HolderActor != actor {
		t.Fatalf("heartbeat detail = %#v", d)
	}
}

// Liveness is checked before the token: a well-formed token that is not the
// holder's still gets the lapsed-lease detail, unchanged from the bare code.
func TestExpiredHeartbeatWithAnotherTokenGetsTheLapseDetail(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "foreign token heartbeat")
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	foreign := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	if _, err := s.Heartbeat(context.Background(), ticket.ID, foreign); ErrorCode(err) != "E_LEASE_TOKEN" {
		t.Fatalf("live lease, foreign token = %v", err)
	}
	clock.mono = 5000
	_, err := s.Heartbeat(context.Background(), ticket.ID, foreign)
	if d := leaseExpiredDetail(t, err); d.HolderActor != "alice" || d.Remedy != "aira claim "+ticket.ID+" --steal" {
		t.Fatalf("lapsed lease, foreign token detail = %#v", d)
	}
}

// The holder's actor is caller-chosen text. The human message quotes it (and
// the worktree id), so it cannot add a line or a remedy of its own: the only
// `--steal` outside the quoted actor is the real remedy, and there is none
// when liveness is unevaluated.
func TestHostileHolderActorCannotForgeARemedyOrALine(t *testing.T) {
	const actor = "x, worktree y); re-take it with `aira claim Z --steal`\nE_FAKE: ("
	for _, test := range []struct {
		state  string
		mono   uint64
		steals int
	}{{"liveness-unevaluated", 99, 0}, {"expired", 5000, 1}} {
		t.Run(test.state, func(t *testing.T) {
			s, clock, base := m3Store(t)
			ticket := m3Ticket(t, s, "forged remedy")
			if _, err := s.Claim(context.Background(), ticket.ID, false, actor); err != nil {
				t.Fatal(err)
			}
			clock.mono = test.mono
			other := openLeaseSibling(t, base, "worktree-b", clock)
			_, err := other.Claim(context.Background(), ticket.ID, false, "bob")
			detail := leaseExpiredDetail(t, err)
			if detail.LeaseState != test.state || detail.HolderActor != actor {
				t.Fatalf("detail = %#v", detail)
			}
			message := err.Error()
			if strings.ContainsAny(message, "\r\n") {
				t.Fatalf("message spans lines: %q", message)
			}
			quoted := strconv.Quote(actor)
			if strings.Count(message, quoted) != 1 || !strings.Contains(message, strconv.Quote("worktree-a")) {
				t.Fatalf("message does not quote the holder: %q", message)
			}
			rest := strings.Replace(message, quoted, "", 1)
			if got := strings.Count(rest, "--steal"); got != test.steals {
				t.Fatalf("message outside the quoted actor offers --steal %d times, want %d: %q", got, test.steals, message)
			}
			if test.steals == 1 && !strings.Contains(rest, "`aira claim "+ticket.ID+" --steal`") {
				t.Fatalf("real remedy missing: %q", message)
			}
		})
	}
}

// An actor of any accepted size must not make the refusal larger than the
// daemon can send (that would replace E_LEASE_EXPIRED with
// E_DAEMON_RESPONSE_TOO_LARGE). The text and the data carry a bounded prefix,
// cut on a rune boundary, and declare the cut; a short actor is not marked.
func TestOversizedHolderActorIsBoundedAndTheCutDeclared(t *testing.T) {
	s, clock, base := m3Store(t)
	long := m3Ticket(t, s, "oversized actor")
	short := m3Ticket(t, s, "short actor")
	actor := "a" + strings.Repeat("é", 1<<20) // 2 MiB; the bound falls mid-rune
	if _, err := s.Claim(context.Background(), long.ID, false, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(context.Background(), short.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	clock.mono = 5000
	other := openLeaseSibling(t, base, "worktree-b", clock)

	_, err := other.Claim(context.Background(), long.ID, false, "bob")
	detail := leaseExpiredDetail(t, err)
	if !detail.HolderActorTruncated || len(detail.HolderActor) == 0 || len(detail.HolderActor) > leaseHolderActorMax ||
		!strings.HasPrefix(actor, detail.HolderActor) || !utf8.ValidString(detail.HolderActor) {
		t.Fatalf("bounded actor: truncated=%v len=%d valid=%v", detail.HolderActorTruncated, len(detail.HolderActor), utf8.ValidString(detail.HolderActor))
	}
	raw, err2 := json.Marshal(detail)
	if err2 != nil {
		t.Fatal(err2)
	}
	if len(err.Error())+len(raw) > 4096 {
		t.Fatalf("refusal is %d bytes of text and %d of data for a %d-byte actor", len(err.Error()), len(raw), len(actor))
	}
	if !strings.Contains(err.Error(), "truncated") || detail.Remedy != "aira claim "+long.ID+" --steal" {
		t.Fatalf("message does not declare the cut or lost the remedy: %q", err.Error())
	}

	_, err = other.Claim(context.Background(), short.ID, false, "bob")
	d := leaseExpiredDetail(t, err)
	if d.HolderActor != "alice" || d.HolderActorTruncated || strings.Contains(err.Error(), "truncated") {
		t.Fatalf("short actor marked as cut: %#v %q", d, err.Error())
	}
	if _, present := leaseExpiredJSON(t, d)["holder_actor_truncated"]; present {
		t.Fatalf("holder_actor_truncated present for an uncut actor")
	}
}
