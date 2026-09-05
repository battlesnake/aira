//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

func int64ptr(value int64) *int64 { return &value }

// verifies: AIRA-70 + AIRA-91 Part A -- the terminal classifier reports only
// what its evidence establishes. Every branch of the documented order is
// exercised, in BOTH directions: each row names the one verdict that is honest
// for its evidence, so a classifier that collapses to a single value (the
// obvious false-pass shape) fails several rows at once.
func TestClassifyConfineTermination(t *testing.T) {
	signalled := func(sig syscall.Signal) confineTermination {
		return confineTermination{Decoded: true, Signaled: true, Signal: sig}
	}
	exited := confineTermination{Decoded: true}
	// Both counters set together, EXCEPT where a row deliberately separates them:
	// the classifier reads memory.events.LOCAL, and the descendant rows below
	// pin that it ignores the hierarchical one.
	readable := func(count int64) cgroupUsage {
		return cgroupUsage{OOMKill: int64ptr(count), OOMKillLocal: int64ptr(count), OOMGroupKillLocal: int64ptr(0)}
	}
	// descendantOOM is the shape a worker sub-cgroup OOM-killed at ITS OWN cap
	// leaves on this scope: the hierarchical counter rises, the local one does
	// not. Measured against real cgroups by
	// TestMemoryEventsLocalDistinguishesOwnLimitFromDescendantOOM.
	descendantOOM := cgroupUsage{OOMKill: int64ptr(2), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}
	// The aitest drained-leader shape: a genuine OOM at THIS scope's cap whose
	// victim lived one cgroup down, so oom_kill (keyed on the victim) stays 0
	// while oom_group_kill (keyed on the cgroup whose memory.oom.group was
	// honoured -- ours) rises.
	drainedOOM := cgroupUsage{OOMKill: int64ptr(2), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(1)}
	unreadable := cgroupUsage{}

	for _, test := range []struct {
		name       string
		term       confineTermination
		usage      cgroupUsage
		supervisor os.Signal
		want       string
		why        string
	}{
		{
			name: "undecodable wait status is unevaluated", term: confineTermination{}, usage: readable(0),
			want: "unevaluated",
			why:  "branch 1: with no wait status there is no evidence of anything, including of a clean exit",
		},
		{
			name: "signalled child with a positive counter is an OOM", term: signalled(syscall.SIGKILL), usage: readable(1),
			want: "oom",
			why:  "branch 2: a positive memory.events oom_kill on a signalled child is a kernel-recorded fact",
		},
		{
			name: "an OOM outranks a supervisor signal that also arrived", term: signalled(syscall.SIGKILL), usage: readable(1), supervisor: syscall.SIGINT,
			want: "oom",
			why:  "branch 2 before 3: cgroup.kill never increments oom_kill, so our own teardown cannot have produced this counter",
		},
		{
			name: "a clean exit is never relabelled by a descendant's OOM", term: exited, usage: readable(3),
			want: "normal",
			why:  "branch 2 requires a kill: memcg events propagate UPWARD, so a child cgroup's OOM shows on our counter while our leader exits normally",
		},
		{
			name: "a SIGTERMed leader is never relabelled by an OOM at our own limit", term: signalled(syscall.SIGTERM), usage: readable(3),
			want: "child-signal:SIGTERM",
			why:  "branch 2 requires SIGKILL: the OOM killer and memory.oom.group deliver SIGKILL and nothing else, so a SIGTERMed leader was not OOM-killed no matter what the counter says (build-review round 1, P1)",
		},
		{
			name: "a supervisor signal outranks an OOM counter on a non-SIGKILL death", term: signalled(syscall.SIGTERM), usage: readable(3), supervisor: syscall.SIGTERM,
			want: "supervisor-signal:SIGTERM",
			why:  "branch 3: with branch 2 correctly gated on SIGKILL, our own witnessed teardown is what reaches the operator",
		},
		{
			name: "an EXTERNAL SIGKILL is not relabelled by a descendant cgroup's OOM", term: signalled(syscall.SIGKILL), usage: descendantOOM,
			want: "unattributed-sigkill",
			why:  "branch 2 reads memory.events.LOCAL: a worker sub-cgroup killed at its own cap raises this scope's HIERARCHICAL counter, and reading that one would report a systemd-oomd kill as a kernel OOM -- in exactly the aitest worker-scope configuration AIRA-91 investigated (build-review round 2, P0)",
		},
		{
			name: "a descendant cgroup's OOM does not relabel a clean exit either", term: exited, usage: descendantOOM,
			want: "normal",
			why:  "branch 4: the leader exited; a worker's OOM is a different fact, and the reserve advisory still speaks for it",
		},
		{
			name: "an OOM at our cap is seen even when the leader was drained into a sub-cgroup", term: signalled(syscall.SIGKILL), usage: drainedOOM,
			want: "oom",
			why:  "branch 2 via LocalOOM's oom_group_kill leg: aitest drains the leader into .aira-supervisor, so oom_kill (keyed on the victim's cgroup) stays 0 for a REAL OOM at our own cap -- reading it alone would report a kernel OOM of every real aitest run as unattributed-sigkill",
		},
		{
			name: "the drained-OOM shape still needs the leader to have been SIGKILLed", term: exited, usage: drainedOOM,
			want: "normal",
			why:  "branch 2's SIGKILL guard holds on this leg too",
		},
		{
			name: "supervisor signal wins over a caught-and-exited child", term: exited, usage: readable(0), supervisor: syscall.SIGTERM,
			want: "supervisor-signal:SIGTERM",
			why:  "branch 3 before 4: a child that CAUGHT our forwarded SIGTERM and exited cleanly was still terminated by us",
		},
		{
			name: "supervisor signal wins over the SIGKILL our own cleanup delivered", term: signalled(syscall.SIGKILL), usage: unreadable, supervisor: syscall.SIGINT,
			want: "supervisor-signal:SIGINT",
			why:  "branch 3 before 7: cleanup()'s cgroup.kill is ours, and the scope it removed is why the counter is unreadable",
		},
		{
			name: "a plain exit is normal", term: exited, usage: readable(0),
			want: "normal",
			why:  "branch 4 -- the facet reports HOW the job ended, not whether it succeeded (the /bin/false row of the trailer test pins the non-zero case end to end)",
		},
		{
			name: "a crashing child names its own signal", term: signalled(syscall.SIGSEGV), usage: readable(0),
			want: "child-signal:SIGSEGV",
			why:  "branch 5 before 7: cgroup.kill and memory.oom.group deliver SIGKILL and nothing else, so a SIGSEGV is not an unattributed SIGKILL",
		},
		{
			name: "a non-SIGKILL signal is named even with an unreadable counter", term: signalled(syscall.SIGABRT), usage: unreadable,
			want: "child-signal:SIGABRT",
			why:  "branch 5 before 6: no counter is needed to rule out an OOM, which always kills with SIGKILL",
		},
		{
			name: "SIGKILL with an unreadable counter is unevaluated", term: signalled(syscall.SIGKILL), usage: unreadable,
			want: "unevaluated",
			why:  "branch 6 before 7: OOM and an unattributed kill are indistinguishable here; claiming either would be a fabricated zero",
		},
		{
			name: "SIGKILL with a readable zero counter is unattributed", term: signalled(syscall.SIGKILL), usage: readable(0),
			want: "unattributed-sigkill",
			why:  "branch 7: AIRA-91's case -- the kill is real, this supervisor did not send it, and this scope records no OOM",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyConfineTermination(test.term, test.usage, test.supervisor); got != test.want {
				t.Fatalf("classify = %q, want %q (%s)", got, test.want, test.why)
			}
		})
	}
}

