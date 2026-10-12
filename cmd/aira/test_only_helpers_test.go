package main

import (
	"context"
	"io"

	"aira/internal/daemon"
)

// Test-only constructors and shortcuts over the production entry points.

func runWithInput(argv []string, stdout, stderr io.Writer, stdin io.Reader) int {
	return runWithInputDispatcher(argv, stdout, stderr, stdin, nil)
}

func (r *tuiRuntime) executeLaunchOnUI(launch executeLaunch) {
	if launch.Detached {
		if !r.detachedSubmitting.CompareAndSwap(false, true) {
			return
		}
		r.executeDetachedLaunchOnUIGuarded(launch)
		return
	}
	if !r.executeRunning.CompareAndSwap(false, true) {
		return
	}
	r.executeLaunchOnUIGuarded(launch)
}

func newTUIState(eventCapacity int) tuiState {
	return newTUIStateForViews(eventCapacity, allViews, dataViews)
}

func newTUIExecutor(ctx context.Context, dispatcher, executeDispatcher Dispatcher, scope daemon.WorktreeScope, workers int) *tuiExecutor {
	return newTUIExecutorWithWatch(ctx, dispatcher, executeDispatcher, scope, workers, true)
}
