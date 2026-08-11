package runner

// Arbitration is the small closed state machine used by the durable waiter
// and kill-intent protocol. It is intentionally independent of PIDs/cgroups,
// which makes the race rule testable without privileged kernel state.
type Arbitration struct {
	WaitPublished bool
	KillIntent    bool
	Terminal      Status
}

func (a Arbitration) PublishWait() bool {
	return !a.KillIntent && !a.Terminal.Terminal()
}

func (a Arbitration) PublishKillIntent() bool {
	return !a.WaitPublished && !a.Terminal.Terminal()
}

func (a *Arbitration) Wait() bool {
	if !a.PublishWait() {
		return false
	}
	a.WaitPublished = true
	return true
}

func (a *Arbitration) Intent() bool {
	if !a.PublishKillIntent() {
		return false
	}
	a.KillIntent = true
	return true
}

func (a *Arbitration) CommitWait(status Status) bool {
	if !a.WaitPublished || a.KillIntent || a.Terminal.Terminal() || status != StatusExited {
		return false
	}
	a.Terminal = status
	return true
}

func (a *Arbitration) CommitKill() bool {
	if !a.KillIntent || a.Terminal.Terminal() {
		return false
	}
	a.Terminal = StatusKilled
	return true
}
