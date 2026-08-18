//go:build !linux

package gitcontext

// readLooseRefBeneath uses the portable Lstat-walk guard plus an O_NOFOLLOW open
// on platforms without openat2. AIRA's release target is a static Linux binary;
// this keeps the package building and testing everywhere else.
func readLooseRefBeneath(root, ref string) ([]byte, bool, error) {
	return readLooseRefBeneathFallback(root, ref)
}
