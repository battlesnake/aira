//go:build !linux

package runner

import (
	"context"
	"errors"
)

func BootstrapAitestSupervisor(ctx context.Context, outerScope string, supervisorPID int) (string, error) {
	return "", errors.New("aitest bootstrap: unsupported on this platform")
}
