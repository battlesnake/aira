package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken stdout") }

// flushWriter is a buffered stdout whose Flush can fail — modelling a real
// buffered CLI writer so the handle-before-ACK ordering exercises the Flush path.
type flushWriter struct {
	buf            bytes.Buffer
	flushed        bool
	flushErr       error
	contentAtFlush string
}

func (w *flushWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *flushWriter) Flush() error {
	w.flushed = true
	w.contentAtFlush = w.buf.String()
	return w.flushErr
}

func TestM20RendererFlushesHandleBeforeACK(t *testing.T) {
	for name, tc := range map[string]struct {
		flushErr  error
		delivered bool
	}{
		"flush-success": {flushErr: nil, delivered: true},
		"flush-failure": {flushErr: errors.New("flush failed"), delivered: false},
	} {
		t.Run(name, func(t *testing.T) {
			w := &flushWriter{flushErr: tc.flushErr}
			called := false
			response := core.Response{OK: true, Code: "OK", Data: runner.RunRecord{ID: "RUN-21", Status: runner.StatusStarting, Detached: true}}
			response.AfterWrite = func(delivered bool) error {
				called = true
				// The ACK/cancel decision must run only AFTER the handle is
				// flushed, and a failed flush is NOT a delivery (→ cancel).
				if !w.flushed {
					t.Fatal("completion hook ran before the handle was flushed")
				}
				if !strings.Contains(w.contentAtFlush, "RUN-21") {
					t.Fatalf("handle was not written before flush: %q", w.contentAtFlush)
				}
				if delivered != tc.delivered {
					t.Fatalf("delivered=%v want %v (flushErr=%v)", delivered, tc.delivered, tc.flushErr)
				}
				return nil
			}
			_ = render(response, false, w, &bytes.Buffer{})
			if !called {
				t.Fatal("completion hook was not called")
			}
		})
	}
}

func TestM20CLIMapsDetachAndFollow(t *testing.T) {
	target, options, err := parseArgs("run", []string{"--detach", "--follow", "--no-stdin", "--timeout", "2s", "--", "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("run", target, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["detach"] != true || request.Args["follow"] != true || request.Args["no_stdin"] != true || request.Args["timeout"] != "2s" {
		t.Fatalf("request=%#v", request)
	}
	if timeout, err := time.ParseDuration(request.Args["timeout"].(string)); err != nil || timeout != 2*time.Second {
		t.Fatalf("timeout=%s err=%v", timeout, err)
	}
}

func TestM20RendererPublishesHandleBeforeACKAndCancelsOnBrokenStdout(t *testing.T) {
	for name, tc := range map[string]struct {
		out       io.Writer
		delivered bool
	}{
		"success": {out: &bytes.Buffer{}, delivered: true},
		"broken":  {out: failingWriter{}, delivered: false},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			response := core.Response{OK: true, Code: "OK", Data: runner.RunRecord{ID: "RUN-20", Status: runner.StatusStarting, Detached: true}}
			response.AfterWrite = func(delivered bool) error {
				called = true
				if delivered != tc.delivered {
					t.Fatalf("delivered=%v want %v", delivered, tc.delivered)
				}
				if buffer, ok := tc.out.(*bytes.Buffer); ok && !strings.Contains(buffer.String(), "RUN-20") {
					t.Fatalf("ACK ran before handle write: %q", buffer.String())
				}
				return nil
			}
			_ = render(response, false, tc.out, &bytes.Buffer{})
			if !called {
				t.Fatal("completion hook was not called")
			}
		})
	}
}

func TestM20MCPFlushesHandleBeforeACK(t *testing.T) {
	for name, tc := range map[string]struct {
		flushErr  error
		delivered bool
	}{
		"flush-success": {flushErr: nil, delivered: true},
		"flush-failure": {flushErr: errors.New("flush failed"), delivered: false},
	} {
		t.Run(name, func(t *testing.T) {
			w := &flushWriter{flushErr: tc.flushErr}
			called := false
			resp := resultResponse(1, map[string]any{"id": "RUN-30", "status": "starting"})
			resp.afterWrite = func(delivered bool) error {
				called = true
				// The MCP handle must be flushed before the ACK/cancel decision,
				// and a failed flush is not a delivery (→ cancel), same as the CLI.
				if !w.flushed {
					t.Fatal("MCP completion hook ran before the handle was flushed")
				}
				if !strings.Contains(w.contentAtFlush, "RUN-30") {
					t.Fatalf("MCP handle not written before flush: %q", w.contentAtFlush)
				}
				if delivered != tc.delivered {
					t.Fatalf("delivered=%v want %v (flushErr=%v)", delivered, tc.delivered, tc.flushErr)
				}
				return nil
			}
			_ = writeMCP(w, resp)
			if !called {
				t.Fatal("MCP completion hook was not called")
			}
		})
	}
}

func TestM20DetachIsInMCPAndSkillButSupervisorIsHidden(t *testing.T) {
	server := newMCPServer(nil)
	binding, ok := server.byName["aira_run"]
	if !ok {
		t.Fatal("aira_run MCP tool is missing")
	}
	schema, ok := binding.tool.InputSchema.(mcpInputSchema)
	if !ok || schema.Properties["detach"].Type != "boolean" {
		t.Fatalf("detach schema=%#v", binding.tool.InputSchema)
	}
	if _, exists := server.byName["__supervise"]; exists {
		t.Fatal("internal supervisor leaked into MCP")
	}
	var help bytes.Buffer
	if exit := Run([]string{"help"}, &help, &bytes.Buffer{}); exit != 0 {
		t.Fatalf("help exit=%d", exit)
	}
	if strings.Contains(help.String(), "__supervise") {
		t.Fatalf("internal supervisor leaked into help: %s", help.String())
	}
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(artifacts.SkillMD, []byte("run --detach")) || bytes.Contains(artifacts.SkillMD, []byte("__supervise")) {
		t.Fatalf("skill detach/hidden contract failed: %s", artifacts.SkillMD)
	}
}
