//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// verifies: AIRA-277
func TestParseConfineTmuxRefKeepsOnlyTheTwoAllowListedVariables(t *testing.T) {
	environ := []byte("HOME=/home/x\x00SECRET_TOKEN=hunter2\x00TMUX=/tmp/tmux-1000/default,3670595,1\x00TMUX_PANE=%155\x00")
	ref, state := parseConfineTmuxRef(environ)
	if state != paneInTmux || ref.socket != "/tmp/tmux-1000/default" || ref.pane != "%155" {
		t.Fatalf("ref=%+v state=%v", ref, state)
	}
	if strings.Contains(ref.socket+ref.pane, "hunter2") {
		t.Fatalf("a non-allow-listed variable leaked: %+v", ref)
	}

	if _, state := parseConfineTmuxRef([]byte("HOME=/home/x\x00PATH=/bin\x00")); state != paneNotInTmux {
		t.Fatalf("complete environ without tmux: state=%v, want paneNotInTmux", state)
	}
	// Anything short of both variables with a usable socket is UNESTABLISHED, never
	// "not in tmux": a prefix match on TMUX= must not be confused with TMUX_PANE=.
	for name, environ := range map[string]string{
		"only TMUX":       "TMUX=/tmp/s,1,2\x00",
		"only TMUX_PANE":  "TMUX_PANE=%3\x00",
		"relative socket": "TMUX=tmp/s,1,2\x00TMUX_PANE=%3\x00",
		"pane without %":  "TMUX=/tmp/s,1,2\x00TMUX_PANE=3\x00",
		"empty socket":    "TMUX=,1,2\x00TMUX_PANE=%3\x00",
		"bare percent":    "TMUX=/tmp/s,1,2\x00TMUX_PANE=%\x00",
	} {
		if _, state := parseConfineTmuxRef([]byte(environ)); state != paneUnestablished {
			t.Errorf("%s: state=%v, want paneUnestablished", name, state)
		}
	}
	// A block bigger than the read limit that does not show the variables proves
	// nothing about their absence.
	big := append([]byte(strings.Repeat("X=1\x00", confineEnvironReadLimit/4+8)), 0)
	if _, state := parseConfineTmuxRef(big); state != paneUnestablished {
		t.Fatalf("truncated environ without tmux: state=%v, want paneUnestablished", state)
	}
}

// verifies: AIRA-277
func TestConfinePaneLabelStripsTrailingGlyphsAndNeverReturnsEmpty(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"deploy 🤔", "deploy"},
		{"kichad 💤", "kichad"},
		{"ci-build", "ci-build"},
		{"my agent 🌀 💤", "my agent"},
		{"agent 2", "agent 2"},
		{"  spaced  ", "spaced"},
		{"🤔", "%7"},
		{"", "%7"},
		{strings.Repeat("é", 100), strings.Repeat("é", confinePaneLabelMax-1) + "…"},
	} {
		if got := confinePaneLabel(tc.name, "%7"); got != tc.want {
			t.Errorf("confinePaneLabel(%q)=%q, want %q", tc.name, got, tc.want)
		}
	}
}

func paneTestEnviron(socket, pane string) []byte {
	return []byte("HOME=/h\x00TMUX=" + socket + ",1,0\x00TMUX_PANE=" + pane + "\x00")
}

