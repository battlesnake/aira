package runner

import "testing"

// verifies: AIRA-280 — ConfineHistoryCursor.After is a strict bytewise order on
// (kind, signature), kind first. A signature that sorts below the previous one
// but under a LATER kind is still after it.
func TestConfineHistoryCursorAfter(t *testing.T) {
	for _, tc := range []struct {
		name         string
		c, other     ConfineHistoryCursor
		wantAfter    bool
		wantReversed bool
	}{
		{"same", ConfineHistoryCursor{"confine", "a"}, ConfineHistoryCursor{"confine", "a"}, false, false},
		{"later signature", ConfineHistoryCursor{"confine", "b"}, ConfineHistoryCursor{"confine", "a"}, true, false},
		{"earlier signature", ConfineHistoryCursor{"confine", "a"}, ConfineHistoryCursor{"confine", "b"}, false, true},
		{"later kind, smaller signature", ConfineHistoryCursor{"pytest-worker", "aaa"}, ConfineHistoryCursor{"confine", "zzz"}, true, false},
		{"earlier kind, larger signature", ConfineHistoryCursor{"confine", "zzz"}, ConfineHistoryCursor{"pytest-worker", "aaa"}, false, true},
	} {
		if got := tc.c.After(tc.other); got != tc.wantAfter {
			t.Errorf("%s: %v.After(%v) = %v, want %v", tc.name, tc.c, tc.other, got, tc.wantAfter)
		}
		if got := tc.other.After(tc.c); got != tc.wantReversed {
			t.Errorf("%s: reversed After = %v, want %v", tc.name, got, tc.wantReversed)
		}
	}
}
