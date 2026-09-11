package daemon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Restart lease dump (design §4, §15 P2-A/P2-B). On a GRACEFUL daemon shutdown
// the live-lease ledger is written to a small file so a fresh daemon (S11) can
// PRE-SEED its ledger before the 2 s new-admission freeze, rather than starting
// empty and relying solely on reconnect/re-declare.
//
// BEST-EFFORT, NOT a correctness requirement (advisor 2026-09-11). S9 makes an
// absent-lease re-declare ESTABLISH the lease granted, so a missing, partial,
// stale, or failed dump is fully recovered by the re-declare path (the
// crash-restart-with-no-dump case is already handled that way). The dump's ONLY
// job is to keep a SLOW re-declarer from losing its space to a new admission
// after the freeze. Therefore the writer FAILS OPEN on any error: log and
// continue, NO retries, NO durability machinery beyond one CreateTemp + write +
// fsync + rename. A dump that fails just falls back to re-declare.
//
// This is NOT the §12 CI `--dump <file>` (that writes JSONL admission/utilisation
// records for external archival). This is the binary lease ledger for restart.
//
// ============================ FROZEN DUMP FORMAT ============================
// S11 READS exactly this; a layout change needs a new version, not an edit.
//
//	File header (20 bytes, big-endian):
//	  magic        [4]byte  = "ALDR"  (Aira Lease Dump Records; distinct from the
//	                                   record frame's "ARDR" magic)
//	  version      u32      = 1
//	  stamp_unixns u64      wall-clock time.Now().UnixNano() at write time; the
//	                        freshness source (S11 compares against time.Now() —
//	                        wall clock, because Go's monotonic reading does not
//	                        survive a process restart)
//	  record_count u32
//	Then record_count records, each:
//	  ardr_frame            one FROZEN ARDR re-declare frame (redeclare_frame.go:
//	                        self-delimiting magic + frame_len + body; carries
//	                        scope_id, ram_bytes, cpu_cores, parent_scope_id)
//	  client_pid   u64      the lease's anchored peer pid; 0 (unreadable credential
//	                        at anchor time) is dumped HONESTLY, never filtered —
//	                        S11 decides what to do with a pid-0 record and MUST
//	                        never `kill -0 0`
//	  start_tick   u64      the peer's /proc start-tick, for S11's pid-recycle-safe
//	                        `kill -0` liveness probe
//
// The record is self-delimiting (the ARDR frame is length-framed, then exactly
// 16 trailing bytes). It is NOT resyncable: a malformed record loses frame
// alignment, so decodeLeaseDump returns the records decoded so far PLUS the
// error. "An unparseable record is logged and skipped" (§15 P2-B) therefore
// means everything from the first bad record onward is dropped; the leading
// prefix is still usable (best-effort; re-declare recovers the rest).
//
// scope_id is stored VERBATIM as the ledger keys it (owner-suffixed, e.g.
// "CONFINE-...@session"), NOT a bare cgroup id — S11 must establish the lease
// under that exact key so the re-anchoring re-declare matches it.
//
// WHY WRAP the ARDR frame rather than reuse it whole: the ARDR frame is the WIRE
// re-declare shape and carries only {scope_id, ram, cpu, parent}. The dump needs
// two fields the frame does NOT carry — client_pid and process-start-tick, which
// S8 put on admitWaiter for S11's kill-probe. So each dump record is [ARDR frame]
// + pid + start-tick; the frame encoder/decoder is reused unchanged for its half.
//
// OUT OF SCOPE of the dump: scope-less `confine-reserve` reservations (scopeID ==
// "") have no ARDR representation and no re-declare key, so they are skipped (see
// snapshotLeaseDump); and aitest WORKER leases (design §15 P2-F) are a different
// shape reconstructed by S11's one-readdir worker-ID re-seed, not by this dump.
const (
	leaseDumpFileName = "admission-leases.dump"
	leaseDumpVersion  = uint32(1)

	// leaseDumpFreshness is the maximum age a reloading daemon (S11) accepts
	// before treating the dump as a reboot/first-upgrade (→ start at full quota,
	// which is correct). It must be >= DrainTimeout (10 s) + RestartSec (2 s) +
	// margin, so a slow graceful drain does not silently make its own dump stale
	// (§15 P2-A). S10 STAMPS the dump with the write time; S11 ENFORCES this
	// threshold. Kept here so the stamp and its threshold live in one file.
	leaseDumpFreshness = 30 * time.Second

	// leaseDumpHeaderLen is magic(4) + version(4) + stamp(8) + count(4).
	leaseDumpHeaderLen = 20
	// leaseDumpRecordTailLen is client_pid(8) + start_tick(8) after each frame.
	leaseDumpRecordTailLen = 16
)

// leaseDumpMagic separates a dump file from any other bytes at the file head.
var leaseDumpMagic = [4]byte{'A', 'L', 'D', 'R'}

