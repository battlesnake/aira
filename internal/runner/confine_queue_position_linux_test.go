//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// fakeConfineListDaemon serves exactly one confine-list request over a unix
// socket, records what it was asked, and replies with the supplied result.
func fakeConfineListDaemon(t *testing.T, reply func(args map[string]any) runnerAdmitResponseFrame) (socket string, asked func() (map[string]any, int)) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen map[string]any
	var proto int
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var frame runnerAdmitRequestFrame
		if readRunnerAdmitFrame(conn, &frame) != nil {
			return
		}
		mu.Lock()
		seen = map[string]any{"verb": frame.Request.Verb}
		for name, value := range frame.Request.Args {
			seen[name] = value
		}
		proto = frame.Proto
		mu.Unlock()
		_ = writeRunnerAdmitFrame(conn, reply(frame.Request.Args))
	}()
	// Close the listener BEFORE joining: a case that deliberately never dials
	// (a probe that must not reach the daemon at all) leaves the goroutine in
	// Accept, and joining it first would deadlock the cleanup.
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return socket, func() (map[string]any, int) {
		mu.Lock()
		defer mu.Unlock()
		return seen, proto
	}
}

func confineListReply(t *testing.T, reserve *ConfineSliceReserve) runnerAdmitResponseFrame {
	t.Helper()
	data, err := json.Marshal(ConfineListResult{Verdict: "pass", Scopes: []ConfineRecord{}, SliceReserve: reserve})
	if err != nil {
		t.Fatal(err)
	}
	return runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data}
}

// AIRA-24: the blocked launcher's own queue-position probe. It must ask the
// daemon over its OWN connection (never the admission socket, whose next byte
// the daemon reads as "the client went away"), naming the resolved slice path
// and its own scope id.
//
// verifies: the probe asks confine-list with the job's scope id and returns
// the position the daemon reports.
func TestConfineQueuePositionProbeAsksTheDaemonForItsOwnScope(t *testing.T) {
	socket, asked := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
		return confineListReply(t, &ConfineSliceReserve{Queued: 3, QueuePosition: 2, QueuedAheadBytes: 2 << 30})
	})
	request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-5101-abc@session-a", Owner: "session-a"}
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(5*time.Second))
	defer cancel()
	got, ok := confineQueuePositionFromDaemon(ctx, request, "/sys/fs/cgroup/aira.slice")
	if !ok {
		t.Fatalf("probe reported no position; want one")
	}
	if got.position != 2 || got.queued != 3 || got.aheadBytes != 2<<30 {
		t.Fatalf("position=%+v, want {2 3 2G}", got)
	}
	args, proto := asked()
	if args["verb"] != "confine-list" {
		t.Fatalf("verb=%v, want confine-list", args["verb"])
	}
	if args["scope_id"] != request.ScopeID {
		t.Fatalf("scope_id=%v, want %q — without it the daemon can only answer the aggregate", args["scope_id"], request.ScopeID)
	}
	if args["slice"] != "/sys/fs/cgroup/aira.slice" {
		t.Fatalf("slice=%v, want the RESOLVED path the admit request used", args["slice"])
	}
	if args["owner"] != "session-a" {
		t.Fatalf("owner=%v, want session-a", args["owner"])
	}
	if proto != DaemonProtocolVersion {
		t.Fatalf("proto=%d, want %d", proto, DaemonProtocolVersion)
	}
}

// An owner the daemon would refuse must not cost the job its progress line:
// confine-list validates the owner but never uses it, so an unset owner asks
// under the explicit "nobody claimed this" identity instead of not asking.
//
// verifies: an empty owner is sent as ConfineUnknownOwner, not as "".
func TestConfineQueuePositionProbeSubstitutesAnUnusableOwner(t *testing.T) {
	socket, asked := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
		return confineListReply(t, &ConfineSliceReserve{Queued: 1, QueuePosition: 1})
	})
	request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-5101-abc"}
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(5*time.Second))
	defer cancel()
	if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); !ok {
		t.Fatalf("probe reported no position for a valid reply")
	}
	args, _ := asked()
	if args["owner"] != ConfineUnknownOwner {
		t.Fatalf("owner=%v, want %q", args["owner"], ConfineUnknownOwner)
	}
	if err := ValidateConfineOwner(ConfineUnknownOwner); err != nil {
		t.Fatalf("the substituted owner must be one the daemon accepts: %v", err)
	}
}

