package runner

import (
	"fmt"
	"strconv"
	"strings"
)

// AIRA-324. The pinned-job charge factor, spelled once for the daemon (which
// applies it) and `aira install --pin-charge-factor` (which records it in the
// daemon unit). It is held as a whole percentage so the booking
// ceil(pin x factor) is exact integer arithmetic.
const (
	PinChargeFactorDefaultPercent int64 = 80
	// 1.0 books the pin: the discount is off even in enforce mode.
	PinChargeFactorMaxPercent int64 = 100
	// Below 0.5 is refused: the grant-time cap bound (sum(memory.max) <=
	// sum(charge) / factor) would pass 2x, and AIRA-325's history charge proposes
	// 0.5 as its own floor ratio, so a lower flat factor is not offered.
	PinChargeFactorMinPercent int64 = 50
)

// ParsePinChargeFactor parses a factor written as a decimal with at most two
// places ("0.8", "0.85", "1", "1.00") into a whole percentage within
// [PinChargeFactorMinPercent, PinChargeFactorMaxPercent]. Anything else is
// refused, never rounded: a factor the operator typed and silently did not get
// would be a fake pass.
func ParsePinChargeFactor(raw string) (int64, bool) {
	value := strings.TrimSpace(raw)
	whole, fraction, dotted := strings.Cut(value, ".")
	if len(whole) != 1 || len(fraction) > 2 || (dotted && fraction == "") {
		return 0, false
	}
	for _, r := range whole + fraction {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	for len(fraction) < 2 {
		fraction += "0"
	}
	hundredths, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, false
	}
	percent := units*100 + hundredths
	if percent < PinChargeFactorMinPercent || percent > PinChargeFactorMaxPercent {
		return 0, false
	}
	return percent, true
}

// FormatPinChargeFactor renders a whole percentage as a two-place decimal
// ("0.80", "1.00").
func FormatPinChargeFactor(percent int64) string {
	return fmt.Sprintf("%d.%02d", percent/100, percent%100)
}
