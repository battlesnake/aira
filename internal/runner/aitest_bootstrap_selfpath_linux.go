//go:build linux

package runner

// CurrentCgroupPath returns the caller's own current cgroup-v2 path. Exported
// for cmd/aira's aitest-bootstrap verb, which cannot reach the package-private
// currentCgroupPath/unifiedMount used throughout this file.
func CurrentCgroupPath() (string, error) {
	mount, err := unifiedMount()
	if err != nil {
		return "", err
	}
	return currentCgroupPath(mount)
}
