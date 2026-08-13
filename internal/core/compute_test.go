package core

import (
	"strings"
	"testing"

	"aira/internal/domain"
)

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
