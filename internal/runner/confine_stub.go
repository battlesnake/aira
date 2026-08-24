//go:build !linux

package runner

import (
	"context"
	"errors"
	"io"
)

func confine(_ context.Context, request ConfineRequest) (ConfineResult, error) {
	slice := ResolveConfineSlice(request.Slice)
	if slice == "" {
		slice = DefaultConfineSlice
	}
	return ConfineResult{Status: ConfineStatus{Slice: slice}}, errors.New("E_CONFINE_UNAVAILABLE: cgroup-v2 confinement is supported only on Linux")
}

func RunConfineSetup([]string, io.Writer) int { return 127 }
