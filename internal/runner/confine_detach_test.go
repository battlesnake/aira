package runner

import (
	"strings"
	"testing"
)

func detachRecord(mutate func(*ConfineDetachRecord)) ConfineDetachRecord {
	record := ConfineDetachRecord{
		Schema: ConfineDetachSchema, ScopeID: "CONFINE-gate-4242-abc@session-a",
		Name: "gate", Owner: "session-a", Slice: "aira.slice",
		Supervisor: PIDIdentity{PID: 4242, StartTick: 99, BootID: "boot-a"},
		Phase:      ConfineDetachPhaseRunning, StartedAt: "2026-09-05T10:00:00Z",
	}
	if mutate != nil {
		mutate(&record)
	}
	return record
}

func aliveProbe(alive, evaluated bool) ConfineSupervisorProbe {
	return func(PIDIdentity) (bool, bool) { return alive, evaluated }
}

// TestResolveConfineDetachStatusNeverFabricatesAnOutcome is the load-bearing
// honesty test for AIRA-22. `aira confine --status` exists so a resumed session
// can pick up a RESULT instead of re-running an hour of work, which makes
// claiming a result it does not have the single worst thing it can do -- and
// claiming `running` for a job that is still queued in admission the second
// worst.
//
// Every branch is a separate case so that an implementation which collapses any
// two of them fails here rather than in production.
//
// verifies: AIRA-22
func TestResolveConfineDetachStatusNeverFabricatesAnOutcome(t *testing.T) {
	exit7 := 7
	for _, test := range []struct {
		name       string
		record     ConfineDetachRecord
		probe      ConfineSupervisorProbe
		want       ConfineDetachState
		wantReason string
	}{
		{
			name:   "terminal record is finished without consulting liveness",
			record: detachRecord(func(r *ConfineDetachRecord) { r.Terminal = true; r.Exit = &exit7 }),
			// A dead supervisor MUST NOT turn a completed job into unknown.
			probe: aliveProbe(false, true),
			want:  ConfineDetachFinished,
		},
		{
			name:   "alive supervisor at proven placement is running",
			record: detachRecord(nil),
			probe:  aliveProbe(true, true),
			want:   ConfineDetachRunning,
		},
		{
			name:       "alive supervisor still in admission is admitting, never running",
			record:     detachRecord(func(r *ConfineDetachRecord) { r.Phase = ConfineDetachPhaseAdmitting }),
			probe:      aliveProbe(true, true),
			want:       ConfineDetachAdmitting,
			wantReason: "not running yet",
		},
		{
			name:   "alive supervisor before the launch gate is starting",
			record: detachRecord(func(r *ConfineDetachRecord) { r.Phase = ConfineDetachPhaseStarting }),
			probe:  aliveProbe(true, true),
			want:   ConfineDetachStarting,
		},
		{
			name:       "supervisor gone with no terminal record is outcome-unknown",
			record:     detachRecord(nil),
			probe:      aliveProbe(false, true),
			want:       ConfineDetachOutcomeUnknown,
			wantReason: "wrote no terminal record",
		},
		{
			name:       "supervisor liveness unevaluated is outcome-unknown, never running",
			record:     detachRecord(nil),
			probe:      aliveProbe(true, false),
			want:       ConfineDetachOutcomeUnknown,
			wantReason: "could not be evaluated",
		},
		{
			name:       "no probe at all is outcome-unknown",
			record:     detachRecord(nil),
			probe:      nil,
			want:       ConfineDetachOutcomeUnknown,
			wantReason: "not evaluated",
		},
		{
			name:       "unreadable record is outcome-unknown, not not-found",
			record:     ConfineDetachRecord{ScopeID: "CONFINE-gate-4242-abc@session-a", Name: "gate", Owner: "session-a", ReadError: "unexpected EOF"},
			probe:      aliveProbe(true, true),
			want:       ConfineDetachOutcomeUnknown,
			wantReason: "could not be read",
		},
		{
			name:       "unknown schema is outcome-unknown, never interpreted",
			record:     detachRecord(func(r *ConfineDetachRecord) { r.Schema = ConfineDetachSchema + 1; r.Terminal = true }),
			probe:      aliveProbe(true, true),
			want:       ConfineDetachOutcomeUnknown,
			wantReason: "cannot interpret",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, err := ResolveConfineDetachStatus([]ConfineDetachRecord{test.record}, "gate", "session-a", test.probe)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if status.State != test.want {
				t.Fatalf("state = %q, want %q (reason %q)", status.State, test.want, status.Reason)
			}
			if test.wantReason != "" && !strings.Contains(status.Reason, test.wantReason) {
				t.Fatalf("reason %q does not explain the verdict (want substring %q)", status.Reason, test.wantReason)
			}
			if test.want == ConfineDetachOutcomeUnknown && status.Reason == "" {
				t.Fatal("outcome-unknown must always say why")
			}
		})
	}
}

