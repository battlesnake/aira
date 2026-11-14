package daemon

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
	"aira/internal/testdeadline"
)

// AIRA-328. `aira confine --kill` on a job that is still QUEUED cancels its
// admission waiter: state -> admitRejected with admitOutcomeCancelled, grantedCh
// closed, the queue kicked. The waiter's own admitConnection then answers its
// client U_ADMIT_CANCELLED and its deferred, state-guarded release removes it.
// No cgroup is touched; the ledger is never charged for a queued waiter, so it
// is never discharged either.

func requireCancelled(t *testing.T, waiter *admitWaiter) {
	t.Helper()
	if waiter.state != admitRejected || waiter.outcome != admitOutcomeCancelled {
		t.Fatalf("waiter %s state=%v outcome=%q, want admitRejected/%s", waiter.scopeID, waiter.state, waiter.outcome, admitOutcomeCancelled)
	}
	select {
	case <-waiter.grantedCh:
	default:
		t.Fatalf("waiter %s grantedCh still open: its admitConnection would wait forever", waiter.scopeID)
	}
}

func requireUntouched(t *testing.T, waiter *admitWaiter, want admitWaiterState) {
	t.Helper()
	if waiter.state != want || waiter.outcome == admitOutcomeCancelled {
		t.Fatalf("waiter %s state=%v outcome=%q, want untouched state %v", waiter.scopeID, waiter.state, waiter.outcome, want)
	}
	if want == admitQueued {
		requireAdmitQueued(t, waiter)
	}
}

