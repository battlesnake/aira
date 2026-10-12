package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// AIRA-183. `confine --list`'s LIVE=no was two different situations wearing one
// word: a supervisor that has DIED (scope orphaned, awaiting the reaper) and a
// supervisor that is alive with a momentarily empty subtree. An operator read
// one as the other, concluded a kill had not worked, and killed a second,
// unrelated PID on that basis.
//
// verifies: AIRA-183

func confineLiveRecord(t *testing.T, name string, pid int, subtree, supervisor *bool) runner.ConfineRecord {
	t.Helper()
	zero, rss, age := 0, int64(1<<20), int64(600)
	scopeCap := "2147483648"
	return runner.ConfineRecord{
		Name: name, Owner: runner.ConfineUnknownOwner, SupervisorPID: &pid,
		ScopeID:   "CONFINE-" + name + "-4242-abc",
		Populated: &zero, SubtreePopulated: subtree, SupervisorLive: supervisor,
		RSSBytes: &rss, AgeSeconds: &age, Cap: &scopeCap,
	}
}

func renderConfineList(t *testing.T, scopes []runner.ConfineRecord) string {
	t.Helper()
	result := runner.ConfineListResult{Verdict: "pass", Scopes: scopes}
	dispatch := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, _ core.Request) core.Response {
		return core.Response{OK: true, Code: "OK", Data: result}
	})
	var stdout terminalBuffer
	var stderr bytes.Buffer
	if exit := runWithInputDispatcher([]string{"confine", "--list"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	return stdout.String()
}

// liveCell returns the LIVE column of the row whose NAME column matches, so an
// assertion cannot be satisfied by a word that appears anywhere else in the
// output (the legend below prints both words in prose, which would make a bare
// strings.Contains vacuous).
func liveCell(t *testing.T, output, name string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != name {
			continue
		}
		// NAME OWNER SUPERVISOR-PID SCOPE-ID LIVE ...
		return fields[4]
	}
	t.Fatalf("no row named %q in:\n%s", name, output)
	return ""
}

// The whole point, in one listing: a killed-supervisor scope and an
// alive-but-idle scope must not read the same. This is the exact shape of the
// reported confusion — two rows, both empty, one dead and one not.
func TestAIRA183ConfineListSeparatesAnOrphanFromAnIdleScope(t *testing.T) {
	t.Parallel()
	live, dead, empty := true, false, false
	output := renderConfineList(t, []runner.ConfineRecord{
		confineLiveRecord(t, "killed", 4242, &empty, &dead),
		confineLiveRecord(t, "idling", 4243, &empty, &live),
	})
	killed, idling := liveCell(t, output, "killed"), liveCell(t, output, "idling")
	if killed == idling {
		t.Fatalf("a dead supervisor and a live one still render the same LIVE cell %q:\n%s", killed, output)
	}
	if killed != "orphaned" {
		t.Fatalf("a dead supervisor over an empty scope must render orphaned, got %q:\n%s", killed, output)
	}
	if idling != "idle" {
		t.Fatalf("a live supervisor over an empty scope must render idle, got %q:\n%s", idling, output)
	}
}

// Every LIVE state, including the two the supervisor reading must NOT change.
func TestAIRA183ConfineListLiveColumnStates(t *testing.T) {
	t.Parallel()
	live, dead, running, empty := true, false, true, false
	for _, test := range []struct {
		name       string
		subtree    *bool
		supervisor *bool
		want       string
	}{
		// A populated subtree is running whoever launched it, and a dead
		// supervisor over a POPULATED scope is emphatically not an orphan —
		// there are processes in there. AIRA-195 refines that arm exactly as
		// AIRA-183 refined the empty one: it is "running", but nothing is
		// waiting on it, so it gets its own word instead of the healthy "yes".
		{"running with a live supervisor", &running, &live, "yes"},
		{"running with a dead supervisor", &running, &dead, "unsupervised"},
		{"running with no supervisor reading", &running, nil, "yes"},
		// An unreadable population stays unevaluated whatever the supervisor did.
		{"unreadable population", nil, &live, "unevaluated"},
		{"unreadable population and a dead supervisor", nil, &dead, "unevaluated"},
		// The empty cases: the supervisor reading is what refines them, and its
		// absence must fall back to exactly the pre-AIRA-183 word.
		{"empty with a live supervisor", &empty, &live, "idle"},
		{"empty with a dead supervisor", &empty, &dead, "orphaned"},
		{"empty with no supervisor reading", &empty, nil, "no"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			output := renderConfineList(t, []runner.ConfineRecord{confineLiveRecord(t, "job", 4242, test.subtree, test.supervisor)})
			if got := liveCell(t, output, "job"); got != test.want {
				t.Fatalf("LIVE=%q want %q:\n%s", got, test.want, output)
			}
		})
	}
}

