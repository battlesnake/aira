package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/codes"
	"aira/internal/domain"
	"aira/internal/store"
)

// verifies: AIRA-107 — a code that only ever travels as a CheckFinding must be
// catalogued at the exit `aira check` actually produces for it.
//
// The two index-divergence codes are the whole reason this test exists. Neither
// is ever raised as an error and neither is ever assigned to Response.Code:
// every emission is a CheckFinding (store/finding.go:624-645,
// store/relation_ready.go:402/408). So the catalogue has no freedom about their
// bucket — `check` takes its exit from the report verdict via exitCode, not from
// any finding's code, and a catalogue entry that disagrees publishes, through
// ResponseContract and the Skill and agent-guide artifacts generated from it, an
// exit no face can emit. An earlier cut of AIRA-107 moved both to 4 on a
// family-resemblance argument ("index-vs-truth divergence is store integrity")
// without checking that side, which is exactly the false-contract this catches:
// against that cut, ResponseContract().ExitCodes[code] was 4 while this test's
// real `check` call exited 1.
//
// The divergences below are real, not hand-built reports: one drops a relation
// from a canonical ticket file so the derived relation index outlives it, the
// other deletes a canonical finding file so the derived finding row does. That
// matters — a hand-assembled CheckReport would only re-assert exitCode's own
// switch, whereas these prove the condition really does reach a caller as an
// evaluated failing check, which is what phase-1 spec §8 assigns exit 1 ("at
// least one selected check is fail") and pointedly not exit 4 ("store/
// reconciliation failed BEFORE the requested checks could be evaluated").
func TestFindingOnlyCodesExitAsTheirCheckVerdictDoes(t *testing.T) {
	t.Run("E_RELATION_INDEX_DIVERGENCE", func(t *testing.T) {
		s, base := coreTestStoreWithRoot(t)
		c := New(s)
		owner := createExitContractTicket(t, c, "relation owner")
		dependent := createExitContractTicket(t, c, "dependent")
		if response := c.Do(context.Background(), Request{Verb: "link", Args: map[string]any{
			"from": owner, "kind": "blocks", "to": dependent,
		}}); !response.OK {
			t.Fatalf("link: %#v", response)
		}

		// Drop the relation from the canonical file. The derived index still
		// carries it, which is the divergence check reports.
		ownerPath := filepath.Join(base, ".aira", "tickets", owner+".md")
		data, err := os.ReadFile(ownerPath)
		if err != nil {
			t.Fatal(err)
		}
		ticket, _, err := domain.ParseTicket(data)
		if err != nil {
			t.Fatal(err)
		}
		ticket.Relations = nil
		writeCoreTicketFile(t, ownerPath, ticket)

		assertFindingOnlyCodeExitMatchesCatalogue(t, c, "E_RELATION_INDEX_DIVERGENCE")
	})

	t.Run("E_FINDING_INDEX_DIVERGENCE", func(t *testing.T) {
		s, base := coreTestStoreWithRoot(t)
		c := New(s)
		added := c.Do(context.Background(), Request{Verb: "find", Args: map[string]any{
			"subverb": "add", "ticket": "AIRA-1", "category": "flaky-test", "severity": "P1",
			"verdict": "confirmed", "source": "codex", "message": "divergence probe",
			"file": "worker.go", "line": 7,
		}})
		if !added.OK {
			t.Fatalf("find add: %#v", added)
		}

		// Delete the canonical file. The derived finding row outlives it, which
		// is the divergence check reports.
		files, err := filepath.Glob(filepath.Join(base, ".aira", "findings", "*.md"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Fatalf("expected exactly one canonical finding file, got %v", files)
		}
		if err := os.Remove(files[0]); err != nil {
			t.Fatal(err)
		}

		assertFindingOnlyCodeExitMatchesCatalogue(t, c, "E_FINDING_INDEX_DIVERGENCE")
	})
}

// assertFindingOnlyCodeExitMatchesCatalogue runs the real check verb and holds
// the three facts together: the divergence is reported as an evaluated failing
// finding carrying `code`, the face exits 1 for it, and the published contract
// says the same number. Asserting the literal 1 as well as the equality is
// deliberate — without it the test would pass if a future edit moved BOTH the
// catalogue entry and exitCode's fail arm to some third value, which would be a
// spec-§8 change dressed up as a code re-bucketing.
func assertFindingOnlyCodeExitMatchesCatalogue(t *testing.T, c *Core, code string) {
	t.Helper()
	response := c.Do(context.Background(), Request{Verb: "check"})
	var report store.CheckReport
	marshalRoundTrip(t, response.Data, &report)

	found := false
	for _, finding := range report.Findings {
		if finding.Code == code {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s was not reported as an evaluated failing finding, so this test proves nothing about its exit: report=%#v", code, report)
	}
	if report.Verdict != "fail" {
		t.Fatalf("%s did not drive a fail verdict: verdict=%q", code, report.Verdict)
	}
	if response.Exit != 1 {
		t.Fatalf("check reporting %s exited %d, want 1 (phase-1 spec §8: a failing check is exit 1)", code, response.Exit)
	}
	if published := codes.ExitForCode(code); published != response.Exit {
		t.Errorf("catalogue publishes exit %d for %s but the check face exited %d; a finding-only code cannot pick its own bucket, because check derives its exit from the report verdict", published, code, response.Exit)
	}
	if published := ResponseContract().ExitCodes[code]; published != response.Exit {
		t.Errorf("ResponseContract publishes exit %d for %s but the check face exited %d; this is what the generated Skill and agent-guide artifacts show agents", published, code, response.Exit)
	}
}

func createExitContractTicket(t *testing.T, c *Core, title string) string {
	t.Helper()
	response := c.Do(context.Background(), Request{Verb: "create", Args: map[string]any{"title": title}})
	if !response.OK {
		t.Fatalf("create %s: %#v", title, response)
	}
	var data map[string]any
	marshalRoundTrip(t, response.Data, &data)
	id, ok := data["id"].(string)
	if !ok || id == "" {
		t.Fatalf("create %s returned no id: %#v", title, data)
	}
	return id
}
