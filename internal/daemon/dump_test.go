package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goldenLeaseDumpStamp is a fixed, arbitrary write-time for the golden file test,
// so the whole file (header included) is byte-frozen. 0x0102030405060708 ns.
const goldenLeaseDumpStamp = int64(0x0102030405060708)

// goldenLeaseDumpRecordTail is the pid(8)+start-tick(8) appended after the ARDR
// frame in a dump record: pid = 0x1234, start-tick = 0x5678, big-endian u64 each.
// Hand-written, like goldenReDeclareFrame, so a layout change reds the golden.
var goldenLeaseDumpRecordTail = []byte{
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x12, 0x34,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x56, 0x78,
}

// goldenLeaseDumpRecord is the frozen single-record bytes: the S7 golden ARDR
// frame (scope "child", 5 GiB, 2 cores, parent "parent") followed by the pid and
// start-tick tail. S11 reads exactly this record shape.
func goldenLeaseDumpRecord() []byte {
	out := append([]byte{}, goldenReDeclareFrame...)
	return append(out, goldenLeaseDumpRecordTail...)
}

// goldenLeaseDumpRecordValue is the decoded form of goldenLeaseDumpRecord().
func goldenLeaseDumpRecordValue() leaseDumpRecord {
	return leaseDumpRecord{
		Frame:            reDeclareRecord{ScopeID: "child", RAMBytes: 5 << 30, CPUCores: 2, ParentScopeID: "parent"},
		ClientPID:        0x1234,
		ProcessStartTick: 0x5678,
	}
}

// verifies: a dump record encodes and decodes back to the identical lease —
// scope, ram, cpu, parent AND the two restart-only fields (pid, start-tick). The
// frame's fields have their own round-trip (redeclare_frame_test); this pins that
// the WRAPPER carries pid and start-tick losslessly beside it.
func TestLeaseDumpRecordRoundTrip(t *testing.T) {
	cases := []leaseDumpRecord{
		{Frame: reDeclareRecord{ScopeID: "CONFINE-x-9-aa@sess", RAMBytes: 3 << 30, CPUCores: 4, ParentScopeID: "parent-1"}, ClientPID: 918273, ProcessStartTick: 44556677},
		// pid 0 (unreadable credential) and empty parent and 0 cores (delegate suite).
		{Frame: reDeclareRecord{ScopeID: "CONFINE-suite-1-bb@sess", RAMBytes: 0, CPUCores: 0, ParentScopeID: ""}, ClientPID: 0, ProcessStartTick: 0},
	}
	for _, want := range cases {
		encoded, err := encodeLeaseDumpRecord(want)
		if err != nil {
			t.Fatalf("encode %+v: %v", want, err)
		}
		got, err := decodeLeaseDumpRecord(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("decode %+v: %v", want, err)
		}
		if got != want {
			t.Fatalf("round-trip: got %+v, want %+v", got, want)
		}
	}
}

// verifies: the EXACT record bytes S11 depends on, both directions. This is the
// freeze — it reuses the frozen S7 frame bytes and hand-writes the pid/tick tail,
// so a change to either the frame layout or the tail layout reds here.
func TestLeaseDumpRecordGoldenBytes(t *testing.T) {
	golden := goldenLeaseDumpRecord()
	encoded, err := encodeLeaseDumpRecord(goldenLeaseDumpRecordValue())
	if err != nil {
		t.Fatalf("encode golden record: %v", err)
	}
	if !bytes.Equal(encoded, golden) {
		t.Fatalf("encoder produced %x, want frozen golden %x", encoded, golden)
	}
	got, err := decodeLeaseDumpRecord(bytes.NewReader(golden))
	if err != nil {
		t.Fatalf("decode golden record: %v", err)
	}
	if want := goldenLeaseDumpRecordValue(); got != want {
		t.Fatalf("golden record decodes to %+v, want %+v", got, want)
	}
}

// verifies: a whole file round-trips — the write-time stamp and every record.
func TestLeaseDumpFileRoundTrip(t *testing.T) {
	stamp := time.Unix(0, goldenLeaseDumpStamp)
	recs := []leaseDumpRecord{
		{Frame: reDeclareRecord{ScopeID: "a@s", RAMBytes: 1 << 30, CPUCores: 1, ParentScopeID: ""}, ClientPID: 10, ProcessStartTick: 100},
		{Frame: reDeclareRecord{ScopeID: "b@s", RAMBytes: 2 << 30, CPUCores: 2, ParentScopeID: "p@s"}, ClientPID: 0, ProcessStartTick: 0},
	}
	data, err := encodeLeaseDump(stamp, recs)
	if err != nil {
		t.Fatalf("encode file: %v", err)
	}
	dump, err := decodeLeaseDump(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode file: %v", err)
	}
	if !dump.Stamp.Equal(stamp) {
		t.Fatalf("stamp = %v, want %v", dump.Stamp, stamp)
	}
	if len(dump.Records) != len(recs) {
		t.Fatalf("decoded %d records, want %d", len(dump.Records), len(recs))
	}
	for i := range recs {
		if dump.Records[i] != recs[i] {
			t.Fatalf("record %d = %+v, want %+v", i, dump.Records[i], recs[i])
		}
	}
}

