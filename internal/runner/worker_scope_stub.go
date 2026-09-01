//go:build !linux

package runner

import (
	"context"
	"errors"
)

func CreateWorkerScope(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
	return "", errors.New("aitest worker scope: unsupported on this platform")
}
