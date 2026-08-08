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

	"aira/internal/domain"
	"golang.org/x/sys/unix"
)

const (
	defaultLeaseTTLNS = uint64(15 * 60 * 1000 * 1000 * 1000)
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
		return domain.Lease{TicketID: ticketID, State: domain.FreeLease{Generation: uint64(row.generation)}}, nil
	}
	if row.state != "held" || !row.holderTokenHash.Valid || !row.bootID.Valid || !row.lastHeartbeatMonoNS.Valid || !row.ttlNS.Valid || !row.actor.Valid || !row.worktree.Valid {
		return domain.Lease{}, errors.New("E_CONFIG_INVALID: malformed held lease")
	}
	hashBytes, err := base64.RawURLEncoding.DecodeString(row.holderTokenHash.String)
	if err != nil || len(hashBytes) != sha256.Size {
		return domain.Lease{}, errors.New("E_CONFIG_INVALID: malformed lease token hash")
	}
	var hash [32]byte
	copy(hash[:], hashBytes)
	return domain.Lease{TicketID: ticketID, State: domain.HeldLease{
		HolderTokenHash: hash, BootID: row.bootID.String, LastHeartbeatMonoNS: uint64(row.lastHeartbeatMonoNS.Int64),
		TTLNS: uint64(row.ttlNS.Int64), Generation: uint64(row.generation), Actor: row.actor.String, Worktree: row.worktree.String,
	}}, nil
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
	if _, err := s.Get(ticketID); err != nil {
		return LeaseClaim{}, err
	}
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return LeaseClaim{}, err
	}
	if actor == "" {
		actor = "aira"
	}
	clear, hash, err := leaseToken()
	if err != nil {
		return LeaseClaim{}, err
	}
	var result LeaseClaim
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		row, rowErr := readLeaseRow(ctx, conn, s.projectID, ticketID)
		if errors.Is(rowErr, sql.ErrNoRows) {
			if _, err := conn.ExecContext(ctx, `INSERT INTO leases(project_id, ticket_id, state, generation,
                    holder_token_hash, boot_id, last_heartbeat_mono_ns, ttl_ns, actor, worktree_id)
                    VALUES(?, ?, 'held', 1, ?, ?, ?, ?, ?, ?)`, s.projectID, ticketID,
				base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS), int64(s.leaseTTLNS), actor, s.worktreeID); err != nil {
				return err
			}
			result.Lease = domain.Lease{TicketID: ticketID, State: domain.HeldLease{HolderTokenHash: hash,
				BootID: bootID, LastHeartbeatMonoNS: monoNS, TTLNS: s.leaseTTLNS, Generation: 1, Actor: actor, Worktree: s.worktreeID}}
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
			generation := currentHeld.Generation + 1
			if !isHeld {
				free, _ := current.Free()
				generation = free.Generation + 1
			}
			result.Stolen = isHeld
			var updateResult sql.Result
			if isHeld {
				updateResult, err = conn.ExecContext(ctx, `UPDATE leases SET state='held', generation=?, holder_token_hash=?, boot_id=?,
                    last_heartbeat_mono_ns=?, ttl_ns=?, actor=?, worktree_id=? WHERE project_id=? AND ticket_id=? AND state='held' AND generation=?
                    AND (boot_id<>? OR ? >= last_heartbeat_mono_ns + ttl_ns)`,
					int64(generation), base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS), int64(s.leaseTTLNS), actor,
					s.worktreeID, s.projectID, ticketID, row.generation, bootID, int64(monoNS))
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
			result.Lease = domain.Lease{TicketID: ticketID, State: domain.HeldLease{HolderTokenHash: hash,
				BootID: bootID, LastHeartbeatMonoNS: monoNS, TTLNS: s.leaseTTLNS, Generation: generation, Actor: actor, Worktree: s.worktreeID}}
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
		return nil
	})
	if err != nil {
		return LeaseClaim{}, err
	}
	result.Token = clear
	if err := s.saveLeaseToken(ticketID, clear); err != nil {
		return LeaseClaim{}, err
	}
	if err := s.journalEvent(ctx, s.projectID, result.Event.Seq); err != nil {
		return LeaseClaim{}, err
	}
	return result, nil
}

func (s *Store) saveLeaseToken(ticketID, token string) error {
	path := s.leaseTokenPath(ticketID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) Release(ctx context.Context, ticketID, token string) (EventKey, error) {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return EventKey{}, err
	}
	hash := sha256.Sum256([]byte{})
	if token != "" {
		var tokenBytes []byte
		tokenBytes, err = base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(tokenBytes) != 32 {
			return EventKey{}, ErrLeaseToken
		}
		hash = sha256.Sum256(tokenBytes)
	}
	var event EventKey
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
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
		if held.HolderTokenHash != hash {
			return ErrLeaseToken
		}
		generation := held.Generation + 1
		sqlResult, err := conn.ExecContext(ctx, `UPDATE leases SET state='free', generation=?, holder_token_hash=NULL, boot_id=NULL,
                last_heartbeat_mono_ns=NULL, ttl_ns=NULL, actor=NULL, worktree_id=NULL
                WHERE project_id=? AND ticket_id=? AND state='held' AND generation=? AND holder_token_hash=? AND boot_id=?
                AND ? < last_heartbeat_mono_ns + ttl_ns`,
			int64(generation), s.projectID, ticketID, row.generation, base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS))
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
		if err := insertEventActor(ctx, conn, s.projectID, seq, held.Actor, "lease.release", ticketID); err != nil {
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
	if current, readErr := s.LeaseToken(ticketID); readErr == nil && current == token {
		_ = os.Remove(s.leaseTokenPath(ticketID))
	}
	return event, nil
}

func (s *Store) Heartbeat(ctx context.Context, ticketID, token string) (domain.Lease, error) {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return domain.Lease{}, err
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return domain.Lease{}, ErrLeaseToken
	}
	hash := sha256.Sum256(tokenBytes)
	var result domain.Lease
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
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
		if held.HolderTokenHash != hash {
			return ErrLeaseToken
		}
		sqlResult, err := conn.ExecContext(ctx, `UPDATE leases SET last_heartbeat_mono_ns=?
                WHERE project_id=? AND ticket_id=? AND state='held' AND generation=? AND holder_token_hash=? AND boot_id=?
                AND ? < last_heartbeat_mono_ns + ttl_ns`, int64(monoNS), s.projectID, ticketID, row.generation,
			base64.RawURLEncoding.EncodeToString(hash[:]), bootID, int64(monoNS))
		if err != nil {
			return err
		}
		if affected, err := sqlResult.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return err
			}
			return ErrLeaseExpired
		}
		held.LastHeartbeatMonoNS = monoNS
		result = domain.Lease{TicketID: ticketID, State: held}
		return nil
	})
	return result, err
}

func (s *Store) GetLease(ctx context.Context, ticketID string) (domain.Lease, error) {
	row, err := readLeaseRow(ctx, s.db, s.projectID, ticketID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Lease{TicketID: ticketID, State: domain.FreeLease{}}, nil
	}
	if err != nil {
		return domain.Lease{}, err
	}
	return leaseFromRow(ticketID, row)
}
