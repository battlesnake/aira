//go:build linux

package runner

import (
	"os"
	"testing"
)

func TestCurrentCgroupPathReturnsAbsolutePath(t *testing.T) {
	path, err := CurrentCgroupPath()
	if err != nil {
		SkipOrFailNoCgroup(t, err)
	}
	if path == "" || path[0] != '/' {
		t.Fatalf("path=%q, want an absolute path", path)
	}
}

func SkipOrFailNoCgroup(t *testing.T, err error) {
	if os.Getenv("AIRA_REAL_CGROUP") == "1" {
		t.Fatalf("cgroup-v2 required: %v", err)
	}
	t.Skipf("cgroup-v2 unavailable: %v", err)
}
