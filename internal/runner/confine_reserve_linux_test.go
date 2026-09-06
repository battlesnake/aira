//go:build linux

package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/pylib"
	"aira/internal/testdeadline"
)

func reserveTestRunner() *Runner {
	return &Runner{
		memorySlice: "aira.slice", memoryReserve: 40,
		admissionMaxWait: time.Second, pollInterval: time.Millisecond,
		clock: systemClock{},
	}
}

func TestConfineReserveKilledHelperReleasesLeaseOnce(t *testing.T) {
	if os.Getenv("AIRA_CONFINE_RESERVE_KILL_HELPER") == "1" {
		reservation, err := ConfineReserve(context.Background(), ConfineReserveRequest{
			Slice: "aira.slice", AdmitSocketPath: os.Getenv("AIRA_CONFINE_RESERVE_KILL_SOCKET"),
			Bytes: 40, Pinned: true, Signature: "pytest:killed-helper", MaxWait: time.Second,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer reservation.Close()
		fmt.Fprintln(os.Stdout, "granted")
		select {}
	}

	socket := filepath.Join(t.TempDir(), "admit.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	released := make(chan struct{})
	var releases atomic.Int64
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var frame runnerAdmitRequestFrame
		if readRunnerAdmitFrame(conn, &frame) != nil {
			return
		}
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
		_ = writeRunnerAdmitFrame(conn, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = conn.Read(one[:])
		releases.Add(1)
		close(released)
	}()
	command := exec.Command(os.Args[0], "-test.run=^TestConfineReserveKilledHelperReleasesLeaseOnce$")
	command.Env = append(os.Environ(),
		"AIRA_CONFINE_RESERVE_KILL_HELPER=1",
		"AIRA_CONFINE_RESERVE_KILL_SOCKET="+socket,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if scanner := bufio.NewScanner(stdout); !scanner.Scan() || scanner.Text() != "granted" {
		_ = command.Process.Kill()
		t.Fatalf("helper grant line=%q err=%v", scanner.Text(), scanner.Err())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	select {
	case <-released:
	case <-testdeadline.After(time.Second):
		t.Fatal("kill -9 did not release daemon lease")
	}
	if releases.Load() != 1 {
		t.Fatalf("lease releases=%d", releases.Load())
	}
}

func TestConfineReserveDaemonDownNeverEngagesFlock(t *testing.T) {
	r := reserveTestRunner()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) {
		return nil, errors.New("daemon down")
	}
	var flockAttempts atomic.Int64
	r.lockAttemptFn = func(string) (*admitLock, error) {
		flockAttempts.Add(1)
		return nil, errors.New("must not engage flock")
	}
	started := time.Now()
	reservation, err := confineReserveWithRunner(context.Background(), ConfineReserveRequest{
		Bytes: 40, Pinned: true, Signature: "pytest:test_example.py::test_case",
	}, r)
	if err == nil || reservation != nil || flockAttempts.Load() != 0 {
		t.Fatalf("reservation=%+v err=%v flockAttempts=%d", reservation, err, flockAttempts.Load())
	}
	if elapsed := time.Since(started); testdeadline.Exceeded(elapsed, 100*time.Millisecond) {
		t.Fatalf("daemon-down reserve was not instant: %s", elapsed)
	}
}

func TestConfineReservePinnedGrantHeldUntilCloseAndReleasedOnce(t *testing.T) {
	r := reserveTestRunner()
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	released := make(chan struct{})
	go func() {
		defer server.Close()
		var frame runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &frame); err != nil {
			t.Error(err)
			close(released)
			return
		}
		args := frame.Request.Args
		if args["reserve"] != float64(40) && args["reserve"] != int64(40) || args["pinned"] != true || args["signature"] != "pytest:test_example.py::test_case" {
			t.Errorf("admit args=%v", args)
		}
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = server.Read(one[:])
		close(released)
	}()
	reservation, err := confineReserveWithRunner(context.Background(), ConfineReserveRequest{
		Bytes: 40, Pinned: true, Signature: "pytest:test_example.py::test_case",
	}, r)
	if err != nil || reservation == nil || reservation.Basis != "pinned:client" || reservation.Reserve != 40 {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	select {
	case <-released:
		t.Fatal("lease released before helper stdin lifecycle ended")
	case <-time.After(20 * time.Millisecond):
	}
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-testdeadline.After(time.Second):
		t.Fatal("lease did not release on close")
	}
}

// verifies: v1 requires a PINNED reserve — an unpinned request is refused BEFORE
// any daemon dial (never sent to the estimate/p90 path). RED against dropping the
// !Pinned guard in validateConfineReserveRequest.
func TestConfineReserveUnpinnedIsRefusedBeforeDial(t *testing.T) {
	r := reserveTestRunner()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) {
		t.Fatal("unpinned reserve must be refused before any daemon dial")
		return nil, errors.New("unreachable")
	}
	reservation, err := confineReserveWithRunner(context.Background(), ConfineReserveRequest{
		Bytes: 40, Pinned: false, Signature: "pytest:unpinned",
	}, r)
	if err == nil || reservation != nil || !strings.Contains(err.Error(), "must be pinned") {
		t.Fatalf("reservation=%+v err=%v, want pinned refusal", reservation, err)
	}
}

// verifies: a saturated slice (budget full for max_wait) is a terminal reserve
// error with NO reservation (the helper exits nonzero → the plugin fails open) —
// it never fabricates a grant. RED against treating E_ADMIT_SATURATED as a grant.
func TestConfineReserveSaturatedIsTerminalNoGrant(t *testing.T) {
	r := reserveTestRunner()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var frame runnerAdmitRequestFrame
			_ = readRunnerAdmitFrame(server, &frame)
			data, _ := json.Marshal(runnerAdmitRejection{Basis: "reject:saturated"})
			_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: "E_ADMIT_SATURATED", Error: "saturated", Data: data})
		}()
		return client, nil
	}
	reservation, err := confineReserveWithRunner(context.Background(), ConfineReserveRequest{
		Bytes: 40, Pinned: true, Signature: "pytest:saturated",
	}, r)
	if err == nil || reservation != nil {
		t.Fatalf("reservation=%+v err=%v, want terminal saturated error", reservation, err)
	}
}

