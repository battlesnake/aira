package main

import (
	"context"
	"fmt"
	"strings"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// worktreeSubverbs is the closed set `aira worktree <subverb>` accepts. Keeping
// it here rather than inferring from the dispatch table means an unknown
// subverb is refused by NAME, with both valid forms printed, instead of falling
// through to a confusing "unknown verb worktree-frobnicate".
var worktreeSubverbs = map[string]bool{"register": true, "audit": true}

// canonicalWorktreeVerb rewrites `worktree <subverb> <rest...>` into
// `worktree-<subverb> <rest...>`.
func canonicalWorktreeVerb(args []string) ([]string, error) {
	if len(args) < 2 || strings.HasPrefix(args[1], "--") {
		return nil, fmt.Errorf("E_SELECTOR_INVALID: worktree requires a subverb: register or audit")
	}
	subverb := strings.ToLower(args[1])
	if !worktreeSubverbs[subverb] {
		return nil, fmt.Errorf("E_SELECTOR_INVALID: unknown worktree subverb %q: expected register or audit", args[1])
	}
	rewritten := append([]string{"worktree-" + subverb}, args[2:]...)
	return rewritten, nil
}

// stampWorktreeOwner resolves the caller's identity for `worktree register` and
// puts it on the request, alongside whether that identity is ATTESTED.
//
// This runs in the face, not in core, for the same reason `confine --list`'s
// owner does: the chain reads process environment and the caller's own
// directory, neither of which core may touch. Both faces that can reach this
// verb (CLI and MCP) call it with the RESOLVED SCOPE ROOT, so identity is
// always attributed to the checkout being registered.
//
// The attested/inferred split is load-bearing and is recorded, never enforced:
// a binding an unattested caller declared is weaker evidence, and the audit says
// so, rather than the write being refused. Refusing it would make it impossible
// to label the ~80 existing worktrees this feature exists for.
func stampWorktreeOwner(ctx context.Context, scope daemon.WorktreeScope, request *core.Request) error {
	if request == nil || core.CanonicalVerb(request.Verb) != "worktree-register" {
		return nil
	}
	if request.Args == nil {
		request.Args = map[string]any{}
	}
	explicit, _ := request.Args["owner"].(string)
	owner, err := resolveOwnerIn(ctx, explicit, scope.Root)
	if err != nil {
		return fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --owner: %w", err)
	}
	request.Args["owner"] = owner
	request.Args["owner_attested"] = runner.ConfineOwnerIsAttested(owner)
	return nil
}
