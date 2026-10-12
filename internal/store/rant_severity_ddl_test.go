package store

import (
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// verifies: AIRA-210 -- the rant severity set has one list. The dispatch
// declaration (internal/core TestRantSeverityOneList) is pinned to the domain
// constants; this pins the SQLite CHECK to the same constants, in order.
func TestRantSeverityDDLMatchesDomainConstants(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, filepath.Join(base, "main"), filepath.Join(base, "common"), filepath.Join(base, "state"))
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='rants'`).Scan(&ddl); err != nil {
		t.Fatalf("rants table definition not readable: %v", err)
	}
	quoted := []string{"''"}
	for _, v := range []domain.RantSeverity{domain.RantSeverityPapercut, domain.RantSeverityAnnoyance, domain.RantSeverityBlocker} {
		quoted = append(quoted, "'"+string(v)+"'")
	}
	squash := func(in string) string { return strings.Join(strings.Fields(in), "") }
	want := "CHECK(severityIN(" + strings.Join(quoted, ",") + "))"
	if !strings.Contains(squash(ddl), want) {
		t.Fatalf("rants DDL severity CHECK does not match the domain constants.\nwant (whitespace removed): %s\nDDL: %s", want, ddl)
	}
}
