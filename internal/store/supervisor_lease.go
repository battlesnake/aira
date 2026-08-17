package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var ErrSupervisorLeaseConflict = errors.New("E_RUN_SUPERVISOR_LEASE_CONFLICT")

type SupervisorLeaseOutcome string

const (
	SupervisorLeaseClaimed  SupervisorLeaseOutcome = "claimed"
	SupervisorLeaseExisting SupervisorLeaseOutcome = "existing"
	SupervisorLeaseOK       SupervisorLeaseOutcome = "ok"
	SupervisorLeaseExpired  SupervisorLeaseOutcome = "expired"
	SupervisorLeaseFenced   SupervisorLeaseOutcome = "fenced"
	SupervisorLeaseToken    SupervisorLeaseOutcome = "token"
	SupervisorLeaseAbsent   SupervisorLeaseOutcome = "absent"
)

type SupervisorLeaseState string

const (
	SupervisorLeaseHeld   SupervisorLeaseState = "held"
	SupervisorLeaseLapsed SupervisorLeaseState = "lapsed"
	SupervisorLeaseNone   SupervisorLeaseState = "absent"
)

// SupervisorLease is the fresh reader projection. PID identity is evidence,
// never an ownership proof; only the capability hash authorises mutations.
type SupervisorLease struct {
	RunID               string
	State               SupervisorLeaseState
	Generation          int64
	HolderTokenHash     string
	HolderPID           int
	HolderStartTick     uint64
	HolderBootID        string
	LastHeartbeatMonoNS uint64
	TTLNS               uint64
	Actor               string
	WorktreeID          string
}

func (l SupervisorLease) IsLive(bootID string, monoNS uint64) bool {
	return l.State == SupervisorLeaseHeld && l.HolderBootID == bootID && monoNS >= l.LastHeartbeatMonoNS && monoNS-l.LastHeartbeatMonoNS < l.TTLNS
}

type supervisorLeaseRow struct {
	state               string
	generation          int64
	tokenHash           string
	pid                 int64
	startTick           int64
	bootID              string
	lastHeartbeatMonoNS int64
	ttlNS               int64
	actor               string
	worktreeID          string
}

type supervisorReapCandidate struct {
	runID      string
	generation int64
}

func validateSupervisorRunID(runID string) error {
	if !strings.HasPrefix(runID, "RUN-") || len(runID) == len("RUN-") {
		return errors.New("E_RUN_ARGUMENT_INVALID: invalid run id")
	}
	if _, err := strconv.ParseUint(strings.TrimPrefix(runID, "RUN-"), 10, 64); err != nil {
		return errors.New("E_RUN_ARGUMENT_INVALID: invalid run id")
	}
	return nil
}

func validateSupervisorTokenHash(tokenHash string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(tokenHash)
	if err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != tokenHash {
		return errors.New("E_RUN_SUPERVISOR_LEASE_INVALID: invalid capability hash")
	}
	return nil
}

func supervisorTokenHash(token string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return "", nil
	}
	hash := sha256.Sum256(decoded)
	return base64.RawURLEncoding.EncodeToString(hash[:]), nil
}

func readSupervisorLeaseRow(ctx context.Context, conn interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID, runID string) (supervisorLeaseRow, error) {
	var row supervisorLeaseRow
	err := conn.QueryRowContext(ctx, `SELECT state, generation, holder_token_hash, holder_pid,
		holder_start_tick, holder_boot_id, last_heartbeat_mono_ns, ttl_ns, actor, worktree_id
		FROM supervisor_leases WHERE project_id=? AND run_id=?`, projectID, runID).Scan(
		&row.state, &row.generation, &row.tokenHash, &row.pid, &row.startTick, &row.bootID,
		&row.lastHeartbeatMonoNS, &row.ttlNS, &row.actor, &row.worktreeID)
	return row, err
}

func supervisorLeaseFromRow(runID string, row supervisorLeaseRow) (SupervisorLease, error) {
	if (row.state != string(SupervisorLeaseHeld) && row.state != string(SupervisorLeaseLapsed)) || row.generation < 1 ||
		row.pid <= 0 || row.pid > math.MaxInt32 || row.startTick <= 0 || strings.TrimSpace(row.bootID) == "" ||
		row.lastHeartbeatMonoNS < 0 || row.ttlNS <= 0 || strings.TrimSpace(row.actor) == "" || strings.TrimSpace(row.worktreeID) == "" {
		return SupervisorLease{}, errors.New("E_CONFIG_INVALID: malformed supervisor lease")
	}
	if err := validateSupervisorTokenHash(row.tokenHash); err != nil {
		return SupervisorLease{}, errors.New("E_CONFIG_INVALID: malformed supervisor lease token hash")
	}
	return SupervisorLease{
		RunID: runID, State: SupervisorLeaseState(row.state), Generation: row.generation,
		HolderTokenHash: row.tokenHash, HolderPID: int(row.pid), HolderStartTick: uint64(row.startTick), HolderBootID: row.bootID,
		LastHeartbeatMonoNS: uint64(row.lastHeartbeatMonoNS), TTLNS: uint64(row.ttlNS), Actor: row.actor, WorktreeID: row.worktreeID,
	}, nil
}

