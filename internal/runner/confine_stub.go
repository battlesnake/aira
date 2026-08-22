//go:build !linux

package runner

import (
	"context"
	"errors"
	"io"
)

func confine(context.Context, ConfineRequest) (ConfineResult, error) {
	return ConfineResult{}, errors.New("E_CONFINE_UNAVAILABLE: cgroup-v2 confinement is supported only on Linux")
}

func RunConfineSetup([]string, io.Writer) int { return 127 }
