package daemon

import (
	"reflect"
	"strconv"
	"testing"

	"aira/internal/runner"
)

// AIRA-108. The ORDER and the CAP are the load-bearing half of the reservation
// rows, and the renderer cannot pin them: it is handed a slice and prints it in
// order, so a daemon that sorted wrongly — or capped before sorting — would
// still render "correctly" while hiding the exact row an operator needs.
//
// The row that matters is the OLDEST hold. AIRA-108's incident was a 52-minute
// hold among short-lived ones; a cap that dropped it, or an arrival-order list
// that buried it below thirty two-second reservations, would leave the operator
// exactly where the aggregate line left them.
//
// verifies: AIRA-108 the longest-held reservations are the ones reported.
func TestConfineReservationRowsAreLongestHeldFirstAndCapped(t *testing.T) {
	t.Run("longest-held-first", func(t *testing.T) {
		got := confineReservationRows([]admitReservationRow{
			{signature: "young", reserve: 1, heldMS: 10},
			{signature: "oldest", reserve: 1, heldMS: 3118000},
			{signature: "middle", reserve: 1, heldMS: 500},
		})
		want := []string{"oldest", "middle", "young"}
		for i, expected := range want {
			if got[i].Signature != expected {
				t.Fatalf("row %d = %q, want %q (order %+v)", i, got[i].Signature, expected, got)
			}
		}
	})

	// The cap must be applied AFTER the sort. Capping first would keep whichever
	// ten happened to arrive first and silently discard the oldest hold — the
	// single row the whole field exists to surface.
	t.Run("the-cap-keeps-the-oldest-not-the-first-seen", func(t *testing.T) {
		rows := make([]admitReservationRow, 0, 40)
		// Arrival order is youngest-first, so a cap-before-sort keeps only the
		// youngest and this fails loudly.
		for i := 0; i < 40; i++ {
			rows = append(rows, admitReservationRow{signature: "sig" + strconv.Itoa(i), reserve: 1, heldMS: int64(i)})
		}
		got := confineReservationRows(rows)
		if len(got) != runner.ConfineReservationRowLimit {
			t.Fatalf("returned %d rows, want the %d-row cap", len(got), runner.ConfineReservationRowLimit)
		}
		if got[0].Signature != "sig39" {
			t.Fatalf("oldest row = %q, want sig39 — the cap was applied before the sort, "+
				"so the longest-held reservation was discarded", got[0].Signature)
		}
		if got[len(got)-1].Signature != "sig30" {
			t.Fatalf("last kept row = %q, want sig30", got[len(got)-1].Signature)
		}
	})

	// Deterministic output: two runs of `confine --list` against an unchanged
	// ledger must not reshuffle, which would read as churn that is not happening.
	t.Run("ties-break-totally-and-deterministically", func(t *testing.T) {
		input := []admitReservationRow{
			{signature: "b", reserve: 100, heldMS: 5},
			{signature: "a", reserve: 100, heldMS: 5},
			{signature: "c", reserve: 900, heldMS: 5},
		}
		first := confineReservationRows(input)
		if first[0].Signature != "c" {
			t.Fatalf("equal ages did not break on the larger reserve: %+v", first)
		}
		if first[1].Signature != "a" || first[2].Signature != "b" {
			t.Fatalf("equal ages and reserves did not break on signature: %+v", first)
		}
		// And the input is not mutated, so a second call over the daemon's own
		// snapshot slice yields the same answer.
		if !reflect.DeepEqual(first, confineReservationRows(input)) {
			t.Fatalf("two calls disagreed; the input slice was sorted in place")
		}
		if input[0].signature != "b" {
			t.Fatalf("the caller's slice was reordered under it: %+v", input)
		}
	})

	// Every emitted row states its own state; none is left to be inferred.
	t.Run("every-row-carries-its-state", func(t *testing.T) {
		for _, row := range confineReservationRows([]admitReservationRow{{signature: "x", reserve: 1, heldMS: 1}}) {
			if row.State != runner.ConfineReservationStateHolding {
				t.Fatalf("row=%+v, want state %q", row, runner.ConfineReservationStateHolding)
			}
		}
	})

	// An empty population is nil, and the wire's omitempty then leaves the field
	// out entirely — an absence, matching the aggregate's own stated zero.
	t.Run("no-reservations-is-nil-not-an-empty-row", func(t *testing.T) {
		if got := confineReservationRows(nil); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
}
