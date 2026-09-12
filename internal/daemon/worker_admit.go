package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// workerAdmitEstimatedBytesMin matches --memory-reserve's minimum
// (cmd/aira/main.go), so a sub-page estimate can never floor memory.max to
// zero pages and instant-OOM the worker on placement.
const workerAdmitEstimatedBytesMin int64 = 1 << 20 // 1 MiB

// workerAdmitBasis is the diagnostic label a worker lease carries in the ledger.
// It participates in no admission decision (validRunnerAdmitGrant only requires a
// non-empty basis were the lease ever framed as an AdmitResponse by a later
// re-anchor); it exists so a `confine --list` walk can tell a worker lease apart
// from an ordinary confine one.
const workerAdmitBasis = "worker"

// WorkerAdmitResponse is the one grant/denial/snapshot payload the worker-admit
// connection sends before optionally holding itself open as the lease.
//
// AIRA-42: State, Class and Reason are drawn from the single vocabulary in
// internal/runner (runner.WorkerAdmitState*/Class*/Reason*), never spelled as
// literals here. Class is the load-bearing field — the disposition the supervisor
// acts on — and Reason is a stable exact-match token; Detail is free text that
// NOTHING parses.
type WorkerAdmitResponse struct {
	State string `json:"state"`
	Class string `json:"class"`
	// Reason is an exact-match token from the runner vocabulary. It is
	// diagnostic: no consumer branches on it, and none may.
	Reason string `json:"reason,omitempty"`
	// Detail elaborates for a human. It carries cgroup paths and raw file
	// contents, i.e. operator-controlled text, which is exactly why nothing
	// classifies from it.
	Detail    string `json:"detail,omitempty"`
	WaitedMS  int64  `json:"waited_ms"`
	WorkerID  string `json:"worker_id,omitempty"`
	ScopePath string `json:"scope_path,omitempty"`
	MemoryMax int64  `json:"memory_max,omitempty"`
	// Containment (AIRA-123) is runner.WorkerAdmitContainmentEnforced or
	// ...Advisory, and is REQUIRED on every granted response. It is what stops an
	// admission-only grant ever being readable as a kernel-enforced one.
	Containment string `json:"containment,omitempty"`
	// Reserved (AIRA-123) is the ADVISORY grant's booked reservation in bytes.
	// Positive exactly on an advisory (shim) grant; on an enforced one memory_max
	// is both the booking and the bound.
	Reserved int64 `json:"reserved,omitempty"`
	// SwapCap (AIRA-35) reports whether this worker's swap could actually be
	// bounded (runner.WorkerAdmitSwapCap*). Empty on an advisory grant and on a
	// non-grant. Diagnostic: nothing branches on it, and it is what stops a lost
	// swap-containment guarantee from being invisible to the run it affects.
	SwapCap string `json:"swap_cap,omitempty"`
	// ParentScopeID is the suite scope-id this worker lease is a sub-reservation of
	// (design §8). It is echoed on a granted response so the relay can re-declare
	// the lease VERBATIM across a daemon restart (internal/redeclare's ARDR frame),
	// re-anchoring it in the new daemon's ledger with the same parent — which is
	// what preserves the exclusivity exemption for a `--exclusive` suite's own
	// workers after a restart.
	ParentScopeID string `json:"parent_scope_id,omitempty"`
	// AvailableBytes / AvailableCPU carry the non-blocking probe's current headroom
	// (design §6/§8): what the unified ledger would admit RIGHT NOW, with no
	// reservation taken. Meaningful only on a snapshot (max_wait_ms present and
	// zero); zero/omitted on every grant and every blocking-claim denial.
	AvailableBytes int64 `json:"available_bytes,omitempty"`
	AvailableCPU   int64 `json:"available_cpu,omitempty"`
}