// leaseDumpPath is where the restart dump lives: RuntimeDir (XDG_RUNTIME_DIR,
// tmpfs) so a real reboot wipes it and the fresh daemon correctly starts at full
// quota (design §4 "no fresh dump file, so the reload is skipped").
func leaseDumpPath(paths Paths) string {
	return filepath.Join(paths.RuntimeDir, leaseDumpFileName)
}

// leaseDumpRecord is one live lease as it is dumped and reloaded: the re-declare
// frame projection plus the two restart-only fields the frame does not carry.
type leaseDumpRecord struct {
	Frame            reDeclareRecord
	ClientPID        int
	ProcessStartTick uint64
}

// encodeLeaseDumpRecord renders one record: the frozen ARDR frame followed by the
// pid and start-tick. It refuses a negative pid (a real pid is non-negative; 0 is
// legal and meaningful — honest "unreadable credential") and inherits every
// structural refusal of the frame encoder, so it cannot emit a record the decoder
// would reject.
func encodeLeaseDumpRecord(rec leaseDumpRecord) ([]byte, error) {
	if rec.ClientPID < 0 {
		return nil, fmt.Errorf("%s: lease dump client_pid %d is negative", CodeProtocol, rec.ClientPID)
	}
	frame, err := encodeReDeclareFrame(rec.Frame)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, len(frame)+leaseDumpRecordTailLen)
	buf = append(buf, frame...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(rec.ClientPID))
	buf = binary.BigEndian.AppendUint64(buf, rec.ProcessStartTick)
	return buf, nil
}

// decodeLeaseDumpRecord reads one record from r. TOTAL, like the frame decoder:
// it returns a record or a CodeProtocol error, never a partial record or a panic.
func decodeLeaseDumpRecord(r io.Reader) (leaseDumpRecord, error) {
	frame, err := decodeReDeclareFrame(r)
	if err != nil {
		return leaseDumpRecord{}, err
	}
	var tail [leaseDumpRecordTailLen]byte
	if _, err := io.ReadFull(r, tail[:]); err != nil {
		return leaseDumpRecord{}, fmt.Errorf("%s: lease dump record pid/start-tick: %w", CodeProtocol, err)
	}
	pid := binary.BigEndian.Uint64(tail[:8])
	if pid > uint64(math.MaxInt64) {
		return leaseDumpRecord{}, fmt.Errorf("%s: lease dump client_pid %d exceeds the int range", CodeProtocol, pid)
	}
	return leaseDumpRecord{
		Frame:            frame,
		ClientPID:        int(pid),
		ProcessStartTick: binary.BigEndian.Uint64(tail[8:]),
	}, nil
}

// encodeLeaseDump renders the whole dump file. stamp is the write time the
// freshness check reads back; it is a parameter (not time.Now()) so a golden test
// can freeze the whole file, header included. Every record is encoded before the
// header is laid down, so a per-record encode error aborts without committing a
// header with a wrong count.
func encodeLeaseDump(stamp time.Time, recs []leaseDumpRecord) ([]byte, error) {
	if len(recs) > math.MaxUint32 {
		return nil, fmt.Errorf("%s: lease dump has too many records (%d)", CodeProtocol, len(recs))
	}
	var body []byte
	for i, rec := range recs {
		encoded, err := encodeLeaseDumpRecord(rec)
		if err != nil {
			return nil, fmt.Errorf("%s: lease dump record %d: %w", CodeProtocol, i, err)
		}
		body = append(body, encoded...)
	}
	buf := make([]byte, 0, leaseDumpHeaderLen+len(body))
	buf = append(buf, leaseDumpMagic[:]...)
	buf = binary.BigEndian.AppendUint32(buf, leaseDumpVersion)
	buf = binary.BigEndian.AppendUint64(buf, uint64(stamp.UnixNano()))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(recs)))
	buf = append(buf, body...)
	return buf, nil
}

// leaseDump is the decoded file: the write-time stamp and the records.
type leaseDump struct {
	Stamp   time.Time
	Records []leaseDumpRecord
}

// decodeLeaseDump reads a whole dump file. On a malformed record it returns the
// records decoded so far PLUS the error (the format is self-delimiting but not
// resyncable), so S11 can log-and-use the prefix. The record count is NOT used to
// preallocate — a hostile count cannot force a large allocation; records are
// appended as they decode.
func decodeLeaseDump(r io.Reader) (leaseDump, error) {
	var head [leaseDumpHeaderLen]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return leaseDump{}, fmt.Errorf("%s: lease dump header: %w", CodeProtocol, err)
	}
	if !bytes.Equal(head[:4], leaseDumpMagic[:]) {
		return leaseDump{}, fmt.Errorf("%s: lease dump magic mismatch", CodeProtocol)
	}
	if version := binary.BigEndian.Uint32(head[4:8]); version != leaseDumpVersion {
		return leaseDump{}, fmt.Errorf("%s: lease dump version %d unsupported (want %d)", CodeProtocol, version, leaseDumpVersion)
	}
	stampNs := int64(binary.BigEndian.Uint64(head[8:16]))
	count := binary.BigEndian.Uint32(head[16:20])
	dump := leaseDump{Stamp: time.Unix(0, stampNs)}
	for i := uint32(0); i < count; i++ {
		rec, err := decodeLeaseDumpRecord(r)
		if err != nil {
			return dump, fmt.Errorf("%s: lease dump record %d of %d: %w", CodeProtocol, i, count, err)
		}
		dump.Records = append(dump.Records, rec)
	}
	// TOTAL parser: no bytes may follow the counted records. The frame decoder rejects
	// its own trailing bytes; the file decoder must too, or a byte sequence would neither
	// decode-as-intended nor reject. Unreachable from the atomic-rename writer, but
	// "decode cleanly or reject" is the contract S11 relies on.
	var tail [1]byte
	if n, _ := io.ReadFull(r, tail[:]); n > 0 {
		return dump, fmt.Errorf("%s: lease dump has trailing bytes after %d record(s)", CodeProtocol, count)
	}
	return dump, nil
}

