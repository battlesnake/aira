package runner

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// verifies: AIRA-145 -- read() used to collapse FIVE distinct record defects
// into the single sentence "E_JOURNAL_CORRUPT: invalid run ledger record",
// naming neither which check failed nor where in the file the bad record sits.
// These tests pin the enriched diagnostic: the reason, the record index, the
// byte offset, and the ledger path.
//
// SAFETY: every ledger here is a synthetic file built inside t.TempDir(). No
// test in this file reads or writes the machine's real common-directory run
// ledger, which is shared live by other sessions.

// synthLedgerEvent renders a minimal, well-formed ledger event payload. Fields
// are spelled out rather than marshaled from ledgerEvent so a test can emit a
// record the current binary would never produce -- which is the whole point.
func synthLedgerEvent(schema int, sequence uint64, kind string, runID string) []byte {
	return []byte(fmt.Sprintf(`{"schema_version":%d,"sequence":%d,"kind":%q,"run":{"id":%q}}`, schema, sequence, kind, runID))
}

// writeSynthLedger writes framed payloads to a fresh ledger under t.TempDir()
// and returns the ledger plus the byte offset at which each record starts.
func writeSynthLedger(t *testing.T, payloads ...[]byte) (*ledger, []int64) {
	t.Helper()
	l, err := newLedger(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	offsets := make([]int64, 0, len(payloads))
	for _, payload := range payloads {
		offsets = append(offsets, int64(len(data)))
		data = append(data, frame(payload)...)
	}
	if err := os.WriteFile(l.ledger, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return l, offsets
}

// verifies: AIRA-145
func TestLedgerCorruptRecordNamesReasonAndOffset(t *testing.T) {
	valid := synthLedgerEvent(ledgerSchema, 1, "starting", "RUN-1")
	tests := []struct {
		name string
		// payloads for the whole synthetic ledger; the LAST one is the bad record.
		payloads [][]byte
		// wantReason must appear in the error and must be specific to this defect.
		wantReason string
		// mustNotContain guards the split: a reason belonging to a DIFFERENT
		// defect must not be reported for this one.
		mustNotContain []string
	}{
		{
			name:           "undecodable json",
			payloads:       [][]byte{valid, []byte(`{"schema_version":1,"sequence":`)},
			wantReason:     "payload did not decode as a ledger event",
			mustNotContain: []string{"schema_version is", "kind is empty", "does not advance"},
		},
		{
			name: "unknown field from a newer writer",
			// DisallowUnknownFields makes a field a NEWER aira binary added a
			// hard decode failure for an older reader on the same shared ledger.
			// The enriched message must name the field.
			payloads:       [][]byte{valid, []byte(`{"schema_version":1,"sequence":2,"kind":"starting","run":{"id":"RUN-2"},"field_from_the_future":true}`)},
			wantReason:     `unknown field "field_from_the_future"`,
			mustNotContain: []string{"schema_version is", "kind is empty", "does not advance"},
		},
		{
			name:           "wrong schema version",
			payloads:       [][]byte{valid, synthLedgerEvent(ledgerSchema+1, 2, "starting", "RUN-2")},
			wantReason:     fmt.Sprintf("schema_version is %d, want %d", ledgerSchema+1, ledgerSchema),
			mustNotContain: []string{"did not decode", "kind is empty", "does not advance", "sequence is 0"},
		},
		{
			name:           "zero sequence",
			payloads:       [][]byte{valid, synthLedgerEvent(ledgerSchema, 0, "starting", "RUN-2")},
			wantReason:     "sequence is 0, want a positive sequence",
			mustNotContain: []string{"did not decode", "kind is empty", "schema_version is"},
		},
		{
			name:           "sequence does not advance",
			payloads:       [][]byte{valid, synthLedgerEvent(ledgerSchema, 1, "starting", "RUN-2")},
			wantReason:     "sequence 1 does not advance past the preceding record's sequence 1",
			mustNotContain: []string{"did not decode", "kind is empty", "schema_version is", "sequence is 0"},
		},
		{
			name:           "empty kind",
			payloads:       [][]byte{valid, synthLedgerEvent(ledgerSchema, 2, "", "RUN-2")},
			wantReason:     "kind is empty",
			mustNotContain: []string{"did not decode", "schema_version is", "does not advance", "sequence is 0"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			l, offsets := writeSynthLedger(t, test.payloads...)
			events, err := l.read()
			if err == nil {
				t.Fatalf("read accepted a corrupt ledger: events=%+v", events)
			}
			if events != nil {
				t.Fatalf("read returned events alongside an error: %+v", events)
			}
			got := err.Error()
			// The stable code and the historical sentence must both survive, so
			// existing callers and greps keep working.
			if !strings.HasPrefix(got, "E_JOURNAL_CORRUPT: invalid run ledger record") {
				t.Fatalf("error lost its stable prefix: %q", got)
			}
			// store.ErrorCode takes the token before the FIRST colon, so the
			// added detail must never displace the code (exit 4 depends on it).
			if code, _, _ := strings.Cut(got, ":"); code != "E_JOURNAL_CORRUPT" {
				t.Fatalf("error code token changed to %q: %q", code, got)
			}
			if !strings.Contains(got, test.wantReason) {
				t.Fatalf("error does not name the reason %q: %q", test.wantReason, got)
			}
			for _, forbidden := range test.mustNotContain {
				if strings.Contains(got, forbidden) {
					t.Fatalf("error reports the wrong defect %q: %q", forbidden, got)
				}
			}
			// Locate the record: index, byte offset, and the file itself.
			bad := len(test.payloads) - 1
			if want := fmt.Sprintf("record %d at byte offset %d", bad, offsets[bad]); !strings.Contains(got, want) {
				t.Fatalf("error does not locate the record (%q): %q", want, got)
			}
			if !strings.Contains(got, l.ledger) {
				t.Fatalf("error does not name the ledger file %q: %q", l.ledger, got)
			}
		})
	}
}

// A decode failure leaves the event partially populated. The diagnostic must
// report those partial values as partial, never as authoritative.
//
// verifies: AIRA-145
func TestLedgerCorruptRecordMarksPartialDecodeHonestly(t *testing.T) {
	valid := synthLedgerEvent(ledgerSchema, 1, "starting", "RUN-1")
	// sequence and kind parse; the run object then fails on an unknown field.
	broken := []byte(`{"schema_version":1,"sequence":2,"kind":"terminal","run":{"id":"RUN-2","not_a_real_field":1}}`)
	l, _ := writeSynthLedger(t, valid, broken)
	_, err := l.read()
	if err == nil {
		t.Fatal("read accepted a record with an unknown nested field")
	}
	got := err.Error()
	if !strings.Contains(got, `partially decoded sequence=2 kind="terminal"`) {
		t.Fatalf("error does not label the salvaged identity as partial: %q", got)
	}
	// A non-decode defect is fully decoded and must NOT claim to be partial.
	l2, _ := writeSynthLedger(t, valid, synthLedgerEvent(ledgerSchema, 2, "", "RUN-2"))
	_, err2 := l2.read()
	if err2 == nil {
		t.Fatal("read accepted an empty-kind record")
	}
	if got2 := err2.Error(); strings.Contains(got2, "partially decoded") || !strings.Contains(got2, `decoded sequence=2 kind=""`) {
		t.Fatalf("fully decoded record mislabelled: %q", got2)
	}
}

// The offset must be the record's TRUE position, which only holds if the reader
// counts the bytes it consumed. Records of differing lengths shift the varint
// prefix width and the payload size, so a hardcoded or recomputed offset drifts.
//
// verifies: AIRA-145
func TestLedgerCorruptRecordOffsetSurvivesVariableRecordLengths(t *testing.T) {
	long := synthLedgerEvent(ledgerSchema, 1, "starting", "RUN-"+strings.Repeat("x", 400))
	short := synthLedgerEvent(ledgerSchema, 2, "running", "RUN-2")
	bad := synthLedgerEvent(ledgerSchema+7, 3, "terminal", "RUN-3")
	l, offsets := writeSynthLedger(t, long, short, bad)
	_, err := l.read()
	if err == nil {
		t.Fatal("read accepted a wrong-schema record")
	}
	got := err.Error()
	if want := fmt.Sprintf("record 2 at byte offset %d", offsets[2]); !strings.Contains(got, want) {
		t.Fatalf("offset wrong after a >127-byte record (want %q): %q", want, got)
	}
	if !strings.Contains(got, fmt.Sprintf("%d-byte payload", len(bad))) {
		t.Fatalf("error does not report the declared payload length: %q", got)
	}
}

// Enriching the message must not change WHICH ledgers read() accepts.
//
// verifies: AIRA-145
func TestLedgerDiagnosticsDoNotChangeAcceptance(t *testing.T) {
	l, _ := writeSynthLedger(t,
		synthLedgerEvent(ledgerSchema, 1, "starting", "RUN-1"),
		synthLedgerEvent(ledgerSchema, 4, "running", "RUN-1"),
		synthLedgerEvent(ledgerSchema, 9, "terminal", "RUN-1"),
	)
	events, err := l.read()
	if err != nil {
		t.Fatalf("read rejected a valid ledger: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events=%d want 3: %+v", len(events), events)
	}
	if events[0].Sequence != 1 || events[1].Sequence != 4 || events[2].Sequence != 9 {
		t.Fatalf("sequences not preserved: %+v", events)
	}
	if events[2].Kind != "terminal" || events[2].Run.ID != "RUN-1" {
		t.Fatalf("record content not preserved: %+v", events[2])
	}
}

// The neighbouring framing failures in the same loop are located too. Their
// codes are unchanged: only the message gains the site.
//
// verifies: AIRA-145
func TestLedgerFramingFailuresAreLocated(t *testing.T) {
	valid := synthLedgerEvent(ledgerSchema, 1, "starting", "RUN-1")
	second := synthLedgerEvent(ledgerSchema, 2, "terminal", "RUN-1")

	// Checksum mismatch: flip one payload byte, leaving framing intact.
	l, offsets := writeSynthLedger(t, valid, second)
	data, err := os.ReadFile(l.ledger)
	if err != nil {
		t.Fatal(err)
	}
	data[offsets[1]+2] ^= 0xff
	if err := os.WriteFile(l.ledger, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = l.read()
	if err == nil {
		t.Fatal("read accepted a checksum-mismatched ledger")
	}
	got := err.Error()
	if !strings.HasPrefix(got, "E_JOURNAL_CORRUPT: run ledger checksum mismatch") {
		t.Fatalf("checksum error changed code or wording: %q", got)
	}
	if want := fmt.Sprintf("record 1 at byte offset %d", offsets[1]); !strings.Contains(got, want) {
		t.Fatalf("checksum error is not located (%q): %q", want, got)
	}

	// Torn tail: truncate the last record mid-payload.
	l2, offsets2 := writeSynthLedger(t, valid, second)
	data2, err := os.ReadFile(l2.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l2.ledger, data2[:len(data2)-40], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = l2.read()
	if err == nil {
		t.Fatal("read accepted a torn ledger")
	}
	got2 := err.Error()
	if !strings.HasPrefix(got2, "U_RUN_RECONCILE_REQUIRED: torn ledger") {
		t.Fatalf("torn error changed code: %q", got2)
	}
	if want := fmt.Sprintf("record 1 at byte offset %d", offsets2[1]); !strings.Contains(got2, want) {
		t.Fatalf("torn error is not located (%q): %q", want, got2)
	}
}
