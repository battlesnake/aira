package redeclare

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := map[string]Record{
		"one core with parent":    {ScopeID: "aira.slice/aira-CONFINE-abc.scope", RAMBytes: 2 << 30, CPUCores: 1, ParentScopeID: "aira.slice/suite.scope"},
		"zero cores (delegate)":   {ScopeID: "aira.slice/suite.scope", RAMBytes: 8 << 30, CPUCores: 0, ParentScopeID: ""},
		"zero ram legal":          {ScopeID: "s", RAMBytes: 0, CPUCores: 4, ParentScopeID: ""},
		"max representable ram":   {ScopeID: "big", RAMBytes: math.MaxInt64, CPUCores: 2, ParentScopeID: "p"},
		"multibyte utf8 scope id": {ScopeID: "aira.slice/café.scope", RAMBytes: 1 << 20, CPUCores: 1, ParentScopeID: "naïve"},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			frame, err := EncodeFrame(rec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := DecodeFrame(bytes.NewReader(frame))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, rec) {
				t.Fatalf("round trip = %+v, want %+v", got, rec)
			}
		})
	}
}

// goldenFrame is a HAND-WRITTEN old-format frame — NOT produced by EncodeFrame —
// so a change to the wire layout reds the golden test even if the encoder and
// decoder change together. This is the freeze: an OLD client's bytes must keep
// decoding to the same record on a NEW daemon. (The daemon package additionally
// pins the resulting LEDGER CHARGE for these bytes; that assertion needs daemon's
// private redeclareCharge and stays there.)
//
//	magic "ARDR"           | 41 52 44 52
//	frame_len = 31         | 00 00 00 1F
//	scope_id_len = 5       | 00 00 00 05
//	scope_id "child"       | 63 68 69 6C 64
//	ram_bytes = 5 GiB      | 00 00 00 01 40 00 00 00
//	cpu_cores = 2          | 00 00 00 02
//	parent_len = 6         | 00 00 00 06
//	parent_scope_id "parent"| 70 61 72 65 6E 74
var goldenFrame = []byte{
	0x41, 0x52, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x1F,
	0x00, 0x00, 0x00, 0x05,
	0x63, 0x68, 0x69, 0x6C, 0x64,
	0x00, 0x00, 0x00, 0x01, 0x40, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x02,
	0x00, 0x00, 0x00, 0x06,
	0x70, 0x61, 0x72, 0x65, 0x6E, 0x74,
}

// TestGoldenBytesRoundTrip freezes the wire LAYOUT: the golden old-format bytes
// decode to the expected record, and the encoder reproduces the exact frozen
// bytes for that record — pinning both directions so the dump writer (S10) and
// the re-declare client (S13/S16) emit the shape this decoder freezes.
func TestGoldenBytesRoundTrip(t *testing.T) {
	want := Record{ScopeID: "child", RAMBytes: 5 << 30, CPUCores: 2, ParentScopeID: "parent"}
	got, err := DecodeFrame(bytes.NewReader(goldenFrame))
	if err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("golden decodes to %+v, want %+v", got, want)
	}
	roundTrip, err := EncodeFrame(want)
	if err != nil {
		t.Fatalf("encode golden record: %v", err)
	}
	if !bytes.Equal(roundTrip, goldenFrame) {
		t.Fatalf("encoder produced %x, want the frozen golden %x", roundTrip, goldenFrame)
	}
}

