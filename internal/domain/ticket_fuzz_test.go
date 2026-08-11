package domain

import "testing"

func FuzzParseTicket(f *testing.F) {
	seeds := []struct {
		ticket Ticket
		body   string
	}{
		{
			ticket: Ticket{ID: "AIRA-1", Project: "aira", Title: "Plan the queue", Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2, Labels: []string{"coordination"}},
			body:   "A rendered ticket body.\n",
		},
		{
			ticket: Ticket{ID: "AIRA-42", Project: "aira", Title: "Fix a flaky test", Status: StatusInProgress, Kind: KindBug, Severity: SeverityP1, Labels: []string{"bug", "test"}},
			body:   "Steps to reproduce\n\n",
		},
	}
	for _, seed := range seeds {
		data, err := RenderTicket(seed.ticket, seed.body)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(data))
	}
	f.Add("garbage")
	f.Add("---\n{}\n---\n")

	f.Fuzz(func(t *testing.T, data string) {
		ticket, body, err := ParseTicket([]byte(data))
		if err != nil {
			return
		}
		canonical, err := RenderTicket(ticket, body)
		if err != nil {
			t.Fatalf("render successful parse: %v", err)
		}
		reparsed, reparsedBody, err := ParseTicket(canonical)
		if err != nil {
			t.Fatalf("parse rendered ticket: %v", err)
		}
		recanonical, err := RenderTicket(reparsed, reparsedBody)
		if err != nil {
			t.Fatalf("re-render parsed ticket: %v", err)
		}
		if string(canonical) != string(recanonical) {
			t.Fatal("ticket did not round-trip through its canonical renderer")
		}
	})
}
