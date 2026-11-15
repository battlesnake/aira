//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// supervisor is established DEAD has ended: the reply says so and never asks the
// operator to retry. Every other empty-scope reading (supervisor alive,
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
				for _, want := range []string{"is empty and its supervisor is gone", "the job has ended", "nothing to kill"} {
					if !strings.Contains(message, want) {
						t.Fatalf("dead-supervisor reply %q lacks %q", message, want)
					}
				}
				if strings.Contains(message, "retry") || strings.Contains(message, "reaper") {
					t.Fatalf("dead-supervisor reply asks for a retry or promises a cleanup: %q", message)
				}
				return
			}
			if !strings.Contains(message, "nothing to kill yet; retry") {
				t.Fatalf("reply %q lost the retry wording", message)
			}
			if strings.Contains(message, "has ended") {
				t.Fatalf("reply %q claims the job ended without an established-dead supervisor", message)
			}
		})
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
