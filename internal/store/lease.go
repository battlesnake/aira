package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aira/internal/domain"
	"golang.org/x/sys/unix"
)

// covers: AR-5, AR-6, AR-7

const (
	defaultLeaseTTLNS = uint64(15 * 60 * 1000 * 1000 * 1000)
	maxInt64Uint      = uint64(^uint64(0) >> 1)
)

var (
	ErrLeaseHeld        = errors.New("E_LEASE_HELD")
	ErrLeaseToken       = errors.New("E_LEASE_TOKEN")
	ErrLeaseExpired     = errors.New("E_LEASE_EXPIRED")
	ErrClockUnavailable = errors.New("E_CLOCK_UNAVAILABLE")
)

// Clock is the only source of lease liveness. Production uses CLOCK_MONOTONIC
// plus the kernel boot ID; tests inject a deterministic cross-process sample.
type Clock interface {
	Now() (bootID string, monoNS uint64, err error)
}

type systemClock struct{}

func (systemClock) Now() (string, uint64, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return "", 0, fmt.Errorf("%w: boot_id: %v", ErrClockUnavailable, err)
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return "", 0, fmt.Errorf("%w: CLOCK_MONOTONIC: %v", ErrClockUnavailable, err)
	}
	ns := ts.Nano()
	if ns < 0 {
		return "", 0, fmt.Errorf("%w: negative monotonic sample", ErrClockUnavailable)
	}
	return strings.TrimSpace(string(boot)), uint64(ns), nil
}

type LeaseClaim struct {
	Lease  domain.Lease `json:"lease"`
	Token  string       `json:"token"`
	Stolen bool         `json:"stolen,omitempty"`
	Event  EventKey     `json:"event"`
}

// HeldLeaseRow is the read-only wire representation of a currently held
// lease. It deliberately omits the holder token hash.
type HeldLeaseRow struct {
	TicketID               string `json:"ticket_id"`
	Actor                  string `json:"actor"`
	WorktreeID             string `json:"worktree_id"`
	Generation             int64  `json:"generation"`
	TTLNanos               int64  `json:"ttl_ns"`
	LastHeartbeatMonoNanos int64  `json:"last_heartbeat_mono_ns"`
	Expired                bool   `json:"expired"`
	AgeNote                string `json:"age_note"`
}

type leaseRow struct {
	state               string
	generation          int64
	holderTokenHash     sql.NullString
	bootID              sql.NullString
	lastHeartbeatMonoNS sql.NullInt64
	ttlNS               sql.NullInt64
	actor               sql.NullString
	worktree            sql.NullString
}

type reapCandidate struct {
	ticketID   string
	generation int64
}

func defaultLeaseStateDir() string {
	if value := os.Getenv("XDG_STATE_HOME"); value != "" {
		return filepath.Join(value, "aira")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "aira-state")
	}
	return filepath.Join(home, ".local", "state", "aira")
}

func (s *Store) leaseTokenPath(id string) string {
	return filepath.Join(s.leaseStateDir, "leases", s.projectID, s.worktreeID, id+".token")
}

func (s *Store) leaseTokenLock(id string) string {
	// Key the lock by the actual token path. This reuses the common path-lock
	// mechanism and also coordinates cooperating claimants sharing one token
	// directory.
	return s.pathLockFor("lease-token", s.leaseTokenPath(id))
}

