package daemon

import (
	"fmt"
	"log"
	"os"
	"strings"

	"aira/internal/runner"
)

// AIRA-324. The flat charge discount for scope-backed `--memory-reserve` pins
// (spec docs/superpowers/specs/2026-11-13-history-charge-design.md section 5.2).
//
// A DISCOUNTABLE waiter (the client's `discountable` admit field: a scope-backed
// job whose cap is the operator's own --memory-reserve, and nothing else) is
// BOOKED on the ledger at ceil(factor x pin), clamped to [1 MiB, pin], while its
// grant still echoes the pin and its scope memory.max stays the pin. A
// discounted waiter is granted only if its FULL pin also fits the slice's real
// free memory (the physical-fit rule in evaluateAdmitQueue).
//
// Three modes, on the slice-ceiling pattern (an env value the installed unit
// renders, `aira install --pin-charge`):
//
//   - off      no discount, no extra work: today's behaviour exactly.
//   - observe  no discount. A discountable grant logs what enforce WOULD have
//     booked and the physical-fit margin at that instant. Every admission
//     decision is the same as off: every waiter's booking is its reserve.
//   - enforce  the discount and the physical-fit rule apply.
//
// The daemon's raw default is off (a daemon started outside the installed unit,
// and every test, is unchanged); `aira install` renders observe. ci-shim mode
// never discounts whatever the mode: it has no physical reading to fit a pin
// against (pinChargeFor's shimMode guard).
type pinChargeMode string

const (
	pinChargeOff     pinChargeMode = "off"
	pinChargeObserve pinChargeMode = "observe"
	pinChargeEnforce pinChargeMode = "enforce"

	// The factor's default and bounds are runner's (shared with `aira install`):
	// 0.8 by default, refused outside [0.5, 1].
	pinChargeFactorDefault = runner.PinChargeFactorDefaultPercent

	// pinChargeFloor is the smallest booking the discount may produce (spec:
	// clamp(ceil(factor x pin), 1 MiB, pin)). It equals runner.MinPinnedScopeCap,
	// the smallest pin the client accepts, so a discounted booking is never below
	// the smallest reserve a caller may declare.
	pinChargeFloor int64 = 1 << 20
)

// pinChargeModeFromEnv reads AIRA_DAEMON_PIN_CHARGE_MODE, default off, on the
// slice ceiling's mode-then-the-rest shape: an unknown value refuses to start the
// daemon rather than running a mode nobody chose.
func pinChargeModeFromEnv() (pinChargeMode, error) {
	mode := pinChargeMode(strings.TrimSpace(os.Getenv("AIRA_DAEMON_PIN_CHARGE_MODE")))
	if mode == "" {
		return pinChargeOff, nil
	}
	switch mode {
	case pinChargeOff, pinChargeObserve, pinChargeEnforce:
		return mode, nil
	default:
		return "", fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_PIN_CHARGE_MODE must be off, observe, or enforce")
	}
}

// pinChargeConfigFromEnv reads the mode and, only when the mode is not off, the
// factor (AIRA_DAEMON_PIN_CHARGE_FACTOR, default 0.8): a typo in the factor must
// not refuse to start a daemon that did not ask for the feature, the same
// ordering sliceCeilingConfigFromEnv keeps.
func pinChargeConfigFromEnv() (pinChargeMode, int64, error) {
	mode, err := pinChargeModeFromEnv()
	if err != nil {
		return "", 0, err
	}
	if mode == pinChargeOff {
		return mode, pinChargeFactorDefault, nil
	}
	raw := strings.TrimSpace(os.Getenv("AIRA_DAEMON_PIN_CHARGE_FACTOR"))
	if raw == "" {
		return mode, pinChargeFactorDefault, nil
	}
	percent, ok := runner.ParsePinChargeFactor(raw)
	if !ok {
		return "", 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_PIN_CHARGE_FACTOR must be a decimal from 0.5 to 1 with at most two places, such as 0.8")
	}
	return mode, percent, nil
}