// verifies: AIRA-328 — resolution, the shared ownership guard, ambiguity and
// fall-through. Each row builds a FRESH queue, because a cancel mutates it.
// Mutations this reds against: drop the owner guard (foreign rows answer OK);
// drop the granted-match count (queued+running twin is cancelled instead of
// ambiguous); let a name or pid reach
// a worker (worker rows handled); cancel a sub-reservation by exact scope id
// (its row answers OK); report handled for a granted-only match (the
// running job's kill would be swallowed).
func TestConfineKillQueuedResolutionAndOwnerGuard(t *testing.T) {
	type fixture struct {
		queue                                       *sliceQueue
		mine, inferred, theirs, twinA, twinB        *admitWaiter
		running, runningTwin, queuedTwin, worker    *admitWaiter
		looseWorker, unowned, alreadyCancelled, sub *admitWaiter
	}
	build := func() fixture {
		now := time.Unix(61000, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		f := fixture{
			mine: frontWaiter(1, "mine", "s-1"), inferred: frontWaiter(2, "inferred", "@home-x"),
			theirs: frontWaiter(3, "theirs", "s-2"), twinA: frontWaiter(4, "twin", "s-1"), twinB: frontWaiter(5, "twin", "s-1"),
			running: frontWaiter(6, "running", "s-1"), runningTwin: frontWaiter(7, "mixed", "s-1"), queuedTwin: frontWaiter(8, "mixed", "s-1"),
			worker: frontWaiter(9, "aitest-w1", "s-1"), looseWorker: frontWaiter(10, "aitest-w2", "s-1"),
			unowned: frontWaiter(11, "unowned", ""), alreadyCancelled: frontWaiter(12, "gone", "s-1"),
		}
		f.running.state, f.running.accounted = admitGranted, true
		f.runningTwin.state, f.runningTwin.accounted = admitGranted, true
		f.worker.parentScopeID = f.running.scopeID
		f.alreadyCancelled.state, f.alreadyCancelled.outcome = admitRejected, admitOutcomeCancelled
		f.sub = f.worker
		f.queue = queueOf(server, f.mine, f.inferred, f.theirs, f.twinA, f.twinB, f.running, f.runningTwin, f.queuedTwin, f.worker, f.looseWorker, f.unowned, f.alreadyCancelled)
		return f
	}
	code := func(err error) string {
		if err == nil {
			return "OK"
		}
		return store.ErrorCode(err)
	}
	for _, test := range []struct {
		name     string
		selector func(fixture) string
		caller   string
		steal    bool
		handled  bool
		want     string
		target   func(fixture) *admitWaiter
	}{
		{name: "own job by name", selector: func(fixture) string { return "mine" }, caller: "s-1", handled: true, want: "OK", target: func(f fixture) *admitWaiter { return f.mine }},
		{name: "own job by supervisor pid", selector: func(fixture) string { return "6001" }, caller: "s-1", handled: true, want: "OK", target: func(f fixture) *admitWaiter { return f.mine }},
		{name: "own job by scope id", selector: func(f fixture) string { return f.mine.scopeID }, caller: "s-1", handled: true, want: "OK", target: func(f fixture) *admitWaiter { return f.mine }},
		{name: "foreign owner refused", selector: func(fixture) string { return "theirs" }, caller: "s-1", handled: true, want: runner.CodeConfineOwnerUnverified, target: func(f fixture) *admitWaiter { return f.theirs }},
		{name: "foreign owner with steal", selector: func(fixture) string { return "theirs" }, caller: "s-1", steal: true, handled: true, want: "OK", target: func(f fixture) *admitWaiter { return f.theirs }},
		{name: "inferred owner refused", selector: func(fixture) string { return "inferred" }, caller: "@home-x", handled: true, want: runner.CodeConfineOwnerUnverified, target: func(f fixture) *admitWaiter { return f.inferred }},
		{name: "unknown owner refused", selector: func(fixture) string { return "unowned" }, caller: "s-1", handled: true, want: runner.CodeConfineOwnerUnverified, target: func(f fixture) *admitWaiter { return f.unowned }},
		{name: "two queued twins are ambiguous", selector: func(fixture) string { return "twin" }, caller: "s-1", handled: true, want: "E_SELECTOR_AMBIGUOUS", target: func(f fixture) *admitWaiter { return f.twinA }},
		{name: "a queued and a running twin are ambiguous", selector: func(fixture) string { return "mixed" }, caller: "s-1", handled: true, want: "E_SELECTOR_AMBIGUOUS", target: func(f fixture) *admitWaiter { return f.queuedTwin }},
		{name: "running only falls through", selector: func(fixture) string { return "running" }, caller: "s-1"},
		{name: "nothing matches falls through", selector: func(fixture) string { return "nobody" }, caller: "s-1"},
		{name: "empty selector falls through", selector: func(fixture) string { return "" }, caller: "s-1"},
		{name: "already cancelled falls through", selector: func(fixture) string { return "gone" }, caller: "s-1"},
		{name: "worker by name is not a job-level selector", selector: func(fixture) string { return "aitest-w1" }, caller: "s-1"},
		{name: "loose worker by pid is not a job-level selector", selector: func(fixture) string { return "6010" }, caller: "s-1"},
		{name: "sub-reservation by exact scope id is refused", selector: func(f fixture) string { return f.sub.scopeID }, caller: "s-1", steal: true, handled: true, want: runner.CodeConfineKillWorkerClaim, target: func(f fixture) *admitWaiter { return f.sub }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := build()
			before := map[*admitWaiter]admitWaiterState{}
			for _, waiter := range f.queue.waiters {
				before[waiter] = waiter.state
			}
			result, handled, err := cancelQueuedConfineLocked(f.queue, test.selector(f), test.caller, test.steal, time.Unix(61010, 0))
			if handled != test.handled {
				t.Fatalf("handled=%v (err=%v), want %v", handled, err, test.handled)
			}
			if !handled {
				if err != nil {
					t.Fatalf("a fall-through must carry no error, got %v", err)
				}
			} else if got := code(err); got != test.want {
				t.Fatalf("code=%s (err=%v), want %s", got, err, test.want)
			}
			for _, waiter := range f.queue.waiters {
				if test.want == "OK" && test.target != nil && waiter == test.target(f) {
					requireCancelled(t, waiter)
					if result.Status != runner.ConfineKillStatusCancelled || result.Phase != runner.ConfineKillPhaseQueued || result.ScopeID != waiter.scopeID {
						t.Fatalf("result=%+v, want status=%s phase=%s scope=%s", result, runner.ConfineKillStatusCancelled, runner.ConfineKillPhaseQueued, waiter.scopeID)
					}
					continue
				}
				if waiter.state != before[waiter] {
					t.Fatalf("waiter %s moved %v -> %v; only the one resolved target may change", waiter.scopeID, before[waiter], waiter.state)
				}
				if waiter.state == admitQueued {
					requireAdmitQueued(t, waiter)
				}
			}
		})
	}
	f := build()
	_, _, err := cancelQueuedConfineLocked(f.queue, "mixed", "s-1", false, time.Unix(61010, 0))
	if err == nil || !strings.Contains(err.Error(), f.queuedTwin.scopeID) || !strings.Contains(err.Error(), f.runningTwin.scopeID) {
		t.Fatalf("ambiguity refusal must name both the queued and the running job: %v", err)
	}
}