// Every probe failure is an ABSENCE of a position, never a fabricated one and
// never a new failure mode for the launch: the job is already waiting, and the
// existing progress line must survive a daemon that is down, wedged, or simply
// does not have this scope queued.
//
// verifies: no socket, a refused connection, an error frame, a missing
// summary, and a zero/inconsistent position all report "no position".
func TestConfineQueuePositionProbeReportsAbsenceNotZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(5*time.Second))
	defer cancel()
	t.Run("no-daemon-socket", func(t *testing.T) {
		if _, ok := confineQueuePositionFromDaemon(ctx, ConfineRequest{ScopeID: "CONFINE-job-1-a"}, "/slice"); ok {
			t.Fatal("a daemon-less launch must report no position")
		}
	})
	t.Run("no-scope-id", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			t.Error("a scope-less request must not reach the daemon")
			return runnerAdmitResponseFrame{}
		})
		if _, ok := confineQueuePositionFromDaemon(ctx, ConfineRequest{AdmitSocketPath: socket}, "/slice"); ok {
			t.Fatal("without a scope id there is nothing to ask about")
		}
	})
	t.Run("unreachable-socket", func(t *testing.T) {
		dead := filepath.Join(t.TempDir(), "absent.sock")
		request := ConfineRequest{AdmitSocketPath: dead, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("an unreachable daemon must report no position")
		}
	})
	t.Run("error-frame", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{OK: false, Code: "E_CONFINE_UNAVAILABLE", Error: "E_CONFINE_UNAVAILABLE: slice"}
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("a refused request must report no position")
		}
	})
	t.Run("no-slice-reserve", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return confineListReply(t, nil)
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("a daemon that could not read the slice reports no position")
		}
	})
	t.Run("scope-not-queued", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return confineListReply(t, &ConfineSliceReserve{Queued: 4})
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("position 0 is an absence, and must never render as a place in the queue")
		}
	})
	t.Run("position-past-the-queue", func(t *testing.T) {
		socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
			return confineListReply(t, &ConfineSliceReserve{Queued: 1, QueuePosition: 3})
		})
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Fatal("a self-contradictory pair must not reach an operator-facing line")
		}
	})
}

// A probe must die with the wait it decorates. The launch path joins the
// diagnostic goroutine, so a daemon that accepts a connection and then never
// answers would otherwise hold an already-granted job at the starting line.
//
// verifies: cancelling the probe context returns immediately, without waiting
// out the deadline.
func TestConfineQueuePositionProbeReturnsWhenTheWaitEnds(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var frame runnerAdmitRequestFrame
		_ = readRunnerAdmitFrame(conn, &frame)
		close(accepted)
		<-time.After(30 * time.Second) // never answers
	}()
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(30*time.Second))
	defer cancel()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-1-a"}
		if _, ok := confineQueuePositionFromDaemon(ctx, request, "/slice"); ok {
			t.Error("a silent daemon must report no position")
		}
	}()
	select {
	case <-accepted:
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("the probe never reached the daemon")
	}
	cancel()
	select {
	case <-returned:
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("the probe outlived its context; a wedged daemon would delay the launch")
	}
}

