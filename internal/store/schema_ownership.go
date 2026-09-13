package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// projectOwnershipTables is the legacy schema surface whose project_id column
// predates foreign-key ownership. Tables added with an FK are intentionally not
// listed: a non-empty PRAGMA foreign_key_list is the migration gate, and the
// schema-introspection test guards the complete current schema.
var projectOwnershipTables = []string{
	"worktrees",
	"prefix_ownership",
	"event_counters",
	"id_counters",
	"allocations",
	"outbox",
	"events",
	"tickets",
	"requirements",
	"relations",
	"findings",
	"leases",
	"supervisor_leases",
	"area_hints",
	"gates",
	"gate_results",
	"gate_proofs",
	"gate_attestations",
	"test_report_counter",
	"rant_counter",
	"compute_event_counter",
	"command_event_counter",
	"quota_snapshot_counter",
}

// ensureProjectOwnershipFKs upgrades pre-lifecycle databases in place. The
// common current-schema path is read-only; a legacy database is rewritten in
// one BEGIN IMMEDIATE transaction so a crash exposes either the old schema or
// the complete ownership chain, never a partially migrated set.
func (s *Store) ensureProjectOwnershipFKs(ctx context.Context) error {
	current := true
	for _, table := range projectOwnershipTables {
		hasFK, err := tableHasAnyForeignKey(ctx, s.db, table)
		if err != nil {
			return translateDBError(err)
		}
		if !hasFK {
			current = false
			break
		}
	}
	if current {
		return nil
	}

	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		for _, table := range projectOwnershipTables {
			hasFK, err := tableHasAnyForeignKey(ctx, conn, table)
			if err != nil {
				return err
			}
			if hasFK {
				continue
			}
			if err := recreateProjectOwnedTable(ctx, conn, table); err != nil {
				return fmt.Errorf("migrate project ownership for %s: %w", table, err)
			}
		}
		rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			var table, parent string
			var rowID sql.NullInt64
			var fkID int
			if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
				return err
			}
			return fmt.Errorf("E_SCHEMA_INVALID: foreign_key_check failed for table %s row %v parent %s fk %d", table, rowID, parent, fkID)
		}
		return rows.Err()
	})
}