func TestConfineReserveTooLargeClampsAndReadmitsPinned(t *testing.T) {
	r := reserveTestRunner()
	var dials atomic.Int64
	r.admitDialFn = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		attempt := dials.Add(1)
		go func() {
			defer server.Close()
			var frame runnerAdmitRequestFrame
			_ = readRunnerAdmitFrame(server, &frame)
			if frame.Request.Args["pinned"] != true {
				t.Errorf("attempt %d was unpinned: %v", attempt, frame.Request.Args)
			}
			if attempt == 1 {
				data, _ := json.Marshal(runnerAdmitRejection{Required: 100, Ceiling: 60, Basis: "pinned:client"})
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: "E_ADMIT_TOO_LARGE", Error: "too large", Data: data})
				return
			}
			if frame.Request.Args["reserve"] != int64(60) && frame.Request.Args["reserve"] != float64(60) {
				t.Errorf("clamped reserve args=%v", frame.Request.Args)
			}
			data, _ := json.Marshal(runnerAdmitGrant{State: "waited", Reserve: 60, Basis: "pinned:client"})
			_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
			var one [1]byte
			_, _ = server.Read(one[:])
		}()
		return client, nil
	}
	reservation, err := confineReserveWithRunner(context.Background(), ConfineReserveRequest{
		Bytes: 100, Pinned: true, Signature: "pytest:monster",
	}, r)
	if err != nil || reservation == nil || reservation.Reserve != 60 || reservation.ClampedFrom != 100 || reservation.Basis != "pinned:client" || dials.Load() != 2 {
		t.Fatalf("reservation=%+v err=%v dials=%d", reservation, err, dials.Load())
	}
	_ = reservation.Close()
}

