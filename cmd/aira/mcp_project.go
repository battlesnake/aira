package main

import (
	"context"
	"io"

	"aira/internal/app"
	"aira/internal/core"
)

func runMCP(ctx context.Context, input io.Reader, output, diagnostics io.Writer) int {
	server := newMCPServer(func(requestContext context.Context, request core.Request) (*core.Core, func(), error) {
		if request.Verb == "init" {
			return core.NewWithInitializer(nil, func(initContext context.Context, args map[string]any) (any, error) {
				return app.Init(initContext, ".", args)
			}), func() {}, nil
		}
		s, _, err := app.Open(requestContext, ".")
		if err != nil {
			return nil, nil, err
		}
		return core.New(s), func() { _ = s.Close() }, nil
	})
	if err := server.Serve(ctx, input, output, diagnostics); err != nil {
		return 1
	}
	return 0
}
