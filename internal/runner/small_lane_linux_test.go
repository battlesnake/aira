package runner

import (
	"strings"
	"testing"
	"time"
)

// verifies: AIRA-350 §2.6 — the wait line carries ONE lane clause, held behind
// the head first; nothing while the lane is off. Mutations: a clause dropped or
// misspelt; laneWait winning over heldByHead.
func TestConfineLaneNote(t *testing.T) {
	for _, test := range []struct {
		laneWait string
		held     bool
		want     string
	}{
		{"", false, ""},
		{"full", true, ", fits but held behind the head of the queue"},
		{"", true, ", fits but held behind the head of the queue"},
		{"full", false, ", small-job lane full"},
		{"kept", false, ", small-job lane kept back so a larger job ahead can fit"},
		{"pushed", false, ", small-job lane closed behind a pushed job"},
		{"no-room", false, ", no room or CPU slot in the slice"},
	} {
		if got := confineLaneNote(confineQueuePosition{laneWait: test.laneWait, heldByHead: test.held}); got != test.want {
			t.Fatalf("laneWait=%q held=%v: %q, want %q", test.laneWait, test.held, got, test.want)
		}
	}
}

// verifies: AIRA-350 §2.6 — a queued row carries the entry's lane diagnoses.
func TestQueuedConfineRecordCarriesLaneDiagnoses(t *testing.T) {
	record, _, ok := queuedConfineRecord(ConfineQueuedEntry{
		ScopeID: "CONFINE-job-4242-abc", ReserveBytes: 1 << 30, LaneWait: "kept", HeldByHead: true,
	}, time.Now())
	if !ok || record.LaneWait != "kept" || !record.HeldByHead {
		t.Fatalf("record %+v ok=%v", record, ok)
	}
}

// verifies: AIRA-350 §2.6 — end to end through the probe: the daemon's
// lane_wait and held_by_head reach the wait line. Mutation: drop the probe
// parser's copy of either -> red.
func TestConfineQueueNoteCarriesTheLaneClause(t *testing.T) {
	base := ConfineSliceReserve{Queued: 4, QueuePosition: 3, GrantedBytes: 8 << 30, Jobs: 2, CeilingBytes: 61 << 30, GrantedEstablished: true}
	kept := base
	kept.LaneWait = "kept"
	if note := queueNoteFor(t, 512<<20, &kept); !strings.Contains(note, ", 3 of 4 queued, small-job lane kept back so a larger job ahead can fit") {
		t.Fatalf("note %q lacks the kept clause", note)
	}
	held := base
	held.LaneWait, held.HeldByHead = "full", true
	if note := queueNoteFor(t, 512<<20, &held); !strings.Contains(note, ", 3 of 4 queued, fits but held behind the head of the queue") || strings.Contains(note, "lane full") {
		t.Fatalf("note %q: held behind the head must be the one clause", note)
	}
	if note := queueNoteFor(t, 512<<20, &base); strings.Contains(note, "lane") || strings.Contains(note, "held behind") {
		t.Fatalf("note %q has a lane clause with the lane off", note)
	}
}
