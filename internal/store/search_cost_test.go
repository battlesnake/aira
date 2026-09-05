package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSearchCostReproduction is the committed, executable reproduction of the
// measurements AIRA-74 publishes. It asserts nothing about wall-clock time —
// timings are not a correctness property and a timing assertion is a flake
// generator — it simply reports the numbers so a later reader can re-take them
// on their own hardware instead of trusting a ticket.
//
// It is skipped by default so it never becomes a CI cost:
//
//	AIRA_SEARCH_COST=1 go test ./internal/store/ -run TestSearchCostReproduction -v
//
// A cautionary note that cost this build real time: take these numbers with
// enough memory. An earlier pass measured inside an `aira confine` scope that
// had auto-reserved ~14 MB and sat pegged at 100% of its cap, which inflated
// every I/O-bound step by 10-30x and produced a confidently wrong conclusion
// about which migration recipe was affordable. Use
// `aira confine --memory-reserve 512M -- ...` or larger.
func TestSearchCostReproduction(t *testing.T) {
	if os.Getenv("AIRA_SEARCH_COST") == "" {
		t.Skip("set AIRA_SEARCH_COST=1 to take the AIRA-74 measurements")
	}

	t.Run("per-query grep", func(t *testing.T) {
		s := queryTestStore(t)
		const tickets = 100
		for i := 0; i < tickets; i++ {
			body := ""
			for word := 0; word < 400; word++ {
				body += fmt.Sprintf("word%d ", (i*400+word)%5000)
			}
			if _, err := s.CreateTicket(context.Background(), testCreateInput(fmt.Sprintf("ticket %d", i), body+" needle")); err != nil {
				t.Fatal(err)
			}
		}
		// Warm up, then measure steady state.
		if _, err := s.Search(context.Background(), "needle", ""); err != nil {
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		const runs = 10
		start := time.Now()
		for i := 0; i < runs; i++ {
			if _, err := s.Search(context.Background(), "needle", ""); err != nil {
				t.Fatal(err)
			}
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		t.Logf("grep over %d tickets: %.1f ms/query, %.1f MB allocated per query",
			tickets, float64(elapsed.Milliseconds())/runs,
			float64(after.TotalAlloc-before.TotalAlloc)/runs/(1<<20))
	})

	t.Run("legacy search_fts migration", func(t *testing.T) {
		base := t.TempDir()
		path := filepath.Join(base, "state.db")
		registry := filepath.Join(base, "registry.jsonl")
		first, err := OpenDB(path, registry)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		seedLegacySearchFTS(t, path, "cost_reproduction_needle", 4000)
		sizeBefore := fileSize(t, path)

		// Time the stages the way dropLegacySearchFTS runs them.
		db, err := sql.Open("sqlite", writableTestDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		defer db.Close()
		s := &Store{db: db, dbPath: path}
		ctx := context.Background()

		dropStart := time.Now()
		if err := s.withImmediate(ctx, func(conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, `DROP TABLE search_fts`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		dropTook := time.Since(dropStart)

		barrierStart := time.Now()
		if err := checkpointTruncateTolerateBusy(ctx, db); err != nil {
			t.Fatal(err)
		}
		barrierTook := time.Since(barrierStart)

		vacuumStart := time.Now()
		if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
			t.Fatal(err)
		}
		if err := checkpointTruncateTolerateBusy(ctx, db); err != nil {
			t.Fatal(err)
		}
		reclaimTook := time.Since(vacuumStart)

		t.Logf("migration: DROP (in BEGIN IMMEDIATE) %v | erasure barrier %v | reclaim %v | %d -> %d bytes",
			dropTook, barrierTook, reclaimTook, sizeBefore, fileSize(t, path))
		t.Logf("the DROP figure is the one that matters: it is held against the DSN's busy_timeout(5000), " +
			"and the daemon runs this before it listens while the client's startWait is 5s")
	})
}