// verifies: the terminal facet reaches the operator-facing line, and an unset
// facet reads as unevaluated rather than as an empty claim.
func TestFormatConfineStatusReportsTerminatedByFacet(t *testing.T) {
	for _, test := range []struct {
		name   string
		status ConfineStatus
		want   string
	}{
		{name: "unset", status: ConfineStatus{Slice: "finite.slice"}, want: "terminated-by=unevaluated"},
		{name: "normal", status: ConfineStatus{Slice: "finite.slice", TerminatedBy: "normal"}, want: "terminated-by=normal"},
		{name: "unattributed", status: ConfineStatus{Slice: "finite.slice", TerminatedBy: "unattributed-sigkill"}, want: "terminated-by=unattributed-sigkill"},
		{name: "supervisor", status: ConfineStatus{Slice: "finite.slice", TerminatedBy: "supervisor-signal:SIGTERM"}, want: "terminated-by=supervisor-signal:SIGTERM"},
	} {
		t.Run(test.name, func(t *testing.T) {
			line := FormatConfineStatus(test.status)
			if !strings.Contains(line, test.want) {
				t.Fatalf("status %q lacks %q", line, test.want)
			}
		})
	}
}

// verifies: AIRA-91 Part A -- the candidates line is emitted for, and ONLY for,
// the unattributed-SIGKILL verdict, and it names the candidate mechanisms
// without asserting any one of them.
func TestFormatConfineTerminationAdvisory(t *testing.T) {
	quiet := cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}
	advisory := formatConfineTerminationAdvisory("unattributed-sigkill", quiet)
	for _, want := range []string{
		"SIGKILL", "cannot attribute", "Candidates", "systemd-oomd", "aira confine --kill", "cgroup.kill",
		"kill -9", "killing itself",
	} {
		if !strings.Contains(advisory, want) {
			t.Fatalf("candidates line %q lacks %q", advisory, want)
		}
	}
	// It must name the LOCAL counter, which is the one the verdict was actually
	// derived from. Saying "memory.events" flat would be a false statement in the
	// descendant-OOM case below, and would contradict the reserve advisory that
	// may be printing an OOM line directly above it (build-review round 3, P1).
	if !strings.Contains(advisory, "memory.events.local") {
		t.Fatalf("candidates line does not say WHICH counter it checked: %q", advisory)
	}
	if strings.Contains(advisory, "cgroup BENEATH") {
		t.Fatalf("a scope with no descendant OOM wrongly mentions one: %q", advisory)
	}

	// With a descendant's OOM on the hierarchical counter, the line must say so
	// rather than flatly claiming no OOM was recorded.
	descendant := formatConfineTerminationAdvisory("unattributed-sigkill", cgroupUsage{OOMKill: int64ptr(2), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)})
	for _, want := range []string{"memory.events.local", "cgroup BENEATH this scope", "2 OOM kill(s)"} {
		if !strings.Contains(descendant, want) {
			t.Fatalf("descendant-OOM candidates line %q lacks %q", descendant, want)
		}
	}

	for _, verdict := range []string{"normal", "oom", "unevaluated", "child-signal:SIGSEGV", "supervisor-signal:SIGTERM", ""} {
		if got := formatConfineTerminationAdvisory(verdict, quiet); got != "" {
			t.Fatalf("verdict %q wrongly produced a candidates line: %q", verdict, got)
		}
	}
}

