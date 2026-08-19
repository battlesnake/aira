package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/store"
)

func storeOpTestServer(t *testing.T) (*Server, WorktreeScope) {
	t.Helper()
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	return server, testScope(t, paths, "relay")
}

func payloadForTest(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := encodeStoreOpPayload(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestStoreOpAppendRoundTripPersistsValueFaithfully(t *testing.T) {
	server, scope := storeOpTestServer(t)
	raw := []byte(`<testsuite tests="1"><testcase classname="pkg" name="TestOne"/></testsuite>`)
	reportInput := domain.TestReportInput{
		Format: "junit", Commit: "abc", Branch: "main", WorktreeID: scope.WorktreeID,
		SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", PreserveEmptyProvenance: true,
	}
	reportFrame := StoreOpFrame{
		Proto: ProtocolVersion, Scope: scope, Op: "add-test-report", BodyLen: uint64(len(raw)), Body: raw,
		Payload: payloadForTest(t, TestReportStoreOpPayload{Input: reportInput}),
	}
	reportResponse := exchangeStoreOpOverPipe(t, server, reportFrame)
	if !reportResponse.OK {
		t.Fatalf("report response = %+v", reportResponse)
	}
	var compact RelayedTestReportResult
	if err := json.Unmarshal(reportResponse.Data, &compact); err != nil {
		t.Fatal(err)
	}
	if compact.Header.ID != "TR-1" || compact.Counts.Pass != 1 || !compact.Header.ParserComplete || strings.Contains(string(reportResponse.Data), `"results"`) {
		t.Fatalf("compact report = %+v", compact)
	}

	large := int64(1<<53 + 1)
	zero := int64(0)
	computeInput := domain.ComputeEventInput{Model: "gpt", Provider: "openai", Source: "manual", Raw: domain.RawUsage{
		PromptTokens: &large, CachedTokens: &zero, CompletionTokens: &zero, TotalTokens: &large,
	}}
	computeInput.GitContext = gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: scope.Root, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: scope.Root, Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: "abc123", Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Status: gitcontext.StatusNone},
		WorktreeID:   gitcontext.Field{Value: "client-worktree", Status: gitcontext.StatusValue},
	}
	computeResponse := exchangeStoreOpOverPipe(t, server, StoreOpFrame{
		Proto: ProtocolVersion, Scope: scope, Op: "add-compute-event", Payload: payloadForTest(t, computeInput),
	})
	if !computeResponse.OK {
		t.Fatalf("compute response = %+v", computeResponse)
	}
	var computeResult store.ComputeEventAddResult
	if err := json.Unmarshal(computeResponse.Data, &computeResult); err != nil {
		t.Fatal(err)
	}
	if computeResult.Event.Buckets.FreshInput == nil || *computeResult.Event.Buckets.FreshInput != large {
		t.Fatalf("compute int64 lost: %+v", computeResult.Event.Buckets)
	}
	if computeResult.Event.GitContext.HeadHash.Value != "abc123" || computeResult.Event.GitContext.HeadHash.Status != gitcontext.StatusMismatch ||
		computeResult.Event.GitContext.HeadRef.Status != gitcontext.StatusNone || computeResult.Event.GitContext.WorktreeID.Value != "client-worktree" ||
		computeResult.Event.GitContext.WorktreeID.Status != gitcontext.StatusMismatch {
		t.Fatalf("compute git context was lost or not daemon-cross-checked: %+v", computeResult.Event.GitContext)
	}

	commandInput := domain.CommandEventInput{
		Key: "go test", KeySource: domain.CommandKeyProgramSubcommand, Program: "go",
		ArgvDigest: strings.Repeat("a", 64), Status: domain.CommandExited, ExitCode: &zero, WallMS: &large,
	}
	commandResponse := exchangeStoreOpOverPipe(t, server, StoreOpFrame{
		Proto: ProtocolVersion, Scope: scope, Op: "add-command-event", Payload: payloadForTest(t, commandInput),
	})
	if !commandResponse.OK {
		t.Fatalf("command response = %+v", commandResponse)
	}
	var commandResult store.CommandEventAddResult
	if err := json.Unmarshal(commandResponse.Data, &commandResult); err != nil {
		t.Fatal(err)
	}
	if commandResult.Event.WallMS == nil || *commandResult.Event.WallMS != large {
		t.Fatalf("command int64 lost: %+v", commandResult.Event)
	}

	view, _, err := server.storeForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	reports, err := view.ListTestReports("")
	if err != nil || len(reports) != 1 || reports[0].ID != compact.Header.ID || len(reports[0].Results) != 1 {
		t.Fatalf("persisted reports=%+v err=%v", reports, err)
	}
	computeRows, err := view.ListComputeEvents("")
	if err != nil || len(computeRows) != 1 || computeRows[0].Buckets.FreshInput == nil || *computeRows[0].Buckets.FreshInput != large ||
		!reflect.DeepEqual(computeRows[0].GitContext, computeResult.Event.GitContext) {
		t.Fatalf("persisted compute=%+v err=%v", computeRows, err)
	}
	commandRows, err := view.ListCommandEvents("")
	if err != nil || len(commandRows) != 1 || commandRows[0].WallMS == nil || *commandRows[0].WallMS != large {
		t.Fatalf("persisted commands=%+v err=%v", commandRows, err)
	}
}