// verifies: AIRA-328 — cancelling a queued waiter is LEDGER-NEUTRAL, and the
// later socket-close release is state-guarded so it discharges exactly once.
// Mutations: charge or discharge on cancel (outstanding moves); drop the
// `state == admitReleased` guard in releaseAdmitWaiterLocked (second release
// reports true).
func TestConfineKillQueuedIsLedgerNeutralAndReleasesOnce(t *testing.T) {
	now := time.Unix(62000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	held := heldLease(1, 40*qGiB)
	held.cpu, held.vram = 2, 1<<20
	target := frontWaiter(2, "target", "s-1")
	target.cpu = 3
	other := frontWaiter(3, "other", "s-1")
	queue := queueOf(server, held, target, other)
	outstanding, cpu, vram, jobs := queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs
	ledgerUnchanged := func(stage string) {
		t.Helper()
		if queue.outstanding != outstanding || queue.cpuOutstanding != cpu || queue.vramOutstanding != vram || queue.outstandingJobs != jobs {
			t.Fatalf("%s: ledger moved: %d/%d/%d/%d, want %d/%d/%d/%d", stage,
				queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs, outstanding, cpu, vram, jobs)
		}
		o, c, v, j := rederiveLedgerLocked(queue)
		if o != outstanding || c != cpu || v != vram || j != jobs {
			t.Fatalf("%s: the derived ledger disagrees with the stored one: %d/%d/%d/%d", stage, o, c, v, j)
		}
	}
	if _, handled, err := cancelQueuedConfineLocked(queue, "target", "s-1", false, now); !handled || err != nil {
		t.Fatalf("cancel: handled=%v err=%v", handled, err)
	}
	requireCancelled(t, target)
	ledgerUnchanged("after cancel")
	if target.accounted {
		t.Fatal("a cancelled waiter must never be marked accounted")
	}
	// The cancelled job's own admitConnection close: the first release removes
	// it, a second (a re-entrant close, or the kill-drop path) is a no-op.
	if !releaseAdmitWaiterLocked(queue, target) {
		t.Fatal("the cancelled waiter's first release did nothing; it would linger in queue.waiters")
	}
	ledgerUnchanged("after release")
	if releaseAdmitWaiterLocked(queue, target) {
		t.Fatal("a second release of the same waiter reported a discharge (double release)")
	}
	ledgerUnchanged("after double release")
	for _, waiter := range queue.waiters {
		if waiter == target {
			t.Fatal("the released waiter is still in queue.waiters")
		}
	}
	requireUntouched(t, other, admitQueued)
}

// verifies: AIRA-328 — cancelling the protected HEAD leaves no ghost. The head
// and its bypass budget are derived per pass from QUEUED waiters, and the
// queue-level freeze anchor is deliberately not written by the cancel (it has
// exactly two writers). So with the hold still armed, the next pass grants the
// waiter the dead head's spent budget was withholding. Mutation: leave the head
// admitQueued on cancel (only close its channel) -> the fourth jumper stays held.
func TestConfineKillQueuedHeadLeavesNoGhost(t *testing.T) {
	now := time.Unix(63000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	held := heldLease(1, 44*qGiB)
	head := frontWaiter(2, "head", "s-1")
	head.reserve = 24 * qGiB
	var jumpers []*admitWaiter
	for i := int64(0); i < 3; i++ {
		jumper := queuedWaiter(3+i, 4*qGiB, now)
		jumpers = append(jumpers, jumper)
	}
	fourth := queuedWaiter(6, 4*qGiB, now)
	queue := queueOf(server, append(append([]*admitWaiter{held, head}, jumpers...), fourth)...)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, fourth)
	if head.bypassed != 12*qGiB || queue.freezeArmedAt.IsZero() {
		t.Fatalf("precondition: head budget spent and hold armed (bypassed=%d armed=%v)", head.bypassed, queue.freezeArmedAt)
	}
	armed := queue.freezeArmedAt

	if _, handled, err := cancelQueuedConfineLocked(queue, "head", "s-1", false, now); !handled || err != nil {
		t.Fatalf("cancel head: handled=%v err=%v", handled, err)
	}
	if !queue.freezeArmedAt.Equal(armed) {
		t.Fatal("the cancel wrote the freeze anchor; it has exactly two writers (arm and completed-cycle clear)")
	}
	server.evaluateAdmitQueue(queue)
	select {
	case <-fourth.grantedCh:
	default:
		t.Fatal("the waiter behind the cancelled head is still held: the dead head's spent budget froze it")
	}
	if queue.freezeHolderSeq == head.seq {
		t.Fatal("the cancelled head is still recorded as the protected head")
	}
}

// verifies: AIRA-328 — cancelling a PUSHED head (--front) works and releases
// everything it held. Mutation: skip pushed waiters in the resolver -> the
// push keeps holding the jumper.
func TestConfineKillQueuedPushedHead(t *testing.T) {
	now := time.Unix(64000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	held := heldLease(1, 44*qGiB)
	pushed := frontWaiter(2, "pushed", "s-1")
	pushed.reserve = 24 * qGiB
	jumper := queuedWaiter(3, 1*qGiB, now)
	queue := queueOf(server, held, pushed, jumper)
	mustPush(t, queue, "pushed")
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, jumper)

	if _, handled, err := cancelQueuedConfineLocked(queue, "pushed", "s-1", false, now); !handled || err != nil {
		t.Fatalf("cancel pushed head: handled=%v err=%v", handled, err)
	}
	requireCancelled(t, pushed)
	server.evaluateAdmitQueue(queue)
	select {
	case <-jumper.grantedCh:
	default:
		t.Fatal("the jumper is still held by the cancelled pushed head")
	}
}

// verifies: AIRA-328 — cancelling a DRAINING exclusive waiter (a queued
// --exclusive job) clears the exclusive gate at once, so the ordinary job it
// was blocking is granted on the next pass. Mutation: cancel by any state other
// than one exclusiveActive() excludes -> the drain keeps blocking.
func TestConfineKillQueuedExclusiveDrainerClearsTheGate(t *testing.T) {
	now := time.Unix(65000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	held := heldLease(1, 1*qGiB)
	drainer := frontWaiter(2, "bench", "s-1")
	drainer.exclusive = true
	ordinary := queuedWaiter(3, 1*qGiB, now)
	queue := queueOf(server, held, drainer, ordinary)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, ordinary)
	if gate := exclusiveGateLocked(queue); gate.draining != drainer {
		t.Fatal("precondition: the drainer holds the exclusive gate")
	}

	if _, handled, err := cancelQueuedConfineLocked(queue, "bench", "s-1", false, now); !handled || err != nil {
		t.Fatalf("cancel drainer: handled=%v err=%v", handled, err)
	}
	if gate := exclusiveGateLocked(queue); gate.draining != nil || gate.holder != nil {
		t.Fatal("a cancelled drainer still holds the exclusive gate")
	}
	server.evaluateAdmitQueue(queue)
	select {
	case <-ordinary.grantedCh:
	default:
		t.Fatal("the ordinary job is still blocked by the cancelled drain")
	}
}

// verifies: AIRA-328 — through the real management handler, in real and ci-shim
// mode: the reply is cancelled/queued, the audit line says phase=queued and is
// written only on success, the queue is kicked, `--list`'s queued half no longer
// shows the job, and a second kill answers not-found (idempotent-honest: it never
// re-reports a cancel). Mutation: drop the queued branch from the handler -> the
// first kill answers E_CONFINE_NOT_FOUND (the bug).
func TestConfineKillQueuedThroughTheHandler(t *testing.T) {
	for _, shim := range []bool{false, true} {
		t.Run(map[bool]string{false: "real", true: "ci-shim"}[shim], func(t *testing.T) {
			now := time.Unix(66000, 0)
			server := queueOrderServer(t, 64*qGiB, &now)
			path := t.TempDir()
			server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
			if shim {
				server.confineMode = runner.ConfineModeShim
				path = runner.ShimConfineSlice
			}
			first, target := frontWaiter(1, "first", "s-1"), frontWaiter(2, "target", "s-1")
			queue := queueOf(server, first, target)
			queue.path = path
			server.admitQueues[path] = queue
			kill := func(caller string) core.Request {
				return core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "aira.slice", "selector": "target", "owner": caller}}
			}

			var refused core.Response
			logged := captureDaemonLog(t, func() { refused = server.confineManagement(t.Context(), kill("s-2")) })
			if refused.OK || refused.Code != runner.CodeConfineOwnerUnverified {
				t.Fatalf("foreign kill response=%+v, want %s", refused, runner.CodeConfineOwnerUnverified)
			}
			if strings.Contains(logged, "confine-kill") {
				t.Fatalf("a refused cancel left an audit line:\n%s", logged)
			}
			select {
			case <-queue.kick:
				t.Fatal("a refused cancel kicked the evaluator")
			default:
			}
			requireAdmitQueued(t, target)

			var accepted core.Response
			logged = captureDaemonLog(t, func() { accepted = server.confineManagement(t.Context(), kill("s-1")) })
			result, ok := accepted.Data.(runner.ConfineKillResult)
			if !accepted.OK || !ok || result.ScopeID != target.scopeID || result.Status != runner.ConfineKillStatusCancelled || result.Phase != runner.ConfineKillPhaseQueued {
				t.Fatalf("kill response=%+v, want the target cancelled in phase queued", accepted)
			}
			if !strings.Contains(logged, "confine-kill: phase=queued killer=s-1 steal=false target-scope="+target.scopeID) {
				t.Fatalf("missing audit line:\n%s", logged)
			}
			select {
			case <-queue.kick:
			default:
				t.Fatal("a cancel did not kick the evaluator")
			}
			requireCancelled(t, target)
			requireUntouched(t, first, admitQueued)
			snapshot := server.admitSliceSnapshotFor(path, "")
			for _, entry := range snapshot.queuedEntries {
				if entry.ScopeID == target.scopeID {
					t.Fatal("--list still shows the cancelled job as queued")
				}
			}
			if snapshot.queued != 1 {
				t.Fatalf("snapshot queued=%d, want 1 (only the untouched job)", snapshot.queued)
			}

			second := server.confineManagement(t.Context(), kill("s-1"))
			if second.OK || second.Code != runner.CodeConfineNotFound {
				t.Fatalf("second kill response=%+v, want %s (the cancel already happened; never report it twice)", second, runner.CodeConfineNotFound)
			}
		})
	}
}