// A non-terminal record must never render an exit code, and a terminal record
// with no exit code must render `unevaluated` rather than a fabricated zero.
//
// verifies: AIRA-22
func TestFormatConfineDetachStatusNeverRendersAFabricatedExit(t *testing.T) {
	running := classifyConfineDetachRecord(detachRecord(nil), aliveProbe(true, true))
	if text := FormatConfineDetachStatus(running); strings.Contains(text, "exit=") {
		t.Fatalf("a running job rendered an exit code: %q", text)
	}
	errored := classifyConfineDetachRecord(detachRecord(func(r *ConfineDetachRecord) {
		r.Terminal = true
		r.ErrorCode = "E_CONFINE_UNAVAILABLE"
		r.Error = "slice aira.slice: uncapped"
	}), aliveProbe(false, true))
	text := FormatConfineDetachStatus(errored)
	if !strings.Contains(text, "exit=unevaluated") || !strings.Contains(text, "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("a failed launch must render exit=unevaluated and its code: %q", text)
	}
	if strings.Contains(text, "exit=0") || strings.Contains(text, "exit=1") {
		t.Fatalf("a failed launch fabricated an exit code: %q", text)
	}
}

// The captured-output paths are evidence and must be printed in EVERY state,
// including the one where the outcome is unknown.
//
// verifies: AIRA-22
func TestFormatConfineDetachStatusAlwaysPrintsTheCapturePaths(t *testing.T) {
	for _, probe := range []ConfineSupervisorProbe{aliveProbe(true, true), aliveProbe(false, true), aliveProbe(false, false)} {
		status := classifyConfineDetachRecord(detachRecord(func(r *ConfineDetachRecord) {
			r.StdoutPath, r.StderrPath, r.SupervisorLogPath = "/s/out", "/s/err", "/s/sup"
		}), probe)
		text := FormatConfineDetachStatus(status)
		for _, want := range []string{"stdout=/s/out", "stderr=/s/err", "supervisor-log=/s/sup"} {
			if !strings.Contains(text, want) {
				t.Fatalf("state %q omitted %q: %q", status.State, want, text)
			}
		}
	}
}

// verifies: AIRA-22
func TestResolveConfineDetachStatusRefusesAnAmbiguousSelectorAndNamesTheCandidates(t *testing.T) {
	first := detachRecord(func(r *ConfineDetachRecord) {
		r.ScopeID, r.StartedAt, r.Terminal = "CONFINE-gate-11-aaa@session-a", "2026-09-05T09:00:00Z", true
	})
	second := detachRecord(func(r *ConfineDetachRecord) {
		r.ScopeID, r.StartedAt = "CONFINE-gate-22-bbb@session-a", "2026-09-05T10:00:00Z"
	})
	_, err := ResolveConfineDetachStatus([]ConfineDetachRecord{first, second}, "gate", "session-a", aliveProbe(true, true))
	if err == nil {
		t.Fatal("two same-owner records named gate resolved to one; ambiguity must be refused")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "E_SELECTOR_AMBIGUOUS") {
		t.Fatalf("wrong code: %v", err)
	}
	for _, want := range []string{first.ScopeID, second.ScopeID, "2026-09-05T09:00:00Z", "2026-09-05T10:00:00Z", "finished", "running"} {
		if !strings.Contains(message, want) {
			t.Fatalf("ambiguity message omits %q, so it is not actionable: %s", want, message)
		}
	}
	// Newest first, so the operator's most likely target is named first.
	if strings.Index(message, second.ScopeID) > strings.Index(message, first.ScopeID) {
		t.Fatalf("candidates are not newest-first: %s", message)
	}
	// The exact same pair, but under different owners, is NOT ambiguous.
	second.Owner = "session-b"
	second.ScopeID = "CONFINE-gate-22-bbb@session-b"
	status, err := ResolveConfineDetachStatus([]ConfineDetachRecord{first, second}, "gate", "session-a", aliveProbe(true, true))
	if err != nil {
		t.Fatalf("owner scoping did not disambiguate: %v", err)
	}
	if status.Record.ScopeID != first.ScopeID {
		t.Fatalf("owner scoping picked %q, want %q", status.Record.ScopeID, first.ScopeID)
	}
}

