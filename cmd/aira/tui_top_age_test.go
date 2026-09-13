package main

import "testing"

// formatConfineAgeShort renders at most two unit-fields: the most-significant
// non-zero unit and the next unit below it, dropping the second when it is zero.
// The two owner-given examples (1d2h3m4s -> 1d2h; 1d0h5m -> 1d) anchor the table.
//
// verifies: AIRA-233
func TestFormatConfineAgeShortShowsAtMostTwoUnitsDroppingAZeroSecond(t *testing.T) {
	const (
		s = int64(1)
		m = 60 * s
		h = 60 * m
		d = 24 * h
	)
	tests := []struct {
		name    string
		seconds int64
		want    string
	}{
		{"zero is seconds, never blank", 0, "0s"},
		{"bare seconds", 4 * s, "4s"},
		{"seconds below a minute", 45 * s, "45s"},
		{"exact minute drops the zero seconds", 1 * m, "1m"},
		{"minute and seconds", 1*m + 1*s, "1m1s"},
		{"minutes and seconds", 3*m + 4*s, "3m4s"},
		{"exact hour drops zero minutes", 1 * h, "1h"},
		{"hour, minute, seconds truncates to two units", 1*h + 1*m + 1*s, "1h1m"},
		{"hours and minutes", 2*h + 3*m + 4*s, "2h3m"},
		{"hours with a zero next unit drops it even when seconds are set", 2*h + 4*s, "2h"},
		{"exact day drops zero hours", 1 * d, "1d"},
		// The two owner-given examples.
		{"owner example: day/hour/min/sec truncates to day+hour", 1*d + 2*h + 3*m + 4*s, "1d2h"},
		{"owner example: day with zero hours drops to just the day", 1*d + 5*m, "1d"},
		{"day, hour, minute, seconds truncates to day+hour", 1*d + 1*h + 1*m + 1*s, "1d1h"},
		{"day plus a whole hour keeps the hour", 1*d + 1*h, "1d1h"},
		{"many days keep two units", 100*d + 2*h + 30*m, "100d2h"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatConfineAgeShort(test.seconds); got != test.want {
				t.Fatalf("formatConfineAgeShort(%d) = %q, want %q", test.seconds, got, test.want)
			}
		})
	}
}

// topAgeCell must render an unreadable age as "unevaluated" (the table's honesty
// convention, shared with topRAMCell/topCPUCell/confineInt), never as "0s": a
// scope whose age could not be read is not a zero-second-old scope.
//
// verifies: AIRA-233
func TestTopAgeCellRendersUnknownAgeAsUnevaluatedNotZero(t *testing.T) {
	if got := topAgeCell(nil); got != "unevaluated" {
		t.Fatalf("topAgeCell(nil) = %q, want %q", got, "unevaluated")
	}
	negative := int64(-1)
	if got := topAgeCell(&negative); got != "unevaluated" {
		t.Fatalf("topAgeCell(negative) = %q, want %q (a negative reading is not a real age)", got, "unevaluated")
	}
	value := int64(93784) // 1d2h3m4s
	if got := topAgeCell(&value); got != "1d2h" {
		t.Fatalf("topAgeCell(%d) = %q, want the compact %q", value, got, "1d2h")
	}
	zero := int64(0)
	if got := topAgeCell(&zero); got != "0s" {
		t.Fatalf("topAgeCell(0) = %q, want %q: a known-zero age is 0s, distinct from an unreadable one", got, "0s")
	}
}
