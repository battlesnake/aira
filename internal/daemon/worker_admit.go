package daemon

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// workerAdmitHeadroomDefault is a SEPARATE, much smaller headroom than
// admitSliceHeadroomBase (2 GiB, sized for the whole machine-wide slice,
// admit.go). Reusing the slice-wide constant here would swallow most of a
// realistically-sized outer scope's own cap in production. This is a
// build-time tunable, not yet sized from field data — a reasonable small
// fixed default for Slice 1.
const workerAdmitHeadroomDefault int64 = 64 << 20 // 64 MiB

// WorkerAdmitResponse is the one grant/denial payload the worker-admit
// connection sends before optionally holding itself open as the lease.
type WorkerAdmitResponse struct {
	State      string `json:"state"` // "granted" | "denied" | "timeout" | "unevaluated"
	Reason     string `json:"reason,omitempty"`
	WaitedMS   int64  `json:"waited_ms"`
	WorkerID   string `json:"worker_id,omitempty"`
	ScopePath  string `json:"scope_path,omitempty"`
	MemoryMax  int64  `json:"memory_max,omitempty"`
	MemoryHigh int64  `json:"memory_high,omitempty"`
}

type workerAdmitRequest struct {
	jobID      string
	outerScope string
	// signature is accepted on the wire (the key spec 3.3 names for a
	// future per-suite peak-history-based cap-sizing backstop) but UNUSED
	// for anything in Slice 1 — deferred past Slice 1; estimatedBytes
	// alone governs the backstop cap for now (see also Task 17's
	// _resolve_estimated_bytes, which states the same deferral on the
	// Python side).
	signature      string
	estimatedBytes int64
	maxWaitMS      int64
}

type workerGrant struct {
	scopePath string
	memoryMax int64
}

type workerJobState struct {
	mu         sync.Mutex
	outerScope string
	nextSeq    int
	grants     map[string]*workerGrant
}

// workerJobKey binds ledger state to the (job_id, outer_scope) PAIR, not
// job_id alone — job_id is caller-supplied and only as unique as the
// caller's own pid-reuse window, so two concurrent requests that reuse the
// same job_id with DIFFERENT outer_scope values must never get their scope
// accounting mixed together.
func workerJobKey(jobID, outerScope string) string {
	return jobID + "\x00" + outerScope
}

// workerJobs is never actively pruned once a job's last worker releases —
// accepted Slice 1 gap: unbounded-but-slow growth, one entry per distinct
// (job_id, outer_scope) pair across the daemon's lifetime. A real concern
// only for a very long-lived daemon running very many distinct aitest
// jobs; not worth cleanup machinery for Slice 1.
func (s *Server) workerJobFor(jobID, outerScope string) *workerJobState {
	key := workerJobKey(jobID, outerScope)
	s.workerJobsMu.Lock()
	defer s.workerJobsMu.Unlock()
	if s.workerJobs == nil {
		s.workerJobs = make(map[string]*workerJobState)
	}
	job := s.workerJobs[key]
	if job == nil {
		job = &workerJobState{outerScope: outerScope, grants: make(map[string]*workerGrant)}
		s.workerJobs[key] = job
	}
	return job
}

// evaluateWorkerAdmit makes one synchronous grant/deny decision for req.
// "Used" is the OUTER scope's own live memory.current, read directly —
// cgroup memory accounting is hierarchical, so this single read already
// includes the supervisor's own RSS plus every already-placed worker's
// (spec 3.3). Summing individually-read worker-scope grants separately (an
// earlier version of this function did) was both redundant with that
// hierarchical accounting AND unsafe: Σ(worker grants) + supervisor RSS
// could exceed outerMax even when the ledger thought there was room,
// risking an outer-scope-level memory.oom.group kill of the ENTIRE run —
// precisely the incident class this design exists to prevent.
func (s *Server) evaluateWorkerAdmit(req workerAdmitRequest) WorkerAdmitResponse {
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	used, outerMax, _, ok, reason := readMemory(req.outerScope)
	if !ok {
		return WorkerAdmitResponse{State: "unevaluated", Reason: reason}
	}
	headroom := s.workerAdmitHeadroom
	if headroom < 0 {
		headroom = 0
	}
	ceiling := outerMax - headroom
	if req.estimatedBytes > ceiling {
		// Could never fit even at zero current usage — a stable fact about
		// THIS request, not a transient contention moment. Deny
		// immediately (workerAdmitConnection, Task 5, breaks its poll loop
		// on this reason) instead of waiting out the full poll timeout
		// only to time out anyway.
		return WorkerAdmitResponse{State: "denied", Reason: "reject:exceeds-ceiling"}
	}
	job := s.workerJobFor(req.jobID, req.outerScope)
	job.mu.Lock()
	defer job.mu.Unlock()
	if req.estimatedBytes > ceiling-used {
		// Not available RIGHT NOW (transient: current live usage), but
		// could be granted once usage drops — the caller's poll loop keeps
		// retrying this until granted or its own max_wait_ms deadline
		// converts it to "timeout".
		return WorkerAdmitResponse{State: "denied", Reason: "fallback:insufficient-headroom"}
	}
	// Worst-case guard, on top of the live-usage check above: live usage
	// having room RIGHT NOW does not mean it always will. Sum the
	// memory.max already promised to this job's other workers — if every
	// one of them simultaneously grew to its own full cap, the total must
	// still fit under ceiling, or an outer-scope memory.oom.group kill can
	// take out the whole run (supervisor plus every sibling worker), not
	// just the one that grew — precisely what Goal 2 in the design spec
	// requires this NOT be able to do. This trades a little utilization
	// (the live-usage check alone would admit a worker whose siblings
	// simply haven't grown to their peaks YET) for that hard guarantee —
	// the same aggregate-not-bound failure class AIRA-27/28/29 already
	// fixed at whole-job granularity, found here at worker granularity by
	// build-review (a live-usage-only check is silent on the SUM of caps,
	// only on CURRENT usage). Pollable, not an immediate reject: an
	// existing worker retiring frees its share of committed capacity.
	var committed int64
	for _, grant := range job.grants {
		committed += grant.memoryMax
	}
	if req.estimatedBytes > ceiling-committed {
		return WorkerAdmitResponse{State: "denied", Reason: "fallback:aggregate-cap-exceeded"}
	}
	job.nextSeq++
	workerID := fmt.Sprintf("%d", job.nextSeq)
	scopePath := runner.WorkerScopeChildPath(req.outerScope, "worker-"+workerID)
	memoryHigh := req.estimatedBytes * 4 / 5
	job.grants[workerID] = &workerGrant{scopePath: scopePath, memoryMax: req.estimatedBytes}
	return WorkerAdmitResponse{State: "granted", WorkerID: workerID, ScopePath: scopePath, MemoryMax: req.estimatedBytes, MemoryHigh: memoryHigh}
}

