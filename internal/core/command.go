package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"aira/internal/domain"
	"aira/internal/pylib"
	"aira/internal/runner"
	"aira/internal/store"

	"golang.org/x/sys/unix"
)

const commandWaitDelay = 250 * time.Millisecond

type commandTimingData struct {
	Command     any `json:"command"`
	ProcessExit int `json:"-"`
}

func responseForCommandTiming(timed commandTimingData, warnings []string, afterWrite func(bool) error) Response {
	return Response{OK: true, Code: "OK", Data: timed.Command, Exit: timed.ProcessExit, Warnings: warnings, AfterWrite: afterWrite}
}

type timedCommandResult struct {
	Status   domain.CommandOutcome
	ExitCode *int64
	Signal   string
	WallMS   *int64
	Exit     int
}

func prepareTimedCommand(ctx context.Context, argv []string, cwd string, env []string, live bool) (*exec.Cmd, func(), error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, func() {}, errors.New("E_RUN_ARGUMENT_INVALID: time target argv is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.WaitDelay = commandWaitDelay
	if live {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd, func() {}, nil
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, func() {}, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	return cmd, func() { _ = null.Close() }, nil
}

func runTimedCommand(parent context.Context, argv []string, cwd string, env []string, live bool, timeout time.Duration) timedCommandResult {
	ctx := parent
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	}
	defer cancel()
	cmd, closeStdio, err := prepareTimedCommand(ctx, argv, cwd, env, live)
	if err != nil {
		return timedCommandResult{Status: domain.CommandLaunchFailed, Exit: 127}
	}
	defer closeStdio()
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return timedCommandResult{Status: domain.CommandLaunchFailed, Exit: 127}
	}
	forward := make(chan os.Signal, 2)
	done := make(chan struct{})
	signal.Notify(forward, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for {
			select {
			case received := <-forward:
				_ = cmd.Process.Signal(received)
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	signal.Stop(forward)
	wall := time.Since(started).Milliseconds()
	if err == nil {
		code := int64(0)
		return timedCommandResult{Status: domain.CommandExited, ExitCode: &code, WallMS: &wall, Exit: 0}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if wait, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if wait.Signaled() {
				sig := wait.Signal()
				if timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return timedCommandResult{Status: domain.CommandTimeout, Signal: "KILL", WallMS: &wall, Exit: 124}
				}
				return timedCommandResult{Status: domain.CommandSignalled, Signal: shortSignalName(unix.SignalName(sig), sig.String()), WallMS: &wall, Exit: 128 + int(sig)}
			}
			code := int64(wait.ExitStatus())
			return timedCommandResult{Status: domain.CommandExited, ExitCode: &code, WallMS: &wall, Exit: int(code)}
		}
	}
	if timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timedCommandResult{Status: domain.CommandTimeout, Signal: "KILL", WallMS: &wall, Exit: 124}
	}
	return timedCommandResult{Status: domain.CommandUnknown, WallMS: &wall, Exit: 3}
}

// shortSignalName renders a signal in the short convention (SIGTERM→TERM,
// SIGKILL→KILL) matching the timeout branch's "KILL" and the fixtures, so one
// signal is one population. `named` is unix.SignalName(sig) (may be empty for a
// realtime/unnamed signal); only it is SIG-stripped. The `fallback`
// (sig.String(), e.g. "signal 34") is NOT stripped — trimming it would mangle
// "SIGNAL 34" into "NAL 34".
func shortSignalName(named, fallback string) string {
	if named != "" {
		return strings.TrimPrefix(named, "SIG")
	}
	return strings.ToUpper(fallback)
}

func commandEventFromInput(input domain.CommandEventInput) domain.CommandEvent {
	return domain.CommandEvent{At: input.At, Key: input.Key, KeySource: input.KeySource, Program: input.Program,
		ArgvPreview: input.ArgvPreview, ArgvDigest: input.ArgvDigest, PrefixPreview: input.PrefixPreview,
		Status: input.Status, ExitCode: cloneCommandInt64(input.ExitCode), Signal: input.Signal, WallMS: cloneCommandInt64(input.WallMS),
		TicketID: input.TicketID, Phase: input.Phase, Actor: input.Actor, Session: input.Session, Cwd: input.Cwd, GitContext: domain.CommandGitContextFrom(input.GitContext)}
}

func cloneCommandInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func effectiveCommandEnvironment(overrides []string, runtimeDir string, diagnostics io.Writer) ([]string, error) {
	values := append([]string(nil), os.Environ()...)
	positions := map[string]int{}
	for i, entry := range values {
		if key, _, ok := strings.Cut(entry, "="); ok {
			positions[key] = i
		}
	}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(entry, '\x00') {
			return nil, fmt.Errorf("E_RUN_ARGUMENT_INVALID: --env requires K=V without NUL")
		}
		if at, exists := positions[key]; exists {
			values[at] = entry
		} else {
			positions[key] = len(values)
			values = append(values, entry)
		}
	}
	return pylib.AppendChildEnvironment(values, runtimeDir, diagnostics), nil
}

type sidecarRuntimeRunner interface {
	SidecarRuntimeDir() string
}

func sidecarRuntimeDir(execution Runner) string {
	if source, ok := execution.(sidecarRuntimeRunner); ok {
		return source.SidecarRuntimeDir()
	}
	return ""
}

func commandArgvDigest(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:])
}

func commandPreview(argv []string) string {
	const maxTokens, maxBytes = 12, 256
	values := argv
	if len(values) > maxTokens {
		values = values[:maxTokens]
	}
	preview := strings.Join(values, " ")
	if len(argv) > maxTokens {
		preview += fmt.Sprintf(" …(+%d tokens)", len(argv)-maxTokens)
	}
	if len(preview) > maxBytes {
		preview = preview[:maxBytes]
		for !utf8.ValidString(preview) {
			preview = preview[:len(preview)-1]
		}
	}
	return preview
}

func normaliseCommandKey(argv []string, label string) (string, domain.CommandKeySource, string) {
	remaining := argv
	for len(remaining) > 0 {
		if commandAssignment(remaining[0]) {
			remaining = remaining[1:]
			continue
		}
		switch remaining[0] {
		case "timeout":
			if len(remaining) < 3 || strings.HasPrefix(remaining[1], "-") {
				break
			}
			remaining = remaining[2:]
			continue
		case "nice":
			if len(remaining) < 4 || remaining[1] != "-n" {
				break
			}
			remaining = remaining[3:]
			continue
		case "ionice":
			if len(remaining) < 4 || remaining[1] != "-c" {
				break
			}
			remaining = remaining[3:]
			continue
		case "stdbuf":
			i := 1
			for i+1 < len(remaining) && (remaining[i] == "-i" || remaining[i] == "-o" || remaining[i] == "-e") {
				i += 2
			}
			if i == 1 || i >= len(remaining) {
				break
			}
			remaining = remaining[i:]
			continue
		case "sudo":
			if len(remaining) > 1 && strings.HasPrefix(remaining[1], "-") {
				if len(remaining) < 4 || remaining[1] != "-u" {
					break
				}
				remaining = remaining[3:]
				continue
			}
			if len(remaining) < 2 {
				break
			}
			remaining = remaining[1:]
			continue
		case "env", "whale-run", "nohup":
			if len(remaining) < 2 {
				break
			}
			remaining = remaining[1:]
			continue
		}
		break
	}
	if len(remaining) == 0 {
		remaining = argv
	}
	program := ""
	if len(remaining) > 0 {
		program = filepath.Base(remaining[0])
	}
	if strings.TrimSpace(label) != "" {
		return program, domain.CommandKeyLabel, strings.TrimSpace(label)
	}
	drivers := map[string]bool{"go": true, "cargo": true, "git": true, "make": true, "npm": true, "pnpm": true, "yarn": true, "node": true, "python": true, "python3": true, "pytest": true, "docker": true, "kubectl": true}
	if drivers[program] {
		for _, token := range remaining[1:] {
			if !strings.HasPrefix(token, "-") {
				return program, domain.CommandKeyProgramSubcommand, program + " " + token
			}
		}
		return program, domain.CommandKeyProgram, program
	}
	return program, domain.CommandKeyProgram, program
}

