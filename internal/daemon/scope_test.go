package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/store"
)

func TestScopeFromProjectMatchesLegacyClientProjection(t *testing.T) {
	tests := []struct {
		name    string
		project app.Project
	}{
		{
			name: "full config",
			project: app.Project{
				Root: "/repo", CommonDir: "/repo/.git", GitDir: "/repo/.git/worktrees/main",
				ProjectID: "project", WorktreeID: "worktree",
				Config: app.Config{
					Schema: 1,
					Project: app.ProjectConfig{
						Slug: "demo", Prefixes: []string{"AIR", "BUG"}, RequirementPrefixes: []string{"REQ"},
						Review:      json.RawMessage(`{"default_tier":4}`),
						TestReports: app.TestReportsConfig{MaxReports: 7, MaxAgeDays: 9},
						Compute:     app.ComputeConfig{MaxEvents: 11, MaxAgeDays: 13, MaxQuotaSnapshots: 17},
						Commands:    app.CommandsConfig{MaxEvents: 19, MaxAgeDays: 23},
					},
					Lease: app.LeaseConfig{TTLSeconds: 29},
				},
			},
		},
		{
			name: "defaults and nonpositive lease",
			project: app.Project{
				Root: "/other", CommonDir: "/common", GitDir: "/git", ProjectID: "p", WorktreeID: "w",
				Config: app.Config{Project: app.ProjectConfig{Slug: "minimal", Prefixes: []string{"MIN"}}, Lease: app.LeaseConfig{TTLSeconds: -1}},
			},
		},
	}
	paths := Paths{StateID: "state-id"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want, wantErr := legacyClientScopeFromProject(test.project, paths)
			got, gotErr := ScopeFromProject(test.project, paths)
			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("error=%v, want %v", gotErr, wantErr)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scope mismatch\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

func TestScopeFromProjectPreservesReviewValidationError(t *testing.T) {
	project := app.Project{Config: app.Config{Project: app.ProjectConfig{Review: json.RawMessage(`null`)}}}
	if _, err := ScopeFromProject(project, Paths{}); err == nil || err.Error() != "E_CONFIG_INVALID: review must be an object" {
		t.Fatalf("error=%v", err)
	}
}

// legacyClientScopeFromProject is the pre-move cmd/aira projection kept here as
// a golden oracle so moving the shared builder cannot alter routed requests.
func legacyClientScopeFromProject(project app.Project, paths Paths) (WorktreeScope, error) {
	reviewPolicy, err := store.LoadReviewPolicy(project.Config.Project.Review)
	if err != nil {
		return WorktreeScope{}, err
	}
	configBytes, err := json.Marshal(project.Config)
	if err != nil {
		return WorktreeScope{}, err
	}
	digest := sha256.Sum256(configBytes)
	leaseTTL := uint64(0)
	if project.Config.Lease.TTLSeconds > 0 {
		leaseTTL = uint64(project.Config.Lease.TTLSeconds) * uint64(time.Second)
	}
	return WorktreeScope{
		Root: project.Root, CommonDir: project.CommonDir, GitDir: project.GitDir,
		ProjectID: project.ProjectID, WorktreeID: project.WorktreeID,
		Slug: project.Config.Project.Slug, Prefixes: project.Config.Project.Prefixes,
		RequirementPrefixes: project.Config.Project.RequirementPrefixes, ReviewPolicy: reviewPolicy,
		ReviewConfigured: reviewPolicy.Configured,
		MaxReports:       project.Config.Project.TestReports.MaxReports, MaxAgeDays: project.Config.Project.TestReports.MaxAgeDays,
		MaxComputeEvents: project.Config.Project.Compute.MaxEvents, MaxComputeAgeDays: project.Config.Project.Compute.MaxAgeDays,
		MaxCommandEvents: project.Config.Project.Commands.MaxEvents, MaxCommandAgeDays: project.Config.Project.Commands.MaxAgeDays,
		MaxQuotaSnapshots: project.Config.Project.Compute.MaxQuotaSnapshots,
		LeaseTTLNS:        leaseTTL, ConfigDigest: hex.EncodeToString(digest[:]), StateID: paths.StateID,
	}, nil
}
