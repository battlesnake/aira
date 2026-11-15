package store

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"aira/internal/runner"
)

func selectRow(subject, direction string, budget, recommended *int64) runner.ConfineBudgetRow {
	return runner.ConfineBudgetRow{Kind: "confine", Subject: "confine / " + subject, Direction: direction,
		Unevaluated: direction == ResourceBudgetUnevaluated, Budget: budget, RecommendedBudget: recommended}
}

func ptr(v int64) *int64 { return &v }

// seededBudgetRows is a shuffled history: 20 under, 70 over (each with a
// distinct gap), 30 acceptable, 20 well-fitted and 500 unevaluated.
func seededBudgetRows() []runner.ConfineBudgetRow {
	var rows []runner.ConfineBudgetRow
	for i := 0; i < 20; i++ {
		rows = append(rows, selectRow(fmt.Sprintf("under-%03d", i), ResourceBudgetUnderProvisioned, ptr(1000), ptr(int64(1001+i))))
	}
	for i := 0; i < 70; i++ {
		rows = append(rows, selectRow(fmt.Sprintf("over-%03d", i), ResourceBudgetOverProvisioned, ptr(100000), ptr(int64(1000+i))))
	}
	for i := 0; i < 30; i++ {
		rows = append(rows, selectRow(fmt.Sprintf("acc-%03d", i), ResourceBudgetAcceptable, ptr(1000), nil))
	}
	for i := 0; i < 20; i++ {
		rows = append(rows, selectRow(fmt.Sprintf("well-%03d", i), ResourceBudgetWellFitted, ptr(1000), nil))
	}
	for i := 0; i < 500; i++ {
		rows = append(rows, selectRow(fmt.Sprintf("once-%03d", i), ResourceBudgetUnevaluated, nil, nil))
	}
	rand.New(rand.NewSource(336)).Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	return rows
}

func runSelection(t *testing.T, direction, limit, grep string, pages ...[]runner.ConfineBudgetRow) runner.ConfineBudgetResult {
	t.Helper()
	filter, err := ParseConfineBudgetFilter(direction, limit, grep)
	if err != nil {
		t.Fatal(err)
	}
	selector := NewConfineBudgetSelector(filter)
	for _, page := range pages {
		selector.Add(page)
	}
	var result runner.ConfineBudgetResult
	selector.Finish(&result)
	return result
}

func subjectsOf(rows []runner.ConfineBudgetRow) []string {
	var out []string
	for _, row := range rows {
		out = append(out, strings.TrimPrefix(row.Subject, "confine / "))
	}
	return out
}

// verifies: AIRA-336 — the default selection is the worst 50 actionable rows
// (every under-provisioned row by shortfall, then the over-provisioned rows with
// the most reclaimable headroom), and the envelope counts every hidden row.
func TestConfineBudgetDefaultSelectionIsTheWorstActionableRows(t *testing.T) {
	rows := seededBudgetRows()
	// Offered in three uneven pages, as the client joins them.
	result := runSelection(t, "", "", "", rows[:7], rows[7:400], rows[400:])
	got := subjectsOf(result.Subjects)
	var want []string
	for i := 19; i >= 0; i-- {
		want = append(want, fmt.Sprintf("under-%03d", i))
	}
	for i := 0; i < 30; i++ { // over gap = 100000 - (1000+i): the smallest i is the worst
		want = append(want, fmt.Sprintf("over-%03d", i))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection =\n%v\nwant\n%v", got, want)
	}
	if *result.Total != 640 || *result.Matched != 90 || *result.Returned != 50 || !*result.Truncated {
		t.Fatalf("envelope total=%d matched=%d returned=%d truncated=%v", *result.Total, *result.Matched, *result.Returned, *result.Truncated)
	}
	wantDistribution := map[string]int{ResourceBudgetUnderProvisioned: 20, ResourceBudgetOverProvisioned: 70,
		ResourceBudgetAcceptable: 30, ResourceBudgetWellFitted: 20, ResourceBudgetUnevaluated: 500}
	if !reflect.DeepEqual(result.Distribution, wantDistribution) {
		t.Fatalf("distribution = %v, want %v", result.Distribution, wantDistribution)
	}
	sum := 0
	for _, count := range result.Distribution {
		sum += count
	}
	if sum != *result.Total {
		t.Fatalf("distribution sums to %d, total %d", sum, *result.Total)
	}
}

