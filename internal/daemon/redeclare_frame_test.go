package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"net"
	"reflect"
	"strings"
	"testing"
)

// TestReDeclareMagicIsDisjointFromFrameLength pins the FROZEN Invariant 8 / gate
// P1-5: the ARDR magic, read as a big-endian u32, is strictly greater than
// MaxFrameBytes. That single inequality is what lets the magic sniff and the
// normal length-prefixed framer coexist unambiguously — a normal frame's size
// header can never equal the magic, and the magic can never be read as a legal
// frame length. The magic's numeric value is decoded from the wire bytes here
// (ardrMagic is the single source of truth), so there is no second constant to drift.
//
// MUTATION: raising MaxFrameBytes above the magic value reds THIS test (not the
// happy-path sniff test, which still routes correctly — it is the DISJOINTNESS
// guarantee that breaks, and this is its pin).
func TestReDeclareMagicIsDisjointFromFrameLength(t *testing.T) {
	magicValue := binary.BigEndian.Uint32(ardrMagic[:])
	if magicValue != 0x41524452 {
		t.Fatalf("ardrMagic bytes decode to 0x%08x, want 0x41524452 (\"ARDR\") — the magic has drifted", magicValue)
	}
	if !(uint32(MaxFrameBytes) < magicValue) {
		t.Fatalf("MaxFrameBytes (0x%08x) must stay STRICTLY BELOW the ARDR magic (0x%08x): "+
			"a normal frame length could otherwise be mistaken for the magic and vice-versa (Invariant 8)", MaxFrameBytes, magicValue)
	}
	if !(maxReDeclareFrameBytes < MaxFrameBytes) {
		t.Fatalf("maxReDeclareFrameBytes (%d) must stay below MaxFrameBytes (%d)", maxReDeclareFrameBytes, MaxFrameBytes)
	}
	if ardrMagic != [4]byte{'A', 'R', 'D', 'R'} {
		t.Fatalf("ardrMagic = %q, want \"ARDR\" — the magic is frozen on the wire", ardrMagic)
	}
}

func TestReDeclareFrameRoundTrip(t *testing.T) {
	cases := map[string]reDeclareRecord{
		"one core with parent":    {ScopeID: "aira.slice/aira-CONFINE-abc.scope", RAMBytes: 2 << 30, CPUCores: 1, ParentScopeID: "aira.slice/suite.scope"},
		"zero cores (delegate)":   {ScopeID: "aira.slice/suite.scope", RAMBytes: 8 << 30, CPUCores: 0, ParentScopeID: ""},
		"zero ram legal":          {ScopeID: "s", RAMBytes: 0, CPUCores: 4, ParentScopeID: ""},
		"max representable ram":   {ScopeID: "big", RAMBytes: math.MaxInt64, CPUCores: 2, ParentScopeID: "p"},
		"multibyte utf8 scope id": {ScopeID: "aira.slice/café.scope", RAMBytes: 1 << 20, CPUCores: 1, ParentScopeID: "naïve"},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			frame, err := encodeReDeclareFrame(rec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := decodeReDeclareFrame(bytes.NewReader(frame))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, rec) {
				t.Fatalf("round trip = %+v, want %+v", got, rec)
			}
		})
	}
}

// goldenReDeclareFrame is a HAND-WRITTEN old-format frame — NOT produced by
// encodeReDeclareFrame — so a change to the wire layout reds the golden test
// even if the encoder and decoder change together. This is the freeze: an OLD
// client's bytes must keep meaning the same ledger charge to a NEW daemon.
//
//	magic "ARDR"           | 41 52 44 52
//	frame_len = 31         | 00 00 00 1F
//	scope_id_len = 5       | 00 00 00 05
//	scope_id "child"       | 63 68 69 6C 64
//	ram_bytes = 5 GiB      | 00 00 00 01 40 00 00 00
//	cpu_cores = 2          | 00 00 00 02
//	parent_len = 6         | 00 00 00 06
//	parent_scope_id "parent"| 70 61 72 65 6E 74
var goldenReDeclareFrame = []byte{
	0x41, 0x52, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x1F,
	0x00, 0x00, 0x00, 0x05,
	0x63, 0x68, 0x69, 0x6C, 0x64,
	0x00, 0x00, 0x00, 0x01, 0x40, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x02,
	0x00, 0x00, 0x00, 0x06,
	0x70, 0x61, 0x72, 0x65, 0x6E, 0x74,
}

