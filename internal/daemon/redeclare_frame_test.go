package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
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
	if got := redeclareChargeOf(rec); got != wantCharge {
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

// The TOTAL-parser contract (TestReDeclareParserIsTotal), the encode/decode
// round trip (TestReDeclareFrameRoundTrip), and the encode-side reject table
// (TestEncodeReDeclareRejectsUnchargeableRecords) moved WITH the codec to
// internal/redeclare (S13). What stays here is what the daemon uniquely owns: the
// magic↔frame-length disjointness (needs daemon.MaxFrameBytes), the golden-bytes
// LEDGER CHARGE (needs daemon's redeclareCharge), and the two SERVER handler paths.

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
