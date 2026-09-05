package daemon

import (
	"bytes"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"aira/internal/core"
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

// AIRA-108 build-review (Sol, P1). A client-supplied signature is bounded WHERE
// IT IS RETAINED, and this is an availability property, not neatness.
//
// validateAdmitArgs accepts `signature` as any string, so its only real limit is
// the 16 MiB admit frame. Before this bound, a handful of reservations carrying
// multi-megabyte signatures pushed the whole confine-list response past
// MaxFrameBytes — writeFrame then refuses the ENTIRE response and `confine
// --list` stops working for every job on that slice. A diagnostic added to
// explain a wedge would itself have become one.
//
// verifies: AIRA-108
func TestConfineReservationSignaturesAreBoundedOnTheWire(t *testing.T) {
	t.Run("an-absurd-signature-is-truncated-and-marked", func(t *testing.T) {
		huge := strings.Repeat("x", 4<<20)
		bounded := boundedAdmitSignature(huge)
		if len([]rune(bounded)) != runner.ConfineReservationSignatureWireLimit+1 {
			t.Fatalf("bounded to %d runes, want the %d-rune limit plus a truncation marker",
				len([]rune(bounded)), runner.ConfineReservationSignatureWireLimit)
		}
		if !strings.HasSuffix(bounded, "…") {
			t.Fatalf("truncation was not marked: %q", bounded[len(bounded)-8:])
		}
	})

	// A realistic pytest nodeid must survive intact — a bound that clipped real
	// signatures would trade one diagnostic failure for another.
	t.Run("a-real-nodeid-is-untouched", func(t *testing.T) {
		real := "pytest:tools/correctness/test_dispatch_coverage.py::test_board_temp_range_has_a_reader_and_is_not_on_the_allowlist"
		if got := boundedAdmitSignature(real); got != real {
			t.Fatalf("a real nodeid was altered: %q", got)
		}
	})

	// Cutting on a rune boundary, not a byte: a split rune becomes U+FFFD in JSON,
	// so the wire would carry a corrupted string an operator cannot match against
	// a real test id.
	t.Run("truncation-keeps-valid-utf8", func(t *testing.T) {
		bounded := boundedAdmitSignature(strings.Repeat("é", 4096))
		if !utf8.ValidString(bounded) {
			t.Fatalf("truncation produced invalid UTF-8: %q", bounded)
		}
		if strings.ContainsRune(bounded, utf8.RuneError) {
			t.Fatalf("truncation split a rune: %q", bounded)
		}
	})

	// The property that actually matters, asserted through the REAL serialiser
	// rather than by arithmetic on the limit: a full slice of maximally-long
	// signatures must still produce a frame writeFrame accepts.
	t.Run("a-full-capped-response-still-serialises", func(t *testing.T) {
		rows := make([]admitReservationRow, 0, runner.ConfineReservationRowLimit)
		for i := 0; i < runner.ConfineReservationRowLimit; i++ {
			rows = append(rows, admitReservationRow{
				signature: boundedAdmitSignature(strings.Repeat("x", 4<<20)),
				reserve:   1 << 30, heldMS: int64(i),
			})
		}
		result := runner.ConfineListResult{
			Verdict: "pass", Scopes: []runner.ConfineRecord{},
			SliceReserve: &runner.ConfineSliceReserve{
				ReservationJobs: runner.ConfineReservationRowLimit,
				Reservations:    confineReservationRows(rows),
			},
		}
		var buffer bytes.Buffer
		if err := writeFrame(&buffer, responseFrame(core.Response{OK: true, Code: "OK", Data: result})); err != nil {
			t.Fatalf("a capped confine-list response was refused by its own serialiser: %v", err)
		}
		if buffer.Len() > MaxFrameBytes {
			t.Fatalf("frame is %d bytes, over the %d limit", buffer.Len(), MaxFrameBytes)
		}
	})

	// And the unbounded shape really would have broken it — so the test above is
	// not vacuously green against a build that never had the bound.
	t.Run("the-unbounded-shape-would-have-broken-the-frame", func(t *testing.T) {
		rows := make([]admitReservationRow, 0, runner.ConfineReservationRowLimit)
		for i := 0; i < runner.ConfineReservationRowLimit; i++ {
			rows = append(rows, admitReservationRow{signature: strings.Repeat("x", 4<<20), reserve: 1 << 30, heldMS: int64(i)})
		}
		result := runner.ConfineListResult{
			Verdict: "pass", Scopes: []runner.ConfineRecord{},
			SliceReserve: &runner.ConfineSliceReserve{Reservations: confineReservationRows(rows)},
		}
		var buffer bytes.Buffer
		if err := writeFrame(&buffer, responseFrame(core.Response{OK: true, Code: "OK", Data: result})); err == nil {
			t.Fatalf("40MiB of signatures serialised into a %d-byte frame; the frame limit is not the "+
				"backstop this test assumes, so the bound above is not load-bearing", buffer.Len())
		}
	})
}
