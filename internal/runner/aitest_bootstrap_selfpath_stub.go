//go:build !linux

package runner

import "errors"

func CurrentCgroupPath() (string, error) {
	return "", errors.New("cgroup discovery unsupported on this platform")
}