// The scan reports all three states from the supervisor's environment, asks tmux
// ONCE per server, and costs no other facet when tmux fails.
//
// verifies: AIRA-277
func TestConfineScanResolvesTheLaunchingTmuxWindow(t *testing.T) {
	slice := t.TempDir()
	now := time.Now()
	ids := map[string]string{} // pid -> scope id
	for i, name := range []string{"in-tmux-a", "in-tmux-b", "other-server", "plain", "unreadable", "gone-pane", "half-env"} {
		pid := 41000 + i
		scope := confineTestScopeID(name, pid, now.Add(-time.Minute).UnixNano())
		writeConfineTestScope(t, slice, scope, strconv.Itoa(pid)+"\n")
		ids[name] = scope
	}
	environ := map[int][]byte{
		41000: paneTestEnviron("/tmp/tmux-a", "%1"),
		41001: paneTestEnviron("/tmp/tmux-a", "%2"),
		41002: paneTestEnviron("/tmp/tmux-b", "%1"),
		41003: []byte("HOME=/h\x00"),
		41005: paneTestEnviron("/tmp/tmux-a", "%99"),
		41006: []byte("TMUX=/tmp/tmux-a,1,0\x00"),
	}
	calls := map[string]int{}
	deps := confineScanDeps{
		now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty,
		readEnviron: func(pid int) ([]byte, error) {
			if data, ok := environ[pid]; ok {
				return data, nil
			}
			return nil, errors.New("permission denied")
		},
		tmuxNames: func(_ context.Context, socket string) (map[string]string, error) {
			calls[socket]++
			if socket == "/tmp/tmux-a" {
				return map[string]string{"%1": "deploy 🤔", "%2": "speed"}, nil
			}
			return map[string]string{"%1": "field"}, nil
		},
	}
	result, err := listConfinesWithDeps(context.Background(), slice, nil, deps)
	if err != nil || len(result.Scopes) != 7 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	byScope := map[string]ConfineRecord{}
	for _, r := range result.Scopes {
		byScope[r.ScopeID] = r
	}
	want := map[string]*string{"in-tmux-a": strp("deploy"), "in-tmux-b": strp("speed"), "other-server": strp("field"), "plain": strp(""), "unreadable": nil, "gone-pane": nil, "half-env": nil}
	for name, w := range want {
		r := byScope[ids[name]]
		named := confineContainsString(r.UnevaluatedFields, "pane")
		switch {
		case w == nil && (r.Pane != nil || !named):
			t.Errorf("%s: pane=%v unevaluated=%v, want nil and named", name, r.Pane, r.UnevaluatedFields)
		case w != nil && (r.Pane == nil || *r.Pane != *w || named):
			t.Errorf("%s: pane=%v unevaluated=%v, want %q", name, r.Pane, r.UnevaluatedFields, *w)
		}
	}
	if calls["/tmp/tmux-a"] != 1 || calls["/tmp/tmux-b"] != 1 {
		t.Errorf("tmux calls=%v, want exactly one per server", calls)
	}

	// A failing tmux names every tmux-launched job unevaluated and leaves the rest.
	deps.tmuxNames = func(context.Context, string) (map[string]string, error) { return nil, errors.New("no server") }
	failed, err := listConfinesWithDeps(context.Background(), slice, nil, deps)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range failed.Scopes {
		if r.ScopeID == ids["in-tmux-a"] && (r.Pane != nil || !confineContainsString(r.UnevaluatedFields, "pane")) {
			t.Errorf("tmux failure: %+v, want a nil pane named unevaluated", r)
		}
		if r.ScopeID == ids["plain"] && (r.Pane == nil || *r.Pane != "") {
			t.Errorf("a job outside tmux lost its established state when tmux failed: %+v", r)
		}
	}
}

func strp(s string) *string { return &s }

// The real thing: a private tmux server, a window named the way a launcher
// decorates it, and the production tmuxWindowNames asking that server. A stub
// would accept any argv, so this is the only check that `-S <socket> list-panes
// -a -F ...` is a command tmux actually answers.
//
// verifies: AIRA-277
func TestTmuxWindowNamesAgainstARealServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("", "aira-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "sock")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-S", socket, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("new-session", "-d", "-s", "t", "-n", "deploy 🤔", "sleep 300")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	pane := run("display-message", "-p", "-t", "t:0", "#{pane_id}")

	names, err := tmuxWindowNames(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := confinePaneLabel(names[pane], pane); got != "deploy" {
		t.Fatalf("names=%v pane=%s label=%q, want deploy", names, pane, got)
	}
	if _, err := tmuxWindowNames(context.Background(), filepath.Join(dir, "nonexistent")); err == nil {
		t.Fatal("a socket with no server answered without error")
	}
}
