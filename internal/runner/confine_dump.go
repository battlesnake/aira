package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// AIRA admission-counter rebuild, S18: `aira confine --dump <file>` writes the
// admission/utilisation records the daemon already collects (the peak-RSS
// history AIRA-180's `confine --budget` already classifies, plus the live
// per-slice queue state `confine --list` already snapshots) as JSONL, for an
// external CI job to ship to blob storage. AIRA writes the file only -- there
// is no network anywhere on this path (design §12).
//
// This is a SEPARATE, new, CI/archival feature. S13 deleted the OLD binary
// restart-lease dump (`internal/daemon/dump.go`, the ARDR-era ALDR format,
// design §4) entirely; nothing here resurrects it or shares its wire shape.

// Record-type discriminators. Every JSONL line carries one of these in its
// own "record_type" field so a consumer can demux a heterogeneous file
// without a schema registry.
const (
	ConfineDumpRecordAdmission = "admission"
	ConfineDumpRecordQueue     = "queue"
	// ConfineDumpRecordWaiter is a currently-live (in-memory) admission --
	// queued, or granted within this daemon process's lifetime -- as
	// distinct from ConfineDumpRecordAdmission's PERSISTED confine_peak_history
	// samples. See ConfineDumpWaiterRow.
	ConfineDumpRecordWaiter = "waiter"
)

// ConfineDumpUnevaluated is the honest sentinel for a string-valued field this
// dump cannot establish (AIRA's core honesty rule: `unevaluated`, never a
// fabricated pass/zero). Pointer-valued fields instead use a nil pointer,
// which marshals to JSON absence/null -- the same rule, the representation a
// numeric field allows.
const ConfineDumpUnevaluated = "unevaluated"

// ConfineDumpAdmissionRow is one persisted confine_peak_history sample,
// reshaped for archival (design §12: "declared reserve vs observed peak").
//
// Outcome and WaitMS are honestly ConfineDumpUnevaluated/nil for EVERY row
// here: confine-report (the sole writer of that table) is a POST-RUN
// self-report of peak_rss/oom and carries neither the admission outcome
// (grant/deny/fail-fast) nor the wait duration. Inventing either from an
// unrelated invariant (e.g. "every reporting job must have been granted")
// would be exactly the fabricated value the honesty rule forbids. Design
// §12's "per admission: ... wait time, and outcome" is instead met by
// ConfineDumpWaiterRow, for the population where those two terms are
// actually measurable: an admission still live in this daemon process's
// in-memory queue at dump time.
type ConfineDumpAdmissionRow struct {
	RecordType           string `json:"record_type"`
	Kind                 string `json:"kind"`
	Signature            string `json:"signature"`
	At                   string `json:"at,omitempty"`
	DeclaredReserveBytes *int64 `json:"declared_reserve_bytes,omitempty"`
	DeclaredReserveBasis string `json:"declared_reserve_basis,omitempty"`
	ObservedPeakBytes    *int64 `json:"observed_peak_bytes,omitempty"`
	OOM                  bool   `json:"oom"`
	Outcome              string `json:"outcome"`
	WaitMS               *int64 `json:"wait_ms,omitempty"`
}

// ConfineDumpQueueRow is one live admission queue's utilisation at dump time
// (design §12: "oldest-blocked wait", "negative-available excursions",
// "per-resource utilisation").
//
// AvailableBytes/NegativeAvailable are meaningful ONLY together: a nil
// AvailableBytes (the ceiling could not be read) leaves NegativeAvailable at
// its neutral false rather than asserting either direction -- never treat
// NegativeAvailable alone as an established fact. OldestBlockedWaitMS is nil
// exactly when QueuedCount is 0 (there is no oldest wait to report); zero
// milliseconds for a waiter enqueued in the same instant as the dump is a
// real, non-fabricated measurement, not "unevaluated".
type ConfineDumpQueueRow struct {
	RecordType          string `json:"record_type"`
	Slice               string `json:"slice"`
	RAMOutstandingBytes int64  `json:"ram_outstanding_bytes"`
	RAMCeilingBytes     *int64 `json:"ram_ceiling_bytes,omitempty"`
	AvailableBytes      *int64 `json:"available_bytes,omitempty"`
	NegativeAvailable   bool   `json:"negative_available"`
	CPUOutstandingCores int64  `json:"cpu_outstanding_cores"`
	CPUCeilingCores     int64  `json:"cpu_ceiling_cores"`
	QueuedCount         int    `json:"queued_count"`
	OldestBlockedWaitMS *int64 `json:"oldest_blocked_wait_ms,omitempty"`
	Phase               string `json:"phase"`
	RestartFrozen       bool   `json:"restart_frozen"`
}

// ConfineDumpWaiterRow is ONE currently-live admission -- queued or granted
// in this daemon process's in-memory queue at dump time -- carrying the
// wait/outcome terms design §12 asks for and ConfineDumpAdmissionRow's
// persisted population structurally cannot (see its doc comment).
//
// State/Outcome/WaitMS are all real, freshly-measured facts, never
// fabricated: a still-QUEUED waiter reports State "queued", Outcome
// ConfineDumpUnevaluated (its eventual grant/deny is not yet decided --
// asserting one now would be a fabrication in the other direction), and
// WaitMS as its wait SO FAR (a genuine, non-final measurement, not the
// eventual total). A GRANTED or REJECTED waiter reports its daemon-decided
// terminal Outcome and final WaitMS.
type ConfineDumpWaiterRow struct {
	RecordType   string `json:"record_type"`
	Slice        string `json:"slice"`
	ScopeID      string `json:"scope_id,omitempty"`
	Signature    string `json:"signature,omitempty"`
	ReserveBytes int64  `json:"reserve_bytes"`
	CPUCores     int64  `json:"cpu_cores"`
	State        string `json:"state"`
	Outcome      string `json:"outcome"`
	WaitMS       int64  `json:"wait_ms"`
}

// ConfineDumpResult is the whole `confine-dump` reply, mirroring
// ConfineBudgetResult's shape (verdict/reason/scope + rows) for consistency
// with the rest of the confine-management family.
type ConfineDumpResult struct {
	Verdict    string                    `json:"verdict"`
	Reason     string                    `json:"reason,omitempty"`
	Scope      string                    `json:"scope"`
	Admissions []ConfineDumpAdmissionRow `json:"admissions,omitempty"`
	Queues     []ConfineDumpQueueRow     `json:"queues,omitempty"`
	Waiters    []ConfineDumpWaiterRow    `json:"waiters,omitempty"`
}

// WriteConfineDumpJSONL writes result's rows as JSONL (Admissions, then
// Waiters, then Queues, one JSON object per line) to path, atomically:
// CreateTemp beside the target, write, fsync, close, rename -- so a reader
// never observes a partially-written file, and a failed write never
// clobbers a prior good dump. Mirrors the identical pattern in
// internal/runner/confine_mode.go's writeInstallModeRecord.
func WriteConfineDumpJSONL(path string, result ConfineDumpResult) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, row := range result.Admissions {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	for _, row := range result.Waiters {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	for _, row := range result.Queues {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".confine-dump-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(buffer.Bytes()); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
