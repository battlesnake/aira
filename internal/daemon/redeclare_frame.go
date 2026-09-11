package daemon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

// ARDR re-declare frame — the version-FROZEN, out-of-band lease re-declaration
// exchanged across a daemon restart/upgrade (design §4). A restart is usually an
// UPGRADE: an OLD client speaks to a NEWER daemon, so this frame is the ONE
// cross-version path and its wire shape is frozen forever. A future field is
// added by a NEW frame kind, never by editing this layout.
//
// FROZEN CONTRACT (changing any of this needs a new frame kind, not an edit):
//   - Sniffed by its 4-byte magic "ARDR" BEFORE any framing/handshake and BEFORE
//     the protocol-version check (server.go serveConnection). That ordering is
//     load-bearing: a version skew must NOT refuse a re-declare (Invariant 6/8).
//   - The magic read as a big-endian uint32 is ardrMagicValue (0x41524452,
//     ~1.09 GB), STRICTLY GREATER than MaxFrameBytes (16 MB). A normal
//     length-prefixed frame's u32 size header can therefore never equal the
//     magic, and the magic can never be read as a valid frame length — the two
//     wire kinds are disjoint by construction. This inequality is the frozen
//     Invariant 8 / gate P1-5, pinned by TestReDeclareMagicIsDisjointFromFrameLength.
//   - All integers are big-endian (as the length-prefixed protocol already is).
//   - Layout: magic(4B "ARDR") | frame_len(u32, BODY length only) |
//     scope_id_len(u32) | scope_id(utf8) | ram_bytes(u64) | cpu_cores(u32) |
//     parent_len(u32) | parent_scope_id(utf8, "" = no parent).
//   - A TOTAL parser: every byte sequence either decodes or is a hard, LOGGED
//     reject (a CodeProtocol-prefixed error). Never a silent or partial drop — a
//     truncated frame is a reject, not a best-effort record.
//   - Structural rejects (frozen): frame_len == 0 or above maxReDeclareFrameBytes;
//     any embedded length that overruns the declared body; trailing bytes after
//     parent_scope_id; an empty scope_id (a lease SET keyed by "" is meaningless);
//     invalid UTF-8 in scope_id or parent_scope_id; ram_bytes above math.MaxInt64
//     (the int64 signed ledger cannot represent, hence cannot charge, it).
//   - Legal by design: cpu_cores == 0 (a --delegate-ram SUITE declares 0 cores,
//     design §8); an empty parent_scope_id (no parent); ram_bytes == 0.
//   - A frozen 1-byte ack (reDeclareAckByte) is written by the daemon after it
//     SETs the lease; the ack is order-independent (design §4).
//
// The SAME encoder/decoder is reused for the dump-on-shutdown records (§15,
// P2-B: "one Go encoder, one golden fixture"), so encode/decode round-trip the
// full frame (magic included). S9 wires a sniffed frame's charge() into the
// ledger SET+re-anchor; S10/S11 reuse this codec for the dump.
const (
	// ardrMagicValue is the 4-byte ARDR magic as a big-endian uint32:
	// 'A'0x41 'R'0x52 'D'0x44 'R'0x52. ~1.09 GB, far above MaxFrameBytes.
	ardrMagicValue = 0x41524452

	// reDeclareAckByte is the FROZEN single-byte re-declare acknowledgement the
	// daemon writes once a lease is SET (design §4). Its value is arbitrary but
	// frozen — a re-declaring client waits for exactly one byte, not a framed
	// response.
	reDeclareAckByte byte = 0x06 // ASCII ACK

	// maxReDeclareFrameBytes bounds the re-declare frame BODY. Scope ids are short
	// cgroup paths; 64 KiB is generous headroom and, sitting far below both
	// MaxFrameBytes (16 MB) and the magic (~1.09 GB), keeps a malformed length a
	// cheap hard reject rather than a large speculative allocation.
	maxReDeclareFrameBytes = 64 << 10
)

// ardrMagic is the on-wire byte order of the sniff token. Kept as a byte array
// (not derived from ardrMagicValue at runtime) so the invariant test can assert
// the two representations agree and neither can silently drift from the other.
var ardrMagic = [4]byte{'A', 'R', 'D', 'R'}

// reDeclareRecord is the decoded re-declare frame — a pure wire projection. The
// ledger meaning is charge(); nothing here is interpreted as a policy.
type reDeclareRecord struct {
	ScopeID       string
	RAMBytes      uint64
	CPUCores      uint32
	ParentScopeID string
}

// redeclareCharge is the SEMANTIC meaning of a re-declare frame: the exact
// per-resource lease SET the signed ledger will apply for ScopeID. The S9
// handler wires this straight into admitWaiter{reserve, cpu, scopeID,
// parentScopeID}; the golden-bytes test asserts THIS value (not field offsets),
// so the freeze is on meaning, not layout (design §4, Invariant 8).
type redeclareCharge struct {
	ScopeID       string
	RAM           int64
	CPU           int64
	ParentScopeID string
}

// charge maps the wire record to the ledger charge. RAMBytes is bounded to
// math.MaxInt64 at decode, so the int64 conversion never wraps negative.
func (r reDeclareRecord) charge() redeclareCharge {
	return redeclareCharge{
		ScopeID:       r.ScopeID,
		RAM:           int64(r.RAMBytes),
		CPU:           int64(r.CPUCores),
		ParentScopeID: r.ParentScopeID,
	}
}