// TestReDeclareGoldenBytesCharge is the design §4 / Invariant 8 semantics-freeze
// test: feed the OLD-format golden bytes to the NEW parser and assert the
// resulting LEDGER CHARGE (ram + cpu + scope + parent), NOT field-by-field
// deserialization. What is frozen is the MEANING — "this frame charges 5 GiB and
// 2 cores to scope child under parent" — so S9's SET is pinned to a stable
// charge across versions. It also checks the encoder reproduces the golden bytes,
// pinning both directions of the layout.
func TestReDeclareGoldenBytesCharge(t *testing.T) {
	rec, err := decodeReDeclareFrame(bytes.NewReader(goldenReDeclareFrame))
	if err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	wantCharge := redeclareCharge{ScopeID: "child", RAM: 5 << 30, CPU: 2, ParentScopeID: "parent"}
	if got := rec.charge(); got != wantCharge {
		t.Fatalf("golden frame charges %+v, want %+v", got, wantCharge)
	}
	// The encoder must reproduce the exact frozen bytes for the same record, so
	// the dump writer (S10) and the re-declare client (S13/S16) emit the shape
	// this decoder freezes.
	roundTrip, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "child", RAMBytes: 5 << 30, CPUCores: 2, ParentScopeID: "parent"})
	if err != nil {
		t.Fatalf("encode golden record: %v", err)
	}
	if !bytes.Equal(roundTrip, goldenReDeclareFrame) {
		t.Fatalf("encoder produced %x, want the frozen golden %x", roundTrip, goldenReDeclareFrame)
	}
}

