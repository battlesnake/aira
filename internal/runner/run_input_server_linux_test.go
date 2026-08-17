//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRunInputClientDroppedBeforeFinalACKIsOutcomeUnknownWithoutRetry(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	appendRunEvent(t, r, "running", RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Owner: r.owner, Status: StatusRunning, StdinConnect: true, InputSocket: "/injected"})
	client, server := net.Pipe()
	dials := 0
	r.inputDialFn = func(context.Context, string) (net.Conn, error) {
		dials++
		return &closeAllWriteConn{Conn: client}, nil
	}
	received := make(chan []byte, 1)
	go func() {
		defer server.Close()
		_, _, _ = readRunInputFrame(server)
		_ = writeRunInputFrame(server, runInputOpAck, encodeRunInputAck(0))
		_, payload, _ := readRunInputFrame(server)
		received <- append([]byte(nil), payload...)
		_ = writeRunInputFrame(server, runInputOpAck, encodeRunInputAck(int64(len(payload))))
		_, _, _ = readRunInputFrame(server)
	}()
	result, err := r.Input(context.Background(), RunInputRequest{RunID: "RUN-1", Reader: bytes.NewReader([]byte("once"))})
	var inputErr *RunInputError
	if result == nil || result.Accepted != 4 || !errors.As(err, &inputErr) || inputErr.Code != "E_RUN_INPUT_OUTCOME_UNKNOWN" || inputErr.Committed != 4 || dials != 1 {
		t.Fatalf("result=%+v err=%v dials=%d", result, err, dials)
	}
	if got := <-received; !bytes.Equal(got, []byte("once")) {
		t.Fatalf("received=%q", got)
	}
}