// LeaseToken reads the local clear token. It is intentionally outside the DB
// and is never included in an event or ticket file.
func (s *Store) LeaseToken(id string) (string, error) {
	data, err := os.ReadFile(s.leaseTokenPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrLeaseToken
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (s *Store) sampleClock() (string, uint64, error) {
	boot, mono, err := s.clock.Now()
	if err != nil {
		if ErrorCode(err) == "E_CLOCK_UNAVAILABLE" {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("%w: %v", ErrClockUnavailable, err)
	}
	if boot == "" {
		return "", 0, ErrClockUnavailable
	}
	if mono > maxInt64Uint {
		return "", 0, fmt.Errorf("%w: monotonic sample exceeds SQLite INTEGER", ErrClockUnavailable)
	}
	return boot, mono, nil
}

func leaseToken() (string, [32]byte, error) {
	var clear [32]byte
	if _, err := rand.Read(clear[:]); err != nil {
		return "", [32]byte{}, err
	}
	hash := sha256.Sum256(clear[:])
	return base64.RawURLEncoding.EncodeToString(clear[:]), hash, nil
}

func leaseFromRow(ticketID string, row leaseRow) (domain.Lease, error) {
	if row.generation < 0 {
		return domain.Lease{}, errors.New("E_CONFIG_INVALID: negative lease generation")
	}
	if row.state == "free" {
		if row.holderTokenHash.Valid || row.bootID.Valid || row.lastHeartbeatMonoNS.Valid || row.ttlNS.Valid || row.actor.Valid || row.worktree.Valid {
			return domain.Lease{}, errors.New("E_CONFIG_INVALID: free lease carries holder state")
		}
		free, err := domain.NewFreeLease(uint64(row.generation))
		if err != nil {
			return domain.Lease{}, err
		}
		return domain.NewLease(ticketID, free)
	}
	if row.state != "held" || row.generation < 1 || !row.holderTokenHash.Valid || strings.TrimSpace(row.holderTokenHash.String) == "" ||
		!row.bootID.Valid || strings.TrimSpace(row.bootID.String) == "" || !row.lastHeartbeatMonoNS.Valid || row.lastHeartbeatMonoNS.Int64 < 0 ||
		!row.ttlNS.Valid || row.ttlNS.Int64 <= 0 || !row.actor.Valid || strings.TrimSpace(row.actor.String) == "" ||
		!row.worktree.Valid || strings.TrimSpace(row.worktree.String) == "" {
		return domain.Lease{}, errors.New("E_CONFIG_INVALID: malformed held lease")
	}
	hashBytes, err := base64.RawURLEncoding.DecodeString(row.holderTokenHash.String)
	if err != nil || len(hashBytes) != sha256.Size {
		return domain.Lease{}, errors.New("E_CONFIG_INVALID: malformed lease token hash")
	}
	var hash [32]byte
	copy(hash[:], hashBytes)
	held, err := domain.NewHeldLease(hash[:], row.bootID.String, uint64(row.lastHeartbeatMonoNS.Int64), row.ttlNS.Int64,
		uint64(row.generation), row.actor.String, row.worktree.String)
	if err != nil {
		return domain.Lease{}, err
	}
	return domain.NewLease(ticketID, held)
}

func readLeaseRow(ctx context.Context, conn interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, project, ticketID string) (leaseRow, error) {
	var row leaseRow
	err := conn.QueryRowContext(ctx, `SELECT state, generation, holder_token_hash, boot_id,
        last_heartbeat_mono_ns, ttl_ns, actor, worktree_id FROM leases WHERE project_id=? AND ticket_id=?`, project, ticketID).
		Scan(&row.state, &row.generation, &row.holderTokenHash, &row.bootID, &row.lastHeartbeatMonoNS, &row.ttlNS, &row.actor, &row.worktree)
	return row, err
}

func (s *Store) Claim(ctx context.Context, ticketID string, steal bool, actor string) (LeaseClaim, error) {
	if err := domain.ValidateID(ticketID); err != nil {
		return LeaseClaim{}, err
	}
	if err := s.ticketExists(ticketID); err != nil {
		return LeaseClaim{}, err
	}
	if actor == "" {
		actor = "aira"
	}
	clear, hash, err := leaseToken()
	if err != nil {
		return LeaseClaim{}, err
	}
	if s.leaseTTLNS > maxInt64Uint {
		return LeaseClaim{}, fmt.Errorf("%w: lease TTL exceeds SQLite INTEGER", ErrClockUnavailable)
	}
	tokenTemp, err := s.writeLeaseTokenTemp(ticketID, clear)
	if err != nil {
		return LeaseClaim{}, err
	}
	var result LeaseClaim
	result.Token = clear
	tokenLock, err := acquireLock(s.leaseTokenLock(ticketID))
	if err != nil {
		_ = os.Remove(tokenTemp)
		return LeaseClaim{}, err
	}
	defer unlockFile(tokenLock)
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		if s.afterLeaseBegin != nil {
			s.afterLeaseBegin()
		}
		// A contender may have waited for BEGIN IMMEDIATE. Sample only after
		// the writer lock is acquired so its sample cannot predate the lease
		// that it is about to inspect.
		bootID, monoNS, err := s.sampleClock()
		if err != nil {
			return err
		}
		row, rowErr := readLeaseRow(ctx, conn, s.projectID, ticketID)
		if errors.Is(rowErr, sql.ErrNoRows) {
			if _, err := conn.ExecContext(ctx, `INSERT INTO leases(project_id, ticket_id, state, generation,
                    holder_token_hash, boot_id, last_heartbeat_mono_ns, ttl_ns, actor, worktree_id)
                    VALUES(?, ?, 'held', 1, ?, ?, ?, ?, ?, ?)`, s.projectID, ticketID,
				base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS), int64(s.leaseTTLNS), actor, s.worktreeID); err != nil {
				return err
			}
			held, err := domain.NewHeldLease(hash[:], bootID, monoNS, int64(s.leaseTTLNS), 1, actor, s.worktreeID)
			if err != nil {
				return err
			}
			result.Lease, err = domain.NewLease(ticketID, held)
			if err != nil {
				return err
			}
		} else {
			if rowErr != nil {
				return rowErr
			}
			current, err := leaseFromRow(ticketID, row)
			if err != nil {
				return err
			}
			currentHeld, isHeld := current.Held()
			if isHeld && currentHeld.IsLive(bootID, monoNS) {
				return ErrLeaseHeld
			}
			if isHeld && !steal {
				return ErrLeaseExpired
			}
			generation := currentHeld.Generation() + 1
			if !isHeld {
				free, _ := current.Free()
				generation = free.Generation + 1
			}
			result.Stolen = isHeld
			var updateResult sql.Result
			if isHeld {
				updateResult, err = conn.ExecContext(ctx, `UPDATE leases SET state='held', generation=?, holder_token_hash=?, boot_id=?,
                    last_heartbeat_mono_ns=?, ttl_ns=?, actor=?, worktree_id=? WHERE project_id=? AND ticket_id=? AND state='held' AND generation=?
					AND (boot_id<>? OR (? >= last_heartbeat_mono_ns AND ? - last_heartbeat_mono_ns >= ttl_ns))`,
					int64(generation), base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS), int64(s.leaseTTLNS), actor,
					s.worktreeID, s.projectID, ticketID, row.generation, bootID, int64(monoNS), int64(monoNS))
			} else {
				updateResult, err = conn.ExecContext(ctx, `UPDATE leases SET state='held', generation=?, holder_token_hash=?, boot_id=?,
                    last_heartbeat_mono_ns=?, ttl_ns=?, actor=?, worktree_id=? WHERE project_id=? AND ticket_id=? AND state='free' AND generation=?`,
					int64(generation), base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS), int64(s.leaseTTLNS), actor,
					s.worktreeID, s.projectID, ticketID, row.generation)
			}
			if err != nil {
				return err
			}
			if affected, err := updateResult.RowsAffected(); err != nil {
				return err
			} else if affected != 1 {
				return ErrLeaseHeld
			}
			held, err := domain.NewHeldLease(hash[:], bootID, monoNS, int64(s.leaseTTLNS), generation, actor, s.worktreeID)
			if err != nil {
				return err
			}
			result.Lease, err = domain.NewLease(ticketID, held)
			if err != nil {
				return err
			}
		}
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		verb := "lease.claim"
		if result.Stolen {
			verb = "lease.steal"
		}
		if err := insertEventActor(ctx, conn, s.projectID, seq, actor, verb, ticketID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
                precondition_digest, intended_digest, intended_bytes, materialised, allocation_id)
                VALUES(?, ?, ?, '', ?, '', '', NULL, 1, '')`, s.projectID, seq, s.worktreeID, verb); err != nil {
			return err
		}
		result.Event = EventKey{ProjectID: s.projectID, Seq: seq}
		if s.beforeLeaseCommit != nil {
			if err := s.beforeLeaseCommit(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = os.Remove(tokenTemp)
		return LeaseClaim{}, err
	}
	held, ok := result.Lease.Held()
	if !ok {
		_ = os.Remove(tokenTemp)
		return LeaseClaim{}, errors.New("E_CONFIG_INVALID: claim committed a non-held lease")
	}
	if err := s.commitLeaseToken(ctx, ticketID, tokenTemp, held.Generation(), hash); err != nil {
		return result, err
	}
	if err := s.journalEvent(ctx, s.projectID, result.Event.Seq); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) writeLeaseTokenTemp(ticketID, token string) (string, error) {
	path := s.leaseTokenPath(ticketID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return "", err
	}
	tmp := file.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	removeTemp = false
	return tmp, nil
}

func (s *Store) commitLeaseToken(ctx context.Context, ticketID, tempPath string, generation uint64, hash [32]byte) error {
	path := s.leaseTokenPath(ticketID)
	lease, err := s.GetLease(ctx, ticketID)
	if err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	held, ok := lease.Held()
	if !ok || held.Generation() != generation || held.HolderTokenHash() != hash {
		_ = os.Remove(tempPath)
		return ErrLeaseHeld
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func (s *Store) Release(ctx context.Context, ticketID, token string) (EventKey, error) {
	if err := domain.ValidateID(ticketID); err != nil {
		return EventKey{}, err
	}
	if err := s.ticketExists(ticketID); err != nil {
		return EventKey{}, err
	}
	if token == "" {
		return EventKey{}, ErrLeaseToken
	}
	var tokenBytes []byte
	var err error
	tokenBytes, err = base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return EventKey{}, ErrLeaseToken
	}
	hash := sha256.Sum256(tokenBytes)
	var event EventKey
	var releasedGeneration uint64
	tokenLock, err := acquireLock(s.leaseTokenLock(ticketID))
	if err != nil {
		return EventKey{}, err
	}
	defer unlockFile(tokenLock)
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		bootID, monoNS, err := s.sampleClock()
		if err != nil {
			return err
		}
		row, err := readLeaseRow(ctx, conn, s.projectID, ticketID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseExpired
		}
		if err != nil {
			return err
		}
		lease, err := leaseFromRow(ticketID, row)
		if err != nil {
			return err
		}
		held, ok := lease.Held()
		if !ok || !held.IsLive(bootID, monoNS) {
			return ErrLeaseExpired
		}
		if held.HolderTokenHash() != hash {
			return ErrLeaseToken
		}
		generation := held.Generation() + 1
		releasedGeneration = generation
		sqlResult, err := conn.ExecContext(ctx, `UPDATE leases SET state='free', generation=?, holder_token_hash=NULL, boot_id=NULL,
                last_heartbeat_mono_ns=NULL, ttl_ns=NULL, actor=NULL, worktree_id=NULL
                WHERE project_id=? AND ticket_id=? AND state='held' AND generation=? AND holder_token_hash=? AND boot_id=?
				AND ? >= last_heartbeat_mono_ns AND ? - last_heartbeat_mono_ns < ttl_ns`,
			int64(generation), s.projectID, ticketID, row.generation, base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS), int64(monoNS))
		if err != nil {
			return err
		}
		if affected, err := sqlResult.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return err
			}
			return ErrLeaseExpired
		}
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		if err := insertEventActor(ctx, conn, s.projectID, seq, held.Actor(), "lease.release", ticketID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
                precondition_digest, intended_digest, intended_bytes, materialised, allocation_id)
                VALUES(?, ?, ?, '', 'lease.release', '', '', NULL, 1, '')`, s.projectID, seq, s.worktreeID); err != nil {
			return err
		}
		event = EventKey{ProjectID: s.projectID, Seq: seq}
		return nil
	})
	if err != nil {
		return EventKey{}, err
	}
	if err := s.journalEvent(ctx, s.projectID, event.Seq); err != nil {
		return EventKey{}, err
	}
	current, readErr := s.GetLease(ctx, ticketID)
	if readErr == nil {
		if free, ok := current.Free(); ok && free.Generation == releasedGeneration {
			if data, fileErr := os.ReadFile(s.leaseTokenPath(ticketID)); fileErr == nil && strings.TrimSpace(string(data)) == token {
				_ = os.Remove(s.leaseTokenPath(ticketID))
			}
		}
	}
	return event, nil
}