// TestReDeclareParserIsTotal exercises the TOTAL-parser contract: every byte
// sequence either decodes or is a hard reject (a CodeProtocol-prefixed error) —
// never a panic, never a silent/partial record.
func TestReDeclareParserIsTotal(t *testing.T) {
	valid, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "child", RAMBytes: 5 << 30, CPUCores: 2, ParentScopeID: "parent"})
	if err != nil {
		t.Fatalf("encode valid: %v", err)
	}

	badMagic := append([]byte{}, valid...)
	badMagic[0] = 'X'

	zeroLen := append(append([]byte{}, ardrMagic[:]...), 0x00, 0x00, 0x00, 0x00)

	oversizeLen := append(append([]byte{}, ardrMagic[:]...), 0x00, 0x01, 0x00, 0x01) // 65537 > 65536

	// frame_len == maxReDeclareFrameBytes (65536) — the EXACT boundary. It PASSES the
	// length gate (the bound is `> max`, so the max itself is allowed), then the body
	// parser rejects: the declared 65536-byte body is not present (truncation). Pins the
	// `> max` (not `>= max`) boundary, and that a large declared length with no body is
	// still a bounded reject.
	boundaryLen := append(append([]byte{}, ardrMagic[:]...), 0x00, 0x01, 0x00, 0x00) // 65536 == 65536

	// frame_len = 8 but scope_id_len = 100 overruns the 8-byte body.
	scopeOverrun := append([]byte{}, ardrMagic[:]...)
	scopeOverrun = append(scopeOverrun, 0x00, 0x00, 0x00, 0x08)
	scopeOverrun = append(scopeOverrun, 0x00, 0x00, 0x00, 0x64, 0x00, 0x00, 0x00, 0x00)

	// A structurally complete frame plus one extra byte inside the declared body.
	trailing := append([]byte{}, valid...)
	trailing[7]++                     // frame_len += 1
	trailing = append(trailing, 0x00) // the extra body byte

	// scope_id_len = 0 → empty scope id (frame_len covers only the empty-scope prefix + ram + cpu + parent_len).
	emptyScope := append([]byte{}, ardrMagic[:]...)
	emptyScope = append(emptyScope, 0x00, 0x00, 0x00, 0x14) // body = 20 bytes
	emptyScope = append(emptyScope, 0x00, 0x00, 0x00, 0x00) // scope_id_len = 0
	emptyScope = append(emptyScope, 0, 0, 0, 0, 0, 0, 0, 0) // ram
	emptyScope = append(emptyScope, 0, 0, 0, 0)             // cpu
	emptyScope = append(emptyScope, 0, 0, 0, 0)             // parent_len = 0

	// invalid utf8 scope id (single 0xff byte).
	badUTF8 := append([]byte{}, ardrMagic[:]...)
	badUTF8 = append(badUTF8, 0x00, 0x00, 0x00, 0x15) // body = 21 bytes
	badUTF8 = append(badUTF8, 0x00, 0x00, 0x00, 0x01) // scope_id_len = 1
	badUTF8 = append(badUTF8, 0xff)                   // invalid utf8
	badUTF8 = append(badUTF8, 0, 0, 0, 0, 0, 0, 0, 0) // ram
	badUTF8 = append(badUTF8, 0, 0, 0, 0)             // cpu
	badUTF8 = append(badUTF8, 0, 0, 0, 0)             // parent_len = 0

	// ram_bytes with the high bit set → above math.MaxInt64.
	ramOverflow := append([]byte{}, ardrMagic[:]...)
	ramOverflow = append(ramOverflow, 0x00, 0x00, 0x00, 0x15) // body = 21 bytes
	ramOverflow = append(ramOverflow, 0x00, 0x00, 0x00, 0x01) // scope_id_len = 1
	ramOverflow = append(ramOverflow, 's')
	ramOverflow = append(ramOverflow, 0x80, 0, 0, 0, 0, 0, 0, 0) // ram high bit set
	ramOverflow = append(ramOverflow, 0, 0, 0, 0)                // cpu
	ramOverflow = append(ramOverflow, 0, 0, 0, 0)                // parent_len = 0

	rejects := map[string][]byte{
		"empty":           {},
		"magic only":      append([]byte{}, ardrMagic[:]...),
		"wrong magic":     badMagic,
		"zero frame_len":  zeroLen,
		"oversize len":    oversizeLen,
		"boundary len":    boundaryLen,
		"scope overrun":   scopeOverrun,
		"trailing byte":   trailing,
		"empty scope id":  emptyScope,
		"invalid utf8":    badUTF8,
		"ram over maxint": ramOverflow,
	}
	for name, raw := range rejects {
		t.Run(name, func(t *testing.T) {
			rec, err := decodeReDeclareFrame(bytes.NewReader(raw))
			if err == nil {
				t.Fatalf("decoded %+v, want a hard reject", rec)
			}
			if !strings.HasPrefix(err.Error(), CodeProtocol+":") {
				t.Fatalf("reject error %q lacks the %s prefix", err.Error(), CodeProtocol)
			}
		})
	}

	// Every proper prefix of a valid frame is a truncation and must hard-reject:
	// a partial frame is never a partial record.
	for cut := 0; cut < len(valid); cut++ {
		if _, err := decodeReDeclareFrame(bytes.NewReader(valid[:cut])); err == nil {
			t.Fatalf("prefix of length %d/%d decoded instead of rejecting the truncation", cut, len(valid))
		}
	}

	// Deterministic fuzz: no byte sequence may panic the parser. Half carry the
	// magic prefix AND an in-range frame_len, so the post-magic body field parser is
	// reached on every magic iteration. (A random u32 in bytes 4..7 would almost
	// always exceed maxReDeclareFrameBytes and reject before the body parser ran, so
	// the fuzz would exercise little beyond the length check — the body parser's
	// deterministic coverage is the reject table + the every-prefix loop above; this
	// fuzz is the no-panic property over arbitrary bodies.)
	rng := rand.New(rand.NewSource(0x5137))
	for i := 0; i < 4000; i++ {
		n := rng.Intn(80)
		if i%2 == 0 && n < 8 {
			n = 8 // room for magic + frame_len
		}
		raw := make([]byte, n)
		rng.Read(raw)
		if i%2 == 0 {
			copy(raw, ardrMagic[:])
			// A small, in-range frame_len so the parser proceeds PAST the length gate
			// into the body field parser this iteration.
			binary.BigEndian.PutUint32(raw[4:8], uint32(rng.Intn(72)))
		}
		// A panic here fails the test; the return value is intentionally ignored —
		// the property under test is "returns, never panics; parse xor reject".
		_, _ = decodeReDeclareFrame(bytes.NewReader(raw))
	}
}

