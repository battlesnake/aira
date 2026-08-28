package runner

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// verifies: the read-back page floor uses the kernel's real PAGE_SIZE, not a
// hardcoded 4096. Non-discriminating on 4KiB hosts (WSL x86) by nature — a
// portability guard that catches the false-fail on 16K/64K-page kernels (arm64).
func TestFloorMemoryPageUsesKernelPageSize(t *testing.T) {
	page := int64(os.Getpagesize())
	if got := floorMemoryPage(page + 1); got != page {
		t.Fatalf("floorMemoryPage(%d)=%d, want %d (kernel PAGE_SIZE floor)", page+1, got, page)
	}
	if page > 4096 {
		v := page + 4096 // 4KiB-aligned but below the next real page boundary
		if got := floorMemoryPage(v); got != page {
			t.Fatalf("floorMemoryPage(%d)=%d, want %d; a hardcoded-4KiB mask false-fails read-back on this arch", v, got, page)
		}
	}
}

// covers: portable size parsing — integer and decimal mantissas; single-letter
// and full unit spellings (K/KB/KiB, M/MB/MiB, G/GB/GiB, T/TB/TiB, B),
// case-insensitive, 1024-based.
func TestParseMemorySize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{input: "0", want: 0},
		{input: "1", want: 1},
		{input: "1025K", want: 1025 << 10},
		{input: "2m", want: 2 << 20},
		{input: "3G", want: 3 << 30},
		{input: "0004k", want: 4 << 10},
		// Widened: full unit spellings; B/iB are accepted synonyms (4G==4GB==4GiB).
		{input: "512B", want: 512},
		{input: "4KB", want: 4 << 10},
		{input: "4KiB", want: 4 << 10},
		{input: "512MB", want: 512 << 20},
		{input: "10MiB", want: 10 << 20},
		{input: "4GB", want: 4 << 30},
		{input: "4GiB", want: 4 << 30},
		{input: "4gb", want: 4 << 30},
		{input: "4gib", want: 4 << 30},
		{input: "1T", want: 1 << 40},
		{input: "2TiB", want: 2 << 40},
		// Keep these parity constants byte-identical with the Python parser test.
		{input: "1.5GB", want: 1610612736},
		{input: "0.5G", want: 536870912},
		{input: "1.05G", want: 1127428915},
		{input: "1.3K", want: 1331},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseMemorySize(test.input)
			if err != nil || got != test.want {
				t.Fatalf("parseMemorySize(%q)=(%d, %v), want (%d, nil)", test.input, got, err, test.want)
			}
		})
	}
}

// verifies: rejects every shape outside [0-9]+(\.[0-9]+)?(unit)? — malformed
// decimals, partial/garbage units, signs, whitespace, and overflow.
func TestParseMemorySizeRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{
		"", "-1", "+1", " 1M", "1M ", "1.", ".5G", "1.2.3", "1,5", "K", "12x",
		"1KI", "1Gi", "1GG", "GB", "1KBB", "1 G", "4G B",
		"1K", "1KB", // U+212A KELVIN SIGN folds to ASCII k under Unicode; must be rejected (Go/Python parity)
		"9223372036854775808", "9007199254740992K", "9007199254740992T",
	} {
		t.Run(strings.ReplaceAll(input, " ", "space"), func(t *testing.T) {
			if got, err := parseMemorySize(input); err == nil {
				t.Fatalf("parseMemorySize(%q)=%d, want error", input, got)
			}
		})
	}
	if got, err := parseMemorySize("9223372036854775807"); err != nil || got != math.MaxInt64 {
		t.Fatalf("max int64 parse=(%d, %v)", got, err)
	}
}

// covers: task-57 numeric cap validation shared by both launch verbs.
func TestValidateScopeMemoryCap(t *testing.T) {
	for _, test := range []struct {
		name      string
		maximum   int64
		high      int64
		wantError bool
	}{
		{name: "unset"},
		{name: "minimum", maximum: 1 << 20},
		{name: "with high", maximum: 32 << 20, high: 16 << 20},
		{name: "high requires max", high: 1 << 20, wantError: true},
		{name: "max below minimum", maximum: (1 << 20) - 1, wantError: true},
		{name: "high above max", maximum: 32 << 20, high: 33 << 20, wantError: true},
		{name: "negative max", maximum: -1, wantError: true},
		{name: "negative high", maximum: 32 << 20, high: -1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateScopeMemoryCap(test.maximum, test.high)
			if (err != nil) != test.wantError {
				t.Fatalf("validateScopeMemoryCap(%d,%d)=%v wantError=%v", test.maximum, test.high, err, test.wantError)
			}
		})
	}
}

// verifies: task-57 verification uses the kernel's 4 KiB floor.
func TestFloorMemoryPage(t *testing.T) {
	for input, want := range map[int64]int64{
		0: 0, 4095: 0, 4096: 4096, 4097: 4096, 1025 << 10: 1024 << 10,
	} {
		if got := floorMemoryPage(input); got != want {
			t.Fatalf("floorMemoryPage(%d)=%d want %d", input, got, want)
		}
	}
}

// verifies: task-57 unset record fields remain absent, while verified values are explicit.
func TestRunRecordScopeMemoryJSONOmitEmpty(t *testing.T) {
	plain, err := json.Marshal(RunRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "scope_memory_max") || strings.Contains(string(plain), "scope_memory_high") {
		t.Fatalf("unset scope memory fields leaked into JSON: %s", plain)
	}
	maximum, high := int64(32<<20), int64(16<<20)
	capped, err := json.Marshal(RunRecord{ScopeMemoryMax: &maximum, ScopeMemoryHigh: &high})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"scope_memory_max":33554432`, `"scope_memory_high":16777216`} {
		if !strings.Contains(string(capped), want) {
			t.Fatalf("capped record JSON %s lacks %s", capped, want)
		}
	}
}