// verifies: AIRA-22
func TestResolveConfineDetachStatusMatchesScopeIDNameAndSupervisorPID(t *testing.T) {
	record := detachRecord(nil)
	for _, selector := range []string{record.ScopeID, "gate", "4242"} {
		status, err := ResolveConfineDetachStatus([]ConfineDetachRecord{record}, selector, "session-a", aliveProbe(true, true))
		if err != nil {
			t.Fatalf("selector %q: %v", selector, err)
		}
		if status.Record.ScopeID != record.ScopeID {
			t.Fatalf("selector %q resolved to %q", selector, status.Record.ScopeID)
		}
	}
	// A scope id is globally unique and explicitly named, so it resolves across
	// owners; a name or pid does not, and the refusal says the record exists.
	other, err := ResolveConfineDetachStatus([]ConfineDetachRecord{record}, record.ScopeID, "session-b", aliveProbe(true, true))
	if err != nil {
		t.Fatalf("an explicitly named scope id must resolve regardless of owner: %v", err)
	}
	if other.Record.ScopeID != record.ScopeID {
		t.Fatalf("scope-id lookup resolved to %q", other.Record.ScopeID)
	}
	_, err = ResolveConfineDetachStatus([]ConfineDetachRecord{record}, "gate", "session-b", aliveProbe(true, true))
	if err == nil || !strings.Contains(err.Error(), "matched other owners") {
		t.Fatalf("a foreign-owner name match must say so, got %v", err)
	}
	_, err = ResolveConfineDetachStatus([]ConfineDetachRecord{record}, "nope", "session-a", aliveProbe(true, true))
	if err == nil || !strings.HasPrefix(err.Error(), CodeConfineNotFound) {
		t.Fatalf("a genuine miss must be %s, got %v", CodeConfineNotFound, err)
	}
}

// verifies: AIRA-22
func TestListConfineDetachStatusesIsOwnerScopedAndNewestFirst(t *testing.T) {
	mine1 := detachRecord(func(r *ConfineDetachRecord) {
		r.ScopeID, r.StartedAt = "CONFINE-a-1-a@session-a", "2026-09-05T09:00:00Z"
	})
	mine2 := detachRecord(func(r *ConfineDetachRecord) {
		r.ScopeID, r.StartedAt = "CONFINE-b-2-b@session-a", "2026-09-05T11:00:00Z"
	})
	theirs := detachRecord(func(r *ConfineDetachRecord) {
		r.ScopeID, r.Owner, r.StartedAt = "CONFINE-c-3-c@session-b", "session-b", "2026-09-05T12:00:00Z"
	})
	listed := ListConfineDetachStatuses([]ConfineDetachRecord{mine1, theirs, mine2}, "session-a", aliveProbe(true, true))
	if len(listed) != 2 {
		t.Fatalf("listed %d records, want only the caller's 2", len(listed))
	}
	if listed[0].Record.ScopeID != mine2.ScopeID || listed[1].Record.ScopeID != mine1.ScopeID {
		t.Fatalf("listing is not newest-first: %q then %q", listed[0].Record.ScopeID, listed[1].Record.ScopeID)
	}
}
