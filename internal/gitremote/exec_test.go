package gitremote

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// verifies: cancellation signals the entire process group and has bounded shutdown.
func TestRealRunTimeoutKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := realRun(ctx, runRequest{
		Name: "/bin/sh", Args: []string{"-c", `trap '' TERM; sleep 30 & child=$!; printf '%s %s\n' "$$" "$child"; wait "$child"`}, Env: commandEnv(nil, time.Second),
	})
	if !errors.Is(result.Err, context.DeadlineExceeded) {
		t.Fatalf("result=%+v", result)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("bounded cancellation took %s", elapsed)
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
		deadline := time.Now().Add(time.Second)
		for {
			err = syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("process-group member %d remains: %v", pid, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
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