// The false-pass direction, and the one that would actually hurt: "orphaned" is
// a claim an operator acts on by NOT investigating and by trusting a kill
// landed. It may be printed only when the supervisor was positively established
// dead — never from an unreadable or vetoed reading.
func TestAIRA183ConfineListNeverCallsAnUnestablishedSupervisorAnOrphan(t *testing.T) {
	t.Parallel()
	empty := false
	output := renderConfineList(t, []runner.ConfineRecord{confineLiveRecord(t, "job", 4242, &empty, nil)})
	if strings.Contains(output, "orphaned") {
		t.Fatalf("an unevaluated supervisor was rendered as an orphan:\n%s", output)
	}
	if strings.Contains(output, "idle") {
		t.Fatalf("an unevaluated supervisor was rendered as alive:\n%s", output)
	}
}

// The legend is printed only for words that are on screen, and it is derived
// from the rendered cell rather than from the record a second time.
func TestAIRA183ConfineListLegendNamesOnlyThePresentStates(t *testing.T) {
	t.Parallel()
	live, dead, running, empty := true, false, true, false

	busy := renderConfineList(t, []runner.ConfineRecord{confineLiveRecord(t, "busy", 4242, &running, &live)})
	if strings.Contains(busy, "LIVE:") {
		t.Fatalf("an all-running listing must carry no legend at all:\n%s", busy)
	}

	orphan := renderConfineList(t, []runner.ConfineRecord{confineLiveRecord(t, "killed", 4242, &empty, &dead)})
	if !strings.Contains(orphan, "LIVE: orphaned = ") {
		t.Fatalf("an orphan row must be explained:\n%s", orphan)
	}
	if strings.Contains(orphan, "idle = ") {
		t.Fatalf("the legend explains a state no row shows:\n%s", orphan)
	}

	idle := renderConfineList(t, []runner.ConfineRecord{confineLiveRecord(t, "idling", 4243, &empty, &live)})
	if !strings.Contains(idle, "LIVE: idle = ") {
		t.Fatalf("an idle row must be explained:\n%s", idle)
	}
	if strings.Contains(idle, "orphaned = ") {
		t.Fatalf("the legend explains a state no row shows:\n%s", idle)
	}

	both := renderConfineList(t, []runner.ConfineRecord{
		confineLiveRecord(t, "killed", 4242, &empty, &dead),
		confineLiveRecord(t, "idling", 4243, &empty, &live),
	})
	if !strings.Contains(both, "idle = ") || !strings.Contains(both, "orphaned = ") {
		t.Fatalf("a listing showing both states must explain both:\n%s", both)
	}
}

// `aira top` keeps AIRA-102's yes/no/unevaluated deliberately (see topLiveCell's
// own comment): it redraws every second, so a dead supervisor shows itself by
// the row going static and then vanishing. This pins that as a DECISION rather
// than an oversight — if a later change wires the split in there, this test is
// the place that says the reasoning has to be revisited.
func TestAIRA183TopKeepsTheUnsplitLivenessCell(t *testing.T) {
	t.Parallel()
	dead, empty := false, false
	record := confineLiveRecord(t, "killed", 4242, &empty, &dead)
	if got := topLiveCell(record); got != "no" {
		t.Fatalf("top's live cell = %q, want the unsplit \"no\"", got)
	}
}

