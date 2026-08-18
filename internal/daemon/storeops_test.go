package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/domain"
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
		Payload: payloadForTest(t, TestReportStoreOpPayload{Input: reportInput, RawPresent: true}),
	}
	reportResponse := exchangeStoreOpOverPipe(t, server, reportFrame)
	if !reportResponse.OK {
		t.Fatalf("report response = %+v", reportResponse)
	}
	var compact RelayedTestReportResult
	if err := json.Unmarshal(reportResponse.Data, &compact); err != nil {
		t.Fatal(err)
	}
	if compact.ReportID != "TR-1" || compact.Counts.Pass != 1 || len(compact.Suite.Results) != 0 || !compact.TestsGreenObserved {
		t.Fatalf("compact report = %+v", compact)
	}

	large := int64(1<<53 + 1)
	zero := int64(0)
	computeInput := domain.ComputeEventInput{Model: "gpt", Provider: "openai", Source: "manual", Raw: domain.RawUsage{
		PromptTokens: &large, CachedTokens: &zero, CompletionTokens: &zero, TotalTokens: &large,
	}}
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
	if err != nil || len(reports) != 1 || reports[0].ID != compact.ReportID || len(reports[0].Results) != 1 {
		t.Fatalf("persisted reports=%+v err=%v", reports, err)
	}
	computeRows, err := view.ListComputeEvents("")
	if err != nil || len(computeRows) != 1 || computeRows[0].Buckets.FreshInput == nil || *computeRows[0].Buckets.FreshInput != large {
		t.Fatalf("persisted compute=%+v err=%v", computeRows, err)
	}
	commandRows, err := view.ListCommandEvents("")
	if err != nil || len(commandRows) != 1 || commandRows[0].WallMS == nil || *commandRows[0].WallMS != large {
		t.Fatalf("persisted commands=%+v err=%v", commandRows, err)
	}
}

func TestNilRawReportUsesExplicitNonemptyMarker(t *testing.T) {
	server, scope := storeOpTestServer(t)
	input := domain.TestReportInput{
		Format: "junit", Commit: "abc", Branch: "main", WorktreeID: scope.WorktreeID,
		SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1", ParserComplete: false,
		SourceDigest: strings.Repeat("a", 64), PreserveEmptyProvenance: true,
	}
	response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{
		Proto: ProtocolVersion, Scope: scope, Op: "add-test-report", BodyLen: 1, Body: []byte{nilReportBodyMarker},
		Payload: payloadForTest(t, TestReportStoreOpPayload{Input: input, RawPresent: false}),
	})
	if !response.OK {
		t.Fatalf("metadata-only response = %+v", response)
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

func TestDaemonOwnedHeavyDeadlineFiresAndReleasesOperation(t *testing.T) {
	server, scope := storeOpTestServer(t)
	server.storeOpHeavyTimeout = 20 * time.Millisecond
	var active atomic.Int32
	server.storeOpRun = func(ctx context.Context, _ *store.Store, frame StoreOpFrame) (any, error) {
		if frame.Op == "check" {
			active.Add(1)
			defer active.Add(-1)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return map[string]bool{"stored": true}, nil
	}
	response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{Proto: ProtocolVersion, Scope: scope, Op: "check"})
	if response.Code != CodeTimeout || !strings.Contains(response.Error, "unevaluated") || active.Load() != 0 {
		t.Fatalf("deadline response=%+v active=%d", response, active.Load())
	}
	appendResponse := exchangeStoreOpOverPipe(t, server, StoreOpFrame{
		Proto: ProtocolVersion, Scope: scope, Op: "add-command-event", Payload: json.RawMessage(`{}`),
	})
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
