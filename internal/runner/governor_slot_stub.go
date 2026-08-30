//go:build !linux

package runner

func setGovernorParentDeathSignal() bool { return false }