// encodeReDeclareFrame renders the frozen full frame (magic + frame_len + body).
// It refuses to encode a record it would then refuse to decode, so the encoder
// and the decoder share one structural contract.
func encodeReDeclareFrame(rec reDeclareRecord) ([]byte, error) {
	if rec.ScopeID == "" {
		return nil, fmt.Errorf("%s: re-declare scope_id is empty", CodeProtocol)
	}
	if !utf8.ValidString(rec.ScopeID) || !utf8.ValidString(rec.ParentScopeID) {
		return nil, fmt.Errorf("%s: re-declare scope_id/parent_scope_id must be valid utf8", CodeProtocol)
	}
	if rec.RAMBytes > math.MaxInt64 {
		return nil, fmt.Errorf("%s: re-declare ram_bytes %d exceeds the int64 ledger range", CodeProtocol, rec.RAMBytes)
	}
	scope := []byte(rec.ScopeID)
	parent := []byte(rec.ParentScopeID)
	bodyLen := 4 + len(scope) + 8 + 4 + 4 + len(parent)
	if bodyLen > maxReDeclareFrameBytes {
		return nil, fmt.Errorf("%s: re-declare frame body %d exceeds %d", CodeProtocol, bodyLen, maxReDeclareFrameBytes)
	}
	buf := make([]byte, 0, 8+bodyLen)
	buf = append(buf, ardrMagic[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(bodyLen))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(scope)))
	buf = append(buf, scope...)
	buf = binary.BigEndian.AppendUint64(buf, rec.RAMBytes)
	buf = binary.BigEndian.AppendUint32(buf, rec.CPUCores)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(parent)))
	buf = append(buf, parent...)
	return buf, nil
}

// decodeReDeclareFrame reads and verifies one frozen frame from r, magic
// included, so it is symmetric with encodeReDeclareFrame and reusable as the
// dump-record reader. It is a TOTAL parser: it returns a record or a
// CodeProtocol-prefixed error, never a partial record and never a panic.
func decodeReDeclareFrame(r io.Reader) (reDeclareRecord, error) {
	var head [8]byte // magic(4) + frame_len(4)
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare header: %w", CodeProtocol, err)
	}
	if !bytes.Equal(head[:4], ardrMagic[:]) {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare magic mismatch", CodeProtocol)
	}
	bodyLen := binary.BigEndian.Uint32(head[4:8])
	if bodyLen == 0 || bodyLen > maxReDeclareFrameBytes {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare frame length %d is invalid", CodeProtocol, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare body: %w", CodeProtocol, err)
	}
	// Parse over the fixed body buffer. Every field bounds-checks against the
	// REMAINING bytes (`remaining < n`, never `cursor+n > len`, which a
	// 0xFFFFFFFF length would wrap past).
	cur := body
	scope, cur, err := takeReDeclareLenPrefixed(cur, "scope_id")
	if err != nil {
		return reDeclareRecord{}, err
	}
	if len(scope) == 0 {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare scope_id is empty", CodeProtocol)
	}
	if !utf8.Valid(scope) {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare scope_id is not valid utf8", CodeProtocol)
	}
	ram, cur, err := takeReDeclareU64(cur, "ram_bytes")
	if err != nil {
		return reDeclareRecord{}, err
	}
	if ram > math.MaxInt64 {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare ram_bytes %d exceeds the int64 ledger range", CodeProtocol, ram)
	}
	cpu, cur, err := takeReDeclareU32(cur, "cpu_cores")
	if err != nil {
		return reDeclareRecord{}, err
	}
	parent, cur, err := takeReDeclareLenPrefixed(cur, "parent_scope_id")
	if err != nil {
		return reDeclareRecord{}, err
	}
	if !utf8.Valid(parent) {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare parent_scope_id is not valid utf8", CodeProtocol)
	}
	if len(cur) != 0 {
		return reDeclareRecord{}, fmt.Errorf("%s: re-declare frame has %d trailing bytes", CodeProtocol, len(cur))
	}
	return reDeclareRecord{
		ScopeID:       string(scope),
		RAMBytes:      ram,
		CPUCores:      cpu,
		ParentScopeID: string(parent),
	}, nil
}

func takeReDeclareU32(b []byte, field string) (uint32, []byte, error) {
	if len(b) < 4 {
		return 0, nil, fmt.Errorf("%s: re-declare %s truncated (%d bytes remain)", CodeProtocol, field, len(b))
	}
	return binary.BigEndian.Uint32(b[:4]), b[4:], nil
}

func takeReDeclareU64(b []byte, field string) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, fmt.Errorf("%s: re-declare %s truncated (%d bytes remain)", CodeProtocol, field, len(b))
	}
	return binary.BigEndian.Uint64(b[:8]), b[8:], nil
}

func takeReDeclareLenPrefixed(b []byte, field string) ([]byte, []byte, error) {
	n, rest, err := takeReDeclareU32(b, field+" length")
	if err != nil {
		return nil, nil, err
	}
	// uint64 comparison so a huge n cannot overflow int arithmetic; `remaining < n`.
	if uint64(len(rest)) < uint64(n) {
		return nil, nil, fmt.Errorf("%s: re-declare %s length %d overruns %d remaining bytes", CodeProtocol, field, n, len(rest))
	}
	return rest[:n], rest[n:], nil
}
