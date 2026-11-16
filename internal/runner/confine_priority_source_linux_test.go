//go:build linux

package runner

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// AIRA-348. The SOURCE of the launch priority (flag, AIRA_CONFINE_PRIORITY or
// the default) through ResolveConfinePriority, the resolver the CLI calls with
// os.LookupEnv, into the real confine admission path (admitConfine over a unix
// socket) and the queued row `--list` and `aira top` show.

// fakePriorityDaemon is a unix-socket admit endpoint that records every
// request's args. A priority-aware daemon (refuseField false) grants each
// request; an OLD one refuses a request that carries `priority` with the
// proto-15 daemon's exact refusal and grants one that does not.
type fakePriorityDaemon struct {
	socket string
	mu     sync.Mutex
	frames []map[string]any
}

func startFakePriorityDaemon(t *testing.T, refuseField bool) *fakePriorityDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "a348")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	daemon := &fakePriorityDaemon{socket: filepath.Join(dir, "d.sock")}
	listener, err := net.Listen("unix", daemon.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request runnerAdmitRequestFrame
				if err := readRunnerAdmitFrame(conn, &request); err != nil {
					return
				}
				daemon.mu.Lock()
				daemon.frames = append(daemon.frames, request.Request.Args)
				daemon.mu.Unlock()
				if _, sent := request.Request.Args["priority"]; sent && refuseField {
					_ = writeRunnerAdmitFrame(conn, runnerAdmitResponseFrame{Code: "E_DAEMON_PROTOCOL", Error: oldDaemonPriorityRefusalFrame})
					return
				}
				grantFrame(conn)
			}()
		}
	}()
	return daemon
}

func (d *fakePriorityDaemon) recorded() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.frames...)
}

// setPriorityEnvForTest sets AIRA_CONFINE_PRIORITY for one test, or unsets it
// when value is "<unset>".
func setPriorityEnvForTest(t *testing.T, value string) {
	t.Helper()
	t.Setenv(ConfinePriorityEnv, "")
	if value == "<unset>" {
		if err := os.Unsetenv(ConfinePriorityEnv); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv(ConfinePriorityEnv, value)
}

// verifies: AIRA-348 — with --priority given the variable is not even read (the
// --owner / AIRA_CONFINE_OWNER shape), so a stale or broken value cannot
// matter. Mutation: look the variable up before the flag check.
func TestResolveConfinePriorityFlagDoesNotReadTheEnvironment(t *testing.T) {
	for _, flag := range []string{ConfinePriorityHigh, ConfinePriorityNormal} {
		read := false
		high, err := ResolveConfinePriority(flag, func(key string) (string, bool) {
			read = read || key == ConfinePriorityEnv
			return "urgent", true
		})
		if err != nil || read || high != (flag == ConfinePriorityHigh) {
			t.Fatalf("--priority %s: high=%v err=%v read=%v", flag, high, err, read)
		}
	}
}

// verifies: AIRA-348 — for every source, the priority resolved from the real
// process environment reaches the admit wire through admitConfine (the confine
// launch path) and the queued row built from what the daemon was sent says the
// same word, so `--list` and `aira top` show the effective priority whichever
// source chose it. The fake daemon stands in for one hop only (wire `priority`
// -> queued entry High), which the daemon owns and
// TestValidateAdmitArgsPriority / TestAdmitSnapshotFollowsTheHighTier cover.
// Mutations: env read before the flag; env never read; whitespace not absent;
// drop `HighPriority: request.HighPriority` in admitConfine; drop the High ->
// Priority mapping.
func TestPrioritySourceReachesTheAdmitWireAndTheQueuedRow(t *testing.T) {
	for _, tc := range []struct {
		name, env, flag, want string
	}{
		{"default", "<unset>", "", ConfinePriorityNormal},
		{"env high", "high", "", ConfinePriorityHigh},
		{"env normal", "normal", "", ConfinePriorityNormal},
		{"env whitespace is absent", " ", "", ConfinePriorityNormal},
		{"flag high", "<unset>", ConfinePriorityHigh, ConfinePriorityHigh},
		{"flag normal beats env high", "high", ConfinePriorityNormal, ConfinePriorityNormal},
		{"flag high beats env normal", "normal", ConfinePriorityHigh, ConfinePriorityHigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setPriorityEnvForTest(t, tc.env)
			high, err := ResolveConfinePriority(tc.flag, os.LookupEnv)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			daemon := startFakePriorityDaemon(t, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := admitConfine(ctx, currentSliceForTest(t), ConfineRequest{AdmitSocketPath: daemon.socket, HighPriority: high}, 1<<20)
			if err != nil {
				t.Fatalf("admitConfine: %v", err)
			}
			result.releaseAdmission()
			frames := daemon.recorded()
			if len(frames) != 1 {
				t.Fatalf("frames=%d, want 1", len(frames))
			}
			raw, sent := frames[0]["priority"]
			if sent != (tc.want == ConfinePriorityHigh) || (sent && raw != ConfinePriorityHigh) {
				t.Fatalf("wire priority=%#v sent=%v, want %s", raw, sent, tc.want)
			}
			rows := shimQueuedConfines([]ConfineQueuedEntry{{ScopeID: "CONFINE-job-42-abc@s-1", ReserveBytes: 1 << 20, High: raw == ConfinePriorityHigh}}, time.Now())
			if len(rows) != 1 || rows[0].Priority != tc.want {
				t.Fatalf("queued rows=%+v, want priority %q", rows, tc.want)
			}
		})
	}
}

// verifies: AIRA-348 with AIRA-340's old-daemon fallback — an ENV-sourced high
// against a daemon that predates the field degrades exactly as a flag-sourced
// one does: one refused request carrying `priority: "high"`, then exactly one
// re-ask at normal (no field), granted, with the visible notice. The variable
// only supplies the client's choice. Mutations: Getenv-style resolution that
// ignores the variable (one frame, no field, no notice); no re-ask (refused);
// a re-ask still carrying the field (refused again); drop the notice.
func TestEnvSourcedHighAgainstAnOldDaemonReasksOnceAtNormal(t *testing.T) {
	setPriorityEnvForTest(t, ConfinePriorityHigh)
	high, err := ResolveConfinePriority("", os.LookupEnv)
	if err != nil || !high {
		t.Fatalf("%s=high resolved high=%v err=%v", ConfinePriorityEnv, high, err)
	}
	daemon := startFakePriorityDaemon(t, true)
	var diagnostics strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := admitConfine(ctx, currentSliceForTest(t), ConfineRequest{AdmitSocketPath: daemon.socket, HighPriority: high, Stderr: &diagnostics}, 1<<20)
	if err != nil {
		t.Fatalf("an env-sourced high against an old daemon was refused instead of degraded: %v", err)
	}
	result.releaseAdmission()
	frames := daemon.recorded()
	if len(frames) != 2 {
		t.Fatalf("frames=%d, want 2 (the refused high request, then one re-ask at normal)", len(frames))
	}
	if frames[0]["priority"] != ConfinePriorityHigh {
		t.Fatalf("first frame priority=%#v, want %q", frames[0]["priority"], ConfinePriorityHigh)
	}
	if _, sent := frames[1]["priority"]; sent {
		t.Fatalf("the re-ask still carried priority=%#v", frames[1]["priority"])
	}
	notice := diagnostics.String()
	if strings.Count(notice, "confine: notice:") != 1 {
		t.Fatalf("want exactly one notice, got %q", notice)
	}
	for _, want := range []string{"NORMAL", "restart aira-daemon.service"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice %q does not contain %q", notice, want)
		}
	}
}
