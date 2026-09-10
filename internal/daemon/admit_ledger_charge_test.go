package daemon

import "testing"

// TestLedgerChargeIsTheDeclaredReserve pins the declared-only admission model
// (S1): a granted waiter contributes its DECLARED reserve to the ledger for the
// whole lifetime of the lease, with no live-usage tracking (the AIRA-29 dynamic
// charge is retired).
//
// This is the DIRECT mutation target for S1: making ledgerCharge() consult any
// value other than waiter.reserve for a non-nil waiter must red this test. The
// end-to-end ledger accounting is separately pinned by
// admit_small_reservation_contention_test.go's "outstanding == Σreserve"
// invariant, which the same mutation also reds.
func TestLedgerChargeIsTheDeclaredReserve(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reserve int64
	}{
		{"typical", 7 << 30},
		{"zero", 0},
		{"cold-start default", 1 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A granted, accounted, scope-backed waiter -- the population that
			// would once have carried a live-tracked charge.
			w := &admitWaiter{
				seq: 1, state: admitGranted, accounted: true,
				scopeID: "CONFINE-x-1-a", reserve: tc.reserve,
			}
			if got := w.ledgerCharge(); got != tc.reserve {
				t.Fatalf("ledgerCharge() = %d, want the declared reserve %d", got, tc.reserve)
			}
		})
	}

	// The nil guard stays: a nil waiter charges nothing rather than panicking.
	if got := (*admitWaiter)(nil).ledgerCharge(); got != 0 {
		t.Fatalf("nil waiter ledgerCharge() = %d, want 0", got)
	}
}
