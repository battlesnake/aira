// verifies: AIRA-290, AIRA-291

package store

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aira/internal/domain"
)

// v034Counts reads the write-side evidence a refused link must leave untouched.
func v034Counts(t *testing.T, s *Store) (outbox, events int) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return outbox, events
}

var v034AdviceRE = regexp.MustCompile(`aira (link|unlink) (\S+) (\S+) (\S+)`)

var v034InverseToForward = []struct {
	inverse, forward domain.RelationKind
}{
	{domain.RelationBlockedBy, domain.RelationBlocks},
	{domain.RelationChild, domain.RelationParent},
	{domain.RelationDuplicatedBy, domain.RelationDuplicates},
	{domain.RelationSupersededBy, domain.RelationSupersedes},
	{domain.RelationResolvedBy, domain.RelationResolves},
}

// verifies: AIRA-290
func TestLinkInverseKindNamesTheForwardCommand(t *testing.T) {
	for _, row := range v034InverseToForward {
		for _, verb := range []string{"link", "unlink"} {
			t.Run(string(row.inverse)+"/"+verb, func(t *testing.T) {
				s, _, _ := m3Store(t)
				a := m3Ticket(t, s, "a")
				b := m3Ticket(t, s, "b")
				ctx := context.Background()
				if verb == "unlink" {
					// A real forward relation exists, so a wrong refusal path
					// would be tempted to act on it.
					if _, err := s.Link(ctx, b.ID, row.forward, a.ID); err != nil {
						t.Fatal(err)
					}
				}
				digest := func() string {
					data, err := os.ReadFile(filepath.Join(s.root, ".aira", "tickets", a.ID+".md"))
					if err != nil {
						t.Fatal(err)
					}
					return string(data)
				}
				beforeFile := digest()
				beforeOutbox, beforeEvents := v034Counts(t, s)
				var err error
				if verb == "link" {
					_, err = s.Link(ctx, a.ID, row.inverse, b.ID)
				} else {
					_, err = s.Unlink(ctx, a.ID, row.inverse, b.ID)
				}
				if ErrorCode(err) != "E_RELATION_INVALID" {
					t.Fatalf("code = %q (err=%v), want E_RELATION_INVALID", ErrorCode(err), err)
				}
				want := "aira " + verb + " " + b.ID + " " + string(row.forward) + " " + a.ID
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err.Error(), want)
				}
				if afterOutbox, afterEvents := v034Counts(t, s); afterOutbox != beforeOutbox || afterEvents != beforeEvents {
					t.Fatalf("refusal wrote: outbox %d->%d events %d->%d", beforeOutbox, afterOutbox, beforeEvents, afterEvents)
				}
				if digest() != beforeFile {
					t.Fatal("refusal changed the owner ticket file")
				}
			})
		}
	}
}

// verifies: AIRA-290
func TestLinkInverseRefusalAdviceWorks(t *testing.T) {
	for _, row := range v034InverseToForward {
		t.Run(string(row.inverse), func(t *testing.T) {
			s, _, _ := m3Store(t)
			a := m3Ticket(t, s, "a")
			b := m3Ticket(t, s, "b")
			ctx := context.Background()
			_, err := s.Link(ctx, a.ID, row.inverse, b.ID)
			m := v034AdviceRE.FindStringSubmatch(err.Error())
			if m == nil || m[1] != "link" {
				t.Fatalf("no link command in refusal %q", err.Error())
			}
			if _, err := s.Link(ctx, m[2], domain.RelationKind(m[3]), m[4]); err != nil {
				t.Fatalf("the advised command failed: %v", err)
			}
			views, err := s.Relations(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(views) != 1 || views[0].Kind != row.inverse || views[0].From != a.ID || views[0].To != b.ID {
				t.Fatalf("advised command stored the wrong relation: %#v", views)
			}
			// The unlink advice must take the same relation away again.
			_, err = s.Unlink(ctx, a.ID, row.inverse, b.ID)
			m = v034AdviceRE.FindStringSubmatch(err.Error())
			if m == nil || m[1] != "unlink" {
				t.Fatalf("no unlink command in refusal %q", err.Error())
			}
			if _, err := s.Unlink(ctx, m[2], domain.RelationKind(m[3]), m[4]); err != nil {
				t.Fatalf("the advised unlink failed: %v", err)
			}
			if views, err := s.Relations(a.ID); err != nil || len(views) != 0 {
				t.Fatalf("advised unlink left %#v err=%v", views, err)
			}
		})
	}
}

// verifies: AIRA-290
func TestLinkUnknownKindListsWritableKinds(t *testing.T) {
	s, _, _ := m3Store(t)
	a := m3Ticket(t, s, "a")
	b := m3Ticket(t, s, "b")
	for _, verb := range []string{"link", "unlink"} {
		var err error
		if verb == "link" {
			_, err = s.Link(context.Background(), a.ID, "bogus", b.ID)
		} else {
			_, err = s.Unlink(context.Background(), a.ID, "bogus", b.ID)
		}
		if ErrorCode(err) != "E_RELATION_INVALID" {
			t.Fatalf("%s code = %q (%v)", verb, ErrorCode(err), err)
		}
		for _, want := range []string{`"bogus"`, "blocks, duplicates, parent, relates, resolves, supersedes"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s refusal %q lacks %q", verb, err.Error(), want)
			}
		}
		if strings.Contains(err.Error(), "aira link") || strings.Contains(err.Error(), "aira unlink") {
			t.Fatalf("%s: unknown kind must not offer a command: %q", verb, err.Error())
		}
	}
}

