package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AIRA-283. The CPU-slots-per-core ratio rides in the install-mode record.

// verifies: AIRA-283 — a recorded ratio round-trips, and the boundaries 1 and 64
// are honoured as themselves.
func TestInstallModeRecordCPUSlotsPerCoreRoundTrip(t *testing.T) {
	for _, ratio := range []int{1, 2, 3, 64} {
		path := filepath.Join(t.TempDir(), "install-mode.json")
		record := InstallModeRecord{Schema: 1, Mode: ConfineModeReal, CPUSlotsPerCore: ratio}
		if err := WriteInstallModeRecord(path, record); err != nil {
			t.Fatal(err)
		}
		got, ok := ReadInstallModeRecord(path)
		if !ok {
			t.Fatalf("R=%d: a record carrying a valid ratio was not readable", ratio)
		}
		if got.CPUSlotsPerCore != ratio {
			t.Fatalf("R=%d: recorded ratio read back as %d", ratio, got.CPUSlotsPerCore)
		}
		effective, problem := got.EffectiveCPUSlotsPerCore()
		if effective != ratio || problem != "" {
			t.Fatalf("R=%d: effective=(%d,%q), want (%d,\"\")", ratio, effective, problem, ratio)
		}
	}
}

// verifies: AIRA-283 invariant 1 — an absent ratio (every record written before
// AIRA-283) and an explicit 0 both mean the default 2, never "zero slots", and
// are not reported as a problem. An omitted ratio is not written at all, so an
// explicit =2 stays distinguishable from omission on disk.
func TestInstallModeRecordAbsentOrZeroRatioIsTheDefault(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"absent": `{"schema":1,"mode":"real-slice"}`,
		"zero":   `{"schema":1,"mode":"real-slice","cpu_slots_per_core":0}`,
		"null":   `{"schema":1,"mode":"real-slice","cpu_slots_per_core":null}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		record, ok := ReadInstallModeRecord(path)
		if !ok {
			t.Fatalf("%s: record unreadable", name)
		}
		effective, problem := record.EffectiveCPUSlotsPerCore()
		if DefaultCPUSlotsPerCore != 2 {
			t.Fatalf("DefaultCPUSlotsPerCore=%d; the shipped default must stay 2 (invariant 1)", DefaultCPUSlotsPerCore)
		}
		if effective != 2 || problem != "" {
			t.Fatalf("%s: effective=(%d,%q), want (2,\"\")", name, effective, problem)
		}
	}

	omitted := filepath.Join(dir, "omitted.json")
	if err := WriteInstallModeRecord(omitted, InstallModeRecord{Schema: 1, Mode: ConfineModeReal}); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(dir, "explicit.json")
	if err := WriteInstallModeRecord(explicit, InstallModeRecord{Schema: 1, Mode: ConfineModeReal, CPUSlotsPerCore: 2}); err != nil {
		t.Fatal(err)
	}
	omittedBytes, _ := os.ReadFile(omitted)
	explicitBytes, _ := os.ReadFile(explicit)
	if strings.Contains(string(omittedBytes), "cpu_slots_per_core") {
		t.Fatalf("an omitted ratio was written to disk:\n%s", omittedBytes)
	}
	if !strings.Contains(string(explicitBytes), `"cpu_slots_per_core": 2`) {
		t.Fatalf("an explicit =2 was not recorded distinctly from omission:\n%s", explicitBytes)
	}
}

// verifies: AIRA-283 invariant 3 + plan §3.1 lenient decode — a string,
// fractional, overflowing or out-of-range ratio in the record resolves to the
// default with a stated problem, and NEVER invalidates the record. Invalidating
// it would flip a ci-shim box onto the real path (no budget, ungated), the
// dangerous direction, so the mode must survive every one of these.
func TestInstallModeRecordUnusableRatioNeverInvalidatesTheRecord(t *testing.T) {
	dir := t.TempDir()
	for name, value := range map[string]string{
		"string":      `"3"`,
		"fractional":  `1.5`,
		"exponent":    `1e2`,
		"overflowing": `99999999999999999999999`,
		"negative":    `-1`,
		"above range": `65`,
		"huge int":    `2147483648`,
		"boolean":     `true`,
		"object":      `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			content := `{"schema":1,"mode":"ci-shim","shim_budget_bytes":8589934592,"shim_budget_source":"declared","cpu_slots_per_core":` + value + `}`
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			record, ok := ReadInstallModeRecord(path)
			if !ok {
				t.Fatalf("a bad ratio %s invalidated the whole record; a shim box would fall to the real path", value)
			}
			if record.Mode != ConfineModeShim || record.ShimBudgetBytes != 8<<30 {
				t.Fatalf("record mode/budget changed by a bad ratio: %+v", record)
			}
			effective, problem := record.EffectiveCPUSlotsPerCore()
			if effective != DefaultCPUSlotsPerCore {
				t.Fatalf("bad ratio %s resolved to %d, want the default %d", value, effective, DefaultCPUSlotsPerCore)
			}
			if problem == "" {
				t.Fatalf("bad ratio %s resolved silently; it must state a problem for the daemon's log line", value)
			}

			t.Setenv(InstallModeFileEnv, path)
			resetConfineModeCache()
			t.Cleanup(resetConfineModeCache)
			if got := ResolveConfineMode(); got != ConfineModeShim {
				t.Fatalf("ResolveConfineMode()=%q with a bad ratio, want ci-shim", got)
			}
		})
	}
}
