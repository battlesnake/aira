// verifies: AR-5, AR-6

package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLeaseConstructorsRejectIllegalHeldStatesAndNilLeaseIsNeither(t *testing.T) {
	hash := bytes.Repeat([]byte{0x42}, 32)
	valid, err := NewHeldLease(hash, "boot-a", 100, 900, 1, "actor", "worktree")
	if err != nil {
		t.Fatalf("valid held lease rejected: %v", err)
	}
	lease, err := NewLease("AIRA-1", valid)
	if err != nil || !lease.Valid() {
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
	if invalid, err := NewLease("AIRA-1", HeldLease{}); err == nil || invalid.Valid() {
		t.Fatal("constructor accepted unvalidated held lease")
	}
}

func TestLeaseStateIsSealedBehindValidatedLeaseConstructor(t *testing.T) {
	typ, ok := reflect.TypeOf(Lease{}).FieldByName("State")
	if ok && typ.PkgPath == "" {
		t.Fatal("Lease.State is exported and permits direct state injection")
	}
	hash := bytes.Repeat([]byte{0x42}, 32)
	held, err := NewHeldLease(hash, "boot-a", 100, 900, 1, "actor", "worktree")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLease("AIRA-1", held); err != nil {
		t.Fatalf("validated lease rejected: %v", err)
	}
	if _, err := NewLease("AIRA-1", HeldLease{}); err == nil {
		t.Fatal("constructor accepted an illegal held state")
	}
}

type leaseTicketIDGetter interface {
	TicketID() string
}

var _ leaseTicketIDGetter = Lease{}

func TestLeaseTicketIDIsSealedAndGetterPreservesConstruction(t *testing.T) {
	typ := reflect.TypeOf(Lease{})
	field, ok := typ.FieldByName("ticketID")
	if !ok || field.PkgPath == "" {
		t.Fatal("Lease ticket ID is not sealed")
	}
	hash := bytes.Repeat([]byte{0x42}, 32)
	held, err := NewHeldLease(hash, "boot-a", 100, 900, 1, "actor", "worktree")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewLease("AIRA-1", held)
	if err != nil {
		t.Fatal(err)
	}
	if lease.TicketID() != "AIRA-1" || !lease.Valid() {
		t.Fatalf("lease ID or validity = %q/%v", lease.TicketID(), lease.Valid())
	}
	data, err := json.Marshal(lease)
	if err != nil {
		t.Fatalf("marshal lease: %v", err)
	}
	var encoded struct {
		TicketID string `json:"ticket_id"`
	}
	if err := json.Unmarshal(data, &encoded); err != nil {
		t.Fatalf("decode lease JSON %q: %v", data, err)
	}
	if encoded.TicketID != "AIRA-1" {
		t.Fatalf("marshaled ticket_id = %q", encoded.TicketID)
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

func TestHeldLeaseJSONPreservesPreSealingShapeWithoutTokenHash(t *testing.T) {
	hash := bytes.Repeat([]byte{0x42}, 32)
	held, err := NewHeldLease(hash, "boot-a", 100, 900, 7, "actor", "worktree")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewLease("AIRA-1", held)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(lease)
	if err != nil {
		t.Fatalf("marshal lease: %v", err)
	}
	var encoded struct {
		TicketID string                     `json:"ticket_id"`
		State    map[string]json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(data, &encoded); err != nil {
		t.Fatalf("decode lease JSON %q: %v", data, err)
	}
	if encoded.TicketID != "AIRA-1" {
		t.Fatalf("ticket_id = %q", encoded.TicketID)
	}
	if len(encoded.State) != 6 {
		t.Fatalf("held state fields = %#v, want exactly the six public lease fields", encoded.State)
	}
	var state struct {
		BootID              string `json:"boot_id"`
		LastHeartbeatMonoNS uint64 `json:"last_heartbeat_mono_ns"`
		TTLNS               uint64 `json:"ttl_ns"`
		Generation          uint64 `json:"generation"`
		Actor               string `json:"actor"`
		Worktree            string `json:"worktree"`
	}
	stateData, err := json.Marshal(encoded.State)
	if err != nil {
		t.Fatalf("remarshal held state: %v", err)
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("decode held state: %v", err)
	}
	if state.BootID != "boot-a" || state.LastHeartbeatMonoNS != 100 || state.TTLNS != 900 || state.Generation != 7 || state.Actor != "actor" || state.Worktree != "worktree" {
		t.Fatalf("held state = %#v", state)
	}
	if _, ok := encoded.State["holder_token_hash"]; ok {
		t.Fatalf("held state exposed holder token hash: %s", data)
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

func TestParseTicketRejectsTrailingFrontmatterContent(t *testing.T) {
	ticket := Ticket{
		ID: "AIRA-42", Project: "aira", Title: "trailing content",
		Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2,
	}
	data, err := RenderTicket(ticket, "Body\n")
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\n---\n")
	for _, trailing := range [][]byte{[]byte("  GARBAGE"), []byte(` {"schema":1}`)} {
		bad := bytes.Replace(data, marker, append(append([]byte{}, trailing...), marker...), 1)
		if _, _, err := ParseTicket(bad); err == nil || !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
			t.Fatalf("ParseTicket trailing frontmatter content %q: expected E_CONFIG_INVALID, got %v", trailing, err)
		}
	}
}

func TestStatusTransitionGraph(t *testing.T) {
	allowed := [][2]Status{
		{StatusDraft, StatusPlanned},
		{StatusPlanned, StatusInProgress},
		{StatusInProgress, StatusInReview},
		{StatusInReview, StatusDone},
		{StatusDone, StatusRetired},
		{StatusDone, StatusInProgress}, // reopen: a done fix found partial returns to in-progress
	}
	for _, edge := range allowed {
		if err := ValidateTransition(edge[0], edge[1]); err != nil {
			t.Errorf("expected %s -> %s to be allowed: %v", edge[0], edge[1], err)
		}
	}
	// Reopen is scoped: done returns to in-progress only, never jumps back to planned.
	if err := ValidateTransition(StatusDone, StatusPlanned); err == nil {
		t.Fatal("done should reopen only to in-progress, not jump back to planned")
	}
	// Retired stays terminal — no reopen from it.
	if err := ValidateTransition(StatusRetired, StatusInProgress); err == nil {
		t.Fatal("retired must stay terminal")
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
