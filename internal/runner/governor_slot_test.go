package runner

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// Revert-check: removing the EOF close or the same UUID reconnect path causes
// these fake-listener tests to time out or observe a changed worker UUID.
func TestGovernorSlotFailOpenOnDialFailure(t *testing.T) {
	var output bytes.Buffer
	if got := GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "/missing", JobID: "job", Stdin: bytes.NewBuffer(nil), Stdout: &output, Dial: func(context.Context, string) (net.Conn, error) { return nil, net.ErrClosed }}); got != 0 || output.String() != "continue\n" {
		t.Fatalf("got exit=%d output=%q", got, output.String())
	}
}

func TestGovernorSlotCarriesSliceInAcquireFrame(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	stdin, stdinWriter := net.Pipe()
	defer stdinWriter.Close()
	var output bytes.Buffer
	received := make(chan governorWireRequest, 1)
	go func() {
		var envelope governorWireRequest
		_ = readGovernorFrame(server, &envelope)
		var acquire governorWireRequest
		_ = readGovernorFrame(server, &acquire)
		received <- acquire
		_ = writeGovernorFrame(server, governorWireReply{State: "active"})
	}()
	done := make(chan int, 1)
	go func() {
		done <- GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Slice: "finite.slice", Stdin: stdin, Stdout: &output, UUID: "stable", Dial: func(context.Context, string) (net.Conn, error) { return client, nil }})
	}()
	select {
	case acquire := <-received:
		if acquire.Type != "acquire" || acquire.Slice != "finite.slice" {
			t.Fatalf("acquire frame=%+v", acquire)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not send acquire")
	}
	_ = stdinWriter.Close()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("exit=%d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not exit after stdin EOF")
	}
}

func TestGovernorMaxWaitUsesGenerousDefaultOnInvalidSetting(t *testing.T) {
	for _, raw := range []string{"", "not-a-duration", "0s", "-1s"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("AIRA_GOVERNOR_MAX_WAIT", raw)
			if got := governorMaxWait(); got != defaultGovernorMaxWait {
				t.Fatalf("max wait for %q = %s, want %s", raw, got, defaultGovernorMaxWait)
			}
		})
	}
	t.Setenv("AIRA_GOVERNOR_MAX_WAIT", "750ms")
	if got := governorMaxWait(); got != 750*time.Millisecond {
		t.Fatalf("configured max wait=%s", got)
	}
}

func TestGovernorSlotStdinEOFClosesHeldConnection(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	stdinReader, stdinWriter := net.Pipe()
	var output bytes.Buffer
	parked := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		var envelope governorWireRequest
		_ = readGovernorFrame(server, &envelope)
		var acquire governorWireRequest
		_ = readGovernorFrame(server, &acquire)
		_ = writeGovernorFrame(server, governorWireReply{State: "active"})
		var checkpoint governorWireRequest
		_ = readGovernorFrame(server, &checkpoint)
		close(parked)
		// Deliberately withhold the checkpoint reply: the relay is parked in
		// readGovernorFrame, not idle in its select loop.
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	done := make(chan int, 1)
	go func() {
		done <- GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: stdinReader, Stdout: &output, UUID: "stable", Dial: func(context.Context, string) (net.Conn, error) { return client, nil }})
	}()
	// Drive one checkpoint so the relay sends a checkpoint frame and then blocks
	// in readGovernorFrame awaiting the (deliberately withheld) reply — i.e. it is
	// parked in a long-poll read, not idle in its select loop.
	go func() { _, _ = stdinWriter.Write([]byte("checkpoint\n")) }()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not reach parked checkpoint read")
	}
	_ = stdinWriter.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stdin EOF did not close daemon connection")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not exit")
	}
}

func TestGovernorSlotAcquireWaitsForAdmissionAndEOFCloses(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	stdinReader, stdinWriter := net.Pipe()
	var output bytes.Buffer
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		var envelope governorWireRequest
		_ = readGovernorFrame(server, &envelope)
		var acquire governorWireRequest
		_ = readGovernorFrame(server, &acquire)
		close(accepted)
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	done := make(chan int, 1)
	go func() {
		done <- GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: stdinReader, Stdout: &output, UUID: "stable", Dial: func(context.Context, string) (net.Conn, error) { return client, nil }})
	}()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not receive acquire")
	}
	select {
	case result := <-done:
		t.Fatalf("relay returned before admission: exit=%d output=%q", result, output.String())
	case <-time.After(200 * time.Millisecond):
	}
	if output.String() != "" {
		t.Fatalf("relay failed open before admission: %q", output.String())
	}
	_ = stdinWriter.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stdin EOF did not close held acquire connection")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not return after EOF")
	}
}

