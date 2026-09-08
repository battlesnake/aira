package worktree

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"aira/internal/gitcontext"
)

// GitCall is the one shape every git read in this package goes through, so a
// test can drive the whole classifier without a repository.
type GitCall func(ctx context.Context, dir string, args ...string) (stdout string, stderr string, err error)

const (
	// callTimeout bounds ONE git subprocess. It matches internal/app's own
	// discovery bound, and exists for the same reason: under load a git can
	// hang, and a credential-helper or fsmonitor GRANDCHILD can inherit git's
	// stdout and keep the read blocked for EOF long after git itself exited.
	callTimeout = 10 * time.Second
	// waitDelay force-closes such a lingering pipe after git exits, so a stuck
	// grandchild costs this much and not the whole audit.
	waitDelay = 3 * time.Second
	// DefaultBudget bounds the WHOLE audit. ~85 checkouts x ~6 subprocesses is
	// the real population here, and F3 of the plan review is right that an
	// unbounded sweep is one stuck grandchild away from hanging. When the budget
	// elapses the audit does NOT fail: every checkout it did not reach is
	// reported with unevaluated facts naming the elapsed budget, which is the
	// honest answer rather than either a partial list presented as complete or
	// an error that throws away the work already done.
	DefaultBudget = 5 * time.Minute
	// maxInferredSubjects bounds commit-message inference. A branch with more
	// unique commits than this still infers from the newest ones; the bound is
	// on how much output is parsed, not on correctness.
	maxInferredSubjects = 200
)

// ErrBudgetElapsed is returned by the audit's internal git wrapper once the
// whole-audit budget is gone.
var ErrBudgetElapsed = errors.New("audit time budget elapsed")

// ExitError reports a git subprocess that RAN and exited non-zero, carrying the
// code. It exists because `merge-base --is-ancestor` answers with its exit
// status — 0 is "yes", 1 is "no", and anything else is a failure that must be
// reported unevaluated rather than read as "no". Collapsing those two would be
// exactly the false-pass this design refuses: a broken git would silently mean
// "not merged" (safe) or, with the test inverted, "merged" (loses work).
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return "git exited " + strconv.Itoa(e.Code)
}

// GitExitCode reports the exit status of a git call that ran and failed.
func GitExitCode(err error) (int, bool) {
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code, true
	}
	var execExit *exec.ExitError
	if errors.As(err, &execExit) {
		return execExit.ExitCode(), true
	}
	return 0, false
}

// RunGit is the production GitCall.
//
// It pins the environment exactly the way internal/store's runGit does:
// gitcontext.ScrubbedEnvironment() plus LC_ALL=C/LANG=C. The scrub is
// load-bearing here, not hygiene — an inherited GIT_DIR OVERRIDES `-C <dir>`,
// so an audit run from a shell that exported one would silently classify a
// DIFFERENT repository's worktrees and recommend removing checkouts it never
// looked at (AIRA-93).
func RunGit(ctx context.Context, dir string, args ...string) (string, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	command := exec.CommandContext(callCtx, "git", append([]string{"-C", dir}, args...)...)
	command.Env = append(gitcontext.ScrubbedEnvironment(), "LC_ALL=C", "LANG=C")
	command.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil && callCtx.Err() != nil && ctx.Err() == nil {
		// Distinguish "this one call timed out" from "the caller cancelled".
		err = context.DeadlineExceeded
	}
	return stdout.String(), stderr.String(), err
}

// checkout is the raw `git worktree list --porcelain` record, before any
// identity resolution or classification.
type checkout struct {
	path     string
	head     string
	branch   string
	bare     bool
	detached bool
	locked   bool
	prunable bool
}

// parseWorktreeList parses `git worktree list --porcelain`.
//
// The porcelain form emits `branch refs/heads/<name>` and `HEAD <sha>` per
// worktree already, so branch and HEAD cost no extra subprocess at all — worth
// having across ~85 checkouts, where per-worktree subprocess work should be
// reserved for facts that genuinely need it.
func parseWorktreeList(output string) []checkout {
	var checkouts []checkout
	var current *checkout
	flush := func() {
		if current != nil && current.path != "" {
			checkouts = append(checkouts, *current)
		}
		current = nil
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			current = &checkout{path: strings.TrimSpace(value)}
		case "HEAD":
			if current != nil {
				current.head = strings.TrimSpace(value)
			}
		case "branch":
			if current != nil {
				current.branch = strings.TrimSpace(value)
			}
		case "bare":
			if current != nil {
				current.bare = true
			}
		case "detached":
			if current != nil {
				current.detached = true
			}
		case "locked":
			if current != nil {
				current.locked = true
			}
		case "prunable":
			if current != nil {
				current.prunable = true
			}
		}
	}
	flush()
	return checkouts
}

// shortBranch turns `refs/heads/aira176-x` into `aira176-x`. A ref that is not
// under refs/heads is returned unchanged rather than mangled.
func shortBranch(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// gitFailure renders a git failure into a reason a reader can act on, without
// ever letting an unbounded stderr into the report.
func gitFailure(what string, stderr string, err error) string {
	detail := strings.TrimSpace(firstLine(stderr))
	if detail == "" {
		detail = err.Error()
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return what + ": " + detail
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}
