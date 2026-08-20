//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	cpuSlotsDirName    = "cpuslots"
	cpuSlotsTempPrefix = ".cpuslots-"
)

func desiredCPUSlots(cpuCount int) (int, error) {
	reserve := 1
	if raw, present := os.LookupEnv("AIRA_DAEMON_CPU_RESERVE"); present && raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_CPU_RESERVE must be a non-negative integer")
		}
		reserve = value
	}
	count := cpuCount - reserve
	if count < 1 {
		count = 1
	}
	return count, nil
}

// ensureCPUSlots creates one immutable slot population. An existing complete
// population wins even when desired has changed; replacing live slot inodes
// would split flock holders into disjoint semaphore populations.
func ensureCPUSlots(runtimeDir string, desired int) (int, error) {
	if desired < 1 {
		return 0, errors.New("CPU slot count must be positive")
	}
	target := filepath.Join(runtimeDir, cpuSlotsDirName)
	if count, err := validateCPUSlots(target); err == nil {
		return count, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	temp, err := buildCPUSlotTemp(runtimeDir, desired, nil)
	if err != nil {
		if temp != "" {
			_ = os.RemoveAll(temp)
		}
		return 0, err
	}
	defer os.RemoveAll(temp)
	if err := unix.Renameat2(unix.AT_FDCWD, temp, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return 0, err
		}
		count, validateErr := validateCPUSlots(target)
		if validateErr != nil {
			return 0, fmt.Errorf("existing CPU slot population is incomplete: %w", validateErr)
		}
		return count, nil
	}
	if err := syncCPUSlotDir(runtimeDir); err != nil {
		return 0, err
	}
	return desired, nil
}

// buildCPUSlotTemp deliberately leaves its private directory on failure. The
// caller cleans ordinary errors; a test can model a process dying mid-build and
// prove that the only residue is unpublished own-prefix litter.
func buildCPUSlotTemp(runtimeDir string, count int, beforeCreate func(int) error) (string, error) {
	temp, err := os.MkdirTemp(runtimeDir, fmt.Sprintf("%s%d-", cpuSlotsTempPrefix, os.Getpid()))
	if err != nil {
		return "", err
	}
	if err := os.Chmod(temp, 0o700); err != nil {
		return temp, err
	}
	for index := 0; index < count; index++ {
		if beforeCreate != nil {
			if err := beforeCreate(index); err != nil {
				return temp, err
			}
		}
		path := filepath.Join(temp, fmt.Sprintf("slot-%d", index))
		fd, err := unix.Open(path, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return temp, err
		}
		if err := unix.Fsync(fd); err != nil {
			_ = unix.Close(fd)
			return temp, err
		}
		if err := unix.Close(fd); err != nil {
			return temp, err
		}
	}
	if err := syncCPUSlotDir(temp); err != nil {
		return temp, err
	}
	return temp, nil
}

func validateCPUSlots(directory string) (int, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, errors.New("CPU slot population is empty")
	}
	seen := make([]bool, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeType != 0 || !strings.HasPrefix(entry.Name(), "slot-") {
			return 0, fmt.Errorf("unexpected CPU slot entry %q", entry.Name())
		}
		index, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "slot-"))
		if err != nil || index < 0 || index >= len(entries) || entry.Name() != fmt.Sprintf("slot-%d", index) || seen[index] {
			return 0, fmt.Errorf("non-contiguous CPU slot entry %q", entry.Name())
		}
		seen[index] = true
	}
	for index, present := range seen {
		if !present {
			return 0, fmt.Errorf("missing CPU slot-%d", index)
		}
	}
	return len(entries), nil
}

func syncCPUSlotDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