// verifies: the EXACT whole-file bytes, header included, both directions. A fixed
// stamp freezes the header; the single record is the frozen golden record. This is
// the contract S11 opens the file against.
func TestLeaseDumpFileGoldenBytes(t *testing.T) {
	header := []byte{
		'A', 'L', 'D', 'R', // magic
		0x00, 0x00, 0x00, 0x01, // version 1
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // stamp 0x0102030405060708
		0x00, 0x00, 0x00, 0x01, // count 1
	}
	want := append(header, goldenLeaseDumpRecord()...)

	data, err := encodeLeaseDump(time.Unix(0, goldenLeaseDumpStamp), []leaseDumpRecord{goldenLeaseDumpRecordValue()})
	if err != nil {
		t.Fatalf("encode golden file: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("encoder produced %x, want frozen golden %x", data, want)
	}
	dump, err := decodeLeaseDump(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("decode golden file: %v", err)
	}
	if dump.Stamp.UnixNano() != goldenLeaseDumpStamp {
		t.Fatalf("golden stamp = %d, want %d", dump.Stamp.UnixNano(), goldenLeaseDumpStamp)
	}
	if len(dump.Records) != 1 || dump.Records[0] != goldenLeaseDumpRecordValue() {
		t.Fatalf("golden file records = %+v", dump.Records)
	}
}

// verifies: an EMPTY ledger dumps to a frozen 20-byte header with count 0, both
// directions — pinning the "always write, even empty" contract as the exact bytes
// S11 opens (a 0-record file is a real dump, not an absent one).
func TestLeaseDumpEmptyGoldenBytes(t *testing.T) {
	want := []byte{
		'A', 'L', 'D', 'R', // magic
		0x00, 0x00, 0x00, 0x01, // version 1
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // stamp
		0x00, 0x00, 0x00, 0x00, // count 0
	}
	data, err := encodeLeaseDump(time.Unix(0, goldenLeaseDumpStamp), nil)
	if err != nil {
		t.Fatalf("encode empty: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("empty encoder produced %x, want %x", data, want)
	}
	dump, err := decodeLeaseDump(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("decode empty golden: %v", err)
	}
	if len(dump.Records) != 0 || dump.Stamp.UnixNano() != goldenLeaseDumpStamp {
		t.Fatalf("empty golden decoded to %d records, stamp %d", len(dump.Records), dump.Stamp.UnixNano())
	}
}

// verifies: the file decoder is TOTAL — every structural defect is a hard error,
// never a panic or a silent partial, and a malformed record returns the decoded
// prefix plus an error (self-delimiting but not resyncable; §15 P2-B).
func TestLeaseDumpDecodeIsTotal(t *testing.T) {
	good, err := encodeLeaseDump(time.Unix(0, goldenLeaseDumpStamp), []leaseDumpRecord{
		goldenLeaseDumpRecordValue(),
		{Frame: reDeclareRecord{ScopeID: "b@s", RAMBytes: 1 << 20, CPUCores: 1}, ClientPID: 7, ProcessStartTick: 9},
	})
	if err != nil {
		t.Fatalf("encode good: %v", err)
	}

	t.Run("empty", func(t *testing.T) {
		if _, err := decodeLeaseDump(bytes.NewReader(nil)); err == nil {
			t.Fatal("empty input decoded without error")
		}
	})
	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte{}, good...)
		bad[0] = 'X'
		if _, err := decodeLeaseDump(bytes.NewReader(bad)); err == nil {
			t.Fatal("bad magic decoded without error")
		}
	})
	t.Run("bad version", func(t *testing.T) {
		bad := append([]byte{}, good...)
		bad[7] = 0x02 // version 2
		if _, err := decodeLeaseDump(bytes.NewReader(bad)); err == nil {
			t.Fatal("unsupported version decoded without error")
		}
	})
	t.Run("truncated header", func(t *testing.T) {
		if _, err := decodeLeaseDump(bytes.NewReader(good[:10])); err == nil {
			t.Fatal("truncated header decoded without error")
		}
	})
	t.Run("every strict prefix errors, never panics", func(t *testing.T) {
		// The format has no trailing bytes, so every prefix SHORTER than the whole
		// file is missing part of the structure and MUST error (a header short, or a
		// record truncated, or the count overrunning the bytes present) — never a
		// silent clean decode, never a panic. This is the TOTAL-decoder claim.
		for n := 0; n < len(good); n++ {
			dump, err := decodeLeaseDump(bytes.NewReader(good[:n]))
			if err == nil {
				t.Fatalf("strict prefix %d of %d decoded WITHOUT error (%d records) — not total", n, len(good), len(dump.Records))
			}
		}
	})
	t.Run("trailing bytes after the counted records error, not a silent tail", func(t *testing.T) {
		// A well-formed file plus one extra byte is malformed — the decoder must reject it
		// rather than decode the records and silently drop the tail (the TOTAL contract).
		withTail := append(append([]byte{}, good...), 0x00)
		if _, err := decodeLeaseDump(bytes.NewReader(withTail)); err == nil {
			t.Fatal("trailing byte after the records decoded without error — not total")
		}
	})
	t.Run("malformed record returns the decoded prefix plus an error", func(t *testing.T) {
		// Corrupt the SECOND record's embedded frame magic. The first record is
		// intact, so the decoder must return it, then error on the second.
		firstLen := len(goldenLeaseDumpRecord())
		corrupt := append([]byte{}, good...)
		corrupt[leaseDumpHeaderLen+firstLen] = 'Z' // first byte of record 1's ARDR magic
		dump, err := decodeLeaseDump(bytes.NewReader(corrupt))
		if err == nil {
			t.Fatal("corrupt trailing record decoded without error")
		}
		if len(dump.Records) != 1 || dump.Records[0] != goldenLeaseDumpRecordValue() {
			t.Fatalf("prefix before the corrupt record = %+v, want the one good golden record", dump.Records)
		}
	})
	t.Run("count larger than records present errors, does not over-read or hang", func(t *testing.T) {
		bad := append([]byte{}, good...)
		bad[19] = 0x09 // claim 9 records; only 2 present
		if _, err := decodeLeaseDump(bytes.NewReader(bad)); err == nil {
			t.Fatal("over-stated count decoded without error")
		}
	})
}