type workerAdmitRequest struct {
	jobID      string
	outerScope string
	// signature is accepted on the wire (spec 3.3's per-suite peak-history key) but
	// carried only as the lease's diagnostic signature; it governs no admission
	// decision in this slice.
	signature      string
	estimatedBytes int64
	// parentScopeID is the suite confine scope id this worker is a sub-reservation
	// OF (design §16d). It is an EXPLICIT required wire field — the supervisor's own
	// AIRA_CONFINE_SCOPE_ID, NOT derived from the outer-scope PATH — so a worker can
	// never silently become a job: an empty one is refused, and a non-empty one must
	// be parseConfineScopeID-parseable (the ci-shim sentinel exempt) so the daemon
	// can copy the PARENT supervisor pid out of it for the worker scope name (Task 1).
	parentScopeID string
	// nonBlocking is true when max_wait_ms is PRESENT on the wire AND equals 0 (the
	// aitest pool-sizing probe, design §6/§8): report current available and reserve
	// nothing. max_wait_ms ABSENT, or PRESENT and positive, is a BLOCKING claim —
	// the request waits until the ledger fits, ended only by a grant, daemon stop,
	// or the client closing its connection (design §4/§6). A positive value no
	// longer imposes a timeout (mirrors the S13 confine admit path).
	nonBlocking bool
}

// validateWorkerAdmitArgs parses the worker-admit wire arguments. Since S15 there
// is NO max_wait ceiling to validate: a blocking claim has no daemon-side timeout
// (design §4/§6), and a present-and-zero max_wait_ms is the non-blocking probe.
func validateWorkerAdmitArgs(args map[string]any) (workerAdmitRequest, error) {
	req := workerAdmitRequest{}
	str := func(key string, required bool) (string, error) {
		raw, exists := args[key]
		if !exists {
			if required {
				return "", fmt.Errorf("%s: worker-admit %s is required", CodeProtocol, key)
			}
			return "", nil
		}
		value, ok := raw.(string)
		if !ok || (required && value == "") {
			return "", fmt.Errorf("%s: worker-admit %s must be a non-empty string", CodeProtocol, key)
		}
		return value, nil
	}
	var err error
	if req.jobID, err = str("job_id", true); err != nil {
		return workerAdmitRequest{}, err
	}
	if req.outerScope, err = str("outer_scope", true); err != nil {
		return workerAdmitRequest{}, err
	}
	// Canonicalise BEFORE anything keys on it, so `/x`, `/x/` and `/x/.` cannot name
	// three different things while mutating one cgroup. A relative path is refused
	// rather than resolved against the daemon's own working directory. The ci-shim
	// SENTINEL is the one accepted non-path value; it is not a cgroup path and must
	// never be Cleaned into one (evaluateWorkerAdmit's mode-agreement check owns
	// whether it is the RIGHT value for this daemon).
	if req.outerScope != runner.ShimConfineSlice {
		if !filepath.IsAbs(req.outerScope) {
			return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit outer_scope must be an absolute cgroup path or the ci-shim sentinel %q, got %q", CodeProtocol, runner.ShimConfineSlice, req.outerScope)
		}
		req.outerScope = filepath.Clean(req.outerScope)
	}
	if req.signature, err = str("signature", false); err != nil {
		return workerAdmitRequest{}, err
	}
	// parent_scope_id is REQUIRED (design §16d): a worker must always declare the
	// suite it is a sub-reservation of, so it can never silently become a job. A
	// non-empty value must be parseConfineScopeID-parseable (mirror admit.go's
	// exclusive_holder / parent_scope_id checks) so Task 1 can extract the parent
	// supervisor pid — the ci-shim sentinel is the one exempt value, and it is tied
	// to shim mode on BOTH fields so a sentinel parent can never ride a real outer
	// scope (nor the reverse).
	if req.parentScopeID, err = str("parent_scope_id", true); err != nil {
		return workerAdmitRequest{}, err
	}
	parentIsSentinel := req.parentScopeID == runner.ShimConfineSlice
	outerIsSentinel := req.outerScope == runner.ShimConfineSlice
	if parentIsSentinel != outerIsSentinel {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit parent_scope_id and outer_scope disagree about ci-shim mode (parent %q, outer %q)", CodeProtocol, req.parentScopeID, req.outerScope)
	}
	if !parentIsSentinel {
		if _, _, _, _, parsed := runner.ParseConfineScopeID(req.parentScopeID); !parsed {
			return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit parent_scope_id %q is not a canonical confine scope id", CodeProtocol, req.parentScopeID)
		}
	}
	// exactAdmitInt64 (admit.go) — overflow-safe float64->int64, so an arbitrary
	// huge float64 cannot truncate unchecked into a plausible small reserve.
	estimated, ok := exactAdmitInt64(args["estimated_bytes"])
	if !ok || estimated < workerAdmitEstimatedBytesMin || estimated > admitMaxReserve {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit estimated_bytes must be at least %d bytes and no larger than %d", CodeProtocol, workerAdmitEstimatedBytesMin, admitMaxReserve)
	}
	req.estimatedBytes = estimated
	if raw, present := args["max_wait_ms"]; present {
		value, ok := exactAdmitInt64(raw)
		if !ok {
			return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit max_wait_ms must be an integer", CodeProtocol)
		}
		// PRESENT and zero → non-blocking probe. PRESENT and positive → a blocking
		// claim (no timeout), the same S13 treatment the confine admit path gives a
		// positive max_wait_ms. ABSENT (below) is also a blocking claim.
		req.nonBlocking = value == 0
	}
	return req, nil
}

