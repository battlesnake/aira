package daemon

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/testdeadline"
)

// serveDumpTestServer builds a daemon from real paths with deterministic admission
// arithmetic — a resolvable slice, no confine scopes, no headroom — so a confine
// admit over the socket is granted immediately and the restart dump has a real,
// still-held lease to capture.
func serveDumpTestServer(t *testing.T) (*Server, Paths) {
	t.Helper()
	paths := testPaths(t)
	server := NewServer(paths)
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitConfineScan = noConfinesScan
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 40, 0, true, ""
	}
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitPollInterval = 5 * time.Millisecond
	return server, paths
}

// startServeInline runs Serve in a goroutine and waits for readiness. Unlike the
// shared startServer helper it does NOT register a t.Cleanup that consumes `done`
// — these tests cancel and read `done` themselves to observe the shutdown path, and
// a cleanup racing them for the channel would spuriously time out.
func startServeInline(t *testing.T, server *Server) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("daemon exited before ready: %v", err)
	case <-testdeadline.After(5 * time.Second):
		cancel()
		t.Fatal("daemon did not become ready")
	}
	return cancel, done
}

// dialHoldAdmit sends a confine admit over the socket, reads the grant, and
// RETURNS THE CONNECTION OPEN so the lease stays held (a real client holds it for
// the job's lifetime). Exchange is deliberately not used — it closes the conn,
// which is an immediate peer-EOF release.
func dialHoldAdmit(t *testing.T, socket string, args map[string]any) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frame := RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "admit", Args: args}}
	if err := writeFrame(conn, frame); err != nil {
		_ = conn.Close()
		t.Fatalf("write admit frame: %v", err)
	}
	var resp ResponseFrame
	if err := readFrame(conn, &resp); err != nil {
		_ = conn.Close()
		t.Fatalf("read grant frame: %v", err)
	}
	if !resp.OK {
		_ = conn.Close()
		t.Fatalf("admit refused: code=%s err=%s", resp.Code, resp.Error)
	}
	return conn
}

func awaitShutdown(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
		return nil
	}
}

// verifies: on graceful shutdown the daemon writes the restart dump while the lease
// is STILL HELD — proven by the dump containing the held lease with its real
// SO_PEERCRED pid — which is only true if the snapshot ran BEFORE close(stopping)
// (the thing that releases the lease and closes its connection, §14 P3 / §15 P2-A).
//
// MUTATION: move s.dumpLeasesForRestart() to after `case <-drained:` in Serve (i.e.
// after close(stopping) and the connection drain). The tracked socket lease has
// provably released by then, so the dump is empty and this test reds. (Placed
// immediately after close(stopping) it is racy-red; after the drain it is
// deterministic — see the BUILT record.)
func TestGracefulShutdownDumpsHeldLeaseBeforeClosingConnection(t *testing.T) {
	server, paths := serveDumpTestServer(t)
	cancel, done := startServeInline(t, server)

	conn := dialHoldAdmit(t, paths.SocketPath, e2eConfineAdmitArgs(2<<20, "CONFINE-dumptest-5101-aa", "dumptest"))
	defer conn.Close()
	// The lease must be in the ledger before we shut down.
	e2eWaitLedger(t, server, 1, 2<<20)

	cancel()
	if err := awaitShutdown(t, done); err != nil {
		t.Fatalf("shutdown returned %v", err)
	}

	f, err := os.Open(leaseDumpPath(paths))
	if err != nil {
		t.Fatalf("open dump: %v", err)
	}
	defer f.Close()
	dump, err := decodeLeaseDump(f)
	if err != nil {
		t.Fatalf("decode dump: %v", err)
	}
	if len(dump.Records) != 1 {
		t.Fatalf("dump has %d records, want exactly the one held lease: %+v", len(dump.Records), dump.Records)
	}
	rec := dump.Records[0]
	if rec.Frame.ScopeID != "CONFINE-dumptest-5101-aa@session-e2e" {
		t.Fatalf("dumped scope_id = %q, want the ledger key verbatim", rec.Frame.ScopeID)
	}
	if rec.Frame.RAMBytes != 2<<20 {
		t.Fatalf("dumped ram = %d, want %d", rec.Frame.RAMBytes, int64(2<<20))
	}
	// A real unix socket carries SO_PEERCRED, so the anchor path recorded THIS
	// process as the peer — a genuine non-zero pid + start-tick round-trip.
	if rec.ClientPID != os.Getpid() {
		t.Fatalf("dumped pid = %d, want this process pid %d (real SO_PEERCRED)", rec.ClientPID, os.Getpid())
	}
	if rec.ProcessStartTick == 0 {
		t.Fatal("dumped process-start-tick is 0 for a live, readable pid")
	}
}

// verifies: a dump WRITE error is swallowed (fail-open) and graceful shutdown still
// completes — no panic, no hang — and no dump file is left. The failure is injected
// at the REAL IO boundary (a read+execute-only RuntimeDir denies CreateTemp), not a
// stub. A stub that accepted any write would pass this test while masking a real
// durability assumption; this does not.
func TestGracefulShutdownDumpWriteErrorIsSwallowed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root: 0500 cannot deny the owner, so CreateTemp would not fail")
	}
	server, paths := serveDumpTestServer(t)
	cancel, done := startServeInline(t, server)

	conn := dialHoldAdmit(t, paths.SocketPath, e2eConfineAdmitArgs(2<<20, "CONFINE-failopen-5101-bb", "failopen"))
	defer conn.Close()
	e2eWaitLedger(t, server, 1, 2<<20)

	// Deny writes in RuntimeDir so the dump's CreateTemp fails with EACCES. Restored
	// before cleanup so t.TempDir can remove the tree. (Socket/lock removal in
	// shutdown are already best-effort and do not gate Serve's return.)
	if err := os.Chmod(paths.RuntimeDir, 0o500); err != nil {
		t.Fatalf("chmod runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(paths.RuntimeDir, 0o700) })

	cancel()
	// The assertion is simply that shutdown RETURNS: a swallowed dump error does not
	// hang or panic. Serve's own return value is unconstrained here.
	_ = awaitShutdown(t, done)

	if _, err := os.Stat(leaseDumpPath(paths)); !os.IsNotExist(err) {
		t.Fatalf("a dump file exists despite the injected write error (stat err=%v)", err)
	}
}