// AIRA-195. A nested confine moves its work to a sibling scope, so killing the
// outer scope leaves running processes whose supervisor PID is gone. The table
// said LIVE=yes for them, exactly as for a healthy job; `--list --json` already
// carried supervisor_live:false but the human faces hid it.
//
// verifies: AIRA-195 (listing half)
func TestAIRA195UnsupervisedIsItsOwnWordOnBothHumanFaces(t *testing.T) {
	t.Parallel()
	live, dead, running := true, false, true
	for _, test := range []struct {
		name       string
		subtree    *bool
		supervisor *bool
		pending    bool
		want       string
	}{
		{"running, supervisor gone", &running, &dead, false, "unsupervised"},
		{"running, supervisor alive", &running, &live, false, "yes"},
		// An unestablished supervisor makes no claim: never "unsupervised".
		{"running, supervisor unread", &running, nil, false, "yes"},
		{"unreadable population, supervisor gone", nil, &dead, false, "unevaluated"},
		{"pending waiter", nil, nil, true, "pending"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			record := confineLiveRecord(t, "job", 4242, test.subtree, test.supervisor)
			record.Pending = test.pending
			if got := topLiveCell(record); got != test.want {
				t.Fatalf("top live cell = %q, want %q", got, test.want)
			}
			if test.pending {
				return
			}
			if got := confineLiveStatus(record); got != test.want {
				t.Fatalf("confine --list LIVE = %q, want %q", got, test.want)
			}
		})
	}
	// The empty arm is untouched on top: still the unsplit "no".
	empty := false
	if got := topLiveCell(confineLiveRecord(t, "gone", 4242, &empty, &dead)); got != "no" {
		t.Fatalf("empty scope, dead supervisor: top cell = %q, want no", got)
	}
}

func TestAIRA195LegendExplainsUnsupervisedOnlyWhenARowShowsIt(t *testing.T) {
	t.Parallel()
	live, dead, running := true, false, true
	healthy := renderConfineList(t, []runner.ConfineRecord{confineLiveRecord(t, "busy", 4242, &running, &live)})
	if strings.Contains(healthy, "unsupervised") || strings.Contains(healthy, "LIVE:") {
		t.Fatalf("a healthy listing must carry no unsupervised word or legend:\n%s", healthy)
	}
	output := renderConfineList(t, []runner.ConfineRecord{
		confineLiveRecord(t, "orphan-run", 4242, &running, &dead),
		confineLiveRecord(t, "busy", 4243, &running, &live),
	})
	if got := liveCell(t, output, "orphan-run"); got != "unsupervised" {
		t.Fatalf("LIVE=%q, want unsupervised:\n%s", got, output)
	}
	if got := liveCell(t, output, "busy"); got != "yes" {
		t.Fatalf("healthy sibling LIVE=%q, want yes:\n%s", got, output)
	}
	if !strings.Contains(output, "unsupervised = processes are still running in the scope but its supervisor PID is gone") {
		t.Fatalf("an unsupervised row must be explained:\n%s", output)
	}
	// The clause must not claim anything about the reserve, and must not call
	// the scope removable: it is running.
	clause := output[strings.Index(output, "unsupervised = "):]
	for _, forbidden := range []string{"reaper", "nothing left to kill", "reserve"} {
		if strings.Contains(clause, forbidden) {
			t.Fatalf("unsupervised legend clause contains %q: %s", forbidden, clause)
		}
	}
}

// The machine-readable record is unchanged: supervisor_live was already there.
func TestAIRA195JSONRecordIsUnchanged(t *testing.T) {
	t.Parallel()
	dead, running := false, true
	record := confineLiveRecord(t, "orphan-run", 4242, &running, &dead)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"supervisor_live":false`) || strings.Contains(string(data), "unsupervised") {
		t.Fatalf("record JSON changed: %s", data)
	}
}
