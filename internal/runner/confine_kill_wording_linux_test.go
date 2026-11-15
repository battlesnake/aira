//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"aira/internal/cgrouptest"
)

// verifies: AIRA-339 (a) -- once cgroup.kill was written, SIGKILL is already
// delivered to every process in the scope, so a waitEmpty timeout must say so
// instead of reading as a failed kill. It keeps the unconfirmed code, never
// reports "killed", and carries the delivered marker and the target's identity
// so the daemon can log it. A cgroup.kill WRITE failure delivered nothing and
// must carry neither the marker nor the wording.
func TestConfineKillDeliveredButUnconfirmedSaysSo(t *testing.T) {
	t.Run("wait-timeout-after-kill-write", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestOwnedScopeID("delivered", "owner", 4411, time.Now().UnixNano())
		path := writeConfineTestScope(t, slice, id, "91\n")
		deps := defaultConfineScanDeps()
		deps.waitEmpty = func(context.Context, Scope, time.Duration) error { return errors.New("scope did not become empty") }
		result, err := killConfineWithDeps(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id}}, 2*time.Second, deps)
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineKillUnconfirmed+":") {
			t.Fatalf("err=%v, want %s", err, CodeConfineKillUnconfirmed)
		}
		if result.Status == ConfineKillStatusKilled {
			t.Fatalf("an unconfirmed kill reported killed: %+v", result)
		}
		if !errors.Is(err, ErrConfineKillDelivered) {
			t.Fatalf("delivered-but-unconfirmed error lacks the delivered marker: %v", err)
		}
		for _, want := range []string{"SIGKILL delivered to every process in scope " + id, "cgroup.kill succeeded", "could not be confirmed empty (scope did not become empty)", "may still be exiting", "aira confine --list"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("message %q lacks %q", err.Error(), want)
			}
		}
		if result.ScopeID != id || result.Name != "delivered" || result.Owner != "owner" || result.Phase != ConfineKillPhaseRunning {
			t.Fatalf("delivered-but-unconfirmed result does not identify the target: %+v", result)
		}
		if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); string(data) != "1" {
			t.Fatalf("cgroup.kill=%q, want the kill written", data)
		}
	})
	// waitEmpty can also end at once on a read error, or early on cancellation.
	// SIGKILL was still delivered; no elapsed wait may be claimed.
	for name, waitErr := range map[string]error{"wait-read-error": errors.New("cgroup.events lacks populated"), "wait-cancelled": context.Canceled} {
		t.Run(name, func(t *testing.T) {
			slice := t.TempDir()
			id := confineTestOwnedScopeID("early", "owner", 4413, time.Now().UnixNano())
			writeConfineTestScope(t, slice, id, "91\n")
			deps := defaultConfineScanDeps()
			deps.waitEmpty = func(context.Context, Scope, time.Duration) error { return waitErr }
			result, err := killConfineWithDeps(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id}}, 2*time.Second, deps)
			if err == nil || !strings.HasPrefix(err.Error(), CodeConfineKillUnconfirmed+":") || result.Status == ConfineKillStatusKilled {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !errors.Is(err, ErrConfineKillDelivered) || !errors.Is(err, waitErr) {
				t.Fatalf("err=%v, want both the delivered marker and the wait cause", err)
			}
			if !strings.Contains(err.Error(), "SIGKILL delivered") || !strings.Contains(err.Error(), waitErr.Error()) {
				t.Fatalf("message %q lacks the delivery or its cause", err.Error())
			}
			if strings.Contains(err.Error(), " 2s") || strings.Contains(err.Error(), "after ") {
				t.Fatalf("message %q claims an elapsed wait that may not have happened", err.Error())
			}
		})
	}
	t.Run("kill-write-error", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestOwnedScopeID("writefail", "owner", 4412, time.Now().UnixNano())
		path := writeConfineTestScope(t, slice, id, "81\n")
		if err := os.Remove(filepath.Join(path, "cgroup.kill")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(path, "cgroup.kill"), 0o755); err != nil {
			t.Fatal(err)
		}
		result, err := KillConfine(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id}})
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineKillUnconfirmed+":") || result.Status == ConfineKillStatusKilled {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if errors.Is(err, ErrConfineKillDelivered) || strings.Contains(err.Error(), "SIGKILL delivered") {
			t.Fatalf("a failed cgroup.kill write claims delivery: %v", err)
		}
	})
}