func commandAssignment(token string) bool {
	name, _, ok := strings.Cut(token, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (c *Core) runCommandTime(ctx context.Context, args *argAccessor) (any, error) {
	target := append([]string(nil), stringSlice(args, "argv")...)
	if len(target) == 0 {
		return nil, runnerError("E_RUN_ARGUMENT_INVALID", errors.New("time target argv is empty"))
	}
	timeout, err := parseRunTimeout(stringArg(args, "timeout"))
	if err != nil {
		return nil, err
	}
	if err := domain.ValidatePhase(stringArg(args, "phase")); err != nil {
		return nil, err
	}
	selectedPrefix := append([]string(nil), c.commandPrefix...)
	if boolArg(args, "no_prefix") {
		selectedPrefix = nil
	} else if args.present("prefix") {
		selectedPrefix = append([]string(nil), stringSlice(args, "prefix")...)
	}
	effective, err := runner.EffectiveArgv(selectedPrefix, target)
	if err != nil {
		return nil, err
	}
	environment, err := effectiveCommandEnvironment(stringSlice(args, "env"), sidecarRuntimeDir(c.runner), c.face.Stderr)
	if err != nil {
		return nil, err
	}
	cwd := strings.TrimSpace(stringArg(args, "cwd"))
	if cwd == "" {
		cwd, err = os.Getwd()
	} else {
		cwd, err = filepath.Abs(cwd)
	}
	if err != nil {
		return nil, runnerError("E_RUN_ARGUMENT_INVALID", err)
	}
	program, source, key := normaliseCommandKey(target, stringArg(args, "label"))
	result := runTimedCommand(ctx, effective, cwd, environment, c.face.Live, timeout)
	input := domain.CommandEventInput{Key: key, KeySource: source, Program: program, ArgvPreview: commandPreview(target), ArgvDigest: commandArgvDigest(target), PrefixPreview: commandPreview(selectedPrefix), Status: result.Status, ExitCode: result.ExitCode, Signal: result.Signal, WallMS: result.WallMS, TicketID: stringArg(args, "ticket"), Phase: stringArg(args, "phase"), Actor: args.actor, Session: args.session, Cwd: cwd}
	if args.gitContext != nil {
		input.GitContext = *args.gitContext
	}
	added, addErr := c.store.AddCommandEvent(ctx, input)
	command := any(commandEventFromInput(input))
	warnings := []string(nil)
	if addErr != nil {
		warnings = append(warnings, "command telemetry: "+addErr.Error())
	} else {
		command = added.Event
	}
	return handlerData{Data: commandTimingData{Command: command, ProcessExit: result.Exit}, Warnings: warnings}, nil
}

func (c *Core) runCommands(ctx context.Context, args *argAccessor) (any, error) {
	subverb := strings.ToLower(strings.TrimSpace(stringArg(args, "subverb")))
	query, by := stringArg(args, "query"), stringArg(args, "by")
	switch subverb {
	case "ls", "list":
		countBy := by
		if countBy == "" {
			countBy = "status"
		}
		distribution, err := c.store.CommandDistribution(query, countBy)
		if err != nil {
			return nil, err
		}
		rows, err := c.store.ListCommandEvents(query)
		if err != nil {
			return nil, err
		}
		data := map[string]any{"total": distribution.Total, "rows": rows}
		if by != "" {
			data["distribution"] = distribution.Groups
		}
		if len(rows) > ListLimit {
			data["rows"], data["truncated"] = rows[:ListLimit], true
		}
		return data, nil
	case "count":
		if by == "" {
			return nil, errors.New("E_SELECTOR_INVALID: commands count requires --by")
		}
		return c.store.CommandDistribution(query, by)
	default:
		return nil, fmt.Errorf("E_UNKNOWN_VERB: unknown commands sub-verb %q", subverb)
	}
}

var _ = store.CommandEventAddResult{}