// releaseWorkerGrant frees one worker's ledger bookkeeping entry. Called
// when its connection closes (Task 5) — the same dies-with-socket lease
// shape admit and governor already use. Idempotent by construction (delete
// on an absent map key is a no-op): Task 5's deferred release can race a
// normal lease-close release of the same grant, and both must be safe.
func (s *Server) releaseWorkerGrant(jobID, outerScope, workerID string) {
	key := workerJobKey(jobID, outerScope)
	s.workerJobsMu.Lock()
	job := s.workerJobs[key]
	s.workerJobsMu.Unlock()
	if job == nil {
		return
	}
	job.mu.Lock()
	delete(job.grants, workerID)
	job.mu.Unlock()
}

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
	if req.signature, err = str("signature", false); err != nil {
		return workerAdmitRequest{}, err
	}
	// exactAdmitInt64 (existing, admit.go) — overflow-safe float64->int64,
	// reused rather than the naive int64(estimated) truncation this used
	// to do, which let an arbitrary huge float64 truncate unchecked.
	estimated, ok := exactAdmitInt64(args["estimated_bytes"])
	if !ok || estimated <= 0 || estimated > admitMaxReserve {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit estimated_bytes must be a positive number no larger than %d", CodeProtocol, admitMaxReserve)
	}
	req.estimatedBytes = estimated
	maxWait, ok := exactAdmitInt64(args["max_wait_ms"])
	if !ok || maxWait < 0 || maxWait > admitWaitCapMs {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit max_wait_ms must be in [0,%d]", CodeProtocol, admitWaitCapMs)
	}
	req.maxWaitMS = maxWait
	return req, nil
}

func (s *Server) workerAdmitConnection(conn net.Conn, args map[string]any) {
	req, err := validateWorkerAdmitArgs(args)
	if err != nil {
		_ = writeFrame(conn, errorFrame(CodeProtocol, err.Error()))
		return
	}
	peerCtx, cancelPeer := context.WithCancel(context.Background())
	defer cancelPeer()
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancelPeer()
	}()

	poll := s.workerAdmitPollInterval
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	deadline := s.admitNowTime().Add(time.Duration(req.maxWaitMS) * time.Millisecond)
	var response WorkerAdmitResponse
	for {
		response = s.evaluateWorkerAdmit(req)
		if response.State == "granted" || response.State == "unevaluated" {
			break
		}
		if response.State == "denied" && response.Reason == "reject:exceeds-ceiling" {
			// A stable "never going to fit" fact about this request, not a
			// transient contention moment — surface "denied" to the client
			// immediately instead of waiting out the full poll timeout
			// only to time out anyway. Every OTHER non-granted state keeps
			// polling below (a live-usage-driven "not right now" is
			// retried until it clears or the deadline converts it to
			// "timeout").
			break
		}
		if !s.admitNowTime().Before(deadline) {
			response = WorkerAdmitResponse{State: "timeout", Reason: "reject:saturated"}
			break
		}
		select {
		case <-time.After(poll):
		case <-peerCtx.Done():
			return
		case <-s.stopping:
			return
		}
	}

	// A "granted" response has ALREADY inserted a ledger entry inside
	// evaluateWorkerAdmit above. From this point on, EVERY exit path —
	// a write failure, the peer vanishing in the exact window between
	// grant and delivery, or the normal lease-close below — must release
	// that grant exactly once, or it leaks against the job's ledger
	// forever. Mirrors admitConnection's own deferred, idempotent release
	// (admit.go:458-466); releaseWorkerGrant is idempotent by construction
	// (delete on an absent key is a no-op), so a double-fire here (e.g. a
	// write failure racing this deferred call with a direct call further
	// down — there is none further down anymore, but the mirroring is
	// deliberate) is always safe.
	released := false
	release := func() {
		if released || response.State != "granted" {
			return
		}
		released = true
		s.releaseWorkerGrant(req.jobID, req.outerScope, response.WorkerID)
	}
	defer release()

	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	ok := response.State == "granted"
	if err := writeFrame(conn, responseFrame(core.Response{OK: ok, Code: "OK", Data: response})); err != nil {
		return
	}
	if !ok {
		return
	}
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
}
