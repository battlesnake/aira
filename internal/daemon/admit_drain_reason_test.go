package daemon

import (
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-185. `aira drain wait --reason` adds ONE optional wire field to the
// admit protocol. These tests pin both halves of what that means: the validator
// accepts it additively and refuses every malformed shape, and NOTHING about
// admission changes because of it.

// drainReasonArgs is a complete, valid exclusive admit request — the only shape
// a reason may legally travel in.
func drainReasonArgs(t *testing.T, reason any) map[string]any {
	t.Helper()
	scope := exclusiveScopeID(t, "drain", 4242)
	args := map[string]any{
		"slice": "aira.slice", "reserve": int64(1), "max_wait_ms": int64(1000),
		"exclusive": true, "scope_id": scope, "name": "drain", "owner": exclusiveScopeOwner(scope),
	}
	if reason != nil {
		args["reason"] = reason
	}
	return args
}

// exclusiveScopeOwner extracts the owner the helper-minted scope id binds to, so
// this file does not restate the scope-id grammar beside the one parser.
func exclusiveScopeOwner(scopeID string) string {
	_, _, _, owner, _ := runner.ParseConfineScopeID(scopeID)
	return owner
}

// verifies: AIRA-185
func TestValidateAdmitArgsAcceptsAnExclusiveHoldReason(t *testing.T) {
	request, err := validateAdmitArgs(drainReasonArgs(t, "deploy: slice-ceiling flip"), admitWaitCeilingMs)
	if err != nil {
		t.Fatalf("a valid reason was refused: %v", err)
	}
	if request.exclusiveReason != "deploy: slice-ceiling flip" {
		t.Fatalf("reason=%q", request.exclusiveReason)
	}
	if !request.exclusive || request.name != "drain" {
		t.Fatalf("the reason changed some other field: %+v", request)
	}
	// The whole point of a separate field: this text can satisfy NEITHER
	// constraint the `name` field carries, so it could not have been smuggled
	// through it.
	if runner.ValidateConfineIdentity("deploy: slice-ceiling flip") == nil {
		t.Fatal("if a confine identity now accepts spaces and colons, this field is redundant")
	}

	// Omitting it stays valid and yields a positive absence, never a placeholder.
	request, err = validateAdmitArgs(drainReasonArgs(t, nil), admitWaitCeilingMs)
	if err != nil || request.exclusiveReason != "" {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}

// verifies: AIRA-185
func TestValidateAdmitArgsRefusesMalformedOrMisplacedHoldReasons(t *testing.T) {
	// A non-string reason is a protocol error, not a value to coerce.
	if _, err := validateAdmitArgs(drainReasonArgs(t, 7), admitWaitCeilingMs); err == nil {
		t.Fatal("a non-string reason was accepted")
	}
	if _, err := validateAdmitArgs(drainReasonArgs(t, true), admitWaitCeilingMs); err == nil {
		t.Fatal("a boolean reason was accepted")
	}

	// A reason on a NON-exclusive request is refused rather than accepted and
	// discarded: it is reported only through the exclusive state, so nothing would
	// ever render it and the caller would be silently ignored.
	nonExclusive := map[string]any{
		"slice": "aira.slice", "reserve": int64(1), "max_wait_ms": int64(1000),
		"reason": "deploy",
	}
	if _, err := validateAdmitArgs(nonExclusive, admitWaitCeilingMs); err == nil {
		t.Fatal("a reason without exclusivity was accepted and would have been silently dropped")
	}

	// The field allowlist is still CLOSED. Widening the count to 13 must not have
	// opened the door to a fourteenth, unknown field.
	unknown := drainReasonArgs(t, "deploy")
	unknown["purpose"] = "deploy"
	if _, err := validateAdmitArgs(unknown, admitWaitCeilingMs); err == nil {
		t.Fatal("an unknown admit field was accepted")
	}
}

// The reason is retained on a long-lived waiter and rides into every
// `confine --list` reply, so it is bounded where it is retained — otherwise a
// few multi-megabyte labels push the whole response past MaxFrameBytes and the
// verb stops working for every job on the slice.
//
// verifies: AIRA-185
func TestValidateAdmitArgsBoundsAndTrimsTheHoldReason(t *testing.T) {
	huge := strings.Repeat("d", runner.ConfineExclusiveReasonWireLimit*4)
	request, err := validateAdmitArgs(drainReasonArgs(t, huge), admitWaitCeilingMs)
	if err != nil {
		t.Fatalf("an over-long reason must be bounded, not refused: %v", err)
	}
	if len([]rune(request.exclusiveReason)) != runner.ConfineExclusiveReasonWireLimit+1 {
		t.Fatalf("retained %d runes, want %d plus an ellipsis",
			len([]rune(request.exclusiveReason)), runner.ConfineExclusiveReasonWireLimit)
	}
	if !strings.HasSuffix(request.exclusiveReason, "…") {
		t.Fatalf("truncation must be marked: %q", request.exclusiveReason[len(request.exclusiveReason)-8:])
	}

	// Whitespace-only is an ABSENT reason, not a blank one: a renderer must omit
	// the clause rather than print an empty quoted field claiming a purpose was
	// stated.
	request, err = validateAdmitArgs(drainReasonArgs(t, "   \t\n "), admitWaitCeilingMs)
	if err != nil || request.exclusiveReason != "" {
		t.Fatalf("whitespace-only reason=%q err=%v", request.exclusiveReason, err)
	}
	request, err = validateAdmitArgs(drainReasonArgs(t, "  deploy  "), admitWaitCeilingMs)
	if err != nil || request.exclusiveReason != "deploy" {
		t.Fatalf("surrounding whitespace survived: %q err=%v", request.exclusiveReason, err)
	}
}

// End to end through the WIRE the operator actually reads: a drain's reason
// reaches `confine --list` in both the draining and the held state, and an
// ordinary exclusive job that supplied none reports none.
//
// verifies: AIRA-185
func TestExclusiveHoldReasonReachesTheConfineListWire(t *testing.T) {
	server, slicePath := exclusiveWireServer(t)
	drainScope := exclusiveScopeID(t, "drain", 4301)
	benchScope := exclusiveScopeID(t, "bench", 4302)
	enqueue := func(what string, request admitRequest) (*sliceQueue, *admitWaiter) {
		t.Helper()
		queue, waiter, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, request)
		if err != nil {
			t.Fatalf("enqueue %s: code=%s err=%v", what, code, err)
		}
		return queue, waiter
	}

	// An ordinary job keeps the slice non-empty, so the drain DRAINS first.
	queue, keeper := enqueue("keeper", admitRequest{})
	evaluate(t, server, queue)
	requireGranted(t, queue, keeper, "the keeper")

	_, drain := enqueue("the drain", admitRequest{
		exclusive: true, scopeID: drainScope, name: "drain", owner: exclusiveScopeOwner(drainScope),
		exclusiveReason: "deploy: slice-ceiling flip",
	})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, drain, "the drain behind the keeper")
	draining := exclusiveOnTheWire(t, server)
	if draining == nil || draining.State != admitExclusiveDraining {
		t.Fatalf("state=%+v", draining)
	}
	if draining.Reason != "deploy: slice-ceiling flip" {
		t.Fatalf("a draining hold lost its reason: %+v", *draining)
	}

	// Through the grant, where the state changes and the reason must not.
	server.releaseAdmitWaiter(queue, keeper)
	evaluate(t, server, queue)
	requireGranted(t, queue, drain, "the drain once the slice emptied")
	held := exclusiveOnTheWire(t, server)
	if held == nil || held.State != admitExclusiveHeld || held.Reason != "deploy: slice-ceiling flip" {
		t.Fatalf("a held hold lost its reason: %+v", held)
	}

	// A DIFFERENT exclusive requester that supplied no reason must report none —
	// never the previous holder's, which is exactly the carry-over AIRA-119
	// records for the identity fields.
	_, next := enqueue("an ordinary waiter", admitRequest{})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, next, "an ordinary waiter behind the hold")
	server.releaseAdmitWaiter(queue, drain)
	evaluate(t, server, queue)
	requireGranted(t, queue, next, "the ordinary waiter once the hold ended")

	_, bench := enqueue("a plain exclusive benchmark", admitRequest{
		exclusive: true, scopeID: benchScope, name: "bench", owner: exclusiveScopeOwner(benchScope),
	})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, bench, "the benchmark behind the ordinary waiter")
	plain := exclusiveOnTheWire(t, server)
	if plain == nil || plain.Name != "bench" {
		t.Fatalf("state=%+v", plain)
	}
	if plain.Reason != "" {
		t.Fatalf("a benchmark that stated no reason was given one: %q", plain.Reason)
	}
}