func (s *Store) Heartbeat(ctx context.Context, ticketID, token string) (domain.Lease, error) {
	if err := domain.ValidateID(ticketID); err != nil {
		return domain.Lease{}, err
	}
	if err := s.ticketExists(ticketID); err != nil {
		return domain.Lease{}, err
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return domain.Lease{}, ErrLeaseToken
	}
	hash := sha256.Sum256(tokenBytes)
	var result domain.Lease
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		bootID, monoNS, err := s.sampleClock()
		if err != nil {
			return err
		}
		row, err := readLeaseRow(ctx, conn, s.projectID, ticketID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseExpired
		}
		if err != nil {
			return err
		}
		lease, err := leaseFromRow(ticketID, row)
		if err != nil {
			return err
		}
		held, ok := lease.Held()
		if !ok || !held.IsLive(bootID, monoNS) {
			return ErrLeaseExpired
		}
		if held.HolderTokenHash() != hash {
			return ErrLeaseToken
		}
		sqlResult, err := conn.ExecContext(ctx, `UPDATE leases SET last_heartbeat_mono_ns=?
                WHERE project_id=? AND ticket_id=? AND state='held' AND generation=? AND holder_token_hash=? AND boot_id=?
				AND ? >= last_heartbeat_mono_ns AND ? - last_heartbeat_mono_ns < ttl_ns`, int64(monoNS), s.projectID, ticketID, row.generation,
			base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS), int64(monoNS))
		if err != nil {
			return err
		}
		if affected, err := sqlResult.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return err
			}
			return ErrLeaseExpired
		}
		heldHash := held.HolderTokenHash()
		held, err = domain.NewHeldLease(heldHash[:], held.BootID(), monoNS, int64(held.TTLNS()), held.Generation(), held.Actor(), held.Worktree())
		if err != nil {
			return err
		}
		result, err = domain.NewLease(ticketID, held)
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) GetLease(ctx context.Context, ticketID string) (domain.Lease, error) {
	row, err := readLeaseRow(ctx, s.db, s.projectID, ticketID)
	if errors.Is(err, sql.ErrNoRows) {
		free, freeErr := domain.NewFreeLease(0)
		if freeErr != nil {
			return domain.Lease{}, freeErr
		}
		return domain.NewLease(ticketID, free)
	}
	if err != nil {
		return domain.Lease{}, err
	}
	return leaseFromRow(ticketID, row)
}

