package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// summaryKeys returns a summary line's keys in wire order, and its values.
func summaryParse(t *testing.T, line []byte) ([]string, map[string]any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		t.Fatalf("line is not a JSON object: %q (%v)", line, err)
	}
	var keys []string
	values := map[string]any{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			t.Fatalf("bad key in %q: %v", line, err)
		}
		key := keyToken.(string)
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("bad value for %s in %q: %v", key, line, err)
		}
		keys = append(keys, key)
		values[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		t.Fatalf("unterminated object %q: %v", line, err)
	}
	return keys, values
}

func i64(v int64) *int64 { return &v }

func ranStatus() ConfineStatus {
	return ConfineStatus{
		Slice: "aira.slice", Name: "step", Owner: "me", Containment: ConfineContainmentEnforced,
		Admission: ConfineAdmissionAdmitted, AdmissionState: "immediate", ReserveBytes: 1 << 30,
		TerminatedBy: ConfineTerminatedNormal,
	}
}

// verifies: AIRA-281 -- the never-ran shape: exactly the always-present keys plus
// `code`, in struct order, and none of the ran-only keys. Always emitting `exit`
// (0) would be a fabricated success and reds this.
func TestConfineSummaryNeverRanShape(t *testing.T) {
	status := ConfineStatus{Slice: "aira.slice", AdmissionState: "saturated"}
	line, err := FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: status},
		errors.New("E_ADMIT_SATURATED: box full"))
	if err != nil {
		t.Fatal(err)
	}
	keys, values := summaryParse(t, line)
	want := []string{"schema", "ran", "name", "owner", "argv_sha256", "slice", "containment", "admission", "reserve_bytes", "code"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if values["ran"] != false || values["code"] != "E_ADMIT_SATURATED" || values["admission"] != "saturated" {
		t.Fatalf("values = %v", values)
	}
	// Facets that were never established read unevaluated, not "" and not 0.
	for _, key := range []string{"name", "owner", "containment", "reserve_bytes"} {
		if values[key] != "unevaluated" {
			t.Errorf("%s = %v, want the string unevaluated", key, values[key])
		}
	}
	// An error without a stable code is unevaluated, not blank.
	line, _ = FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{}, errors.New("no code here"))
	if _, values = summaryParse(t, line); values["code"] != "unevaluated" {
		t.Fatalf("code = %v, want unevaluated", values["code"])
	}
}

// verifies: AIRA-281 -- nil counters are the string "unevaluated" (never 0, never
// a missing key); an OBSERVED CPU of 0 is the number 0. Mutations: rendering *p
// or 0 for nil, dropping the key (omitempty on a nil pointer), clamping CPU 0.
func TestConfineSummaryRanNilCountersAreUnevaluatedAndZeroCPUIsZero(t *testing.T) {
	line, err := FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: ranStatus()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys, values := summaryParse(t, line)
	want := []string{"schema", "ran", "name", "owner", "argv_sha256", "slice", "containment", "admission", "reserve_bytes",
		"exit", "terminated_by", "wall_us", "peak_rss_bytes", "cpu_user_us", "cpu_sys_us",
		"rusage_user_us", "rusage_sys_us", "rusage_maxrss_largest_process_bytes"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if values["ran"] != true || values["exit"] != json.Number("0") {
		t.Fatalf("ran/exit = %v/%v", values["ran"], values["exit"])
	}
	for _, key := range []string{"wall_us", "peak_rss_bytes", "cpu_user_us", "cpu_sys_us", "rusage_user_us", "rusage_sys_us", "rusage_maxrss_largest_process_bytes"} {
		if values[key] != "unevaluated" {
			t.Errorf("%s = %#v, want the string unevaluated", key, values[key])
		}
	}

	status := ranStatus()
	status.CPUUser, status.CPUSys = i64(0), i64(0)
	status.RusageUserUS, status.RusageSysUS = i64(0), i64(0)
	line, _ = FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: status}, nil)
	_, values = summaryParse(t, line)
	for _, key := range []string{"cpu_user_us", "cpu_sys_us", "rusage_user_us", "rusage_sys_us"} {
		if values[key] != json.Number("0") {
			t.Errorf("%s = %#v, want the number 0 (a real observation)", key, values[key])
		}
	}

	// Counters for which 0 is impossible never become a number 0.
	status = ranStatus()
	status.PeakRSS, status.RusageMaxRSS, status.WallUS, status.ReserveBytes = i64(0), i64(-1), i64(0), 0
	line, _ = FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: status}, nil)
	_, values = summaryParse(t, line)
	for _, key := range []string{"peak_rss_bytes", "rusage_maxrss_largest_process_bytes", "wall_us", "reserve_bytes"} {
		if values[key] != "unevaluated" {
			t.Errorf("%s = %#v, want unevaluated for a non-positive reading", key, values[key])
		}
	}
}

