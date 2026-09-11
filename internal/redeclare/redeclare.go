// Package redeclare is the leaf home of the version-FROZEN ARDR re-declare frame
// codec (design §4). It is imported by BOTH internal/daemon (the SET-or-establish
// handler + the restart dump) and internal/runner (the client that SENDS a
// re-declare across a daemon restart). It has no non-stdlib dependency, so it
// breaks the daemon↔runner import cycle: internal/daemon imports internal/runner,
// so the codec cannot live in daemon if the runner client is to reuse it, and it
// must NOT be duplicated — S7's frozen contract is one encoder, one golden fixture.
//
// FROZEN CONTRACT (changing any of this needs a new frame kind, not an edit):
//   - Sniffed by its 4-byte magic "ARDR" BEFORE any framing/handshake and BEFORE
//     the protocol-version check (daemon server.serveConnection). That ordering is
//     load-bearing: a version skew must NOT refuse a re-declare (Invariant 6/8).
//   - The magic read as a big-endian uint32 is 0x41524452 (~1.09 GB), STRICTLY
//     GREATER than the daemon's MaxFrameBytes (16 MB). A normal length-prefixed
//     frame's u32 size header can therefore never equal the magic, and the magic
//     can never be read as a valid frame length — the two wire kinds are disjoint
//     by construction. That inequality is the frozen Invariant 8 / gate P1-5,
//     pinned by daemon.TestReDeclareMagicIsDisjointFromFrameLength (which stays in
//     daemon because it references daemon.MaxFrameBytes).
//   - All integers are big-endian (as the length-prefixed protocol already is).
//   - Layout: magic(4B "ARDR") | frame_len(u32, BODY length only) |
//     scope_id_len(u32) | scope_id(utf8) | ram_bytes(u64) | cpu_cores(u32) |
//     parent_len(u32) | parent_scope_id(utf8, "" = no parent).
//   - A TOTAL parser: every byte sequence either decodes or is a hard, LOGGED
//     reject (a CodeProtocol-prefixed error). Never a silent or partial drop — a
//     truncated frame is a reject, not a best-effort record.
//   - Structural rejects (frozen): frame_len == 0 or above MaxBodyBytes; any
//     embedded length that overruns the declared body; trailing bytes after
//     parent_scope_id; an empty scope_id (a lease SET keyed by "" is meaningless);
//     invalid UTF-8 in scope_id or parent_scope_id; ram_bytes above math.MaxInt64
//     (the int64 signed ledger cannot represent, hence cannot charge, it).
//   - Legal by design: cpu_cores == 0 (a --delegate-ram SUITE declares 0 cores,
//     design §8); an empty parent_scope_id (no parent); ram_bytes == 0.
//   - A frozen 1-byte ack (AckByte) is written by the daemon after it SETs the
//     lease; the ack is order-independent (design §4).
package redeclare

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

// CodeProtocol prefixes every hard reject the codec returns. It is redeclare's
// OWN copy of the "E_DAEMON_PROTOCOL" spelling (daemon keeps its identical
// daemon.CodeProtocol; runner keeps its own duplicate pinned by
// worker_admit_outcome_test.go). The value is frozen; a re-declare reject reads
// the same to every peer regardless of which package produced it.
const CodeProtocol = "E_DAEMON_PROTOCOL"

// AckByte is the FROZEN single-byte re-declare acknowledgement the daemon writes
// once a lease is SET (design §4). Its value is arbitrary but frozen — a
// re-declaring client waits for exactly one byte, not a framed response.
const AckByte byte = 0x06 // ASCII ACK

// MaxBodyBytes bounds the re-declare frame BODY. Scope ids are short cgroup
// paths; 64 KiB is generous headroom and, sitting far below both the daemon's
// MaxFrameBytes (16 MB) and the magic (~1.09 GB), keeps a malformed length a
// cheap hard reject rather than a large speculative allocation.
const MaxBodyBytes = 64 << 10

// Magic is the on-wire byte order of the sniff token, and the single source of
// truth for the magic. The daemon's disjointness invariant test decodes it to a
// uint32 and asserts that value stays strictly above MaxFrameBytes, so the
// frame-length/magic separation cannot silently drift.
var Magic = [4]byte{'A', 'R', 'D', 'R'}

// Record is the decoded re-declare frame — a pure wire projection. The ledger
// meaning (daemon's redeclareCharge) is derived from it; nothing here is
// interpreted as a policy.
type Record struct {
	ScopeID       string
	RAMBytes      uint64
	CPUCores      uint32
	ParentScopeID string
}

