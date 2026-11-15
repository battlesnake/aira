package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/codes"
)

// AIRA-338: every face gets the E_LEASE_EXPIRED detail as response data next
// to the unchanged code, exit and the remedy-bearing error text.
func TestExpiredLeaseResponseCarriesDetailWithUnchangedCode(t *testing.T) {
	s, clock, _ := coreTestStoreWithClock(t)
	c := New(s)
	created := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": "lease detail"}})
	if !created.OK {
		t.Fatalf("create: %#v", created)
	}
	var ticket map[string]any
	marshalRoundTrip(t, created.Data, &ticket)
	id := ticket["id"].(string)
	if claim := c.Do(context.Background(), Request{Verb: "claim", Args: map[string]any{"selector": id, "actor": "holder"}}); !claim.OK {
		t.Fatalf("claim: %#v", claim)
	}
	// A live-lease refusal is a different code and carries no lease detail.
	held := c.Do(context.Background(), Request{Verb: "claim", Args: map[string]any{"selector": id}})
	if held.OK || held.Code != "E_LEASE_HELD" || held.Data != nil {
		t.Fatalf("live claim refusal = %#v", held)
	}
	clock.mono = 100 + 900 + 42

	for _, verb := range []string{"claim", "heartbeat"} {
		response := c.Do(context.Background(), Request{Verb: verb, Args: map[string]any{"selector": id}})
		if response.OK || response.Code != "E_LEASE_EXPIRED" || response.Exit != codes.ExitForCode("E_LEASE_EXPIRED") {
			t.Fatalf("%s response = %#v", verb, response)
		}
		if !strings.HasPrefix(response.Error, "E_LEASE_EXPIRED: ") || !strings.Contains(response.Error, "`aira claim "+id+" --steal`") ||
			!strings.Contains(response.Error, "this worktree") {
			t.Fatalf("%s error text = %q", verb, response.Error)
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Code string         `json:"code"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"ticket_id": id, "lease_state": "expired", "holder_actor": "holder", "holder_worktree_id": "main",
			"holder_is_this_worktree": true, "expired_for_ns": float64(42), "remedy": "aira claim " + id + " --steal",
		}
		if len(wire.Data) != len(want) {
			t.Fatalf("%s data = %v, want %v", verb, wire.Data, want)
		}
		for key, value := range want {
			if wire.Data[key] != value {
				t.Fatalf("%s data[%s] = %#v, want %#v (data %v)", verb, key, wire.Data[key], value, wire.Data)
			}
		}
	}
}

// AIRA-338: the generated help (dispatch-table summaries) names the expiry
// remedy for both lease verbs, not only the --steal argument description.
func TestLeaseVerbHelpNamesExpiryRemedy(t *testing.T) {
	found := map[string]string{}
	for _, entry := range New(nil).Help() {
		if entry["verb"] == "claim" || entry["verb"] == "heartbeat" {
			found[entry["verb"]] = entry["summary"]
		}
	}
	if s := found["claim"]; !strings.Contains(s, "expired") || !strings.Contains(s, "--steal") || !strings.Contains(s, "this worktree") {
		t.Fatalf("claim help summary = %q", s)
	}
	if s := found["heartbeat"]; !strings.Contains(s, "E_LEASE_EXPIRED") || !strings.Contains(s, "claim --steal") {
		t.Fatalf("heartbeat help summary = %q", s)
	}
}
