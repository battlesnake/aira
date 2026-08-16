package main

import (
	"context"
	"io"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
)

const mcpOutputCap = 64 * 1024

func runMCP(ctx context.Context, input io.Reader, output, diagnostics io.Writer) int {
	return runMCPWithDispatcher(ctx, input, output, diagnostics, nil)
}

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
	server.dispatch = func(requestContext context.Context, request core.Request) core.Response {
		var scope daemon.WorktreeScope
		var scopeErr error
		if core.CanonicalVerb(request.Verb) == "init" {
			project, discoverErr := app.DiscoverBootstrap(requestContext, ".")
			if discoverErr != nil {
				scopeErr = discoverErr
			} else {
				scope = bootstrapScope(project, paths)
			}
		} else {
			scope, scopeErr = scopeForCWD(requestContext, ".", paths)
		}
		if scopeErr != nil {
			code := store.ErrorCode(scopeErr)
			return core.Response{Code: code, Error: scopeErr.Error(), Exit: store.ExitForCode(code)}
		}
		if err := prepareImportContent(&request); err != nil {
			code := store.ErrorCode(err)
			return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		}
		response := dispatcher.Dispatch(requestContext, scope, request)
		if core.CanonicalVerb(request.Verb) == "init" {
			relativiseInitResponse(&response, ".")
		}
		return response
	}
	if err := server.Serve(ctx, input, output, diagnostics); err != nil {
		return 1
	}
	return 0
}
