package store

import (
	"container/heap"
	"fmt"
	"strconv"
	"strings"

	"aira/internal/runner"
)

// AIRA-336. `aira confine --budget` used to print every subject in the
// machine-wide history (180k rows, 100 MB on one JSON line), 93% of them
// unevaluated one-offs. The client now selects while it joins the daemon's
// pages: it counts every row into the distribution and keeps only the worst
// --limit rows of the selected directions in a bounded heap, so neither its
// memory nor its output grows with the history. The daemon is unchanged, so an
// older daemon is selected from exactly the same way.

// ConfineBudgetDefaultLimit is the default --limit.
const ConfineBudgetDefaultLimit = 50

// confineBudgetDirections maps every accepted --direction spelling onto the
// directions it selects. "actionable" (the default) is the two that carry a
// recommendation; the short and full names each select one direction.
var confineBudgetDirections = map[string][]string{
	"actionable":                   {ResourceBudgetUnderProvisioned, ResourceBudgetOverProvisioned},
	"all":                          nil,
	"under":                        {ResourceBudgetUnderProvisioned},
	"over":                         {ResourceBudgetOverProvisioned},
	"well":                         {ResourceBudgetWellFitted},
	ResourceBudgetUnderProvisioned: {ResourceBudgetUnderProvisioned},
	ResourceBudgetOverProvisioned:  {ResourceBudgetOverProvisioned},
	ResourceBudgetWellFitted:       {ResourceBudgetWellFitted},
	ResourceBudgetAcceptable:       {ResourceBudgetAcceptable},
	ResourceBudgetUnevaluated:      {ResourceBudgetUnevaluated},
}

// ConfineBudgetDirectionChoices lists the accepted --direction values for the
// dispatch table (help and MCP schema), in a fixed order.
func ConfineBudgetDirectionChoices() []string {
	return []string{"actionable", "under", "over", "well", "acceptable", "unevaluated", "all",
		ResourceBudgetUnderProvisioned, ResourceBudgetOverProvisioned, ResourceBudgetWellFitted}
}

// ConfineBudgetFilter is a parsed selection. Directions nil selects every
// direction; Limit 0 keeps every selected row.
type ConfineBudgetFilter struct {
	Directions map[string]bool
	Limit      int
	Grep       string
}

// ParseConfineBudgetFilter validates the three selection arguments. Empty
// direction is "actionable" and empty limit is ConfineBudgetDefaultLimit. An
// unknown direction or a limit that is not a non-negative integer is refused,
// never read as a default.
func ParseConfineBudgetFilter(direction, limit, grep string) (ConfineBudgetFilter, error) {
	filter := ConfineBudgetFilter{Limit: ConfineBudgetDefaultLimit, Grep: grep}
	if direction == "" {
		direction = "actionable"
	}
	selected, known := confineBudgetDirections[direction]
	if !known {
		return filter, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --direction %q is not one of %s", direction, strings.Join(ConfineBudgetDirectionChoices(), ", "))
	}
	if selected != nil {
		filter.Directions = map[string]bool{}
		for _, name := range selected {
			filter.Directions[name] = true
		}
	}
	if limit != "" {
		parsed, err := strconv.Atoi(limit)
		if err != nil || parsed < 0 {
			return filter, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --limit %q is not a non-negative integer (0 = every selected row)", limit)
		}
		filter.Limit = parsed
	}
	return filter, nil
}

// ConfineBudgetSelector applies a filter to rows offered one page at a time.
type ConfineBudgetSelector struct {
	filter       ConfineBudgetFilter
	kept         confineBudgetHeap
	total        int
	distribution map[string]int
}

// NewConfineBudgetSelector starts an empty selection.
func NewConfineBudgetSelector(filter ConfineBudgetFilter) *ConfineBudgetSelector {
	distribution := map[string]int{}
	for _, direction := range []string{ResourceBudgetUnderProvisioned, ResourceBudgetOverProvisioned,
		ResourceBudgetAcceptable, ResourceBudgetWellFitted, ResourceBudgetUnevaluated} {
		distribution[direction] = 0
	}
	return &ConfineBudgetSelector{filter: filter, distribution: distribution}
}

// Add offers rows. Every row matching --grep is counted; a row in the selected
// directions is kept only while it is among the worst Limit seen so far.
func (s *ConfineBudgetSelector) Add(rows []runner.ConfineBudgetRow) {
	for _, row := range rows {
		if s.filter.Grep != "" && !strings.Contains(row.Subject, s.filter.Grep) {
			continue
		}
		s.total++
		s.distribution[row.Direction]++
		if s.filter.Directions != nil && !s.filter.Directions[row.Direction] {
			continue
		}
		switch {
		case s.filter.Limit == 0 || len(s.kept) < s.filter.Limit:
			heap.Push(&s.kept, row)
		case confineBudgetRowBefore(row, s.kept[0]):
			// The root is the least bad row kept; a worse one replaces it.
			s.kept[0] = row
			heap.Fix(&s.kept, 0)
		}
	}
}

// Finish writes the selection into result: the kept rows worst-first and the
// envelope. It replaces whatever Subjects held.
func (s *ConfineBudgetSelector) Finish(result *runner.ConfineBudgetResult) {
	rows := []runner.ConfineBudgetRow(s.kept)
	if rows == nil {
		rows = []runner.ConfineBudgetRow{}
	}
	SortConfineBudgetRows(rows)
	total, returned := s.total, len(rows)
	truncated := returned < total
	result.Subjects = rows
	result.Total, result.Returned, result.Truncated = &total, &returned, &truncated
	result.Distribution = s.distribution
}

// confineBudgetHeap is a max-heap in worst-first order: its root is the row
// that sorts LAST among those kept, the one a worse arrival evicts.
type confineBudgetHeap []runner.ConfineBudgetRow

func (h confineBudgetHeap) Len() int           { return len(h) }
func (h confineBudgetHeap) Less(i, j int) bool { return confineBudgetRowBefore(h[j], h[i]) }
func (h confineBudgetHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *confineBudgetHeap) Push(x any)        { *h = append(*h, x.(runner.ConfineBudgetRow)) }
func (h *confineBudgetHeap) Pop() any {
	old := *h
	row := old[len(old)-1]
	*h = old[:len(old)-1]
	return row
}
