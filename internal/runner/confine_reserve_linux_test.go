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
	"sync/atomic"
	"testing"
	"time"
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
	case <-time.After(time.Second):
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
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
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
	case <-time.After(time.Second):
		t.Fatal("lease did not release on close")
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
