package main

import (
	"context"
	"path/filepath"
	"strings"

	"aira/internal/app"
	"aira/internal/daemon"
	"aira/internal/store"
)

// daemonStoreOpRelay is the detached supervisor's transport to the DB-owning
// daemon. It deliberately does NOT reuse daemonDispatcher's exchangeOrStart /
// exchangeWithReplacement wrappers: a background supervisor must never start or
// replace the machine-wide daemon on its own (AIRA-83 removed exactly that
// class of implicit, shared-blast-radius action), so this dials an
// already-listening socket and nothing more.
//
// An empty socket path yields a nil relay, which writeRelayStore.exchange
// reports as E_DAEMON_UNAVAILABLE. That is the honest outcome: the write did
// not happen, the wiring is marked incomplete, and nothing silently falls back
// to writing state.db directly.
func daemonStoreOpRelay(socket string) storeOpRelay {
	if strings.TrimSpace(socket) == "" {
		return nil
	}
	return func(ctx context.Context, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		return daemon.ExchangeStoreOp(ctx, socket, frame)
	}
}

// supervisorTelemetryStore is the production construction runSupervisor uses:
// the read-only project view plus the relay to the daemon named by paths. A
// zero Paths (daemon.PathsFromEnv failed) yields the nil relay above rather than
// a direct writer.
func supervisorTelemetryStore(project app.Project, paths daemon.Paths) (*writeRelayStore, error) {
	return openSupervisorRelayStore(project, paths, daemonStoreOpRelay(paths.SocketPath))
}

// openSupervisorRelayStore builds the detached supervisor's ONLY state.db
// handle: a read-only view of the machine database whose every write is relayed
// to the daemon, exactly as the CLI's client-routed dispatch already does
// (daemonDispatcher.dispatchClient). The relay is a parameter so tests can
// observe the frames; production always goes through supervisorTelemetryStore.
//
// covers: AIRA-85. Before this, `aira supervisor` opened a read-WRITE handle via
// app.OpenWithDiagnostics and appended a terminal detached run's test report and
// compute event straight to SQLite (internal/core/run_wiring.go AddTestReport /
// AddComputeEvent) — a third production writer beside the daemon and the CLI
// relay, invisible to the daemon that is meant to own the file. Opening
// mode=ro/query_only means any writer this relay does not override now fails
// loudly against SQLite instead of quietly becoming a writer again.
//
// KNOWN, ACCEPTED STRUCTURAL GAP (AIRA-85, recorded not closed): nothing
// *enforces* single-writer beyond this convention. Any future code path may
// still call store.Open and write directly. A mechanical "no store.Open outside
// internal/daemon" lint was considered and dropped: it could not have caught
// this defect (the supervisor opened the DB correctly; it wrote afterwards),
// and the daemon itself opens via store.OpenDB. The residual guard is code
// review, not a test. A runtime single-writer assertion remains a follow-up,
// out of this ticket's scope.
func openSupervisorRelayStore(project app.Project, paths daemon.Paths, relay storeOpRelay) (*writeRelayStore, error) {
	scope, err := daemon.ScopeFromProject(project, paths)
	if err != nil {
		return nil, err
	}
	readScope := scope
	readScope.ReviewPolicy.Configured = readScope.ReviewConfigured
	readOnly, err := store.OpenReadOnly(filepath.Join(project.StateDir, "state.db"), store.ScopeOptions{
		Root: readScope.Root, CommonDir: readScope.CommonDir, GitDir: readScope.GitDir,
		ProjectID: readScope.ProjectID, WorktreeID: readScope.WorktreeID, ProjectSlug: readScope.Slug,
		Prefixes: readScope.Prefixes, RequirementPrefixes: readScope.RequirementPrefixes, ReviewPolicy: readScope.ReviewPolicy,
		LeaseTTLNS: readScope.LeaseTTLNS, MaxReports: readScope.MaxReports, MaxAgeDays: readScope.MaxAgeDays,
		MaxComputeEvents: readScope.MaxComputeEvents, MaxComputeAgeDays: readScope.MaxComputeAgeDays,
		MaxCommandEvents: readScope.MaxCommandEvents, MaxCommandAgeDays: readScope.MaxCommandAgeDays,
		MaxQuotaSnapshots: readScope.MaxQuotaSnapshots, ConfigDigest: readScope.ConfigDigest,
	})
	if err != nil {
		return nil, err
	}
	return newWriteRelayStore(readOnly, scope, relay), nil
}
