package runner

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AIRA-27's class steering, and AIRA-113's dynamic steering on top of it.
//
// These two constants and their env overrides used to live in confine_linux.go,
// beside the only caller that then existed (confineSetupArgv, which passes the
// resolved value to the confined child so it can write its OWN
// /proc/self/oom_score_adj at exec). AIRA-113 gives them a SECOND caller in a
// different process — the daemon, deciding what an already-running scope's pids
// should carry — and the two must never disagree about what a class's baseline
// is. So the policy moved to this portable file, which the daemon can call on
// any platform, and the launcher keeps calling the identical function.
const (
	// ConfineOOMScoreAdj is the non-delegate class baseline: the AIRA-27
	// "protected" class. It is still hugely more killable than anything
	// unconfined (adj 0), which is the property that makes a confined job the
	// preferred victim of a HOST OOM.
	ConfineOOMScoreAdj = 500
	// ConfineDelegateOOMScoreAdj is the --delegate-ram class baseline: the
	// AIRA-27 "preferred victim" class, because a delegate scope's memory.max is
	// a generous ceiling rather than its reserve, so it is the class most likely
	// to be the one over-committing the slice.
	ConfineDelegateOOMScoreAdj = 800
	// ConfineMaxOOMScoreAdj is the kernel's ceiling for oom_score_adj and the
	// value AIRA-113 steers a proven offender to.
	ConfineMaxOOMScoreAdj = 1000
)

// OOMScoreSteerResult is what one subtree walk actually established. Every
// count is a positive fact; nothing here is inferred.
//
// Failed is deliberately not an error: a pid that exited between the
// cgroup.procs read and the write is the normal case for a busy scope, not a
// fault, and a walk that wrote 200 of 201 pids has still steered the scope.
// Skipped counts pids that failed the identity re-check below (see
// pidInConfineScope) — they were NOT written, and that is the safe direction.
type OOMScoreSteerResult struct {
	Cgroups int
	PIDs    int
	Written int
	Failed  int
	Skipped int
}

// ConfineClassOOMScoreAdj returns the AIRA-27 class baseline for a scope id.
//
// The class is read from the scope ID itself (the restart-surviving cap-type
// carrier), so it needs no daemon memory and no cgroup read, and it honours the
// same two environment overrides the launcher honours — including their
// ordering invariant (delegate strictly above non-delegate). An unparseable
// override is an ERROR rather than a silent fallback: a steering decision made
// against a baseline the launcher did not use would be steering against the
// wrong number.
func ConfineClassOOMScoreAdj(scopeID string) (int, error) {
	nonDelegate, delegate, err := confineOOMScoreAdjValues()
	if err != nil {
		return 0, err
	}
	if IsDelegateRAMScopeID(scopeID) {
		return delegate, nil
	}
	return nonDelegate, nil
}

func confineOOMScoreAdjValues() (nonDelegate, delegate int, err error) {
	nonDelegate, err = parseConfineOOMScoreAdjEnv("AIRA_CONFINE_OOM_SCORE_ADJ", ConfineOOMScoreAdj)
	if err != nil {
		return 0, 0, err
	}
	delegate, err = parseConfineOOMScoreAdjEnv("AIRA_CONFINE_OOM_SCORE_ADJ_DELEGATE", ConfineDelegateOOMScoreAdj)
	if err != nil {
		return 0, 0, err
	}
	if delegate <= nonDelegate {
		return 0, 0, errors.New("E_CONFINE_ARGUMENT_INVALID: AIRA_CONFINE_OOM_SCORE_ADJ_DELEGATE must be greater than AIRA_CONFINE_OOM_SCORE_ADJ")
	}
	return nonDelegate, delegate, nil
}

func parseConfineOOMScoreAdjEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < ConfineOOMScoreAdj || value > ConfineMaxOOMScoreAdj {
		return 0, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %s must be an integer in [%d, %d]", name, ConfineOOMScoreAdj, ConfineMaxOOMScoreAdj)
	}
	return value, nil
}

// SetSubtreeOOMScoreAdj writes adj to /proc/<pid>/oom_score_adj for every
// process in the cgroup subtree rooted at scopePath, and reports what it
// actually managed to do.
//
// SUBTREE, not leaf, and that is the whole reason this function exists rather
// than a Members() loop: Members() reads LEAF cgroup.procs, and cgroup-v2's
// no-internal-process rule means a scope that created any child cgroup has NO
// pids of its own. BootstrapAitestSupervisor drains every pid of an aitest
// outer scope into <outer>/.aira-supervisor and .aira-worker-N, and a confine
// job using podman --cgroups=split nests likewise, so a leaf-only walker would
// steer exactly zero processes for the population most likely to be the
// offender — the inert-subsystem failure this project has shipped once already.
//
// An out-of-range adj is refused rather than clamped: the caller decides
// policy, and a clamp would let a policy bug write a value nobody chose. The
// floor is ConfineOOMScoreAdj, not the kernel's -1000, because nothing in AIRA
// may make a confined job LESS killable than its class baseline; that is
// AIRA-27's containment and this subsystem exists to sharpen it, never to
// weaken it.
func SetSubtreeOOMScoreAdj(scopePath string, adj int) (OOMScoreSteerResult, error) {
	if adj < ConfineOOMScoreAdj || adj > ConfineMaxOOMScoreAdj {
		return OOMScoreSteerResult{}, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: oom_score_adj %d is outside the steerable range [%d, %d]", adj, ConfineOOMScoreAdj, ConfineMaxOOMScoreAdj)
	}
	return setSubtreeOOMScoreAdj(scopePath, adj)
}