// writeWorkerAdmitResponse stamps the observed wait and writes the one terminal
// worker-admit frame. OK is true exactly on a grant. It never returns the write
// error: on a grant the lease is released by the holder's EOF (design §3), NOT by
// this write's success, so the caller falls through to the held read regardless
// (serveReDeclare's ack does the same); on a non-grant the handler returns anyway.
func (s *Server) writeWorkerAdmitResponse(conn net.Conn, start time.Time, resp WorkerAdmitResponse) {
	resp.WaitedMS = elapsedMilliseconds(start, s.admitNowTime())
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	ok := resp.State == runner.WorkerAdmitStateGranted
	_ = writeFrame(conn, responseFrame(core.Response{OK: ok, Code: "OK", Data: resp}))
}

// workerAdmitSnapshot is the non-blocking probe's answer: the unified ledger's
// current available RAM and CPU under path's queue, with NO reservation taken
// (design §6/§8). During the restart freeze it reports unevaluated rather than a
// figure — the slice is not saturated, it is frozen (the S11 honesty pin).
//
// It re-derives available the same way evaluateAdmitQueue does (checkedAvailable in
// dev, ledgerAvailable in shim, cpuAvailable for cores), reading the queue's ledger
// under queue.mu. A slice with no queue yet reads outstanding 0 (the ceiling is
// wholly available).
func (s *Server) workerAdmitSnapshot(path string, current, maximum, reclaimable int64) WorkerAdmitResponse {
	now := s.admitNowTime()
	if s.restartFrozenAt(now) {
		return WorkerAdmitResponse{
			State: runner.WorkerAdmitStateUnevaluated, Class: runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonSnapshot,
			Detail: "restart freeze: available is transiently unestablished",
		}
	}
	var outstanding, cpuOutstanding int64
	outstandingJobs := 0
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	s.admitRegistryMu.Unlock()
	if queue != nil {
		queue.mu.Lock()
		outstanding, cpuOutstanding, outstandingJobs = queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs
		queue.mu.Unlock()
	}
	headroom := s.admitSliceHeadroom(addJobCountClamp(outstandingJobs, 1))
	effectiveMaximum := maximum
	var available int64
	if s.shimMode() {
		available = ledgerAvailable(effectiveMaximum, outstanding, headroom)
	} else {
		effectiveMaximum = s.admitEffectiveMaximum(path, maximum)
		available = checkedAvailable(current, effectiveMaximum, reclaimable, outstanding, headroom)
	}
	return WorkerAdmitResponse{
		State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassContended,
		Reason:         runner.WorkerAdmitReasonSnapshot,
		AvailableBytes: available,
		AvailableCPU:   cpuAvailable(s.cpuCeiling(), cpuOutstanding),
	}
}

// workerAdmitEnqueueRejection maps an enqueue error code to a worker-admit
// disposition. CodeAdmitTooLarge is the one genuinely-terminal case (a request
// larger than the whole ceiling can never fit); every other code is reported
// RETRIABLE (contended) so the supervisor polls rather than abandoning
// daemon-backed admission and running the whole suite UNCONFINED — the AIRA-63
// safety regression a wrongly-terminal class caused.
func workerAdmitEnqueueRejection(code string) WorkerAdmitResponse {
	if code == CodeAdmitTooLarge {
		return WorkerAdmitResponse{
			State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassRequestInvalid,
			Reason: runner.WorkerAdmitReasonExceedsCeiling,
			Detail: "estimated bytes exceed the slice ceiling minus headroom",
		}
	}
	return WorkerAdmitResponse{
		State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassContended,
		Reason: runner.WorkerAdmitReasonAdmitSlotsSaturated,
		Detail: "worker-admit enqueue refused: " + code,
	}
}

