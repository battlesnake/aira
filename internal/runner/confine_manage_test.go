package runner

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestOrphanedConfineScopeCandidatesRequireEveryPositiveProof(t *testing.T) {
	intPtr := func(value int) *int { return &value }
	int64Ptr := func(value int64) *int64 { return &value }
	record := func(scopeID string) ConfineRecord {
		return ConfineRecord{
			ScopeID:       scopeID,
			Populated:     intPtr(0),
			SupervisorPID: intPtr(101),
			AgeSeconds:    int64Ptr(120),
		}
	}

	reap := record("CONFINE-reap-101-1")
	populated := record("CONFINE-populated-102-1")
	populated.Populated = intPtr(1)
	populated.SupervisorPID = intPtr(102)
	unknownPopulation := record("CONFINE-population-unknown-103-1")
	unknownPopulation.Populated = nil
	unknownPopulation.SupervisorPID = intPtr(103)
	unknownSupervisor := record("CONFINE-supervisor-unknown-104-1")
	unknownSupervisor.SupervisorPID = nil
	aliveSupervisor := record("CONFINE-alive-105-1")
	aliveSupervisor.SupervisorPID = intPtr(105)
	young := record("CONFINE-young-106-1")
	young.SupervisorPID = intPtr(106)
	young.AgeSeconds = int64Ptr(119)
	unknownAge := record("CONFINE-age-unknown-107-1")
	unknownAge.SupervisorPID = intPtr(107)
	unknownAge.AgeSeconds = nil
	pending := record("CONFINE-pending-108-1")
	pending.SupervisorPID = intPtr(108)
	pending.Pending = true
	leased := record("CONFINE-leased-109-1")
	leased.SupervisorPID = intPtr(109)

	deadCalls := make(map[int]int)
	got := orphanedConfineScopeCandidates([]ConfineRecord{
		reap,
		populated,
		unknownPopulation,
		unknownSupervisor,
		aliveSupervisor,
		young,
		unknownAge,
		pending,
		leased,
	}, 2*time.Minute, func(pid int) bool {
		deadCalls[pid]++
		return pid != 105
	}, func(scopeID string) bool {
		return scopeID == leased.ScopeID
	})

	if !reflect.DeepEqual(got, []ConfineRecord{reap}) {
		t.Fatalf("candidates=%+v, want only %+v", got, reap)
	}
	// A live daemon lease keeps the scope even though it is empty+dead+old, and its
	// PID gate is still evaluated (the lease check is last in the disjunction).
	if want := map[int]int{101: 1, 105: 1, 106: 1, 107: 1, 108: 1, 109: 1}; !reflect.DeepEqual(deadCalls, want) {
		t.Fatalf("supervisorDead calls=%v, want %v", deadCalls, want)
	}
	if got := orphanedConfineScopeCandidates([]ConfineRecord{reap}, 2*time.Minute, nil, nil); len(got) != 0 {
		t.Fatalf("nil supervisor-death check selected candidates=%+v", got)
	}
}