// verifies: the record encoder refuses a negative pid (a pid is non-negative; 0 is
// a legal, meaningful value and must still encode), and inherits the frame
// encoder's empty-scope_id refusal — so it can never emit a record the decoder
// would reject.
func TestEncodeLeaseDumpRecordStructuralRefusals(t *testing.T) {
	if _, err := encodeLeaseDumpRecord(leaseDumpRecord{Frame: reDeclareRecord{ScopeID: "x@s"}, ClientPID: -1}); err == nil {
		t.Fatal("negative pid encoded without error")
	}
	if _, err := encodeLeaseDumpRecord(leaseDumpRecord{Frame: reDeclareRecord{ScopeID: ""}, ClientPID: 5}); err == nil {
		t.Fatal("empty scope_id encoded without error")
	}
	// pid 0 is legal and must encode.
	if _, err := encodeLeaseDumpRecord(leaseDumpRecord{Frame: reDeclareRecord{ScopeID: "x@s"}, ClientPID: 0}); err != nil {
		t.Fatalf("pid 0 refused: %v", err)
	}
}

// dumpTestWaiter builds a granted, accounted waiter with the full field set the
// dump reads.
func dumpTestWaiter(seq, reserve, cpu int64, scopeID, parent string, pid int, tick uint64) *admitWaiter {
	return &admitWaiter{
		seq: seq, reserve: reserve, cpu: cpu, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), scopeID: scopeID, parentScopeID: parent,
		clientPID: pid, processStartTick: tick,
	}
}