// verifies: AIRA-328 review P3 — the handler's race window. The registry is
// snapshotted before the queued cancel, so a job granted between the snapshot
// and the cancel falls through to a scope kill that can find no scope yet (its
// launcher has not created it). Through the real handler: a GRANTED waiter with
// no scope on disk answers U_CONFINE_NOT_LAUNCHED ("retry"), never
// E_CONFINE_NOT_FOUND, and kills nothing; a selector naming no job still answers
// not-found. Mutation: drop the granted-match check after the scope kill -> the
// granted row answers E_CONFINE_NOT_FOUND.
func TestConfineKillJustGrantedJobAnswersNotLaunched(t *testing.T) {
	now := time.Unix(66500, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	path := t.TempDir()
	server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
	granted := frontWaiter(1, "fresh", "s-1")
	granted.state, granted.accounted = admitGranted, true
	queue := queueOf(server, granted)
	queue.path = path
	server.admitQueues[path] = queue
	kill := func(selector string) core.Request {
		return core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "aira.slice", "selector": selector, "owner": "s-1"}}
	}
	for _, selector := range []string{"fresh", granted.scopeID} {
		var response core.Response
		logged := captureDaemonLog(t, func() { response = server.confineManagement(t.Context(), kill(selector)) })
		if response.OK || response.Code != runner.CodeConfineNotLaunched || !strings.Contains(response.Error, "retry") || !strings.Contains(response.Error, granted.scopeID) {
			t.Fatalf("kill %q of a just-granted job: response=%+v, want %s naming %s with a retry", selector, response, runner.CodeConfineNotLaunched, granted.scopeID)
		}
		if strings.Contains(logged, "confine-kill") {
			t.Fatalf("a not-launched answer left an audit line:\n%s", logged)
		}
		requireUntouched(t, granted, admitGranted)
	}
	if response := server.confineManagement(t.Context(), kill("nobody")); response.OK || response.Code != runner.CodeConfineNotFound {
		t.Fatalf("kill of no job: response=%+v, want %s", response, runner.CodeConfineNotFound)
	}
}