// verifies: build-review round 3, P1 -- once the run has ended, a signal to the
// supervisor terminated nothing. It must not be recorded as the cause of death,
// and above all it must not tear the scope down out from under the post-run
// reads, which would silently degrade an honest verdict to `unevaluated` and
// lose the peak-RSS report with it.
//
// The fixture drives the exact shape: `readUsage` is the first thing that runs
// after the cut-off, so it delivers the late signal from inside itself and then
// returns readable counters, standing in for a scope that is still there to be
// read. A handler that still ran cleanup() would have removed it.
// watchedWriter is a race-free diagnostics sink that closes `seen` the moment a
// given substring has been written. It replaces a plain bytes.Buffer, which the
// signal-handler goroutine and the test goroutine would otherwise share
// unsynchronised.
type watchedWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	seen   chan struct{}
	once   sync.Once
}

func newWatchedWriter(marker string) *watchedWriter {
	return &watchedWriter{marker: marker, seen: make(chan struct{})}
}

func (w *watchedWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(payload)
	if strings.Contains(w.buf.String(), w.marker) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *watchedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestConfineTrailerIgnoresASignalThatArrivesAfterTheRunEnded(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	signals := make(chan os.Signal, 1)
	deps.signalSource = func() (<-chan os.Signal, func()) { return signals, func() {} }
	deps.reportPeak = func(context.Context, ConfineRequest, string, *int64, bool) error { return nil }

	// Synchronise on the WRITE, not on a poll or a sleep. The earlier version of
	// this fixture polled for the handler having dequeued the signal, which is
	// not the same event as the handler having WRITTEN, and it flaked under
	// goroutine starvation on a loaded box (observed at load average ~1000).
	// Blocking readUsage until the line appears makes the ordering a fact rather
	// than a hope, and the generous deadline exists only so a genuine regression
	// fails loudly instead of hanging.
	diagnostics := newWatchedWriter("after the job had already ended")
	var once sync.Once
	deps.readUsage = func(string) cgroupUsage {
		once.Do(func() {
			signals <- syscall.SIGTERM
			select {
			case <-diagnostics.seen:
			case <-time.After(60 * time.Second):
			}
		})
		return cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}
	}

	// A self-SIGKILL, so the verdict before the late signal is unattributed.
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "kill -s KILL $$"},
		SelfPath: os.Args[0], Stderr: diagnostics,
	}, deps); err != nil {
		t.Fatalf("confine: %v (diagnostics=%q)", err, diagnostics.String())
	}
	trailer := diagnostics.String()
	if strings.Contains(trailer, "terminated-by=supervisor-signal") {
		t.Fatalf("a signal that arrived after the job ended was recorded as its cause: %q", trailer)
	}
	if !strings.Contains(trailer, "terminated-by=unattributed-sigkill") {
		t.Fatalf("the verdict established before the late signal did not survive it: %q", trailer)
	}
	if !strings.Contains(trailer, "after the job had already ended") {
		t.Fatalf("the late signal was not reported at all, so it is as invisible as AIRA-70 found it: %q", trailer)
	}
	if strings.Contains(trailer, ", forwarding to the confined job") {
		t.Fatalf("a late signal claimed to be killing a job that had already ended: %q", trailer)
	}
}

