package pylib

import (
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	embeddedRoot = "aira_xdist_governor"
	readyName    = ".ready"
	tempPrefix   = ".tmp-"
)

// all: is load-bearing: Python packages and the extraction hygiene file begin
// with '_' and '.', which a plain directory embed would omit.
//
//go:embed all:aira_xdist_governor
var embeddedPyLib embed.FS

// ExtractPyLib publishes the embedded Python sidecar beneath a content hash.
// AIRA distributes these bytes but never imports or executes them.
func ExtractPyLib() (string, error) {
	dataHome, err := dataHomeFromEnv()
	if err != nil {
		return "", err
	}
	return extractPyLibFS(embeddedPyLib, embeddedRoot, dataHome)
}

func dataHomeFromEnv() (string, error) {
	dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Abs(dataHome)
}

func extractPyLibFS(source fs.FS, sourceRoot, dataHome string) (string, error) {
	digest, err := treeDigest(source, sourceRoot)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dataHome, "aira", "pylib")
	target := filepath.Join(root, digest)
	if readyTree(target, digest) {
		return target, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(root, fmt.Sprintf("%s%d-", tempPrefix, os.Getpid()))
	if err != nil {
		return "", err
	}
	defer func() {
		if temp != "" {
			_ = os.RemoveAll(temp)
		}
	}()
	if err := os.Chmod(temp, 0o700); err != nil {
		return "", err
	}
	if err := extractTree(source, sourceRoot, temp); err != nil {
		return "", err
	}
	if err := syncTreeDirs(temp); err != nil {
		return "", err
	}
	if err := writeSyncedFile(filepath.Join(temp, readyName), []byte(digest+"\n")); err != nil {
		return "", err
	}
	if err := syncDir(temp); err != nil {
		return "", err
	}

	err = unix.Renameat2(unix.AT_FDCWD, temp, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
	if err == nil {
		temp = ""
		if err := syncDir(root); err != nil {
			return "", err
		}
		return target, nil
	}
	if !errors.Is(err, unix.EEXIST) {
		return "", err
	}
	if !readyTree(target, digest) {
		return "", fmt.Errorf("published Python sidecar %s is incomplete", target)
	}
	return target, nil
}

func treeDigest(source fs.FS, sourceRoot string) (string, error) {
	hash := sha256.New()
	var length [binary.MaxVarintLen64]byte
	err := fs.WalkDir(source, sourceRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		kind := byte('f')
		if entry.IsDir() {
			kind = 'd'
		}
		_, _ = hash.Write([]byte{kind})
		n := binary.PutUvarint(length[:], uint64(len(relative)))
		_, _ = hash.Write(length[:n])
		_, _ = hash.Write([]byte(relative))
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		n = binary.PutUvarint(length[:], uint64(len(data)))
		_, _ = hash.Write(length[:n])
		_, _ = hash.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func extractTree(source fs.FS, sourceRoot, targetRoot string) error {
	return fs.WalkDir(source, sourceRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(filepath.Dir(sourceRoot), name)
		if err != nil {
			return err
		}
		destination := filepath.Join(targetRoot, relative)
		if entry.IsDir() {
			return os.Mkdir(destination, 0o700)
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		return writeSyncedFile(destination, data)
	})
}

func writeSyncedFile(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncTreeDirs(root string) error {
	dirs := make([]string, 0)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readyTree(target, digest string) bool {
	data, err := os.ReadFile(filepath.Join(target, readyName))
	return err == nil && strings.TrimSpace(string(data)) == digest
}
