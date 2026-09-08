package worktree

import (
	"context"
	"strings"

	"aira/internal/gitcontext"
)

// Capture is what `aira worktree register` observes about a checkout at the
// moment it is registered. Every field is a tri-state: a branch that could not
// be established is recorded as `none`, never as a plausible-looking default,
// because the audit later reads BaseRef back as one step of its integration-ref
// chain and a fabricated value there would silently steer every later verdict.
type Capture struct {
	Branch     gitcontext.Field
	BaseRef    gitcontext.Field
	BaseCommit gitcontext.Field
}

// CaptureRegistration resolves the branch, integration ref and merge-base for
// one checkout. It reuses the audit's own chain — steps 1, 3 and 4 — so a
// registration can never record a base the audit would refuse to accept. Step 2
// (a previously recorded base_ref) is deliberately not consulted: a
// registration is a fresh assertion, not a re-derivation of an older one.
//
// An unresolvable explicit --base is an ERROR, not an unevaluated field: the
// caller named a ref, and silently recording nothing would let a typo pass for
// a successful registration.
func CaptureRegistration(ctx context.Context, git GitCall, root, configRef, baseOverride string) (Capture, error) {
	auditor := &Auditor{Git: git}
	capture := Capture{}

	out, stderr, err := auditor.git()(ctx, root, "branch", "--show-current")
	switch {
	case err != nil:
		capture.Branch = fieldUnevaluated(gitFailure("branch --show-current", stderr, err))
	case strings.TrimSpace(out) == "":
		capture.Branch = fieldNone("detached HEAD")
	default:
		capture.Branch = fieldValue(strings.TrimSpace(out))
	}

	baseRef, refErr := auditor.resolveRepoIntegrationRef(ctx, Inputs{
		Root: root, ConfigRef: configRef, BaseOverride: baseOverride,
	})
	if refErr != nil {
		return Capture{}, refErr
	}
	capture.BaseRef = baseRef
	if baseRef.Status != gitcontext.StatusValue {
		capture.BaseCommit = fieldNone("no integration ref, so there is no merge-base to record")
		return capture, nil
	}

	base, baseStderr, baseErr := auditor.git()(ctx, root, "merge-base", baseRef.Value, "HEAD")
	if baseErr != nil {
		capture.BaseCommit = fieldUnevaluated(gitFailure("merge-base", baseStderr, baseErr))
		return capture, nil
	}
	if commit := strings.TrimSpace(base); commit != "" {
		capture.BaseCommit = fieldValue(commit)
	} else {
		capture.BaseCommit = fieldNone("git named no merge-base")
	}
	return capture, nil
}