// confineTrailer runs one real child to completion through the unit harness and
// returns the diagnostics the supervisor printed. Every target here terminates
// ITSELF, so no case depends on the test delivering a signal at the right
// moment: the classification is a function of the child's own exit shape and
// the injected usage counters alone.
func confineTrailer(t *testing.T, argv []string, usage cgroupUsage, scopeMemoryMax int64) string {
	t.Helper()
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.readUsage = func(string) cgroupUsage { return usage }
	deps.reportPeak = func(context.Context, ConfineRequest, string, *int64, bool) error { return nil }
	if scopeMemoryMax > 0 {
		deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	}
	var diagnostics bytes.Buffer
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: argv, SelfPath: os.Args[0],
		ScopeMemoryMax: scopeMemoryMax, Stderr: &diagnostics,
	}, deps); err != nil {
		t.Fatalf("confine: %v (diagnostics=%q)", err, diagnostics.String())
	}
	return diagnostics.String()
}

// verifies: AIRA-70 + AIRA-91 Part A -- the trailer distinguishes the terminal
// shapes that are byte-identical today. Five sub-cases deliberately: a trailer
// test with only the SIGKILL row passes against a classifier that hardcodes one
// verdict, which is precisely the false-pass this fix must not ship with.
func TestConfineTrailerReportsTerminationFacet(t *testing.T) {
	selfKill := []string{"/bin/sh", "-c", "kill -s KILL $$"}
	for _, test := range []struct {
		name       string
		argv       []string
		usage      cgroupUsage
		want       string
		candidates bool
	}{
		{name: "clean exit", argv: []string{"/bin/true"}, usage: cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}, want: "terminated-by=normal"},
		{name: "non-zero exit is still normal", argv: []string{"/bin/false"}, usage: cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}, want: "terminated-by=normal"},
		{
			name: "crashing child", argv: []string{"/bin/sh", "-c", "kill -s USR1 $$"},
			usage: cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}, want: "terminated-by=child-signal:SIGUSR1",
		},
		{
			name: "SIGKILL with a readable zero counter", argv: selfKill,
			usage: cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}, want: "terminated-by=unattributed-sigkill", candidates: true,
		},
		{
			name: "SIGKILL with an unreadable counter", argv: selfKill,
			usage: cgroupUsage{}, want: "terminated-by=unevaluated",
		},
		{
			// The whole trailer, end to end, for the AIRA-91 shape that the
			// hierarchical counter used to mislabel: a worker sub-cgroup OOMed
			// at its own cap, and then the WHOLE scope was killed from outside.
			name: "SIGKILL alongside a descendant cgroup's OOM", argv: selfKill,
			usage: cgroupUsage{OOMKill: int64ptr(2), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)},
			want:  "terminated-by=unattributed-sigkill", candidates: true,
		},
		{
			// And the reverse: an OOM at this scope's OWN limit still reads as
			// an OOM, so the local-counter gate did not simply disable branch 2.
			name: "SIGKILL with an OOM at our own limit", argv: selfKill,
			usage: cgroupUsage{OOMKill: int64ptr(1), OOMKillLocal: int64ptr(1), OOMGroupKillLocal: int64ptr(0)},
			want:  "terminated-by=oom",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			trailer := confineTrailer(t, test.argv, test.usage, 0)
			if !strings.Contains(trailer, test.want) {
				t.Fatalf("trailer %q lacks %q", trailer, test.want)
			}
			hasCandidates := strings.Contains(trailer, "cannot attribute")
			if hasCandidates != test.candidates {
				t.Fatalf("candidates line present=%v, want %v; trailer=%q", hasCandidates, test.candidates, trailer)
			}
		})
	}
}

