package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCapture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadCapturedFileWindowsAreExactAndFaultsAreClassified pins the reader
// AIRA-196 factored out of Runner.ReadOutput so run-log and confine-log share
// one definition of "a bounded window over a captured file". Each case is a
// window the two verbs must agree on byte for byte; a reader that rounded any
// of them would hand one verb different bytes than the other for the same file.
//
// verifies: AIRA-196
func TestReadCapturedFileWindowsAreExactAndFaultsAreClassified(t *testing.T) {
	path := writeCapture(t, "abcdefghij")
	for _, test := range []struct {
		name          string
		request       capturedReadRequest
		wantBytes     string
		wantOffset    int64
		wantNext      int64
		wantTruncated bool
		wantFault     capturedReadFault
	}{
		{name: "whole file", request: capturedReadRequest{Path: path}, wantBytes: "abcdefghij", wantNext: 10},
		{name: "from offset", request: capturedReadRequest{Path: path, From: 4}, wantBytes: "efghij", wantOffset: 4, wantNext: 10},
		{name: "tail", request: capturedReadRequest{Path: path, Tail: 3}, wantBytes: "hij", wantOffset: 7, wantNext: 10},
		{name: "tail larger than file clamps to zero", request: capturedReadRequest{Path: path, Tail: 99}, wantBytes: "abcdefghij", wantNext: 10},
		// From wins over Tail, exactly as Runner.ReadOutput has always behaved:
		// tail applies only when the caller named no offset.
		{name: "from suppresses tail", request: capturedReadRequest{Path: path, From: 8, Tail: 3}, wantBytes: "ij", wantOffset: 8, wantNext: 10},
		{name: "cap truncates and advances by what was returned", request: capturedReadRequest{Path: path, MaxBytes: 4}, wantBytes: "abcd", wantNext: 4, wantTruncated: true},
		{name: "offset at EOF is an empty, untruncated window", request: capturedReadRequest{Path: path, From: 10}, wantBytes: "", wantOffset: 10, wantNext: 10},
		{name: "offset past EOF is an argument fault", request: capturedReadRequest{Path: path, From: 11}, wantFault: capturedFaultArgument},
		{name: "negative offset is an argument fault", request: capturedReadRequest{Path: path, From: -1}, wantFault: capturedFaultArgument},
		{name: "negative tail is an argument fault", request: capturedReadRequest{Path: path, Tail: -1}, wantFault: capturedFaultArgument},
		{name: "negative cap is an argument fault", request: capturedReadRequest{Path: path, MaxBytes: -1}, wantFault: capturedFaultArgument},
		{name: "empty path is unavailable, never an empty read", request: capturedReadRequest{}, wantFault: capturedFaultUnavailable},
		{name: "missing file is unavailable, never an empty read", request: capturedReadRequest{Path: filepath.Join(t.TempDir(), "absent")}, wantFault: capturedFaultUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, fault, err := readCapturedFile(test.request)
			if fault != test.wantFault {
				t.Fatalf("fault=%v err=%v, want fault %v", fault, err, test.wantFault)
			}
			if test.wantFault != capturedFaultNone {
				if err == nil {
					t.Fatal("a fault was reported with no error explaining it")
				}
				if result != nil {
					t.Fatalf("a faulted read returned a result: %#v", result)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(result.Bytes) != test.wantBytes {
				t.Fatalf("bytes=%q, want %q", result.Bytes, test.wantBytes)
			}
			if result.Offset != test.wantOffset || result.NextOffset != test.wantNext {
				t.Fatalf("offset=%d next=%d, want %d/%d", result.Offset, result.NextOffset, test.wantOffset, test.wantNext)
			}
			if result.TotalBytes != 10 {
				t.Fatalf("total=%d, want 10", result.TotalBytes)
			}
			if result.Truncated != test.wantTruncated {
				t.Fatalf("truncated=%v, want %v", result.Truncated, test.wantTruncated)
			}
			if result.Filtered {
				t.Fatal("an unfiltered read reported filtered=true")
			}
		})
	}
}

// TestReadCapturedFileGrepFiltersLinesWithoutMovingTheCursor is the honesty
// test for --grep. The filter must change WHICH BYTES come back and nothing
// else: offset/next_offset/total_bytes keep describing the underlying file, so
// `--from $next_offset` still pages correctly, and `filtered` says outright
// that the body is not the whole window.
//
// A filter that renumbered the cursor to the kept bytes would silently corrupt
// every subsequent --from/--follow read, which is exactly the false-pass this
// pins.
//
// verifies: AIRA-196
func TestReadCapturedFileGrepFiltersLinesWithoutMovingTheCursor(t *testing.T) {
	content := "alpha ok\nbeta FAIL\ngamma ok\ndelta FAIL\n"
	path := writeCapture(t, content)
	pattern, err := compileCapturedGrep("FAIL")
	if err != nil {
		t.Fatal(err)
	}
	result, fault, err := readCapturedFile(capturedReadRequest{Path: path, Grep: pattern})
	if fault != capturedFaultNone || err != nil {
		t.Fatalf("fault=%v err=%v", fault, err)
	}
	if got, want := string(result.Bytes), "beta FAIL\ndelta FAIL\n"; got != want {
		t.Fatalf("filtered bytes=%q, want %q", got, want)
	}
	if !result.Filtered {
		t.Fatal("a filtered read did not report filtered=true")
	}
	if result.Offset != 0 || result.NextOffset != int64(len(content)) || result.TotalBytes != int64(len(content)) {
		t.Fatalf("cursor moved under the filter: offset=%d next=%d total=%d, want 0/%d/%d",
			result.Offset, result.NextOffset, result.TotalBytes, len(content), len(content))
	}
	if result.Truncated {
		t.Fatal("a fully-read window was reported truncated because the filter shortened it")
	}
}

// TestReadCapturedFileGrepEdges covers the cases a naive splitter gets wrong:
// an anchored pattern (which never matches if the terminator is left on the
// line), a final line with no terminator, CRLF input, and a pattern that
// matches nothing (an empty body, NOT a fault).
//
// verifies: AIRA-196
func TestReadCapturedFileGrepEdges(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		pattern string
		want    string
	}{
		{name: "anchored pattern matches despite the newline", content: "keep end\ndrop endX\n", pattern: "end$", want: "keep end\n"},
		{name: "final line without a terminator is still matched", content: "a\nkeep me", pattern: "keep", want: "keep me"},
		{name: "CRLF line endings do not defeat an anchor", content: "keep end\r\ndrop\r\n", pattern: "end$", want: "keep end\r\n"},
		{name: "no match is an empty body, not a fault", content: "a\nb\n", pattern: "zzz", want: ""},
		{name: "a match on every line returns the window unchanged", content: "a\nb\n", pattern: "", want: "a\nb\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pattern, err := compileCapturedGrep(test.pattern)
			if err != nil {
				t.Fatal(err)
			}
			result, fault, err := readCapturedFile(capturedReadRequest{Path: writeCapture(t, test.content), Grep: pattern})
			if fault != capturedFaultNone || err != nil {
				t.Fatalf("fault=%v err=%v", fault, err)
			}
			if got := string(result.Bytes); got != test.want {
				t.Fatalf("bytes=%q, want %q", got, test.want)
			}
			if wantFiltered := test.pattern != ""; result.Filtered != wantFiltered {
				t.Fatalf("filtered=%v, want %v", result.Filtered, wantFiltered)
			}
		})
	}
}