// verifies: the snapshot captures exactly the still-held, SCOPE-BACKED granted
// leases — with scope/ram/cpu/parent/pid/start-tick intact — and a pid-0 lease is
// captured HONESTLY (not filtered). A scope-less reservation, a non-accounted
// waiter and a queued waiter are all excluded. The assertion is on the decoded
// dump records (not on queue internals) so a renderer change cannot satisfy it.
func TestSnapshotLeaseDumpCapturesHeldScopeBackedLeasesHonestly(t *testing.T) {
	server := NewServer(Paths{})
	queueA := &sliceQueue{path: "/slice", server: server}
	queueA.waiters = []*admitWaiter{
		dumpTestWaiter(1, 4<<30, 2, "CONFINE-a-11-x@s", "parent-a@s", 4242, 99),
		// pid 0 — unreadable credential at anchor time — must still be dumped.
		dumpTestWaiter(2, 1<<30, 0, "CONFINE-b-22-y@s", "", 0, 0),
		// scope-less reservation: no ARDR key, must be skipped.
		dumpTestWaiter(3, 512<<20, 1, "", "parent-a@s", 7777, 5),
		// queued (not granted): excluded.
		{seq: 4, reserve: 8 << 30, state: admitQueued, grantedCh: make(chan struct{}), scopeID: "CONFINE-q-44-z@s"},
		// granted but not accounted: excluded (matches rederiveLedgerLocked).
		{seq: 5, reserve: 2 << 30, state: admitGranted, accounted: false, grantedCh: make(chan struct{}), scopeID: "CONFINE-u-55-w@s"},
	}
	queueB := &sliceQueue{path: "/slice-b", server: server}
	queueB.waiters = []*admitWaiter{
		dumpTestWaiter(1, 3<<30, 3, "CONFINE-c-33-q@s", "", 515151, 123456),
	}
	registerAdmitQueue(server, queueA)
	registerAdmitQueue(server, queueB)

	recs := server.snapshotLeaseDump()

	byScope := map[string]leaseDumpRecord{}
	for _, rec := range recs {
		byScope[rec.Frame.ScopeID] = rec
	}
	want := map[string]leaseDumpRecord{
		"CONFINE-a-11-x@s": {Frame: reDeclareRecord{ScopeID: "CONFINE-a-11-x@s", RAMBytes: 4 << 30, CPUCores: 2, ParentScopeID: "parent-a@s"}, ClientPID: 4242, ProcessStartTick: 99},
		"CONFINE-b-22-y@s": {Frame: reDeclareRecord{ScopeID: "CONFINE-b-22-y@s", RAMBytes: 1 << 30, CPUCores: 0, ParentScopeID: ""}, ClientPID: 0, ProcessStartTick: 0},
		"CONFINE-c-33-q@s": {Frame: reDeclareRecord{ScopeID: "CONFINE-c-33-q@s", RAMBytes: 3 << 30, CPUCores: 3, ParentScopeID: ""}, ClientPID: 515151, ProcessStartTick: 123456},
	}
	if len(recs) != len(want) {
		t.Fatalf("snapshot captured %d records, want %d: %+v", len(recs), len(want), recs)
	}
	for scope, wantRec := range want {
		got, ok := byScope[scope]
		if !ok {
			t.Fatalf("scope %q missing from snapshot %+v", scope, recs)
		}
		if got != wantRec {
			t.Fatalf("scope %q = %+v, want %+v", scope, got, wantRec)
		}
	}
	if _, leaked := byScope[""]; leaked {
		t.Fatal("a scope-less reservation leaked into the dump")
	}

	// And the captured set survives a write/read round-trip with the fields intact.
	data, err := encodeLeaseDump(time.Now(), recs)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	dump, err := decodeLeaseDump(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(dump.Records) != len(want) {
		t.Fatalf("round-tripped %d records, want %d", len(dump.Records), len(want))
	}
}

// verifies: writeLeaseDump writes a 0600 file under RuntimeDir that decodeLeaseDump
// reads back, with a fresh (non-zero, recent) stamp — the real fsync+rename path.
func TestWriteLeaseDumpAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(Paths{})
	server.Paths.RuntimeDir = dir
	recs := []leaseDumpRecord{
		{Frame: reDeclareRecord{ScopeID: "a@s", RAMBytes: 1 << 30, CPUCores: 1, ParentScopeID: "p@s"}, ClientPID: 321, ProcessStartTick: 654},
	}

	before := time.Now()
	if err := server.writeLeaseDump(recs); err != nil {
		t.Fatalf("write: %v", err)
	}

	path := filepath.Join(dir, leaseDumpFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("dump perms = %o, want 0600", perm)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open dump: %v", err)
	}
	defer f.Close()
	dump, err := decodeLeaseDump(f)
	if err != nil {
		t.Fatalf("decode written dump: %v", err)
	}
	if dump.Stamp.Before(before) || dump.Stamp.After(time.Now().Add(time.Second)) {
		t.Fatalf("stamp %v not within the write window", dump.Stamp)
	}
	if len(dump.Records) != 1 || dump.Records[0] != recs[0] {
		t.Fatalf("written records = %+v, want %+v", dump.Records, recs)
	}

	// No leftover temp files beside the committed dump.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != leaseDumpFileName {
			t.Fatalf("stray file left in RuntimeDir: %q", e.Name())
		}
	}
}

// verifies: writeLeaseDump always writes, even with zero held leases — the rename
// replaces any previous dump, so there is no stale-file special case.
func TestWriteLeaseDumpWritesEmptyLedger(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(Paths{})
	server.Paths.RuntimeDir = dir
	if err := server.writeLeaseDump(nil); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, leaseDumpFileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	dump, err := decodeLeaseDump(f)
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(dump.Records) != 0 {
		t.Fatalf("empty dump has %d records", len(dump.Records))
	}
}
