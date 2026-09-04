package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ProjectRegistration struct {
	ProjectID    string
	Slug         string
	CommonDir    string
	ConfigDigest string
	Prefixes     []ProjectPrefix
	Worktrees    []ProjectWorktree
}

type ProjectPrefix struct {
	Prefix string `json:"prefix"`
	Kind   string `json:"kind"`
}

type ProjectWorktree struct {
	WorktreeID string `json:"worktree_id"`
	Root       string `json:"root"`
}

type EjectResult struct {
	ProjectID          string         `json:"project_id"`
	PrefixesReleased   []string       `json:"prefixes_released"`
	RowsDropped        map[string]int `json:"rows_dropped"`
	TelemetryDiscarded map[string]int `json:"telemetry_discarded"`
	Files              string         `json:"files"`
}

func (db *DB) ResolveProject(ctx context.Context, projectSelector, prefix string) (ProjectRegistration, error) {
	if db == nil || db.db == nil {
		return ProjectRegistration{}, errors.New("E_INTERNAL: state database unavailable")
	}
	if projectSelector != "" && prefix != "" {
		return ProjectRegistration{}, errors.New("E_SELECTOR_AMBIGUOUS: choose exactly one of project or prefix")
	}
	var ids []string
	if prefix != "" {
		var id string
		err := db.db.QueryRowContext(ctx, `SELECT project_id FROM prefix_ownership WHERE prefix=?`, strings.ToUpper(prefix)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectRegistration{}, fmt.Errorf("E_NOT_ADOPTED: no project owns prefix %s", strings.ToUpper(prefix))
		}
		if err != nil {
			return ProjectRegistration{}, translateDBError(err)
		}
		ids = []string{id}
	} else {
		selector := strings.TrimSpace(projectSelector)
		if selector == "" {
			return ProjectRegistration{}, errors.New("E_NO_PROJECT: no project selector or current .aira/config")
		}
		rows, err := db.db.QueryContext(ctx, `SELECT project_id FROM projects WHERE project_id=? OR project_id LIKE ? ORDER BY project_id`, selector, selector+"%")
		if err != nil {
			return ProjectRegistration{}, translateDBError(err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return ProjectRegistration{}, err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return ProjectRegistration{}, err
		}
		if len(ids) == 0 {
			return ProjectRegistration{}, fmt.Errorf("E_NOT_ADOPTED: project %s is not adopted", selector)
		}
		if len(ids) > 1 {
			return ProjectRegistration{}, fmt.Errorf("E_SELECTOR_AMBIGUOUS: project selector %s matches %s", selector, strings.Join(ids, ", "))
		}
	}
	return db.projectRegistration(ctx, ids[0])
}

func (db *DB) projectRegistration(ctx context.Context, projectID string) (ProjectRegistration, error) {
	var result ProjectRegistration
	err := db.db.QueryRowContext(ctx, `SELECT project_id,slug,common_dir,config_digest FROM projects WHERE project_id=?`, projectID).Scan(
		&result.ProjectID, &result.Slug, &result.CommonDir, &result.ConfigDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectRegistration{}, fmt.Errorf("E_NOT_ADOPTED: project %s is not adopted", projectID)
	}
	if err != nil {
		return ProjectRegistration{}, translateDBError(err)
	}
	rows, err := db.db.QueryContext(ctx, `SELECT prefix,kind FROM prefix_ownership WHERE project_id=? ORDER BY prefix`, projectID)
	if err != nil {
		return ProjectRegistration{}, translateDBError(err)
	}
	for rows.Next() {
		var item ProjectPrefix
		if err := rows.Scan(&item.Prefix, &item.Kind); err != nil {
			_ = rows.Close()
			return ProjectRegistration{}, err
		}
		result.Prefixes = append(result.Prefixes, item)
	}
	if err := rows.Close(); err != nil {
		return ProjectRegistration{}, err
	}
	rows, err = db.db.QueryContext(ctx, `SELECT worktree_id,root FROM worktrees WHERE project_id=? AND active=1 ORDER BY worktree_id`, projectID)
	if err != nil {
		return ProjectRegistration{}, translateDBError(err)
	}
	for rows.Next() {
		var item ProjectWorktree
		if err := rows.Scan(&item.WorktreeID, &item.Root); err != nil {
			_ = rows.Close()
			return ProjectRegistration{}, err
		}
		result.Worktrees = append(result.Worktrees, item)
	}
	if err := rows.Close(); err != nil {
		return ProjectRegistration{}, err
	}
	return result, nil
}

// ProjectRegistration returns the daemon's current registration snapshot for
// an exact project ID. Lifecycle callers use it only while holding their
// in-memory exclusion, after all in-flight registration paths have drained.
func (db *DB) ProjectRegistration(ctx context.Context, projectID string) (ProjectRegistration, error) {
	return db.projectRegistration(ctx, projectID)
}

func (db *DB) ProjectEjected(ctx context.Context, projectID string) (bool, error) {
	var one int
	err := db.db.QueryRowContext(ctx, `SELECT 1 FROM ejections WHERE project_id=?`, projectID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, translateDBError(err)
	}
	return true, nil
}

// PreflightAdoption validates every requested prefix before staging any rows.
func (s *Store) PreflightAdoption(ctx context.Context) error {
	var existing int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE project_id=?`, s.projectID).Scan(&existing); err != nil {
		return translateDBError(err)
	}
	if existing != 0 {
		return fmt.Errorf("E_ALREADY_INITIALIZED: project %s is already adopted", s.projectID)
	}
	for prefix := range s.prefixes {
		var owner string
		err := s.db.QueryRowContext(ctx, `SELECT project_id FROM prefix_ownership WHERE prefix=?`, prefix).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || err == nil && owner == s.projectID {
			continue
		}
		if err != nil {
			return translateDBError(err)
		}
		var root string
		_ = s.db.QueryRowContext(ctx, `SELECT root FROM worktrees WHERE project_id=? AND active=1 ORDER BY updated_at DESC LIMIT 1`, owner).Scan(&root)
		return fmt.Errorf("E_PREFIX_OWNERSHIP_CONFLICT: %s owned by project %s at %s; run aira eject --project %s", prefix, owner, root, owner)
	}
	return nil
}

// StageAdoption creates only the projects parent and current worktree needed by
// FK-enforced Rebuild. It claims no prefix, clears no tombstone, and writes no
// breadcrumb; RollbackStagedAdoption therefore restores the externally visible
// pre-adoption state after a failed rebuild.
func (s *Store) StageAdoption(ctx context.Context) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `INSERT INTO projects(project_id,slug,common_dir,config_digest,created_at) VALUES (?,?,?,?,?)`, s.projectID, s.projectSlug, s.commonDir, s.configDigest, now); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO worktrees(project_id,worktree_id,root,active,updated_at) VALUES (?,?,?,1,?)`, s.projectID, s.worktreeID, s.root, now)
		return err
	})
}