// TestCompileCapturedGrepRefusesAnInvalidPattern keeps a bad pattern an
// argument error the operator sees, never a filter that silently matches
// nothing — which would read exactly like "the job produced no matching
// output".
//
// verifies: AIRA-196
func TestCompileCapturedGrepRefusesAnInvalidPattern(t *testing.T) {
	if _, err := compileCapturedGrep("("); err == nil {
		t.Fatal("an unparseable pattern was accepted")
	}
	pattern, err := compileCapturedGrep("")
	if err != nil || pattern != nil {
		t.Fatalf("an empty pattern must compile to no filter: pattern=%v err=%v", pattern, err)
	}
}

// TestReadCapturedFileGrepAppliesAfterTheCap pins the ORDER, which is not
// cosmetic: the cap bounds what is READ and the filter then selects from it, so
// a capped read reports truncated=true and the operator knows matches beyond
// the cap were never examined. Filtering first would let an unbounded scan
// masquerade as a bounded one.
//
// verifies: AIRA-196
func TestReadCapturedFileGrepAppliesAfterTheCap(t *testing.T) {
	path := writeCapture(t, "hit one\nmiss\nhit two\n")
	pattern, err := compileCapturedGrep("hit")
	if err != nil {
		t.Fatal(err)
	}
	result, fault, err := readCapturedFile(capturedReadRequest{Path: path, MaxBytes: 8, Grep: pattern})
	if fault != capturedFaultNone || err != nil {
		t.Fatalf("fault=%v err=%v", fault, err)
	}
	if got, want := string(result.Bytes), "hit one\n"; got != want {
		t.Fatalf("bytes=%q, want %q: the cap must bound the read before the filter selects", got, want)
	}
	if !result.Truncated {
		t.Fatal("a capped read did not report truncated, so a caller could read the filtered body as complete")
	}
	if result.NextOffset != 8 {
		t.Fatalf("next_offset=%d, want 8", result.NextOffset)
	}
}
