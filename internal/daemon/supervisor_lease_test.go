package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/store"
)

func daemonSupervisorToken(seed byte) (string, string) {
	clear := make([]byte, 32)
	for i := range clear {
		clear[i] = seed
	}
	token := base64.RawURLEncoding.EncodeToString(clear)
	hash := sha256.Sum256(clear)
	return token, base64.RawURLEncoding.EncodeToString(hash[:])
}

func TestSupervisorLeaseSocketClaimRenewRelease(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "supervisor-lease")
	token, tokenHash := daemonSupervisorToken(1)
	call := func(verb string, args map[string]any, data any) ResponseFrame {
		t.Helper()
		response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
			Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: verb, Args: args},
		})
		if err != nil || !response.OK {
			t.Fatalf("%s response=%+v err=%v", verb, response, err)
		}
		if err := json.Unmarshal(response.Data, data); err != nil {
			t.Fatal(err)
		}
		return response
	}
	var claim struct {
		Generation int64  `json:"generation"`
		Outcome    string `json:"outcome"`
	}
	call("supervise-lease-claim", map[string]any{
		"run_id": "RUN-1", "pid": os.Getpid(), "start_tick": 1,
		"boot_id": currentBootID(), "ttl_ms": int64((60 * time.Second).Milliseconds()), "token_hash": tokenHash,
	}, &claim)
	if claim.Generation != 1 || claim.Outcome != "claimed" {
		t.Fatalf("claim=%+v", claim)
	}
	var mutation struct {
		Outcome string `json:"outcome"`
	}
	call("supervise-lease-renew", map[string]any{"run_id": "RUN-1", "generation": claim.Generation, "token": token}, &mutation)
	if mutation.Outcome != "ok" {
		t.Fatalf("renew=%+v", mutation)
	}
	call("supervise-lease-release", map[string]any{"run_id": "RUN-1", "generation": claim.Generation, "token": token}, &mutation)
	if mutation.Outcome != "ok" {
		t.Fatalf("release=%+v", mutation)
	}
}

func TestSupervisorLeasePeerCredentialRejects(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid() + 1, os.Getpid(), nil }
	scope := testScope(t, paths, "peer-reject")
	_, tokenHash := daemonSupervisorToken(2)
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), serverConn)
		close(done)
	}()
	if err := writeFrame(clientConn, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{
		Verb: "supervise-lease-claim", Args: map[string]any{"run_id": "RUN-2", "pid": os.Getpid(), "start_tick": 1, "boot_id": currentBootID(), "ttl_ms": int64(60000), "token_hash": tokenHash},
	}}); err != nil {
		t.Fatal(err)
	}
	var response ResponseFrame
	if err := readFrame(clientConn, &response); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.Close()
	<-done
	if response.OK || response.Code != CodeProtocol {
		t.Fatalf("peer rejection response=%+v", response)
	}
}

// TestSupervisorLeasePeerPIDMismatchRejected covers the peer_pid==holder_pid
// defence-in-depth (the UID-only test cannot catch its removal): a claim whose
// holder pid differs from the connecting peer is rejected with no row, and a
// current-generation renew from a different peer pid is rejected leaving the row
// unchanged (Sol build r1 P2).
func TestSupervisorLeasePeerPIDMismatchRejected(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	scope := testScope(t, paths, "peer-pid")
	view, _, err := server.storeForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	token, tokenHash := daemonSupervisorToken(4)
	exchange := func(peerPID int, req core.Request) ResponseFrame {
		t.Helper()
		server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid(), peerPID, nil }
		serverConn, clientConn := net.Pipe()
		done := make(chan struct{})
		go func() { server.serveConnection(context.Background(), serverConn); close(done) }()
		if err := writeFrame(clientConn, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: req}); err != nil {
			t.Fatal(err)
		}
		var response ResponseFrame
		if err := readFrame(clientConn, &response); err != nil {
			t.Fatal(err)
		}
		_ = clientConn.Close()
		<-done
		return response
	}
	claim := exchange(os.Getpid()+1, core.Request{Verb: "supervise-lease-claim", Args: map[string]any{
		"run_id": "RUN-1", "pid": os.Getpid(), "start_tick": 1, "boot_id": currentBootID(),
		"ttl_ms": int64(60000), "token_hash": tokenHash,
	}})
	if claim.OK || claim.Code != CodeProtocol {
		t.Fatalf("wrong-peer-pid claim response=%+v", claim)
	}
	if lease, err := view.GetSupervisorLease(context.Background(), "RUN-1"); err != nil || lease.State != store.SupervisorLeaseNone {
		t.Fatalf("rejected claim created a row: %+v err=%v", lease, err)
	}
	generation, _, err := view.ClaimSupervisorLease(context.Background(), "RUN-1", os.Getpid(), 1, currentBootID(), tokenHash, uint64(time.Minute.Milliseconds())*1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	before, err := view.GetSupervisorLease(context.Background(), "RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	renew := exchange(os.Getpid()+1, core.Request{Verb: "supervise-lease-renew", Args: map[string]any{
		"run_id": "RUN-1", "generation": generation, "token": token,
	}})
	if renew.OK || renew.Code != CodeProtocol {
		t.Fatalf("wrong-peer-pid renew response=%+v", renew)
	}
	after, err := view.GetSupervisorLease(context.Background(), "RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation || after.LastHeartbeatMonoNS != before.LastHeartbeatMonoNS || after.HolderPID != before.HolderPID {
		t.Fatalf("wrong-peer-pid renew mutated the lease: before=%+v after=%+v", before, after)
	}
}

func TestSupervisorLeaseDaemonReaperTimerLapsesExpired(t *testing.T) {
	t.Setenv("AIRA_DAEMON_REAP_INTERVAL", "5ms")
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "supervisor-reaper")
	view, _, err := server.storeForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	_, tokenHash := daemonSupervisorToken(3)
	if _, _, err := view.ClaimSupervisorLease(context.Background(), "RUN-3", os.Getpid(), 1, currentBootID(), tokenHash, uint64(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		lease, err := view.GetSupervisorLease(context.Background(), "RUN-3")
		if err != nil {
			t.Fatal(err)
		}
		if lease.State == store.SupervisorLeaseLapsed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon reaper did not lapse supervisor lease: %+v", lease)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
