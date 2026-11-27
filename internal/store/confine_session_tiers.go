package store

import (
	"context"
	"errors"
)

// ConfineSessionTiers reads every stored session priority mark (AIRA-381):
// owner -> "high" or "low". The daemon is its one caller, once at start; it
// validates the rows itself (an unusable row makes the whole table unevaluated,
// never a silently dropped mark), so this reader returns them as stored.
func (db *DB) ConfineSessionTiers(ctx context.Context) (map[string]string, error) {
	if db == nil || db.db == nil {
		return nil, errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	rows, err := db.db.QueryContext(ctx, `SELECT owner, tier FROM confine_session_tiers`)
	if err != nil {
		return nil, translateDBError(err)
	}
	defer rows.Close()
	marks := map[string]string{}
	for rows.Next() {
		var owner, tier string
		if err := rows.Scan(&owner, &tier); err != nil {
			return nil, translateDBError(err)
		}
		marks[owner] = tier
	}
	if err := rows.Err(); err != nil {
		return nil, translateDBError(err)
	}
	return marks, nil
}

// ReplaceConfineSessionTiers makes the stored marks exactly marks, in one
// transaction: every row is deleted and the map inserted. The daemon calls it
// only with a table it loaded from here and then changed by one set, so it
// never deletes a row the daemon did not read (AIRA-381 §3.1).
func (db *DB) ReplaceConfineSessionTiers(ctx context.Context, marks map[string]string) error {
	if db == nil || db.db == nil {
		return errors.New("E_DAEMON_UNAVAILABLE: state database is unavailable")
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return translateDBError(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM confine_session_tiers`); err != nil {
		return translateDBError(err)
	}
	for owner, tier := range marks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO confine_session_tiers(owner, tier) VALUES(?, ?)`, owner, tier); err != nil {
			return translateDBError(err)
		}
	}
	return translateDBError(tx.Commit())
}