func TestNilRawReportUsesLegalEmptyBody(t *testing.T) {
	server, scope := storeOpTestServer(t)
	input := domain.TestReportInput{
		TicketID: "AIRA-7", Phase: "test", Format: "junit", Commit: "abc", Branch: "main", WorktreeID: scope.WorktreeID,
		Agent: "terra", Session: "gate-fix", At: "2026-08-18T12:00:00Z", RunRef: "RUN-7",
		SuiteID: "unit", Runner: "go", Config: "default", EnvDigest: "env", ParserComplete: false,
		Coverage:     &domain.Coverage{Pct: float64Pointer(87.5)},
		SourceDigest: strings.Repeat("a", 64), PreserveEmptyProvenance: true,
	}
	response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{
		Proto: ProtocolVersion, Scope: scope, Op: "add-test-report",
		Payload: payloadForTest(t, TestReportStoreOpPayload{Input: input}),
	})
	if !response.OK {
		t.Fatalf("metadata-only response = %+v", response)
	}
	var relayed RelayedTestReportResult
	if err := json.Unmarshal(response.Data, &relayed); err != nil {
		t.Fatal(err)
	}
	if relayed.Header.Shard != "1/1" || relayed.Header.ParserComplete || strings.Contains(string(response.Data), `"results"`) {
		t.Fatalf("normalised metadata-only header = %+v", relayed.Header)
	}
	view, _, err := server.storeForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := view.ListTestReports("")
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored reports=%+v err=%v", stored, err)
	}
	wantHeader := stored[0]
	wantHeader.Results = nil
	if got := relayed.Header.TestReport(); !reflect.DeepEqual(got, wantHeader) {
		t.Fatalf("relayed header=%+v want stored header=%+v", got, wantHeader)
	}
	want := input
	want.Shard = "1/1"
	if relayed.Header.TicketID != want.TicketID || relayed.Header.Phase != want.Phase || relayed.Header.Commit != want.Commit ||
		relayed.Header.Branch != want.Branch || relayed.Header.WorktreeID != want.WorktreeID || relayed.Header.Agent != want.Agent ||
		relayed.Header.Session != want.Session || relayed.Header.At != want.At || relayed.Header.RunRef != want.RunRef ||
		relayed.Header.SuiteID != want.SuiteID || relayed.Header.Runner != want.Runner || relayed.Header.Config != want.Config ||
		relayed.Header.EnvDigest != want.EnvDigest || relayed.Header.Format != want.Format || relayed.Header.SourceDigest != want.SourceDigest ||
		relayed.Header.Coverage == nil || relayed.Header.Coverage.Pct == nil || *relayed.Header.Coverage.Pct != *want.Coverage.Pct ||
		relayed.Header.ID == "" || relayed.Header.AtSeq < 1 || relayed.Remaining != 1 {
		t.Fatalf("relayed header omitted stored fields: %+v", relayed)
	}
}

func float64Pointer(value float64) *float64 { return &value }

func TestReconcileAndRebuildAreSeparateBodylessStoreOps(t *testing.T) {
	server, scope := storeOpTestServer(t)
	var called []string
	server.storeOpRun = func(_ context.Context, _ *store.Store, frame StoreOpFrame) (any, error) {
		called = append(called, frame.Op)
		return nil, nil
	}
	for _, op := range []string{"reconcile", "rebuild"} {
		response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{Proto: ProtocolVersion, Scope: scope, Op: op})
		if !response.OK || len(response.Data) != 0 {
			t.Fatalf("%s response = %+v", op, response)
		}
	}
	if strings.Join(called, ",") != "reconcile,rebuild" {
		t.Fatalf("called ops = %v", called)
	}
}

func TestBoundedCheckReportKeepsExactCountsAndBoundedEncoding(t *testing.T) {
	full := store.CheckReport{Verdict: "fail", Dimensions: map[string]string{"traceability": "fail"}}
	for i := 0; i < MaxRelayedFindings+37; i++ {
		full.Findings = append(full.Findings, store.CheckFinding{
			Code: "E_TRACE_DANGLING", Subject: strings.Repeat("subject", 100), Message: strings.Repeat("message", 100), Kind: "fail",
		})
	}
	full.Warnings = []store.CheckFinding{{Code: "W_TRACE_UNCOVERED", Kind: "warning"}}
	bounded := boundedCheckReport(full)
	if bounded.Verdict != full.Verdict || bounded.Dimensions["traceability"] != "fail" ||
		bounded.FindingCounts["findings"] != uint(len(full.Findings)) || bounded.FindingCounts["warnings"] != 1 ||
		len(bounded.Findings) != MaxRelayedFindings || bounded.FindingsOmitted != 38 || bounded.FindingsTruncated == 0 {
		t.Fatalf("bounded report = %+v", bounded)
	}
	encoded, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= MaxFrameBytes {
		t.Fatalf("bounded report encoded to %d bytes, frame max %d", len(encoded), MaxFrameBytes)
	}
}

