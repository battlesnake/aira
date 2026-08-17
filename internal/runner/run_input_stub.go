//go:build !linux

package runner

import "context"

func (r *Runner) Input(context.Context, RunInputRequest) (*RunInputResult, error) {
	return nil, nonLinuxRunError()
}
