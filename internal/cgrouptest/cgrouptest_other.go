//go:build !linux

// Package cgrouptest provides shared test-support for real cgroup-v2 tests. Real
// cgroup delegation exists only on Linux; on other platforms the helper skips —
// unless AIRA_REAL_CGROUP=1 demands real containment, which cannot be honoured here.
package cgrouptest

import (
	"os"
	"testing"
)

// IsolatedScopeParent skips the calling test: real cgroup-v2 scope isolation is
// Linux-only. Under AIRA_REAL_CGROUP=1 (mandatory-real mode) it hard-fails instead
// of skipping, so asking for real containment on a platform that cannot provide it
// is never a silent pass. The signature matches the Linux build so cross-platform
// test builds compile.
func IsolatedScopeParent(t *testing.T) string {
	t.Helper()
	SkipOrFailRealCgroup(t, "real cgroup-v2 tests require linux")
	return ""
}

// SkipOrFailRealCgroup mirrors the Linux policy for non-Linux callers.
func SkipOrFailRealCgroup(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("AIRA_REAL_CGROUP") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
