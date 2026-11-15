//go:build linux

package daemon

import (
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// completionRow is one confine_peak_history row as AIRA-323 tests read it back.
type completionRow struct {
	kind                        string
	peak                        sql.NullInt64
	oom                         int64
	budget                      sql.NullInt64
	basis                       sql.NullString
	at                          string
	wallMS, waitMS, startedAtMS sql.NullInt64
	repo                        sql.NullString
}

func readCompletionRows(t *testing.T, dbPath, signature string) []completionRow {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.Query(`SELECT kind, peak_rss, oom, budget, budget_basis, at, wall_ms, wait_ms, started_at_ms, repo
FROM confine_peak_history WHERE signature=? ORDER BY rowid`, signature)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []completionRow
	for rows.Next() {
		var row completionRow
		if err := rows.Scan(&row.kind, &row.peak, &row.oom, &row.budget, &row.basis, &row.at,
			&row.wallMS, &row.waitMS, &row.startedAtMS, &row.repo); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (row completionRow) factsAllNull() bool {
	return !row.wallMS.Valid && !row.waitMS.Valid && !row.startedAtMS.Valid && !row.repo.Valid
}

// completionServer starts a REAL daemon (Serve, real store, real socket) and
// counts the confine-report requests it dispatches.
func completionServer(t *testing.T) (Paths, *atomic.Int64) {
	t.Helper()
	paths := testPaths(t)
	server := NewServer(paths)
	var reports atomic.Int64
	server.OnRequest = func(_ WorktreeScope, request core.Request) {
		if request.Verb == "confine-report" {
			reports.Add(1)
		}
	}
	_, _ = startServer(t, server)
	return paths, &reports
}

func reportCtx(t *testing.T) context.Context {
	t.Helper()
	// Generous: these exchanges are local; the production 250 ms deadline is the
	// launch site's, not the function's.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func ptr64(value int64) *int64 { return &value }

// withPre323Allowlist swaps the handler's field set for master's six-field set,
// so the real handler's count guard and allowlist loop reproduce a daemon that
// predates AIRA-323. Not parallel-safe; restored on cleanup.
func withPre323Allowlist(t *testing.T) {
	t.Helper()
	saved := confineReportFields
	confineReportFields = map[string]bool{
		"signature": true, "peak_rss": true, "oom": true,
		"kind": true, "budget": true, "budget_basis": true,
	}
	t.Cleanup(func() { confineReportFields = saved })
}

// TestConfineReportCompletionFactsEndToEnd is T1: the real client sender to the
// real daemon handler and store, every sent value stored exactly, including a
// real 0 wait and a start time an hour before the report (delayed delivery: the
// daemon must store the sent start, not derive `at - wall_ms`).
//
// verifies: AIRA-323
func TestConfineReportCompletionFactsEndToEnd(t *testing.T) {
	paths, reports := completionServer(t)
	startedAt := time.Now().Add(-time.Hour).UnixMilli()
	report := runner.ConfinePeakReport{
		Signature: "aira-323\x00t1", Peak: ptr64(64 << 20), OOM: false,
		Budget: ptr64(128 << 20), BudgetBasis: "cap:operator:--memory-reserve",
		WallMS: ptr64(1_000), WaitMS: ptr64(0), StartedAtMS: ptr64(startedAt), Repo: "0123456789abcdef",
	}
	if err := runner.ReportPeakSample(reportCtx(t), paths.SocketPath, report); err != nil {
		t.Fatal(err)
	}
	rows := readCompletionRows(t, paths.DBPath, report.Signature)
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	row := rows[0]
	if row.kind != "confine" || !row.peak.Valid || row.peak.Int64 != 64<<20 || row.oom != 0 ||
		!row.budget.Valid || row.budget.Int64 != 128<<20 || row.basis.String != "cap:operator:--memory-reserve" {
		t.Fatalf("pre-323 columns differ: %+v", row)
	}
	if !row.wallMS.Valid || row.wallMS.Int64 != 1_000 || !row.waitMS.Valid || row.waitMS.Int64 != 0 ||
		!row.startedAtMS.Valid || row.startedAtMS.Int64 != startedAt || !row.repo.Valid || row.repo.String != "0123456789abcdef" {
		t.Fatalf("completion facts differ from the sent values: %+v (sent started_at_ms %d)", row, startedAt)
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("confine-report requests=%d, want exactly 1", got)
	}
}

// TestConfineReportCompletionZeroWallEndToEnd: a real 0 wall_ms (a
// sub-millisecond run) survives the real sender and is stored as 0, not NULL.
//
// verifies: AIRA-323
func TestConfineReportCompletionZeroWallEndToEnd(t *testing.T) {
	paths, reports := completionServer(t)
	report := runner.ConfinePeakReport{
		Signature: "aira-323\x00zero-wall", OOM: false,
		WallMS: ptr64(0), StartedAtMS: ptr64(time.Now().UnixMilli()), Repo: "none",
	}
	if err := runner.ReportPeakSample(reportCtx(t), paths.SocketPath, report); err != nil {
		t.Fatal(err)
	}
	rows := readCompletionRows(t, paths.DBPath, report.Signature)
	if len(rows) != 1 || !rows[0].wallMS.Valid || rows[0].wallMS.Int64 != 0 {
		t.Fatalf("rows=%+v, want wall_ms stored as a real 0", rows)
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("requests=%d, want exactly 1", got)
	}
}

// TestConfineReportCompletionAbsentIsNullForEveryKind is T2.
//
// verifies: AIRA-323
func TestConfineReportCompletionAbsentIsNullForEveryKind(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := store.OpenDB(dbPath, filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(Paths{})
	server.db = db
	for _, args := range []map[string]any{
		{"signature": "confine-bare", "oom": false},
		{"signature": "pool-bare", "oom": false, "kind": "pytest-worker"},
		{"signature": "pool-full", "oom": true, "kind": "pytest-worker",
			"wall_ms": int64(5), "wait_ms": int64(0), "started_at_ms": int64(1_763_078_400_000), "repo": "fedcba9876543210"},
	} {
		if response := server.confineReport(args); !response.OK {
			t.Fatalf("%v refused: %+v", args, response)
		}
	}
	for _, signature := range []string{"confine-bare", "pool-bare"} {
		rows := readCompletionRows(t, dbPath, signature)
		if len(rows) != 1 || !rows[0].factsAllNull() {
			t.Fatalf("%s: absent facts must be stored NULL: %+v", signature, rows)
		}
	}
	rows := readCompletionRows(t, dbPath, "pool-full")
	if len(rows) != 1 || rows[0].kind != "pytest-worker" || rows[0].wallMS.Int64 != 5 || !rows[0].waitMS.Valid || rows[0].waitMS.Int64 != 0 ||
		rows[0].startedAtMS.Int64 != 1_763_078_400_000 || rows[0].repo.String != "fedcba9876543210" {
		t.Fatalf("a pytest-worker report's facts must be stored as given (no kind gate): %+v", rows)
	}
}

// TestConfineReportCompletionValidation is T3: the handler is the one layer that
// validates; every bound of every field has a case one past it, and every
// boundary value is accepted and stored as sent.
//
// verifies: AIRA-323
func TestConfineReportCompletionValidation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := store.OpenDB(dbPath, filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(Paths{})
	server.db = db
	const maxDuration = int64(31_622_400_000)
	refused := map[string]map[string]any{
		"wall_ms -1":                {"wall_ms": int64(-1)},
		"wall_ms 366d+1":            {"wall_ms": maxDuration + 1},
		"wait_ms -1":                {"wait_ms": int64(-1)},
		"wait_ms 366d+1":            {"wait_ms": maxDuration + 1},
		"wall_ms 1.5":               {"wall_ms": 1.5},
		"wall_ms abc":               {"wall_ms": "abc"},
		"wait_ms 1.5":               {"wait_ms": 1.5},
		"wait_ms abc":               {"wait_ms": "abc"},
		"started_at_ms 1.5":         {"started_at_ms": 1.5},
		"started_at_ms abc":         {"started_at_ms": "abc"},
		"started_at_ms before 2000": {"started_at_ms": int64(946_684_799_999)},
		"started_at_ms 2100":        {"started_at_ms": int64(4_102_444_800_000)},
		"started_at_ms 0":           {"started_at_ms": int64(0)},
		"started_at_ms -1":          {"started_at_ms": int64(-1)},
		"repo empty":                {"repo": ""},
		"repo NONE":                 {"repo": "NONE"},
		"repo 15 hex":               {"repo": "0123456789abcde"},
		"repo 17 hex":               {"repo": "0123456789abcdef0"},
		"repo 64 hex":               {"repo": strings.Repeat("0123456789abcdef", 4)},
		"repo upper hex":            {"repo": "0123456789ABCDEF"},
		"repo path":                 {"repo": "/home/x/repo/.git"},
		"repo not a string":         {"repo": int64(1)},
		"repo 16 non-hex lowercase": {"repo": "0123456789abcdeg"},
		"wall_ms bool":              {"wall_ms": true},
	}
	for name, extra := range refused {
		signature := "refused/" + name
		args := map[string]any{"signature": signature, "oom": false}
		for key, value := range extra {
			args[key] = value
		}
		response := server.confineReport(args)
		if response.OK || response.Code != CodeProtocol {
			t.Fatalf("%s: response=%+v, want E_DAEMON_PROTOCOL", name, response)
		}
		if rows := readCompletionRows(t, dbPath, signature); len(rows) != 0 {
			t.Fatalf("%s: a refused report wrote %d rows", name, len(rows))
		}
	}
	future := time.Now().Add(365 * 24 * time.Hour).UnixMilli()
	accepted := map[string]struct {
		field string
		value any
	}{
		"wall_ms 0":              {"wall_ms", int64(0)},
		"wall_ms 366d":           {"wall_ms", maxDuration},
		"wait_ms 0":              {"wait_ms", int64(0)},
		"wait_ms 366d":           {"wait_ms", maxDuration},
		"started_at_ms 2000":     {"started_at_ms", int64(946_684_800_000)},
		"started_at_ms <2100":    {"started_at_ms", int64(4_102_444_799_999)},
		"started_at_ms future":   {"started_at_ms", future},
		"repo none":              {"repo", "none"},
		"repo 16 hex":            {"repo", "0123456789abcdef"},
		"wall_ms decimal text":   {"wall_ms", "42"},
		"wait_ms integral float": {"wait_ms", float64(7)},
	}
	for name, tc := range accepted {
		signature := "accepted/" + name
		response := server.confineReport(map[string]any{"signature": signature, "oom": false, tc.field: tc.value})
		if !response.OK {
			t.Fatalf("%s: refused: %+v", name, response)
		}
		rows := readCompletionRows(t, dbPath, signature)
		if len(rows) != 1 {
			t.Fatalf("%s: rows=%d", name, len(rows))
		}
		row := rows[0]
		var got any
		switch tc.field {
		case "wall_ms":
			got = row.wallMS
		case "wait_ms":
			got = row.waitMS
		case "started_at_ms":
			got = row.startedAtMS
		case "repo":
			got = row.repo
		}
		want := map[string]any{
			"wall_ms 0": int64(0), "wall_ms 366d": maxDuration, "wait_ms 0": int64(0), "wait_ms 366d": maxDuration,
			"started_at_ms 2000": int64(946_684_800_000), "started_at_ms <2100": int64(4_102_444_799_999),
			"started_at_ms future": future, "repo none": "none", "repo 16 hex": "0123456789abcdef",
			"wall_ms decimal text": int64(42), "wait_ms integral float": int64(7),
		}[name]
		switch value := got.(type) {
		case sql.NullInt64:
			if !value.Valid || value.Int64 != want {
				t.Fatalf("%s: stored %+v, want %v", name, value, want)
			}
		case sql.NullString:
			if !value.Valid || value.String != want {
				t.Fatalf("%s: stored %+v, want %v", name, value, want)
			}
		}
	}
}

// TestConfineReportVersionSkewResend is T9: a new client against a daemon that
// predates AIRA-323 (the real handler with master's six-field allowlist), and
// the once-only re-send's edges. Not parallel: it swaps a package variable.
//
// verifies: AIRA-323
func TestConfineReportVersionSkewResend(t *testing.T) {
	startedAt := time.Now().UnixMilli()
	full := func(signature string, oom bool) runner.ConfinePeakReport {
		return runner.ConfinePeakReport{
			Signature: signature, Peak: ptr64(32 << 20), OOM: oom,
			Budget: ptr64(64 << 20), BudgetBasis: "cap:operator:--memory-reserve",
			WallMS: ptr64(900), WaitMS: ptr64(12), StartedAtMS: ptr64(startedAt), Repo: "0123456789abcdef",
		}
	}
	for _, oom := range []bool{true, false} {
		wantOOM := int64(0)
		if oom {
			wantOOM = 1
		}
		oomName := map[bool]string{true: "oom", false: "clean"}[oom]
		t.Run("(a) old daemon, nine fields: the count guard path, "+oomName, func(t *testing.T) {
			withPre323Allowlist(t)
			paths, reports := completionServer(t)
			report := full("skew-a-"+oomName, oom)
			if err := runner.ReportPeakSample(reportCtx(t), paths.SocketPath, report); err != nil {
				t.Fatalf("the re-send must keep the sample: %v", err)
			}
			rows := readCompletionRows(t, paths.DBPath, report.Signature)
			if len(rows) != 1 || !rows[0].factsAllNull() || rows[0].oom != wantOOM ||
				rows[0].peak.Int64 != 32<<20 || rows[0].budget.Int64 != 64<<20 || rows[0].basis.String != "cap:operator:--memory-reserve" {
				t.Fatalf("rows=%+v, want the peak, the budget pair, oom=%d and NULL facts", rows, wantOOM)
			}
			if got := reports.Load(); got != 2 {
				t.Fatalf("requests=%d, want 2 (one refusal, one re-send)", got)
			}
		})
		t.Run("(b) old daemon, four fields: the unknown-field path, "+oomName, func(t *testing.T) {
			withPre323Allowlist(t)
			paths, reports := completionServer(t)
			report := runner.ConfinePeakReport{Signature: "skew-b-" + oomName, OOM: oom, WallMS: ptr64(900), StartedAtMS: ptr64(startedAt)}
			if err := runner.ReportPeakSample(reportCtx(t), paths.SocketPath, report); err != nil {
				t.Fatalf("the re-send must keep the sample: %v", err)
			}
			rows := readCompletionRows(t, paths.DBPath, report.Signature)
			if len(rows) != 1 || !rows[0].factsAllNull() || rows[0].oom != wantOOM {
				t.Fatalf("rows=%+v, want oom=%d and NULL facts", rows, wantOOM)
			}
			if got := reports.Load(); got != 2 {
				t.Fatalf("requests=%d, want 2", got)
			}
		})
	}
	t.Run("(b0) old daemon, the unknown-field path is the path taken", func(t *testing.T) {
		withPre323Allowlist(t)
		server := NewServer(Paths{})
		response := server.confineReport(map[string]any{"signature": "s", "oom": false, "wall_ms": int64(1), "started_at_ms": startedAt})
		if response.Code != CodeProtocol || !strings.Contains(response.Error, "unexpected confine-report field") {
			t.Fatalf("response=%+v, want the allowlist refusal", response)
		}
		nine := map[string]any{"signature": "s", "oom": false, "peak_rss": int64(1), "budget": int64(1), "budget_basis": "cap:x",
			"wall_ms": int64(1), "wait_ms": int64(1), "started_at_ms": startedAt, "repo": "none"}
		response = server.confineReport(nine)
		if response.Code != CodeProtocol || !strings.Contains(response.Error, "requires signature and oom") {
			t.Fatalf("response=%+v, want the count-guard refusal", response)
		}
	})
	t.Run("(c) new daemon refuses a value: re-sent without the facts", func(t *testing.T) {
		paths, reports := completionServer(t)
		report := full("skew-c", true)
		report.Repo = "NONE"
		if err := runner.ReportPeakSample(reportCtx(t), paths.SocketPath, report); err != nil {
			t.Fatal(err)
		}
		rows := readCompletionRows(t, paths.DBPath, report.Signature)
		if len(rows) != 1 || !rows[0].factsAllNull() || rows[0].oom != 1 || rows[0].peak.Int64 != 32<<20 {
			t.Fatalf("rows=%+v", rows)
		}
		if got := reports.Load(); got != 2 {
			t.Fatalf("requests=%d, want 2", got)
		}
	})
	t.Run("(d) a refusal of a frame without new fields is not re-sent", func(t *testing.T) {
		paths, reports := completionServer(t)
		err := runner.ReportPeakSample(reportCtx(t), paths.SocketPath, runner.ConfinePeakReport{Signature: "skew-d", Kind: "guess"})
		if err == nil || !strings.Contains(err.Error(), "E_DAEMON_PROTOCOL") {
			t.Fatalf("err=%v, want the protocol refusal returned", err)
		}
		if got := reports.Load(); got != 1 {
			t.Fatalf("requests=%d, want 1", got)
		}
	})
	t.Run("(e) the re-send happens once, never until success", func(t *testing.T) {
		paths, reports := completionServer(t)
		err := runner.ReportPeakSample(reportCtx(t), paths.SocketPath, runner.ConfinePeakReport{Signature: "skew-e", Kind: "guess", WallMS: ptr64(1)})
		if err == nil || !strings.Contains(err.Error(), "E_DAEMON_PROTOCOL") {
			t.Fatalf("err=%v, want the second refusal returned", err)
		}
		if got := reports.Load(); got != 2 {
			t.Fatalf("requests=%d, want exactly 2", got)
		}
	})
	t.Run("(f) no re-send on a missing reply", func(t *testing.T) {
		socket := filepath.Join(shortRuntimeDir(t), "s")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		var connections atomic.Int64
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				connections.Add(1)
				// Read the whole frame, then close without replying.
				var header [4]byte
				if _, err := io.ReadFull(conn, header[:]); err == nil {
					_, _ = io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint32(header[:])))
				}
				_ = conn.Close()
			}
		}()
		err = runner.ReportPeakSample(reportCtx(t), socket, full("skew-f", false))
		if err == nil {
			t.Fatal("a missing reply must be an error")
		}
		_ = listener.Close()
		if got := connections.Load(); got != 1 {
			t.Fatalf("connections=%d, want exactly 1", got)
		}
	})
	t.Run("(h) no re-send on an envelope that is not a well-formed refusal", func(t *testing.T) {
		// Only an explicitly present ok=false with the protocol code is a
		// refusal. ok=true with that code contradicts itself, and a missing or
		// null ok establishes nothing: in each case the daemon may have
		// committed, so a re-send could duplicate the row.
		for _, tc := range []struct {
			name  string
			reply any
		}{
			{"contradictory ok=true", ResponseFrame{OK: true, Code: CodeProtocol, Error: CodeProtocol + ": contradictory"}},
			{"ok missing", map[string]any{"code": CodeProtocol, "error": CodeProtocol + ": no ok"}},
			{"ok null", map[string]any{"ok": nil, "code": CodeProtocol, "error": CodeProtocol + ": null ok"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				socket := filepath.Join(shortRuntimeDir(t), "s")
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				var connections atomic.Int64
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						connections.Add(1)
						var header [4]byte
						if _, err := io.ReadFull(conn, header[:]); err == nil {
							_, _ = io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint32(header[:])))
						}
						_ = writeFrame(conn, tc.reply)
						_ = conn.Close()
					}
				}()
				err = runner.ReportPeakSample(reportCtx(t), socket, full("skew-h", false))
				if err == nil {
					t.Fatal("a reply that is not a success must be an error")
				}
				_ = listener.Close()
				if got := connections.Load(); got != 1 {
					t.Fatalf("connections=%d, want exactly 1", got)
				}
			})
		}
	})
	t.Run("(g) no re-send on another refusal code", func(t *testing.T) {
		// A real server with NO store answers E_DAEMON_UNAVAILABLE.
		server := NewServer(Paths{})
		server.stopping = make(chan struct{})
		var reports atomic.Int64
		server.OnRequest = func(_ WorktreeScope, request core.Request) {
			if request.Verb == "confine-report" {
				reports.Add(1)
			}
		}
		socket := filepath.Join(shortRuntimeDir(t), "s")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go server.serveConnection(ctx, conn)
			}
		}()
		err = runner.ReportPeakSample(reportCtx(t), socket, runner.ConfinePeakReport{Signature: "skew-g", WallMS: ptr64(1)})
		if err == nil || !strings.Contains(err.Error(), CodeUnavailable) {
			t.Fatalf("err=%v, want %s returned", err, CodeUnavailable)
		}
		if got := reports.Load(); got != 1 {
			t.Fatalf("requests=%d, want exactly 1", got)
		}
	})
}
