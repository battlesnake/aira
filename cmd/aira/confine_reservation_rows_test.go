package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-108. `confine --list` must NAME each scope-less reservation, not only
// count it.
//
// WHY THIS EXISTS, stated as the failure it prevents rather than as a feature.
// A granted `aira confine-reserve --pinned` helper and one still WAITING for
// admission are byte-identical in `ps` — same argv, same `--max-wait 300s` — and
// only the granted one is charged to the ledger. Before this, the only report of
// the granted population was one aggregate ("5 scope-less reservations
// 5751380K"), so an operator who saw a long-lived helper could not establish
// which of the two states it was in. Two sessions concluded from that ambiguity
// that the helper had blown its own `--max-wait`, spent hours at /proc level,
// and filed a P0 that was not there: the process was holding a valid reservation
// for a caller whose test had stopped progressing, which is exactly what the
// verb's design spec says it does ("holds the connection open, blocking on
// stdin, until stdin closes"). AIRA-68's own comment records the FIRST false P0
// from this same blind spot, so the aggregate line it added is demonstrably not
// sufficient on its own.
//
// verifies: AIRA-108 the scope-less population is named, aged and stated.
func TestRenderConfineListNamesScopelessReservations(t *testing.T) {
	render := func(t *testing.T, holds []runner.ConfineReservationHold, jobs int) string {
		t.Helper()
		reserve := runner.ConfineSliceReserve{
			GrantedBytes: 8 << 30, CeilingBytes: 61 << 30, Jobs: jobs,
			ReservationJobs: jobs, ReservationBytes: 8 << 30, Reservations: holds,
		}
		result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}, SliceReserve: &reserve}
		var stdout, stderr bytes.Buffer
		if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
		}
		return stdout.String()
	}

	hold := func(signature string, reserve, heldMS int64) runner.ConfineReservationHold {
		return runner.ConfineReservationHold{
			State: runner.ConfineReservationStateHolding, Signature: signature,
			Reserve: reserve, HeldMS: heldMS,
		}
	}

	// The line an operator reading AIRA-108's incident would have needed: the
	// state, the age, the size and the name, on one row.
	t.Run("a-row-answers-the-question-that-produced-the-false-p0", func(t *testing.T) {
		out := render(t, []runner.ConfineReservationHold{
			hold("pytest:tools/correctness/test_dispatch_coverage.py::test_board_temp_range", 1047035904, 3118_000),
		}, 1)
		want := "    state=holding held=51m58s reserve=1022496K signature=pytest:tools/correctness/test_dispatch_coverage.py::test_board_temp_range"
		if !strings.Contains(out, want) {
			t.Fatalf("stdout=%q\nwant a row containing %q", out, want)
		}
	})

	// `state=holding` is printed even though every row in this section is
	// holding. A row that does not SAY what it is leaves the reader to infer it
	// from the heading, and inferring state from context is the exact failure
	// this whole field exists to end.
	t.Run("every-row-states-its-own-state", func(t *testing.T) {
		out := render(t, []runner.ConfineReservationHold{
			hold("a", 1<<30, 1000), hold("b", 1<<30, 500),
		}, 2)
		if got := countReservationRows(out); got != 2 {
			t.Fatalf("rendered %d reservation rows, want 2; stdout=%q", got, out)
		}
	})

	// A cap that could drop the OLDEST hold would defeat the field: that row is
	// the one an operator is looking for. The daemon sends them longest-held
	// first and the renderer must not reorder them.
	t.Run("elided-rows-are-counted-never-silently-dropped", func(t *testing.T) {
		holds := make([]runner.ConfineReservationHold, 0, runner.ConfineReservationRowLimit)
		for i := 0; i < runner.ConfineReservationRowLimit; i++ {
			holds = append(holds, hold(fmt.Sprintf("pytest:t%02d", i), 1<<30, int64(1000*(50-i))))
		}
		out := render(t, holds, 37)
		if got := countReservationRows(out); got != runner.ConfineReservationRowLimit {
			t.Fatalf("rendered %d rows, want the %d-row cap; stdout=%q", got, runner.ConfineReservationRowLimit, out)
		}
		if !strings.Contains(out, "… and 27 further reservations not listed") {
			t.Fatalf("elided rows were not accounted for; stdout=%q", out)
		}
		// Longest-held first, unreordered.
		if first, last := strings.Index(out, "pytest:t00"), strings.Index(out, "pytest:t09"); first < 0 || last < 0 || first > last {
			t.Fatalf("rows were reordered away from longest-held-first; stdout=%q", out)
		}
	})

	// A daemon that predates this field sends the aggregate and NO rows. Saying
	// "and N further reservations not listed" is then the honest report; printing
	// nothing would imply the population had been enumerated and found empty.
	t.Run("a-daemon-that-sends-no-rows-reports-them-as-unlisted", func(t *testing.T) {
		out := render(t, nil, 5)
		if !strings.Contains(out, "… and 5 further reservations not listed") {
			t.Fatalf("stdout=%q", out)
		}
		if countReservationRows(out) != 0 {
			t.Fatalf("fabricated a row from an aggregate; stdout=%q", out)
		}
	})

	t.Run("singular-elision", func(t *testing.T) {
		out := render(t, nil, 1)
		if !strings.Contains(out, "… and 1 further reservation not listed") {
			t.Fatalf("stdout=%q", out)
		}
	})

	// The signature is arbitrary client-supplied text printed straight into an
	// operator's terminal. Control characters can rewrite the line, hide rows or
	// forge output, so they are escaped; an over-long value is truncated with a
	// visible marker rather than silently.
	t.Run("untrusted-signatures-cannot-rewrite-the-terminal", func(t *testing.T) {
		out := render(t, []runner.ConfineReservationHold{
			hold("evil\r\n    state=holding held=0s reserve=0B signature=forged\x1b[2K", 1<<30, 1000),
		}, 1)
		if strings.Contains(out, "\r") || strings.Contains(out, "\x1b") {
			t.Fatalf("raw control characters reached the terminal; stdout=%q", out)
		}
		// The forgery attempt is DEFEATED by escaping, not by absence: the literal
		// text "state=holding" is still present INSIDE the escaped signature, and
		// that is fine. What must not happen is a second ROW — a newline the
		// signature injected into the operator's view.
		if got := countReservationRows(out); got != 1 {
			t.Fatalf("a signature forged %d extra rows; stdout=%q", got-1, out)
		}
		long := render(t, []runner.ConfineReservationHold{hold(strings.Repeat("x", 400), 1<<30, 1000)}, 1)
		if !strings.Contains(long, "…") {
			t.Fatalf("an over-long signature was not marked as truncated; stdout=%q", long)
		}
		for _, line := range strings.Split(long, "\n") {
			if strings.Contains(line, "state=holding") && len(line) > 300 {
				t.Fatalf("an unbounded signature reached the terminal: %d chars", len(line))
			}
		}
	})

	// An absent signature is a legitimate state (`signature` is optional on the
	// admit wire). It is stated as an absence, never filled in with a guess.
	t.Run("an-absent-signature-is-stated-not-invented", func(t *testing.T) {
		out := render(t, []runner.ConfineReservationHold{hold("  ", 1<<30, 1000)}, 1)
		if !strings.Contains(out, "signature=(unnamed)") {
			t.Fatalf("stdout=%q", out)
		}
	})

	// An age the daemon could not establish must not render as "0s", which reads
	// as a brand-new hold — the opposite of what an unestablished age means, and
	// the direction that would have hidden AIRA-108's own 52-minute hold.
	t.Run("an-unestablished-age-is-unevaluated-never-zero", func(t *testing.T) {
		out := render(t, []runner.ConfineReservationHold{hold("pytest:x", 1<<30, 0)}, 1)
		if !strings.Contains(out, "held=unevaluated") {
			t.Fatalf("stdout=%q", out)
		}
	})

	// The rows are an ADDITION. The aggregate line AIRA-68 added, and its
	// explanation, must both survive.
	t.Run("the-aggregate-line-still-prints", func(t *testing.T) {
		out := render(t, []runner.ConfineReservationHold{hold("pytest:x", 8<<30, 1000)}, 1)
		if !strings.Contains(out, "1 scope-less reservation 8G") ||
			!strings.Contains(out, "never appears in the table above") {
			t.Fatalf("stdout=%q", out)
		}
	})

	// An empty population prints its stated zero and NOTHING else — no heading
	// for rows that do not exist.
	t.Run("an-empty-population-prints-no-rows-at-all", func(t *testing.T) {
		reserve := runner.ConfineSliceReserve{GrantedBytes: 0, CeilingBytes: 61 << 30}
		result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}, SliceReserve: &reserve}
		var stdout, stderr bytes.Buffer
		if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 {
			t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "0 scope-less reservations 0B") {
			t.Fatalf("stdout=%q", out)
		}
		if countReservationRows(out) != 0 || strings.Contains(out, "not listed") {
			t.Fatalf("stdout=%q", out)
		}
	})
}

// countReservationRows counts RENDERED ROWS, by their leading indent, rather
// than occurrences of the substring "state=holding". The difference is the point
// of the escaping test above: a hostile signature may legitimately contain that
// text, and what must be impossible is for it to occupy a LINE of its own.
func countReservationRows(out string) int {
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "    state=holding ") {
			rows++
		}
	}
	return rows
}