func TestGovernorSlotStuckDaemonReplyFailsOpenWithinConfiguredBound(t *testing.T) {
	// Revert-check: without the acquire read deadline this test times out below.
	// The fake daemon accepts and consumes the acquire, so this is specifically a
	// stuck-but-alive daemon rather than a dial failure.
	t.Setenv("AIRA_GOVERNOR_MAX_WAIT", "200ms")
	server, client := net.Pipe()
	defer server.Close()
	stdinReader, stdinWriter := net.Pipe()
	output := &governorOutputSpy{writes: make(chan string, 2)}
	accepted := make(chan struct{})
	go func() {
		var envelope governorWireRequest
		_ = readGovernorFrame(server, &envelope)
		var acquire governorWireRequest
		_ = readGovernorFrame(server, &acquire)
		close(accepted)
		// Deliberately never write a reply.
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	done := make(chan int, 1)
	go func() {
		done <- GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: stdinReader, Stdout: output, UUID: "stable", Dial: func(context.Context, string) (net.Conn, error) { return client, nil }})
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("daemon did not receive acquire")
	}
	select {
	case got := <-output.writes:
		if got != "continue\n" {
			t.Fatalf("fail-open output=%q", got)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("stuck acquire did not fail open within configured bound")
	}
	_ = stdinWriter.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not exit after fail-open")
	}
}

func TestGovernorSlotReconnectDoesNotBlockFailOpenOutput(t *testing.T) {
	t.Setenv("AIRA_GOVERNOR_MAX_WAIT", "200ms")
	firstServer, firstClient := net.Pipe()
	defer firstServer.Close()
	stdinReader, stdinWriter := net.Pipe()
	defer stdinWriter.Close()
	output := &governorOutputSpy{writes: make(chan string, 4)}
	outputSeen := make(chan struct{})
	secondAccepted := make(chan struct{})
	dials := 0
	dial := func(context.Context, string) (net.Conn, error) {
		dials++
		if dials == 1 {
			return firstClient, nil
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var envelope governorWireRequest
			_ = readGovernorFrame(server, &envelope)
			var acquire governorWireRequest
			_ = readGovernorFrame(server, &acquire)
			select {
			case <-outputSeen:
			default:
				t.Errorf("reconnect began before fail-open output")
			}
			close(secondAccepted)
			var one [1]byte
			_, _ = server.Read(one[:]) // Never reply to this re-acquire.
		}()
		return client, nil
	}
	go func() {
		var envelope governorWireRequest
		_ = readGovernorFrame(firstServer, &envelope)
		var acquire governorWireRequest
		_ = readGovernorFrame(firstServer, &acquire)
		_ = writeGovernorFrame(firstServer, governorWireReply{State: "active"})
		var checkpoint governorWireRequest
		_ = readGovernorFrame(firstServer, &checkpoint)
		// Keep the connection open but do not reply, exercising the bounded
		// checkpoint read rather than a simple EOF.
		var one [1]byte
		_, _ = firstServer.Read(one[:])
	}()
	done := make(chan int, 1)
	go func() {
		done <- GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: stdinReader, Stdout: output, UUID: "stable", Dial: dial})
	}()
	if got := <-output.writes; got != "active\n" {
		t.Fatalf("acquire output=%q", got)
	}
	_, _ = stdinWriter.Write([]byte("checkpoint\n"))
	select {
	case got := <-output.writes:
		if got != "continue\n" {
			t.Fatalf("checkpoint fail-open output=%q", got)
		}
		close(outputSeen)
	case <-time.After(700 * time.Millisecond):
		t.Fatal("checkpoint failure waited behind reconnect")
	}
	select {
	case <-secondAccepted:
	case <-time.After(time.Second):
		t.Fatal("background reconnect did not start")
	}
	_ = stdinWriter.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not exit")
	}
}

func TestGovernorSlotPrintsActualAcquireState(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	stdinReader, stdinWriter := net.Pipe()
	defer stdinWriter.Close()
	output := &governorOutputSpy{writes: make(chan string, 2)}
	go func() {
		var envelope governorWireRequest
		_ = readGovernorFrame(server, &envelope)
		var acquire governorWireRequest
		_ = readGovernorFrame(server, &acquire)
		_ = writeGovernorFrame(server, governorWireReply{State: "continue"})
	}()
	done := make(chan int, 1)
	go func() {
		done <- GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: stdinReader, Stdout: output, UUID: "stable", Dial: func(context.Context, string) (net.Conn, error) { return client, nil }})
	}()
	select {
	case got := <-output.writes:
		if got != "continue\n" {
			t.Fatalf("acquire output=%q, want continue", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not print acquire state")
	}
	_ = stdinWriter.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not exit")
	}
}

