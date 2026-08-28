package runner

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
)

const minimumScopeMemoryMax = int64(1 << 20)

// parseMemorySize parses a decimal byte count with an optional 1024-based unit.
// The unit is case-insensitive and every spelling of a scale is a synonym:
// K/KB/KiB, M/MB/MiB, G/GB/GiB, T/TB/TiB, and a bare B (bytes). So 4G == 4GB ==
// 4GiB. It is intentionally independent of Linux and cgroup files. A decimal
// mantissa is floored to whole bytes without floating-point rounding.
func parseMemorySize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("memory size is empty")
	}
	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, fmt.Errorf("memory size %q must be [0-9]+(\\.[0-9]+)? with an optional K/M/G/T (× i × B) unit", s)
	}
	mantissaEnd := digits
	fractionStart := 0
	if mantissaEnd < len(s) && s[mantissaEnd] == '.' {
		fractionStart = mantissaEnd + 1
		mantissaEnd = fractionStart
		for mantissaEnd < len(s) && s[mantissaEnd] >= '0' && s[mantissaEnd] <= '9' {
			mantissaEnd++
		}
		if mantissaEnd == fractionStart {
			return 0, fmt.Errorf("memory size %q must be [0-9]+(\\.[0-9]+)? with an optional K/M/G/T (× i × B) unit", s)
		}
	}
	var multiplier int64
	switch strings.ToUpper(s[mantissaEnd:]) {
	case "", "B":
		multiplier = 1
	case "K", "KB", "KIB":
		multiplier = 1 << 10
	case "M", "MB", "MIB":
		multiplier = 1 << 20
	case "G", "GB", "GIB":
		multiplier = 1 << 30
	case "T", "TB", "TIB":
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("memory size %q has an invalid unit; use one of K/KB/KiB, M/MB/MiB, G/GB/GiB, T/TB/TiB, or B", s)
	}
	value, err := strconv.ParseInt(s[:digits], 10, 64)
	if err != nil || value > math.MaxInt64/multiplier {
		return 0, errors.New("memory size overflows int64 bytes")
	}
	whole := value * multiplier
	if fractionStart == 0 {
		return whole, nil
	}

	// Keep the prescribed frac*scale/10^len(frac) calculation exact. math/big
	// avoids both float rounding and intermediate int64 overflow for a long frac.
	var numerator, denominator big.Int
	numerator.SetString(s[fractionStart:mantissaEnd], 10)
	numerator.Mul(&numerator, big.NewInt(multiplier))
	denominator.Exp(big.NewInt(10), big.NewInt(int64(mantissaEnd-fractionStart)), nil)
	numerator.Quo(&numerator, &denominator)
	fraction := numerator.Int64()
	if whole > math.MaxInt64-fraction {
		return 0, errors.New("memory size overflows int64 bytes")
	}
	return whole + fraction, nil
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

// floorMemoryPage rounds down to the kernel's real PAGE_SIZE. cgroup-v2 stores
// memory.max/high as (bytes / PAGE_SIZE) * PAGE_SIZE, so the read-back verify must
// compare against the same floor — hardcoding 4096 would false-fail legitimate caps
// on 16K/64K-page kernels (arm64). os.Getpagesize() returns a power of two.
func floorMemoryPage(value int64) int64 {
	page := int64(os.Getpagesize())
	return value &^ (page - 1)
}
