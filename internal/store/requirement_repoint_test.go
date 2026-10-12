// verifies: AIRA-297
//
// Rebuild re-points a still-`allocated` allocation row at the scanned entity
// file, for requirements as it has for tickets since AIRA-175, through one code
// path; a materialised row is never moved.

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
)

func requirementStoreSharing(t *testing.T, base, worktreeID string) *Store {
	t.Helper()
	root := filepath.Join(base, worktreeID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"),
		DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"),
		ProjectID: "project-aira", WorktreeID: worktreeID, ProjectSlug: "aira",
		Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"AR"},
	})
	if err != nil {
		t.Fatalf("open sharing store %s: %v", worktreeID, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeRequirementFile(t *testing.T, root, id string) string {
	t.Helper()
	valid, err := domain.NewRequirement(domain.RequirementInput{ID: id, Text: "Resolves from any worktree.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(valid)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".aira", "requirements", id+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func allocationPathWorktree(t *testing.T, s *Store, prefix string, number int64) (path, worktree, state string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT path, worktree_id, state FROM allocations WHERE project_id=? AND prefix=? AND number=?`,
		s.projectID, prefix, number).Scan(&path, &worktree, &state); err != nil {
		t.Fatalf("allocation %s-%d: %v", prefix, number, err)
	}
	return path, worktree, state
}

// The requirement mirror of the AIRA-175 pin
// (TestCheckResolvesAnAllocatedIDFromTheCheckingWorktreeAfterTheRecordedOneIsRemoved).
func TestCheckResolvesAnAllocatedRequirementFromTheCheckingWorktree(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	a := requirementStoreSharing(t, base, "wt-a")
	c := requirementStoreSharing(t, base, "wt-c")
	if id, err := a.AllocateID(ctx, "AR"); err != nil || id != "AR-1" {
		t.Fatalf("AllocateID(AR) = %q, %v", id, err)
	}
	cFile := writeRequirementFile(t, c.root, "AR-1")

	report, err := c.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, f := range report.Findings {
		if f.Code == "E_ID_UNRESOLVED" {
			t.Fatalf("Check reports E_ID_UNRESOLVED for a requirement file present in the checking worktree: %+v", f)
		}
	}
	if got := report.Dimensions["allocated-id-file"]; got != "pass" {
		t.Fatalf("allocated-id-file = %q, want pass: %v", got, report.Dimensions)
	}
	if path, worktree, _ := allocationPathWorktree(t, c, "AR", 1); path != cFile || worktree != "wt-c" {
		t.Fatalf("allocation after Check = %q in %q, want re-pointed to %q in wt-c", path, worktree, cFile)
	}
}

// A materialised (or retired) allocation is never moved by a copy of its file in
// another worktree, for either entity kind.
func TestRebuildNeverRepointsAMaterialisedAllocation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		prefix string
		make   func(t *testing.T, a *Store) string
		copyTo func(t *testing.T, a, c *Store, id string)
	}{
		{
			name: "ticket", prefix: "AIRA",
			make: func(t *testing.T, a *Store) string {
				ticket, err := a.CreateTicket(ctx, domain.CreateTicketInput{Title: "materialised"})
				if err != nil {
					t.Fatal(err)
				}
				return ticket.ID
			},
			copyTo: func(t *testing.T, a, c *Store, id string) {
				data, err := os.ReadFile(a.ticketPath(id))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(c.ticketPath(id)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.ticketPath(id), data, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "requirement", prefix: "AR",
			make: func(t *testing.T, a *Store) string {
				req, _, err := a.AddRequirement(ctx, domain.RequirementInput{Text: "materialised requirement", Status: domain.RequirementBuilt})
				if err != nil {
					t.Fatal(err)
				}
				return req.ID
			},
			copyTo: func(t *testing.T, a, c *Store, id string) {
				data, err := os.ReadFile(a.requirementPath(id))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(c.requirementPath(id)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.requirementPath(id), data, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			a := requirementStoreSharing(t, base, "wt-a")
			c := requirementStoreSharing(t, base, "wt-c")
			id := tc.make(t, a)
			_, number, _ := splitTicketID(id)
			pathBefore, worktreeBefore, state := allocationPathWorktree(t, a, tc.prefix, int64(number))
			if state != "materialised" || worktreeBefore != "wt-a" {
				t.Fatalf("fixture %s: state=%s worktree=%s", id, state, worktreeBefore)
			}
			tc.copyTo(t, a, c, id)
			if err := c.Rebuild(ctx); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			if path, worktree, _ := allocationPathWorktree(t, c, tc.prefix, int64(number)); path != pathBefore || worktree != "wt-a" {
				t.Fatalf("materialised %s moved to %q in %q, want %q in wt-a", id, path, worktree, pathBefore)
			}
		})
	}
}