func TestEncodeReDeclareRejectsUnchargeableRecords(t *testing.T) {
	cases := map[string]reDeclareRecord{
		"empty scope":        {ScopeID: "", RAMBytes: 1, CPUCores: 1},
		"ram over int64":     {ScopeID: "s", RAMBytes: math.MaxInt64 + 1, CPUCores: 1},
		"invalid utf8 scope": {ScopeID: string([]byte{0xff}), RAMBytes: 1, CPUCores: 1},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := encodeReDeclareFrame(rec); err == nil {
				t.Fatalf("encoded an unchargeable record %+v", rec)
			}
		})
	}
}

// TestOldClientReDeclareIsSniffedBeforeProtocolCheck is the load-bearing sniff
// test. It drives serveConnection with a raw ARDR frame (NO proto field — a
// re-declare is out-of-band by construction) and asserts the daemon replies the
// frozen 1-byte ack rather than a protocol refusal. This is the OLD-client ↔
// NEW-daemon upgrade path (design §4).
//
// S9: the handler is no longer the stub (ack-and-close). It now gates on SO_PEERCRED
// same-uid and HOLDS the connection, so the test injects a same-uid credential seam and
// a slice resolver (NEVER the real host resolver — the box itself runs under aira.slice),
// reads the ack through the ACCEPT path, then closes the client so the hold exits.
//
// MUTATION: moving the magic sniff AFTER the protocol-version check reds this
// test — readInboundFrame then reads the magic as an oversized frame length,
// refuses with a length-prefixed E_DAEMON_PROTOCOL response, and the first byte
// the client reads is that response's 0x00 length prefix, not the 0x06 ack.
func TestOldClientReDeclareIsSniffedBeforeProtocolCheck(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	// reDeclareTestServer wires ALL the re-declare seams (same-uid credential, resolvable
	// slice, fail-closed memory reader, no real confine scan, a long poll) so the
	// establish's evaluator never touches the real host ListConfines / memory reader on
	// its tick — this is a unit test, not the host slice.
	server := reDeclareTestServer()
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), serverConn)
		close(done)
	}()
	frame, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "aira.slice/aira-CONFINE-x.scope", RAMBytes: 1 << 30, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = clientConn.Write(frame) }()

	var ack [1]byte
	if _, err := io.ReadFull(clientConn, ack[:]); err != nil {
		t.Fatalf("re-declare produced no ack: %v (mutation tell: the sniff moved below the proto check, or the magic/frame-length overlap broke)", err)
	}
	if ack[0] != reDeclareAckByte {
		t.Fatalf("re-declare ack = 0x%02x, want the frozen 0x%02x", ack[0], reDeclareAckByte)
	}
	// S9 HOLDS the lease-bearing connection: close the client so the hold's EOF path
	// releases the lease and the handler returns.
	_ = clientConn.Close()
	<-done
}

// TestReDeclareHandlerRejectsMalformedFrameWithoutAck pins the TOTAL-parser
// contract at the SERVER boundary: a frame with a valid magic but a malformed
// body is a hard reject — the daemon logs it and writes NOTHING, so the peer
// reads EOF, never a fabricated ack. The malformed-frame path is identical under
// S9 (decode error → return before any lease or ack), so this stays green across
// the stub→handler replacement.
func TestReDeclareHandlerRejectsMalformedFrameWithoutAck(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	done := make(chan struct{})
	go func() {
		NewServer(Paths{StateID: "state"}).serveConnection(context.Background(), serverConn)
		close(done)
	}()
	// Valid magic, frame_len = 8, scope_id_len = 100 → overruns the 8-byte body.
	bad := append([]byte{}, ardrMagic[:]...)
	bad = append(bad, 0x00, 0x00, 0x00, 0x08)
	bad = append(bad, 0x00, 0x00, 0x00, 0x64, 0x00, 0x00, 0x00, 0x00)
	go func() { _, _ = clientConn.Write(bad) }()

	var ack [1]byte
	if n, err := io.ReadFull(clientConn, ack[:]); err == nil {
		t.Fatalf("stub wrote an ack (n=%d byte=0x%02x) for a malformed frame; a hard reject writes nothing", n, ack[0])
	}
	<-done
}
