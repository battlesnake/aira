// verifies: AR-5, AR-6

package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestLeaseConstructorsRejectIllegalHeldStatesAndNilLeaseIsNeither(t *testing.T) {
	hash := bytes.Repeat([]byte{0x42}, 32)
	valid, err := NewHeldLease(hash, "boot-a", 100, 900, 1, "actor", "worktree")
	if err != nil {
		t.Fatalf("valid held lease rejected: %v", err)
	}
	if lease := (Lease{TicketID: "AIRA-1", State: valid}); !lease.Valid() {
		t.Fatal("validated held lease is not valid")
	}
	for name, args := range map[string]struct {
		hash       []byte
		boot       string
		heartbeat  uint64
		ttl        int64
		generation uint64
		actor      string
		worktree   string
	}{
		"zero generation":   {hash: hash, boot: "boot-a", heartbeat: 100, ttl: 900, generation: 0, actor: "actor", worktree: "worktree"},
		"zero ttl":          {hash: hash, boot: "boot-a", heartbeat: 100, ttl: 0, generation: 1, actor: "actor", worktree: "worktree"},
		"negative ttl":      {hash: hash, boot: "boot-a", heartbeat: 100, ttl: -1, generation: 1, actor: "actor", worktree: "worktree"},
		"empty holder hash": {hash: nil, boot: "boot-a", heartbeat: 100, ttl: 900, generation: 1, actor: "actor", worktree: "worktree"},
		"empty boot":        {hash: hash, boot: "", heartbeat: 100, ttl: 900, generation: 1, actor: "actor", worktree: "worktree"},
		"empty actor":       {hash: hash, boot: "boot-a", heartbeat: 100, ttl: 900, generation: 1, actor: "", worktree: "worktree"},
		"empty worktree":    {hash: hash, boot: "boot-a", heartbeat: 100, ttl: 900, generation: 1, actor: "actor", worktree: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewHeldLease(args.hash, args.boot, args.heartbeat, args.ttl, args.generation, args.actor, args.worktree); err == nil {
				t.Fatal("illegal held lease was accepted")
			}
		})
	}

	var zero Lease
	if zero.Valid() {
		t.Fatal("zero lease is valid")
	}
	if _, ok := zero.Held(); ok {
		t.Fatal("nil-state lease reports Held")
	}
	if _, ok := zero.Free(); ok {
		t.Fatal("nil-state lease reports Free")
	}
	invalid := Lease{TicketID: "AIRA-1", State: HeldLease{}}
	if invalid.Valid() {
		t.Fatal("unvalidated held lease is valid")
	}
	if _, ok := invalid.Held(); ok {
		t.Fatal("unvalidated held lease reports Held")
	}
}

func TestHeldLeaseFieldsAreSealedAndGettersPreserveConstruction(t *testing.T) {
	typ := reflect.TypeOf(HeldLease{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).PkgPath == "" {
			t.Fatalf("HeldLease field %q is exported", typ.Field(i).Name)
		}
	}
	hash := bytes.Repeat([]byte{0x42}, 32)
	held, err := NewHeldLease(hash, "boot-a", 100, 900, 7, "actor", "worktree")
	if err != nil {
		t.Fatal(err)
	}
	if held.Generation() != 7 || held.BootID() != "boot-a" || held.LastHeartbeatMonoNS() != 100 || held.TTLNS() != 900 || held.Actor() != "actor" || held.Worktree() != "worktree" {
		t.Fatalf("held getters = generation %d boot %q heartbeat %d ttl %d actor %q worktree %q", held.Generation(), held.BootID(), held.LastHeartbeatMonoNS(), held.TTLNS(), held.Actor(), held.Worktree())
	}
	returnedHash := held.HolderTokenHash()
	returnedHash[0] = 0
	if held.HolderTokenHash()[0] == 0 || !held.IsLive("boot-a", 100) {
		t.Fatal("getter exposed mutable held lease state")
	}
}

