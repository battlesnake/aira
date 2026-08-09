package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func importTestStore(t *testing.T, name string) *Store {
	t.Helper()
	base := persistentTemp(t, name)
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
}

func TestImportFindingsValidAndIdempotent(t *testing.T) {
	s := importTestStore(t, "import-valid")
	jsonl := strings.Join([]string{
		`{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"codex","message":"off by one","file":"a.go","line":10}`,
		`{"ticket":"AIRA-2","category":"perf","severity":"P2","verdict":"plausible","source":"gemini","message":"slow loop"}`,
	}, "\n") + "\n"

	sum, err := s.ImportFindings(context.Background(), strings.NewReader(jsonl), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if sum.Imported != 2 || sum.Updated != 0 || len(sum.Skipped) != 0 || sum.Total != 2 {
		t.Fatalf("first import summary = %#v, want imported=2 updated=0 skipped=0 total=2", sum)
	}
	rows, err := s.ListFindings("")
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListFindings = %d rows err=%v, want 2", len(rows), err)
	}

	// Re-import identical content: idempotent -> all updated, none new.
	sum2, err := s.ImportFindings(context.Background(), strings.NewReader(jsonl), false)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if sum2.Imported != 0 || sum2.Updated != 2 || sum2.Total != 2 {
		t.Fatalf("reimport summary = %#v, want imported=0 updated=2 total=2", sum2)
	}
	if rows2, _ := s.ListFindings(""); len(rows2) != 2 {
		t.Fatalf("after reimport ListFindings = %d, want 2 (no duplicates)", len(rows2))
	}
}

func TestImportFindingsUpdateExistingContent(t *testing.T) {
	s := importTestStore(t, "import-update")
	first := `{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"codex","message":"first","file":"a.go","line":5}` + "\n"
	if sum, err := s.ImportFindings(context.Background(), strings.NewReader(first), false); err != nil || sum.Imported != 1 {
		t.Fatalf("first import = %#v err=%v, want imported=1", sum, err)
	}
	// Same identity (ticket/source/category/file/line), changed mutable content.
	second := `{"ticket":"AIRA-1","category":"correctness","severity":"P0","verdict":"refuted","source":"codex","message":"revised","file":"a.go","line":5}` + "\n"
	sum, err := s.ImportFindings(context.Background(), strings.NewReader(second), false)
	if err != nil || sum.Imported != 0 || sum.Updated != 1 {
		t.Fatalf("update import = %#v err=%v, want imported=0 updated=1", sum, err)
	}
	if rows, _ := s.ListFindings(""); len(rows) != 1 {
		t.Fatalf("update kept %d findings, want 1", len(rows))
	}
}

func mixedImportJSONL() string {
	return strings.Join([]string{
		`{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"codex","message":"good one"}`, // 1 valid
		`not json at all`,                             // 2 bad JSON
		`{"ticket":"AIRA-2","category":"perf","severity":"P9","verdict":"confirmed","source":"codex","message":"bad severity"}`, // 3 invalid enum
		``,                                            // 4 blank (ignored)
		`{"ticket":"","category":"x","severity":"P1","verdict":"confirmed","source":"codex","message":"no ticket"}`, // 5 empty ticket
		`{"subtype":"reconciliation","ticket":"AIRA-3","category":"x","severity":"P1","verdict":"confirmed","source":"aira","message":"recon"}`, // 6 wrong subtype
		`{"ticket":"AIRA-4","category":"style","severity":"P2","verdict":"plausible","source":"human","message":"another good"}`, // 7 valid
	}, "\n") + "\n"
}

func TestImportFindingsMixedNonStrictReportsSkips(t *testing.T) {
	s := importTestStore(t, "import-mixed")
	sum, err := s.ImportFindings(context.Background(), strings.NewReader(mixedImportJSONL()), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if sum.Imported != 2 {
		t.Fatalf("imported = %d, want 2 (the two valid records)", sum.Imported)
	}
	// Total counts non-blank lines: 6 (the blank line is not counted).
	if sum.Total != 6 {
		t.Fatalf("total = %d, want 6 non-blank records", sum.Total)
	}
	if len(sum.Skipped) != 4 {
		t.Fatalf("skipped = %#v, want 4 bad records", sum.Skipped)
	}
	// Skips carry the correct 1-based line numbers, never silently dropped.
	gotLines := map[int]bool{}
	for _, sk := range sum.Skipped {
		gotLines[sk.Line] = true
		if sk.Error == "" {
			t.Fatalf("skip has empty error: %#v", sk)
		}
	}
	for _, want := range []int{2, 3, 5, 6} {
		if !gotLines[want] {
			t.Fatalf("expected a skip at line %d; skips=%#v", want, sum.Skipped)
		}
	}
	if rows, _ := s.ListFindings(""); len(rows) != 2 {
		t.Fatalf("imported %d findings, want 2", len(rows))
	}
}

func TestImportFindingsStrictAbortsAndImportsNothing(t *testing.T) {
	s := importTestStore(t, "import-strict")
	_, err := s.ImportFindings(context.Background(), strings.NewReader(mixedImportJSONL()), true)
	if err == nil || ErrorCode(err) != "E_IMPORT_INVALID" {
		t.Fatalf("strict import err = %v, want E_IMPORT_INVALID", err)
	}
	if rows, _ := s.ListFindings(""); len(rows) != 0 {
		t.Fatalf("strict import wrote %d findings, want 0 (import nothing on any bad record)", len(rows))
	}
}

func TestImportFindingsLongLineNotSilentlyDropped(t *testing.T) {
	s := importTestStore(t, "import-longline")
	// A ~9MB message exceeds the old 8MB Scanner limit; bufio.Reader must still
	// read it (never silently drop), and a record AFTER it must still import.
	huge := strings.Repeat("x", 9*1024*1024)
	jsonl := `{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"codex","message":"` + huge + `"}` + "\n" +
		`{"ticket":"AIRA-2","category":"perf","severity":"P2","verdict":"plausible","source":"gemini","message":"after the huge line"}` + "\n"
	sum, err := s.ImportFindings(context.Background(), strings.NewReader(jsonl), false)
	if err != nil {
		t.Fatalf("import with huge line: %v", err)
	}
	if sum.Imported != 2 || len(sum.Skipped) != 0 || sum.Total != 2 {
		t.Fatalf("summary = %#v, want imported=2 skipped=0 total=2 (huge line + following record both imported)", sum)
	}
}

func TestImportFindingsNormalizedKeyCountsAsUpdate(t *testing.T) {
	s := importTestStore(t, "import-normkey")
	// Same finding identity written two ways: an un-normalised file path
	// (./sub/../a.go) and its canonical form (a.go). The second import must
	// count as an UPDATE (same normalised key), not a new import.
	first := `{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"codex","message":"m","file":"./sub/../a.go","line":3}` + "\n"
	if sum, err := s.ImportFindings(context.Background(), strings.NewReader(first), false); err != nil || sum.Imported != 1 {
		t.Fatalf("first = %#v err=%v, want imported=1", sum, err)
	}
	second := `{"ticket":"AIRA-1","category":"correctness","severity":"P1","verdict":"confirmed","source":"codex","message":"m","file":"a.go","line":3}` + "\n"
	sum, err := s.ImportFindings(context.Background(), strings.NewReader(second), false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if sum.Imported != 0 || sum.Updated != 1 {
		t.Fatalf("second = %#v, want imported=0 updated=1 (normalised key matches)", sum)
	}
	if rows, _ := s.ListFindings(""); len(rows) != 1 {
		t.Fatalf("normalised key created %d findings, want 1", len(rows))
	}
}
