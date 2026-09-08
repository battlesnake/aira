package main

import (
	"fmt"

	"aira/internal/runner"
)

// AIRA-187. A nested `aira confine` does not stay inside its parent's cgroup:
// it deliberately resolves the same slice, creates a SIBLING scope, and requests
// admission as an ordinary independent request with no relation to the parent's
// existing grant. Under real contention that second request can consume its
// whole admission wait and produce an unrun leg — while the parent's own
// reservation, sized for its peak, sat there already covering the work.
//
// The coordinate needed to notice this has been exported to every nested launch
// for as long as the coordinate has existed (AIRA_CONFINE_SCOPE_ID, published by
// pylib.AppendConfineChildEnvironment). Only `aira confine-reserve` ever read
// it; a plain `aira confine` never looked, and so never said anything.
//
// This is the whole fix, and deliberately so: a warning, not a refusal, and not
// grant-inheritance machinery. Cgroup v2 memory accounting is already
// hierarchical, so the correct remedy costs nothing and lives in the caller's
// own script — drop the inner wrapper and let the child inherit the parent's
// scope by ordinary fork/exec. Building an admission path to route around a
// caller double-confining would be exactly the complexity this project's
// architectural-simplicity preference argues against.

// inheritedConfineScopeID is a seam. Production reads the environment through
// runner.InheritedConfineScopeID, which returns "" for an absent OR malformed
// coordinate — a stray value must never be reported as a parent scope.
var inheritedConfineScopeID = runner.InheritedConfineScopeID

// nestedConfineWarning returns the line to print for a nested launch, or "".
//
// `--exclusive` is exempt, and that exemption is the ticket's, not a
// convenience: an exclusive request is a deliberate statement about the whole
// slice rather than an accidental second reservation, and AIRA-101's
// ExclusiveHolderEnv already gives nesting under an exclusive hold a defined,
// non-deadlocking meaning. Warning there would be advising against the one
// nesting shape the design supports on purpose.
//
// Everything else about the launch is irrelevant on purpose. Whether the nested
// call names a different --slice, a smaller --memory-reserve or --delegate-ram
// changes nothing about the claim being made: it still requests its own
// admission and still does not draw on the parent's grant.
func nestedConfineWarning(options map[string]string, parentScopeID string) string {
	if parentScopeID == "" || options["exclusive"] == "true" {
		return ""
	}
	// Every clause is true whatever else the nested launch asks for, including a
	// different --slice: it still requests admission of its own, it still draws
	// on no existing grant, and dropping the wrapper still leaves the command in
	// the parent's scope. Nothing here claims the parent's reservation is
	// SUFFICIENT for the work — that is the caller's judgement, not a fact this
	// process can establish.
	return fmt.Sprintf("confine: this launch is nested inside confine scope %s; "+
		"it requests slice admission of its own and does not draw on the parent's already-granted reservation, "+
		"so it queues independently of the job it is running inside. "+
		"Running the command WITHOUT an inner `aira confine` would instead leave it in the parent's scope, "+
		"charged to the reservation the parent already holds.",
		parentScopeID)
}
