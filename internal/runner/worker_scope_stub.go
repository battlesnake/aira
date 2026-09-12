//go:build !linux

package runner

import (
	"context"
	"errors"
)

func CreateWorkerScope(ctx context.Context, parent, scopeName string, memoryMax int64) (string, string, error) {
	return "", "", errors.New("aitest worker scope: unsupported on this platform")
}