// AIRA-135. The wrapped command is extracted from the supervisor's own
// NUL-separated argv at the FIRST bare `--`, which is where `aira confine`'s own
// flags stop and the job begins.
//
// verifies: AIRA-135
func TestConfineCommandFromCmdlineSplitsAtTheFirstBareSeparator(t *testing.T) {
	cmdline := func(argv ...string) []byte {
		out := make([]byte, 0, 64)
		for _, arg := range argv {
			out = append(out, arg...)
			out = append(out, 0)
		}
		return out
	}
	for _, testCase := range []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{
			// The ordinary shape. aira's own flags are NOT part of the answer: an
			// implementation that returned the whole argv would show every operator
			// the same `aira confine --slice ...` prefix instead of the job.
			name: "wrapped-command-after-the-separator",
			data: cmdline("aira", "confine", "--name", "build", "--", "go", "test", "./..."),
			want: "go test ./...", ok: true,
		},
		{
			// A `--` INSIDE the wrapped command belongs to the command. Splitting at
			// the last one, or at every one, loses part of the real invocation.
			name: "later-separators-belong-to-the-command",
			data: cmdline("aira", "confine", "--", "go", "test", "--", "-run", "TestX"),
			want: "go test -- -run TestX", ok: true,
		},
		{
			// Documented fallback: no separator at all yields the whole argv rather
			// than dropping the field.
			name: "no-separator-falls-back-to-the-whole-argv",
			data: cmdline("aira", "confine", "--list"),
			want: "aira confine --list", ok: true,
		},
		{
			// An empty argument inside the command is real and is kept; only the
			// NUL terminator's trailing empty is dropped.
			name: "empty-argument-is-kept",
			data: cmdline("aira", "confine", "--", "sh", "-c", ""),
			want: "sh -c ", ok: true,
		},
		{"empty-cmdline", nil, "", false},
		{"only-nuls", []byte{0, 0, 0}, "", false},
		{
			// `--` with nothing after it launched nothing. An empty string here
			// would render as a job running no command at all.
			name: "separator-with-nothing-after-it",
			data: cmdline("aira", "confine", "--"),
			want: "", ok: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := confineCommandFromCmdline(testCase.data)
			if ok != testCase.ok || got != testCase.want {
				t.Fatalf("confineCommandFromCmdline=%q,%v want %q,%v", got, ok, testCase.want, testCase.ok)
			}
		})
	}
}

// An over-long argv is bounded for availability (a `confine --list` reply must
// not be pushed past MaxFrameBytes by one absurd command line), and the elision
// is MARKED rather than silent — a truncated command that looked complete would
// be a fabrication.
//
// verifies: AIRA-135
func TestConfineCommandFromCmdlineMarksAnElidedArgv(t *testing.T) {
	data := append([]byte("aira\x00confine\x00--\x00"), bytes.Repeat([]byte("x"), 2*ConfineCommandWireLimit)...)
	got, ok := confineCommandFromCmdline(data)
	if !ok {
		t.Fatal("an over-long command was dropped entirely")
	}
	if !strings.HasSuffix(got, " …") {
		t.Fatalf("elided command=%q, want a marked elision", got)
	}
	if len(got) > ConfineCommandWireLimit+len(" …") {
		t.Fatalf("elided command is %d bytes, want it bounded by %d", len(got), ConfineCommandWireLimit)
	}
	// A cut that lands mid-rune must not put invalid UTF-8 on the wire and into a
	// terminal.
	multibyte := append([]byte("aira\x00confine\x00--\x00"), bytes.Repeat([]byte("é"), ConfineCommandWireLimit)...)
	cut, ok := confineCommandFromCmdline(multibyte)
	if !ok || !utf8.ValidString(cut) {
		t.Fatalf("mid-rune cut produced %q (valid=%v)", cut, utf8.ValidString(cut))
	}
}

// TestCheckConfineOwnerVerdictWithSessionOwners pins the guard's VERDICT as a
// function of (owner, caller, steal) over every owner form, including the
// Claude Code session owner AIRA-313 adds: it opens only when both sides are
// attested and equal, or on --steal. Two identical unattested values (an
// inferred @cwd owner, the literal unknown) must still refuse.
//
// verifies: AIRA-313
func TestCheckConfineOwnerVerdictWithSessionOwners(t *testing.T) {
	sessionX := "cc-b6b9daa7-4e9b-4de2-b1be-fff8ea5fb6d2"
	sessionY := "cc-0d4c5f1e-9a2b-4c3d-8e7f-001122334455"
	worktree := strings.Repeat("7", 64)
	for _, test := range []struct {
		owner, caller string
		open          bool
	}{
		{sessionX, sessionX, true},
		{sessionX, sessionY, false},
		{sessionY, sessionX, false},
		{sessionX, worktree, false},
		{worktree, sessionX, false},
		{worktree, worktree, true},
		{sessionX, "@cwd-x", false},
		{"@cwd-x", sessionX, false},
		{"@cwd-x", "@cwd-x", false},
		{ConfineUnknownOwner, ConfineUnknownOwner, false},
		{"", ConfineUnknownOwner, false},
		{"", sessionX, false},
		{sessionX, "", false},
	} {
		err := CheckConfineOwner("CONFINE-job-1-a", test.owner, test.caller, false)
		if (err == nil) != test.open {
			t.Fatalf("owner=%q caller=%q steal=false: err=%v, want open=%v", test.owner, test.caller, err, test.open)
		}
		if err != nil && !strings.HasPrefix(err.Error(), CodeConfineOwnerUnverified+":") {
			t.Fatalf("owner=%q caller=%q: refusal %q lacks the stable code", test.owner, test.caller, err)
		}
		if err := CheckConfineOwner("CONFINE-job-1-a", test.owner, test.caller, true); err != nil {
			t.Fatalf("owner=%q caller=%q steal=true: %v", test.owner, test.caller, err)
		}
	}
}