// reserveAdmitCapture stands up a real unix admit socket (the path
// ConfineReserve itself takes, since it builds its own Runner and has no dial
// seam) and records the args of the first admit frame it is sent, then grants.
func reserveAdmitCapture(t *testing.T) (socket string, args func() map[string]any, dials func() int64) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "admit.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var captured atomic.Value
	var accepted atomic.Int64
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer conn.Close()
				var frame runnerAdmitRequestFrame
				if readRunnerAdmitFrame(conn, &frame) != nil {
					return
				}
				captured.Store(frame.Request.Args)
				data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
				_ = writeRunnerAdmitFrame(conn, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				var one [1]byte
				_, _ = conn.Read(one[:])
			}()
		}
	}()
	return socket, func() map[string]any {
		value, _ := captured.Load().(map[string]any)
		return value
	}, func() int64 { return accepted.Load() }
}

// TestConfineReserveChargesTheInheritedParentSlice is AIRA-115's regression
// test. `confineReserve` inherited the parent job's SCOPE ID from the
// environment but defaulted its SLICE to DefaultConfineSlice independently, so
// a job confined to any other slice had its per-test sub-reservations booked
// against aira.slice: the reserving slice over-charged for memory it does not
// host (healthy jobs there wait behind a phantom reservation), the hosting slice
// under-counted.
//
// RED against the old `if request.Slice == "" { request.Slice =
// DefaultConfineSlice }`: the wire slice was "aira.slice", not the parent's.
//
// The parent_scope_id assertion is the anti-porosity guard, not decoration. It
// is what proves the two halves of the same fact now travel together — a fix
// that inherited the slice but dropped the scope-id marker would break the
// AIRA-101 drain-convergence carve-out, and a test that only looked at the slice
// could not see it. It also proves the frame was actually populated.
//
// verifies: AIRA-115
func TestConfineReserveChargesTheInheritedParentSlice(t *testing.T) {
	socket, args, _ := reserveAdmitCapture(t)
	parent := confineScopeID("pytest", "", true)
	t.Setenv("AIRA_CONFINE_SCOPE_ID", parent)
	t.Setenv(pylib.ConfineParentSliceEnv, "/sys/fs/cgroup/user.slice/custom.slice")

	reservation, err := ConfineReserve(context.Background(), ConfineReserveRequest{
		AdmitSocketPath: socket, Bytes: 40, Pinned: true,
		Signature: "pytest:test_example.py::test_case", MaxWait: 5 * time.Second,
	})
	if err != nil || reservation == nil {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	defer reservation.Close()
	got := args()
	if got["slice"] != "/sys/fs/cgroup/user.slice/custom.slice" {
		t.Fatalf("admit slice=%v, want the parent job's resolved slice (charging %q mis-attributes the reservation to a slice that does not host it)",
			got["slice"], DefaultConfineSlice)
	}
	if got["parent_scope_id"] != parent {
		t.Fatalf("admit parent_scope_id=%v, want %q: the sub-reservation marker and the slice must travel together", got["parent_scope_id"], parent)
	}
}

// TestConfineReserveOutsideAConfineJobKeepsTheDefaultSlice is the false-fail
// direction of the same fix: an UNCONFINED caller (no inherited scope id, no
// inherited slice) still gets DefaultConfineSlice, which is what that default
// was always actually for. Without this, "inherit or refuse" could be
// over-applied into refusing every standalone `aira confine-reserve`.
//
// The operator's AIRA_CONFINE_SLICE is set here on purpose, and its being
// IGNORED is a recorded decision rather than an oversight (build-review F4).
// This fix teaches confine-reserve to inherit what its PARENT JOB emitted; it
// does not newly teach it to read the operator's launch setting, which would be
// a separate behaviour change to a separate input. Pre-AIRA-115 behaviour for
// the unconfined caller is therefore unchanged.
//
// verifies: AIRA-115
func TestConfineReserveOutsideAConfineJobKeepsTheDefaultSlice(t *testing.T) {
	socket, args, _ := reserveAdmitCapture(t)
	// Emptied rather than assumed absent: the suite itself runs inside `aira
	// confine`, which now exports both.
	t.Setenv("AIRA_CONFINE_SCOPE_ID", "")
	t.Setenv(pylib.ConfineParentSliceEnv, "")
	t.Setenv("AIRA_CONFINE_SLICE", "operator.slice")

	reservation, err := ConfineReserve(context.Background(), ConfineReserveRequest{
		AdmitSocketPath: socket, Bytes: 40, Pinned: true,
		Signature: "pytest:standalone", MaxWait: 5 * time.Second,
	})
	if err != nil || reservation == nil {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	defer reservation.Close()
	if got := args(); got["slice"] != DefaultConfineSlice {
		t.Fatalf("admit slice=%v, want %q for an unconfined caller (the operator's launch setting is not this resolver's input)", got["slice"], DefaultConfineSlice)
	}
}

// TestConfineReserveExplicitSliceOverridesTheInheritedOne pins the precedence:
// AIRA-58 forbids silently SUBSTITUTING for a caller's declared value, so an
// explicit --slice still wins over the inherited one. Inheritance fills the gap
// where the caller said nothing; it does not overrule what they said.
//
// verifies: AIRA-115
func TestConfineReserveExplicitSliceOverridesTheInheritedOne(t *testing.T) {
	socket, args, _ := reserveAdmitCapture(t)
	t.Setenv("AIRA_CONFINE_SCOPE_ID", confineScopeID("pytest", "", true))
	t.Setenv(pylib.ConfineParentSliceEnv, "/sys/fs/cgroup/user.slice/inherited.slice")

	reservation, err := ConfineReserve(context.Background(), ConfineReserveRequest{
		Slice: "explicit.slice", AdmitSocketPath: socket, Bytes: 40, Pinned: true,
		Signature: "pytest:explicit", MaxWait: 5 * time.Second,
	})
	if err != nil || reservation == nil {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	defer reservation.Close()
	if got := args(); got["slice"] != "explicit.slice" {
		t.Fatalf("admit slice=%v, want the explicitly declared slice", got["slice"])
	}
}

// TestConfineReserveRefusesRatherThanDefaultUnderAParentScope is the AIRA-58
// half of AIRA-115: a scope id present with no usable slice means the
// environment is asserting this reservation belongs to a running job while
// withholding where that job lives. Defaulting there is exactly the silent
// mis-attribution being removed, so it is refused BEFORE any dial — the caller
// then fails open and the test runs unreserved, which under-counts one slice
// rather than over-charging an unrelated one.
//
// The zero-dial assertion is what makes this more than an error-string test: a
// refusal issued after the daemon had already been asked would have charged the
// wrong slice anyway.
//
// verifies: AIRA-115
func TestConfineReserveRefusesRatherThanDefaultUnderAParentScope(t *testing.T) {
	socket, _, dials := reserveAdmitCapture(t)
	parent := confineScopeID("pytest", "", true)
	t.Setenv("AIRA_CONFINE_SCOPE_ID", parent)
	// A ".." component is exactly as unusable as absence: both slice resolvers
	// refuse one, so InheritedConfineSlice discards it rather than forwarding it.
	for name, slice := range map[string]string{"absent": "", "unusable": "../escape.slice"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(pylib.ConfineParentSliceEnv, slice)
			before := dials()
			reservation, err := ConfineReserve(context.Background(), ConfineReserveRequest{
				AdmitSocketPath: socket, Bytes: 40, Pinned: true,
				Signature: "pytest:orphan-slice", MaxWait: 5 * time.Second,
			})
			if err == nil || reservation != nil {
				if reservation != nil {
					_ = reservation.Close()
				}
				t.Fatalf("reservation=%+v err=%v, want a refusal rather than a default-slice charge", reservation, err)
			}
			if !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") || !strings.Contains(err.Error(), parent) {
				t.Fatalf("err=%v, want E_CONFINE_UNAVAILABLE naming the inherited scope %q", err, parent)
			}
			if after := dials(); after != before {
				t.Fatalf("daemon was dialled %d time(s) before refusing", after-before)
			}
		})
	}
}