// discountedPinCharge is the booking the discount gives a pin: ceil(pin x
// percent / 100), clamped to [pinChargeFloor, pin]. Pure, so the factor and the
// clamp are pinned by a table test. A pin at or below the floor books itself; a
// pin too large for the exact product (above admitMaxReserve, which only a
// hand-crafted re-declare could carry) books itself too, the fail-safe direction.
func discountedPinCharge(pin, percent int64) int64 {
	if pin <= pinChargeFloor || percent >= 100 || percent <= 0 || pin > admitMaxReserve {
		return pin
	}
	charge := (pin*percent + 99) / 100
	if charge < pinChargeFloor {
		charge = pinChargeFloor
	}
	if charge > pin {
		charge = pin
	}
	return charge
}

// pinChargeFor is the booking a waiter with this eligibility and this reserve
// gets on THIS daemon: the discounted charge only in enforce mode, outside
// ci-shim mode, for a discountable waiter; the reserve itself otherwise. It is
// the ONE place the mode is consulted for a booking, called at enqueue and at
// the re-anchoring SET, so off and observe book every waiter at its reserve by
// construction. The mode and factor are fixed for the daemon's lifetime (read
// once in Serve), so "a mode or factor change applies to new grants only" holds
// trivially: changing either needs a restart, and a restart re-declares every
// survivor at its full pin.
func (s *Server) pinChargeFor(discountable bool, reserve int64) int64 {
	if !discountable || s.pinChargeMode != pinChargeEnforce || s.shimMode() {
		return reserve
	}
	return discountedPinCharge(reserve, s.pinChargePercent)
}

// bookingField turns a booking into the waiter's stored charge: zero when the
// booking is the reserve itself (the representation every non-discounted waiter,
// and every hand-built test waiter, already has), else the booking.
func bookingField(booking, reserve int64) int64 {
	if booking >= reserve || booking < 0 {
		return 0
	}
	return booking
}

// logPinChargeGrantLocked is the observe/enforce record of one discountable
// grant. queue.mu must be held (it reads the waiter). physicalFree is the
// slice's real free memory at this pass (ceiling - headroom - effectiveCurrent),
// the quantity the physical-fit rule compares the pin with.
//
// In observe it states what enforce WOULD have booked. An observe grant has
// already passed the ledger check at the full pin, and checkedAvailable takes
// the larger of the ledger and the physical reading, so its physical-fit verdict
// is necessarily true; the margin (physicalFree - pin) is what the line adds.
// It is a record of the grant, not a shadow ledger: enforce would also have
// granted OTHER jobs earlier, which no per-grant line can show (the spec's
// counterfactual replay on AIRA-323 data is that evidence).
func (s *Server) logPinChargeGrantLocked(waiter *admitWaiter, physicalFree int64) {
	if waiter == nil || !waiter.discountable || s.shimMode() {
		return
	}
	switch s.pinChargeMode {
	case pinChargeObserve:
		log.Printf("aira daemon: pin-charge observe: scope=%s pin=%d charged=%d would-charge=%d factor=%s physical-free=%d physical-fit=%t",
			waiter.scopeID, waiter.reserve, waiter.ledgerCharge(), discountedPinCharge(waiter.reserve, s.pinChargePercent),
			formatPinChargeFactor(s.pinChargePercent), physicalFree, waiter.reserve <= physicalFree)
	case pinChargeEnforce:
		log.Printf("aira daemon: pin-charge enforce: scope=%s pin=%d charged=%d factor=%s physical-free=%d physical-fit=%t",
			waiter.scopeID, waiter.reserve, waiter.ledgerCharge(), formatPinChargeFactor(s.pinChargePercent), physicalFree, waiter.reserve <= physicalFree)
	}
}

// formatPinChargeFactor is runner.FormatPinChargeFactor, named for the log lines.
func formatPinChargeFactor(percent int64) string {
	return runner.FormatPinChargeFactor(percent)
}