// ListLeases returns every held row, including expired rows that have not yet
// been reaped. Liveness and age notes are stamped from one monotonic sample.
func (s *Store) ListLeases(ctx context.Context) ([]HeldLeaseRow, error) {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ticket_id, actor, worktree_id, generation,
		boot_id, last_heartbeat_mono_ns, ttl_ns
		FROM leases WHERE project_id=? AND state='held' ORDER BY ticket_id`, s.projectID)
	if err != nil {
		return nil, err
	}

	result := make([]HeldLeaseRow, 0)
	for rows.Next() {
		var row HeldLeaseRow
		var rowBootID string
		if err := rows.Scan(&row.TicketID, &row.Actor, &row.WorktreeID, &row.Generation,
			&rowBootID, &row.LastHeartbeatMonoNanos, &row.TTLNanos); err != nil {
			_ = rows.Close()
			return nil, err
		}
		last := uint64(row.LastHeartbeatMonoNanos)
		ttl := uint64(row.TTLNanos)
		switch {
		case rowBootID != bootID:
			row.Expired = true
			row.AgeNote = "stale (prior boot)"
		case last > monoNS:
			row.Expired = false
			row.AgeNote = "concurrently renewed"
		default:
			age := monoNS - last
			row.Expired = age >= ttl
			row.AgeNote = time.Duration(age).String()
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReapExpiredLeases frees only leases that are positively expired at the one
// clock sample taken for this sweep. Detection is advisory; every free repeats
// the exact expiry predicate under BEGIN IMMEDIATE and the detected generation.
func (s *Store) ReapExpiredLeases(ctx context.Context) (int, error) {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return 0, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT ticket_id, generation, boot_id, last_heartbeat_mono_ns, ttl_ns
		FROM leases WHERE project_id=? AND state='held'`, s.projectID)
	if err != nil {
		return 0, err
	}
	var candidates []reapCandidate
	for rows.Next() {
		var ticketID, rowBootID string
		var generation, lastHeartbeatMonoNS, ttlNS int64
		if err := rows.Scan(&ticketID, &generation, &rowBootID, &lastHeartbeatMonoNS, &ttlNS); err != nil {
			_ = rows.Close()
			return 0, err
		}
		last := uint64(lastHeartbeatMonoNS)
		ttl := uint64(ttlNS)
		if rowBootID != bootID || (monoNS >= last && monoNS-last >= ttl) {
			candidates = append(candidates, reapCandidate{ticketID: ticketID, generation: generation})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	reaped := 0
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return reaped, err
		}
		if s.beforeReapCAS != nil {
			s.beforeReapCAS(candidate.ticketID)
		}
		changed := false
		// Compute the successor generation in Go and bind it, matching Claim /
		// Release. The generation=? guard fixes the row's value to
		// candidate.generation, so this is exactly the SQL generation+1 — but
		// done in Go it avoids SQLite promoting an int64 overflow to REAL (which
		// the generation>=0 CHECK would silently accept, corrupting the row).
		nextGeneration := candidate.generation + 1
		err := s.withImmediate(ctx, func(conn *sql.Conn) error {
			result, err := conn.ExecContext(ctx, `UPDATE leases SET state='free', generation=?,
				holder_token_hash=NULL, boot_id=NULL, last_heartbeat_mono_ns=NULL,
				ttl_ns=NULL, actor=NULL, worktree_id=NULL
				WHERE project_id=? AND ticket_id=? AND state='held' AND generation=?
				AND (boot_id<>? OR (?>=last_heartbeat_mono_ns AND ?-last_heartbeat_mono_ns>=ttl_ns))`,
				nextGeneration, s.projectID, candidate.ticketID, candidate.generation, bootID, int64(monoNS), int64(monoNS))
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				return nil
			}
			if affected != 1 {
				return fmt.Errorf("E_INTERNAL: reap changed %d leases", affected)
			}
			seq, err := nextSequence(ctx, conn, s.projectID)
			if err != nil {
				return err
			}
			if err := insertEventActor(ctx, conn, s.projectID, seq, "aira-daemon", "lease.lapse", candidate.ticketID); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
				precondition_digest, intended_digest, intended_bytes, materialised, allocation_id)
				VALUES(?, ?, '', '', 'lease.lapse', '', '', NULL, 1, '')`, s.projectID, seq); err != nil {
				return err
			}
			changed = true
			return nil
		})
		if err != nil {
			return reaped, err
		}
		if changed {
			reaped++
		}
	}
	return reaped, nil
}
