package domain

import (
	"errors"
	"fmt"
	"strings"

	"aira/internal/gitcontext"
)

const CommandCodeInvalid = "E_COMMAND_INVALID"

type CommandOutcome string

const (
	CommandExited       CommandOutcome = "exited"
	CommandSignalled    CommandOutcome = "signalled"
	CommandTimeout      CommandOutcome = "timeout"
	CommandLaunchFailed CommandOutcome = "launch-failed"
	CommandUnknown      CommandOutcome = "unknown"
)

type CommandKeySource string

const (
	CommandKeyLabel             CommandKeySource = "label"
	CommandKeyProgramSubcommand CommandKeySource = "program-subcommand"
	CommandKeyProgram           CommandKeySource = "program"
)

type CommandEvent struct {
	ID            string            `json:"id"`
	At            string            `json:"at"`
	AtSeq         int64             `json:"at_seq"`
	Key           string            `json:"key"`
	KeySource     CommandKeySource  `json:"key_source"`
	Program       string            `json:"program"`
	ArgvPreview   string            `json:"argv_preview"`
	ArgvDigest    string            `json:"argv_digest"`
	PrefixPreview string            `json:"prefix_preview"`
	Status        CommandOutcome    `json:"status"`
	ExitCode      *int64            `json:"exit_code,omitempty"`
	Signal        string            `json:"signal,omitempty"`
	WallMS        *int64            `json:"wall_ms,omitempty"`
	TicketID      string            `json:"ticket_id,omitempty"`
	Phase         string            `json:"phase,omitempty"`
	Actor         string            `json:"actor,omitempty"`
	Session       string            `json:"session,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	GitContext    CommandGitContext `json:"git_context"`
}

// CommandGitContext is the lean, status-preserving command provenance view.
// Stable repository scope is implied by project_id and Field.Reason is
// deliberately absent from high-volume command rows.
type CommandGitContext struct {
	HeadHash   gitcontext.Field `json:"head_hash"`
	HeadRef    gitcontext.Field `json:"head_ref"`
	WorktreeID gitcontext.Field `json:"worktree_id"`
}

func CommandGitContextFrom(value gitcontext.GitContext) CommandGitContext {
	field := func(input gitcontext.Field) gitcontext.Field {
		return gitcontext.Field{Value: input.Value, Status: input.Status}
	}
	return CommandGitContext{HeadHash: field(value.HeadHash), HeadRef: field(value.HeadRef), WorktreeID: field(value.WorktreeID)}
}

type CommandEventInput struct {
	At            string
	Key           string
	KeySource     CommandKeySource
	Program       string
	ArgvPreview   string
	ArgvDigest    string
	PrefixPreview string
	Status        CommandOutcome
	ExitCode      *int64
	Signal        string
	WallMS        *int64
	TicketID      string
	Phase         string
	Actor         string
	Session       string
	Cwd           string
	GitContext    gitcontext.GitContext
}

func (input CommandEventInput) Validate() error {
	if strings.TrimSpace(input.Key) == "" || strings.ContainsRune(input.Key, '\x00') {
		return errors.New(CommandCodeInvalid + ": command key is empty or contains NUL")
	}
	if strings.TrimSpace(input.Program) == "" || strings.ContainsRune(input.Program, '\x00') {
		return errors.New(CommandCodeInvalid + ": command program is empty or contains NUL")
	}
	switch input.KeySource {
	case CommandKeyLabel, CommandKeyProgramSubcommand, CommandKeyProgram:
	default:
		return fmt.Errorf("%s: invalid key source %q", CommandCodeInvalid, input.KeySource)
	}
	if err := ValidatePhase(input.Phase); err != nil {
		return err
	}
	if input.ExitCode != nil && *input.ExitCode < 0 {
		return errors.New(CommandCodeInvalid + ": exit code must be non-negative")
	}
	if input.WallMS != nil && *input.WallMS < 0 {
		return errors.New(CommandCodeInvalid + ": wall_ms must be non-negative")
	}
	switch input.Status {
	case CommandExited:
		if input.ExitCode == nil || input.Signal != "" || input.WallMS == nil {
			return errors.New(CommandCodeInvalid + ": exited requires exit_code and wall_ms and forbids signal")
		}
	case CommandSignalled, CommandTimeout:
		if input.ExitCode != nil || strings.TrimSpace(input.Signal) == "" || input.WallMS == nil {
			return errors.New(CommandCodeInvalid + ": signalled/timeout require signal and wall_ms and forbid exit_code")
		}
	case CommandLaunchFailed:
		if input.ExitCode != nil || input.Signal != "" || input.WallMS != nil {
			return errors.New(CommandCodeInvalid + ": launch-failed forbids exit_code, signal, and wall_ms")
		}
	case CommandUnknown:
		if input.ExitCode != nil || input.Signal != "" {
			return errors.New(CommandCodeInvalid + ": unknown forbids exit_code and signal")
		}
	default:
		return fmt.Errorf("%s: invalid command outcome %q", CommandCodeInvalid, input.Status)
	}
	return nil
}
