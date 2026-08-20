package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestListLeasesReturnsEveryHeldLeaseHonestly(t *testing.T) {
	s, _, _ := m3Store(t)
	clock := &countingClock{boot: "boot-a", mono: 100}
	s.clock = clock

	hash := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("h", 32)))
	insert := func(ticketID, state, boot string, last, ttl int64) {
		t.Helper()
		if state == "free" {
			if _, err := s.db.Exec(`INSERT INTO leases(project_id, ticket_id, state, generation)
				VALUES(?, ?, 'free', 4)`, s.projectID, ticketID); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := s.db.Exec(`INSERT INTO leases(project_id, ticket_id, state, generation,
			holder_token_hash, boot_id, last_heartbeat_mono_ns, ttl_ns, actor, worktree_id)
			VALUES(?, ?, 'held', 3, ?, ?, ?, ?, 'alice', 'worktree-a')`,
			s.projectID, ticketID, hash, boot, last, ttl); err != nil {
			t.Fatal(err)
		}
	}
	insert("AIRA-1", "held", "boot-a", 0, 50)     // age 100 > ttl 50 → expired
	insert("AIRA-2", "held", "boot-old", 99, 900) // prior boot → stale
	insert("AIRA-3", "held", "boot-a", 101, 900)  // last > sample → concurrently renewed
	insert("AIRA-5", "held", "boot-a", 50, 50)    // age 50 == ttl 50 → expired (>=, NOT >)
	insert("AIRA-6", "held", "boot-a", 80, 50)    // age 20 < ttl 50 → live
	insert("AIRA-4", "free", "", 0, 0)

	rows, err := s.ListLeases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if clock.calls != 1 {
		t.Fatalf("clock samples=%d, want exactly 1", clock.calls)
	}
	if len(rows) != 5 {
		t.Fatalf("rows=%#v, want exactly the five held rows (free excluded)", rows)
	}
	if rows[0].TicketID != "AIRA-1" || !rows[0].Expired || rows[0].AgeNote == "" {
		t.Fatalf("expired held row=%#v", rows[0])
	}
	if rows[1].TicketID != "AIRA-2" || !rows[1].Expired || rows[1].AgeNote != "stale (prior boot)" {
		t.Fatalf("prior-boot row=%#v", rows[1])
	}
	if rows[2].TicketID != "AIRA-3" || rows[2].Expired || rows[2].AgeNote != "concurrently renewed" {
		t.Fatalf("heartbeat-after-sample row=%#v", rows[2])
	}
	// age == ttl is the reap boundary: the predicate is >=, so this MUST be
	// expired. A `>`-only implementation would wrongly report it live.
	if rows[3].TicketID != "AIRA-5" || !rows[3].Expired {
		t.Fatalf("age==ttl boundary must be expired (>=): %#v", rows[3])
	}
	// age < ttl is genuinely live, with a real (non-marker) age note.
	if rows[4].TicketID != "AIRA-6" || rows[4].Expired || rows[4].AgeNote == "" ||
		rows[4].AgeNote == "stale (prior boot)" || rows[4].AgeNote == "concurrently renewed" {
		t.Fatalf("live lease row=%#v", rows[4])
	}

	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "holder_token") || strings.Contains(string(raw), hash) {
		t.Fatalf("lease wire data leaked holder token hash: %s", raw)
	}
	var roundTrip []HeldLeaseRow
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, rows) {
		t.Fatalf("round trip=%#v, want %#v", roundTrip, rows)
	}
}
