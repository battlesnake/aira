package gitremote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const processGrace = 2 * time.Second

// realRun gives every subprocess its own process group, drains both pipes
// concurrently, and performs bounded TERM -> KILL -> reap cancellation.
func realRun(ctx context.Context, request runRequest) runResult {
	if err := ctx.Err(); err != nil {
		return runResult{ExitCode: -1, Err: err}
	}
	cmd := exec.Command(request.Name, request.Args...)
	cmd.Env, cmd.Dir = request.Env, request.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if request.Stdin != "" {
		cmd.Stdin = bytes.NewBufferString(request.Stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runResult{ExitCode: -1, Err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return runResult{ExitCode: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return runResult{ExitCode: -1, Err: err}
	}

	var outTail, errTail tailWriter
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); drain(stdout, &outTail, request.LiveStdout) }()
	go func() { defer drains.Done(); drain(stderr, &errTail, request.LiveStderr) }()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case waitErr = <-waited:
		case <-time.After(processGrace):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case waitErr = <-waited:
			case <-time.After(processGrace):
				waitErr = ctx.Err()
			}
		}
		waitErr = ctx.Err()
	}
	drained := make(chan struct{})
	go func() { drains.Wait(); close(drained) }()
	drainComplete := false
	select {
	case <-drained:
		drainComplete = true
	case <-time.After(processGrace):
	}
	if !drainComplete {
		return runResult{ExitCode: -1, Err: errors.New("subprocess output drain did not finish")}
	}
	exit := 0
	if waitErr != nil {
		exit = -1
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitCode()
			waitErr = nil
		}
	}
	return runResult{ExitCode: exit, Stdout: outTail.String(), Stderr: errTail.String(), Err: waitErr, StdoutTruncated: outTail.truncated, StderrTruncated: errTail.truncated}
}

func drain(src io.Reader, captured *tailWriter, live io.Writer) {
	if live == nil {
		_, _ = io.Copy(captured, src)
		return
	}
	redactor := &lineRedactor{dst: live}
	_, _ = io.Copy(drainWriter{captured: captured, live: redactor}, src)
	_ = redactor.Flush()
}

type drainWriter struct {
	captured io.Writer
	live     io.Writer
}

func (w drainWriter) Write(p []byte) (int, error) {
	_, _ = w.captured.Write(p)
	_, _ = w.live.Write(p)
	return len(p), nil
}

type tailWriter struct {
	data      []byte
	truncated bool
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= outputTailBytes {
		w.truncated = true
		w.data = append(w.data[:0], p[len(p)-outputTailBytes:]...)
		return len(p), nil
	}
	if over := len(w.data) + len(p) - outputTailBytes; over > 0 {
		w.truncated = true
		copy(w.data, w.data[over:])
		w.data = w.data[:len(w.data)-over]
	}
	w.data = append(w.data, p...)
	return len(p), nil
}
func (w *tailWriter) String() string { return string(w.data) }

// lineRedactor withholds one logical line so URL userinfo and token-shaped
// material are redacted before any live face sees it.
type lineRedactor struct {
	dst        io.Writer
	pending    []byte
	suppressed bool
}

func (w *lineRedactor) Write(p []byte) (int, error) {
	original := len(p)
	if w.suppressed {
		idx := bytes.IndexAny(p, "\r\n")
		if idx < 0 {
			return original, nil
		}
		if _, err := io.WriteString(w.dst, "[aira git: long output line suppressed]"+string(p[idx:idx+1])); err != nil {
			return 0, err
		}
		w.suppressed = false
		p = p[idx+1:]
	}
	w.pending = append(w.pending, p...)
	for {
		idx := bytes.IndexAny(w.pending, "\r\n")
		if idx < 0 {
			break
		}
		chunk := string(w.pending[:idx+1])
		if _, err := io.WriteString(w.dst, redact(chunk)); err != nil {
			return 0, err
		}
		w.pending = w.pending[idx+1:]
	}
	if len(w.pending) > outputTailBytes {
		w.pending = nil
		w.suppressed = true
	}
	return original, nil
}
func (w *lineRedactor) Flush() error {
	if w.suppressed {
		_, err := io.WriteString(w.dst, "[aira git: long output line suppressed]")
		w.suppressed = false
		return err
	}
	_, err := io.WriteString(w.dst, redact(string(w.pending)))
	w.pending = nil
	return err
}
