package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"aira/internal/app"
	"aira/internal/store"
)

// ScopeFromProject projects read-only application discovery into the exact
// worktree descriptor accepted by daemon scope construction.
func ScopeFromProject(project app.Project, paths Paths) (WorktreeScope, error) {
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