// EncodeFrame renders the frozen full frame (magic + frame_len + body). It
// refuses to encode a record it would then refuse to decode, so the encoder and
// the decoder share one structural contract.
func EncodeFrame(rec Record) ([]byte, error) {
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
	if bodyLen > MaxBodyBytes {
		return nil, fmt.Errorf("%s: re-declare frame body %d exceeds %d", CodeProtocol, bodyLen, MaxBodyBytes)
	}
	buf := make([]byte, 0, 8+bodyLen)
	buf = append(buf, Magic[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(bodyLen))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(scope)))
	buf = append(buf, scope...)
	buf = binary.BigEndian.AppendUint64(buf, rec.RAMBytes)
	buf = binary.BigEndian.AppendUint32(buf, rec.CPUCores)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(parent)))
	buf = append(buf, parent...)
	return buf, nil
}

// DecodeFrame reads and verifies one frozen frame from r, magic included, so it
// is symmetric with EncodeFrame and reusable as the dump-record reader. It is a
// TOTAL parser: it returns a record or a CodeProtocol-prefixed error, never a
// partial record and never a panic.
func DecodeFrame(r io.Reader) (Record, error) {
	var head [8]byte // magic(4) + frame_len(4)
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Record{}, fmt.Errorf("%s: re-declare header: %w", CodeProtocol, err)
	}
	if !bytes.Equal(head[:4], Magic[:]) {
		return Record{}, fmt.Errorf("%s: re-declare magic mismatch", CodeProtocol)
	}
	bodyLen := binary.BigEndian.Uint32(head[4:8])
	if bodyLen == 0 || bodyLen > MaxBodyBytes {
		return Record{}, fmt.Errorf("%s: re-declare frame length %d is invalid", CodeProtocol, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return Record{}, fmt.Errorf("%s: re-declare body: %w", CodeProtocol, err)
	}
	// Parse over the fixed body buffer. Every field bounds-checks against the
	// REMAINING bytes (`remaining < n`, never `cursor+n > len`, which a
	// 0xFFFFFFFF length would wrap past).
	cur := body
	scope, cur, err := takeLenPrefixed(cur, "scope_id")
	if err != nil {
		return Record{}, err
	}
	if len(scope) == 0 {
		return Record{}, fmt.Errorf("%s: re-declare scope_id is empty", CodeProtocol)
	}
	if !utf8.Valid(scope) {
		return Record{}, fmt.Errorf("%s: re-declare scope_id is not valid utf8", CodeProtocol)
	}
	ram, cur, err := takeU64(cur, "ram_bytes")
	if err != nil {
		return Record{}, err
	}
	if ram > math.MaxInt64 {
		return Record{}, fmt.Errorf("%s: re-declare ram_bytes %d exceeds the int64 ledger range", CodeProtocol, ram)
	}
	cpu, cur, err := takeU32(cur, "cpu_cores")
	if err != nil {
		return Record{}, err
	}
	parent, cur, err := takeLenPrefixed(cur, "parent_scope_id")
	if err != nil {
		return Record{}, err
	}
	if !utf8.Valid(parent) {
		return Record{}, fmt.Errorf("%s: re-declare parent_scope_id is not valid utf8", CodeProtocol)
	}
	if len(cur) != 0 {
		return Record{}, fmt.Errorf("%s: re-declare frame has %d trailing bytes", CodeProtocol, len(cur))
	}
	return Record{
		ScopeID:       string(scope),
		RAMBytes:      ram,
		CPUCores:      cpu,
		ParentScopeID: string(parent),
	}, nil
}

func takeU32(b []byte, field string) (uint32, []byte, error) {
	if len(b) < 4 {
		return 0, nil, fmt.Errorf("%s: re-declare %s truncated (%d bytes remain)", CodeProtocol, field, len(b))
	}
	return binary.BigEndian.Uint32(b[:4]), b[4:], nil
}

func takeU64(b []byte, field string) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, fmt.Errorf("%s: re-declare %s truncated (%d bytes remain)", CodeProtocol, field, len(b))
	}
	return binary.BigEndian.Uint64(b[:8]), b[8:], nil
}

func takeLenPrefixed(b []byte, field string) ([]byte, []byte, error) {
	n, rest, err := takeU32(b, field+" length")
	if err != nil {
		return nil, nil, err
	}
	// uint64 comparison so a huge n cannot overflow int arithmetic; `remaining < n`.
	if uint64(len(rest)) < uint64(n) {
		return nil, nil, fmt.Errorf("%s: re-declare %s length %d overruns %d remaining bytes", CodeProtocol, field, n, len(rest))
	}
	return rest[:n], rest[n:], nil
}