func TestRunInputClientClassifiesDiscoveryBeforeDial(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	appendRunEvent(t, r, "starting", RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, StdinConnect: true})
	appendRunEvent(t, r, "starting", RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-2", Status: StatusStarting, StdinConnect: true})
	appendRunEvent(t, r, "terminal", RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-2", Status: StatusExited, StdinConnect: true, InputSocket: "/must-not-dial"})
	appendRunEvent(t, r, "starting", RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-3", Status: StatusRunning})
	dials := 0
	r.inputDialFn = func(context.Context, string) (net.Conn, error) {
		dials++
		return nil, errors.New("dial must not run")
	}
	for id, code := range map[string]string{
		"RUN-1": "E_RUN_INPUT_NOT_READY",
		"RUN-2": "E_RUN_INPUT_CLOSED",
		"RUN-3": "E_RUN_INPUT_UNAVAILABLE",
	} {
		_, err := r.Input(context.Background(), RunInputRequest{RunID: id, Close: true})
		var inputErr *RunInputError
		if !errors.As(err, &inputErr) || inputErr.Code != code {
			t.Fatalf("%s error=%v want=%s", id, err, code)
		}
	}
	if dials != 0 {
		t.Fatalf("discovery classifications dialled %d times", dials)
	}
}

func TestRunInputClientReportsDeterminatePartialCommittedCount(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	appendRunEvent(t, r, "running", RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Owner: r.owner, Status: StatusRunning, StdinConnect: true, InputSocket: "/injected"})
	inputR, inputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputR.Close()
	defer inputW.Close()
	plane := &runInputPlane{inputW: inputW, owner: r.owner, peerUID: func(net.Conn) (int, error) { return os.Geteuid(), nil }}
	plane.state.Store(1)
	client, server := net.Pipe()
	r.inputDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go plane.handle(server)
	go func() {
		_, _ = io.CopyN(io.Discard, inputR, 4096)
		_ = inputR.Close()
	}()
	payload := make([]byte, MaxRunInputFrameBytes)
	_, err = r.Input(context.Background(), RunInputRequest{RunID: "RUN-1", Reader: bytes.NewReader(payload)})
	var inputErr *RunInputError
	if !errors.As(err, &inputErr) || inputErr.Code != "E_RUN_INPUT_PARTIAL" || inputErr.Committed <= 0 || inputErr.Committed >= int64(len(payload)) {
		t.Fatalf("partial error=%v", err)
	}
}

type closeAllWriteConn struct{ net.Conn }

func (c *closeAllWriteConn) CloseWrite() error { return c.Close() }

func TestRunInputServerACKReconnectAppendAndExplicitClose(t *testing.T) {
	plane := newTestRunInputPlane(t, "owner")
	plane.serve()

	first := dialRunInputHello(t, plane.path, runInputHello{Owner: "owner"})
	if err := writeRunInputFrame(first, runInputOpData, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if committed := mustRunInputACK(t, first); committed != 5 {
		t.Fatalf("first ACK=%d", committed)
	}
	if err := first.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if committed := mustRunInputACK(t, first); committed != 5 {
		t.Fatalf("final first ACK=%d", committed)
	}
	_ = first.Close()

	second := dialRunInputHello(t, plane.path, runInputHello{Owner: "owner"})
	if err := writeRunInputFrame(second, runInputOpData, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if committed := mustRunInputACK(t, second); committed != 6 {
		t.Fatalf("second ACK=%d", committed)
	}
	if err := writeRunInputFrame(second, runInputOpClose, nil); err != nil {
		t.Fatal(err)
	}
	if committed := mustRunInputACK(t, second); committed != 6 {
		t.Fatalf("close ACK=%d", committed)
	}
	_ = second.Close()

	data, err := io.ReadAll(plane.inputR)
	if err != nil || !bytes.Equal(data, []byte("firstsecond")) {
		t.Fatalf("stdin=%q err=%v", data, err)
	}
}

func TestRunInputServerContinuousAcceptBusyWithNonReadingPeer(t *testing.T) {
	plane := newTestRunInputPlane(t, "owner")
	plane.serve()
	active := dialRunInputHello(t, plane.path, runInputHello{Owner: "owner"})
	defer active.Close()

	nonReading, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: plane.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer nonReading.Close()

	third, err := net.DialTimeout("unix", plane.path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	_ = third.SetReadDeadline(time.Now().Add(time.Second))
	op, payload, err := readRunInputFrame(third)
	if err != nil || op != runInputOpError {
		t.Fatalf("third op=%d payload=%q err=%v", op, payload, err)
	}
	var inputErr *RunInputError
	if err := decodeRunInputWireError(payload); !errors.As(err, &inputErr) || inputErr.Code != "E_RUN_INPUT_BUSY" {
		t.Fatalf("third refusal=%v", err)
	}
}

func TestRunInputServerAuthStealAndFraming(t *testing.T) {
	t.Run("peer uid", func(t *testing.T) {
		plane := newTestRunInputPlane(t, "owner")
		plane.peerUID = func(net.Conn) (int, error) { return os.Geteuid() + 1, nil }
		plane.serve()
		conn, err := net.Dial("unix", plane.path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		hello, _ := encodeRunInputJSON(runInputHello{Owner: "owner"})
		_ = writeRunInputFrame(conn, runInputOpHello, hello)
		assertRunInputErrorCode(t, conn, "E_RUN_INPUT_PROTOCOL")
	})

	t.Run("foreign and steal", func(t *testing.T) {
		plane := newTestRunInputPlane(t, "owner-a")
		plane.serve()
		foreign := dialRunInputRawHello(t, plane.path, runInputHello{Owner: "owner-b"})
		assertRunInputErrorCode(t, foreign, "E_RUN_INPUT_FOREIGN_OWNER")
		_ = foreign.Close()
		steal := dialRunInputHello(t, plane.path, runInputHello{Owner: "owner-b", Steal: true})
		_ = writeRunInputFrame(steal, runInputOpClose, nil)
		_ = mustRunInputACK(t, steal)
		_ = steal.Close()
	})

	t.Run("oversize leaves pipe untouched", func(t *testing.T) {
		plane := newTestRunInputPlane(t, "owner")
		plane.serve()
		conn := dialRunInputHello(t, plane.path, runInputHello{Owner: "owner"})
		var header [5]byte
		header[0] = runInputOpData
		binary.BigEndian.PutUint32(header[1:], uint32(MaxRunInputFrameBytes+1))
		if _, err := conn.Write(header[:]); err != nil {
			t.Fatal(err)
		}
		assertRunInputErrorCode(t, conn, "E_RUN_INPUT_PROTOCOL")
		_ = conn.Close()
		closer := dialRunInputHello(t, plane.path, runInputHello{Owner: "owner"})
		_ = writeRunInputFrame(closer, runInputOpClose, nil)
		_ = mustRunInputACK(t, closer)
		data, err := io.ReadAll(plane.inputR)
		if err != nil || len(data) != 0 {
			t.Fatalf("pipe changed: %x err=%v", data, err)
		}
	})
}

func newTestRunInputPlane(t *testing.T, owner string) *runInputPlane {
	t.Helper()
	base := newRunInputRuntimeDir(t)
	plane, err := prepareRunInputPlane(base, "RUN-1", owner)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EROFS) {
			skipOrFailRunInputSocket(t, "Unix socket unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(plane.closeTerminal)
	info, err := os.Stat(plane.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode=%v err=%v", info, err)
	}
	return plane
}

func newRunInputRuntimeDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "tmp", "aira-d6-run-input-tests")
	if err := os.MkdirAll(root, 0o700); err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
			skipOrFailRunInputSocket(t, "run-input test directory unavailable: %v", err)
		}
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(root, "case-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	absolute, err := filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func skipOrFailRunInputSocket(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("AIRA_REAL_SOCKET") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func dialRunInputRawHello(t *testing.T, path string, hello runInputHello) *net.UnixConn {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := encodeRunInputJSON(hello)
	if err := writeRunInputFrame(conn, runInputOpHello, payload); err != nil {
		t.Fatal(err)
	}
	return conn
}

func dialRunInputHello(t *testing.T, path string, hello runInputHello) *net.UnixConn {
	t.Helper()
	conn := dialRunInputRawHello(t, path, hello)
	if committed := mustRunInputACK(t, conn); committed != 0 {
		t.Fatalf("HELLO ACK=%d", committed)
	}
	return conn
}

func mustRunInputACK(t *testing.T, reader io.Reader) int64 {
	t.Helper()
	op, payload, err := readRunInputFrame(reader)
	if err != nil || op != runInputOpAck {
		t.Fatalf("ACK op=%d payload=%x err=%v", op, payload, err)
	}
	committed, err := decodeRunInputAck(payload)
	if err != nil {
		t.Fatal(err)
	}
	return committed
}

func assertRunInputErrorCode(t *testing.T, reader io.Reader, code string) {
	t.Helper()
	op, payload, err := readRunInputFrame(reader)
	if err != nil || op != runInputOpError {
		t.Fatalf("error frame op=%d payload=%q err=%v", op, payload, err)
	}
	var inputErr *RunInputError
	decoded := decodeRunInputWireError(payload)
	if !errors.As(decoded, &inputErr) || inputErr.Code != code {
		t.Fatalf("error=%v want=%s", decoded, code)
	}
}