func tableHasAnyForeignKey(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) (bool, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_list(`+quoteIdentifier(table)+`)`)
	if err != nil {
		return false, err
	}
	hasFK := rows.Next()
	if err := rows.Close(); err != nil {
		return false, err
	}
	return hasFK, nil
}

func recreateProjectOwnedTable(ctx context.Context, conn *sql.Conn, table string) error {
	var createSQL string
	if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&createSQL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("E_SCHEMA_INVALID: table is missing")
		}
		return err
	}
	open := strings.Index(createSQL, "(")
	close := strings.LastIndex(createSQL, ")")
	if open < 0 || close <= open || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(createSQL)), "CREATE TABLE") {
		return fmt.Errorf("E_SCHEMA_INVALID: cannot rewrite CREATE TABLE statement")
	}

	columns, err := tableColumnNames(ctx, conn, table)
	if err != nil {
		return err
	}
	if len(columns) == 0 || !stringSliceContains(columns, "project_id") {
		return fmt.Errorf("E_SCHEMA_INVALID: project_id column is missing")
	}
	objects, err := tableSchemaObjects(ctx, conn, table)
	if err != nil {
		return err
	}

	// Rows without a projects parent are already detached, rebuildable index
	// data. They cannot be copied into the now-enforced ownership chain.
	if _, err := conn.ExecContext(ctx, `DELETE FROM `+quoteIdentifier(table)+` WHERE NOT EXISTS (SELECT 1 FROM projects WHERE projects.project_id=`+quoteIdentifier(table)+`.project_id)`); err != nil {
		return err
	}

	temporary := table + "_project_fk"
	if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS `+quoteIdentifier(temporary)); err != nil {
		return err
	}
	replacementDDL := `CREATE TABLE ` + quoteIdentifier(temporary) + ` ` + createSQL[open:close] + `,
		FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE` + createSQL[close:]
	if _, err := conn.ExecContext(ctx, replacementDDL); err != nil {
		return err
	}
	quotedColumns := make([]string, len(columns))
	for index, column := range columns {
		quotedColumns[index] = quoteIdentifier(column)
	}
	columnList := strings.Join(quotedColumns, ",")
	if _, err := conn.ExecContext(ctx, `INSERT INTO `+quoteIdentifier(temporary)+` (`+columnList+`) SELECT `+columnList+` FROM `+quoteIdentifier(table)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE `+quoteIdentifier(table)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE `+quoteIdentifier(temporary)+` RENAME TO `+quoteIdentifier(table)); err != nil {
		return err
	}
	for _, ddl := range objects {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// allocationsDDL is the single source of truth for the allocations table shape,
// used by both the fresh CREATE (ifNotExists=true, table="allocations") and by
// ensureAllocationsSuffix's recreation (a temp name). The primary key is
// (project_id, prefix, number, suffix) so a hand-authored split-child
// (FEE-BL-10a) keeps an allocation row distinct from its plain-numbered twin
// (AIRA-237 Task 3, path A). suffix is TEXT NOT NULL with NO DEFAULT: an INSERT
// that omits it fails loudly rather than silently writing the plain-twin key.
func allocationsDDL(table string, ifNotExists bool) string {
	ine := ""
	if ifNotExists {
		ine = "IF NOT EXISTS "
	}
	return `CREATE TABLE ` + ine + table + ` (
            project_id TEXT NOT NULL, prefix TEXT NOT NULL, number INTEGER NOT NULL,
            worktree_id TEXT NOT NULL, state TEXT NOT NULL, path TEXT NOT NULL,
            seq INTEGER NOT NULL, kind TEXT NOT NULL DEFAULT 'ticket', suffix TEXT NOT NULL,
            PRIMARY KEY(project_id, prefix, number, suffix),
            FOREIGN KEY(project_id) REFERENCES projects(project_id) ON DELETE CASCADE
        )`
}

// ensureAllocationsSuffix widens the allocations primary key with the Task-3
// `suffix` column on a pre-suffix database. SQLite cannot ALTER a primary key
// and `CREATE TABLE IF NOT EXISTS` is a no-op on the live machine-wide
// state.db, so the table is recreated: a temp with the 4-column PK, INSERT ...
// SELECT ..., '' (every existing row keeps an empty suffix), DROP, RENAME — all
// in one BEGIN IMMEDIATE. A fast-path PRAGMA check keeps the common (already
// migrated) open read-only, and the predicate is re-checked under the write
// lock so a losing racer is a no-op (the AIRA-97 shape). It runs BEFORE
// ensureProjectOwnershipFKs — the recreation carries the project FK forward, so
// the later FK migration sees allocations already owned and skips it.
func (s *Store) ensureAllocationsSuffix(ctx context.Context) error {
	present, err := tableHasColumn(ctx, s.db, "allocations", "suffix")
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		present, err := tableHasColumn(ctx, conn, "allocations", "suffix")
		if err != nil {
			return err
		}
		if present {
			return nil
		}
		columns, err := tableColumnNames(ctx, conn, "allocations")
		if err != nil {
			return err
		}
		if len(columns) == 0 || !stringSliceContains(columns, "project_id") {
			return fmt.Errorf("E_SCHEMA_INVALID: allocations table is missing")
		}
		// Rows without a projects parent are detached, rebuildable index data;
		// they cannot enter the now-FK-enforced table (mirrors
		// recreateProjectOwnedTable). Harmless on a healthy database.
		if _, err := conn.ExecContext(ctx, `DELETE FROM allocations WHERE NOT EXISTS (SELECT 1 FROM projects WHERE projects.project_id=allocations.project_id)`); err != nil {
			return err
		}
		const temporary = "allocations_suffix"
		if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS `+temporary); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, allocationsDDL(temporary, false)); err != nil {
			return err
		}
		quotedColumns := make([]string, len(columns))
		for index, column := range columns {
			quotedColumns[index] = quoteIdentifier(column)
		}
		columnList := strings.Join(quotedColumns, ",")
		// Existing rows carry no suffix, so the widened key gets ''.
		if _, err := conn.ExecContext(ctx, `INSERT INTO `+temporary+` (`+columnList+`, suffix) SELECT `+columnList+`, '' FROM allocations`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DROP TABLE allocations`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `ALTER TABLE `+temporary+` RENAME TO allocations`); err != nil {
			return err
		}
		return nil
	})
}

func tableColumnNames(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+quoteIdentifier(table)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// tableSchemaObjects captures explicit indexes and triggers before DROP TABLE;
// SQLite recreates autoindexes from the table constraints themselves.
func tableSchemaObjects(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE tbl_name=? AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []string
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			return nil, err
		}
		objects = append(objects, ddl)
	}
	return objects, rows.Err()
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func stringSliceContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
