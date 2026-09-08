package store

// TotalRowChanges is SQLite's own row-write counter for this database's single
// connection (OpenDB pins the pool to one), exported so a face outside this
// package can assert that a read-only surface really wrote nothing.
//
// It exists for AIRA-180's report-only constraint, which is otherwise only
// checkable by reading code. Returns -1 when the count cannot be established —
// an unevaluated answer, never a silent zero that would make a write-count
// assertion vacuously pass.
func TotalRowChanges(db *DB) int64 {
	if db == nil || db.db == nil {
		return -1
	}
	var changes int64
	if err := db.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		return -1
	}
	return changes
}
