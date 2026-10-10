package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// AIRA-280. The paged handlers: whole subjects, a 4 MiB byte budget that also
// covers the dump's first-page live state, and an exact `next`.

func recordLongSubject(t *testing.T, db *store.DB, letter string, signatureBytes int, at time.Time) string {
	t.Helper()
	signature := strings.Repeat(letter, signatureBytes)
	peak := int64(1 << 20)
	if err := db.RecordConfinePeak(context.Background(), store.ResourcePeakObservation{
		Kind: store.ResourcePeakKindConfine, Signature: signature, Peak: &peak, At: at,
	}); err != nil {
		t.Fatal(err)
	}
	return signature
}

func dumpPageArgs(kind, signature string) map[string]any {
	args := map[string]any{"owner": "session-a", "paged": true}
	if kind != "" {
		args["after_kind"], args["after_signature"] = kind, signature
	}
	return args
}

func decodeDumpPage(t *testing.T, response core.Response) runner.ConfineDumpResult {
	t.Helper()
	if !response.OK {
		t.Fatalf("response = %+v", response)
	}
	return response.Data.(runner.ConfineDumpResult)
}

// dumpPages reads every page and returns them.
func dumpPages(t *testing.T, server *Server) []runner.ConfineDumpResult {
	t.Helper()
	var pages []runner.ConfineDumpResult
	kind, signature := "", ""
	for len(pages) < 50 {
		page := decodeDumpPage(t, server.confineDump(dumpPageArgs(kind, signature)))
		pages = append(pages, page)
		if page.Next == nil {
			return pages
		}
		kind, signature = page.Next.Kind, page.Next.Signature
	}
	t.Fatal("paging did not terminate")
	return nil
}

// verifies: AIRA-280 — a dump page never exceeds the byte budget (unless it holds
// one subject), `next` is present exactly when more remain, and a history whose
// last page is exactly full ends with that page, not an extra empty one.
func TestConfineDumpPagesAreByteBoundedAndExact(t *testing.T) {
	server := ciDumpTestServer(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// 1.2 MiB signatures: two subjects (plus the cursor that repeats the last
	// signature) fit in 4 MiB, three do not. Four subjects
	// are therefore exactly two full pages.
	var signatures []string
	for i, letter := range []string{"a", "b", "c", "d"} {
		signatures = append(signatures, recordLongSubject(t, server.db, letter, 1200*1024, base.Add(time.Duration(i)*time.Minute)))
	}
	pages := dumpPages(t, server)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want exactly 2 (an exactly-full last page must not be followed by an empty one)", len(pages))
	}
	for index, page := range pages {
		if len(page.Admissions) != 2 {
			t.Fatalf("page %d holds %d subjects' rows, want 2", index, len(page.Admissions))
		}
		encoded, _ := json.Marshal(page)
		if len(encoded) > confineHistoryPageBytes+4096 {
			t.Fatalf("page %d is %d bytes, over the %d budget", index, len(encoded), confineHistoryPageBytes)
		}
	}
	if pages[0].Next == nil || pages[0].Next.Kind != "confine" || pages[0].Next.Signature != signatures[1] {
		t.Fatalf("page 0 next = %+v, want the cursor of its last subject", pages[0].Next)
	}
	if pages[1].Next != nil {
		t.Fatalf("the last page carries next = %+v", pages[1].Next)
	}
	var got []string
	for _, page := range pages {
		for _, row := range page.Admissions {
			got = append(got, row.Signature)
		}
	}
	if strings.Join(got, "|") != strings.Join(signatures, "|") {
		t.Fatalf("rows across pages are not the history in order")
	}
}

// verifies: AIRA-280 — Waiters and Queues (live state) ride on the first page
// only, and the first page's budget is charged with them: a large waiter set
// leaves room for fewer history subjects, keeping the whole page within budget.
func TestConfineDumpLiveStateIsFirstPageOnlyAndCharged(t *testing.T) {
	server := ciDumpTestServer(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	server.admitNow = func() time.Time { return now }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "no memory reader configured for this test"
	}
	name := strings.Repeat("n", 500)
	injectCIDumpQueue(server, "/aira.slice", func(queue *sliceQueue) {
		for i := 0; i < 4800; i++ {
			queue.waiters = append(queue.waiters, &admitWaiter{name: name, state: admitQueued, enqueued: now.Add(-time.Second), reserve: 1 << 20})
		}
	})
	_, waiters := server.buildConfineDumpQueuesAndWaiters()
	charged := jsonLen(waiters)
	if charged < 3_000_000 || charged > 3_300_000 {
		t.Fatalf("fixture drifted: live state is %d bytes, want 3.0-3.3 MB (room for one 400 KB subject and its cursor, not two)", charged)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, letter := range []string{"a", "b", "c"} {
		recordLongSubject(t, server.db, letter, 400*1024, base.Add(time.Duration(i)*time.Minute))
	}
	pages := dumpPages(t, server)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2 (live state leaves room for one subject on page 1)", len(pages))
	}
	if len(pages[0].Waiters) != 4800 || len(pages[0].Queues) != 1 {
		t.Fatalf("page 0 live state: %d waiters, %d queues", len(pages[0].Waiters), len(pages[0].Queues))
	}
	if len(pages[1].Waiters) != 0 || len(pages[1].Queues) != 0 {
		t.Fatalf("page 1 repeats live state: %d waiters, %d queues", len(pages[1].Waiters), len(pages[1].Queues))
	}
	for index, page := range pages {
		encoded, _ := json.Marshal(page)
		if len(encoded) > confineHistoryPageBytes+4096 {
			t.Fatalf("page %d is %d bytes, over the %d budget (live state not charged?)", index, len(encoded), confineHistoryPageBytes)
		}
	}
	if len(pages[0].Admissions) != 1 || len(pages[1].Admissions) != 2 {
		t.Fatalf("admissions per page = %d, %d; want 1, 2", len(pages[0].Admissions), len(pages[1].Admissions))
	}
}