// AIRA-103. A wait under a reduced slice ceiling is NOT ordinary contention, and
// the existing line ("waiting for memory admission ... queue position N of M")
// reads as though it were: the slice can look far from full while admission is
// nonetheless closed by memory used outside it. The blocked launcher's own probe
// already carries SliceReserve, so this is a rendering change on a reply it
// already has -- no extra round trip and no new daemon verb.
//
// verifies: only an ENFORCING, throttled ceiling is announced; observe mode and
// an unevaluated ceiling are not, because neither is applied.
func TestConfineQueueNoteReportsAReducedCeilingOnlyWhenEnforced(t *testing.T) {
	for _, test := range []struct {
		name         string
		mode, state  string
		basis        string
		memAvailable int64
		want         string
		absent       string
	}{
		{
			name: "enforced-throttle", mode: "enforce", state: "throttled", basis: "system-pressure", memAvailable: 6 << 30,
			want: "slice ceiling reduced to keep the configured system free-memory reserve (system MemAvailable 6G)",
		},
		{
			name: "enforced-throttle-without-a-reading", mode: "enforce", state: "throttled", basis: "system-pressure",
			want: "slice ceiling reduced to keep the configured system free-memory reserve,", absent: "MemAvailable",
		},
		{
			// AIRA-106. The STATIC machine-reserve term reduced this ceiling.
			// Naming pressure here would send a blocked launcher looking for memory
			// nothing is using, and printing the MemAvailable figure beside it would
			// invite exactly that diagnosis -- so the figure is withheld, since it is
			// not the cause.
			name: "enforced-throttle-by-the-machine-reserve", mode: "enforce", state: "throttled",
			basis: "machine-reserve", memAvailable: 40 << 30,
			want:   "slice ceiling reduced to keep the configured share of this machine outside the slice",
			absent: "MemAvailable",
		},
		{
			// An absent or unrecognised basis names NEITHER cause.
			name: "enforced-throttle-without-a-basis", mode: "enforce", state: "throttled", memAvailable: 6 << 30,
			want: "slice ceiling reduced below the configured ceiling", absent: "free-memory reserve",
		},
		{name: "observe-applies-nothing", mode: "observe", state: "throttled", basis: "system-pressure", memAvailable: 6 << 30, absent: "slice ceiling"},
		{name: "unevaluated", mode: "enforce", state: "unevaluated", absent: "slice ceiling"},
		{name: "subsystem-off", absent: "slice ceiling"},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
				return confineListReply(t, &ConfineSliceReserve{
					Queued: 3, QueuePosition: 2, QueuedAheadBytes: 2 << 30,
					CeilingMode: test.mode, CeilingState: test.state, CeilingBasis: test.basis,
					MemAvailableBytes: test.memAvailable,
				})
			})
			request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-5101-abc@session-a", Owner: "session-a"}
			deps := fillConfineDeps(confineDeps{})
			deps.queuePosition = confineQueuePositionFromDaemon
			ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(5*time.Second))
			defer cancel()
			note := confineQueueNote(ctx, deps, request, "/slice", 0, make(chan struct{}))
			if !strings.Contains(note, "queue position 2 of 3") {
				t.Fatalf("note=%q, want the AIRA-24 position preserved", note)
			}
			if test.want != "" && !strings.Contains(note, test.want) {
				t.Fatalf("note=%q, want %q", note, test.want)
			}
			if test.absent != "" && strings.Contains(note, test.absent) {
				t.Fatalf("note=%q, must not contain %q", note, test.absent)
			}
		})
	}
}

// queueNoteFor renders one wait-line clause against a daemon that answers with
// exactly the supplied summary. It exercises the real probe, not a stub, so a
// field the probe drops on the floor cannot pass by being set in the fixture.
func queueNoteFor(t *testing.T, clientReserve int64, reserve *ConfineSliceReserve) string {
	t.Helper()
	socket, _ := fakeConfineListDaemon(t, func(map[string]any) runnerAdmitResponseFrame {
		return confineListReply(t, reserve)
	})
	request := ConfineRequest{AdmitSocketPath: socket, ScopeID: "CONFINE-job-5101-abc@session-a", Owner: "session-a"}
	deps := fillConfineDeps(confineDeps{})
	deps.queuePosition = confineQueuePositionFromDaemon
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(5*time.Second))
	defer cancel()
	return confineQueueNote(ctx, deps, request, "/slice", clientReserve, make(chan struct{}))
}

// AIRA-181. At the head of the queue "0B queued ahead" is true by definition and
// says nothing about what is actually blocking the job — the reserve already
// GRANTED to the admitted jobs, which is a different population entirely. Four
// independent sessions read the head-of-queue line as "nothing is blocking me"
// on one night: one killed a job that was correctly waiting on three running
// jobs holding ~20G, and another guessed its own reserve down from a correct 40G
// to a 30G that then OOM-killed with no results at all.
//
// The pre-existing comment in confineQueueNote predicted this misreading exactly
// and warned against a bare "reserved ahead" figure. This test holds the fix to
// that warning: BOTH populations appear, each named in its own words, so neither
// number can be read as the other.
//
// verifies: the held reserve, its job count and the ceiling reach the line
// beside the queued-ahead figure, and the two are separately labelled.
func TestConfineQueueNoteNamesTheReserveHeldByAdmittedJobs(t *testing.T) {
	note := queueNoteFor(t, 4<<30, &ConfineSliceReserve{
		Queued: 9, QueuePosition: 1, QueuedAheadBytes: 0,
		GrantedBytes: 52 << 30, Jobs: 9, CeilingBytes: 64 << 30,
	})
	if !strings.Contains(note, "queue position 1 of 9 by enqueue order, 0B queued ahead") {
		t.Fatalf("note=%q, want the AIRA-24 queue clause unchanged", note)
	}
	if !strings.Contains(note, "52G already granted across 9 admitted jobs / 64G slice ceiling") {
		t.Fatalf("note=%q, want the running set's held reserve, its job count and the ceiling", note)
	}
	// The whole failure being fixed is one population being read as the other.
	// The two figures must never share a label: a line that said "52G queued
	// ahead" or "0B granted" would be worse than the line it replaced.
	if strings.Contains(note, "52G queued ahead") || strings.Contains(note, "0B already granted") {
		t.Fatalf("note=%q, the queued and granted populations must not be confused", note)
	}
	if strings.Contains(note, "reserved ahead") {
		t.Fatalf("note=%q, the ambiguous pre-AIRA-24 wording must not return", note)
	}
}