// verifies: AIRA-70 finding #3 -- a kernel OOM is now reported on the status
// line whether or not a scope cap happened to be configured, which is the case
// formatConfineReserveAdvisory has always been silent for. The second sub-case
// pins the classifier's `signalled` requirement: a descendant cgroup's OOM
// (which propagates up onto our counter) must not relabel a clean exit.
func TestConfineTrailerReportsOOM(t *testing.T) {
	t.Run("uncapped OOM is no longer silent", func(t *testing.T) {
		trailer := confineTrailer(t, []string{"/bin/sh", "-c", "kill -s KILL $$"}, cgroupUsage{OOMKill: int64ptr(1), OOMKillLocal: int64ptr(1), OOMGroupKillLocal: int64ptr(0)}, 0)
		if !strings.Contains(trailer, "terminated-by=oom") {
			t.Fatalf("trailer %q lacks terminated-by=oom", trailer)
		}
		if !strings.Contains(trailer, "scope-memory.max=not-requested") {
			t.Fatalf("fixture no longer runs uncapped, so it cannot pin AIRA-70 #3: %q", trailer)
		}
		if strings.Contains(trailer, "OOM-killed at its memory cap") {
			t.Fatalf("the cap-gated reserve advisory fired without a cap: %q", trailer)
		}
		if strings.Contains(trailer, "cannot attribute") {
			t.Fatalf("an attributed OOM wrongly printed the candidates line: %q", trailer)
		}
	})
	// Renamed and re-shaped on build-review round 4: this used to pass
	// OOMKillLocal: 1, which is the OWN-limit shape, so it proved nothing about
	// descendants. The descendant shape is a positive HIERARCHICAL counter with
	// both local kill counters at zero.
	t.Run("a descendant cgroup's OOM does not relabel a clean exit", func(t *testing.T) {
		trailer := confineTrailer(t, []string{"/bin/true"},
			cgroupUsage{OOMKill: int64ptr(2), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0), OOMLocal: int64ptr(0)}, 0)
		if !strings.Contains(trailer, "terminated-by=normal") {
			t.Fatalf("trailer %q lacks terminated-by=normal", trailer)
		}
	})

	// AIRA-27's slice-OOM collateral shape. The facet is honestly `oom` -- the
	// OOM killer really did kill this job -- but the trailer must not let the
	// operator conclude their own cap was too small, because it never fired.
	t.Run("an ancestor's limit is named as such, not as this job's own cap", func(t *testing.T) {
		collateral := cgroupUsage{
			OOMKill: int64ptr(2), OOMKillLocal: int64ptr(2), OOMGroupKillLocal: int64ptr(1),
			OOMLocal: int64ptr(0), // the max-breach landed on the ANCESTOR
		}
		trailer := confineTrailer(t, []string{"/bin/sh", "-c", "kill -s KILL $$"}, collateral, 0)
		if !strings.Contains(trailer, "terminated-by=oom") {
			t.Fatalf("an ancestor OOM that killed our processes is still an OOM kill: %q", trailer)
		}
		if !strings.Contains(trailer, "did NOT fire at this scope's own limit") {
			t.Fatalf("slice-level collateral was reported as though this job hit its own cap: %q", trailer)
		}
		if strings.Contains(trailer, "fired at this scope's OWN memory limit") {
			t.Fatalf("contradictory limit attribution: %q", trailer)
		}
	})
	t.Run("our own limit is named as such", func(t *testing.T) {
		own := cgroupUsage{
			OOMKill: int64ptr(2), OOMKillLocal: int64ptr(2), OOMGroupKillLocal: int64ptr(1), OOMLocal: int64ptr(1),
		}
		trailer := confineTrailer(t, []string{"/bin/sh", "-c", "kill -s KILL $$"}, own, 0)
		if !strings.Contains(trailer, "fired at this scope's OWN memory limit") {
			t.Fatalf("an own-limit OOM was not named as such: %q", trailer)
		}
	})
	t.Run("an unreadable limit counter says nothing about whose limit fired", func(t *testing.T) {
		trailer := confineTrailer(t, []string{"/bin/sh", "-c", "kill -s KILL $$"},
			cgroupUsage{OOMKill: int64ptr(2), OOMKillLocal: int64ptr(2), OOMGroupKillLocal: int64ptr(1)}, 0)
		if !strings.Contains(trailer, "terminated-by=oom") {
			t.Fatalf("trailer %q lacks terminated-by=oom", trailer)
		}
		if strings.Contains(trailer, "own memory limit") || strings.Contains(trailer, "did NOT fire") {
			t.Fatalf("whose limit fired was claimed from an unread counter: %q", trailer)
		}
	})
	t.Run("a capped OOM still gets the reserve advisory", func(t *testing.T) {
		trailer := confineTrailer(t, []string{"/bin/sh", "-c", "kill -s KILL $$"},
			cgroupUsage{OOMKill: int64ptr(1), OOMKillLocal: int64ptr(1), OOMGroupKillLocal: int64ptr(0), PeakRSS: int64ptr(31 << 20)}, 32<<20)
		if !strings.Contains(trailer, "terminated-by=oom") || !strings.Contains(trailer, "OOM-killed at its memory cap") {
			t.Fatalf("capped OOM lost a line: %q", trailer)
		}
	})
}

