//go:build linux

package gitcontext

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// readLooseRefBeneath opens ref strictly beneath root with no symlink component
// followed, using openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) — which closes
// the whole class of intermediate-directory symlink swaps a check-then-open
// walk cannot. A symlink or escape surfaces as a read error (→ unevaluated),
// never a followed read. On a kernel without openat2 it falls back to the
// Lstat-walk guard plus an O_NOFOLLOW open.
func readLooseRefBeneath(root, ref string) ([]byte, bool, error) {
	dirfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer unix.Close(dirfd)
	how := &unix.OpenHow{Flags: uint64(unix.O_RDONLY | unix.O_CLOEXEC), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS}
	fd, err := unix.Openat2(dirfd, filepath.FromSlash(ref), how)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM):
			return readLooseRefBeneathFallback(root, ref)
		case errors.Is(err, os.ErrNotExist), errors.Is(err, unix.ENOTDIR):
			return nil, false, nil
		default: // ELOOP (symlink), EXDEV (escape), etc. — refuse.
			return nil, false, err
		}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root, filepath.FromSlash(ref)))
	defer file.Close()
	return readBoundedFromFile(file)
}

func readBoundedFromFile(file *os.File) ([]byte, bool, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("not a regular file")
	}
	const max = 16 << 20
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > max {
		return nil, false, errors.New("file exceeds resolver limit")
	}
	return bytes.Clone(data), true, nil
}