// verifies: AIRA-328 — the wire: a real admitConnection whose waiter is
// cancelled answers its client U_ADMIT_CANCELLED with basis reject:cancelled
// (not saturated, not fail-fast), then returns and its deferred release removes
// the waiter. The exclusive row is a real `--exclusive` / `aira drain wait`
// requester still DRAINING behind a held lease: its cancel must not be reported
// as the drain-timeout E_ADMIT_SATURATED (exclusive=draining), which the skill
// tells agents to retry. Mutations: release the waiter in the kill instead of
// rejecting it -> no frame (the client would see EOF and reconnect); drop the
// render arm -> E_ADMIT_SATURATED; move the cancelled arm below the exclusive
// arm (or skip it for exclusive waiters) -> the exclusive row gets
// E_ADMIT_SATURATED.
func TestConfineKillQueuedWireRender(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "exclusive-drainer"}[exclusive], func(t *testing.T) {
			const maximum = int64(1) << 30
			server := saturatedServer(t)
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, maximum, 0, true, "" }
			scopeID := testConfineScopeID(t, "wired", 7301) + "@s-1"
			args := map[string]any{"slice": "slice", "reserve": int64(1) << 20, "pinned": true, "scope_id": scopeID, "name": "wired", "owner": "s-1"}
			var seed []*admitWaiter
			if exclusive {
				args["exclusive"] = true
				// A live lease keeps the slice non-empty, so the requester drains
				// rather than being granted on its first pass.
				seed = append(seed, heldLease(1, 1<<20))
			}
			run := startSaturatedAdmit(t, server, maximum, args, seed...)
			if exclusive {
				run.queue.mu.Lock()
				draining := exclusiveGateLocked(run.queue).draining
				run.queue.mu.Unlock()
				if draining != run.waiter {
					t.Fatal("precondition: the exclusive requester is draining the slice")
				}
			}

			result, handled, err := server.cancelQueuedConfine("/slice", "wired", "s-1", false)
			if !handled || err != nil || result.ScopeID != scopeID {
				t.Fatalf("cancel: result=%+v handled=%v err=%v", result, handled, err)
			}
			select {
			case frame := <-run.frames:
				if frame.Code != CodeAdmitCancelled {
					t.Fatalf("frame code=%q, want %s (an operator cancel must not read as contention)", frame.Code, CodeAdmitCancelled)
				}
				if !strings.HasPrefix(frame.Error, CodeAdmitCancelled+":") {
					t.Fatalf("message %q must lead with the code so the client extracts it", frame.Error)
				}
				var rejection admitRejection
				if err := json.Unmarshal(frame.Data, &rejection); err != nil {
					t.Fatalf("rejection payload: %v", err)
				}
				if rejection.Basis != "reject:cancelled" || rejection.Exclusive != "" {
					t.Fatalf("basis=%q exclusive=%q, want reject:cancelled with no exclusive state", rejection.Basis, rejection.Exclusive)
				}
			case err := <-run.errs:
				t.Fatalf("read: %v (the client saw EOF instead of a refusal: it would reconnect and re-queue)", err)
			case <-testdeadline.After(5 * time.Second):
				t.Fatal("no frame after the cancel")
			}
			select {
			case <-run.done:
			case <-testdeadline.After(5 * time.Second):
				t.Fatal("admitConnection did not return after answering the cancel")
			}
			run.queue.mu.Lock()
			defer run.queue.mu.Unlock()
			for _, waiter := range run.queue.waiters {
				if waiter == run.waiter {
					t.Fatal("the cancelled waiter is still in queue.waiters after its connection ended")
				}
			}
			if run.waiter.state != admitReleased {
				t.Fatalf("waiter state=%v, want admitReleased", run.waiter.state)
			}
			if gate := exclusiveGateLocked(run.queue); gate.draining != nil || gate.holder != nil {
				t.Fatal("the cancelled requester still holds the exclusive gate")
			}
		})
	}
}

