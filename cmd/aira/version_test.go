package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/buildid"
	"aira/internal/core"
	"aira/internal/daemon"
)

// TestVersionAnswersBothSpellings pins the two-registry trap that made the first
// draft of this fix wrong.
//
// The CLI raises E_UNKNOWN_VERB from cmd/aira/buildRequest's OWN enumerated
// switch (default arm, main.go), not from internal/core's dispatch table — the
// two are independent registries, and core.Do is never reached for a verb
// buildRequest does not know. So a fix that registers `version` in the dispatch
// table alone leaves `aira version` broken while `--version` works, or the
// reverse. Asserting only one spelling would pass against exactly that
// half-fix, which is why both are driven here.
//
// verifies: AIRA-202
func TestVersionAnswersBothSpellings(t *testing.T) {
	for _, argv := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := RunWithDispatcher(argv, &stdout, &stderr, versionDispatcher(t, buildid.Identity{
				Established: true, Revision: "4b751d3bfde79f5275a4d471963cf01726b9a764", Time: "2026-09-09T00:39:34Z",
			}))
			if exit != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
			}
			if stdout.Len() == 0 {
				t.Fatalf("no output for %v", argv)
			}
		})
	}
}

// TestVersionReportsClientAndDaemonSeparatelyAndFlagsDivergence is the point of
// the verb.
//
// RANT-25's actual hazard is not that the CLI is old — it is that the DAEMON is,
// because create/show/link/rant execute inside it, so the daemon's compiled-in
// internal/domain decides what is legal. RANT-21 and RANT-25 are both that: a
// P3 ticket refused by a daemon predating AIRA-170 while the files on disk
// already carried P3. Reporting one number for "aira" would have hidden exactly
// the split that mattered, so the two are separate fields and disagreement is
// stated rather than left for the reader to spot.
//
// verifies: AIRA-202
func TestVersionReportsClientAndDaemonSeparatelyAndFlagsDivergence(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher([]string{"version", "--json"}, &stdout, &stderr, versionDispatcher(t, buildid.Identity{
		Established: true, Revision: "0000000000000000000000000000000000000000", Time: "2026-09-01T00:00:00Z",
	}))
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	var envelope struct {
		Data buildid.Report `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout.String(), err)
	}
	if envelope.Data.Daemon.Revision != "0000000000000000000000000000000000000000" {
		t.Fatalf("daemon identity not carried: %+v", envelope.Data)
	}
	// The client half is this test binary, which is built in a linked worktree and
	// therefore genuinely unstamped -- so assert the RELATIONSHIP, not a literal.
	if envelope.Data.Client.Established && !envelope.Data.Diverged {
		t.Fatalf("client %q and daemon %q differ but diverged=false: %+v",
			envelope.Data.Client.Revision, envelope.Data.Daemon.Revision, envelope.Data)
	}
	if !envelope.Data.Client.Established && envelope.Data.Diverged {
		t.Fatalf("divergence must not be asserted when the client identity is unevaluated: %+v", envelope.Data)
	}
}

// TestVersionSurvivesAnUnreachableDaemon keeps the verb useful in the situation
// it is most needed.
//
// A stale or wedged daemon is exactly when someone asks what is running, so
// answering nothing at all -- or failing -- would defeat the verb. The client
// half is always establishable locally; the daemon half becomes `unevaluated`
// with its reason, and divergence is NOT asserted, because an unknown revision
// is not a differing one.
//
// verifies: AIRA-202
func TestVersionSurvivesAnUnreachableDaemon(t *testing.T) {
	var stdout, stderr bytes.Buffer
	down := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return core.Response{Code: daemon.CodeUnavailable, Error: daemon.CodeUnavailable + ": down", Exit: 4}
	})
	if exit := RunWithDispatcher([]string{"version", "--json"}, &stdout, &stderr, down); exit != 0 {
		t.Fatalf("exit=%d stderr=%q; the verb must answer even with the daemon down", exit, stderr.String())
	}
	var envelope struct {
		Data buildid.Report `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout.String(), err)
	}
	if envelope.Data.Daemon.Established {
		t.Fatalf("daemon identity must not be established with the daemon down: %+v", envelope.Data.Daemon)
	}
	if envelope.Data.Daemon.Reason == "" {
		t.Fatal("an unevaluated daemon identity must say why")
	}
	if envelope.Data.Diverged {
		t.Fatal("an unknown daemon revision is not a differing one; divergence must not be asserted")
	}
}