func TestGovernorSlotFailOpenOnMalformedOrTimedOutAcquire(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		server, client := net.Pipe()
		defer server.Close()
		var output bytes.Buffer
		go func() {
			var envelope governorWireRequest
			_ = readGovernorFrame(server, &envelope)
			var acquire governorWireRequest
			_ = readGovernorFrame(server, &acquire)
			_ = writeGovernorFrame(server, governorWireReply{State: "not-a-state"})
		}()
		if got := GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: bytes.NewBuffer(nil), Stdout: &output, Dial: func(context.Context, string) (net.Conn, error) { return client, nil }}); got != 0 || output.String() != "continue\n" {
			t.Fatalf("got exit=%d output=%q", got, output.String())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		var output bytes.Buffer
		if got := GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: bytes.NewBuffer(nil), Stdout: &output, Dial: func(context.Context, string) (net.Conn, error) { return immediateTimeoutConn{}, nil }}); got != 0 || output.String() != "continue\n" {
			t.Fatalf("got exit=%d output=%q", got, output.String())
		}
	})
}

type immediateTimeoutConn struct{}

func (immediateTimeoutConn) Read([]byte) (int, error)         { return 0, immediateTimeoutError{} }
func (immediateTimeoutConn) Write(p []byte) (int, error)      { return len(p), nil }
func (immediateTimeoutConn) Close() error                     { return nil }
func (immediateTimeoutConn) LocalAddr() net.Addr              { return timeoutAddr("local") }
func (immediateTimeoutConn) RemoteAddr() net.Addr             { return timeoutAddr("remote") }
func (immediateTimeoutConn) SetDeadline(time.Time) error      { return nil }
func (immediateTimeoutConn) SetReadDeadline(time.Time) error  { return nil }
func (immediateTimeoutConn) SetWriteDeadline(time.Time) error { return nil }

type immediateTimeoutError struct{}

func (immediateTimeoutError) Error() string   { return "read timeout" }
func (immediateTimeoutError) Timeout() bool   { return true }
func (immediateTimeoutError) Temporary() bool { return true }

type timeoutAddr string

func (a timeoutAddr) Network() string { return "timeout" }
func (a timeoutAddr) String() string  { return string(a) }

type governorOutputSpy struct {
	writes chan string
}

func (s *governorOutputSpy) Write(p []byte) (int, error) {
	copy := string(append([]byte(nil), p...))
	select {
	case s.writes <- copy:
	default:
	}
	return len(p), nil
}

func TestGovernorSlotReconnectsWithSameUUID(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	stdinReader, stdinWriter := net.Pipe()
	defer stdinWriter.Close()
	var output bytes.Buffer
	var mu sync.Mutex
	var seen []string
	lost := make(chan struct{})
	connected := make(chan struct{})
	reconnected := make(chan struct{})
	dials := 0
	dial := func(context.Context, string) (net.Conn, error) {
		dials++
		if dials == 1 {
			return client, nil
		}
		left, right := net.Pipe()
		go func() {
			defer right.Close()
			var envelope governorWireRequest
			_ = readGovernorFrame(right, &envelope)
			var acquire governorWireRequest
			_ = readGovernorFrame(right, &acquire)
			mu.Lock()
			seen = append(seen, acquire.WorkerUUID)
			mu.Unlock()
			_ = writeGovernorFrame(right, governorWireReply{State: "active"})
			close(connected)
			var checkpoint governorWireRequest
			_ = readGovernorFrame(right, &checkpoint)
			_ = writeGovernorFrame(right, governorWireReply{State: "continue"})
			close(reconnected)
		}()
		return left, nil
	}
	done := make(chan int, 1)
	go func() {
		done <- GovernorSlot(context.Background(), GovernorSlotRequest{SocketPath: "fake", JobID: "job", Stdin: stdinReader, Stdout: &output, Dial: dial, UUID: "stable"})
	}()
	go func() {
		defer server.Close()
		var envelope governorWireRequest
		_ = readGovernorFrame(server, &envelope)
		var acquire governorWireRequest
		_ = readGovernorFrame(server, &acquire)
		mu.Lock()
		seen = append(seen, acquire.WorkerUUID)
		mu.Unlock()
		_ = writeGovernorFrame(server, governorWireReply{State: "active"})
		// First checkpoint loses the connection, forcing the next checkpoint to reconnect.
		var checkpoint governorWireRequest
		_ = readGovernorFrame(server, &checkpoint)
		close(lost)
	}()
	_, _ = stdinWriter.Write([]byte("checkpoint\n"))
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("first checkpoint did not reach listener")
	}
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not reconnect in background")
	}
	_, _ = stdinWriter.Write([]byte("checkpoint\n"))
	select {
	case <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not reconnect")
	}
	_ = stdinWriter.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not exit")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "stable" || seen[1] != "stable" {
		t.Fatalf("re-acquire UUIDs = %#v", seen)
	}
}
