package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

func coreComputeI64(value int64) *int64 { return &value }

func TestSpendInputRejectsPayloadBucketsAndDuplicateBuckets(t *testing.T) {
	_, err := usageArgs(newArgAccessor(map[string]any{"raw": []byte(`{"output_tokens":1}`), "bucket": []string{"output=1"}, "reasoning-subset": false}), "anthropic")
	if err == nil || !strings.HasPrefix(err.Error(), domain.ComputeCodeInvalid) {
		t.Fatalf("payload plus buckets error = %v", err)
	}
	_, err = usageArgs(newArgAccessor(map[string]any{"bucket": []string{"output=1", "output=2"}, "reasoning-subset": false}), "mystery")
	if err == nil || !strings.HasPrefix(err.Error(), domain.ComputeCodeInvalid) {
		t.Fatalf("duplicate buckets error = %v", err)
	}
}

func TestSpendLSJSONPreservesAbsentAndExplicitZeroBuckets(t *testing.T) {
	s := coreTestStore(t)
	if _, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{}},
		GitContext: gitcontext.GitContext{HeadHash: gitcontext.Field{Value: "abc123", Status: gitcontext.StatusValue}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{Output: coreComputeI64(0)}},
	}); err != nil {
		t.Fatal(err)
	}
	response := New(s).Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls"}})
	if !response.OK {
		t.Fatalf("spend ls response=%#v", response)
	}
	var data struct {
		Rows []domain.ComputeEvent `json:"rows"`
	}
	marshalRoundTrip(t, response.Data, &data)
	if len(data.Rows) != 2 {
		t.Fatalf("spend ls rows=%#v", data.Rows)
	}
	if data.Rows[0].Buckets.Output == nil || *data.Rows[0].Buckets.Output != 0 {
		t.Fatalf("explicit zero row=%#v", data.Rows[0])
	}
	if data.Rows[1].Buckets.Output != nil {
		t.Fatalf("absent row=%#v", data.Rows[1])
	}
	if data.Rows[1].GitContext.HeadHash.Value != "abc123" || data.Rows[1].GitContext.HeadHash.Status != gitcontext.StatusValue {
		t.Fatalf("spend ls omitted compute provenance: %#v", data.Rows[1].GitContext)
	}
	zeroJSON, err := json.Marshal(data.Rows[0])
	if err != nil {
		t.Fatal(err)
	}
	absentJSON, err := json.Marshal(data.Rows[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(zeroJSON), `"output":0`) || strings.Contains(string(absentJSON), `"output"`) {
		t.Fatalf("nullable JSON zero=%s absent=%s", zeroJSON, absentJSON)
	}
}

func TestSpendAddRecordsUnevaluatedGitContext(t *testing.T) {
	s := coreTestStore(t)
	response := New(s).Do(context.Background(), Request{Verb: "spend", Args: map[string]any{
		"subverb": "add", "model": "gpt", "provider": "openai", "source": "manual", "bucket": []string{"output=0"},
	}})
	if !response.OK {
		t.Fatalf("spend add response=%+v", response)
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 1 || rows[0].GitContext.HeadHash.Status != gitcontext.StatusUnevaluated ||
		rows[0].GitContext.HeadRef.Status != gitcontext.StatusUnevaluated || rows[0].GitContext.WorktreeID.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("spend add git context rows=%#v err=%v", rows, err)
	}
}