// verifies: AIRA-70 finding #1 -- a signal delivered to the confine supervisor
// itself is recorded, forwarded, and named on the trailer. It used to leave no
// trace anywhere. The assertion is on the FACET, never on the exit code: in
// production cleanup()'s cgroup.kill races the forwarded signal, so the child's
// wait status is 137 or 143 depending on which lands first.
func TestConfineTrailerReportsSupervisorSignal(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ready")
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	signals := make(chan os.Signal, 1)
	deps.signalSource = func() (<-chan os.Signal, func()) { return signals, func() {} }
	deps.readUsage = func(string) cgroupUsage { return cgroupUsage{} }
	deps.reportPeak = func(context.Context, ConfineRequest, string, *int64, bool) error { return nil }

	// The child announces itself, then becomes a single `sleep` via exec, so the
	// signal is delivered to a running job and nothing is orphaned when it dies.
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(testdeadline.Wait(10 * time.Second))
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				signals <- syscall.SIGTERM
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var diagnostics bytes.Buffer
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice:    "finite.slice",
		Argv:     []string{"/bin/sh", "-c", `echo ready > "$1"; exec sleep 60`, "sh", marker},
		SelfPath: os.Args[0], Stderr: &diagnostics,
	}, deps)
	<-done
	if err != nil {
		t.Fatalf("confine: %v (diagnostics=%q)", err, diagnostics.String())
	}
	trailer := diagnostics.String()
	if !strings.Contains(trailer, "terminated-by=supervisor-signal:SIGTERM") {
		t.Fatalf("trailer %q lacks terminated-by=supervisor-signal:SIGTERM", trailer)
	}
	if !strings.Contains(trailer, "confine: received SIGTERM") {
		t.Fatalf("the supervisor's own signal left no log line: %q", trailer)
	}
	if strings.Contains(trailer, "cannot attribute") {
		t.Fatalf("a kill this supervisor caused wrongly printed the candidates line: %q", trailer)
	}
	if count := strings.Count(trailer, "confine: received SIGTERM"); count != 1 {
		t.Fatalf("supervisor signal logged %d times, want exactly 1: %q", count, trailer)
	}
}

// verifies: the signal handler's stop function JOINS its goroutine, so nothing
// the handler writes -- the AIRA-70 log line, the witness the trailer reads --
// can land after confineWithDeps has returned and its caller has already read
// the diagnostics. Before the join the stop function only closed a channel, so
// a signal landing at the moment of return raced the caller (build-review P2).
//
// The fixture holds the handler inside onSignal until after stop is called: if
// stop did not wait, it would return while the callback was still running, and
// the post-stop read of `running` would see true.
func TestForwardConfineSignalsStopJoinsTheHandler(t *testing.T) {
	forward := make(chan os.Signal, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	running := false
	stop := forwardConfineSignals(forward, func() *os.Process { return nil }, func(os.Signal) {
		mu.Lock()
		running = true
		mu.Unlock()
		close(entered)
		<-release
		mu.Lock()
		running = false
		mu.Unlock()
	})
	forward <- syscall.SIGTERM
	<-entered
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while the handler callback was still running: it did not join")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("stop never returned after the handler finished")
	}
	mu.Lock()
	stillRunning := running
	mu.Unlock()
	if stillRunning {
		t.Fatal("handler still running after stop returned")
	}
	// Idempotent: the production path calls stop from a defer, and a double call
	// must not panic on a re-closed channel.
	stop()
}
