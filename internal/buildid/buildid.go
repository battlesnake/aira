// Package buildid reports which commit the running binary was built from.
//
// AIRA-202. Every other component of AIRA can say what it is; the binary could
// not. Four rants in the 2026-09-09 corpus (RANT-20, RANT-22, RANT-23, RANT-25)
// were wholly or partly artifacts of an unlabelled stale binary — a defect
// reported against source that already carried the fix — and two more (RANT-19,
// RANT-29) reported messages master already had. The triage of that corpus had
// to re-derive commit ancestry by hand to separate "the code is wrong" from
// "your binary is old".
//
// The identity comes from the toolchain's own VCS stamp rather than an -ldflags
// -X variable, because the stamp is already present in the two populations that
// matter — the installed binary and the released artifacts — and a -X value can
// be set to anything by whoever builds, which is the opposite of provenance.
//
// The stamp is NOT universal, and this package's contract is built around where
// it is missing: Go does not record VCS information for a build made inside a
// LINKED git worktree (one whose .git is a file rather than a directory), and it
// omits it silently — no stamp and no error, even under an explicit
// -buildvcs=true. Since CLAUDE.md requires all AIRA development to happen in
// worktrees, the entire dev-build population is unstamped. That is why an
// unestablished identity is a first-class, explained answer here rather than an
// error: it is the normal state of a developer's own build.
package buildid

import "runtime/debug"

// Unevaluated is the rendered form of an identity that could not be established.
// It is deliberately the project's standard honesty token rather than "dev",
// "unknown" or an empty string: a consumer must not be able to print it as
// though it were a version.
const Unevaluated = "unevaluated"

// Identity is what the running binary can prove about its own origin.
type Identity struct {
	Established bool   `json:"established"`
	Revision    string `json:"revision,omitempty"`
	Time        string `json:"time,omitempty"`
	Modified    bool   `json:"modified,omitempty"`
	// Reason is set only when Established is false, and always then. It names the
	// cause so a reader can tell an explicable absence (a worktree build) from a
	// broken one.
	Reason string `json:"reason,omitempty"`
}

const missingStampReason = "this binary carries no VCS stamp, so the commit it was built from cannot be " +
	"established. The usual cause is a build made inside a linked git worktree, where Go omits the stamp " +
	"silently -- even under an explicit -buildvcs=true -- which is how AIRA development builds are made; " +
	"a build from a checkout with its own .git directory, such as the installed binary or a release " +
	"artifact, is normally stamped. This does NOT establish which of those applies here."

// Current reports the identity of the running binary.
func Current() Identity {
	info, ok := debug.ReadBuildInfo()
	return identityFrom(info, ok)
}

// identityFrom is the pure half, so every branch is reachable from a test
// without building a differently-stamped binary to drive it.
func identityFrom(info *debug.BuildInfo, ok bool) Identity {
	if !ok || info == nil {
		return Identity{Reason: "the runtime reported no build information for this binary"}
	}
	identity := Identity{}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			identity.Revision = setting.Value
		case "vcs.time":
			identity.Time = setting.Value
		case "vcs.modified":
			identity.Modified = setting.Value == "true"
		}
	}
	// The revision is the whole point: a stamp carrying a time but no revision
	// establishes nothing, so it is treated as absent rather than half-reported.
	if identity.Revision == "" {
		return Identity{Reason: missingStampReason}
	}
	identity.Established = true
	return identity
}

// String renders the identity for a human. An unestablished identity renders as
// the honesty token and never as a blank or a guess; callers wanting the cause
// read Reason.
func (i Identity) String() string {
	if !i.Established {
		return Unevaluated
	}
	rendered := i.Revision
	if i.Time != "" {
		rendered += " (" + i.Time + ")"
	}
	if i.Modified {
		rendered += " [modified: built from an uncommitted tree, so it is NOT this revision]"
	}
	return rendered
}

// Report is the whole `aira version` answer.
//
// Client and Daemon are separate fields, not one "aira version", because the
// component whose staleness actually changes behaviour is the DAEMON: create,
// show, link and rant are RouteDaemon and execute inside it, so its compiled-in
// internal/domain decides what is legal. RANT-21 and RANT-25 are both that shape
// — a P3 ticket refused by a daemon predating AIRA-170 while the files on disk
// already carried P3 — and a single merged number would have hidden exactly the
// split that mattered.
type Report struct {
	Client Identity `json:"client"`
	Daemon Identity `json:"daemon"`
	// Diverged is asserted ONLY when both identities are established and differ.
	// An unevaluated half is not a differing one, and claiming divergence from an
	// unknown would be the same fabrication this package exists to prevent.
	Diverged bool `json:"diverged"`
}

// NewReport pairs the two identities and settles divergence.
func NewReport(client, daemon Identity) Report {
	return Report{
		Client:   client,
		Daemon:   daemon,
		Diverged: client.Established && daemon.Established && client.Revision != daemon.Revision,
	}
}