// verifies: AIRA-280 — without `paged` the handlers behave as before: one reply,
// everything in it, live state included, no next.
func TestUnpagedConfineDumpAndBudgetAreUnchanged(t *testing.T) {
	server := ciDumpTestServer(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, letter := range []string{"a", "b", "c"} {
		recordLongSubject(t, server.db, letter, 100, base.Add(time.Duration(i)*time.Minute))
	}
	dump := decodeDumpPage(t, server.confineDump(map[string]any{"owner": "session-a"}))
	if len(dump.Admissions) != 3 || dump.Next != nil {
		t.Fatalf("unpaged dump: %d rows, next=%v", len(dump.Admissions), dump.Next)
	}
	response := server.confineBudget(map[string]any{"owner": "session-a"})
	budget := response.Data.(runner.ConfineBudgetResult)
	if len(budget.Subjects) != 3 || budget.Next != nil {
		t.Fatalf("unpaged budget: %d rows, next=%v", len(budget.Subjects), budget.Next)
	}
}

// verifies: AIRA-280 — the budget pages are byte-bounded, exact, and each page is
// sorted worst-first; every subject appears once.
func TestConfineBudgetPagesAreByteBoundedAndExact(t *testing.T) {
	server, db := budgetTestServer(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, letter := range []string{"a", "b", "c", "d"} {
		recordLongSubject(t, db, letter, 1200*1024, base.Add(time.Duration(i)*time.Minute))
	}
	var pages []runner.ConfineBudgetResult
	kind, signature := "", ""
	for len(pages) < 20 {
		args := map[string]any{"owner": "session-a", "paged": true}
		if kind != "" {
			args["after_kind"], args["after_signature"] = kind, signature
		}
		response := server.confineBudget(args)
		if !response.OK {
			t.Fatalf("response = %+v", response)
		}
		page := response.Data.(runner.ConfineBudgetResult)
		pages = append(pages, page)
		if page.Next == nil {
			break
		}
		kind, signature = page.Next.Kind, page.Next.Signature
	}
	if len(pages) != 2 || len(pages[0].Subjects) != 2 || len(pages[1].Subjects) != 2 || pages[1].Next != nil {
		t.Fatalf("pages = %d (%d, %d subjects), want 2 pages of 2 with a next-less last page", len(pages), len(pages[0].Subjects), len(pages[1].Subjects))
	}
	for index, page := range pages {
		encoded, _ := json.Marshal(page)
		if len(encoded) > confineHistoryPageBytes+4096 {
			t.Fatalf("page %d is %d bytes, over the %d budget", index, len(encoded), confineHistoryPageBytes)
		}
	}
}

// verifies: AIRA-280 — bad paging arguments are refused, never ignored: a cursor
// without `paged`, half a cursor, and an unknown kind.
func TestPagedHistoryArgumentsAreValidated(t *testing.T) {
	server := ciDumpTestServer(t)
	for _, args := range []map[string]any{
		{"owner": "session-a", "after_kind": "confine", "after_signature": "x"},
		{"owner": "session-a", "paged": true, "after_kind": "confine"},
		{"owner": "session-a", "paged": true, "after_signature": "x"},
		{"owner": "session-a", "paged": true, "after_kind": "bogus", "after_signature": "x"},
	} {
		for name, call := range map[string]func(map[string]any) core.Response{"dump": server.confineDump, "budget": server.confineBudget} {
			response := call(args)
			if response.OK || response.Code != "E_CONFINE_ARGUMENT_INVALID" {
				t.Fatalf("%s %v: response = %+v, want E_CONFINE_ARGUMENT_INVALID", name, args, response)
			}
		}
	}
}
