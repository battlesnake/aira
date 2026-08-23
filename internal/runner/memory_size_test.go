package runner

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// covers: task-57 portable [KMG] parsing, including lowercase suffixes and zero.
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

// verifies: task-57 rejects every shape outside [0-9]+[KMGkmg]? and overflow.
func TestParseMemorySizeRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", "-1", "+1", " 1M", "1M ", "1T", "1MB", "1.5G", "K", "12x", "9223372036854775808", "9007199254740992K"} {
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