func TestTicketRoundTripAndCanonicalRelationOrder(t *testing.T) {
	ticket := Ticket{
		ID:       "AIRA-42",
		Project:  "aira",
		Title:    "Implement the ready queue",
		Status:   StatusPlanned,
		Kind:     KindFeature,
		Severity: SeverityP2,
		Labels:   []string{"queue", "phase1"},
		Relations: []Relation{
			{Kind: RelationRelates, From: "AIRA-42", To: "AIRA-44"},
			{Kind: RelationBlocks, From: "AIRA-42", To: "AIRA-43"},
		},
	}
	data, err := RenderTicket(ticket, "Body\n")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, body, err := ParseTicket(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ID != ticket.ID || got.Title != ticket.Title || got.Kind != KindFeature {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	if body != "Body\n" {
		t.Fatalf("body = %q", body)
	}
	if len(got.Relations) != 2 || got.Relations[0].Kind != RelationBlocks {
		t.Fatalf("relations were not canonicalised: %#v", got.Relations)
	}
}

func TestStatusTransitionGraph(t *testing.T) {
	allowed := [][2]Status{
		{StatusDraft, StatusPlanned},
		{StatusPlanned, StatusInProgress},
		{StatusInProgress, StatusInReview},
		{StatusInReview, StatusDone},
		{StatusDone, StatusRetired},
	}
	for _, edge := range allowed {
		if err := ValidateTransition(edge[0], edge[1]); err != nil {
			t.Errorf("expected %s -> %s to be allowed: %v", edge[0], edge[1], err)
		}
	}
	if err := ValidateTransition(StatusDone, StatusInProgress); err == nil {
		t.Fatal("backward transition unexpectedly allowed")
	}
}

func TestRenderTicketDeduplicatesSortedLabels(t *testing.T) {
	ticket := Ticket{Schema: 1, ID: "AIRA-1", Project: "aira", Title: "labels", Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2, Labels: []string{"z", "a", "z", "a"}}
	data, err := RenderTicket(ticket, "body")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := ParseTicket(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Labels) != 2 || parsed.Labels[0] != "a" || parsed.Labels[1] != "z" {
		t.Fatalf("labels = %#v", parsed.Labels)
	}
}

func TestParseTicketRejectsNonCanonicalLabels(t *testing.T) {
	ticket := Ticket{Schema: 1, ID: "AIRA-1", Project: "aira", Title: "labels", Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2, Labels: []string{"z", "a", "z"}}
	data, err := json.Marshal(ticket)
	if err != nil {
		t.Fatal(err)
	}
	data = append(append([]byte("---\n"), data...), []byte("\n---\nbody\n")...)
	if _, _, err := ParseTicket(data); err == nil {
		t.Fatal("non-canonical labels parsed successfully")
	}
}

func TestParseTicketRejectsNonCanonicalRelations(t *testing.T) {
	cases := []struct {
		name      string
		ticketID  string
		relations []Relation
	}{
		{
			name:     "duplicate",
			ticketID: "AIRA-1",
			relations: []Relation{
				{Kind: RelationBlocks, From: "AIRA-1", To: "AIRA-2"},
				{Kind: RelationBlocks, From: "AIRA-1", To: "AIRA-2"},
			},
		},
		{
			name:     "unsorted",
			ticketID: "AIRA-1",
			relations: []Relation{
				{Kind: RelationRelates, From: "AIRA-1", To: "AIRA-3"},
				{Kind: RelationBlocks, From: "AIRA-1", To: "AIRA-2"},
			},
		},
		{
			name:     "wrong canonical side",
			ticketID: "AIRA-2",
			relations: []Relation{
				{Kind: RelationBlocks, From: "AIRA-2", To: "AIRA-1"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticket := Ticket{Schema: 1, ID: tc.ticketID, Project: "aira", Title: "relations", Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2, Relations: tc.relations}
			data, err := json.Marshal(ticket)
			if err != nil {
				t.Fatal(err)
			}
			data = append(append([]byte("---\n"), data...), []byte("\n---\nbody\n")...)
			if _, _, err := ParseTicket(data); err == nil {
				t.Fatal("non-canonical relations parsed successfully")
			}
		})
	}
}