// snapshotLeaseDump collects every still-held lease across all admission queues
// as dump records. It takes admitRegistryMu to copy the queue set, RELEASES it,
// then takes each queue.mu in turn (the established admitRegistryMu -> queue.mu
// order, and never two queue locks at once) so a consistent per-queue snapshot is
// taken without risking lock-order issues across queues.
//
// Call it BEFORE close(stopping) in Serve: close(stopping) is what releases the
// leases (admit.go's granted-lease select returns) and closes their connections,
// so a snapshot after it would capture nothing.
func (s *Server) snapshotLeaseDump() []leaseDumpRecord {
	s.admitRegistryMu.Lock()
	queues := make([]*sliceQueue, 0, len(s.admitQueues))
	for _, queue := range s.admitQueues {
		queues = append(queues, queue)
	}
	s.admitRegistryMu.Unlock()

	var recs []leaseDumpRecord
	for _, queue := range queues {
		queue.mu.Lock()
		for _, waiter := range queue.waiters {
			// The ledger predicate, exactly as rederiveLedgerLocked sums it: a lease
			// counts iff it is granted AND accounted.
			if waiter == nil || waiter.state != admitGranted || !waiter.accounted {
				continue
			}
			// A scope-less `confine-reserve` reservation (scopeID == "") has NO ARDR
			// representation (the frame refuses an empty scope_id) and no re-declare
			// key, so it is outside the dump/reload mechanism entirely. Skipping it is
			// FORCED by the frozen frame, not a policy choice — and such a reservation
			// is released only by its connection closing (admit_release_e2e_test), so a
			// restart loses it regardless. Best-effort.
			if waiter.scopeID == "" {
				continue
			}
			recs = append(recs, leaseDumpRecord{
				Frame: reDeclareRecord{
					// The ledger key VERBATIM; S11 establishes the lease under this.
					ScopeID: waiter.scopeID,
					// reserve is a non-negative RAM byte count validated to [0, admitMaxReserve]
					// at admission, so it always fits the frame encoder's MaxInt64 bound. If the
					// encoder ever DID refuse, encodeLeaseDump aborts the WHOLE dump (there is no
					// per-record skip) and the write fails open — not a silent partial file.
					RAMBytes: uint64(waiter.reserve),
					// cpu is 0..cpuCeiling() (== 2*NumCPU, admit.go's fail-fast), so it
					// fits u32 and is never negative.
					CPUCores:      uint32(waiter.cpu),
					ParentScopeID: waiter.parentScopeID,
				},
				// pid == 0 (credential unreadable at anchor time) is dumped HONESTLY.
				ClientPID:        waiter.clientPID,
				ProcessStartTick: waiter.processStartTick,
			})
		}
		queue.mu.Unlock()
	}
	return recs
}

// writeLeaseDump atomically replaces the restart dump with recs. BEST-EFFORT
// (design §15 P2-A): exactly one CreateTemp + write + fsync + rename, NO retries
// and NO further durability — the caller logs-and-continues on error. It ALWAYS
// writes, even with zero records: the rename atomically replaces any previous
// dump, so there is no stale-file special case.
func (s *Server) writeLeaseDump(recs []leaseDumpRecord) error {
	data, err := encodeLeaseDump(time.Now(), recs)
	if err != nil {
		return err
	}
	dir := s.Paths.RuntimeDir
	tmp, err := os.CreateTemp(dir, leaseDumpFileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, leaseDumpPath(s.Paths)); err != nil {
		return err
	}
	committed = true
	return nil
}

// dumpLeasesForRestart is the graceful-shutdown hook: snapshot the held leases
// and write the dump, BEFORE close(stopping) in Serve. Best-effort — a failure is
// logged and swallowed; S9's absent-lease re-declare recovers a missing or
// partial dump.
func (s *Server) dumpLeasesForRestart() {
	if err := s.writeLeaseDump(s.snapshotLeaseDump()); err != nil {
		log.Printf("aira daemon: restart lease dump failed (best-effort; re-declare recovers): %v", err)
	}
}