func insertSupervisorLeaseEvent(ctx context.Context, conn *sql.Conn, projectID, worktreeID, actor, verb, runID string) error {
	seq, err := nextSequence(ctx, conn, projectID)
	if err != nil {
		return err
	}
	if err := insertEventActor(ctx, conn, projectID, seq, actor, verb, runID); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO outbox(project_id, seq, worktree_id, path, verb,
		precondition_digest, intended_digest, intended_bytes, materialised, allocation_id)
		VALUES(?, ?, ?, '', ?, '', '', NULL, 1, '')`, projectID, seq, worktreeID, verb)
	return err
}

// ClaimSupervisorLease installs a held lease or recovers an ambiguous committed
// claim idempotently. The clock is sampled only after BEGIN IMMEDIATE owns the
// writer lock.
func (s *Store) ClaimSupervisorLease(ctx context.Context, runID string, pid int, startTick uint64, bootID, tokenHash string, ttlNS uint64) (int64, SupervisorLeaseOutcome, error) {
	if err := validateSupervisorRunID(runID); err != nil {
		return 0, "", err
	}
	if pid <= 0 || startTick == 0 || startTick > maxInt64Uint || strings.TrimSpace(bootID) == "" || ttlNS == 0 || ttlNS > maxInt64Uint {
		return 0, "", errors.New("E_RUN_SUPERVISOR_LEASE_INVALID: invalid lease claim")
	}
	if err := validateSupervisorTokenHash(tokenHash); err != nil {
		return 0, "", err
	}
	var generation int64
	outcome := SupervisorLeaseClaimed
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		if s.afterSupervisorLeaseBegin != nil {
			s.afterSupervisorLeaseBegin()
		}
		sampleBootID, monoNS, err := s.sampleClock()
		if err != nil {
			return err
		}
		if sampleBootID != bootID {
			return errors.New("E_RUN_SUPERVISOR_LEASE_INVALID: holder boot id is not current")
		}
		row, rowErr := readSupervisorLeaseRow(ctx, conn, s.projectID, runID)
		if errors.Is(rowErr, sql.ErrNoRows) {
			generation = 1
			if _, err := conn.ExecContext(ctx, `INSERT INTO supervisor_leases(project_id, run_id, state, generation,
				holder_token_hash, holder_pid, holder_start_tick, holder_boot_id, last_heartbeat_mono_ns, ttl_ns, actor, worktree_id)
				VALUES(?, ?, 'held', ?, ?, ?, ?, ?, ?, ?, 'aira-supervisor', ?)`, s.projectID, runID, generation,
				tokenHash, pid, int64(startTick), bootID, int64(monoNS), int64(ttlNS), s.worktreeID); err != nil {
				return err
			}
			return insertSupervisorLeaseEvent(ctx, conn, s.projectID, s.worktreeID, "aira-supervisor", "lease.claim", runID)
		}
		if rowErr != nil {
			return rowErr
		}
		current, err := supervisorLeaseFromRow(runID, row)
		if err != nil {
			return err
		}
		if current.IsLive(sampleBootID, monoNS) {
			if current.HolderPID == pid && current.HolderStartTick == startTick && current.HolderBootID == bootID && current.HolderTokenHash == tokenHash {
				generation, outcome = current.Generation, SupervisorLeaseExisting
				return nil
			}
			return ErrSupervisorLeaseConflict
		}
		positivelyExpired := current.State == SupervisorLeaseLapsed || current.HolderBootID != sampleBootID ||
			(monoNS >= current.LastHeartbeatMonoNS && monoNS-current.LastHeartbeatMonoNS >= current.TTLNS)
		if !positivelyExpired {
			return ErrClockUnavailable
		}
		if row.generation == math.MaxInt64 {
			return errors.New("E_CONFIG_INVALID: supervisor lease generation overflow")
		}
		generation = row.generation + 1
		result, err := conn.ExecContext(ctx, `UPDATE supervisor_leases SET state='held', generation=?, holder_token_hash=?,
			holder_pid=?, holder_start_tick=?, holder_boot_id=?, last_heartbeat_mono_ns=?, ttl_ns=?, actor='aira-supervisor', worktree_id=?
			WHERE project_id=? AND run_id=? AND generation=? AND ((state='lapsed') OR
			(state='held' AND (holder_boot_id<>? OR (?>=last_heartbeat_mono_ns AND ?-last_heartbeat_mono_ns>=ttl_ns))))`,
			generation, tokenHash, pid, int64(startTick), bootID, int64(monoNS), int64(ttlNS), s.worktreeID,
			s.projectID, runID, row.generation, sampleBootID, int64(monoNS), int64(monoNS))
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrSupervisorLeaseConflict
		}
		return insertSupervisorLeaseEvent(ctx, conn, s.projectID, s.worktreeID, "aira-supervisor", "lease.claim", runID)
	})
	if err != nil {
		return 0, "", err
	}
	return generation, outcome, nil
}

func (s *Store) RenewSupervisorLease(ctx context.Context, runID string, generation int64, token string) (SupervisorLeaseOutcome, error) {
	if err := validateSupervisorRunID(runID); err != nil {
		return "", err
	}
	tokenHash, err := supervisorTokenHash(token)
	if err != nil {
		return "", err
	}
	if tokenHash == "" {
		return SupervisorLeaseToken, nil
	}
	var outcome SupervisorLeaseOutcome
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		if s.afterSupervisorLeaseBegin != nil {
			s.afterSupervisorLeaseBegin()
		}
		bootID, monoNS, err := s.sampleClock()
		if err != nil {
			return err
		}
		row, err := readSupervisorLeaseRow(ctx, conn, s.projectID, runID)
		if errors.Is(err, sql.ErrNoRows) {
			outcome = SupervisorLeaseAbsent
			return nil
		}
		if err != nil {
			return err
		}
		lease, err := supervisorLeaseFromRow(runID, row)
		if err != nil {
			return err
		}
		switch {
		case lease.Generation != generation:
			outcome = SupervisorLeaseFenced
			return nil
		case lease.State != SupervisorLeaseHeld:
			outcome = SupervisorLeaseExpired
			return nil
		case lease.HolderTokenHash != tokenHash:
			outcome = SupervisorLeaseToken
			return nil
		}
		result, err := conn.ExecContext(ctx, `UPDATE supervisor_leases SET last_heartbeat_mono_ns=?
			WHERE project_id=? AND run_id=? AND state='held' AND generation=? AND holder_token_hash=? AND holder_boot_id=?
			AND ?>=last_heartbeat_mono_ns AND ?-last_heartbeat_mono_ns<ttl_ns`, int64(monoNS), s.projectID, runID,
			generation, tokenHash, bootID, int64(monoNS), int64(monoNS))
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			outcome = SupervisorLeaseExpired
			return nil
		}
		if affected != 1 {
			return fmt.Errorf("E_INTERNAL: renew changed %d supervisor leases", affected)
		}
		outcome = SupervisorLeaseOK
		return nil
	})
	return outcome, err
}

func (s *Store) ReleaseSupervisorLease(ctx context.Context, runID string, generation int64, token string) (SupervisorLeaseOutcome, error) {
	if err := validateSupervisorRunID(runID); err != nil {
		return "", err
	}
	tokenHash, err := supervisorTokenHash(token)
	if err != nil {
		return "", err
	}
	if tokenHash == "" {
		return SupervisorLeaseToken, nil
	}
	var outcome SupervisorLeaseOutcome
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		if s.afterSupervisorLeaseBegin != nil {
			s.afterSupervisorLeaseBegin()
		}
		bootID, monoNS, err := s.sampleClock()
		if err != nil {
			return err
		}
		row, err := readSupervisorLeaseRow(ctx, conn, s.projectID, runID)
		if errors.Is(err, sql.ErrNoRows) {
			outcome = SupervisorLeaseAbsent
			return nil
		}
		if err != nil {
			return err
		}
		lease, err := supervisorLeaseFromRow(runID, row)
		if err != nil {
			return err
		}
		switch {
		case lease.Generation != generation:
			outcome = SupervisorLeaseFenced
			return nil
		case lease.State != SupervisorLeaseHeld:
			outcome = SupervisorLeaseAbsent
			return nil
		case lease.HolderTokenHash != tokenHash:
			outcome = SupervisorLeaseToken
			return nil
		case !lease.IsLive(bootID, monoNS):
			outcome = SupervisorLeaseAbsent
			return nil
		case generation == math.MaxInt64:
			return errors.New("E_CONFIG_INVALID: supervisor lease generation overflow")
		}
		nextGeneration := generation + 1
		result, err := conn.ExecContext(ctx, `UPDATE supervisor_leases SET state='lapsed', generation=?
			WHERE project_id=? AND run_id=? AND state='held' AND generation=? AND holder_token_hash=? AND holder_boot_id=?
			AND ?>=last_heartbeat_mono_ns AND ?-last_heartbeat_mono_ns<ttl_ns`, nextGeneration, s.projectID, runID,
			generation, tokenHash, bootID, int64(monoNS), int64(monoNS))
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			outcome = SupervisorLeaseAbsent
			return nil
		}
		if affected != 1 {
			return fmt.Errorf("E_INTERNAL: release changed %d supervisor leases", affected)
		}
		if err := insertSupervisorLeaseEvent(ctx, conn, s.projectID, s.worktreeID, "aira-supervisor", "lease.release", runID); err != nil {
			return err
		}
		outcome = SupervisorLeaseOK
		return nil
	})
	return outcome, err
}

func (s *Store) GetSupervisorLease(ctx context.Context, runID string) (SupervisorLease, error) {
	if err := validateSupervisorRunID(runID); err != nil {
		return SupervisorLease{}, err
	}
	row, err := readSupervisorLeaseRow(ctx, s.db, s.projectID, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return SupervisorLease{RunID: runID, State: SupervisorLeaseNone}, nil
	}
	if err != nil {
		return SupervisorLease{}, err
	}
	return supervisorLeaseFromRow(runID, row)
}

// SupervisorLeaseLive is the narrow runner reader seam: it performs a fresh
// row read and treats clock or row faults as errors, never as absence/death.
func (s *Store) SupervisorLeaseLive(ctx context.Context, runID string) (bool, error) {
	lease, err := s.GetSupervisorLease(ctx, runID)
	if err != nil || lease.State != SupervisorLeaseHeld {
		return false, err
	}
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return false, err
	}
	return lease.IsLive(bootID, monoNS), nil
}

// ReapExpiredSupervisorLeases uses one sweep sample and repeats the exact
// positive-expiry predicate under a generation-guarded BEGIN IMMEDIATE CAS.
func (s *Store) ReapExpiredSupervisorLeases(ctx context.Context) (int, error) {
	bootID, monoNS, err := s.sampleClock()
	if err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, generation, holder_boot_id, last_heartbeat_mono_ns, ttl_ns
		FROM supervisor_leases WHERE project_id=? AND state='held'`, s.projectID)
	if err != nil {
		return 0, err
	}
	var candidates []supervisorReapCandidate
	for rows.Next() {
		var runID, rowBootID string
		var generation, last, ttl int64
		if err := rows.Scan(&runID, &generation, &rowBootID, &last, &ttl); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if generation < 1 || last < 0 || ttl <= 0 {
			_ = rows.Close()
			return 0, errors.New("E_CONFIG_INVALID: malformed supervisor lease")
		}
		if rowBootID != bootID || (monoNS >= uint64(last) && monoNS-uint64(last) >= uint64(ttl)) {
			candidates = append(candidates, supervisorReapCandidate{runID: runID, generation: generation})
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
		if s.beforeSupervisorReapCAS != nil {
			s.beforeSupervisorReapCAS(candidate.runID)
		}
		if candidate.generation == math.MaxInt64 {
			return reaped, errors.New("E_CONFIG_INVALID: supervisor lease generation overflow")
		}
		changed := false
		nextGeneration := candidate.generation + 1
		err := s.withImmediate(ctx, func(conn *sql.Conn) error {
			if s.afterSupervisorReapBegin != nil {
				s.afterSupervisorReapBegin()
			}
			// Re-sample the clock AFTER BEGIN IMMEDIATE owns the writer lock (Sol
			// build r1 P1): the sweep sample chose the candidate, but the reaping
			// CAS must decide expiry against a clock taken under the lock, so a
			// renew that landed after the sweep is authoritatively current here.
			casBootID, casMonoNS, err := s.sampleClock()
			if err != nil {
				return err
			}
			result, err := conn.ExecContext(ctx, `UPDATE supervisor_leases SET state='lapsed', generation=?
				WHERE project_id=? AND run_id=? AND state='held' AND generation=? AND
				(holder_boot_id<>? OR (?>=last_heartbeat_mono_ns AND ?-last_heartbeat_mono_ns>=ttl_ns))`,
				nextGeneration, s.projectID, candidate.runID, candidate.generation, casBootID, int64(casMonoNS), int64(casMonoNS))
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
				return fmt.Errorf("E_INTERNAL: reap changed %d supervisor leases", affected)
			}
			if err := insertSupervisorLeaseEvent(ctx, conn, s.projectID, "", "aira-daemon", "lease.lapse", candidate.runID); err != nil {
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
