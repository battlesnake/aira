package gitremote

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// armedDeadline is a context whose deadline the test fires by hand, once the
// precondition it must not race against has been observed. Err is derived from the
// same channel as Done, never from a flag written after the close: realRun reads
// ctx.Err() as soon as <-ctx.Done() fires, so Err must be non-nil the instant Done
// is closed. expire records WHICH event expired the context, and when, inside the
// same once that closes the channel, so exactly one cause wins and the time is the
// instant of the actual expiry.
type armedDeadline struct {
	context.Context
	done      chan struct{}
	once      sync.Once
	cause     string
	expiredAt time.Time
}

func newArmedDeadline() *armedDeadline {
	return &armedDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (a *armedDeadline) Done() <-chan struct{} { return a.done }

func (a *armedDeadline) Err() error {
	select {
	case <-a.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (a *armedDeadline) expire(cause string) {
	a.once.Do(func() {
		a.cause, a.expiredAt = cause, time.Now()
		close(a.done)
	})
}

// winner reports the cause that expired the context and when. Call it only after
// realRun has returned (which happens after Done closed), so the once has completed.
func (a *armedDeadline) winner() (string, time.Time) {
	a.once.Do(func() {}) // a no-op if expire already ran; otherwise marks "never expired"
	return a.cause, a.expiredAt
}

// firstLineTrigger expires the deadline, with cause "pid line", on the first live
// write carrying text. The live face sits behind lineRedactor, which releases a line
// only once it is complete and passes it through redact (whose URL pass trims the
// trailing newline), so a live write is a complete pid line without its terminator.
// The drain writes the captured tail before the live face, so by then the line is
// already in result.Stdout.
type firstLineTrigger struct {
	deadline *armedDeadline
}

func (w *firstLineTrigger) Write(p []byte) (int, error) {
	if len(bytes.TrimSpace(p)) > 0 {
		w.deadline.expire("pid line")
	}
	return len(p), nil
}

// verifies: cancellation signals the entire process group and has bounded shutdown.
//
// The subject under test is the cancellation, so its deadline is armed only after
// the precondition (sh started, trapped TERM, forked, printed both pids) is
// observed: a fixed 50 ms timeout armed at start was also the whole budget for that
// precondition, which a loaded box does not always meet.
func TestRealRunTimeoutKillsProcessGroup(t *testing.T) {
	deadline := newArmedDeadline()
	trigger := &firstLineTrigger{deadline: deadline}
	// Backstop: a child that never prints still ends the subject; the test then
	// fails below (the pid line did not win), it never passes.
	backstop := time.AfterFunc(testdeadline.Wait(5*time.Second), func() { deadline.expire("backstop") })
	defer backstop.Stop()
	// negative-wait fixture: the child-side delay before the trap makes a deadline
	// armed at start (the old shape) fail on every run instead of only under load.
	result := realRun(deadline, runRequest{
		Name: "/bin/sh", Args: []string{"-c", `sleep 0.2; trap '' TERM; sleep 30 & child=$!; printf '%s %s\n' "$$" "$child"; wait "$child"`}, Env: commandEnv(nil, time.Second),
		LiveStdout: trigger,
	})
	backstop.Stop()
	cause, expiredAt := deadline.winner()
	if cause != "pid line" {
		t.Fatalf("the context was expired by %q, not by the pid line: cancellation began before its precondition; result=%+v", cause, result)
	}
	if !errors.Is(result.Err, context.DeadlineExceeded) {
		t.Fatalf("result=%+v", result)
	}
	// realRun's worst case after expiry is three processGrace waits (TERM, KILL, then
	// the drains); the expected path is one (the trap ignores TERM, KILL reaps both).
	if elapsed := time.Since(expiredAt); testdeadline.Exceeded(elapsed, 3*processGrace+time.Second) {
		t.Fatalf("bounded cancellation took %s after the deadline", elapsed)
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) != 2 {
		t.Fatalf("pid output=%q", result.Stdout)
	}
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("pid output=%q error=%v", result.Stdout, err)
		}
		// An orphaned sleep is reaped by init or a subreaper, a latency this test
		// does not own.
		testdeadline.Eventually(t, time.Second, func() bool {
			return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		}, "process-group member %d remains", pid)
	}
}

func TestLiveAndPayloadOutputAreRedactedAndBounded(t *testing.T) {
	secret := "https://user:token@github.com/o/r.git ghp_abcdefghijklmnopqrstuvwxyz password=supersecret"
	if got := redact(secret); strings.Contains(got, "token") || strings.Contains(got, "ghp_") || strings.Contains(got, "supersecret") {
		t.Fatalf("redacted=%q", got)
	}
	var live bytes.Buffer
	w := &lineRedactor{dst: &live}
	_, _ = w.Write([]byte("remote " + secret[:35]))
	_, _ = w.Write([]byte(secret[35:] + "\n"))
	_ = w.Flush()
	if got := live.String(); strings.Contains(got, "token") || strings.Contains(got, "ghp_") || strings.Contains(got, "supersecret") {
		t.Fatalf("live=%q", got)
	}

	fake := goodFake("https://example.com/o/r.git")
	fake.op = runResult{Stdout: strings.Repeat("x", outputTailBytes+100), Stderr: secret}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.StdoutTail) != outputTailBytes || strings.Contains(result.StderrTail, "token") || strings.Contains(result.StderrTail, "supersecret") {
		t.Fatalf("result=%+v", result)
	}
}

// verifies: a seam that blocks until the shared deadline maps to E_GIT_TIMEOUT.
func TestSharedDeadlineCoversResolutionChild(t *testing.T) {
	client := newWithRun(Config{GhFallback: true, OpTimeout: 20 * time.Millisecond}, func(ctx context.Context, _ runRequest) runResult {
		<-ctx.Done()
		return runResult{ExitCode: -1, Err: ctx.Err()}
	})
	started := time.Now()
	_, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if errorCode(err) != CodeTimeout || time.Since(started) > time.Second {
		t.Fatalf("error=%v elapsed=%s", err, time.Since(started))
	}
}
