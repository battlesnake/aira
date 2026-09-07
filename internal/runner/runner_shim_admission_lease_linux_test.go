//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// AIRA-141. The coverage gap AIRA-129's own review named: no test drove
// launchShim through a REAL daemon grant far enough to observe what happens to
// the lease after the child starts, which is exactly why an early release
// shipped unnoticed. These two tests drive that path and observe the lease
// itself, not a proxy for it.

// shimAdmitLease is a fake daemon that answers one `admit` and then reports,
// through `released`, the moment the client lets the lease go.
//
// The lease IS the connection: admitThroughDaemon returns the socket as
// admissionResult.release, and releaseAdmission() closes it. Nothing else is
// ever written on it, so a read on the daemon side returns at the release and
// at no other time — the observation needs no seam and no timing guess.
type shimAdmitLease struct {
	released chan struct{}
}

func shimDaemonGrant(t *testing.T, r *Runner, reserve int64) *shimAdmitLease {
	t.Helper()
	client, server := net.Pipe()
	lease := &shimAdmitLease{released: make(chan struct{})}
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer close(lease.released)
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		data, err := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: reserve, Basis: "pinned:client"})
		if err != nil {
			return
		}
		if err := writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data}); err != nil {
			return
		}
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	t.Cleanup(func() { _ = client.Close() })
	return lease
}

// verifies: AIRA-141 — a ci-shim `aira run` holds its DAEMON admission lease for
// the job's whole life, releasing it only once the wait has returned.
//
// Why it must. In shim mode there is no cgroup, so there is no memory.current
// charge naming this job and nothing else in the system that knows it exists.
// The booked reserve is the ONLY representation of its RAM in the daemon's
// ledger. Releasing it at child start therefore made a running job invisible the
// instant it began, and a second `aira run` could be admitted against RAM the
// first was already using — silently over-committing the ci-shim budget the
// mechanism exists to enforce. The real path can release early because the
// kernel keeps counting; this one cannot.
//
// Non-porosity: `running` is appended AFTER the child starts, and the pre-fix
// release happened at the end of launchPrep — strictly BEFORE that append. So
// under the wrong behaviour the lease is already closed when this test reaches
// its assertion, and the dwell only removes the goroutine-scheduling window; it
// is not what the test rests on. Verified by reverting: with the release moved
// back to launchPrep this fails, and the flock test below still passes.
func TestShimRunHoldsTheDaemonAdmissionLeaseUntilTheJobEnds(t *testing.T) {
	r := shimRunner(t, Config{MemorySlice: "aira.slice", MemoryReserve: 1 << 20})
	lease := shimDaemonGrant(t, r, 1<<20)

	marker := filepath.Join(t.TempDir(), "release")
	type launchOutcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan launchOutcome, 1)
	go func() {
		record, err := r.Launch(context.Background(), Request{
			Argv: []string{"/bin/sh", "-c", "while [ ! -f " + marker + " ]; do sleep 0.01; done; exit 0"},
		})
		done <- launchOutcome{record: record, err: err}
	}()
	waitForShimRunning(t, r, "RUN-1")

	// The child is provably still running: it exits only on a marker this test
	// alone writes, and the exit-0 assertion below proves it was alive the whole
	// time rather than having died and left the assertion vacuous.
	select {
	case <-lease.released:
		t.Fatal("the daemon admission lease was released while the ci-shim job was still running: " +
			"the job's RAM is now invisible to the ledger and a second run can be admitted against it (AIRA-141)")
	case <-time.After(250 * time.Millisecond):
	}

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("launch err=%v", outcome.err)
	}
	if outcome.record.Status != StatusExited || outcome.record.ExitCode == nil || *outcome.record.ExitCode != 0 {
		t.Fatalf("record=%+v", outcome.record)
	}
	// The launch really went through a daemon GRANT. Without this the test could
	// pass against an `admission=disabled` launch, whose nil lease is never
	// released at all — the false-pass direction, and the exact reading AIRA-129's
	// own dogfood was limited to.
	if outcome.record.Admission != "immediate" {
		t.Fatalf("admission=%q, want a real daemon grant (`immediate`); the lease assertion above would be vacuous otherwise", outcome.record.Admission)
	}
	// Held is not the same as leaked: the reserve must come back when the job
	// ends, or the ledger over-books permanently in the other direction.
	select {
	case <-lease.released:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon admission lease was never released after the run ended: the reserve leaks for the life of the process")
	}
}

// verifies: AIRA-141 — the FLOCK fallback is still released when the child
// starts, and is NOT swept up in the daemon lease's longer lifetime.
//
// This is the false-fail direction of the same change, and it pins a real
// distinction rather than a taste. A daemon grant is a booked reserve against
// the shim RAM budget and must last as long as the RAM does. The flock fallback
// (daemon down) carries no reserve at all: it is whole-slice mutual exclusion,
// one holder at a time, admitting the next client only when this one lets go.
// Holding it for a job's life would turn a degraded fallback into a global
// serialiser of every shim launch on the box.
//
// Non-porosity: dropping the `admission.lock != nil` discriminator — releasing
// nothing at start — fails this while the daemon test above still passes.
func TestShimRunReleasesTheFlockFallbackWhenTheChildStarts(t *testing.T) {
	r := shimRunner(t, Config{MemorySlice: currentSliceForTest(t), MemoryReserve: 40})
	// No daemon: admitDialFn stays nil and admitSocketPath is empty, so admit
	// falls through to admitWithFlock, which is the only producer of an
	// admissionResult carrying a lock.
	r.sliceMemory = func(string) (int64, int64, bool, string) { return 0, 1 << 40, true, "" }
	lockPath := filepath.Join(t.TempDir(), "admission.lock")
	r.lockAttemptFn = func(string) (*admitLock, error) {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = f.Close()
			return nil, err
		}
		return &admitLock{file: f}, nil
	}

	marker := filepath.Join(t.TempDir(), "release")
	type launchOutcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan launchOutcome, 1)
	go func() {
		record, err := r.Launch(context.Background(), Request{
			Argv: []string{"/bin/sh", "-c", "while [ ! -f " + marker + " ]; do sleep 0.01; done; exit 0"},
		})
		done <- launchOutcome{record: record, err: err}
	}()
	waitForShimRunning(t, r, "RUN-1")

	// A second open file description on the same file. flock(2) treats separate
	// descriptions independently even within one process, so this contends with
	// the launch's own lock exactly as another client's would.
	probe, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("the flock fallback was still held after the ci-shim child started (%v): it carries no reserve, so holding it for the job's life serialises every shim launch (AIRA-141)", err)
	}
	_ = unix.Flock(int(probe.Fd()), unix.LOCK_UN)

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("launch err=%v", outcome.err)
	}
	if outcome.record.Status != StatusExited || outcome.record.ExitCode == nil || *outcome.record.ExitCode != 0 {
		t.Fatalf("record=%+v", outcome.record)
	}
	// The fallback really was taken; otherwise the probe above proves nothing.
	if outcome.record.Admission != "immediate" || outcome.record.AdmissionReserve == nil {
		t.Fatalf("admission=%q reserve=%v, want the flock fallback's own grant", outcome.record.Admission, outcome.record.AdmissionReserve)
	}
}
