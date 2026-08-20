//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCPUReserveFromEnvAndFloor(t *testing.T) {
	tests := []struct {
		name, value string
		set         bool
		cpus, want  int
		wantErr     bool
	}{
		{name: "default", cpus: 8, want: 7},
		{name: "zero", set: true, value: "0", cpus: 8, want: 8},
		{name: "reserve", set: true, value: "3", cpus: 8, want: 5},
		{name: "floor", set: true, value: "99", cpus: 8, want: 1},
		{name: "malformed", set: true, value: "many", cpus: 8, wantErr: true},
		{name: "negative", set: true, value: "-1", cpus: 8, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.set {
				t.Setenv("AIRA_DAEMON_CPU_RESERVE", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_CPU_RESERVE")
			}
			got, err := desiredCPUSlots(test.cpus)
			if test.wantErr {
				if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
					t.Fatalf("slots=%d err=%v", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("slots=%d want=%d err=%v", got, test.want, err)
			}
		})
	}
}

func TestEnsureCPUSlotsPublishesCompleteSetAndNeverResizes(t *testing.T) {
	runtimeDir := t.TempDir()
	effective, err := ensureCPUSlots(runtimeDir, 4)
	if err != nil || effective != 4 {
		t.Fatalf("first ensure effective=%d err=%v", effective, err)
	}
	target := filepath.Join(runtimeDir, cpuSlotsDirName)
	if got, err := validateCPUSlots(target); err != nil || got != 4 {
		t.Fatalf("published set=%d err=%v", got, err)
	}
	locked, err := os.OpenFile(filepath.Join(target, "slot-3"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if err := unix.Flock(int(locked.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before, err := locked.Stat()
	if err != nil {
		t.Fatal(err)
	}
	effective, err = ensureCPUSlots(runtimeDir, 2)
	if err != nil || effective != 4 {
		t.Fatalf("second ensure effective=%d err=%v", effective, err)
	}
	after, err := os.Stat(filepath.Join(target, "slot-3"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("live slot was replaced: before=%v after=%v err=%v", before, after, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(runtimeDir, cpuSlotsTempPrefix+"*")); len(matches) != 0 {
		t.Fatalf("loser temps leaked: %v", matches)
	}
}

func TestCPUSlotBuildAbortLeavesNoPublishedPartialSet(t *testing.T) {
	runtimeDir := t.TempDir()
	temp, err := buildCPUSlotTemp(runtimeDir, 4, func(index int) error {
		if index == 2 {
			return errors.New("injected crash boundary")
		}
		return nil
	})
	if err == nil {
		t.Fatal("build unexpectedly succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(runtimeDir, cpuSlotsDirName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial target was visible: %v", statErr)
	}
	entries, readErr := os.ReadDir(temp)
	if readErr != nil || len(entries) != 2 {
		t.Fatalf("private crash temp entries=%v err=%v", entries, readErr)
	}
	if err := os.RemoveAll(temp); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCPUSlotsObserverSeesAbsentOrComplete(t *testing.T) {
	runtimeDir := t.TempDir()
	target := filepath.Join(runtimeDir, cpuSlotsDirName)
	stop := make(chan struct{})
	failures := make(chan error, 1)
	var observer sync.WaitGroup
	observer.Add(1)
	go func() {
		defer observer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			count, err := validateCPUSlots(target)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || count != 64 {
				select {
				case failures <- fmt.Errorf("observer count=%d err=%v", count, err):
				default:
				}
				return
			}
		}
	}()
	_, err := ensureCPUSlots(runtimeDir, 64)
	close(stop)
	observer.Wait()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

func TestServeCreatesCPUSlotsBeforeReadyAndDegradesOnFailure(t *testing.T) {
	t.Run("before ready", func(t *testing.T) {
		paths := testPaths(t)
		t.Setenv("AIRA_DAEMON_CPU_RESERVE", strconv.Itoa(runtime.NumCPU()-2))
		server := NewServer(paths)
		_, _ = startServer(t, server)
		if got, err := validateCPUSlots(filepath.Join(paths.RuntimeDir, cpuSlotsDirName)); err != nil || got != 2 {
			t.Fatalf("ready exposed slots=%d err=%v", got, err)
		}
	})
	t.Run("creation failure is advisory", func(t *testing.T) {
		paths := testPaths(t)
		server := NewServer(paths)
		server.ensureCPUSlotsFn = func(string, int) (int, error) {
			return 0, errors.New("injected slot failure")
		}
		_, _ = startServer(t, server)
	})
	t.Run("malformed reserve is advisory", func(t *testing.T) {
		paths := testPaths(t)
		t.Setenv("AIRA_DAEMON_CPU_RESERVE", "many")
		server := NewServer(paths)
		_, _ = startServer(t, server)
	})
}

func TestCPUSlotSemaphoreCapsIndependentProcesses(t *testing.T) {
	if os.Getenv("AIRA_CPU_SLOT_HELPER") == "cap" {
		runCPUSlotCapHelper()
		return
	}
	runtimeDir := t.TempDir()
	if _, err := ensureCPUSlots(runtimeDir, 2); err != nil {
		t.Fatal(err)
	}
	start := filepath.Join(runtimeDir, "start")
	counter := filepath.Join(runtimeDir, "counter")
	if err := os.WriteFile(counter, []byte("0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 0, 8)
	for range 8 {
		command := exec.Command(os.Args[0], "-test.run=^TestCPUSlotSemaphoreCapsIndependentProcesses$")
		command.Env = append(os.Environ(),
			"AIRA_CPU_SLOT_HELPER=cap",
			"AIRA_CPU_SLOT_DIR="+filepath.Join(runtimeDir, cpuSlotsDirName),
			"AIRA_CPU_SLOT_START="+start,
			"AIRA_CPU_SLOT_COUNTER="+counter,
		)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	if err := os.WriteFile(start, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper failed: %v", err)
		}
	}
	current, maximum, err := readCPUSlotCounter(counter)
	if err != nil || current != 0 || maximum > 2 || maximum < 2 {
		t.Fatalf("counter current=%d max=%d err=%v", current, maximum, err)
	}
}

func TestCPUSlotFlockAutoReleasesAfterSIGKILL(t *testing.T) {
	if os.Getenv("AIRA_CPU_SLOT_HELPER") == "crash" {
		runCPUSlotCrashHelper()
		return
	}
	runtimeDir := t.TempDir()
	if _, err := ensureCPUSlots(runtimeDir, 1); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(runtimeDir, cpuSlotsDirName, "slot-0")
	marker := filepath.Join(runtimeDir, "locked")
	command := exec.Command(os.Args[0], "-test.run=^TestCPUSlotFlockAutoReleasesAfterSIGKILL$")
	command.Env = append(os.Environ(), "AIRA_CPU_SLOT_HELPER=crash", "AIRA_CPU_SLOT_FILE="+slot, "AIRA_CPU_SLOT_MARKER="+marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("helper did not acquire slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	probe, err := os.OpenFile(slot, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("slot was not locked by helper")
	}
	if err := command.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("slot stayed locked after SIGKILL: %v", err)
	}
}

func runCPUSlotCapHelper() {
	for {
		if _, err := os.Stat(os.Getenv("AIRA_CPU_SLOT_START")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	file, err := acquireTestCPUSlot(os.Getenv("AIRA_CPU_SLOT_DIR"), 5*time.Second)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	if err := updateCPUSlotCounter(os.Getenv("AIRA_CPU_SLOT_COUNTER"), 1); err != nil {
		panic(err)
	}
	time.Sleep(75 * time.Millisecond)
	if err := updateCPUSlotCounter(os.Getenv("AIRA_CPU_SLOT_COUNTER"), -1); err != nil {
		panic(err)
	}
}

func runCPUSlotCrashHelper() {
	file, err := os.OpenFile(os.Getenv("AIRA_CPU_SLOT_FILE"), os.O_RDWR, 0)
	if err != nil {
		panic(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Getenv("AIRA_CPU_SLOT_MARKER"), []byte("locked"), 0o600); err != nil {
		panic(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func acquireTestCPUSlot(directory string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			file, err := os.OpenFile(filepath.Join(directory, entry.Name()), os.O_RDWR, 0)
			if err != nil {
				return nil, err
			}
			if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
				return file, nil
			}
			_ = file.Close()
		}
		time.Sleep(time.Millisecond)
	}
	return nil, errors.New("timed out acquiring test CPU slot")
}

func updateCPUSlotCounter(path string, delta int) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	current, maximum, err := scanCPUSlotCounter(file)
	if err != nil {
		return err
	}
	current += delta
	if current > maximum {
		maximum = current
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "%d %d\n", current, maximum)
	return err
}

func readCPUSlotCounter(path string) (int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	return scanCPUSlotCounter(file)
}

func scanCPUSlotCounter(file *os.File) (int, int, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return 0, 0, err
	}
	var current, maximum int
	_, err := fmt.Fscan(file, &current, &maximum)
	return current, maximum, err
}
