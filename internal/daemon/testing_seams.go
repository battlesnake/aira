package daemon

func (s *Server) SetAdmitReadMemoryForTest(fn func(string) (int64, int64, int64, bool, string)) {
	s.admitReadMemory = fn
}

// SetAdmitReadWorkerSupervisorMemoryForTest overrides the aggregate guard's
// supervisor-scope read (worker_admit.go) -- a SEPARATE seam from
// SetAdmitReadMemoryForTest above, since the supervisor's own child scope
// is deliberately never given a memory.max (AIRA-38) and so cannot use the
// same reader as the outer-scope ledger read.
func (s *Server) SetAdmitReadWorkerSupervisorMemoryForTest(fn func(string) (int64, int64, bool, string)) {
	s.admitReadWorkerSupervisorMemory = fn
}

// SetWorkerAdmitHeadroomForTest overrides the production worker-admit
// headroom default (64 MiB, worker_admit.go) so a test admitting against
// small synthetic byte values (or a small real cgroup memory.max) is not
// universally denied.
func (s *Server) SetWorkerAdmitHeadroomForTest(value int64) {
	s.workerAdmitHeadroom = value
}