// verifies: AIRA-281 -- established values are raw integers (bytes, microseconds),
// not the trailer's "4G" or "1.5s" text.
func TestConfineSummaryEstablishedValuesAreRawIntegers(t *testing.T) {
	status := ranStatus()
	status.WallUS, status.PeakRSS = i64(1_500_000), i64(4<<30)
	status.CPUUser, status.CPUSys = i64(7_000_001), i64(3)
	status.RusageUserUS, status.RusageSysUS, status.RusageMaxRSS = i64(11), i64(22), i64(64<<20)
	line, _ := FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: status, Exit: 137}, nil)
	_, values := summaryParse(t, line)
	for key, want := range map[string]string{
		"exit": "137", "reserve_bytes": "1073741824", "wall_us": "1500000", "peak_rss_bytes": "4294967296",
		"cpu_user_us": "7000001", "cpu_sys_us": "3", "rusage_user_us": "11", "rusage_sys_us": "22",
		"rusage_maxrss_largest_process_bytes": "67108864",
	} {
		if values[key] != json.Number(want) {
			t.Errorf("%s = %#v, want the number %s", key, values[key], want)
		}
	}
	if values["terminated_by"] != "normal" {
		t.Errorf("terminated_by = %v", values["terminated_by"])
	}
	// terminated_by is written whatever the exit code (an exit 0 never implies normal).
	status.TerminatedBy = "supervisor-signal:SIGTERM"
	line, _ = FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: status, Exit: 0}, nil)
	if _, values = summaryParse(t, line); values["exit"] != json.Number("0") || values["terminated_by"] != "supervisor-signal:SIGTERM" {
		t.Fatalf("exit/terminated_by = %v/%v", values["exit"], values["terminated_by"])
	}
	status.TerminatedBy = ""
	line, _ = FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: status}, nil)
	if _, values = summaryParse(t, line); values["terminated_by"] != "unevaluated" {
		t.Fatalf("empty TerminatedBy = %v, want unevaluated", values["terminated_by"])
	}
}

// verifies: AIRA-281 -- argv_sha256 is the sha256 of the argv joined by NUL, with
// no trailing NUL: a known vector. Joining with spaces, or adding a trailing NUL,
// reds this.
func TestConfineSummaryArgvHashVector(t *testing.T) {
	const want = "3f49099c53e1778c944d936c640571dbbab67493bc2350de4ec78e9c5fd73b57" // printf 'sh\0-c\0echo hi' | sha256sum
	line, _ := FormatConfineSummary(ConfineRequest{Argv: []string{"sh", "-c", "echo hi"}}, ConfineResult{Status: ranStatus()}, nil)
	if _, values := summaryParse(t, line); values["argv_sha256"] != want {
		t.Fatalf("argv_sha256 = %v, want %s", values["argv_sha256"], want)
	}
	// Argument boundaries matter: ["a b"] and ["a","b"] hash differently.
	if confineArgvSHA256([]string{"a b"}) == confineArgvSHA256([]string{"a", "b"}) {
		t.Fatal("argv boundaries are not part of the hash")
	}
}

