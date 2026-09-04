package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultGovernorMaxWait = 300 * time.Second

// governorMaxWait is deliberately generous: an admitted-at-start worker may be
// parked for a real rotation. It is only a liveness backstop for a wedged daemon.
func governorMaxWait() time.Duration {
	raw := os.Getenv("AIRA_GOVERNOR_MAX_WAIT")
	if raw == "" {
		return defaultGovernorMaxWait
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return defaultGovernorMaxWait
	}
	return value
}

// GovernorSlotRequest is the line-protocol relay used by one pytest worker.
// It is deliberately independent of the daemon package (which imports runner).
type GovernorSlotRequest struct {
	SocketPath string
	JobID      string
	// Slice is the validated confine slice name carried in acquire. The daemon
	// resolves it to the canonical ledger path once and keeps it immutable.
	Slice  string
	Stdin  io.Reader
	Stdout io.Writer
	Dial   func(context.Context, string) (net.Conn, error) // test seam
	UUID   string                                          // test seam; empty generates one
}

type governorWireRequest struct {
	Proto   int            `json:"proto,omitempty"`
	Scope   map[string]any `json:"scope,omitempty"`
	Request *struct {
		Verb string         `json:"verb"`
		Args map[string]any `json:"args"`
	} `json:"request,omitempty"`
	Type        string `json:"type,omitempty"`
	WorkerUUID  string `json:"worker_uuid,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	Slice       string `json:"slice,omitempty"`
	HeldRSS     int64  `json:"held_rss,omitempty"`
	NextTestEst int64  `json:"next_test_est,omitempty"`
}
type governorWireReply struct {
	State string `json:"state"`
}

func newGovernorUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func writeGovernorFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > 16<<20 {
		return errors.New("governor frame invalid")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err = writeGovernorBytes(w, header[:]); err != nil {
		return err
	}
	return writeGovernorBytes(w, payload)
}

func writeGovernorBytes(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func readGovernorFrame(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > 16<<20 {
		return errors.New("governor frame invalid")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}

func parseGovernorCheckpoint(line string) (int64, int64, bool) {
	fields := strings.Fields(line)
	if len(fields) != 1 && len(fields) != 3 || len(fields) > 0 && fields[0] != "checkpoint" {
		return 0, 0, false
	}
	if len(fields) == 1 {
		return 0, 0, true
	}
	var rss, est int64
	if _, err := fmt.Sscan(fields[1], &rss); err != nil || rss < 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscan(fields[2], &est); err != nil || est < 0 || est > 1<<60 {
		return 0, 0, false
	}
	return rss, est, true
}

func governorDial(ctx context.Context, req GovernorSlotRequest) (net.Conn, error) {
	if req.Dial != nil {
		return req.Dial(ctx, req.SocketPath)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", req.SocketPath)
}

// connectGovernor holds the acquire connection until the daemon admits it, up
// to maxWait. EOF and context cancellation still interrupt it promptly.
func connectGovernor(ctx context.Context, req GovernorSlotRequest, uuid string, eof <-chan struct{}, maxWait time.Duration) (net.Conn, string, error) {
	conn, err := governorDial(ctx, req)
	if err != nil {
		return nil, "", err
	}
	fail := func(err error) (net.Conn, string, error) { _ = conn.Close(); return nil, "", err }
	interruptDone := make(chan struct{})
	go func() {
		select {
		case <-eof:
			_ = conn.Close()
		case <-ctx.Done():
			_ = conn.Close()
		case <-interruptDone:
		}
	}()
	defer close(interruptDone)
	request := &struct {
		Verb string         `json:"verb"`
		Args map[string]any `json:"args"`
	}{Verb: "governor", Args: map[string]any{}}
	// DaemonProtocolVersion, never a literal: this was hand-written as `5` and
	// would have silently mismatched the daemon the moment ProtocolVersion moved
	// (found while bumping it for AIRA-39). It is the same hand-copied-constant
	// class AIRA-83 item 3 pinned for DaemonProtocolVersion itself.
	if err := writeGovernorFrame(conn, governorWireRequest{Proto: DaemonProtocolVersion, Scope: map[string]any{}, Request: request}); err != nil {
		return fail(err)
	}
	if err := writeGovernorFrame(conn, governorWireRequest{Type: "acquire", WorkerUUID: uuid, JobID: req.JobID, Slice: req.Slice}); err != nil {
		return fail(err)
	}
	var reply governorWireReply
	if err := conn.SetReadDeadline(time.Now().Add(maxWait)); err != nil {
		return fail(err)
	}
	if err := readGovernorFrame(conn, &reply); err != nil {
		return fail(err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	if reply.State != "active" && reply.State != "continue" {
		return fail(errors.New("invalid governor acquire reply"))
	}
	return conn, reply.State, nil
}

// GovernorSlot relays checkpoint lines. The stdin goroutine is the only stdin
// owner. EOF or cancellation closes the held socket so a parked long-poll
// read cannot delay worker shutdown until its reply deadline.
func GovernorSlot(ctx context.Context, req GovernorSlotRequest) int {
	if strings.TrimSpace(req.JobID) == "" {
		_, _ = fmt.Fprintln(req.Stdout, "continue")
		return 0
	}
	if strings.TrimSpace(req.Slice) == "" {
		// Match confine-reserve's default charge target. The daemon resolves and
		// validates this name to its canonical ledger path on acquire.
		req.Slice = DefaultConfineSlice
	}
	uuid := req.UUID
	if uuid == "" {
		var err error
		uuid, err = newGovernorUUID()
		if err != nil {
			_, _ = fmt.Fprintln(req.Stdout, "continue")
			return 0
		}
	}
	maxWait := governorMaxWait()
	lines := make(chan string, 1)
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		defer close(lines)
		scanner := bufio.NewScanner(req.Stdin)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	var conn net.Conn
	var connMu sync.Mutex
	closeConn := func() {
		connMu.Lock()
		current := conn
		conn = nil
		connMu.Unlock()
		if current != nil {
			_ = current.Close()
		}
	}
	defer closeConn()
	relayDone := make(chan struct{})
	defer close(relayDone)
	go func() {
		select {
		case <-eof:
			closeConn()
		case <-ctx.Done():
			closeConn()
		case <-relayDone:
		}
	}()
	interrupted := func() bool {
		select {
		case <-eof:
			return true
		case <-ctx.Done():
			return true
		default:
			return false
		}
	}
	installConn := func(newConn net.Conn) bool {
		if interrupted() {
			return false
		}
		connMu.Lock()
		if conn != nil {
			connMu.Unlock()
			return false
		}
		conn = newConn
		connMu.Unlock()
		if !interrupted() {
			return true
		}
		// EOF may have won after the first check, while the closer had no
		// installed connection yet. Remove only this connection; a concurrent
		// closer or reconnect must not be disturbed.
		connMu.Lock()
		if conn == newConn {
			conn = nil
		}
		connMu.Unlock()
		return false
	}
	var reconnecting bool
	scheduleReconnect := func() {
		connMu.Lock()
		if conn != nil || reconnecting {
			connMu.Unlock()
			return
		}
		reconnecting = true
		connMu.Unlock()
		go func() {
			defer func() {
				connMu.Lock()
				reconnecting = false
				connMu.Unlock()
			}()
			if interrupted() {
				return
			}
			newConn, _, reconnectErr := connectGovernor(ctx, req, uuid, eof, maxWait)
			if reconnectErr != nil {
				return
			}
			if !installConn(newConn) {
				_ = newConn.Close()
			}
		}()
	}
	var logFailureOnce sync.Once
	failOpen := func(err error) {
		// The stdout reply comes before reconnecting: a daemon outage must never
		// make the worker wait for a new acquire attempt.
		_, _ = fmt.Fprintln(req.Stdout, "continue")
		logFailureOnce.Do(func() { log.Printf("aira governor relay: fail-open: %v", err) })
	}
	// The relay is child-side crash safety in addition to the EOF watcher.
	if !setGovernorParentDeathSignal() { /* non-Linux or unavailable: EOF remains the guard */
	}
	var err error
	initialConn, state, err := connectGovernor(ctx, req, uuid, eof, maxWait)
	if err != nil {
		failOpen(err)
	} else if !installConn(initialConn) {
		_ = initialConn.Close()
		return 0
	} else {
		_, _ = fmt.Fprintln(req.Stdout, state)
	}
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-eof:
			closeConn()
			return 0
		case line, ok := <-lines:
			if !ok {
				closeConn()
				return 0
			}
			rss, est, valid := parseGovernorCheckpoint(line)
			if !valid {
				_, _ = fmt.Fprintln(req.Stdout, "continue")
				continue
			}
			connMu.Lock()
			current := conn
			connMu.Unlock()
			if current == nil {
				// Do not make this checkpoint wait behind re-acquire. The next one
				// may use a best-effort connection if it becomes available.
				failOpen(errors.New("governor connection unavailable"))
				scheduleReconnect()
				continue
			}
			if err := writeGovernorFrame(current, governorWireRequest{Type: "checkpoint", HeldRSS: rss, NextTestEst: est}); err != nil {
				closeConn()
				failOpen(err)
				scheduleReconnect()
				continue
			}
			// EOF closes current from the concurrent reader, so this long-poll
			// read exits even if the daemon has parked this worker.
			if err := current.SetReadDeadline(time.Now().Add(maxWait)); err != nil {
				closeConn()
				failOpen(err)
				scheduleReconnect()
				continue
			}
			var reply governorWireReply
			err = readGovernorFrame(current, &reply)
			_ = current.SetReadDeadline(time.Time{})
			if err != nil || (reply.State != "continue" && reply.State != "active") {
				closeConn()
				if err == nil {
					err = errors.New("invalid governor checkpoint reply")
				}
				failOpen(err)
				scheduleReconnect()
				continue
			}
			_, _ = fmt.Fprintln(req.Stdout, reply.State)
		}
	}
}