func (s *Store) RollbackStagedAdoption(ctx context.Context) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		var claimed int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM prefix_ownership WHERE project_id=?`, s.projectID).Scan(&claimed); err != nil {
			return err
		}
		if claimed != 0 {
			return errors.New("E_INTERNAL: cannot roll back a claimed adoption")
		}
		_, err := conn.ExecContext(ctx, `DELETE FROM projects WHERE project_id=?`, s.projectID)
		return err
	})
}

func (db *DB) EjectLiveHolders(ctx context.Context, projectID string) ([]string, error) {
	bootID, monoNS, err := (systemClock{}).Now()
	if err != nil {
		return nil, fmt.Errorf("E_EJECT_UNVERIFIED: live-state clock unavailable: %w", err)
	}
	return ejectLiveHolders(ctx, db.db, projectID, bootID, monoNS)
}

func ejectLiveHolders(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectID, bootID string, monoNS uint64) ([]string, error) {
	var holders []string
	rows, err := q.QueryContext(ctx, `SELECT ticket_id,actor,worktree_id,boot_id,last_heartbeat_mono_ns,ttl_ns FROM leases WHERE project_id=? AND state='held' ORDER BY ticket_id`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ticket, actor, worktree, rowBoot string
		var last, ttl int64
		if err := rows.Scan(&ticket, &actor, &worktree, &rowBoot, &last, &ttl); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if last < 0 || ttl <= 0 {
			_ = rows.Close()
			return nil, errors.New("E_EJECT_UNVERIFIED: malformed ticket lease")
		}
		live := rowBoot == bootID && (monoNS < uint64(last) || monoNS-uint64(last) < uint64(ttl))
		if live {
			holders = append(holders, fmt.Sprintf("ticket %s held by %s (%s)", ticket, actor, worktree))
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT run_id,actor,worktree_id,holder_pid,holder_boot_id,last_heartbeat_mono_ns,ttl_ns FROM supervisor_leases WHERE project_id=? AND state='held' ORDER BY run_id`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var runID, actor, worktree, rowBoot string
		var pid, last, ttl int64
		if err := rows.Scan(&runID, &actor, &worktree, &pid, &rowBoot, &last, &ttl); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if last < 0 || ttl <= 0 {
			_ = rows.Close()
			return nil, errors.New("E_EJECT_UNVERIFIED: malformed supervisor lease")
		}
		live := rowBoot == bootID && (monoNS < uint64(last) || monoNS-uint64(last) < uint64(ttl))
		if live {
			holders = append(holders, fmt.Sprintf("run %s held by %s pid %d (%s)", runID, actor, pid, worktree))
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	sort.Strings(holders)
	return holders, nil
}