// The false-fail direction. A held figure the daemon did not establish must
// print NOTHING: a fabricated "0B already granted across 0 admitted jobs" on a
// slice that is in fact full would state the exact opposite of the truth, and
// would be the same class of defect as the misreading this ticket fixes — only
// now asserted by AIRA itself rather than inferred by a reader.
//
// verifies: negative ledger figures and an unestablished ceiling each suppress
// only what they make unknowable, and never the queue clause.
func TestConfineQueueNoteWithholdsAnUnestablishedHeldReserve(t *testing.T) {
	t.Run("negative-granted-total", func(t *testing.T) {
		// A negative granted total is a ledger defect, not a reading. Printing
		// half of a self-contradictory pair is worse than printing none of it.
		note := queueNoteFor(t, 4<<30, &ConfineSliceReserve{
			Queued: 2, QueuePosition: 1, GrantedBytes: -1, Jobs: 3, CeilingBytes: 64 << 30,
		})
		if !strings.Contains(note, "queue position 1 of 2") {
			t.Fatalf("note=%q, the queue clause must survive a bad ledger figure", note)
		}
		if strings.Contains(note, "already granted") {
			t.Fatalf("note=%q, an unestablished held reserve must print nothing", note)
		}
	})
	t.Run("negative-job-count", func(t *testing.T) {
		note := queueNoteFor(t, 4<<30, &ConfineSliceReserve{
			Queued: 2, QueuePosition: 1, GrantedBytes: 8 << 30, Jobs: -2, CeilingBytes: 64 << 30,
		})
		if strings.Contains(note, "already granted") {
			t.Fatalf("note=%q, a negative job count must take the whole pair with it", note)
		}
	})
	t.Run("unestablished-ceiling", func(t *testing.T) {
		// The ceiling is a separate reading. Losing it must not take the held
		// figure with it, and it must never render as "0B slice ceiling", which
		// would state that the slice can admit nothing at all.
		note := queueNoteFor(t, 4<<30, &ConfineSliceReserve{
			Queued: 2, QueuePosition: 1, GrantedBytes: 8 << 30, Jobs: 2, CeilingBytes: 0,
		})
		if !strings.Contains(note, "8G already granted across 2 admitted jobs") {
			t.Fatalf("note=%q, an unreadable ceiling must not suppress the held reserve", note)
		}
		if strings.Contains(note, "slice ceiling") {
			t.Fatalf("note=%q, an unestablished ceiling must print nothing", note)
		}
	})
	t.Run("empty-but-closed-slice", func(t *testing.T) {
		// An ESTABLISHED zero is the opposite case and must be printed. A slice
		// held by a drain, a fairness freeze or a collapsed ceiling really does
		// have nothing admitted, and that is the single most useful thing the
		// line can say — suppressing it would leave the reader with exactly the
		// "nothing is blocking me" reading this clause exists to end.
		note := queueNoteFor(t, 4<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 0, Jobs: 0, CeilingBytes: 64 << 30,
		})
		if !strings.Contains(note, "0B already granted across 0 admitted jobs / 64G slice ceiling") {
			t.Fatalf("note=%q, an established empty running set is a fact and must be stated", note)
		}
	})
	t.Run("one-admitted-job-reads-as-one", func(t *testing.T) {
		note := queueNoteFor(t, 4<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 20 << 30, Jobs: 1, CeilingBytes: 64 << 30,
		})
		if !strings.Contains(note, "across 1 admitted job /") {
			t.Fatalf("note=%q, want the singular", note)
		}
	})
}

