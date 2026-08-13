package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/domain"
)

func coreComputeI64(value int64) *int64 { return &value }

func TestSpendInputRejectsPayloadBucketsAndDuplicateBuckets(t *testing.T) {
	_, err := usageArgs(newArgAccessor(map[string]any{"raw": []byte(`{"output_tokens":1}`), "bucket": []string{"output=1"}, "reasoning-subset": false}), "anthropic")
	if err == nil || !strings.HasPrefix(err.Error(), domain.ComputeCodeInvalid) {
		t.Fatalf("payload plus buckets error = %v", err)
	}
	_, err = usageArgs(newArgAccessor(map[string]any{"bucket": []string{"output=1", "output=2"}, "reasoning-subset": false}), "mystery")
	if err == nil || !strings.HasPrefix(err.Error(), domain.ComputeCodeInvalid) {
		t.Fatalf("duplicate buckets error = %v", err)
	}
}

func TestSpendLSJSONPreservesAbsentAndExplicitZeroBuckets(t *testing.T) {
	s := coreTestStore(t)
	if _, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{Output: coreComputeI64(0)}},
	}); err != nil {
		t.Fatal(err)
	}
	response := New(s).Do(context.Background(), Request{Verb: "spend", Args: map[string]any{"subverb": "ls"}})
	if !response.OK {
		t.Fatalf("spend ls response=%#v", response)
	}
	var data struct {
		Rows []domain.ComputeEvent `json:"rows"`
	}
	marshalRoundTrip(t, response.Data, &data)
	if len(data.Rows) != 2 {
		t.Fatalf("spend ls rows=%#v", data.Rows)
	}
	if data.Rows[0].Buckets.Output == nil || *data.Rows[0].Buckets.Output != 0 {
		t.Fatalf("explicit zero row=%#v", data.Rows[0])
	}
	if data.Rows[1].Buckets.Output != nil {
		t.Fatalf("absent row=%#v", data.Rows[1])
	}
	zeroJSON, err := json.Marshal(data.Rows[0])
	if err != nil {
		t.Fatal(err)
	}
	absentJSON, err := json.Marshal(data.Rows[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(zeroJSON), `"output":0`) || strings.Contains(string(absentJSON), `"output"`) {
		t.Fatalf("nullable JSON zero=%s absent=%s", zeroJSON, absentJSON)
	}
}
