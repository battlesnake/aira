package main

import (
	"context"
	"fmt"
	"io"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/store"
)

const mcpOutputCap = 64 * 1024

func runMCPWithDispatcher(ctx context.Context, input io.Reader, output, diagnostics io.Writer, injected Dispatcher) int {
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		return 1
	}
	dispatcher := injected
	if dispatcher == nil {
		production, createErr := newDaemonDispatcher(input, io.Discard, diagnostics, true)
		if createErr != nil {
			return 1
		}
		production.outputCap = mcpOutputCap
		dispatcher = production
	}
	server := newMCPServer(nil)
	server.dispatch = func(requestContext context.Context, request core.Request, scopeDirOverride string) core.Response {
		var scope daemon.WorktreeScope
		var scopeErr error
		// An MCP request carries no working directory of its own, so without an
		// explicit override every call is scoped against this server process's
		// directory — which is wherever the host launched it, not the worktree
		// the caller is working in (AIRA-82).
		scopeDir, scopeDirErr := resolveScopeDir(scopeDirOverride)
		if scopeDirErr != nil {
			code := store.ErrorCode(scopeDirErr)
			return core.Response{Code: code, Error: scopeDirErr.Error(), Exit: store.ExitForCode(code)}
		}
		canonical := core.CanonicalVerb(request.Verb)
		if canonical == "init" {
			project, discoverErr := app.DiscoverBootstrap(requestContext, scopeDir)
			if discoverErr != nil {
				scopeErr = discoverErr
			} else {
				scope = bootstrapScope(project, paths)
			}
		} else if canonical == "eject" {
			// Eject is a machine-level daemon operation; its safety checks are
			// performed by the daemon and it has no project scope to discover.
			scope = daemon.WorktreeScope{}
		} else if canonical == "confine-list" || canonical == "confine-kill" {
			// Confine management is machine-local and project-less. Ownership,
			// destructive confirmation, and populated-gate checks remain in the
			// management handler; this bypasses project discovery only.
			scope = daemon.WorktreeScope{}
			owner, ownerErr := resolveConfineOwner(requestContext, stringRequestArg(request.Args, "owner"))
			if ownerErr != nil {
				scopeErr = fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --owner: %w", ownerErr)
			} else {
				if request.Args == nil {
					request.Args = map[string]any{}
				}
				request.Args["owner"] = owner
				request.Args["slice"] = runner.ResolveConfineSlice(stringRequestArg(request.Args, "slice"))
			}
		} else {
			scope, scopeErr = scopeForCWD(requestContext, scopeDir, paths)
		}
		if scopeErr != nil {
			code := store.ErrorCode(scopeErr)
			return core.Response{Code: code, Error: scopeErr.Error(), Exit: store.ExitForCode(code)}
		}
		if err := refuseAmbiguousImportPath(request, scopeDirOverride); err != nil {
			code := store.ErrorCode(err)
			return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		}
		if err := prepareImportContent(&request); err != nil {
			code := store.ErrorCode(err)
			return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		}
		response := dispatcher.Dispatch(requestContext, scope, request)
		if canonical == "init" {
			relativiseInitResponse(&response, scopeDir)
		}
		return response
	}
	if err := server.Serve(ctx, input, output, diagnostics); err != nil {
		return 1
	}
	return 0
}

func stringRequestArg(args map[string]any, name string) string {
	value, _ := args[name].(string)
	return value
}
