package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/store"

	"golang.org/x/sys/unix"
)

func isSupervisorLeaseVerb(verb string) bool {
	switch verb {
	case "supervise-lease-claim", "supervise-lease-renew", "supervise-lease-release":
		return true
	default:
		return false
	}
}

func unixPeerCredential(conn net.Conn) (uid, pid int, err error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("peer is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		cred, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if controlErr != nil || cred == nil {
		return 0, 0, controlErr
	}
	return int(cred.Uid), int(cred.Pid), nil
}

func leaseProtocolError(message string) core.Response {
	return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": " + message, Exit: codes.ExitForCode(CodeProtocol)}
}

func leaseStoreError(err error) core.Response {
	code := store.ErrorCode(err)
	if code == "E_INTERNAL" {
		code = CodeInternal
	}
	return core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
}

func leaseIntArg(args map[string]any, name string) (int64, error) {
	value, ok := args[name]
	if !ok {
		return 0, fmt.Errorf("missing %s", name)
	}
	switch number := value.(type) {
	case json.Number:
		return number.Int64()
	case float64:
		if number != math.Trunc(number) || number < math.MinInt64 || number > math.MaxInt64 {
			return 0, fmt.Errorf("invalid %s", name)
		}
		return int64(number), nil
	case int:
		return int64(number), nil
	case int64:
		return number, nil
	case string:
		return strconv.ParseInt(number, 10, 64)
	default:
		return 0, fmt.Errorf("invalid %s", name)
	}
}

func leaseStringArg(args map[string]any, name string) (string, error) {
	value, ok := args[name].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("invalid %s", name)
	}
	return value, nil
}

// supervisorLeaseRequest takes conn ONLY to read its peer credentials
// (getsockopt SO_PEERCRED via unixPeerCredential); it never reads the socket.
// That matters since AIRA-84: serveConnection clears the handshake read
// deadline before dispatching here, so this connection has NO read deadline. A
// read added below without setting its own would block forever rather than
// inherit a stale one — flagged in build review as the likeliest place for that
// mistake, since this is the one post-handshake handler that is handed conn
// without owning a framed reader.
func (s *Server) supervisorLeaseRequest(ctx context.Context, conn net.Conn, scope WorktreeScope, verb string, args map[string]any) core.Response {
	credential := s.peerCredential
	if credential == nil {
		credential = unixPeerCredential
	}
	uid, peerPID, err := credential(conn)
	if err != nil || uid != os.Geteuid() || peerPID <= 0 {
		return leaseProtocolError("supervisor peer credentials rejected")
	}
	view, _, err := s.storeForScope(scope)
	if err != nil {
		return leaseStoreError(err)
	}
	runID, err := leaseStringArg(args, "run_id")
	if err != nil {
		return leaseProtocolError(err.Error())
	}
	switch verb {
	case "supervise-lease-claim":
		pid, pidErr := leaseIntArg(args, "pid")
		startTick, tickErr := leaseIntArg(args, "start_tick")
		ttlMS, ttlErr := leaseIntArg(args, "ttl_ms")
		bootID, bootErr := leaseStringArg(args, "boot_id")
		tokenHash, tokenErr := leaseStringArg(args, "token_hash")
		if pidErr != nil || tickErr != nil || ttlErr != nil || bootErr != nil || tokenErr != nil || pid <= 0 || pid > math.MaxInt32 || startTick <= 0 || ttlMS < 60_000 || ttlMS > math.MaxInt64/int64(1_000_000) {
			return leaseProtocolError("invalid supervisor lease claim")
		}
		if int(pid) != peerPID {
			return leaseProtocolError("claim holder pid does not match peer pid")
		}
		generation, outcome, claimErr := view.ClaimSupervisorLease(ctx, runID, int(pid), uint64(startTick), bootID, tokenHash, uint64(ttlMS)*1_000_000)
		if claimErr != nil {
			return leaseStoreError(claimErr)
		}
		return core.Response{OK: true, Code: "OK", Data: map[string]any{"generation": generation, "outcome": outcome}}
	case "supervise-lease-renew", "supervise-lease-release":
		generation, generationErr := leaseIntArg(args, "generation")
		token, tokenErr := leaseStringArg(args, "token")
		if generationErr != nil || tokenErr != nil || generation < 1 {
			return leaseProtocolError("invalid supervisor lease mutation")
		}
		lease, readErr := view.GetSupervisorLease(ctx, runID)
		if readErr != nil {
			return leaseStoreError(readErr)
		}
		if lease.State != store.SupervisorLeaseNone && lease.Generation == generation && lease.HolderPID != peerPID {
			return leaseProtocolError("lease holder pid does not match peer pid")
		}
		var outcome store.SupervisorLeaseOutcome
		if verb == "supervise-lease-renew" {
			outcome, err = view.RenewSupervisorLease(ctx, runID, generation, token)
		} else {
			outcome, err = view.ReleaseSupervisorLease(ctx, runID, generation, token)
		}
		if err != nil {
			return leaseStoreError(err)
		}
		return core.Response{OK: true, Code: "OK", Data: map[string]any{"outcome": outcome}}
	default:
		return leaseProtocolError("unknown supervisor lease verb")
	}
}