func TestCompactTestReportResultEchoesStoredHeaderAndBoundsResults(t *testing.T) {
	metadata := strings.Repeat("suite", 300)
	stored := domain.TestReport{
		ID: "TR-1", TicketID: "AIRA-1", Phase: "verify", Commit: "abc", Branch: "main", WorktreeID: "worktree",
		Agent: "terra", Session: "session", At: "2026-08-18T12:00:00Z", RunRef: "RUN-1", SuiteID: metadata,
		Runner: "go", Config: "cfg", EnvDigest: "env", Shard: "1/1", RetryIndex: 2, ParserComplete: true,
		Coverage: &domain.Coverage{Pct: float64Pointer(91.5)}, Format: "go-json", SourceDigest: strings.Repeat("a", 64), AtSeq: 9, Pinned: true,
		Results: []domain.TestResult{{Name: strings.Repeat("r", MaxFrameBytes), Outcome: domain.OutcomePass}},
	}
	result := compactTestReportResult(store.TestReportAddResult{
		ID: "TR-1", EvictedCount: 2, Remaining: 7, Report: stored,
	})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= MaxFrameBytes || result.Counts.Pass != 1 || result.Header.SuiteID != metadata || strings.Contains(string(encoded), `"results"`) ||
		result.Header.ID != "TR-1" || result.Header.AtSeq != 9 || !result.Header.Pinned || result.Evicted != 2 || result.Remaining != 7 {
		t.Fatalf("compact bytes=%d result=%+v", len(encoded), result)
	}
	stored.Results = nil
	if got := result.Header.TestReport(); !reflect.DeepEqual(got, stored) {
		t.Fatalf("header=%+v want=%+v", got, stored)
	}
}

func TestDaemonOwnedHeavyDeadlineFiresAndReleasesOperation(t *testing.T) {
	server, scope := storeOpTestServer(t)
	server.storeOpHeavyTimeout = 20 * time.Millisecond
	var active atomic.Int32
	started := make(chan struct{})
	released := make(chan struct{})
	server.storeOpRun = func(ctx context.Context, _ *store.Store, frame StoreOpFrame) (any, error) {
		if frame.Op == "check" {
			active.Add(1)
			close(started)
			defer func() {
				active.Add(-1)
				close(released)
			}()
			<-ctx.Done()
			return nil, ctx.Err()
		}
		<-released
		return map[string]bool{"stored": true}, nil
	}
	checkDone := make(chan ResponseFrame, 1)
	go func() {
		checkDone <- exchangeStoreOpOverPipe(t, server, StoreOpFrame{Proto: ProtocolVersion, Scope: scope, Op: "check"})
	}()
	<-started
	appendDone := make(chan ResponseFrame, 1)
	go func() {
		appendDone <- exchangeStoreOpOverPipe(t, server, StoreOpFrame{
			Proto: ProtocolVersion, Scope: scope, Op: "add-command-event", Payload: json.RawMessage(`{}`),
		})
	}()
	response := <-checkDone
	if response.Code != CodeTimeout || !strings.Contains(response.Error, "unevaluated") || active.Load() != 0 {
		t.Fatalf("deadline response=%+v active=%d", response, active.Load())
	}
	appendResponse := <-appendDone
	if !appendResponse.OK || active.Load() != 0 {
		t.Fatalf("writer remained pinned: response=%+v active=%d", appendResponse, active.Load())
	}
}

func TestExchangeStoreOpPostWriteDisconnectIsOutcomeUnknown(t *testing.T) {
	if os.Getenv("AIRA_REAL_SOCKET") != "1" {
		t.Skip("real Unix socket requires AIRA_REAL_SOCKET=1")
	}
	paths := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.SocketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := netListenUnix(paths.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var accepted atomic.Int32
	done := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted.Add(1)
			var frame StoreOpFrame
			_ = readFrame(conn, &frame)
			_ = conn.Close()
		}
		close(done)
	}()
	_, err = ExchangeStoreOp(context.Background(), paths.SocketPath, StoreOpFrame{Proto: ProtocolVersion, Op: "ensure-scope"})
	<-done
	if !IsStoreOpOutcomeUnknown(err) || accepted.Load() != 1 {
		t.Fatalf("err=%v accepted=%d", err, accepted.Load())
	}
}

func netListenUnix(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

func TestDecodeStoreOpPayloadRejectsTrailingJSON(t *testing.T) {
	var input domain.CommandEventInput
	err := decodeStoreOpPayload(json.RawMessage(`{} {}`), &input)
	if err == nil || !strings.HasPrefix(err.Error(), CodeProtocol+":") {
		t.Fatalf("trailing payload error = %v", err)
	}
}
