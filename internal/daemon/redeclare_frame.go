package daemon

import (
	"io"

	"aira/internal/redeclare"
)

// The ARDR re-declare frame codec moved to the leaf package internal/redeclare
// (S13) so the runner client can SEND a frame without importing daemon (daemon
// imports runner — a codec in daemon would cycle). The codec is single-source
// there; this file keeps daemon's private vocabulary (thin aliases) so the rest
// of the daemon and its tests read unchanged, and it keeps the ledger MEANING —
// redeclareCharge — daemon-private, mapping a wire Record to the exact per-resource
// lease SET the signed ledger applies (design §4, Invariant 8).

// reDeclareRecord is the daemon's alias for the wire projection. A type alias (not
// a fresh type) so every existing daemon literal and field type reads unchanged
// and no conversion is needed at the codec boundary.
type reDeclareRecord = redeclare.Record

// Frozen wire constants, re-exported into daemon under their existing names.
const (
	reDeclareAckByte       = redeclare.AckByte
	maxReDeclareFrameBytes = redeclare.MaxBodyBytes
)

// ardrMagic is the daemon's handle on the sniff token (server.serveConnection
// compares the four sniffed bytes against it, and the disjointness invariant test
// decodes it).
var ardrMagic = redeclare.Magic

// encodeReDeclareFrame / decodeReDeclareFrame forward to the single codec. They
// are one-line wrappers, NOT a second implementation — the wire logic lives once,
// in internal/redeclare.
func encodeReDeclareFrame(rec reDeclareRecord) ([]byte, error) { return redeclare.EncodeFrame(rec) }

func decodeReDeclareFrame(r io.Reader) (reDeclareRecord, error) { return redeclare.DecodeFrame(r) }

// redeclareCharge is the SEMANTIC meaning of a re-declare frame: the exact
// per-resource lease SET the signed ledger will apply for ScopeID. The S9 handler
// wires this straight into admitWaiter{reserve, cpu, scopeID, parentScopeID}; the
// golden-bytes test asserts THIS value (not field offsets), so the freeze is on
// meaning, not layout (design §4, Invariant 8). It is daemon-private: the runner
// client only ENCODES a frame; only the daemon interprets one as a ledger charge.
type redeclareCharge struct {
	ScopeID       string
	RAM           int64
	CPU           int64
	ParentScopeID string
}

// redeclareChargeOf maps a wire record to the ledger charge. RAMBytes is bounded
// to math.MaxInt64 at decode, so the int64 conversion never wraps negative.
func redeclareChargeOf(r reDeclareRecord) redeclareCharge {
	return redeclareCharge{
		ScopeID:       r.ScopeID,
		RAM:           int64(r.RAMBytes),
		CPU:           int64(r.CPUCores),
		ParentScopeID: r.ParentScopeID,
	}
}
