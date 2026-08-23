package runner

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

const minimumScopeMemoryMax = int64(1 << 20)

// parseMemorySize parses an integer byte count with an optional binary K, M,
// or G suffix. It is intentionally independent of Linux and cgroup files.
func parseMemorySize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("memory size is empty")
	}
	multiplier := int64(1)
	last := s[len(s)-1]
	switch last {
	case 'K', 'k':
		multiplier, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		multiplier, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		multiplier, s = 1<<30, s[:len(s)-1]
	}
	if s == "" {
		return 0, errors.New("memory size has no digits")
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("memory size %q must match [0-9]+[KMGkmg]?", s)
		}
	}
	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil || value > math.MaxInt64/multiplier {
		return 0, errors.New("memory size overflows int64 bytes")
	}
	return value * multiplier, nil
}

// ParseMemorySize exposes the shared portable parser to CLI and Core faces.
func ParseMemorySize(s string) (int64, error) { return parseMemorySize(s) }

func validateScopeMemoryCap(maximum, high int64) error {
	if maximum < 0 || high < 0 {
		return errors.New("memory values must be non-negative")
	}
	if maximum == 0 {
		if high > 0 {
			return errors.New("--memory-high requires --memory-max")
		}
		return nil
	}
	if maximum < minimumScopeMemoryMax {
		return errors.New("--memory-max must be at least 1MiB")
	}
	if high > maximum {
		return errors.New("--memory-high must be less than or equal to --memory-max")
	}
	return nil
}

// ValidateScopeMemoryCap exposes the common numeric relationship validation to
// the CLI and Core faces. A zero value means the corresponding option is unset.
func ValidateScopeMemoryCap(maximum, high int64) error {
	return validateScopeMemoryCap(maximum, high)
}

func floorMemoryPage(value int64) int64 { return value &^ int64(4095) }