// verifies: AIRA-336 — the bounded heap selects exactly what a full sort and
// cut would, for every limit and page split (the false-pass direction: a heap
// that keeps the wrong end would still return the right count).
func TestConfineBudgetSelectionEqualsFullSortPrefix(t *testing.T) {
	rows := seededBudgetRows()
	full := append([]runner.ConfineBudgetRow(nil), rows...)
	SortConfineBudgetRows(full)
	for _, limit := range []int{1, 2, 13, 89, 90, 91, 640, 1000} {
		result := runSelection(t, "all", fmt.Sprint(limit), "", rows[:limit%len(rows)], rows[limit%len(rows):])
		want := full
		if limit < len(full) {
			want = full[:limit]
		}
		if !reflect.DeepEqual(subjectsOf(result.Subjects), subjectsOf(want)) {
			t.Fatalf("limit %d: heap selection != sorted prefix\n%v\n%v", limit, subjectsOf(result.Subjects), subjectsOf(want))
		}
		if *result.Truncated != (limit < len(rows)) {
			t.Fatalf("limit %d: truncated=%v", limit, *result.Truncated)
		}
	}
}

// verifies: AIRA-336 — --direction all with --limit 0 returns everything, and
// is not truncated.
func TestConfineBudgetAllAndUnlimitedReturnsEverything(t *testing.T) {
	result := runSelection(t, "all", "0", "", seededBudgetRows())
	if len(result.Subjects) != 640 || *result.Returned != 640 || *result.Total != 640 || *result.Truncated {
		t.Fatalf("returned %d total=%d truncated=%v", len(result.Subjects), *result.Total, *result.Truncated)
	}
	if !sort.SliceIsSorted(result.Subjects, func(i, j int) bool { return confineBudgetRowBefore(result.Subjects[i], result.Subjects[j]) }) {
		t.Fatal("not worst-first")
	}
	// --limit 0 alone keeps the default direction set.
	actionable := runSelection(t, "", "0", "", seededBudgetRows())
	if len(actionable.Subjects) != 90 || *actionable.Total != 640 || !*actionable.Truncated {
		t.Fatalf("--limit 0: returned %d total=%d", len(actionable.Subjects), *actionable.Total)
	}
}

// verifies: AIRA-336 — each direction spelling selects its rows; --grep narrows
// the universe (total and distribution count only matching subjects).
func TestConfineBudgetDirectionAndGrep(t *testing.T) {
	for direction, want := range map[string]int{"under": 20, "over": 70, "well": 20, "acceptable": 30, "unevaluated": 500,
		ResourceBudgetUnderProvisioned: 20, ResourceBudgetOverProvisioned: 70, ResourceBudgetWellFitted: 20, "actionable": 90, "all": 640} {
		result := runSelection(t, direction, "0", "", seededBudgetRows())
		if len(result.Subjects) != want || *result.Matched != want {
			t.Fatalf("--direction %s: %d rows, want %d", direction, len(result.Subjects), want)
		}
	}
	result := runSelection(t, "all", "", "over-06", seededBudgetRows())
	if got := subjectsOf(result.Subjects); len(got) != 10 || *result.Total != 10 || result.Distribution[ResourceBudgetOverProvisioned] != 10 || result.Distribution[ResourceBudgetUnevaluated] != 0 {
		t.Fatalf("--grep over-06: %v total=%d distribution=%v", got, *result.Total, result.Distribution)
	}
	none := runSelection(t, "", "", "no-such-command", seededBudgetRows())
	if len(none.Subjects) != 0 || none.Subjects == nil || *none.Total != 0 || *none.Truncated {
		t.Fatalf("no match: %+v", none)
	}
}

// verifies: AIRA-336 — a bad --direction or --limit is refused with the stable
// argument code, never read as the default.
func TestConfineBudgetFilterRefusesBadArguments(t *testing.T) {
	for _, bad := range [][2]string{{"sideways", ""}, {"Under", ""}, {"", "-1"}, {"", "ten"}, {"", "1.5"}} {
		if _, err := ParseConfineBudgetFilter(bad[0], bad[1], ""); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID: ") {
			t.Fatalf("direction=%q limit=%q: err=%v", bad[0], bad[1], err)
		}
	}
	filter, err := ParseConfineBudgetFilter("", "", "")
	if err != nil || filter.Limit != ConfineBudgetDefaultLimit || len(filter.Directions) != 2 {
		t.Fatalf("defaults: %+v %v", filter, err)
	}
}

// verifies: AIRA-336 — within a direction the larger gap sorts first, and a row
// whose figure is withheld sorts after every row with one (never as gap 0 ahead
// of a real gap... nor ahead of it alphabetically).
func TestConfineBudgetSortWorstGapFirst(t *testing.T) {
	rows := []runner.ConfineBudgetRow{
		selectRow("a-withheld", ResourceBudgetUnderProvisioned, ptr(1000), nil),
		selectRow("b-small", ResourceBudgetUnderProvisioned, ptr(1000), ptr(1100)),
		selectRow("c-big", ResourceBudgetUnderProvisioned, ptr(1000), ptr(9000)),
		selectRow("d-over", ResourceBudgetOverProvisioned, ptr(1<<40), ptr(1)),
	}
	SortConfineBudgetRows(rows)
	if got := strings.Join(subjectsOf(rows), ","); got != "c-big,b-small,a-withheld,d-over" {
		t.Fatalf("order = %s", got)
	}
}