// TestVersionNeverPrintsAPlaceholder is the false-pass twin of the whole
// ticket. MCP's serverInfo answered "m8a" for roughly a thousand commits; a
// version surface that substitutes any placeholder for an absent stamp
// reintroduces precisely the defect.
//
// The assertion is on the RENDERED IDENTITY VALUES, not on the whole output:
// scanning the prose would false-positive on an explanatory reason (an earlier
// draft of this test tripped on "dev" inside "development builds"), and a test
// that fails for the wrong reason teaches the next reader to loosen it.
//
// verifies: AIRA-202
func TestVersionNeverPrintsAPlaceholder(t *testing.T) {
	var stdout, stderr bytes.Buffer
	down := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return core.Response{Code: daemon.CodeUnavailable, Exit: 4}
	})
	if exit := RunWithDispatcher([]string{"version", "--json"}, &stdout, &stderr, down); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var envelope struct {
		Data buildid.Report `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout.String(), err)
	}
	for name, identity := range map[string]buildid.Identity{"client": envelope.Data.Client, "daemon": envelope.Data.Daemon} {
		if identity.Established {
			continue
		}
		if identity.Revision != "" {
			t.Fatalf("%s: unestablished but carries revision %q", name, identity.Revision)
		}
		if rendered := identity.String(); rendered != buildid.Unevaluated {
			t.Fatalf("%s: rendered as %q, want %q", name, rendered, buildid.Unevaluated)
		}
	}

	// And the human render, which is the surface an operator actually reads.
	var human bytes.Buffer
	renderVersion(envelope.Data, &human)
	for _, line := range strings.Split(human.String(), "\n") {
		prefix, _, found := strings.Cut(line, ": ")
		if !found || (prefix != "client" && prefix != "daemon") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, prefix+": "))
		for _, placeholder := range []string{"m8a", "dev", "unknown", "v0.0.0", "HEAD", ""} {
			if value == placeholder {
				t.Fatalf("%s rendered the placeholder %q as its version", prefix, placeholder)
			}
		}
	}
}

func versionDispatcher(t *testing.T, daemonIdentity buildid.Identity) Dispatcher {
	t.Helper()
	return dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
		if request.Verb != "version" {
			t.Fatalf("dispatched verb=%q, want version", request.Verb)
		}
		return core.Response{OK: true, Code: "OK", Data: daemonIdentity}
	})
}

// TestRenderVersionStatesDivergenceWhenBothHalvesAreEstablished pins the render
// of the asserting direction, which no end-to-end test could reach: this test
// binary is built in a linked worktree, so its own client identity is never
// established and every Diverged==true branch was dead.
//
// The build review found the existing divergence test's only real guard was
// therefore unreachable — a test that passes but could not fail, which is the
// house failure mode. Driving the renderer directly fixes that.
//
// verifies: AIRA-202
func TestRenderVersionStatesDivergenceWhenBothHalvesAreEstablished(t *testing.T) {
	report := buildid.NewReport(
		buildid.Identity{Established: true, Revision: "aaaaaaa", Time: "2026-09-09T00:39:34Z"},
		buildid.Identity{Established: true, Revision: "bbbbbbb", Time: "2026-09-01T00:00:00Z"},
	)
	if !report.Diverged {
		t.Fatal("fixture must diverge, or this test asserts nothing")
	}
	var stdout bytes.Buffer
	renderVersion(report, &stdout)
	out := stdout.String()
	if !strings.Contains(out, "DIVERGED") {
		t.Fatalf("divergence must be stated, not left for the reader to spot:\n%s", out)
	}
	// The remedy matters as much as the fact: the daemon is what decides legality,
	// so a reader must be told to restart it rather than to reinstall the client.
	if !strings.Contains(out, "DAEMON") || !strings.Contains(out, "aira install") {
		t.Fatalf("the divergence line must name the daemon and the remedy:\n%s", out)
	}
	if !strings.Contains(out, "aaaaaaa") || !strings.Contains(out, "bbbbbbb") {
		t.Fatalf("both revisions must be shown so the reader can tell which is which:\n%s", out)
	}
}

// TestMCPServerInfoReportsTheRealBuildIdentity is the ticket's own HOW-TO-TEST
// item (4), which the first pass did not implement — the build review flagged it
// four separate times. Without it, reverting mcp.go to the literal "m8a" stays
// green, and that literal is the one surface that actually fabricated a version.
//
// verifies: AIRA-202
func TestMCPServerInfoReportsTheRealBuildIdentity(t *testing.T) {
	server := &mcpServer{}
	response, _ := server.handle(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	encoded, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var result struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	if want := buildid.Current().String(); result.ServerInfo.Version != want {
		t.Fatalf("serverInfo.version=%q, want the live build identity %q — a literal here is how %q survived a thousand commits",
			result.ServerInfo.Version, want, "m8a")
	}
}