// workerAdmitConnection admits ONE worker as a lease on the unified signed ledger
// and holds it on this connection for the worker's life; the connection's EOF
// releases the lease (design §3, the AIRA-41 reversal). One connection per worker
// lease.
//
// AIRA-41 REVERSAL. Before S15 a closed worker-admit connection freed NOTHING (the
// ledger summed the outer scope's `.aira-worker-*` children, so a killed relay
// could not drop a live worker's charge). S15 makes the worker lease a normal
// signed-ledger lease keyed on the worker's scope path and RELEASES it on the
// holder's EOF via compare-and-release (releaseAdmitWaiterAnchored) — the same
// primitive S8 built for confine. RAM (and the worker's one core) return
// IMMEDIATELY when the relay closes, not at suite end.
func (s *Server) workerAdmitConnection(conn net.Conn, args map[string]any) {
	start := s.admitNowTime()
	// AIRA-63: worker-admit shares the admitSlots semaphore that bounds concurrent
	// admission connections. Saturation is a `contended` DENIAL (retriable), not an
	// error frame — an unrecognised error Code would be classed contract-violation
	// (terminal) by the client and strip containment for the whole suite.
	if !s.acquireAdmitSlot() {
		s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
			State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonAdmitSlotsSaturated,
		})
		return
	}
	slotReleased := false
	releaseSlot := func() {
		if !slotReleased {
			slotReleased = true
			s.releaseAdmitSlot()
		}
	}
	defer releaseSlot()

	req, err := validateWorkerAdmitArgs(args)
	if err != nil {
		_ = writeFrame(conn, errorFrame(CodeProtocol, err.Error()))
		return
	}

	// THE MODE-AGREEMENT CHECK, both directions. The client's outer_scope is the
	// ci-shim sentinel exactly when the CLIENT resolved shim mode, and an absolute
	// cgroup path exactly when it resolved real mode. A disagreement means two
	// processes read different install-mode records; neither branch below is safe on
	// the other's request. Terminal (admission-unusable), because waiting cannot make
	// two records agree.
	shim := s.shimMode()
	if shim != (req.outerScope == runner.ShimConfineSlice) {
		s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
			State: runner.WorkerAdmitStateUnavailable, Class: runner.WorkerAdmitClassAdmissionUnusable,
			Reason: runner.WorkerAdmitReasonConfineModeMismatch,
			Detail: "this daemon is in " + s.confineModeName() + " mode and the client asked about outer scope " + req.outerScope,
		})
		return
	}

	// Resolve the ONE slice whose signed ledger every lease charges (design §7, D1):
	// aira.slice in real mode, the ci-shim sentinel in shim mode. This is the SAME
	// resolver+input serveReDeclare uses, so a fresh worker lease and its
	// post-restart re-declare land in the identical queue and cannot double-count.
	path, ok, resolveReason := s.sliceResolver()(runner.DefaultConfineSlice)
	if !ok {
		s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
			State: runner.WorkerAdmitStateUnevaluated, Class: runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonOuterScopeUnreadable, Detail: resolveReason,
		})
		return
	}
	current, maximum, reclaimable, ok, memReason := s.memoryReader()(path)
	if !ok {
		// FAIL CLOSED on a new admission, but RETRIABLE: the supervisor keeps polling
		// rather than stripping containment. A persistent failure ends the claim only
		// when the client gives up (its connection closes).
		s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
			State: runner.WorkerAdmitStateUnevaluated, Class: runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonOuterScopeUnreadable, Detail: memReason,
		})
		return
	}

	if req.nonBlocking {
		// Non-blocking probe: report current available, reserve nothing, create no
		// scope. The aitest supervisor sizes its pool from this; a CLAIM is a blocking
		// request (max_wait_ms absent or positive).
		s.writeWorkerAdmitResponse(conn, start, s.workerAdmitSnapshot(path, current, maximum, reclaimable))
		return
	}

	// BLOCKING CLAIM (design §8 v1 scheduler): reserve {ram, one core} on the unified
	// ledger and wait until it fits conjunctively (RAM AND CPU), then create the
	// worker's cgroup scope as a SIBLING directly under the slice (S2a §4) and hold
	// the lease. There is no outer-cap aggregate scan, and no shared smaller-than-slice
	// parent cap either: each worker's own memory.oom.group bounds its own footprint,
	// and the unified ledger bounds Σ(all leases) ≤ the slice ceiling. AIRA-229's
	// whole-suite kill and AIRA-232's multi-supervisor breach dissolve under this
	// topology (design §11) — there is no aggregate for an outer oom.group to kill.

	// Exceeds-ceiling fast-fail, BEFORE the worker-id allocation reads the tree: a
	// request larger than the whole slice ceiling can never fit, so refuse it up
	// front (terminal, request-invalid) rather than allocate an id and enqueue a
	// waiter that would block forever. enqueue re-checks the ceiling authoritatively
	// against the jobs-scaled headroom; this pre-check only avoids the doomed work.
	if req.estimatedBytes > subtractFloor(maximum, s.admitSliceHeadroom(1)) {
		s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
			State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassRequestInvalid,
			Reason: runner.WorkerAdmitReasonExceedsCeiling,
			Detail: fmt.Sprintf("estimated %d bytes exceeds the slice ceiling", req.estimatedBytes),
		})
		return
	}

	// The suite scope-id this worker is a sub-reservation OF — the EXPLICIT wire
	// field (design §16d), validated non-empty and parseable above. In real mode the
	// worker scope NAME embeds the PARENT supervisor pid copied out of this id (S2a
	// §16a) — the pid the Task-5 escape exemption checks locally against os.Getpid().
	parentScopeID := req.parentScopeID

	// Mint the worker's scope id + path BEFORE enqueue so the lease has a stable
	// scope-id key. Real mode mints a first-class confine id
	// (CONFINE-aitest-w<seq>-<parentPid>-<stamp>) — unique by construction, so
	// there is no tree re-seed and no EEXIST path; shim mode has no cgroup, so a
	// synthetic monotonic id keys the advisory lease.
	var workerID, scopePath, scopeID string
	if shim {
		workerID = strconv.FormatUint(s.shimWorkerSeq.Add(1), 10)
		scopeID = "ci-shim-worker-" + workerID
	} else {
		_, parentPID, _, _, ok := runner.ParseConfineScopeID(parentScopeID)
		if !ok {
			// No parent pid to stamp into the worker name: refuse terminally rather
			// than mint a scope whose pid slot the reaper and the escape exemption
			// cannot reason about. (Task 2 makes the authoritative refusal the
			// arg-validator on the explicit parent_scope_id field; this stays as a
			// defensive invariant — a worker can never silently lose its parent pid.)
			s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
				State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassRequestInvalid,
				Reason: runner.WorkerAdmitReasonParentScopeUnparseable,
				Detail: fmt.Sprintf("parent scope id %q is not a canonical confine id", parentScopeID),
			})
			return
		}
		seq := int(s.workerScopeSeq.Add(1))
		scopeID = runner.MintWorkerScopeID(seq, parentPID)
		// S2a §4: the worker scope is a SIBLING under the resolved slice (path), not
		// nested under req.outerScope. req.outerScope now serves only the mode-agreement
		// sentinel check above and the parent↔worker linkage carried in parentScopeID.
		scopePath = runner.WorkerScopeChildPath(path, scopeID)
		workerID = strconv.Itoa(seq)
	}

	request := admitRequest{
		reserve:       req.estimatedBytes,
		cpu:           runner.DefaultConfineCPUCores,
		scopeID:       scopeID,
		parentScopeID: parentScopeID,
		signature:     req.signature,
		conn:          conn,
	}
	// Anchor identity, read BEFORE the enqueue lock (mirrors admitConnection): the
	// lease's EOF release compares this handler's own conn against the anchor, and
	// the re-declare same-uid gate reads peerSameUID. A net.Pipe test conn with no
	// injected credential seam simply anchors with pid 0.
	if uid, pid, credErr := s.peerCredentialOf(conn); credErr == nil {
		request.peerSameUID = uid == os.Geteuid()
		if pid > 0 {
			request.clientPID = pid
			if tick, ok, _ := readProcStartTime(pid); ok {
				request.processStartTick = tick
			}
		}
	}
	queue, waiter, code, enqErr := s.enqueueResolvedConfineAdmit(path, req.estimatedBytes, workerAdmitBasis, maximum, request)
	if enqErr != nil {
		s.writeWorkerAdmitResponse(conn, start, workerAdmitEnqueueRejection(code))
		return
	}
	peerCtx, cancelPeer := watchPeerEOF(conn)
	defer cancelPeer()
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		s.releaseAdmitWaiterAnchored(queue, waiter, conn)
	}
	defer release()

	// A blocking claim has NO daemon-side deadline (design §4/§6): the wait ends only
	// on a grant, a stopping daemon, or the peer closing its connection.
	select {
	case <-waiter.grantedCh:
	case <-s.stopping:
		return
	case <-peerCtx.Done():
		return
	}
	queue.mu.Lock()
	grantedState := waiter.state
	queue.mu.Unlock()
	if grantedState != admitGranted {
		// A blocking claim never times out, so the only non-grant terminal state is a
		// rejection by the exclusive-drain unestablished-emptiness rule — retriable.
		s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
			State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonSaturated,
		})
		return
	}
	// Granted. Release the shared admission slot NOW (release-after-grant): the lease
	// is charged on the ledger and will be held on this connection for the worker's
	// whole life, which is NOT admission negotiation. A re-declared worker lease
	// (serveReDeclare) holds no slot either, so releasing here keeps a fresh grant
	// consistent with its own re-declared future and decouples the max live-worker
	// count from admitGlobalMax and the box's core count. Queued claims still hold a
	// slot while waiting, bounded by admitMaxWaiters (256) < admitGlobalMax (1024).
	releaseSlot()

	containment := runner.WorkerAdmitContainmentEnforced
	reserved := int64(0)
	memoryMax := req.estimatedBytes
	swapCap := ""
	if shim {
		// Advisory: no cgroup to name and no cap to write; the outcome renderer refuses
		// an advisory grant that carries a scope_path or memory_max, so both stay zero
		// and the booking is reported via Reserved instead.
		containment = runner.WorkerAdmitContainmentAdvisory
		reserved = req.estimatedBytes
		memoryMax = 0
	} else {
		// The daemon creates the worker's cgroup scope AFTER the grant — never under
		// queue.mu (the evaluator must do no filesystem I/O). It is created as a SIBLING
		// under the resolved slice (path), via the ordinary confine scope-creation path,
		// NOT nested under req.outerScope (S2a §4). The ledger already charges the
		// reserve; a creation failure discharges it.
		create := s.workerScopeCreate
		if create == nil {
			create = runner.CreateWorkerScope
		}
		sp, sc, createErr := create(peerCtx, path, scopeID, req.estimatedBytes)
		if createErr != nil {
			release()
			// Fail closed: no grant is delivered without its scope. request-invalid is
			// the TERMINAL-BUT-DAEMON-HEALTHY disposition — a `contended` class would
			// retry indefinitely, stalling every aitest run on the machine.
			s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
				State: runner.WorkerAdmitStateDenied, Class: runner.WorkerAdmitClassRequestInvalid,
				Reason: runner.WorkerAdmitReasonWorkerScopeCreateFailed, Detail: createErr.Error(),
			})
			return
		}
		scopePath = sp
		swapCap = sc
	}
	s.writeWorkerAdmitResponse(conn, start, WorkerAdmitResponse{
		State: runner.WorkerAdmitStateGranted, Class: runner.WorkerAdmitClassGranted,
		WorkerID: workerID, ScopePath: scopePath, MemoryMax: memoryMax,
		SwapCap: swapCap, Containment: containment, Reserved: reserved,
		ParentScopeID: parentScopeID,
	})
	// Hold the lease until the holder's EOF (the deferred compare-and-release then
	// discharges the ledger) or graceful shutdown. The write result above does not
	// gate this: release is EOF-keyed, not write-keyed.
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
}