// AIRA-186. An UNPINNED request prints a compiled-in hint on its own progress
// line; the daemon replaced that hint with a history-derived estimate before the
// job was ever queued, and nothing told the client. A 35.7G estimate against a
// 61.6G ceiling then looks exactly like ordinary contention from the wait site —
// the reporter burned three 30-minute waits before pinning 24G admitted the same
// job in under 25 seconds. Their own follow-up confirmed the ESTIMATE was right
// (measured peak 31.97 GiB); what was missing was any way to SEE it.
//
// verifies: a resolved reserve that differs from the printed figure is stated,
// and one that matches it is not repeated.
func TestConfineQueueNoteNamesTheJobsOwnResolvedReserve(t *testing.T) {
	t.Run("unpinned-hint-replaced-by-the-daemon", func(t *testing.T) {
		note := queueNoteFor(t, 2<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 40 << 30, Jobs: 3, CeilingBytes: 61 << 30,
			ResolvedReserveBytes: 35 << 30,
		})
		if !strings.Contains(note, "this job's own reserve resolves to 35G") {
			t.Fatalf("note=%q, want the resolved reserve the daemon is actually gating on", note)
		}
		// The hint is not the number the slice is contending over, and must not
		// be presented as though it were.
		if strings.Contains(note, "own reserve resolves to 2G") || strings.Contains(note, "own reserve is 2G") {
			t.Fatalf("note=%q, the client's replaced hint must never be named as this job's reserve", note)
		}
	})
	t.Run("pinned-figure-is-not-repeated", func(t *testing.T) {
		// A pinned request is honoured verbatim, so the resolved figure is the
		// one already at the head of the line. Restating it every 15 seconds is
		// noise, and "resolves to" would imply the daemon moved a number it did
		// not.
		note := queueNoteFor(t, 44<<30, &ConfineSliceReserve{
			Queued: 9, QueuePosition: 1, GrantedBytes: 52 << 30, Jobs: 9, CeilingBytes: 64 << 30,
			ResolvedReserveBytes: 44 << 30,
		})
		if strings.Contains(note, "this job's own reserve") {
			t.Fatalf("note=%q, a figure already on the line must not be repeated", note)
		}
		if !strings.Contains(note, "52G already granted across 9 admitted jobs / 64G slice ceiling") {
			t.Fatalf("note=%q, the AIRA-181 clause still has to carry the comparison", note)
		}
	})
	t.Run("unreported-reserve-prints-nothing", func(t *testing.T) {
		// An older daemon, or a waiter it did not match. The client's own figure
		// is NOT substituted: while unpinned it is a hint, and naming it as the
		// resolved reserve would be the fabrication this ticket is about.
		note := queueNoteFor(t, 2<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 40 << 30, Jobs: 3, CeilingBytes: 61 << 30,
		})
		if strings.Contains(note, "this job's own reserve") {
			t.Fatalf("note=%q, an unreported reserve must print nothing", note)
		}
	})
}

