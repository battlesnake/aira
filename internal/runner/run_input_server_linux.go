//go:build linux

package runner

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	runInputHelloTimeout  = 2 * time.Second
	runInputRejectTimeout = 100 * time.Millisecond
	runInputNonceAttempts = 16
	unixSocketPathMax     = 107
)

type runInputPlane struct {
	listener *net.UnixListener
	inputR   *os.File
	inputW   *os.File
	path     string
	owner    string

	state atomic.Int32 // 0 free, 1 claimed, 2 permanently closed
	once  sync.Once
	mu    sync.Mutex
	conn  net.Conn

	rejectSlots chan struct{}
	peerUID     func(net.Conn) (int, error)
}

func prepareRunInputPlane(runtimeDir, runID, owner string) (*runInputPlane, error) {
	if runtimeDir == "" {
		return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: errors.New("run input runtime directory is unavailable")}
	}
	dir := filepath.Join(runtimeDir, "inputs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: err}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: err}
	}
	inputR, inputW, err := os.Pipe()
	if err != nil {
		return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: err}
	}
	closePipe := true
	defer func() {
		if closePipe {
			_ = inputR.Close()
			_ = inputW.Close()
		}
	}()
	for attempt := 0; attempt < runInputNonceAttempts; attempt++ {
		nonce, nonceErr := freshRunInputNonce()
		if nonceErr != nil {
			return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: nonceErr}
		}
		path := filepath.Join(dir, runID+"-"+nonce+".sock")
		if len(path) > unixSocketPathMax {
			return nil, &RunInputError{Code: "E_RUN_INPUT_PATH_TOO_LONG", Err: fmt.Errorf("Unix socket path is %d bytes; maximum is %d", len(path), unixSocketPathMax)}
		}
		listener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if listenErr != nil {
			if errors.Is(listenErr, syscall.EADDRINUSE) {
				continue
			}
			return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: listenErr}
		}
		listener.SetUnlinkOnClose(false)
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			_ = listener.Close()
			_ = os.Remove(path)
			return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: chmodErr}
		}
		closePipe = false
		return &runInputPlane{
			listener: listener, inputR: inputR, inputW: inputW, path: path, owner: owner,
			rejectSlots: make(chan struct{}, 8), peerUID: runInputPeerUID,
		}, nil
	}
	return nil, &RunInputError{Code: "E_RUN_INPUT_UNREACHABLE", Err: syscall.EADDRINUSE}
}

func freshRunInputNonce() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func runInputPeerUID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("peer is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if controlErr != nil || credential == nil {
		return 0, controlErr
	}
	return int(credential.Uid), nil
}

func (p *runInputPlane) serve() {
	go p.acceptLoop()
}

func (p *runInputPlane) acceptLoop() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		if !p.state.CompareAndSwap(0, 1) {
			code := "E_RUN_INPUT_BUSY"
			if p.state.Load() == 2 {
				code = "E_RUN_INPUT_CLOSED"
			}
			p.reject(conn, code)
			continue
		}
		p.mu.Lock()
		if p.state.Load() != 1 {
			p.mu.Unlock()
			_ = conn.Close()
			continue
		}
		p.conn = conn
		p.mu.Unlock()
		go p.handle(conn)
	}
}

func (p *runInputPlane) reject(conn net.Conn, code string) {
	select {
	case p.rejectSlots <- struct{}{}:
		go func() {
			defer func() { <-p.rejectSlots }()
			defer conn.Close()
			_ = conn.SetWriteDeadline(time.Now().Add(runInputRejectTimeout))
			_ = writeRunInputError(conn, code, 0, code)
		}()
	default:
		_ = conn.Close()
	}
}

func (p *runInputPlane) handle(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		p.mu.Lock()
		if p.conn == conn {
			p.conn = nil
		}
		p.mu.Unlock()
		p.state.CompareAndSwap(1, 0)
	}()
	_ = conn.SetReadDeadline(time.Now().Add(runInputHelloTimeout))
	uid, credentialErr := p.peerUID(conn)
	if credentialErr != nil || uid != os.Geteuid() {
		writeRunInputBoundedError(conn, "E_RUN_INPUT_PROTOCOL", 0, "peer credentials rejected")
		return
	}
	op, payload, err := readRunInputFrame(conn)
	if err != nil || op != runInputOpHello {
		writeRunInputBoundedError(conn, "E_RUN_INPUT_PROTOCOL", 0, "HELLO frame required")
		return
	}
	var hello runInputHello
	if err := json.Unmarshal(payload, &hello); err != nil {
		writeRunInputBoundedError(conn, "E_RUN_INPUT_PROTOCOL", 0, "invalid HELLO frame")
		return
	}
	if !hello.Steal && p.owner != "" && p.owner != hello.Owner {
		writeRunInputBoundedError(conn, "E_RUN_INPUT_FOREIGN_OWNER", 0, "run belongs to another owner; pass --steal to override")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if err := writeRunInputFrame(conn, runInputOpAck, encodeRunInputAck(0)); err != nil {
		return
	}
	var committed int64
	for {
		op, payload, err = readRunInputFrame(conn)
		if errors.Is(err, io.EOF) {
			_ = writeRunInputFrame(conn, runInputOpAck, encodeRunInputAck(committed))
			return
		}
		if err != nil {
			var protocol *RunInputError
			if errors.As(err, &protocol) && protocol.Code == "E_RUN_INPUT_PROTOCOL" {
				writeRunInputBoundedError(conn, protocol.Code, committed, protocol.Error())
			}
			return
		}
		switch op {
		case runInputOpData:
			for len(payload) > 0 {
				n, writeErr := p.inputW.Write(payload)
				if n > 0 {
					committed += int64(n)
					payload = payload[n:]
				}
				if writeErr != nil || n == 0 {
					writeRunInputBoundedError(conn, "E_RUN_INPUT_CLOSED", committed, "child stdin is closed")
					return
				}
			}
			if err := writeRunInputFrame(conn, runInputOpAck, encodeRunInputAck(committed)); err != nil {
				return
			}
		case runInputOpClose:
			// ACK BEFORE closing inputW: closing inputW EOFs the child, which can
			// exit and trigger the leader-exit teardown (which closes THIS
			// connection) — racing the ACK write. Sending the ACK first means the
			// client has its confirmation before the child-exit teardown can fire.
			_ = writeRunInputFrame(conn, runInputOpAck, encodeRunInputAck(committed))
			p.closeInput()
			return
		default:
			writeRunInputBoundedError(conn, "E_RUN_INPUT_PROTOCOL", committed, fmt.Sprintf("unexpected opcode %d", op))
			return
		}
	}
}

func writeRunInputBoundedError(conn net.Conn, code string, committed int64, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(runInputRejectTimeout))
	_ = writeRunInputError(conn, code, committed, message)
}

func writeRunInputError(writer io.Writer, code string, committed int64, message string) error {
	payload, err := encodeRunInputJSON(runInputWireError{Code: code, Committed: committed, Message: message})
	if err != nil {
		return err
	}
	return writeRunInputFrame(writer, runInputOpError, payload)
}

func (p *runInputPlane) closeInput() {
	p.state.Store(2)
	_ = p.inputW.Close()
}

func (p *runInputPlane) closeTerminal() {
	p.once.Do(func() {
		p.state.Store(2)
		_ = p.listener.Close()
		p.mu.Lock()
		if p.conn != nil {
			_ = p.conn.Close()
		}
		p.mu.Unlock()
		_ = p.inputW.Close()
		_ = p.inputR.Close()
		_ = os.Remove(p.path)
	})
}