func (db *DB) Eject(ctx context.Context, project ProjectRegistration, force bool) (EjectResult, error) {
	result := EjectResult{ProjectID: project.ProjectID, RowsDropped: map[string]int{}, TelemetryDiscarded: map[string]int{}, Files: "kept"}
	for _, prefix := range project.Prefixes {
		result.PrefixesReleased = append(result.PrefixesReleased, prefix.Prefix)
	}
	s := &Store{db: db.db}
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		bootID, monoNS, err := (systemClock{}).Now()
		if err != nil {
			return fmt.Errorf("E_EJECT_UNVERIFIED: live-state clock unavailable: %w", err)
		}
		holders, err := ejectLiveHolders(ctx, conn, project.ProjectID, bootID, monoNS)
		if err != nil {
			return err
		}
		if len(holders) > 0 && !force {
			return fmt.Errorf("E_EJECT_LIVE_STATE: %s", strings.Join(holders, "; "))
		}
		var pending int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE project_id=? AND materialised=0`, project.ProjectID).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return fmt.Errorf("E_EJECT_UNVERIFIED: %d unresolved materialisations remain", pending)
		}

		tables, err := projectTablesOnConn(ctx, conn)
		if err != nil {
			return err
		}
		for _, table := range tables {
			if table == "ejections" {
				continue
			}
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+quoteIdentifier(table)+` WHERE project_id=?`, project.ProjectID).Scan(&count); err != nil {
				return err
			}
			result.RowsDropped[table] = count
		}
		for _, table := range []string{
			"test_report_counter", "test_reports", "test_report_results",
			"rant_counter", "rants", "rant_tags", "rant_git_context", "rant_context_refs", "rant_reviews",
			"compute_event_counter", "compute_events", "command_event_counter", "command_events",
			"quota_snapshot_counter", "quota_snapshots",
		} {
			result.TelemetryDiscarded[table] = result.RowsDropped[table]
		}
		for _, prefix := range project.Prefixes {
			changed, err := conn.ExecContext(ctx, `DELETE FROM prefix_ownership WHERE prefix=? AND project_id=?`, prefix.Prefix, project.ProjectID)
			if err != nil {
				return err
			}
			n, err := changed.RowsAffected()
			if err != nil || n != 1 {
				return fmt.Errorf("E_EJECT_UNVERIFIED: prefix ownership changed during eject for %s", prefix.Prefix)
			}
		}
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS rant_reviews_no_delete`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS rant_reviews_no_update`); err != nil {
			return err
		}
		deleted, err := conn.ExecContext(ctx, `DELETE FROM projects WHERE project_id=?`, project.ProjectID)
		if err != nil {
			return err
		}
		if n, err := deleted.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("E_EJECT_UNVERIFIED: project changed during eject")
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM search_fts WHERE project_id=?`, project.ProjectID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO ejections(project_id,ejected_at) VALUES (?,?)`, project.ProjectID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `CREATE TRIGGER rant_reviews_no_delete BEFORE DELETE ON rant_reviews BEGIN SELECT RAISE(ABORT,'rant reviews are append-only'); END`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, rantReviewNoUpdateTriggerDDL); err != nil {
			return err
		}
		for _, table := range tables {
			if table == "ejections" {
				continue
			}
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+quoteIdentifier(table)+` WHERE project_id=?`, project.ProjectID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("E_SCHEMA_INVALID: eject left %d rows in %s", count, table)
			}
		}
		return nil
	})
	if err != nil {
		return EjectResult{}, err
	}
	return result, nil
}

func projectTablesOnConn(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var all []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, name)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var result []string
	for _, table := range all {
		columns, err := tableColumnNames(ctx, conn, table)
		if err != nil {
			return nil, err
		}
		if stringSliceContains(columns, "project_id") {
			result = append(result, table)
		}
	}
	return result, nil
}

// TrimRegistryProject removes every breadcrumb for projectID under the same
// advisory lock used by appendJSONLine, then atomically replaces the JSONL.
func TrimRegistryProject(path, projectID string) error {
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var kept []registryEntry
	for {
		var entry registryEntry
		err := dec.Decode(&entry)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("E_CONFIG_INVALID: registry: %w", err)
		}
		if entry.ProjectID != projectID {
			kept = append(kept, entry)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".trim-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		_ = tmp.Close()
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	for _, entry := range kept {
		if err := appendJSONValue(tmp, entry); err != nil {
			return err
		}
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTmp = false
	return syncDir(filepath.Dir(path))
}