// verifies: AIRA-281 -- exactly one trailing newline and no other, parseable at
// the worst-case sizes, with `<` and `&` left unescaped.
func TestConfineSummaryLineShape(t *testing.T) {
	status := ranStatus()
	status.Name = strings.Repeat("n", 97) + "<&>"
	status.Owner = "@" + strings.Repeat("o", 63)
	status.Slice = strings.Repeat("s", 255)
	max := int64(math.MaxInt64)
	status.ReserveBytes, status.WallUS, status.PeakRSS = max, i64(max), i64(max)
	status.CPUUser, status.CPUSys = i64(max), i64(max)
	status.RusageUserUS, status.RusageSysUS, status.RusageMaxRSS = i64(max), i64(max), i64(max)
	request := ConfineRequest{Argv: []string{"x\ny"}, SummaryTreeHash: strings.Repeat("t", 128)}
	line, err := FormatConfineSummary(request, ConfineResult{Status: status, Exit: math.MaxInt32}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(line, []byte("}\n")) || bytes.Count(line, []byte("\n")) != 1 {
		t.Fatalf("line must end in exactly one newline and hold no other: %q", line)
	}
	if !bytes.Contains(line, []byte("<&>")) || bytes.Contains(line, []byte("\\u003c")) || bytes.Contains(line, []byte("\\u0026")) {
		t.Fatalf("HTML characters were escaped: %q", line)
	}
	_, values := summaryParse(t, line)
	if values["name"] != status.Name || values["peak_rss_bytes"] != json.Number("9223372036854775807") {
		t.Fatalf("worst-case values did not round-trip: %v", values)
	}
	// The worst legitimate record is about 1.3 KiB: nothing is silently truncated.
	if len(line) > 2048 {
		t.Fatalf("worst-case line is %d bytes", len(line))
	}
}

// verifies: AIRA-281 -- tree_hash is absent when unset, verbatim when set, and the
// validator refuses empty, 129 bytes, a space, a quote and a newline. Removing the
// alphabet check reds the last three.
func TestConfineSummaryTreeHash(t *testing.T) {
	line, _ := FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: ranStatus()}, nil)
	if keys, _ := summaryParse(t, line); containsKey(keys, "tree_hash") {
		t.Fatalf("tree_hash present although none was supplied: %v", keys)
	}
	line, _ = FormatConfineSummary(ConfineRequest{Argv: []string{"x"}, SummaryTreeHash: "sha256:AbC.d_e-f+g/h="}, ConfineResult{Status: ranStatus()}, nil)
	keys, values := summaryParse(t, line)
	if values["tree_hash"] != "sha256:AbC.d_e-f+g/h=" {
		t.Fatalf("tree_hash = %v", values["tree_hash"])
	}
	// Positioned right after argv_sha256.
	for i, key := range keys {
		if key == "tree_hash" && keys[i-1] != "argv_sha256" {
			t.Fatalf("tree_hash is after %q, want argv_sha256", keys[i-1])
		}
	}
	for _, good := range []string{"a", strings.Repeat("a", 128), "sha256:" + strings.Repeat("0", 64), "AZaz09._:+/=-"} {
		if err := ValidateConfineSummaryTreeHash(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
	for name, bad := range map[string]string{
		"empty": "", "129 bytes": strings.Repeat("a", 129), "space": "a b", "quote": `a"b`,
		"newline": "a\nb", "backslash": `a\b`, "non-ascii": "café", "tab": "a\tb",
	} {
		if err := ValidateConfineSummaryTreeHash(bad); err == nil {
			t.Errorf("%s: %q accepted", name, bad)
		}
	}
}

func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

// verifies: AIRA-281 -- the trailer gains wall=, rendered directly after cpu=, and
// reads unevaluated (never 0) when not established. Omitting the facet when nil
// reds the first arm.
func TestFormatConfineStatusRendersWallFacet(t *testing.T) {
	line := FormatConfineStatus(ConfineStatus{Slice: "aira.slice", CPUUser: i64(1_000_000), CPUSys: i64(2_000_000)})
	if !strings.HasSuffix(line, " cpu=1s+2s wall=unevaluated") {
		t.Fatalf("nil wall must read unevaluated, directly after cpu=: %q", line)
	}
	line = FormatConfineStatus(ConfineStatus{Slice: "aira.slice", WallUS: i64(1_500_000)})
	if !strings.HasSuffix(line, " cpu=unevaluated wall=1.5s") {
		t.Fatalf("wall = 1.5s must follow cpu=: %q", line)
	}
	if strings.Count(line, "wall=") != 1 {
		t.Fatalf("exactly one wall= facet expected: %q", line)
	}
	// The never-ran trailer is unchanged.
	never := FormatConfineNeverRan(ConfineStatus{}, errors.New("E_X: y"))
	if strings.Contains(never, "wall=") {
		t.Fatalf("never-ran trailer gained wall=: %q", never)
	}
}

// verifies: AIRA-281 -- the trailer renders the admission facet through the same
// derivation as the summary (AdmissionState first, then Admission).
func TestConfineAdmissionFacetDerivation(t *testing.T) {
	if got := confineAdmissionFacet(ConfineStatus{AdmissionState: "immediate", Admission: ConfineAdmissionAdmitted}); got != "immediate" {
		t.Fatalf("state must win: %q", got)
	}
	if got := confineAdmissionFacet(ConfineStatus{Admission: ConfineAdmissionTimeout}); got != "timeout" {
		t.Fatalf("fallback to Admission: %q", got)
	}
	if got := confineAdmissionFacet(ConfineStatus{}); got != "" {
		t.Fatalf("nothing established must be empty: %q", got)
	}
}