// TestParserIsTotal exercises the TOTAL-parser contract: every byte sequence
// either decodes or is a hard reject (a CodeProtocol-prefixed error) — never a
// panic, never a silent/partial record.
func TestParserIsTotal(t *testing.T) {
	valid, err := EncodeFrame(Record{ScopeID: "child", RAMBytes: 5 << 30, CPUCores: 2, ParentScopeID: "parent"})
	if err != nil {
		t.Fatalf("encode valid: %v", err)
	}

	badMagic := append([]byte{}, valid...)
	badMagic[0] = 'X'

	zeroLen := append(append([]byte{}, Magic[:]...), 0x00, 0x00, 0x00, 0x00)

	oversizeLen := append(append([]byte{}, Magic[:]...), 0x00, 0x01, 0x00, 0x01) // 65537 > 65536

	// frame_len == MaxBodyBytes (65536) — the EXACT boundary. It PASSES the length
	// gate (the bound is `> max`, so the max itself is allowed), then the body parser
	// rejects: the declared 65536-byte body is not present (truncation). Pins the
	// `> max` (not `>= max`) boundary, and that a large declared length with no body
	// is still a bounded reject.
	boundaryLen := append(append([]byte{}, Magic[:]...), 0x00, 0x01, 0x00, 0x00) // 65536 == 65536

	// frame_len = 8 but scope_id_len = 100 overruns the 8-byte body.
	scopeOverrun := append([]byte{}, Magic[:]...)
	scopeOverrun = append(scopeOverrun, 0x00, 0x00, 0x00, 0x08)
	scopeOverrun = append(scopeOverrun, 0x00, 0x00, 0x00, 0x64, 0x00, 0x00, 0x00, 0x00)

	// A structurally complete frame plus one extra byte inside the declared body.
	trailing := append([]byte{}, valid...)
	trailing[7]++                     // frame_len += 1
	trailing = append(trailing, 0x00) // the extra body byte

	// scope_id_len = 0 → empty scope id.
	emptyScope := append([]byte{}, Magic[:]...)
	emptyScope = append(emptyScope, 0x00, 0x00, 0x00, 0x14) // body = 20 bytes
	emptyScope = append(emptyScope, 0x00, 0x00, 0x00, 0x00) // scope_id_len = 0
	emptyScope = append(emptyScope, 0, 0, 0, 0, 0, 0, 0, 0) // ram
	emptyScope = append(emptyScope, 0, 0, 0, 0)             // cpu
	emptyScope = append(emptyScope, 0, 0, 0, 0)             // parent_len = 0

	// invalid utf8 scope id (single 0xff byte).
	badUTF8 := append([]byte{}, Magic[:]...)
	badUTF8 = append(badUTF8, 0x00, 0x00, 0x00, 0x15) // body = 21 bytes
	badUTF8 = append(badUTF8, 0x00, 0x00, 0x00, 0x01) // scope_id_len = 1
	badUTF8 = append(badUTF8, 0xff)                   // invalid utf8
	badUTF8 = append(badUTF8, 0, 0, 0, 0, 0, 0, 0, 0) // ram
	badUTF8 = append(badUTF8, 0, 0, 0, 0)             // cpu
	badUTF8 = append(badUTF8, 0, 0, 0, 0)             // parent_len = 0

	// ram_bytes with the high bit set → above math.MaxInt64.
	ramOverflow := append([]byte{}, Magic[:]...)
	ramOverflow = append(ramOverflow, 0x00, 0x00, 0x00, 0x15) // body = 21 bytes
	ramOverflow = append(ramOverflow, 0x00, 0x00, 0x00, 0x01) // scope_id_len = 1
	ramOverflow = append(ramOverflow, 's')
	ramOverflow = append(ramOverflow, 0x80, 0, 0, 0, 0, 0, 0, 0) // ram high bit set
	ramOverflow = append(ramOverflow, 0, 0, 0, 0)                // cpu
	ramOverflow = append(ramOverflow, 0, 0, 0, 0)                // parent_len = 0

	rejects := map[string][]byte{
		"empty":           {},
		"magic only":      append([]byte{}, Magic[:]...),
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
			rec, err := DecodeFrame(bytes.NewReader(raw))
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
		if _, err := DecodeFrame(bytes.NewReader(valid[:cut])); err == nil {
			t.Fatalf("prefix of length %d/%d decoded instead of rejecting the truncation", cut, len(valid))
		}
	}

	// Deterministic fuzz: no byte sequence may panic the parser. Half carry the
	// magic prefix AND an in-range frame_len, so the post-magic body field parser is
	// reached on every magic iteration.
	rng := rand.New(rand.NewSource(0x5137))
	for i := 0; i < 4000; i++ {
		n := rng.Intn(80)
		if i%2 == 0 && n < 8 {
			n = 8 // room for magic + frame_len
		}
		raw := make([]byte, n)
		rng.Read(raw)
		if i%2 == 0 {
			copy(raw, Magic[:])
			binary.BigEndian.PutUint32(raw[4:8], uint32(rng.Intn(72)))
		}
		// A panic here fails the test; the return value is intentionally ignored —
		// the property under test is "returns, never panics; parse xor reject".
		_, _ = DecodeFrame(bytes.NewReader(raw))
	}
}

func TestEncodeRejectsUnchargeableRecords(t *testing.T) {
	cases := map[string]Record{
		"empty scope":        {ScopeID: "", RAMBytes: 1, CPUCores: 1},
		"ram over int64":     {ScopeID: "s", RAMBytes: math.MaxInt64 + 1, CPUCores: 1},
		"invalid utf8 scope": {ScopeID: string([]byte{0xff}), RAMBytes: 1, CPUCores: 1},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := EncodeFrame(rec); err == nil {
				t.Fatalf("encoded an unchargeable record %+v", rec)
			}
		})
	}
}
