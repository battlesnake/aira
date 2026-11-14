//go:build linux

package runner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// AIRA-330: concurrent `aira confine` starts minted the delegation-probe child
// name from the clock alone, so starters in one coarse clock tick collided and
// the loser's mkdir refused the whole start as E_CONFINE_UNAVAILABLE. These
// tests drive the name+mkdir step without a real cgroup.

// forceProbeClock pins the probe clock to one instant and, when failRand is
// set, forces the random source to fail so the clock fallback is exercised.
func forceProbeClock(t *testing.T, failRand bool) {
	t.Helper()
	oldNow, oldRand := probeNow, probeRandRead
	fixed := time.Unix(1_700_000_000, 123)
	probeNow = func() time.Time { return fixed }
	if failRand {
		probeRandRead = func([]byte) (int, error) { return 0, errors.New("forced rand failure") }
	}
	t.Cleanup(func() { probeNow, probeRandRead = oldNow, oldRand })
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// verifies: AIRA-330 (a) concurrent starters at one forced timestamp all succeed
// with distinct names, on both the random path and the rand-failure fallback.
func TestProbeMkdirConcurrentSameTimestampAllSucceed(t *testing.T) {
	for _, failRand := range []bool{false, true} {
		t.Run(map[bool]string{false: "random", true: "rand-fallback"}[failRand], func(t *testing.T) {
			forceProbeClock(t, failRand)
			parent := t.TempDir()
			const n = 64
			var wg sync.WaitGroup
			start := make(chan struct{})
			paths := make([]string, n)
			errs := make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					paths[i], errs[i] = mkdirProbe(parent, os.Mkdir)
				}()
			}
			close(start)
			wg.Wait()
			seen := map[string]bool{}
			for i := range n {
				if errs[i] != nil {
					t.Fatalf("starter %d refused: %v", i, errs[i])
				}
				if seen[paths[i]] {
					t.Fatalf("starter %d reused name %s", i, paths[i])
				}
				seen[paths[i]] = true
			}
			if got := len(listDir(t, parent)); got != n {
				t.Fatalf("want %d probe dirs, got %d", n, got)
			}
		})
	}
}

// verifies: AIRA-330 (b) EEXIST below the bound retries with a fresh name and
// succeeds; EEXIST at the bound fails naming the path and saying it retried.
func TestProbeMkdirRetriesOnExist(t *testing.T) {
	parent := t.TempDir()
	for k := 0; k < probeMkdirAttempts; k++ {
		calls := 0
		var tried []string
		mkdir := func(p string, _ os.FileMode) error {
			calls++
			tried = append(tried, p)
			if calls <= k {
				return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
			}
			return nil
		}
		got, err := mkdirProbe(parent, mkdir)
		if err != nil {
			t.Fatalf("k=%d: want success after %d retries, got %v", k, k, err)
		}
		if calls != k+1 {
			t.Fatalf("k=%d: want %d mkdir calls, got %d", k, k+1, calls)
		}
		if got != tried[len(tried)-1] {
			t.Fatalf("k=%d: returned %s, last tried %s", k, got, tried[len(tried)-1])
		}
		for i := 1; i < len(tried); i++ {
			if tried[i] == tried[i-1] {
				t.Fatalf("k=%d: retry reused name %s", k, tried[i])
			}
		}
	}

	calls := 0
	var last string
	_, err := mkdirProbe(parent, func(p string, _ os.FileMode) error {
		calls++
		last = p
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	})
	if err == nil {
		t.Fatal("want failure when every attempt exists")
	}
	if calls != probeMkdirAttempts {
		t.Fatalf("want %d attempts, got %d", probeMkdirAttempts, calls)
	}
	if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), last) || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("exhausted error must wrap EEXIST, name %s and say it retried: %v", last, err)
	}
	// It still reaches the caller under the stable code.
	if msg := confineUnavailable("aira.slice", err).Error(); !strings.HasPrefix(msg, "E_CONFINE_UNAVAILABLE:") {
		t.Fatalf("not coded: %s", msg)
	}

	// A non-EEXIST error is not retried.
	calls = 0
	_, err = mkdirProbe(parent, func(p string, _ os.FileMode) error {
		calls++
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrPermission}
	})
	if err == nil || calls != 1 {
		t.Fatalf("permission error: want 1 call and failure, got %d calls, err %v", calls, err)
	}
}

// verifies: AIRA-330 (c) a pre-existing directory carrying the first name the
// generator would mint does not fail the start.
func TestProbeMkdirSurvivesPreexistingName(t *testing.T) {
	parent := t.TempDir()
	// Mint the next name by hand, rewind the counter so mkdirProbe mints the
	// same one first, and plant it as a stale probe.
	forceProbeClock(t, true) // deterministic: pid + counter + fixed clock
	before := probeSeq.Load()
	stale := newProbeName()
	probeSeq.Store(before)
	if err := os.Mkdir(filepath.Join(parent, stale), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := mkdirProbe(parent, os.Mkdir)
	if err != nil {
		t.Fatalf("stale probe refused the start: %v", err)
	}
	if filepath.Base(got) == stale {
		t.Fatalf("returned the stale directory %s", stale)
	}
}

// verifies: AIRA-330 (d) the probe child is removed on success and when the
// cgroup.kill open fails.
func TestProbeDelegationRemovesProbe(t *testing.T) {
	parent := t.TempDir()
	if err := probeDelegation(parent, os.Mkdir, func(string) error { return nil }); err != nil {
		t.Fatalf("success path: %v", err)
	}
	if left := listDir(t, parent); len(left) != 0 {
		t.Fatalf("success leaked %v", left)
	}
	// A plain directory has no cgroup.kill: the real opener fails.
	if err := probeDelegation(parent, os.Mkdir, openCgroupKill); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want cgroup.kill open failure, got %v", err)
	}
	if left := listDir(t, parent); len(left) != 0 {
		t.Fatalf("kill-open failure leaked %v", left)
	}
}

// verifies: AIRA-330 (e) names never collide across 10,000 sequential calls,
// even with the clock frozen and the random source failing.
func TestProbeNameUniqueSequential(t *testing.T) {
	for _, failRand := range []bool{false, true} {
		forceProbeClock(t, failRand)
		seen := make(map[string]bool, 10000)
		for i := range 10000 {
			n := newProbeName()
			if !strings.HasPrefix(n, ".aira-probe-") {
				t.Fatalf("name %q lost the prefix", n)
			}
			if seen[n] {
				t.Fatalf("failRand=%v: collision at call %d: %s", failRand, i, n)
			}
			seen[n] = true
		}
	}
}
