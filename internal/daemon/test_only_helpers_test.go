package daemon

import "testing"

// enqueueAdmit enqueues a plain (unresolved, non-confine) admit request.
func (s *Server) enqueueAdmit(path string, reserve int64) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, "", 0, false, admitRequest{})
}

// enqueueOn enqueues reserve on the queue for path and fails the test on error.
// Shim tests pass runner.ShimConfineSlice, the SENTINEL path and the only queue
// a shim daemon has: resolveShimSlicePath maps every requested slice name onto
// it, and confineManagement looks the registry up by that same key.
func enqueueOn(t *testing.T, server *Server, path string, reserve int64) (*sliceQueue, *admitWaiter) {
	t.Helper()
	queue, waiter, code, err := server.enqueueAdmit(path, reserve)
	if err != nil {
		t.Fatalf("enqueue: code=%s err=%v", code, err)
	}
	return queue, waiter
}

// effectiveWatchdogCapFrom is effectiveWatchdogCapEvaluated without the
// evaluated flag.
func effectiveWatchdogCapFrom(mount, path string) (int64, bool) {
	cap, finite, _ := effectiveWatchdogCapEvaluated(mount, path)
	return cap, finite
}
