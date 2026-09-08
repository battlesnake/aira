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

// TestP3IsARealSeverityTheReaderPathAccepts is the RED direction of AIRA-170's
// first half. Seven tickets already merged to master — AIRA-162 through
// AIRA-168 — carry "severity":"P3" in their canonical git frontmatter, and
// before this change validSeverity accepted only P0..P2, so every reader path
// refused a value the writer path had already committed. A value the file
// format accepts and the tool refuses is not a stricter enum; it is a store
// that cannot read its own contents.
//
// verifies: AIRA-170
func TestP3IsARealSeverityTheReaderPathAccepts(t *testing.T) {
	if !ValidSeverity(SeverityP3) {
		t.Fatal("P3 must be a valid severity: the repository's own merged tickets carry it")
	}
	for _, severity := range []Severity{SeverityP0, SeverityP1, SeverityP2, SeverityP3} {
		ticket := Ticket{
			Schema: 1, ID: "AIRA-165", Project: "aira", Title: "a P3 ticket parses",
			Status: StatusPlanned, Kind: KindBug, Severity: severity,
		}
		data, err := RenderTicket(ticket, "Body\n")
		if err != nil {
			t.Fatalf("render %s: %v", severity, err)
		}
		got, _, err := ParseTicket(data)
		if err != nil {
			t.Fatalf("ParseTicket %s: %v", severity, err)
		}
		if got.Severity != severity {
			t.Fatalf("severity round trip: got %q want %q", got.Severity, severity)
		}
	}
	// Widening must not have turned the enum into a free-text field: the ladder
	// is P0..P3 and nothing else.
	for _, rejected := range []Severity{"", "P4", "P9", "p3", "P3 ", "critical"} {
		if ValidSeverity(rejected) {
			t.Fatalf("severity %q must still be refused", rejected)
		}
	}
	if want := []string{"P0", "P1", "P2", "P3"}; !reflect.DeepEqual(AllowedSeverityStrings(), want) {
		t.Fatalf("AllowedSeverityStrings() = %v, want %v", AllowedSeverityStrings(), want)
	}
	// Findings share the ticket ladder through the same validator, so widening
	// it must widen both surfaces or `find --severity P3` becomes a new instance
	// of the very split this ticket closed.
	finding := ReviewFindingInput{
		TicketID: "AIRA-170", Category: "docs", Source: "opus", Message: "accepted deferral",
		Severity: SeverityP3, Verdict: VerdictConfirmed, Disposition: DispositionOpen,
	}
	if _, err := NewReviewFinding(finding); err != nil {
		t.Fatalf("a P3 finding must be accepted: %v", err)
	}
}

// TestInvalidTicketFieldNamesFieldValueAndAllowedSet is AIRA-170's second half.
//
// The old refusal was `E_CONFIG_INVALID: ticket enum is invalid`, which is
// wrong twice: the code blames `.aira/config`, which is not the broken thing,
// and the message names neither which of the three enums failed, nor what it
// held, nor what it could legally hold. The fixture severity here is
// deliberately a value nothing would ever legitimately write ("P9") rather
// than P3, so this stays a live test of the ERROR after P3 itself became
// valid.
//
// verifies: AIRA-170
func TestInvalidTicketFieldNamesFieldValueAndAllowedSet(t *testing.T) {
	base := Ticket{
		Schema: 1, ID: "AIRA-1", Project: "aira", Title: "invalid field",
		Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2,
	}
	cases := []struct {
		name    string
		mutate  func(*Ticket)
		field   string
		value   string
		allowed []string
	}{
		{"severity", func(ticket *Ticket) { ticket.Severity = "P9" }, "severity", "P9", AllowedSeverityStrings()},
		{"status", func(ticket *Ticket) { ticket.Status = "blocked" }, "status", "blocked", AllowedStatusStrings()},
		{"kind", func(ticket *Ticket) { ticket.Kind = "epic" }, "kind", "epic", AllowedKindStrings()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticket := base
			tc.mutate(&ticket)
			err := ticket.Validate()
			if err == nil {
				t.Fatal("an invalid enum must be refused")
			}
			message := err.Error()
			if !strings.HasPrefix(message, CodeTicketInvalid+":") {
				t.Fatalf("refusal must carry the ticket-shaped code, got %q", message)
			}
			if strings.Contains(message, "E_CONFIG_INVALID") {
				t.Fatalf("a ticket's own field must not be reported as a broken config: %q", message)
			}
			for _, want := range []string{tc.field, tc.value} {
				if !strings.Contains(message, want) {
					t.Fatalf("refusal %q does not name %q", message, want)
				}
			}
			for _, allowed := range tc.allowed {
				if !strings.Contains(message, allowed) {
					t.Fatalf("refusal %q does not name the allowed value %q", message, allowed)
				}
			}
		})
	}
	// The same refusal must survive a round trip through ParseTicket, which is
	// the path every reader (show, link, scan, rant --ref) actually takes.
	broken := base
	broken.Severity = "P9"
	data, err := json.Marshal(broken)
	if err != nil {
		t.Fatal(err)
	}
	data = append(append([]byte("---\n"), data...), []byte("\n---\nbody\n")...)
	_, _, parseErr := ParseTicket(data)
	if parseErr == nil || !strings.HasPrefix(parseErr.Error(), CodeTicketInvalid+":") ||
		!strings.Contains(parseErr.Error(), `severity "P9"`) {
		t.Fatalf("ParseTicket refusal = %v, want %s naming severity \"P9\"", parseErr, CodeTicketInvalid)
	}
}