// v034DivergedStore returns a store in the git-merge shape: A's file carries
// `A blocks B`, but the derived relation index has no row for it.
func v034DivergedStore(t *testing.T) (s *Store, a, b domain.Ticket) {
	t.Helper()
	s, _, _ = m3Store(t)
	a = m3Ticket(t, s, "blocker")
	b = m3Ticket(t, s, "blocked")
	ctx := context.Background()
	if _, err := s.Link(ctx, a.ID, domain.RelationBlocks, b.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, ".aira", "tickets", a.ID+".md")
	withRelation, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unlink(ctx, a.ID, domain.RelationBlocks, b.ID); err != nil {
		t.Fatal(err)
	}
	// What `git merge` does: the file changes with no aira write behind it.
	if err := os.WriteFile(path, withRelation, 0o644); err != nil {
		t.Fatal(err)
	}
	return s, a, b
}

func v034Finding(findings []CheckFinding, code string) *CheckFinding {
	for i := range findings {
		if findings[i].Code == code {
			return &findings[i]
		}
	}
	return nil
}

// verifies: AIRA-291
func TestReadyBlocksOnARelationTheIndexHasNotSeen(t *testing.T) {
	s, a, b := v034DivergedStore(t)
	rows, err := s.Ready("")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ReadyRecord{}
	for _, row := range rows {
		byID[row.Ticket.Ticket.ID] = row
	}
	blocked, ok := byID[b.ID]
	if !ok {
		t.Fatalf("no row for %s: %#v", b.ID, rows)
	}
	// False-pass direction: the index lacks the edge, the file has it, and the
	// file wins.
	if blocked.Ready || len(blocked.Blockers) != 1 || blocked.Blockers[0].Kind != domain.RelationBlockedBy ||
		blocked.Blockers[0].From != b.ID || blocked.Blockers[0].To != a.ID {
		t.Fatalf("a relation the index has not seen did not block: %#v", blocked)
	}
	if blocked.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass (the warning must not change the verdict)", blocked.Verdict)
	}
	finding := v034Finding(blocked.Findings, "E_RELATION_INDEX_DIVERGENCE")
	if finding == nil || finding.Kind != "warning" {
		t.Fatalf("divergence not surfaced as a warning: %#v", blocked.Findings)
	}
	if !strings.Contains(finding.Message, "computed relation blockers from the ticket files") {
		t.Fatalf("ready warning does not say it changes no blocker: %q", finding.Message)
	}
	if !byID[a.ID].Ready {
		t.Fatalf("the blocker itself must stay ready: %#v", byID[a.ID])
	}
	// `check` rebuilds the index; the file still blocks, and the warning is gone.
	if _, err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Ready(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Ready || v034Finding(rows[0].Findings, "E_RELATION_INDEX_DIVERGENCE") != nil {
		t.Fatalf("after check: %#v", rows)
	}
}

// verifies: AIRA-291
func TestCheckDivergenceMessageNamesRebuild(t *testing.T) {
	s, _, _ := v034DivergedStore(t)
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finding := v034Finding(report.Findings, "E_RELATION_INDEX_DIVERGENCE")
	if finding == nil {
		t.Fatalf("check reported no divergence: %#v", report.Findings)
	}
	if !strings.Contains(finding.Message, "`aira reconcile --rebuild`") {
		t.Fatalf("check message does not name the rebuild command: %q", finding.Message)
	}
	if strings.Contains(finding.Message, "computed relation blockers") {
		t.Fatalf("check message carries the ready-only clause: %q", finding.Message)
	}
}

// verifies: AIRA-291
func TestSequentialLinksLeaveNoIndexDivergence(t *testing.T) {
	s, _, _ := m3Store(t)
	ids := make([]string, 6)
	for i := range ids {
		ids[i] = m3Ticket(t, s, "t").ID
	}
	ctx := context.Background()
	links := [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}}
	for _, l := range links {
		if _, err := s.Link(ctx, ids[l[0]], domain.RelationBlocks, ids[l[1]]); err != nil {
			t.Fatal(err)
		}
	}
	for _, to := range []int{2, 3, 4, 5} {
		if _, err := s.Link(ctx, ids[0], domain.RelationRelates, ids[to]); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Ready("")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if f := v034Finding(row.Findings, "E_RELATION_INDEX_DIVERGENCE"); f != nil {
			t.Fatalf("ready found a divergence after sequential links: %#v", f)
		}
	}
	// Check rebuilds on the way; read the divergence verdict it reported first.
	report, err := s.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["relation-integrity"] != "pass" || v034Finding(report.Findings, "E_RELATION_INDEX_DIVERGENCE") != nil {
		t.Fatalf("relation-integrity = %q findings=%#v", report.Dimensions["relation-integrity"], report.Findings)
	}
}