// verifies: AIRA-339 (c), folded in from AIRA-319 -- an empty scope whose
// supervisor is established DEAD will not fill again: the reply says nothing
// remains in this scope to kill and never asks the operator to retry. It claims
// no more than the scope shows: never that the job has ended (an escaped
// workload can outlive its supervisor; see the real-cgroup escape regression). Every other empty-scope reading (supervisor alive,
// liveness unevaluated, or a dead probe vetoed by a still-held lease) keeps the
// "retry" reply. The code and the no-kill decision are unchanged in all of them.
func TestConfineKillEmptyScopeWordingFollowsSupervisorLiveness(t *testing.T) {
	alive, dead := true, false
	cases := []struct {
		name     string
		live     *bool
		leased   bool
		ended    bool
		scopePID int
	}{
		{name: "supervisor-dead", live: &dead, ended: true, scopePID: 4421},
		{name: "supervisor-alive", live: &alive, scopePID: 4422},
		{name: "supervisor-unevaluated", live: nil, scopePID: 4423},
		{name: "dead-probe-vetoed-by-held-lease", live: &dead, leased: true, scopePID: 4424},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slice := t.TempDir()
			id := confineTestOwnedScopeID("emptyjob", "owner", tc.scopePID, time.Now().UnixNano())
			path := writeConfineTestScope(t, slice, id, "")
			deps := defaultConfineScanDeps()
			deps.supervisorLive = func(int) *bool { return tc.live }
			var registry []ConfineRegistryEntry
			if tc.leased {
				registry = []ConfineRegistryEntry{{ScopeID: id}}
			}
			result, err := killConfineWithDeps(context.Background(), slice, id, "owner", false, registry, time.Second, deps)
			if err == nil || !strings.HasPrefix(err.Error(), CodeConfineNotLaunched+":") || result.Status == ConfineKillStatusKilled {
				t.Fatalf("result=%+v err=%v, want %s", result, err, CodeConfineNotLaunched)
			}
			if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); len(data) != 0 {
				t.Fatalf("empty scope kill=%q", data)
			}
			message := err.Error()
			if tc.ended {
				for _, want := range []string{"is empty and its supervisor is gone", "nothing remains in this scope to kill"} {
					if !strings.Contains(message, want) {
						t.Fatalf("dead-supervisor reply %q lacks %q", message, want)
					}
				}
				if strings.Contains(message, "retry") || strings.Contains(message, "reaper") || strings.Contains(message, "ended") {
					t.Fatalf("dead-supervisor reply asks for a retry, promises a cleanup, or claims the job ended: %q", message)
				}
				return
			}
			if !strings.Contains(message, "nothing to kill yet; retry") {
				t.Fatalf("reply %q lost the retry wording", message)
			}
			if strings.Contains(message, "supervisor is gone") || strings.Contains(message, "nothing remains") {
				t.Fatalf("reply %q reads the supervisor as dead without an established-dead supervisor", message)
			}
		})
	}
}

// verifies: AIRA-339 (c) escape regression, from the Sol round-2 review -- an
// empty scope plus a dead supervisor does NOT establish that the job ended. On a
// real kernel a workload starts inside its scope, migrates out of it, and stays
// alive after its supervisor is gone. --kill must still refuse to kill (same
// code, the escaped process untouched), and its reply must claim only that
// nothing remains in the scope, never that the job has ended.
func TestConfineKillEmptyScopeDeadSupervisorEscapedWorkloadNeverReadsAsEnded(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real cgroup backend unavailable: %v", err)
	}
	// A real supervisor PID that is established dead (reaped, so ESRCH).
	supervisor := exec.Command("/bin/true")
	if err := supervisor.Run(); err != nil {
		t.Fatalf("run stand-in supervisor: %v", err)
	}
	id := confineTestOwnedScopeID("escaped", "owner", supervisor.Process.Pid, time.Now().UnixNano())
	scope, err := backend.Create(context.Background(), id)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real scope unavailable: %v", err)
	}
	// The escape target is a sibling outside every confine scan (not .aira-CONFINE-*).
	outside := filepath.Join(parent, "escape-target")
	if err := os.Mkdir(outside, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "escape-target cgroup unavailable: %v", err)
	}
	scopeFD, err := syscall.Open(scope.Reference(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "scope fd unavailable: %v", err)
	}
	defer syscall.Close(scopeFD)
	workload := exec.Command("/bin/sleep", "60")
	workload.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scopeFD}
	if err := workload.Start(); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "start workload in scope: %v", err)
	}
	defer func() { _ = workload.Process.Kill(); _, _ = workload.Process.Wait() }()
	if empty, err := scope.Empty(); err != nil || empty {
		t.Fatalf("workload did not start inside its scope: empty=%v err=%v", empty, err)
	}
	if err := os.WriteFile(filepath.Join(outside, "cgroup.procs"), []byte(fmt.Sprint(workload.Process.Pid)), 0); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "migrate workload out of its scope: %v", err)
	}
	if empty, err := scope.Empty(); err != nil || !empty {
		t.Fatalf("scope not empty after the workload migrated out: empty=%v err=%v", empty, err)
	}
	if live := probeSupervisorLive(supervisor.Process.Pid); live == nil || *live {
		t.Fatalf("stand-in supervisor not established dead: %v", live)
	}

	result, err := KillConfine(context.Background(), parent, id, "owner", false, nil)
	if err == nil || !strings.HasPrefix(err.Error(), CodeConfineNotLaunched+":") || result.Status == ConfineKillStatusKilled {
		t.Fatalf("result=%+v err=%v, want %s", result, err, CodeConfineNotLaunched)
	}
	if err := unix.Kill(workload.Process.Pid, 0); err != nil {
		t.Fatalf("the escaped workload was touched by an empty-scope --kill: %v", err)
	}
	message := err.Error()
	if !strings.Contains(message, "supervisor is gone") || !strings.Contains(message, "nothing remains in this scope to kill") {
		t.Fatalf("reply %q is not the dead-supervisor empty-scope reply", message)
	}
	if strings.Contains(message, "ended") || strings.Contains(message, "retry") {
		t.Fatalf("reply %q claims the job ended (its escaped workload is alive) or asks for a retry", message)
	}
}

// verifies: AIRA-339 (b) -- the unattributed-SIGKILL advisory names the
// operator's OWN `aira confine --kill` as a candidate (the kill path never tells
// the supervisor, so it cannot tell its own operator's kill from oomd or a stray
// kill -9), and still claims none of the candidates.
func TestFormatConfineTerminationAdvisoryIncludesOwnConfineKill(t *testing.T) {
	quiet := cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}
	advisory := formatConfineTerminationAdvisory(ConfineTerminatedUnattributedSIGKILL, quiet)
	for _, want := range []string{"`aira confine --kill` -- your own or another session's", "kill -9", "systemd-oomd", "cannot tell which"} {
		if !strings.Contains(advisory, want) {
			t.Fatalf("advisory %q lacks %q", advisory, want)
		}
	}
}