// verifies: AIRA-284 — the core face passes --turn-id/--resolve-ticket through
// and renders the per-session view with its observed-subtotal label.
func spendAddArgs(extra map[string]any) map[string]any {
	args := map[string]any{"subverb": "add", "model": "claude-x", "provider": "anthropic", "source": "claude-mod", "session": "s1", "bucket": []string{"fresh_input=3", "output=4"}}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func TestSpendAddTurnIDIsIdempotentAndConflictIsRefused(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	first := c.Do(context.Background(), Request{Verb: "spend", Args: spendAddArgs(map[string]any{"turn-id": "t1"})})
	if !first.OK {
		t.Fatalf("first add = %+v", first)
	}
	dup := c.Do(context.Background(), Request{Verb: "spend", Args: spendAddArgs(map[string]any{"turn-id": "t1"})})
	if !dup.OK {
		t.Fatalf("duplicate add = %+v", dup)
	}
	var data struct {
		Duplicate bool   `json:"duplicate"`
		ID        string `json:"id"`
	}
	marshalRoundTrip(t, dup.Data, &data)
	if !data.Duplicate || data.ID != "CE-1" {
		t.Fatalf("duplicate data = %+v, want duplicate CE-1", data)
	}
	conflict := c.Do(context.Background(), Request{Verb: "spend", Args: spendAddArgs(map[string]any{"turn-id": "t1", "bucket": []string{"fresh_input=3", "output=5"}})})
	if conflict.OK || conflict.Code != domain.ComputeCodeTurnConflict {
		t.Fatalf("conflicting add = ok:%v code:%q, want %s", conflict.OK, conflict.Code, domain.ComputeCodeTurnConflict)
	}
	if conflict.Exit != 1 {
		t.Fatalf("conflict exit = %d, want 1 (a state conflict, not a bad request)", conflict.Exit)
	}
	bad := c.Do(context.Background(), Request{Verb: "spend", Args: spendAddArgs(map[string]any{"turn-id": "has space"})})
	if bad.OK || bad.Code != domain.ComputeCodeInvalid {
		t.Fatalf("bad turn-id = ok:%v code:%q", bad.OK, bad.Code)
	}
}

func TestSpendAddResolveTicketFlagReachesTheStore(t *testing.T) {
	s, _, _ := coreTestStoreWithClock(t)
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "t", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	c := New(s)
	with := c.Do(context.Background(), Request{Verb: "spend", Args: spendAddArgs(map[string]any{"turn-id": "t1", "resolve-ticket": true})})
	without := c.Do(context.Background(), Request{Verb: "spend", Args: spendAddArgs(map[string]any{"turn-id": "t2"})})
	if !with.OK || !without.OK {
		t.Fatalf("with=%+v without=%+v", with, without)
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	byTurn := map[string]domain.ComputeEvent{}
	for _, r := range rows {
		byTurn[r.TurnID] = r
	}
	if byTurn["t1"].TicketID != ticket.ID || byTurn["t1"].TicketStatus != domain.TicketStatusLeaseHeld {
		t.Fatalf("flagged row = %+v", byTurn["t1"])
	}
	if byTurn["t2"].TicketID != "" || byTurn["t2"].TicketStatus != domain.TicketStatusUnknown {
		t.Fatalf("unflagged row = %+v", byTurn["t2"])
	}
}

func TestSpendLsBySessionAndSessionFilter(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	for _, a := range []map[string]any{
		spendAddArgs(map[string]any{"turn-id": "t1"}),
		spendAddArgs(map[string]any{"turn-id": "t2"}),
		spendAddArgs(map[string]any{"turn-id": "t1", "session": "s2", "bucket": []string{"output=9"}}),
	} {
		if r := c.Do(context.Background(), Request{Verb: "spend", Args: a}); !r.OK {
			t.Fatalf("add %+v = %+v", a, r)
		}
	}
	by := c.Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls", "by": "session"}})
	if !by.OK {
		t.Fatalf("by session = %+v", by)
	}
	var data struct {
		Total int    `json:"total"`
		Basis string `json:"basis"`
		Note  string `json:"note"`
		Rows  []struct {
			Session      string `json:"session"`
			TicketStatus string `json:"ticket_status"`
			Turns        int    `json:"turns"`
			FreshInput   *int64 `json:"fresh_input"`
			Output       *int64 `json:"output"`
		} `json:"rows"`
	}
	marshalRoundTrip(t, by.Data, &data)
	if data.Basis != "observed-subtotal" || !strings.Contains(data.Note, "lower bounds") || !strings.Contains(data.Note, "does not say this session did the work") {
		t.Fatalf("observed-subtotal label missing: basis=%q note=%q", data.Basis, data.Note)
	}
	if data.Total != 3 || len(data.Rows) != 2 || data.Rows[0].Session != "s1" || data.Rows[0].Turns != 2 || *data.Rows[0].Output != 8 || data.Rows[0].TicketStatus != "unknown" {
		t.Fatalf("by session rows = %+v total=%d", data.Rows, data.Total)
	}
	if data.Rows[1].Session != "s2" || data.Rows[1].FreshInput != nil || *data.Rows[1].Output != 9 {
		t.Fatalf("s2 row (fresh_input must stay absent) = %+v", data.Rows[1])
	}

	filtered := c.Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls", "session": "s2"}})
	var list struct {
		Total int                   `json:"total"`
		Rows  []domain.ComputeEvent `json:"rows"`
	}
	marshalRoundTrip(t, filtered.Data, &list)
	if !filtered.OK || list.Total != 1 || list.Rows[0].Session != "s2" {
		t.Fatalf("session filter = %+v", filtered)
	}
	both := c.Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls", "session": "s1", "by": "session"}})
	marshalRoundTrip(t, both.Data, &data)
	if !both.OK || len(data.Rows) != 1 || data.Rows[0].Session != "s1" {
		t.Fatalf("session filter with --by session = %+v", both)
	}
	spaced := c.Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls", "session": "a b"}})
	if spaced.OK || spaced.Code != "E_SELECTOR_INVALID" {
		t.Fatalf("whitespace session = ok:%v code:%q", spaced.OK, spaced.Code)
	}
	// An explicitly empty --session is an ambiguous selector: refused, never
	// silently read as "no filter" (which would list every session).
	for _, by := range []string{"", "session"} {
		empty := c.Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls", "session": "", "by": by}})
		if empty.OK || empty.Code != "E_SELECTOR_INVALID" {
			t.Fatalf("empty --session (by=%q) = ok:%v code:%q, want E_SELECTOR_INVALID", by, empty.OK, empty.Code)
		}
	}
	if absent := c.Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls"}}); !absent.OK {
		t.Fatalf("an absent --session must stay valid: %+v", absent)
	}
	if !strings.Contains(data.Note, "older aira") || strings.Contains(data.Note, "never asked for a ticket") {
		t.Fatalf("the unknown definition must cover legacy rows that carry an explicit ticket: %q", data.Note)
	}
}