// The gate must be BLIND to the reason. This is the property the plan is
// emphatic about: --reason is metadata, not a behaviour change, and no
// admission, drain-convergence or blocking decision may consult it.
//
// verifies: AIRA-185
func TestTheHoldReasonChangesNoAdmissionDecision(t *testing.T) {
	for _, reason := range []string{"", "deploy", strings.Repeat("d", 400)} {
		server, slicePath := exclusiveWireServer(t)
		scope := exclusiveScopeID(t, "drain", 4401)
		enqueue := func(what string, request admitRequest) (*sliceQueue, *admitWaiter) {
			t.Helper()
			queue, waiter, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, request)
			if err != nil {
				t.Fatalf("enqueue %s: code=%s err=%v", what, code, err)
			}
			return queue, waiter
		}
		queue, keeper := enqueue("keeper", admitRequest{})
		evaluate(t, server, queue)
		requireGranted(t, queue, keeper, "the keeper")

		_, drain := enqueue("drain", admitRequest{
			exclusive: true, scopeID: scope, name: "drain", owner: exclusiveScopeOwner(scope),
			exclusiveReason: reason,
		})
		// Blocked while the slice is occupied, granted once it empties, and
		// blocking the next arrival while held — identically for every reason.
		evaluate(t, server, queue)
		requireStillQueued(t, queue, drain, "a drain must wait for a busy slice whatever its reason")
		server.releaseAdmitWaiter(queue, keeper)
		evaluate(t, server, queue)
		requireGranted(t, queue, drain, "a drain must be admitted to an empty slice whatever its reason")
		_, blocked := enqueue("a job arriving during the hold", admitRequest{})
		evaluate(t, server, queue)
		requireStillQueued(t, queue, blocked, "a hold must block new work whatever its reason")
	}
}
