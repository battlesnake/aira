//go:build !linux

package runner

import (
	"context"
	"errors"
)

type unavailableBackend struct{}

func newDefaultBackend(string) ScopeBackend { return unavailableBackend{} }
func (unavailableBackend) Probe(context.Context) error {
	return errors.New("cgroup-v2 runner is supported only on Linux")
}
func (unavailableBackend) Create(context.Context, string) (Scope, error) {
	return nil, errors.New("cgroup-v2 runner is supported only on Linux")
}
func (unavailableBackend) Open(context.Context, string) (Scope, error) {
	return nil, errors.New("cgroup-v2 runner is supported only on Linux")
}