// verifies: AIRA-328 — grant XOR cancel. An evaluator pass that would GRANT
// the waiter meets the kill on queue.mu in both forced orders and, over many
// iterations, genuinely concurrently. The reply and the
// client frame always agree: a "cancelled" reply means the client got
// U_ADMIT_CANCELLED and the waiter was never granted; a fall-through means it
// was granted and the client got a grant. The client never hangs: every read,
// the racing pair's join and startSaturatedAdmit's cleanup are bounded, so a
// wrong resolver fails in seconds rather than at the binary timeout. Run with
// -race. Mutation: drop the `state == admitQueued` filter in
// the resolver -> a granted waiter is "cancelled" (a second close of grantedCh
// panics, or the reply and the frame disagree).
func TestConfineKillQueuedRacesTheGrant(t *testing.T) {
	const maximum = int64(1) << 30
	sawCancel, sawGrant := 0, 0
	for i := 0; i < 50; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			server := saturatedServer(t)
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, maximum, 0, true, "" }
			scopeID := testConfineScopeID(t, "racer", 7400+i) + "@s-1"
			args := map[string]any{"slice": "slice", "reserve": int64(1) << 20, "pinned": true, "scope_id": scopeID, "name": "racer", "owner": "s-1"}
			run := startSaturatedAdmit(t, server, maximum, args)

			// A third of the iterations FORCE the cancel first and a third force
			// the grant first, so both outcomes are exercised deterministically
			// (a race that only ever resolves one way proves nothing about the
			// other, and a scheduler-dependent "both seen" assertion would
			// flake). The last third genuinely race on one barrier, which is
			// what -race checks; those assert only the XOR below.
			var result runner.ConfineKillResult
			var handled bool
			var err error
			cancel := func() {
				result, handled, err = server.cancelQueuedConfine("/slice", "racer", "s-1", false)
			}
			order := i % 3
			switch order {
			case 0:
				cancel()
				run.pass()
			case 1:
				run.pass()
				cancel()
			default:
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					if i%2 == 0 {
						runtime.Gosched()
					}
					run.pass()
				}()
				go func() {
					defer wg.Done()
					<-start
					if i%2 == 1 {
						runtime.Gosched()
					}
					cancel()
				}()
				close(start)
				joined := make(chan struct{})
				go func() { wg.Wait(); close(joined) }()
				select {
				case <-joined:
				case <-testdeadline.After(5 * time.Second):
					t.Fatal("the racing grant and cancel never both returned (a lock left held?)")
				}
			}
			if order == 0 && !handled {
				t.Fatal("a cancel that ran first was not handled: the queued job would run")
			}
			if order == 1 && handled {
				t.Fatal("a kill after the grant reported a cancel: the job is running and was never cancelled")
			}
			if err != nil {
				t.Fatalf("cancel error: %v", err)
			}
			var frame ResponseFrame
			select {
			case frame = <-run.frames:
			case err := <-run.errs:
				t.Fatalf("client read failed (no answer): %v", err)
			case <-testdeadline.After(5 * time.Second):
				t.Fatal("the client hung: neither a grant nor a cancel arrived")
			}
			run.queue.mu.Lock()
			state := run.waiter.state
			run.queue.mu.Unlock()
			if handled {
				sawCancel++
				if result.Status != runner.ConfineKillStatusCancelled || frame.Code != CodeAdmitCancelled {
					t.Fatalf("kill said cancelled (%+v) but the client got %q", result, frame.Code)
				}
				if state == admitGranted {
					t.Fatal("a cancelled waiter was also granted")
				}
			} else {
				sawGrant++
				if !frame.OK || frame.Code != "OK" || state != admitGranted {
					t.Fatalf("kill fell through but the waiter is %v and the client got %+v", state, frame)
				}
			}
		})
	}
	t.Logf("cancelled=%d granted=%d", sawCancel, sawGrant)
	if sawCancel == 0 || sawGrant == 0 {
		t.Fatalf("the race resolved only one way (cancelled=%d granted=%d); both outcomes must be exercised", sawCancel, sawGrant)
	}
}