// TestCheckConfineOwnerRefusalSaysWhy pins the three refusal reasons (AIRA-313):
// the job's owner was inferred; the caller's identity was inferred; both are
// attested but differ. Each keeps the stable leading fields, says which case it
// is, and none ever suggests passing --owner (that would invite copying the
// victim's printed owner, an override the audit line records as steal=false).
//
// verifies: AIRA-313
func TestCheckConfineOwnerRefusalSaysWhy(t *testing.T) {
	sessionX := "cc-b6b9daa7-4e9b-4de2-b1be-fff8ea5fb6d2"
	sessionY := "cc-0d4c5f1e-9a2b-4c3d-8e7f-001122334455"
	const (
		ownerPhrase  = "the job's owner was inferred"
		callerPhrase = "your own identity was inferred"
		differPhrase = "belongs to a different session or worktree"
	)
	for _, test := range []struct {
		name, owner, caller string
		want                string
		notWant             []string
		extra               []string
	}{
		{"job owner inferred", "@cwd-kit-fix", sessionX, ownerPhrase, []string{callerPhrase, differPhrase}, []string{"--steal", "AIRA_CONFINE_OWNER"}},
		{"job owner unknown", "", sessionX, ownerPhrase, []string{callerPhrase, differPhrase}, []string{"--steal"}},
		{"both inferred reports the job owner", "@cwd-x", "@cwd-x", ownerPhrase, []string{callerPhrase, differPhrase}, nil},
		{"caller inferred", sessionX, "@cwd-aira", callerPhrase, []string{ownerPhrase, differPhrase}, []string{"--steal", "AIRA_CONFINE_OWNER"}},
		{"caller unknown", sessionX, ConfineUnknownOwner, callerPhrase, []string{ownerPhrase, differPhrase}, nil},
		{"both attested, different", sessionX, sessionY, differPhrase, []string{ownerPhrase, callerPhrase}, []string{"AIRA_CONFINE_OWNER", "session", "worktree", "--steal"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckConfineOwner("CONFINE-job-1-a", test.owner, test.caller, false)
			if err == nil {
				t.Fatal("guard opened")
			}
			message := err.Error()
			wantOwner := test.owner
			if wantOwner == "" {
				wantOwner = ConfineUnknownOwner
			}
			for _, fact := range append([]string{
				CodeConfineOwnerUnverified + ": scope=CONFINE-job-1-a owner=" + wantOwner + " caller=" + test.caller,
				test.want,
			}, test.extra...) {
				if !strings.Contains(message, fact) {
					t.Fatalf("refusal %q lacks %q", message, fact)
				}
			}
			for _, absent := range append([]string{"--owner"}, test.notWant...) {
				if strings.Contains(message, absent) {
					t.Fatalf("refusal %q contains %q", message, absent)
				}
			}
			if err := CheckConfineOwner("CONFINE-job-1-a", test.owner, test.caller, true); err != nil {
				t.Fatalf("steal: %v", err)
			}
		})
	}
}
