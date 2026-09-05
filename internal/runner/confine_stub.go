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

// The detached-confine surface is Linux-only for the same reason confinement is:
// it exists to own a cgroup-v2 scope. Every stub refuses rather than pretending,
// so a non-Linux build cannot report a detached job it never started.

func LaunchConfineDetached(context.Context, ConfineRequest) (*ConfineDetachLaunch, error) {
	return nil, errors.New(CodeConfineDetachFailed + ": detached confinement is supported only on Linux")
}

func SuperviseConfineDetached(context.Context, string, int, int) error {
	return errors.New(CodeConfineDetachFailed + ": detached confinement is supported only on Linux")
}

func ListConfineDetachRecords(string) ([]ConfineDetachRecord, error) {
	return nil, errors.New(CodeConfineOutcomeUnknown + ": detached confinement is supported only on Linux")
}

func ConfineDetachStatusFor(string, string, string) (ConfineDetachStatus, error) {
	return ConfineDetachStatus{}, errors.New(CodeConfineOutcomeUnknown + ": detached confinement is supported only on Linux")
}

func ConfineDetachStatusList(string, string) ([]ConfineDetachStatus, error) {
	return nil, errors.New(CodeConfineOutcomeUnknown + ": detached confinement is supported only on Linux")
}
