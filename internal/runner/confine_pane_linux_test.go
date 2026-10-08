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
	if state != paneInTmux || ref.socket != "/tmp/tmux-1000/default" || ref.serverPID != "3670595" || ref.pane != "%155" {
		t.Fatalf("ref=%+v state=%v", ref, state)
	}
	if strings.Contains(ref.socket+ref.pane, "hunter2") {
		t.Fatalf("a non-allow-listed variable leaked: %+v", ref)
	}

	if ref, _ := parseConfineTmuxRef([]byte("TMUX=/tmp//a/./s,1,2\x00TMUX_PANE=%1\x00")); ref.socket != "/tmp/a/s" {
		t.Fatalf("socket=%q, want the cleaned path so one server is one key", ref.socket)
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
		{strings.Repeat("é", confinePaneLabelMax), strings.Repeat("é", confinePaneLabelMax)},
		{strings.Repeat("é", confinePaneLabelMax+1), strings.Repeat("é", confinePaneLabelMax-1) + "…"},
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
	for i, name := range []string{"in-tmux-a", "in-tmux-b", "other-server", "plain", "unreadable", "gone-pane", "half-env", "recycled-server"} {
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
		// Same socket and pane id as a live pane, but launched by an EARLIER server
		// (pid 7): the current server (pid 1) has an unrelated "%1".
		41007: []byte("TMUX=/tmp/tmux-a,7,0\x00TMUX_PANE=%1\x00"),
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
				return map[string]string{"1 %1": "deploy 🤔", "1 %2": "speed", "2 %99": "other-server-gen"}, nil
			}
			return map[string]string{"1 %1": "field"}, nil
		},
	}
	result, err := listConfinesWithDeps(context.Background(), slice, nil, deps)
	if err != nil || len(result.Scopes) != 8 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	byScope := map[string]ConfineRecord{}
	for _, r := range result.Scopes {
		byScope[r.ScopeID] = r
	}
	want := map[string]*string{"in-tmux-a": strp("deploy"), "in-tmux-b": strp("speed"), "other-server": strp("field"), "plain": strp(""), "unreadable": nil, "gone-pane": nil, "half-env": nil, "recycled-server": nil}
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
	failCalls := map[string]int{}
	deps.tmuxNames = func(_ context.Context, socket string) (map[string]string, error) {
		failCalls[socket]++
		return nil, errors.New("no server")
	}
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
	// A wedged server must cost the listing ONE timeout, not one per job on it.
	if failCalls["/tmp/tmux-a"] != 1 || failCalls["/tmp/tmux-b"] != 1 {
		t.Errorf("tmux calls while failing=%v, want exactly one per server", failCalls)
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
	pane := run("display-message", "-p", "-t", "t:0", "#{pid} #{pane_id}")
	pid, paneID, _ := strings.Cut(pane, " ")

	names, err := tmuxWindowNames(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := confinePaneLabel(names[pane], paneID); got != "deploy" || pid == "" {
		t.Fatalf("names=%v pane=%s label=%q, want deploy", names, pane, got)
	}
	if _, err := tmuxWindowNames(context.Background(), filepath.Join(dir, "nonexistent")); err == nil {
		t.Fatal("a socket with no server answered without error")
	}
}

// Registry-only (pending) rows perform no live read, so they name "pane"
// unevaluated beside cwd and command rather than leaving it silently absent.
//
// verifies: AIRA-277
func TestConfinePendingRowNamesThePaneUnevaluated(t *testing.T) {
	pending := confineTestOwnedScopeID("pending-pane", "session-a", 4902, time.Now().UnixNano())
	listed := ShimConfineList([]ConfineRegistryEntry{{ScopeID: pending}})
	if len(listed.Scopes) != 1 {
		t.Fatalf("listed=%+v", listed)
	}
	if record := listed.Scopes[0]; record.Pane != nil || !confineContainsString(record.UnevaluatedFields, "pane") {
		t.Fatalf("pending record=%+v, want a nil pane named unevaluated", record)
	}
}

// An environ block at least as big as the limit is read one byte past it, so the
// parser can see it overran; reading exactly the limit would make a missing
// variable look like an absent one.
//
// verifies: AIRA-277
func TestReadBoundedEnvironReadsOnePastTheLimit(t *testing.T) {
	got, err := readBoundedEnviron(strings.NewReader(strings.Repeat("x", confineEnvironReadLimit+500)))
	if err != nil || len(got) != confineEnvironReadLimit+1 {
		t.Fatalf("len=%d err=%v, want %d", len(got), err, confineEnvironReadLimit+1)
	}
	if _, state := parseConfineTmuxRef(got); state != paneUnestablished {
		t.Fatalf("an overrun block without the variables parsed as %v, want paneUnestablished", state)
	}
}

// A tmux that never answers costs one bounded wait, not a hung listing. The fake
// `tmux` execs a long sleep so the process that is killed is the one holding
// stdout, then a second variant leaves a CHILD holding it (the WaitDelay case).
//
// verifies: AIRA-277
func TestTmuxWindowNamesTimesOutOnAWedgedServer(t *testing.T) {
	for name, script := range map[string]string{
		"exec sleep":  "#!/bin/sh\nexec sleep 30\n",
		"child holds": "#!/bin/sh\nsleep 30 &\nwait\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
		start := time.Now()
		_, err := tmuxWindowNames(context.Background(), "/tmp/does-not-matter")
		if err == nil {
			t.Fatalf("%s: a wedged tmux returned no error", name)
		}
		if elapsed := time.Since(start); elapsed > confineTmuxTimeout+2*time.Second {
			t.Fatalf("%s: took %v, want about %v", name, elapsed, confineTmuxTimeout)
		}
	}
}

// A block that overruns the limit is cut mid-entry: the final element is a
// fragment, and "TMUX_PANE=%15" read from "TMUX_PANE=%155" would name another pane.
//
// verifies: AIRA-277
func TestParseConfineTmuxRefIgnoresATruncatedFinalVariable(t *testing.T) {
	head := "TMUX=/tmp/s,1,2\x00TMUX_PANE=%15"
	filler := strings.Repeat("A", confineEnvironReadLimit+1-len(head))
	environ := []byte(head + filler)
	if len(environ) != confineEnvironReadLimit+1 {
		t.Fatalf("fixture length %d", len(environ))
	}
	// TMUX_PANE's value runs on into the filler, so the entry is the one cut by
	// the limit; with the fragment ignored the pane is missing, not "%15AAAA...".
	if ref, state := parseConfineTmuxRef(environ); state == paneInTmux {
		t.Fatalf("a fragment of the final entry was accepted: %+v", ref)
	}
}

// The reaper and --kill never read Pane: they must not read environ or fork tmux.
//
// verifies: AIRA-277
func TestWithoutPaneLookupDoesNotForkTmux(t *testing.T) {
	slice := t.TempDir()
	scope := confineTestScopeID("reaper-view", 41100, time.Now().Add(-time.Minute).UnixNano())
	writeConfineTestScope(t, slice, scope, "41100\n")
	called := false
	deps := withoutPaneLookup(confineScanDeps{
		now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty,
		readEnviron: func(int) ([]byte, error) { return paneTestEnviron("/tmp/tmux-a", "%1"), nil },
		tmuxNames:   func(context.Context, string) (map[string]string, error) { called = true; return nil, nil },
	})
	if _, err := listConfinesWithDeps(context.Background(), slice, nil, deps); err != nil || called {
		t.Fatalf("err=%v tmux called=%v, want no tmux for a caller that ignores Pane", err, called)
	}
}

// tmux output beyond the cap is an error (unestablished), not an unbounded buffer.
//
// verifies: AIRA-277
func TestTmuxWindowNamesCapsTheOutput(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexec head -c " + strconv.Itoa(confineTmuxOutputLimit+4096) + " /dev/zero\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if _, err := tmuxWindowNames(context.Background(), "/tmp/x"); err == nil {
		t.Fatal("output beyond the cap was accepted")
	}
}