// AIRA-186's discriminating case: a reserve larger than the slice's whole
// ceiling is not waiting for contention to clear, and no amount of patience is
// the remedy. Reachable while queued even though admission refuses `reserve >
// ceiling` outright at enqueue, because the ceiling falls afterwards — under
// outside memory pressure, or as more jobs are admitted and each takes its own
// headroom.
//
// verifies: the oversize case is stated in full with both numbers, is stated
// even when it duplicates the pinned figure already on the line, and is NOT
// claimed when the reserve merely needs a quiet slice or when the ceiling could
// not be established.
func TestConfineQueueNoteDistinguishesAnUngrantableReserveFromContention(t *testing.T) {
	t.Run("larger-than-the-whole-ceiling", func(t *testing.T) {
		note := queueNoteFor(t, 2<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 8 << 30, Jobs: 2, CeilingBytes: 61 << 30,
			ResolvedReserveBytes: 70 << 30,
		})
		if !strings.Contains(note, "this job's own reserve resolves to 70G — larger than the whole 61G slice ceiling") {
			t.Fatalf("note=%q, want the oversize case named with both numbers", note)
		}
		if !strings.Contains(note, "blocked by its own size and not by the jobs ahead of it") {
			t.Fatalf("note=%q, want the distinction from ordinary contention stated", note)
		}
		if !strings.Contains(note, "pin a smaller --memory-reserve") {
			t.Fatalf("note=%q, want the one remedy that applies", note)
		}
	})
	t.Run("pinned-and-oversize-still-speaks", func(t *testing.T) {
		// The figure duplicates the head of the line, but a job that cannot be
		// granted at all is decisive news, not a restatement.
		note := queueNoteFor(t, 70<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 8 << 30, Jobs: 2, CeilingBytes: 61 << 30,
			ResolvedReserveBytes: 70 << 30,
		})
		if !strings.Contains(note, "this job's own reserve is 70G — larger than the whole 61G slice ceiling") {
			t.Fatalf("note=%q, an ungrantable pinned reserve must still be called out", note)
		}
		if strings.Contains(note, "resolves to") {
			t.Fatalf("note=%q, a pinned reserve is honoured verbatim and did not 'resolve' anywhere", note)
		}
	})
	t.Run("large-but-grantable-is-not-called-ungrantable", func(t *testing.T) {
		// The false-pass direction, and the reported case itself: 35G of a 61G
		// ceiling needs a nearly-empty slice but CAN be granted. Claiming
		// otherwise would send a caller to pin down a reserve their own history
		// says they need — which is precisely how the reporter's 24G pin came to
		// OOM at a measured 31.97 GiB peak.
		note := queueNoteFor(t, 2<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 52 << 30, Jobs: 3, CeilingBytes: 61 << 30,
			ResolvedReserveBytes: 35 << 30,
		})
		if strings.Contains(note, "larger than the whole") || strings.Contains(note, "blocked by its own size") {
			t.Fatalf("note=%q, a reserve that fits the ceiling must not be called ungrantable", note)
		}
		if !strings.Contains(note, "this job's own reserve resolves to 35G") {
			t.Fatalf("note=%q, it must still be stated so the caller can weigh it", note)
		}
	})
	t.Run("exactly-the-ceiling-is-not-oversize", func(t *testing.T) {
		// The boundary. `reserve == ceiling` is the largest ADMISSIBLE reserve,
		// so the strictly-greater comparison is the honest one.
		note := queueNoteFor(t, 2<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 0, Jobs: 0, CeilingBytes: 61 << 30,
			ResolvedReserveBytes: 61 << 30,
		})
		if strings.Contains(note, "larger than the whole") {
			t.Fatalf("note=%q, a reserve equal to the ceiling is admissible", note)
		}
	})
	t.Run("no-ceiling-no-verdict", func(t *testing.T) {
		// Without a ceiling there is nothing to compare against, and "cannot fit"
		// would be an assertion the data does not support.
		note := queueNoteFor(t, 2<<30, &ConfineSliceReserve{
			Queued: 1, QueuePosition: 1, GrantedBytes: 8 << 30, Jobs: 2, CeilingBytes: 0,
			ResolvedReserveBytes: 70 << 30,
		})
		if strings.Contains(note, "larger than the whole") {
			t.Fatalf("note=%q, an unestablished ceiling supports no verdict", note)
		}
		if !strings.Contains(note, "this job's own reserve resolves to 70G") {
			t.Fatalf("note=%q, the reserve itself is still a fact worth stating", note)
		}
	})
}

// The new clauses ride the SAME probe as the position, so an absent position
// must still print an absolutely unchanged line. A held figure or a reserve
// attached to a wait the daemon knows nothing about would put numbers on a
// daemon-less launch's line that nothing established.
//
// verifies: no position means no held clause and no reserve clause either.
func TestConfineQueueNoteAddsNothingWithoutAPosition(t *testing.T) {
	for _, test := range []struct {
		name    string
		reserve *ConfineSliceReserve
	}{
		{name: "scope-not-queued", reserve: &ConfineSliceReserve{
			Queued: 4, GrantedBytes: 52 << 30, Jobs: 9, CeilingBytes: 64 << 30, ResolvedReserveBytes: 35 << 30,
		}},
		{name: "self-contradictory-pair", reserve: &ConfineSliceReserve{
			Queued: 1, QueuePosition: 3, GrantedBytes: 52 << 30, Jobs: 9, CeilingBytes: 64 << 30, ResolvedReserveBytes: 35 << 30,
		}},
		{name: "no-slice-reserve", reserve: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if note := queueNoteFor(t, 2<<30, test.reserve); note != "" {
				t.Fatalf("note=%q, want an empty clause for an unestablished position", note)
			}
		})
	}
}
